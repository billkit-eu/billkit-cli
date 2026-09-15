package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/billkit-eu/billkit-cli/internal/api"
	"github.com/billkit-eu/billkit-cli/internal/sign"
	"github.com/spf13/cobra"
)

// Stream liveness and reconnect tuning.
const (
	// streamStallTimeout is how long an established stream may produce no
	// bytes at all before the CLI calls the socket dead. The server sends a
	// ":" keep-alive comment every 15s of idle (api/billkit/api/events.py),
	// so this is three missed keep-alives: silence this long is a dead
	// connection, not a quiet account.
	streamStallTimeout = 45 * time.Second
	// streamStallCheckStep is how often the watchdog compares the clock to
	// the last byte read.
	streamStallCheckStep = 5 * time.Second
	// streamHeaderTimeout catches the other stall: a host that accepts the
	// TCP connection and then never answers at all.
	streamHeaderTimeout = 30 * time.Second
	// streamSettleDelay is how long to wait after the stream's response
	// headers arrive before asking the event log what was missed. The server
	// picks the stream's cursor just after it writes those headers, so a
	// backfill issued in the same instant can race it. Waiting makes the two
	// windows overlap instead, and the overlap is dropped by event id.
	streamSettleDelay = 250 * time.Millisecond
	// streamStableFor is how long one connection must survive before the
	// reconnect backoff counts as recovered and resets to its floor.
	streamStableFor = 30 * time.Second
	// gapReplayPages caps a replay at gapReplayPages * gapPageSize events, so
	// a laptop that slept all weekend does not fire thousands of webhooks at
	// a local dev server. Anything past the cap is reported, not swallowed.
	gapReplayPages = 5
	gapPageSize    = 100
)

// errStreamStalled is an established connection that stopped producing bytes
// for longer than the server's keep-alive interval can explain.
var errStreamStalled = errors.New("stream stopped sending data")

func listenCmd() *cobra.Command {
	var forwardTo string
	var eventsFilter string
	var printJSON bool

	cmd := &cobra.Command{
		Use:   "listen",
		Short: "Stream live events and (optionally) forward them to a local URL",
		Long: "Open a live stream of the events your BillKit account emits and forward\n" +
			"each one to a local URL — no ngrok needed. Forwarded events are signed with\n" +
			"a fresh secret that's printed on start; verify them locally with any BillKit\n" +
			"SDK's webhook verifier.\n\n" +
			"  billkit listen --forward-to http://localhost:3000/billkit/webhook",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cr, err := resolve()
			if err != nil {
				return err
			}
			apiKey, baseURL := cr.apiKey, cr.baseURL
			if apiKey == "" {
				return fmt.Errorf("no API key — run `billkit login` or pass --api-key")
			}
			announceHost(cr)

			secret, err := sign.NewWebhookSecret()
			if err != nil {
				return err
			}

			errw := cmd.ErrOrStderr()
			mode := cr.mode
			if mode == "" {
				mode = "?"
			}
			fmt.Fprintf(errw, "> Ready! Streaming %s-mode events (Ctrl-C to quit)\n", mode)
			if forwardTo != "" {
				fmt.Fprintf(errw, "> Forwarding to %s\n", forwardTo)
				fmt.Fprintf(errw, "> Your webhook signing secret is %s\n", secret)
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			// With --print-json, stdout belongs to the JSON: the per-event
			// log lines move to stderr so the stream stays pipeable.
			logOut := cmd.OutOrStdout()
			if printJSON {
				logOut = errw
			}
			fwd := &forwarder{
				url:       forwardTo,
				secret:    secret,
				printJSON: printJSON,
				// A hung local receiver must not wedge the event stream.
				client: &http.Client{Timeout: 15 * time.Second},
				out:    cmd.OutOrStdout(),
				logOut: logOut,
			}

			l := newListener(baseURL, apiKey, eventsFilter, fwd, errw)
			l.fill = &apiGapFiller{
				client: api.New(baseURL, apiKey, Version, newHTTPClient(readTimeout+5*time.Second)),
			}
			return l.run(ctx)
		},
	}
	cmd.Flags().StringVar(&forwardTo, "forward-to", "", "local URL to POST each event to")
	cmd.Flags().StringVar(&eventsFilter, "events", "", "comma-separated event types to receive (default: all)")
	cmd.Flags().BoolVar(&printJSON, "print-json", false, "print each event's JSON to stdout")
	return cmd
}

