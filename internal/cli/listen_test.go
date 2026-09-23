package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/billkit-eu/billkit-cli/internal/api"
)

// --- helpers ---------------------------------------------------------------

// syncBuf is a writer the test goroutine can read while the listener
// goroutine is still writing to it.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// target is a local webhook receiver that records the id of every event the
// CLI forwards to it, in arrival order.
type target struct {
	*httptest.Server
	mu  sync.Mutex
	ids []string
}

func newTarget(t *testing.T) *target {
	t.Helper()
	tg := &target{}
	tg.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		id := eventID(body)
		tg.mu.Lock()
		tg.ids = append(tg.ids, id)
		tg.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(tg.Close)
	return tg
}

func (tg *target) got() []string {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	return slices.Clone(tg.ids)
}

func (tg *target) forwarder() *forwarder {
	return &forwarder{
		url:    tg.URL,
		secret: "bkwhsec_unit_test",
		client: tg.Client(),
		out:    io.Discard,
		logOut: io.Discard,
	}
}

// sseServer answers /v1/events/stream. Connection n is handed to script n;
// the last script repeats for every further reconnect.
type sseServer struct {
	*httptest.Server
	mu      sync.Mutex
	conns   int
	scripts []func(http.ResponseWriter, *http.Request)
}

func newSSEServer(t *testing.T, scripts ...func(http.ResponseWriter, *http.Request)) *sseServer {
	t.Helper()
	s := &sseServer{scripts: scripts}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		n := s.conns
		s.conns++
		s.mu.Unlock()
		if n >= len(s.scripts) {
			n = len(s.scripts) - 1
		}
		s.scripts[n](w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *sseServer) connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns
}

func openSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
}

func sendFrame(w http.ResponseWriter, event, data string) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	w.(http.Flusher).Flush()
}

func eventJSON(id, etype string, created int64) string {
	return fmt.Sprintf(`{"id":%q,"object":"event","type":%q,"created":%d,"livemode":false,"data":{}}`, id, etype, created)
}

// testListener runs the real reconnect loop at test speed.
func testListener(baseURL string, fwd *forwarder, errOut io.Writer) *listener {
	l := newListener(baseURL, "bk_test_unit", "", fwd, errOut)
	l.stallTimeout = 150 * time.Millisecond
	l.stallStep = 15 * time.Millisecond
	l.stableFor = time.Hour
	l.wait = func(ctx context.Context, _ int, _ time.Duration) bool {
		return api.SleepFor(ctx, 5*time.Millisecond)
	}
	return l
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// fakeAnchor is a scripted seed for the resume cursor, so the start-up path
// is asserted without standing up a second API.
type fakeAnchor struct {
	id  string
	err error
}

func (f *fakeAnchor) newest(context.Context) (string, error) { return f.id, f.err }

// cursorLog records the query each connection to the stream carried, which is
// how the resume behaviour is observed: `starting_after` and `types` are the
// entire contract between this CLI and the server's replay.
type cursorLog struct {
	mu      sync.Mutex
	cursors []string
	types   []string
}

func (c *cursorLog) record(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cursors = append(c.cursors, r.URL.Query().Get("starting_after"))
	c.types = append(c.types, r.URL.Query().Get("types"))
}

func (c *cursorLog) seen() ([]string, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.cursors), slices.Clone(c.types)
}

// --- SSE parsing + forwarding (pre-existing behaviour) ---------------------

func TestConsumeSSEForwardsOnlyMessageFramesWithValidSignature(t *testing.T) {
	const secret = "bkwhsec_unit_test"

	type captured struct{ body, sig, etype string }
	var got []captured
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, captured{
			body:  string(body),
			sig:   r.Header.Get("BillKit-Signature"),
			etype: r.Header.Get("BillKit-Event-Type"),
		})
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	fwd := &forwarder{
		url:    target.URL,
		secret: secret,
		client: target.Client(),
		out:    io.Discard,
		logOut: io.Discard,
	}
	onMessage := func(ctx context.Context, f sseFrame) {
		if f.event == "message" && f.dropped == 0 {
			fwd.handle(ctx, f.data)
		}
	}

	frame := func(event, data string) string {
		return "event: " + event + "\ndata: " + data + "\n\n"
	}
	// A stream mixing a keep-alive comment, two real events, and the server's
	// idle "timeout" frame (which must not be forwarded).
	stream := ": keep-alive\n\n" +
		frame("message", `{"id":"evt_1","type":"customer.created"}`) +
		frame("timeout", `{"message":"idle"}`) +
		frame("message", `{"id":"evt_2","type":"subscription.updated"}`)

	if err := consumeSSE(context.Background(), strings.NewReader(stream), onMessage); err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 {
		t.Fatalf("expected 2 forwarded events, got %d", len(got))
	}
	if got[0].etype != "customer.created" || got[1].etype != "subscription.updated" {
		t.Fatalf("unexpected forwarded types: %q, %q", got[0].etype, got[1].etype)
	}
	for _, c := range got {
		if !validSignature(secret, c.sig, []byte(c.body)) {
			t.Fatalf("signature %q does not verify for body %q", c.sig, c.body)
		}
	}
}

