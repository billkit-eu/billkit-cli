package cli

import (
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
		id, _ := eventMeta(body)
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
		secret: "whsec_unit_test",
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
	l := newListener(baseURL, "sk_test_unit", "", fwd, errOut)
	l.stallTimeout = 150 * time.Millisecond
	l.stallStep = 15 * time.Millisecond
	l.settle = 0
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

// fakeFill is a scripted gap filler, so replay behaviour is asserted without
// standing up a second API.
type fakeFill struct {
	newestID  string
	newestAt  int64
	gap       [][]byte
	truncated bool
	err       error
	calls     atomic.Int32
}

func (f *fakeFill) newest(context.Context) (string, int64, error) {
	return f.newestID, f.newestAt, nil
}

func (f *fakeFill) eventsAfter(_ context.Context, _ string, _ int64) ([][]byte, bool, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, false, f.err
	}
	return f.gap, f.truncated, nil
}

// --- SSE parsing + forwarding (pre-existing behaviour) ---------------------

func TestConsumeSSEForwardsOnlyMessageFramesWithValidSignature(t *testing.T) {
	const secret = "whsec_unit_test"

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
	onMessage := func(ctx context.Context, event string, data []byte) {
		if event == "message" {
			fwd.handle(ctx, data)
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

// --- gap replay ------------------------------------------------------------

// TestListenReplaysEventsMissedDuringTheGap covers P1-2 and P1-42: the events
// written while the stream was down must reach the local endpoint, in order,
// exactly once.
func TestListenReplaysEventsMissedDuringTheGap(t *testing.T) {
	tg := newTarget(t)
	srv := newSSEServer(t,
		func(w http.ResponseWriter, _ *http.Request) {
			openSSE(w)
			sendFrame(w, "message", eventJSON("evt_1", "customer.created", 100))
		},
		func(w http.ResponseWriter, r *http.Request) {
			openSSE(w)
			// The reconnected stream re-sends evt_2, which the replay has
			// already delivered: the overlap must be dropped, not doubled.
			sendFrame(w, "message", eventJSON("evt_2", "customer.updated", 101))
			sendFrame(w, "message", eventJSON("evt_3", "invoice.paid", 102))
			<-r.Context().Done()
		},
	)

	fill := &fakeFill{gap: [][]byte{[]byte(eventJSON("evt_2", "customer.updated", 101))}}
	errOut := &syncBuf{}
	l := testListener(srv.URL, tg.forwarder(), errOut)
	l.fill = fill

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
		t.Fatalf("events missed during the gap must be replayed once, in order: got %v, want %v", got, want)
	}
	if !strings.Contains(errOut.String(), "Replaying 1 event(s)") {
		t.Errorf("the replay must be reported, got %q", errOut.String())
	}
}

// TestListenReportsAGapItCannotReplay is the fallback: when the missed events
// cannot be fetched, say so and name the command that reconciles them.
// Silence is the one unacceptable answer.
func TestListenReportsAGapItCannotReplay(t *testing.T) {
	srv := newSSEServer(t, func(w http.ResponseWriter, _ *http.Request) {
		openSSE(w)
		sendFrame(w, "message", eventJSON("evt_1", "customer.created", 100))
	})

	fill := &fakeFill{err: errors.New("event log unreachable")}
	errOut := &syncBuf{}
	l := testListener(srv.URL, newTarget(t).forwarder(), errOut)
	l.fill = fill

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- l.run(ctx) }()

	waitFor(t, "the gap warning", func() bool {
		return strings.Contains(errOut.String(), "billkit events list")
	})
	cancel()
	<-done

	if !strings.Contains(errOut.String(), "Could not read the events missed") {
		t.Errorf("the warning must say what failed, got %q", errOut.String())
	}
}

// TestListenReportsAReconnectWithNoResumePoint covers the case where there is
// nothing to resume from at all.
func TestListenReportsAReconnectWithNoResumePoint(t *testing.T) {
	srv := newSSEServer(t, func(w http.ResponseWriter, _ *http.Request) { openSSE(w) })

	errOut := &syncBuf{}
	l := testListener(srv.URL, newTarget(t).forwarder(), errOut)
	l.fill = nil // no event log to replay from

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

// TestResumeCursorOnlyMovesForward keeps `listen` a tail rather than a history
// dump: an event older than the cursor must not drag the next replay back past
// the point the user started listening.
func TestResumeCursorOnlyMovesForward(t *testing.T) {
	l := testListener("http://127.0.0.1:1", newTarget(t).forwarder(), &syncBuf{})
	l.advance("evt_new", 200)
	l.advance("evt_old", 100)
	if l.lastID != "evt_new" || l.lastCreated != 200 {
		t.Fatalf("cursor rewound to %s/%d", l.lastID, l.lastCreated)
	}
	// An event in the same second is still an advance, since the log breaks
	// those ties by id and the walk stops on the id landmark.
	l.advance("evt_same", 200)
	if l.lastID != "evt_same" {
		t.Fatalf("cursor stuck at %s", l.lastID)
	}
}

// --- the backfill walk -----------------------------------------------------

func TestGapFillerWalksBackToTheLastSeenEvent(t *testing.T) {
	// Newest first, as GET /v1/events orders them.
	log := []string{
		eventJSON("evt_5", "invoice.paid", 105),
		eventJSON("evt_4", "customer.updated", 104),
		eventJSON("evt_3", "customer.created", 103),
		eventJSON("evt_2", "customer.created", 102),
		eventJSON("evt_1", "customer.created", 101),
	}
	var pages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages.Add(1)
		start := 0
		if after := r.URL.Query().Get("starting_after"); after != "" {
			for i, raw := range log {
				if id, _ := eventMeta([]byte(raw)); id == after {
					start = i + 1
					break
				}
			}
		}
		end := min(start+2, len(log)) // two rows per page, so the walk pages
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"object":"list","data":[%s],"has_more":%t}`,
			strings.Join(log[start:end], ","), end < len(log))
	}))
	defer srv.Close()

	g := &apiGapFiller{client: api.New(srv.URL, "sk_test_unit", "dev", srv.Client())}

	newestID, newestAt, err := g.newest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if newestID != "evt_5" || newestAt != 105 {
		t.Fatalf("newest() = %q/%d, want evt_5/105", newestID, newestAt)
	}

	got, truncated, err := g.eventsAfter(context.Background(), "evt_2", 102)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Error("a three-event gap is not truncated")
	}
	var ids []string
	for _, raw := range got {
		id, _ := eventMeta(raw)
		ids = append(ids, id)
	}
	want := []string{"evt_3", "evt_4", "evt_5"}
	if !slices.Equal(ids, want) {
		t.Fatalf("replay must be oldest first and stop at the cursor: got %v, want %v", ids, want)
	}
	if pages.Load() < 2 {
		t.Errorf("expected the walk to page, made %d request(s)", pages.Load())
	}
}

func TestGapFillerStopsAtAnOlderEventWhenTheCursorIsGone(t *testing.T) {
	// evt_2 has been pruned by retention, so the id landmark never matches.
	log := []string{
		eventJSON("evt_4", "invoice.paid", 104),
		eventJSON("evt_3", "customer.created", 103),
		eventJSON("evt_1", "customer.created", 101),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"object":"list","data":[%s],"has_more":false}`, strings.Join(log, ","))
	}))
	defer srv.Close()

	g := &apiGapFiller{client: api.New(srv.URL, "sk_test_unit", "dev", srv.Client())}
	got, _, err := g.eventsAfter(context.Background(), "evt_2", 102)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, raw := range got {
		id, _ := eventMeta(raw)
		ids = append(ids, id)
	}
	want := []string{"evt_3", "evt_4"}
	if !slices.Equal(ids, want) {
		t.Fatalf("the walk must stop at the first older event: got %v, want %v", ids, want)
	}
}

func TestGapFillerReportsATruncatedReplay(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// An endless log, always newer than the cursor.
		var rows []string
		for range gapPageSize {
			i := n.Add(1)
			rows = append(rows, eventJSON(fmt.Sprintf("evt_%d", i), "customer.created", 9999))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"object":"list","data":[%s],"has_more":true}`, strings.Join(rows, ","))
	}))
	defer srv.Close()

	g := &apiGapFiller{client: api.New(srv.URL, "sk_test_unit", "dev", srv.Client())}
	got, truncated, err := g.eventsAfter(context.Background(), "evt_missing", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("a gap longer than the replay cap must be reported as truncated")
	}
	if len(got) != gapReplayPages*gapPageSize {
		t.Fatalf("replay should stop at the cap, got %d events", len(got))
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