// listener owns one `billkit listen` session: the reconnect loop, the stall
// watchdog, and the replay of the events a reconnect would otherwise skip.
type listener struct {
	baseURL string
	apiKey  string
	types   string
	fwd     *forwarder
	stream  *http.Client
	// fill reads the events missed between two connections. Nil disables
	// replay, which is honest but lossy, so the loop says so out loud.
	fill   gapFiller
	errOut io.Writer

	// Tunables. Fields rather than constants so the tests can drive the real
	// loop in milliseconds instead of minutes.
	stallTimeout time.Duration
	stallStep    time.Duration
	settle       time.Duration
	stableFor    time.Duration
	// wait pauses between reconnects. A seam so a test can observe the
	// backoff curve without sleeping through it.
	wait func(ctx context.Context, attempt int, retryAfter time.Duration) bool

	// Resume position: the newest event already handed to the forwarder.
	lastID      string
	lastCreated int64
	// replayed holds the ids this attempt's replay already delivered, so the
	// overlap the live stream repeats is dropped rather than forwarded twice.
	replayed map[string]bool
}

func newListener(baseURL, apiKey, types string, fwd *forwarder, errOut io.Writer) *listener {
	l := &listener{
		baseURL:      strings.TrimRight(baseURL, "/"),
		apiKey:       apiKey,
		types:        types,
		fwd:          fwd,
		stream:       newStreamHTTPClient(),
		errOut:       errOut,
		stallTimeout: streamStallTimeout,
		stallStep:    streamStallCheckStep,
		settle:       streamSettleDelay,
		stableFor:    streamStableFor,
	}
	l.wait = l.defaultWait
	return l
}

// newStreamHTTPClient is the client for the long-lived SSE connection: no
// overall Timeout, because the body is meant to stay open for hours, but a
// response-header deadline so a host that accepts the connection and then
// says nothing is caught instead of waited on forever. Liveness of an
// established stream is the stall watchdog's job, not the client's.
func newStreamHTTPClient() *http.Client {
	if transportOverride != nil {
		return &http.Client{Transport: transportOverride}
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Client{}
	}
	tr := base.Clone()
	tr.ResponseHeaderTimeout = streamHeaderTimeout
	return &http.Client{Transport: tr}
}

// run connects, forwards, and reconnects until the context is cancelled.
//
// It draws one line the old loop did not: a failure the server will keep
// giving the same answer to (a rejected key, a missing scope, a malformed
// filter) ends the command with an error, because retrying it forever and
// then exiting 0 tells a CI step the credentials work when they do not.
// Everything else is transient and is retried on the same exponential
// backoff the API client uses.
func (l *listener) run(ctx context.Context) error {
	l.anchor(ctx)

	var lastErr error
	attempt := 0
	connected := false
	for {
		start := time.Now()
		reached, err := l.streamOnce(ctx, connected)
		connected = connected || reached

		if ctx.Err() != nil {
			fmt.Fprintln(l.errOut, "\n> Stopped.")
			// Never once reached the stream: report the reason rather than
			// exiting 0, so a script that wraps `billkit listen` to smoke
			// test its credentials cannot read silence as success.
			if !connected {
				return lastErr
			}
			return nil
		}

		if err != nil {
			var status *streamStatusError
			if errors.As(err, &status) && status.fatal() {
				if hint := status.hint(); hint != "" {
					fmt.Fprintf(l.errOut, "! %s\n", hint)
				}
				return fmt.Errorf("the event stream refused the connection: %w", err)
			}
			lastErr = err
			fmt.Fprintf(l.errOut, "! Stream error: %v. Reconnecting.\n", err)
		}

		// A connection that lasted counts as recovery, so a stream that
		// drops once an hour does not creep up to the backoff ceiling.
		if time.Since(start) >= l.stableFor {
			attempt = 0
		}
		var retryAfter time.Duration
		var status *streamStatusError
		if errors.As(err, &status) {
			retryAfter = status.retryAfter
		}
		if !l.wait(ctx, attempt, retryAfter) {
			fmt.Fprintln(l.errOut, "\n> Stopped.")
			if !connected {
				return lastErr
			}
			return nil
		}
		attempt++
	}
}

// defaultWait pauses before the next reconnect on the same exponential
// backoff with full jitter the API client uses for money calls.
func (l *listener) defaultWait(ctx context.Context, attempt int, retryAfter time.Duration) bool {
	return api.SleepFor(ctx, api.Backoff(attempt, retryAfter))
}