func TestForwarderWithoutURLDoesNotPost(t *testing.T) {
	var buf strings.Builder
	fwd := &forwarder{url: "", out: io.Discard, logOut: &buf}
	fwd.handle(context.Background(), []byte(`{"id":"evt_1","type":"customer.created"}`))
	if !strings.Contains(buf.String(), "customer.created") {
		t.Fatalf("expected event logged, got %q", buf.String())
	}
}

// TestPrintJSONKeepsStdoutMachineReadable pins the rule the whole command
// follows: under --print-json, stdout is the JSON and nothing else.
func TestPrintJSONKeepsStdoutMachineReadable(t *testing.T) {
	var out, logOut strings.Builder
	fwd := &forwarder{url: "", printJSON: true, out: &out, logOut: &logOut}
	raw := `{"id":"evt_1","type":"customer.created"}`
	fwd.handle(context.Background(), []byte(raw))

	if strings.TrimSpace(out.String()) != raw {
		t.Fatalf("stdout must carry the event JSON and nothing else, got %q", out.String())
	}
	if !strings.Contains(logOut.String(), "customer.created") {
		t.Fatalf("the human-readable line must still be written somewhere, got %q", logOut.String())
	}
}

// --- fatal versus transient ------------------------------------------------

// TestListenExitsNonZeroWhenTheKeyIsRejected covers P1-1, P1-11, P1-24, P1-30
// and P1-41: a 401 is the server saying the key is wrong, not "try again".
func TestListenExitsNonZeroWhenTheKeyIsRejected(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"type":"invalid_request_error","code":"invalid_api_key","message":"Invalid API key."}}`)
	}))
	defer srv.Close()

	errOut := &syncBuf{}
	l := testListener(srv.URL, newTarget(t).forwarder(), errOut)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- l.run(ctx) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("a rejected key must end the command with an error so the process exits non-zero; got nil after %d attempts", hits.Load())
		}
		if !strings.Contains(err.Error(), "401") {
			t.Errorf("error should name the status, got %q", err)
		}
		if !strings.Contains(errOut.String(), "billkit login") {
			t.Errorf("stderr should name the fix, got %q", errOut.String())
		}
	case <-ctx.Done():
		t.Fatalf("the reconnect loop never gave up on a 401: it re-dialled %d times and would have looped forever", hits.Load())
	}

	if n := hits.Load(); n != 1 {
		t.Errorf("a fatal 401 must not be retried, but the CLI dialled %d times", n)
	}
}

// TestListenTreatsForbiddenAsFatal is the missing-scope case: same class of
// failure, different remedy, so the hint has to differ too.
func TestListenTreatsForbiddenAsFatal(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":{"code":"insufficient_scope","message":"Missing scope."}}`)
	}))
	defer srv.Close()

	errOut := &syncBuf{}
	l := testListener(srv.URL, newTarget(t).forwarder(), errOut)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := l.run(ctx)
	if err == nil {
		t.Fatal("a 403 must end the command with an error")
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("a fatal 403 must not be retried, but the CLI dialled %d times", n)
	}
	if !strings.Contains(errOut.String(), "events:read") {
		t.Errorf("stderr should name the scope the key needs, got %q", errOut.String())
	}
}

// TestListenThatNeverConnectedReportsWhy is the CI case: a wrapper that runs
// `billkit listen` for a bounded time to smoke test connectivity must not
// read "the deadline passed without ever connecting" as success.
func TestListenThatNeverConnectedReportsWhy(t *testing.T) {
	// A server that is closed before the listener starts, so every dial is a
	// connection error: transient, retried, and never successful.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := dead.URL
	dead.Close()

	errOut := &syncBuf{}
	l := testListener(url, newTarget(t).forwarder(), errOut)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	if err := l.run(ctx); err == nil {
		t.Fatal("a listener that never reached the stream must exit non-zero, not report success")
	}
}

// TestListenRetriesTransientFailures is the other half of the same rule: a
// 503 really is "try again", and the stream must recover from it.
func TestListenRetriesTransientFailures(t *testing.T) {
	tg := newTarget(t)
	srv := newSSEServer(t,
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) },
		func(w http.ResponseWriter, r *http.Request) {
			openSSE(w)
			sendFrame(w, "message", eventJSON("evt_live", "customer.created", 200))
			<-r.Context().Done()
		},
	)

	errOut := &syncBuf{}
	l := testListener(srv.URL, tg.forwarder(), errOut)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- l.run(ctx) }()

	waitFor(t, "the event that arrived after the 503", func() bool {
		return slices.Contains(tg.got(), "evt_live")
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("a recovered stream must not end in an error, got %v", err)
	}
}

