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
	// streamStableFor is how long one connection must survive before the
	// reconnect backoff counts as recovered and resets to its floor.
	streamStableFor = 30 * time.Second
)

// errStreamStalled is an established connection that stopped producing bytes
// for longer than the server's keep-alive interval can explain.
var errStreamStalled = errors.New("stream stopped sending data")

func listenCmd(g *globals) *cobra.Command {
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
			// Flags first, before any credential is read or any host is
			// named. A mistyped flag is the user's to fix either way, and
			// reporting it after "> Using API host …" reads as though the
			// connection is what went wrong.
			//
			// --forward-to with no scheme ("localhost:3000/hook") is refused
			// by http.NewRequest on every single event, and a per-event
			// "build error" line reads as a broken webhook handler rather
			// than as a mistyped flag.
			if err := validateForwardURL(forwardTo); err != nil {
				return err
			}
			// The server rejects an unknown event type too, but a developer
			// opening a long-lived stream should learn about a typo now, not
			// from a 400 buried in reconnect output.
			if unknown := validateEventTypes(eventsFilter); len(unknown) > 0 {
				return fmt.Errorf(
					"unknown event type(s) in --events: %s\n"+
						"Omit --events to receive every event, or run "+
						"`billkit api GET /v1/webhook_endpoints/event_types` for the %d available",
					strings.Join(unknown, ", "), len(knownEventTypes),
				)
			}

			cr, err := resolve(g)
			if err != nil {
				return err
			}
			apiKey, baseURL := cr.apiKey, cr.baseURL
			if apiKey == "" {
				return fmt.Errorf("no API key — run `billkit login` or pass --api-key")
			}
			announceHost(cmd.ErrOrStderr(), g, cr)

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
			l.anchor = &apiEventAnchor{
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
// watchdog, and the resume cursor that makes a reconnect gapless.
type listener struct {
	baseURL string
	apiKey  string
	types   string
	fwd     *forwarder
	stream  *http.Client
	// anchor reads the newest event id once, before the first connection,
	// so a reconnect that happens before anything has arrived still has a
	// resume point. Nil disables that seeding, which is honest but lossy, so
	// the loop says so out loud.
	anchor eventAnchor
	errOut io.Writer

	// Tunables. Fields rather than constants so the tests can drive the real
	// loop in milliseconds instead of minutes.
	stallTimeout time.Duration
	stallStep    time.Duration
	stableFor    time.Duration
	// wait pauses between reconnects. A seam so a test can observe the
	// backoff curve without sleeping through it.
	wait func(ctx context.Context, attempt int, retryAfter time.Duration) bool

	// resumeAfter is the id of the newest event already handed to the
	// forwarder, and it is the whole of the CLI's gap handling: it goes back
	// as `starting_after`, and the server replays everything recorded after
	// it, in order and under the same `types` filter, before going live.
	resumeAfter string
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
	// No transparent gzip on a stream. Go's automatic decompression is fine
	// for a response that ends, but an intermediary that gzips an SSE feed
	// buffers frames until it has a block worth compressing, so events
	// arrive in clumps, and a quiet period looks to the stall watchdog like
	// a dead socket while bytes are in fact sitting in a proxy.
	tr.DisableCompression = true
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
	l.seedResume(ctx)

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
			isStatus := errors.As(err, &status)
			switch {
			case isStatus && l.resumeAfter != "" && status.staleCursor():
				// Not fatal, even though it is a 4xx: the request is only
				// wrong because of a cursor this loop owns and can drop.
				// Exiting here would kill a listener that has been running
				// for days over an event that aged out of the log.
				fmt.Fprintf(l.errOut, "! The resume point %s is no longer in the event log, so anything recorded since then was not replayed.\n", l.resumeAfter)
				fmt.Fprintln(l.errOut, "!   Reconcile with: billkit events list")
				l.resumeAfter = ""
				lastErr = err
			case isStatus && status.fatal():
				if hint := status.hint(); hint != "" {
					fmt.Fprintf(l.errOut, "! %s\n", hint)
				}
				return fmt.Errorf("the event stream refused the connection: %w", err)
			default:
				lastErr = err
				fmt.Fprintf(l.errOut, "! Stream error: %v. Reconnecting.\n", err)
			}
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

// seedResume reads the newest event that already exists, so a reconnect
// happening before the first live event still knows where to resume from.
// Without it, the window between start-up and the first delivered event is
// the one gap `starting_after` cannot close, because there is no id yet.
//
// Failure is silent on purpose: the stream needs the same events:read scope,
// so a real permission problem is about to be reported properly by the
// connection itself, and a transient blip costs only the seed — which the
// first delivered event replaces anyway, and whose absence the reconnect
// notice says out loud.
func (l *listener) seedResume(ctx context.Context) {
	if l.anchor == nil {
		return
	}
	if id, err := l.anchor.newest(ctx); err == nil {
		l.resumeAfter = id
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
	q := url.Values{}
	if l.types != "" {
		q.Set("types", l.types)
	}
	// The resume cursor. GET /v1/events/stream replays everything recorded
	// after this id, in order and under the same types filter, before it goes
	// live — which is why this CLI no longer walks GET /v1/events itself. The
	// backfill it used to do was capped at 500 events, and it read the event
	// log unfiltered, so every reconnect under --events forwarded event types
	// the user had explicitly excluded.
	if l.resumeAfter != "" {
		q.Set("starting_after", l.resumeAfter)
	}
	if encoded := q.Encode(); encoded != "" {
		endpoint += "?" + encoded
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

	l.announceResume(reconnect)

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
func (l *listener) onFrame(ctx context.Context, f sseFrame) {
	if f.dropped > 0 {
		l.reportOversized(f)
		return
	}
	switch f.event {
	case "message":
		l.deliver(ctx, f.data)
	case "timeout":
		fmt.Fprintln(l.errOut, "> The server closed the idle stream. Reconnecting.")
	}
}

// reportOversized handles an event too large to buffer: it names it, says
// where to read it, and moves the cursor past it.
//
// Skipping is the only option that terminates. The frame cannot be parsed, so
// it cannot be signed or forwarded; and since the resume cursor makes the
// server replay from the last event delivered, failing the connection would
// fetch the same oversized row on every reconnect, forever, at an ever-longer
// backoff. That is a listener that looks alive and has stopped working.
//
// Advancing the cursor past an event that was never forwarded is a real loss,
// which is why it is reported with the command that reads it rather than
// logged and forgotten. The id comes from the frame's `id:` field, which the
// server writes before the payload, so it survives even when the payload does
// not.
func (l *listener) reportOversized(f sseFrame) {
	id := f.id
	if id == "" {
		id = "(no id)"
	}
	fmt.Fprintf(l.errOut,
		"! Event %s is larger than the %d MiB this CLI buffers per event (at least %d byte(s) over), so it was NOT forwarded.\n",
		id, maxSSEFrameBytes>>20, f.dropped)
	if f.id != "" {
		fmt.Fprintf(l.errOut, "!   Read it with: billkit events retrieve %s\n", f.id)
		// Past it, so the next reconnect does not replay the same row and
		// stall here again.
		l.resumeAfter = f.id
	} else {
		fmt.Fprintln(l.errOut, "!   Reconcile with: billkit events list")
	}
}

// deliver forwards one event and advances the resume cursor past it.
//
// The cursor moves after the forward, not before, so it always means "already
// handed over". There is no de-duplication left to do: the server delivers
// each event once per connection and resumes strictly after the cursor, so
// the overlap the old client-side backfill had to filter cannot occur.
func (l *listener) deliver(ctx context.Context, raw []byte) {
	l.fwd.handle(ctx, raw)
	if id := eventID(raw); id != "" {
		l.resumeAfter = id
	}
}

// announceResume says what this connection is going to do about the gap
// before it, because a reconnect that silently skips events is the failure
// that has the developer debugging their own app for hours.
func (l *listener) announceResume(reconnect bool) {
	if !reconnect {
		return
	}
	if l.resumeAfter == "" {
		fmt.Fprintln(l.errOut, "! Reconnected with no resume point, so events emitted while the stream was down were not replayed.")
		fmt.Fprintln(l.errOut, "!   Reconcile with: billkit events list")
		return
	}
	fmt.Fprintf(l.errOut, "> Reconnected. Replaying anything recorded after %s.\n", l.resumeAfter)
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

// staleCursor reports whether the server refused the resume point itself.
// An event id ages out of the log (api/billkit/workers/retention.py prunes
// it), and from then on every reconnect carrying it gets the same 400. It is
// the one 4xx the loop does not treat as fatal, because dropping the cursor
// makes the very next attempt valid — lossy, and said out loud, but alive.
//
// Keyed on the envelope's `param`, so a 400 about the `types` filter still
// ends the command rather than being retried forever without its cursor.
func (e *streamStatusError) staleCursor() bool {
	if e.status != http.StatusBadRequest || e.body == "" {
		return false
	}
	return api.ParseError(e.status, []byte(e.body)).Param == "starting_after"
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

// eventAnchor reads the newest event already recorded, which is where a
// listener that has not received anything yet resumes from.
type eventAnchor interface {
	newest(ctx context.Context) (string, error)
}

// apiEventAnchor reads GET /v1/events, which is ordered newest first and
// needs the same events:read scope the stream already required. It asks for
// one row and ignores the types filter on purpose: the answer is a cursor,
// and the server re-applies the filter to everything it replays after it.
type apiEventAnchor struct{ client *api.Client }

func (a *apiEventAnchor) newest(ctx context.Context) (string, error) {
	raw, err := a.client.Do(ctx, http.MethodGet, "/v1/events?limit=1", nil)
	if err != nil {
		return "", err
	}
	var env struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("could not read the event log: %w", err)
	}
	if len(env.Data) == 0 {
		return "", nil
	}
	return eventID(env.Data[0]), nil
}

// SSE reading limits.
const (
	// sseReadBuffer is the working buffer for one line. Lines longer than it
	// are read in pieces rather than failing, so it is a throughput knob and
	// not a ceiling.
	sseReadBuffer = 64 * 1024
	// maxSSEFrameBytes is the most event payload the CLI will hold in memory
	// for one frame. A BillKit event is a resource snapshot with a bounded
	// metadata bag, so this is far above anything the API emits; it exists so
	// a pathological or hostile payload cannot exhaust memory on a developer
	// laptop.
	maxSSEFrameBytes = 1 << 20 // 1 MiB
)

// sseFrame is one complete frame off the stream.
type sseFrame struct {
	// id is the frame's `id:` field, which for BillKit is the event id. The
	// server writes it before the payload, so it identifies a frame whose
	// data was too large to keep.
	id    string
	event string
	data  []byte
	// dropped is how many bytes of this frame were discarded for exceeding
	// maxSSEFrameBytes. Non-zero means data is incomplete and must never be
	// forwarded: half an event that still parses as JSON is the worst thing a
	// webhook relay can deliver.
	dropped int
}

// consumeSSE parses a text/event-stream and invokes onFrame for each complete
// frame. ":" keep-alive comments carry no frame and are ignored here; they
// still count as liveness one layer down, in liveReader. It returns when the
// reader is exhausted, errors, or the context is cancelled.
//
// Repeated `data:` lines within one frame are joined with newlines, which is
// what the SSE spec says they mean. BillKit's stream encodes each event with
// json.dumps and so only ever writes one, but overwriting instead of
// appending would turn any future multi-line payload into a truncated
// fragment that still parses as JSON often enough to be forwarded — the worst
// available failure mode for a webhook relay.
//
// An oversized frame is reported through onFrame with `dropped` set, not
// turned into a stream error. This used to be a bufio.Scanner, which fails
// the whole connection on a token longer than its buffer: combined with the
// resume cursor, the server would replay the same oversized event on every
// reconnect and the listener would spin on one row forever. Reading past it
// and saying so is the only behaviour that both keeps the stream moving and
// keeps the loss visible.
func consumeSSE(ctx context.Context, r io.Reader, onFrame func(context.Context, sseFrame)) error {
	br := bufio.NewReaderSize(r, sseReadBuffer)
	var (
		frame sseFrame
		data  []string
		size  int
	)
	reset := func() {
		frame, data, size = sseFrame{}, nil, 0
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line, over, err := sseLine(br, maxSSEFrameBytes)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		frame.dropped += over

		switch {
		case strings.HasPrefix(line, "id:"):
			frame.id = strings.TrimSpace(line[len("id:"):])
		case strings.HasPrefix(line, "event:"):
			frame.event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			// Already known incomplete (this line overflowed, or an earlier
			// one did), so there is nothing worth keeping: the frame can only
			// be reported, never forwarded.
			if frame.dropped > 0 {
				break
			}
			chunk := strings.TrimSpace(line[len("data:"):])
			if size+len(chunk) > maxSSEFrameBytes {
				frame.dropped += len(chunk)
				break
			}
			size += len(chunk)
			data = append(data, chunk)
		case line == "": // blank line terminates one SSE frame
			frame.data = []byte(strings.Join(data, "\n"))
			// The SSE spec's default: a frame carrying data but no `event:`
			// line is a "message". BillKit always writes `event: message`
			// (api/billkit/api/events.py, `_sse`), so this changes nothing
			// today. It is a contract guard, so an intermediary that strips
			// the line, or a future frame that omits it, delivers the event
			// rather than dropping it silently.
			if frame.event == "" && len(frame.data) > 0 {
				frame.event = "message"
			}
			if frame.dropped > 0 || (frame.event != "" && len(frame.data) > 0) {
				onFrame(ctx, frame)
			}
			reset()
		}
	}
}

// sseLine reads one line, keeping at most limit bytes of it and reporting how
// many it threw away.
//
// ReadSlice rather than ReadString or a Scanner, because both of those grow a
// buffer to fit whatever arrives: a single unterminated multi-gigabyte line
// would be read entirely into memory before anyone could decide it was too
// long. ReadSlice hands back what fits in the fixed buffer and says there is
// more, which is what makes discarding the rest incremental.
func sseLine(br *bufio.Reader, limit int) (line string, dropped int, err error) {
	var kept []byte
	for {
		chunk, e := br.ReadSlice('\n')
		if room := limit - len(kept); room > 0 {
			n := min(len(chunk), room)
			// Copied immediately: ReadSlice returns a view into the reader's
			// own buffer, which the next read overwrites.
			kept = append(kept, chunk[:n]...)
			dropped += len(chunk) - n
		} else {
			dropped += len(chunk)
		}
		switch {
		case e == nil:
			return strings.TrimRight(string(kept), "\r\n"), dropped, nil
		case errors.Is(e, bufio.ErrBufferFull):
			continue
		case errors.Is(e, io.EOF) && (len(kept) > 0 || dropped > 0):
			// A final line the server never terminated. It is still a line.
			return strings.TrimRight(string(kept), "\r\n"), dropped, nil
		default:
			return "", dropped, e
		}
	}
}

// eventID pulls the id out of one event payload. A payload that does not
// parse is still forwarded; it just cannot advance the resume cursor.
func eventID(raw []byte) string {
	var meta struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &meta)
	return meta.ID
}

// validateForwardURL rejects a --forward-to that http.NewRequest would refuse
// on every event, so the mistake is reported once, at start-up, instead of
// once per delivery. An empty value is fine: it means "log, do not forward".
func validateForwardURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid --forward-to %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid --forward-to %q: expected an http:// or https:// URL", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid --forward-to %q: no host", raw)
	}
	return nil
}

// forwardBackoff is the pause before each retry of a failed local delivery.
// Its length is also the attempt budget: three attempts in total, spread over
// a little under four seconds, which covers the restart of a dev server
// without holding the stream up long enough to matter.
var forwardBackoff = []time.Duration{250 * time.Millisecond, time.Second, 2 * time.Second}

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
	// backoff overrides forwardBackoff. A field so the tests can drive the
	// real retry path in microseconds; nil everywhere else.
	backoff []time.Duration
}

// handle forwards one event, retrying a delivery that never reached the local
// app at all.
//
// Only a connection error or a timeout is retried, never an HTTP response of
// any status. A 500 from the developer's handler is that handler's answer to
// this event: a real webhook endpoint would get a redelivery, but a real
// endpoint also gets an idempotent handler written against one, and quietly
// re-posting the same body into a local app that answered would manufacture
// duplicates the production dispatcher's own schedule would not.
//
// A refused connection is different. `deliver` advances the resume cursor
// after this returns, so before this loop existed an event that arrived in
// the second a dev server spends restarting on a file save was logged once
// and then gone for good: the listener still looked healthy, and the event
// was never coming back.
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

	backoff := f.backoff
	if backoff == nil {
		backoff = forwardBackoff
	}
	var lastErr error
	for attempt := 1; attempt <= len(backoff)+1; attempt++ {
		status, err := f.post(ctx, raw, meta.Type, meta.ID, attempt)
		if err == nil {
			fmt.Fprintf(f.logOut, "  %s  %s  -> %d\n", meta.Type, meta.ID, status)
			return
		}
		lastErr = err
		if attempt > len(backoff) {
			break
		}
		fmt.Fprintf(f.logOut, "  %s  %s  -> %v (retrying in %s)\n", meta.Type, meta.ID, err, backoff[attempt-1])
		// Inside the attempt context, so Ctrl-C still stops promptly rather
		// than waiting out the longest step.
		if !api.SleepFor(ctx, backoff[attempt-1]) {
			return
		}
	}
	fmt.Fprintf(f.logOut, "  %s  %s  -> NOT forwarded after %d attempts: %v\n",
		meta.Type, meta.ID, len(backoff)+1, lastErr)
	if meta.ID != "" {
		fmt.Fprintf(f.logOut, "!   Read it with: billkit events retrieve %s\n", meta.ID)
	}
}

// post makes one delivery attempt. It returns the response status, or an
// error when the request never got one.
//
// The header set matches what the real dispatcher sends
// (api/billkit/services/webhook_dispatcher.py), because the whole point of
// `listen` is that code written against it works unchanged in production. It
// used to omit BillKit-Event-Id, so a handler that deduped on that header
// read an empty string locally, passed every local test, and deduped nothing
// once deployed.
//
// Two of them differ deliberately. The User-Agent carries both identities, so
// a log can still tell a relayed event from a real delivery. And
// BillKit-Delivery-Attempt counts this loop's attempts rather than always
// saying 1: a retried forward really is a second attempt at the same event,
// which is exactly what the header means on the wire.
func (f *forwarder) post(ctx context.Context, raw []byte, eventType, eventID string, attempt int) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.url, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "BillKit-Webhook/1.0 billkit-cli/"+Version)
	req.Header.Set("BillKit-Signature", sign.Header(f.secret, time.Now().Unix(), raw))
	req.Header.Set("BillKit-Event-Id", eventID)
	req.Header.Set("BillKit-Event-Type", eventType)
	req.Header.Set("BillKit-Delivery-Attempt", strconv.Itoa(attempt))

	resp, err := f.client.Do(req)
	if err != nil {
		return 0, err
	}
	// Drain before closing, or net/http cannot reuse the connection and a
	// busy stream opens a fresh socket to the local app per event.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}