// anchor seeds the resume cursor from the newest event that already exists,
// so a reconnect that happens before the first live event still knows where
// to resume from. Failure is silent on purpose: the stream needs the same
// scope, so a real permission problem is about to be reported properly, and
// a transient blip is reported later by fillGap if it ever matters.
func (l *listener) anchor(ctx context.Context) {
	if l.fill == nil {
		return
	}
	if id, created, err := l.fill.newest(ctx); err == nil {
		l.lastID, l.lastCreated = id, created
	}
}

// streamOnce opens the stream and forwards events until it ends. It reports
// whether the connection was ever established, which is what lets the caller
// tell "the key is wrong" from "the connection dropped".
func (l *listener) streamOnce(ctx context.Context, reconnect bool) (bool, error) {
	// Per-attempt cancellation: the watchdog uses it to unstick a parked
	// read, and the deferred cancel guarantees no goroutine outlives the
	// attempt that started it.
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	endpoint := l.baseURL + "/v1/events/stream"
	if l.types != "" {
		endpoint += "?types=" + url.QueryEscape(l.types)
	}
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+l.apiKey)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", "billkit-cli/"+Version)

	resp, err := l.stream.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return false, &streamStatusError{
			status:     resp.StatusCode,
			body:       strings.TrimSpace(string(body)),
			retryAfter: api.ParseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	l.fillGap(attemptCtx, reconnect)

	live := &liveReader{r: resp.Body}
	live.mark()
	watch := l.watchStall(attemptCtx, live, cancel)
	defer watch.stop()

	err = consumeSSE(attemptCtx, live, l.onFrame)
	if watch.fired() {
		return true, fmt.Errorf("%w for %s", errStreamStalled, l.stallTimeout)
	}
	return true, err
}

// onFrame routes one parsed SSE frame. The server's idle-ceiling "timeout"
// frame is announced rather than dropped: an unexplained reconnect once an
// hour is exactly the silence that makes a developer distrust the tool.
func (l *listener) onFrame(ctx context.Context, event string, data []byte) {
	switch event {
	case "message":
		l.deliver(ctx, data)
	case "timeout":
		fmt.Fprintln(l.errOut, "> The server closed the idle stream. Reconnecting.")
	}
}

// deliver forwards one live event and advances the resume cursor, skipping
// anything the replay already handed over a moment ago.
func (l *listener) deliver(ctx context.Context, raw []byte) {
	id, created := eventMeta(raw)
	if id != "" {
		if l.replayed[id] {
			delete(l.replayed, id)
			return
		}
		l.advance(id, created)
	}
	l.fwd.handle(ctx, raw)
}

// advance moves the resume cursor forward, and only forward. `listen` is a
// tail, not a history dump: a cursor that could move backwards would make the
// next reconnect replay events from before the point the user started
// listening.
func (l *listener) advance(id string, created int64) {
	if id == "" || created < l.lastCreated {
		return
	}
	l.lastID, l.lastCreated = id, created
}

// fillGap replays the events created since the last one forwarded.
//
// GET /v1/events/stream takes no resume cursor and re-anchors to the newest
// event every time it is opened, so the replay has to come from the event
// log instead. Whatever cannot be replayed is said out loud and paired with
// the command that reconciles it: a listener that silently skips events is
// worse than one that admits it, because the developer debugs their own app
// for hours instead.
func (l *listener) fillGap(ctx context.Context, reconnect bool) {
	l.replayed = nil
	if l.fill == nil || l.lastID == "" {
		if reconnect {
			fmt.Fprintln(l.errOut, "! Reconnected with no resume point, so events emitted while the stream was down were not replayed.")
			fmt.Fprintln(l.errOut, "!   Reconcile with: billkit events list")
		}
		return
	}
	if !api.SleepFor(ctx, l.settle) {
		return
	}

	events, truncated, err := l.fill.eventsAfter(ctx, l.lastID, l.lastCreated)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		fmt.Fprintf(l.errOut, "! Could not read the events missed while the stream was down: %v\n", err)
		fmt.Fprintln(l.errOut, "!   Reconcile with: billkit events list")
		return
	}
	if truncated {
		fmt.Fprintf(l.errOut, "! The gap was longer than %d events, so only the most recent %d are replayed.\n", gapReplayPages*gapPageSize, len(events))
		fmt.Fprintln(l.errOut, "!   Reconcile the rest with: billkit events list")
	}
	switch {
	case reconnect && len(events) == 0:
		fmt.Fprintln(l.errOut, "> Reconnected. No events were missed.")
	case reconnect:
		fmt.Fprintf(l.errOut, "> Reconnected. Replaying %d event(s) that arrived while the stream was down.\n", len(events))
	case len(events) > 0:
		// The first connection closes the window between reading the resume
		// point and the stream opening. Rare, but never silent: an event the
		// user did not expect to see is still better explained than not.
		fmt.Fprintf(l.errOut, "> Replaying %d event(s) created while the stream was opening.\n", len(events))
	}

	l.replayed = make(map[string]bool, len(events))
	for _, raw := range events {
		if ctx.Err() != nil {
			return
		}
		id, created := eventMeta(raw)
		l.advance(id, created)
		l.fwd.handle(ctx, raw)
		if id != "" {
			l.replayed[id] = true
		}
	}
}