// TestListenHonoursRetryAfterOnRateLimit checks 429 stays retryable and that
// the server's own pacing is passed to the backoff.
func TestListenHonoursRetryAfterOnRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	var seen atomic.Int64
	errOut := &syncBuf{}
	l := testListener(srv.URL, newTarget(t).forwarder(), errOut)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l.wait = func(_ context.Context, _ int, retryAfter time.Duration) bool {
		seen.Store(int64(retryAfter))
		cancel()
		return false
	}

	if err := l.run(ctx); err == nil {
		t.Fatal("a listener that never reached the stream must report why")
	}
	if got := time.Duration(seen.Load()); got != 7*time.Second {
		t.Errorf("Retry-After should reach the backoff, got %v", got)
	}
}

// TestStreamStatusErrorClassification is the whole fatal-versus-transient rule
// in one table.
func TestStreamStatusErrorClassification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		wantFatal bool
		wantHint  string
	}{
		{"bad request names the filter", 400, true, "--events"},
		{"unauthorized names login", 401, true, "billkit login"},
		{"forbidden names the scope", 403, true, "events:read"},
		{"not found names the host", 404, true, "--base-url"},
		{"request timeout is temporary", 408, false, ""},
		{"unprocessable names the filter", 422, true, "--events"},
		{"rate limited is temporary", 429, false, ""},
		{"server error is temporary", 500, false, ""},
		{"bad gateway is temporary", 502, false, ""},
		{"gateway timeout is temporary", 504, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := &streamStatusError{status: c.status}
			if got := e.fatal(); got != c.wantFatal {
				t.Errorf("fatal() = %v, want %v", got, c.wantFatal)
			}
			if c.wantHint != "" && !strings.Contains(e.hint(), c.wantHint) {
				t.Errorf("hint() = %q, want it to mention %q", e.hint(), c.wantHint)
			}
		})
	}
}

func TestStreamStatusErrorReadsTheAPIEnvelope(t *testing.T) {
	e := &streamStatusError{status: 401, body: `{"error":{"code":"invalid_api_key","message":"Invalid API key provided."}}`}
	if got := e.Error(); !strings.Contains(got, "Invalid API key provided.") || strings.Contains(got, `{"error"`) {
		t.Errorf("Error() = %q, want the parsed message rather than raw JSON", got)
	}
	// A body that is not the envelope still has to survive intact.
	plain := &streamStatusError{status: 502, body: "<html>bad gateway</html>"}
	if got := plain.Error(); !strings.Contains(got, "bad gateway") {
		t.Errorf("Error() = %q, want the raw body kept", got)
	}
	if got := (&streamStatusError{status: 503}).Error(); got != "HTTP 503" {
		t.Errorf("Error() = %q, want %q", got, "HTTP 503")
	}
}

// --- backoff ---------------------------------------------------------------

func TestDefaultWaitUsesTheSharedBackoff(t *testing.T) {
	l := testListener("http://127.0.0.1:1", newTarget(t).forwarder(), &syncBuf{})
	start := time.Now()
	if !l.defaultWait(context.Background(), 0, 40*time.Millisecond) {
		t.Fatal("the wait should have completed")
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("Retry-After was not honoured: waited %v", elapsed)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if l.defaultWait(ctx, 3, 0) {
		t.Error("a cancelled context must end the wait, not complete it")
	}
}

// TestListenBacksOffFurtherOnEachFailedAttempt covers the "tight 2s retry
// loop" half of P1-24: the delay has to grow, not stay flat.
func TestListenBacksOffFurtherOnEachFailedAttempt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	var mu sync.Mutex
	var attempts []int
	errOut := &syncBuf{}
	l := testListener(srv.URL, newTarget(t).forwarder(), errOut)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l.wait = func(_ context.Context, attempt int, _ time.Duration) bool {
		mu.Lock()
		attempts = append(attempts, attempt)
		n := len(attempts)
		mu.Unlock()
		if n >= 6 {
			cancel()
			return false
		}
		return true
	}

	_ = l.run(ctx)

	mu.Lock()
	defer mu.Unlock()
	want := []int{0, 1, 2, 3, 4, 5}
	if !slices.Equal(attempts, want) {
		t.Fatalf("each failed reconnect must back off further: attempts %v, want %v", attempts, want)
	}
	// And the curve those attempt numbers feed must actually grow.
	if api.BackoffCeiling(attempts[len(attempts)-1]) <= api.BackoffCeiling(attempts[0]) {
		t.Fatal("the backoff ceiling did not grow across attempts")
	}
}

// TestListenResetsBackoffAfterAStableStream keeps a stream that drops once an
// hour from creeping up to the ceiling.
func TestListenResetsBackoffAfterAStableStream(t *testing.T) {
	srv := newSSEServer(t, func(w http.ResponseWriter, _ *http.Request) {
		openSSE(w)
	})

	var mu sync.Mutex
	var attempts []int
	errOut := &syncBuf{}
	l := testListener(srv.URL, newTarget(t).forwarder(), errOut)
	l.stableFor = 0 // every connection counts as stable
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l.wait = func(_ context.Context, attempt int, _ time.Duration) bool {
		mu.Lock()
		attempts = append(attempts, attempt)
		n := len(attempts)
		mu.Unlock()
		if n >= 4 {
			cancel()
			return false
		}
		return true
	}

	_ = l.run(ctx)

	mu.Lock()
	defer mu.Unlock()
	for i, a := range attempts {
		if a != 0 {
			t.Fatalf("attempt %d backed off at step %d after a stable stream; want the floor", i, a)
		}
	}
}

// --- stalled socket --------------------------------------------------------

// TestListenDetectsAStalledStream covers P1-23: an open socket that stops
// delivering must be noticed and replaced, not waited on forever.
func TestListenDetectsAStalledStream(t *testing.T) {
	tg := newTarget(t)
	srv := newSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		openSSE(w)
		sendFrame(w, "message", eventJSON("evt_before_stall", "customer.created", 100))
		// Headers sent, one frame delivered, then silence: no further data,
		// no keep-alive, no close. Exactly the proxy-holds-the-socket case.
		<-r.Context().Done()
	})

	errOut := &syncBuf{}
	l := testListener(srv.URL, tg.forwarder(), errOut)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- l.run(ctx) }()

	waitFor(t, "the stalled stream to be dropped and re-dialled", func() bool {
		return srv.connections() >= 2
	})
	cancel()
	<-done

	if !strings.Contains(errOut.String(), errStreamStalled.Error()) {
		t.Errorf("the user must be told the stream went silent, got %q", errOut.String())
	}
}

// TestStreamOnceReturnsStalledRatherThanBlocking is the same failure at the
// level of one attempt, so the assertion is exact.
func TestStreamOnceReturnsStalledRatherThanBlocking(t *testing.T) {
	srv := newSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		openSSE(w)
		<-r.Context().Done()
	})

	l := testListener(srv.URL, newTarget(t).forwarder(), &syncBuf{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	type result struct {
		reached bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		reached, err := l.streamOnce(ctx, false)
		done <- result{reached, err}
	}()

	select {
	case got := <-done:
		if !got.reached {
			t.Error("the connection was established, so streamOnce must report it")
		}
		if !errors.Is(got.err, errStreamStalled) {
			t.Fatalf("want a stalled-stream error, got %v", got.err)
		}
	case <-ctx.Done():
		t.Fatal("streamOnce blocked on a silent socket instead of giving up")
	}
}

// --- resuming across a reconnect -------------------------------------------

// TestListenResumesFromTheLastEventItForwarded covers P1-2 and P1-42: the
// events written while the stream was down must reach the local endpoint, in
// order, exactly once.
//
// The CLI no longer backfills them itself. It hands the server the id of the
// last event it forwarded and the server replays from there, which is what
// removed the old walk's 500-event ceiling.
func TestListenResumesFromTheLastEventItForwarded(t *testing.T) {
	tg := newTarget(t)
	log := &cursorLog{}
	srv := newSSEServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			log.record(r)
			openSSE(w)
			sendFrame(w, "message", eventJSON("evt_1", "customer.created", 100))
		},
		func(w http.ResponseWriter, r *http.Request) {
			log.record(r)
			openSSE(w)
			// What the server replays because of the cursor, then live.
			sendFrame(w, "message", eventJSON("evt_2", "customer.updated", 101))
			sendFrame(w, "message", eventJSON("evt_3", "invoice.paid", 102))
			<-r.Context().Done()
		},
	)

	errOut := &syncBuf{}
	l := testListener(srv.URL, tg.forwarder(), errOut)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- l.run(ctx) }()

	waitFor(t, "the reconnected stream to deliver evt_3", func() bool {
		return slices.Contains(tg.got(), "evt_3")
	})
	cancel()
	<-done

	want := []string{"evt_1", "evt_2", "evt_3"}
	if got := tg.got(); !slices.Equal(got, want) {
		t.Fatalf("events must be forwarded once, in order: got %v, want %v", got, want)
	}
	cursors, _ := log.seen()
	if len(cursors) < 2 || cursors[0] != "" || cursors[1] != "evt_1" {
		t.Fatalf("want a first connection with no cursor and a reconnect resuming after evt_1, got %v", cursors)
	}
	if !strings.Contains(errOut.String(), "Replaying anything recorded after evt_1") {
		t.Errorf("the replay must be reported, got %q", errOut.String())
	}
}