// streamStatusError is a non-200 answer from GET /v1/events/stream.
type streamStatusError struct {
	status     int
	body       string
	retryAfter time.Duration
}

// Error reads the API's error envelope where there is one, so a rejected
// stream reports the same way every other call in the CLI does instead of
// dumping raw JSON at the user.
func (e *streamStatusError) Error() string {
	if e.body == "" {
		return fmt.Sprintf("HTTP %d", e.status)
	}
	parsed := api.ParseError(e.status, []byte(e.body))
	if parsed.Message == "" {
		return fmt.Sprintf("HTTP %d: %s", e.status, e.body)
	}
	return parsed.Error()
}

// fatal reports whether repeating this request unchanged could ever succeed.
// A 4xx is the server saying the request itself is wrong, so retrying it is
// noise. The two exceptions are the ones that mean "not now" rather than
// "not ever": 408 and 429.
func (e *streamStatusError) fatal() bool {
	switch e.status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return false
	}
	return e.status >= 400 && e.status < 500
}

// hint names the thing the user can actually change, because "HTTP 403" on
// its own does not tell anyone which key to swap.
func (e *streamStatusError) hint() string {
	switch e.status {
	case http.StatusUnauthorized:
		return "The API key was rejected. Check `billkit config list`, then run `billkit login` or pass a working --api-key."
	case http.StatusForbidden:
		return "This API key may not read events. Use a key that has the `events:read` scope."
	case http.StatusNotFound:
		return "This host has no event stream. Check --base-url."
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "The server rejected the request. Check the value passed to --events."
	}
	return ""
}

// liveReader records when the stream last produced bytes. That timestamp is
// the only thing that separates a quiet connection from a dead one, since a
// blocked Read looks identical to an idle account.
type liveReader struct {
	r    io.Reader
	last atomic.Int64 // unix nanos
}

func (lr *liveReader) mark()               { lr.last.Store(time.Now().UnixNano()) }
func (lr *liveReader) lastRead() time.Time { return time.Unix(0, lr.last.Load()) }

func (lr *liveReader) Read(p []byte) (int, error) {
	n, err := lr.r.Read(p)
	if n > 0 {
		lr.mark()
	}
	return n, err
}

// stallWatch is the watchdog over one stream attempt.
type stallWatch struct {
	cancel  context.CancelFunc
	tripped atomic.Bool
	done    chan struct{}
}

// fired reports whether the watchdog, rather than the server or the user,
// ended the attempt.
func (w *stallWatch) fired() bool { return w.tripped.Load() }

// stop ends the watchdog and waits for it, so no goroutine outlives the
// attempt that started it.
func (w *stallWatch) stop() {
	w.cancel()
	<-w.done
}

// watchStall cancels the attempt when the stream goes silent for longer than
// the server's keep-alive interval can explain. Cancelling closes the body,
// which unsticks the parked read and hands control back to the reconnect
// loop instead of hanging until the user notices, hours later, that nothing
// has arrived.
func (l *listener) watchStall(ctx context.Context, live *liveReader, cancel context.CancelFunc) *stallWatch {
	w := &stallWatch{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(l.stallStep)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if time.Since(live.lastRead()) < l.stallTimeout {
					continue
				}
				w.tripped.Store(true)
				cancel()
				return
			}
		}
	}()
	return w
}

// gapFiller reads the events a reconnect would otherwise skip.
type gapFiller interface {
	// newest returns the id and creation time of the most recent event, or
	// an empty id when the log holds none yet.
	newest(ctx context.Context) (string, int64, error)
	// eventsAfter returns the events created after the given position,
	// oldest first. truncated reports that the gap ran longer than the CLI
	// is willing to replay in one go.
	eventsAfter(ctx context.Context, lastID string, lastCreated int64) (events [][]byte, truncated bool, err error)
}