// TestListenSendsTheEventsFilterWithTheCursor is the regression test for the
// bug the server-side cursor closed. The replay used to be a client-side walk
// of GET /v1/events, which has no multi-type filter, so every reconnect under
// `--events` forwarded event types the user had explicitly excluded — and the
// server's idle ceiling makes a reconnect happen roughly hourly.
func TestListenSendsTheEventsFilterWithTheCursor(t *testing.T) {
	const filter = "customer.created,invoice.paid"
	log := &cursorLog{}
	srv := newSSEServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			log.record(r)
			openSSE(w)
			sendFrame(w, "message", eventJSON("evt_1", "customer.created", 100))
		},
		func(w http.ResponseWriter, r *http.Request) {
			log.record(r)
			openSSE(w)
			<-r.Context().Done()
		},
	)

	l := testListener(srv.URL, newTarget(t).forwarder(), &syncBuf{})
	l.types = filter

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- l.run(ctx) }()

	waitFor(t, "the reconnect", func() bool { return srv.connections() >= 2 })
	cancel()
	<-done

	cursors, types := log.seen()
	if len(types) < 2 || types[0] != filter || types[1] != filter {
		t.Fatalf("every connection must carry the filter, got %v", types)
	}
	if len(cursors) < 2 || cursors[1] != "evt_1" {
		t.Fatalf("the reconnect must carry the cursor alongside the filter, got %v", cursors)
	}
}

// TestListenSeedsTheCursorBeforeAnyEventArrives closes the one gap the cursor
// cannot close on its own: between start-up and the first delivered event
// there is no id to resume from, so the newest existing event is read once and
// used as the starting point.
func TestListenSeedsTheCursorBeforeAnyEventArrives(t *testing.T) {
	log := &cursorLog{}
	srv := newSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		openSSE(w)
	})

	l := testListener(srv.URL, newTarget(t).forwarder(), &syncBuf{})
	l.anchor = &fakeAnchor{id: "evt_0"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- l.run(ctx) }()

	waitFor(t, "the first connection", func() bool { return srv.connections() >= 1 })
	cancel()
	<-done

	cursors, _ := log.seen()
	if len(cursors) == 0 || cursors[0] != "evt_0" {
		t.Fatalf("the first connection must resume from the seeded cursor, got %v", cursors)
	}
}