// apiGapFiller walks GET /v1/events, which is ordered newest first and
// cursor-paginated by `starting_after`, and needs the same `events:read`
// scope the stream already required.
type apiGapFiller struct{ client *api.Client }

func (g *apiGapFiller) newest(ctx context.Context) (string, int64, error) {
	page, _, err := g.page(ctx, 1, "")
	if err != nil || len(page) == 0 {
		return "", 0, err
	}
	id, created := eventMeta(page[0])
	return id, created, nil
}

func (g *apiGapFiller) eventsAfter(ctx context.Context, lastID string, lastCreated int64) ([][]byte, bool, error) {
	var newer [][]byte
	cursor := ""
	for range gapReplayPages {
		page, hasMore, err := g.page(ctx, gapPageSize, cursor)
		if err != nil {
			return nil, false, err
		}
		for _, raw := range page {
			id, created := eventMeta(raw)
			// The log is ordered (created desc, id desc), so either landmark
			// ends the walk: the exact event last forwarded, or the first
			// event older than it, in case that row has since been pruned.
			if id == lastID || created < lastCreated {
				slices.Reverse(newer)
				return newer, false, nil
			}
			newer = append(newer, raw)
			cursor = id
		}
		if !hasMore || cursor == "" {
			slices.Reverse(newer)
			return newer, false, nil
		}
	}
	slices.Reverse(newer)
	return newer, true, nil
}

func (g *apiGapFiller) page(ctx context.Context, limit int, cursor string) ([][]byte, bool, error) {
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	if cursor != "" {
		q.Set("starting_after", cursor)
	}
	raw, err := g.client.Do(ctx, http.MethodGet, "/v1/events?"+q.Encode(), nil)
	if err != nil {
		return nil, false, err
	}
	var env struct {
		Data    []json.RawMessage `json:"data"`
		HasMore bool              `json:"has_more"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, false, fmt.Errorf("could not read the event log: %w", err)
	}
	out := make([][]byte, len(env.Data))
	for i, item := range env.Data {
		out[i] = item
	}
	return out, env.HasMore, nil
}

// consumeSSE parses a text/event-stream and invokes onFrame with the name and
// raw data of each complete frame. ":" keep-alive comments carry no frame and
// are ignored here; they still count as liveness one layer down, in
// liveReader. It returns when the reader is exhausted, errors, or the context
// is cancelled.
func consumeSSE(ctx context.Context, r io.Reader, onFrame func(context.Context, string, []byte)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20) // up to 1 MiB per event
	var eventName, data string
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(line[len("data:"):])
		case line == "": // blank line terminates one SSE frame
			if eventName != "" && data != "" {
				onFrame(ctx, eventName, []byte(data))
			}
			eventName, data = "", ""
		}
	}
	return scanner.Err()
}

// eventMeta pulls the id and creation time out of one event payload. A
// payload that does not parse is still forwarded; it just cannot anchor the
// resume cursor.
func eventMeta(raw []byte) (string, int64) {
	var meta struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
	}
	_ = json.Unmarshal(raw, &meta)
	return meta.ID, meta.Created
}

// forwarder logs and (optionally) re-signs + POSTs each event to a local URL.
type forwarder struct {
	url       string
	secret    string
	printJSON bool
	client    *http.Client
	// out carries machine-readable output only: the raw event JSON under
	// --print-json. logOut carries the human-readable per-event lines, which
	// move to stderr under --print-json so stdout stays pipeable.
	out    io.Writer
	logOut io.Writer
}

func (f *forwarder) handle(ctx context.Context, raw []byte) {
	var meta struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &meta)

	if f.printJSON {
		fmt.Fprintln(f.out, string(raw))
	}
	if f.url == "" {
		fmt.Fprintf(f.logOut, "%s  %s  %s\n", time.Now().Format("15:04:05"), meta.Type, meta.ID)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.url, bytes.NewReader(raw))
	if err != nil {
		fmt.Fprintf(f.logOut, "  %s  %s  -> build error: %v\n", meta.Type, meta.ID, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "billkit-cli/"+Version)
	req.Header.Set("BillKit-Event-Type", meta.Type)
	req.Header.Set("BillKit-Signature", sign.Header(f.secret, time.Now().Unix(), raw))

	resp, err := f.client.Do(req)
	if err != nil {
		fmt.Fprintf(f.logOut, "  %s  %s  -> error: %v\n", meta.Type, meta.ID, err)
		return
	}
	_ = resp.Body.Close()
	fmt.Fprintf(f.logOut, "  %s  %s  -> %d\n", meta.Type, meta.ID, resp.StatusCode)
}