// TestListenDropsAResumePointTheServerHasPruned is the failure mode the
// retention sweep creates: the event the cursor names ages out of the log, and
// from then on every reconnect carrying it is refused with a 400. A 400 is
// otherwise fatal here, so without this the listener would exit on an event
// that simply got old. Lossy is acceptable; said out loud is mandatory.
func TestListenDropsAResumePointTheServerHasPruned(t *testing.T) {
	tg := newTarget(t)
	log := &cursorLog{}
	srv := newSSEServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			log.record(r)
			openSSE(w)
			sendFrame(w, "message", eventJSON("evt_1", "customer.created", 100))
		},
		func(w http.ResponseWriter, r *http.Request) {
			log.record(r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"type":"invalid_request_error","code":"parameter_invalid","message":"No such event for cursor.","param":"starting_after"}}`)
		},
		func(w http.ResponseWriter, r *http.Request) {
			log.record(r)
			openSSE(w)
			sendFrame(w, "message", eventJSON("evt_9", "invoice.paid", 900))
			<-r.Context().Done()
		},
	)

	errOut := &syncBuf{}
	l := testListener(srv.URL, tg.forwarder(), errOut)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- l.run(ctx) }()

	waitFor(t, "the stream to recover without its cursor", func() bool {
		return slices.Contains(tg.got(), "evt_9")
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("a pruned resume point must not end the command: %v", err)
	}

	cursors, _ := log.seen()
	if len(cursors) < 3 || cursors[1] != "evt_1" || cursors[2] != "" {
		t.Fatalf("want the refused cursor dropped on the next attempt, got %v", cursors)
	}
	if !strings.Contains(errOut.String(), "no longer in the event log") ||
		!strings.Contains(errOut.String(), "billkit events list") {
		t.Errorf("the loss must be reported with its reconcile command, got %q", errOut.String())
	}
}

// TestStaleCursorOnlyMatchesTheCursorParameter keeps the exemption narrow: a
// 400 about anything else is still the server saying the request is wrong, and
// retrying it forever while exiting 0 is what this CLI must never do.
func TestStaleCursorOnlyMatchesTheCursorParameter(t *testing.T) {
	stale := &streamStatusError{
		status: http.StatusBadRequest,
		body:   `{"error":{"code":"parameter_invalid","message":"No such event for cursor.","param":"starting_after"}}`,
	}
	if !stale.staleCursor() {
		t.Error("a 400 naming starting_after is a stale cursor")
	}
	badFilter := &streamStatusError{
		status: http.StatusBadRequest,
		body:   `{"error":{"code":"parameter_invalid","message":"Unknown event types: nope","param":"types"}}`,
	}
	if badFilter.staleCursor() {
		t.Error("a 400 about the types filter must stay fatal")
	}
	if (&streamStatusError{status: http.StatusBadRequest}).staleCursor() {
		t.Error("a 400 with no envelope says nothing about the cursor")
	}
	if (&streamStatusError{status: http.StatusNotFound, body: `{"error":{"param":"starting_after"}}`}).staleCursor() {
		t.Error("only a 400 can be a stale cursor")
	}
}

// TestListenReportsAReconnectWithNoResumePoint covers the case where there is
// nothing to resume from at all. Silence is the one unacceptable answer.
func TestListenReportsAReconnectWithNoResumePoint(t *testing.T) {
	srv := newSSEServer(t, func(w http.ResponseWriter, _ *http.Request) { openSSE(w) })

	errOut := &syncBuf{}
	l := testListener(srv.URL, newTarget(t).forwarder(), errOut)
	l.anchor = nil // no event log to seed a cursor from

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- l.run(ctx) }()

	waitFor(t, "the no-resume-point warning", func() bool {
		return strings.Contains(errOut.String(), "were not replayed")
	})
	cancel()
	<-done

	if !strings.Contains(errOut.String(), "billkit events list") {
		t.Errorf("the warning must name the reconcile command, got %q", errOut.String())
	}
}

// TestListenAnnouncesTheServersIdleCeiling stops the hourly reconnect from
// being completely invisible.
func TestListenAnnouncesTheServersIdleCeiling(t *testing.T) {
	srv := newSSEServer(t, func(w http.ResponseWriter, _ *http.Request) {
		openSSE(w)
		sendFrame(w, "timeout", `{"message":"Idle ceiling reached. Reconnect to keep listening."}`)
	})

	errOut := &syncBuf{}
	l := testListener(srv.URL, newTarget(t).forwarder(), errOut)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- l.run(ctx) }()

	waitFor(t, "the idle-ceiling notice", func() bool {
		return strings.Contains(errOut.String(), "closed the idle stream")
	})
	cancel()
	<-done
}

// --- the cursor seed -------------------------------------------------------

func TestEventAnchorReadsTheNewestEventID(t *testing.T) {
	var query atomic.Value
	body := atomic.Value{}
	body.Store(fmt.Sprintf(`{"object":"list","data":[%s],"has_more":true}`, eventJSON("evt_5", "invoice.paid", 105)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query.Store(r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body.Load().(string))
	}))
	defer srv.Close()

	a := &apiEventAnchor{client: api.New(srv.URL, "bk_test_unit", "dev", srv.Client())}
	id, err := a.newest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id != "evt_5" {
		t.Fatalf("newest() = %q, want evt_5", id)
	}
	if got := query.Load().(string); got != "limit=1" {
		t.Errorf("the seed must ask for one row, got %q", got)
	}

	// An account that has never emitted an event has no cursor, and that is
	// not an error: the stream starts from now, which is already correct.
	body.Store(`{"object":"list","data":[],"has_more":false}`)
	id, err = a.newest(context.Background())
	if err != nil || id != "" {
		t.Fatalf("an empty log must yield no cursor, got %q / %v", id, err)
	}
}

// --- flag validation -------------------------------------------------------

// TestValidateForwardURL: a --forward-to with no scheme used to be accepted at
// start-up and then reported once per event as a build error, which reads as a
// broken webhook handler rather than as a mistyped flag.
func TestValidateForwardURL(t *testing.T) {
	for _, ok := range []string{"", "http://localhost:3000/hook", "https://example.test/x"} {
		if err := validateForwardURL(ok); err != nil {
			t.Errorf("validateForwardURL(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"localhost:3000/hook", "/hook", "ftp://example.test", "http://"} {
		if err := validateForwardURL(bad); err == nil {
			t.Errorf("validateForwardURL(%q) must be refused before the stream opens", bad)
		}
	}
}

// --- shared helper ---------------------------------------------------------

// validSignature reproduces what a receiving BillKit SDK does with the header.
func validSignature(secret, header string, body []byte) bool {
	var ts, v1 string
	for _, part := range strings.Split(header, ",") {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch key {
		case "t":
			ts = value
		case "v1":
			v1 = value
		}
	}
	if ts == "" || v1 == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%s.", ts)
	mac.Write(body)
	return hmac.Equal([]byte(v1), []byte(hex.EncodeToString(mac.Sum(nil))))
}

// --- oversized frames ------------------------------------------------------

// TestConsumeSSESkipsAnOversizedEventAndKeepsGoing is the regression for a
// deterministic livelock. The parser used to be a bufio.Scanner, which fails
// the whole connection on a token longer than its buffer; once `listen`
// resumed from a cursor, the server replayed the same oversized event on every
// reconnect and the listener spun on one row forever at a growing backoff,
// looking alive the whole time.
func TestConsumeSSESkipsAnOversizedEventAndKeepsGoing(t *testing.T) {
	pad := strings.Repeat("x", maxSSEFrameBytes+4096)
	stream := "id: evt_big\nevent: message\ndata: {\"id\":\"evt_big\",\"pad\":\"" + pad + "\"}\n\n" +
		"id: evt_next\nevent: message\ndata: {\"id\":\"evt_next\"}\n\n"

	var frames []sseFrame
	if err := consumeSSE(context.Background(), strings.NewReader(stream),
		func(_ context.Context, f sseFrame) { frames = append(frames, f) }); err != nil {
		t.Fatal(err)
	}

	if len(frames) != 2 {
		t.Fatalf("the stream must keep moving past the oversized event, got %d frame(s)", len(frames))
	}
	if frames[0].dropped == 0 {
		t.Error("the oversized frame must be marked incomplete")
	}
	if frames[0].id != "evt_big" {
		t.Errorf("the id: field survives a payload that does not: got %q", frames[0].id)
	}
	// Half an event that still parses as JSON is the worst thing a webhook
	// relay can deliver, so nothing of it is retained.
	if len(frames[0].data) != 0 {
		t.Errorf("an incomplete frame must carry no data, got %d byte(s)", len(frames[0].data))
	}
	if frames[1].dropped != 0 || string(frames[1].data) != `{"id":"evt_next"}` {
		t.Errorf("the next event must arrive intact, got %+v", frames[1])
	}
}

// TestListenReportsAndStepsPastAnOversizedEvent is the same thing one layer
// up: the loss is named, the command that reads the event is printed, and the
// cursor moves past it so the next reconnect does not stall on the same row.
func TestListenReportsAndStepsPastAnOversizedEvent(t *testing.T) {
	tg := newTarget(t)
	pad := strings.Repeat("x", maxSSEFrameBytes+4096)
	srv := newSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		openSSE(w)
		fmt.Fprintf(w, "id: evt_big\nevent: message\ndata: {\"id\":\"evt_big\",\"pad\":\"%s\"}\n\n", pad)
		w.(http.Flusher).Flush()
		sendFrame(w, "message", eventJSON("evt_small", "customer.created", 101))
		<-r.Context().Done()
	})

	errOut := &syncBuf{}
	l := testListener(srv.URL, tg.forwarder(), errOut)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- l.run(ctx) }()

	waitFor(t, "the event after the oversized one", func() bool {
		return slices.Contains(tg.got(), "evt_small")
	})
	cancel()
	<-done

	if slices.Contains(tg.got(), "evt_big") {
		t.Fatal("an event that could not be buffered whole must never be forwarded")
	}
	out := errOut.String()
	if !strings.Contains(out, "evt_big") || !strings.Contains(out, "NOT forwarded") {
		t.Errorf("the skipped event must be named, got %q", out)
	}
	if !strings.Contains(out, "billkit events retrieve evt_big") {
		t.Errorf("the notice must say how to read it, got %q", out)
	}
	if l.resumeAfter != "evt_small" {
		t.Errorf("resumeAfter = %q; the cursor must have moved past the oversized row", l.resumeAfter)
	}
}

func TestSSELine(t *testing.T) {
	t.Run("plain lines, CRLF and LF alike", func(t *testing.T) {
		br := bufio.NewReaderSize(strings.NewReader("a\r\nbb\n\n"), 16)
		for _, want := range []string{"a", "bb", ""} {
			got, dropped, err := sseLine(br, 64)
			if err != nil || dropped != 0 || got != want {
				t.Fatalf("sseLine = %q/%d/%v, want %q", got, dropped, err, want)
			}
		}
	})

	t.Run("a line longer than the limit is truncated, not fatal", func(t *testing.T) {
		br := bufio.NewReaderSize(strings.NewReader(strings.Repeat("y", 100)+"\nnext\n"), 16)
		got, dropped, err := sseLine(br, 10)
		if err != nil {
			t.Fatal(err)
		}
		// 91, not 90: the line terminator lands on the dropped side too, so
		// the count the user sees is a lower bound and is worded as one.
		if len(got) != 10 || dropped != 91 {
			t.Fatalf("kept %d, dropped %d; want exactly the limit kept and the rest dropped", len(got), dropped)
		}
		// The reader is left positioned on the next line, which is the whole
		// point: the frame is skipped, the stream is not.
		got, dropped, err = sseLine(br, 10)
		if err != nil || dropped != 0 || got != "next" {
			t.Fatalf("sseLine after an overflow = %q/%d/%v", got, dropped, err)
		}
	})

	t.Run("a final line the server never terminated is still a line", func(t *testing.T) {
		br := bufio.NewReaderSize(strings.NewReader("tail"), 16)
		got, _, err := sseLine(br, 64)
		if err != nil || got != "tail" {
			t.Fatalf("sseLine = %q/%v, want tail", got, err)
		}
		if _, _, err = sseLine(br, 64); !errors.Is(err, io.EOF) {
			t.Fatalf("the read after it must be EOF, got %v", err)
		}
	})
}

// TestForwardedHeadersMatchTheRealDispatcher. `listen` exists so code written
// against it works unchanged in production, and the header set is most of
// that promise. BillKit-Event-Id was missing, so a handler that deduped on it
// read an empty string locally, passed every local test, and deduped nothing
// once deployed. api/tests/test_cli_webhook_headers.py pins the same set from
// the other side, against webhook_dispatcher.py itself.
func TestForwardedHeadersMatchTheRealDispatcher(t *testing.T) {
	var got http.Header
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	fwd := &forwarder{
		url:    target.URL,
		secret: "bkwhsec_unit_test",
		client: target.Client(),
		out:    io.Discard,
		logOut: io.Discard,
	}
	raw := []byte(`{"id":"evt_42","type":"customer.created"}`)
	fwd.handle(context.Background(), raw)

	want := map[string]string{
		"Content-Type":             "application/json",
		"BillKit-Event-Id":         "evt_42",
		"BillKit-Event-Type":       "customer.created",
		"BillKit-Delivery-Attempt": "1",
	}
	for header, value := range want {
		if have := got.Get(header); have != value {
			t.Errorf("%s = %q, want %q", header, have, value)
		}
	}
	// Both identities: the dispatcher's, so a handler that matches on it
	// behaves the same, and the CLI's, so a log can still tell them apart.
	ua := got.Get("User-Agent")
	if !strings.HasPrefix(ua, "BillKit-Webhook/1.0") || !strings.Contains(ua, "billkit-cli/") {
		t.Errorf("User-Agent = %q, want the dispatcher's identity plus this CLI's", ua)
	}
	if !validSignature("bkwhsec_unit_test", got.Get("BillKit-Signature"), raw) {
		t.Errorf("BillKit-Signature %q does not verify", got.Get("BillKit-Signature"))
	}
}

// TestForwardRetriesAConnectionFailure. `deliver` advances the resume cursor
// after handle() returns, so a forward that failed used to be a permanent
// loss: the one second a dev server spends restarting on a file save ate the
// event, the listener still looked healthy, and nothing was coming back.
func TestForwardRetriesAConnectionFailure(t *testing.T) {
	var hits int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt64(&hits, 1) <= 2 {
			// Refuse this delivery the way a restarting server does: accept
			// nothing and drop the connection before answering.
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					_ = conn.Close()
					return
				}
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	var log strings.Builder
	fwd := &forwarder{
		url:     target.URL,
		secret:  "bkwhsec_unit_test",
		client:  target.Client(),
		out:     io.Discard,
		logOut:  &log,
		backoff: []time.Duration{time.Millisecond, time.Millisecond},
	}
	fwd.handle(context.Background(), []byte(`{"id":"evt_1","type":"customer.created"}`))

	if got := atomic.LoadInt64(&hits); got != 3 {
		t.Fatalf("delivery attempts = %d, want 3 (two refused, then accepted)", got)
	}
	if !strings.Contains(log.String(), "-> 200") {
		t.Fatalf("the successful delivery must be reported: %q", log.String())
	}
}

// When the retries run out the loss is stated, with the command that reads
// the event back. Silence is the one option that is not allowed here.
func TestForwardReportsAnExhaustedDelivery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	client := srv.Client()
	srv.Close() // nothing is listening any more, so every attempt fails to dial

	var log strings.Builder
	fwd := &forwarder{
		url:     url,
		secret:  "bkwhsec_unit_test",
		client:  client,
		out:     io.Discard,
		logOut:  &log,
		backoff: []time.Duration{time.Millisecond, time.Millisecond},
	}
	fwd.handle(context.Background(), []byte(`{"id":"evt_9","type":"customer.created"}`))

	out := log.String()
	if !strings.Contains(out, "NOT forwarded after 3 attempts") {
		t.Errorf("the loss must be named: %q", out)
	}
	if !strings.Contains(out, "billkit events retrieve evt_9") {
		t.Errorf("the recovery command must be printed: %q", out)
	}
}

// An HTTP answer of any status is final. A 500 is the developer's handler
// answering this event; re-posting the same body into a local app that
// already ran it manufactures duplicates the production dispatcher's own
// schedule would not.
func TestForwardDoesNotRetryAnHTTPFailure(t *testing.T) {
	var hits int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer target.Close()

	fwd := &forwarder{
		url:     target.URL,
		secret:  "bkwhsec_unit_test",
		client:  target.Client(),
		out:     io.Discard,
		logOut:  io.Discard,
		backoff: []time.Duration{time.Millisecond, time.Millisecond},
	}
	fwd.handle(context.Background(), []byte(`{"id":"evt_1","type":"customer.created"}`))

	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("delivery attempts = %d, want 1: an HTTP status is an answer", got)
	}
}

// TestConsumeSSEDefaultsToTheMessageEventType. The SSE spec says a frame with
// data and no `event:` line is a "message". BillKit always writes
// `event: message` (api/billkit/api/events.py, _sse), so this is a contract
// guard: an intermediary that strips the line must not turn every event into
// a silent drop.
func TestConsumeSSEDefaultsToTheMessageEventType(t *testing.T) {
	var seen []sseFrame
	stream := "data: {\"id\":\"evt_1\",\"type\":\"customer.created\"}\n\n"
	err := consumeSSE(context.Background(), strings.NewReader(stream), func(_ context.Context, f sseFrame) {
		seen = append(seen, f)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Fatalf("frames = %d, want 1: a frame with no `event:` line was dropped", len(seen))
	}
	if seen[0].event != "message" {
		t.Fatalf("event = %q, want %q", seen[0].event, "message")
	}
}
