// Package api is a thin authenticated HTTP client for the BillKit API,
// used by the CLI's resource, trigger, and events commands.
package api

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Client talks to one BillKit API host with one key.
type Client struct {
	BaseURL string
	APIKey  string
	Version string
	HTTP    *http.Client
}

// New builds a client; baseURL falls back to the production host upstream.
func New(baseURL, apiKey, version string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Version: version,
		HTTP:    httpClient,
	}
}

// RequestOption tweaks a request before it's sent (e.g. an idempotency key).
type RequestOption func(*http.Request)

// WithIdempotencyKey sets the Idempotency-Key header so a retried create can't
// double-charge — matching every BillKit SDK's idempotency contract. A blank
// key is a no-op.
func WithIdempotencyKey(key string) RequestOption {
	return func(r *http.Request) {
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
	}
}

// NewIdempotencyKey mints a random Idempotency-Key for a mutating call, so a
// CLI create is never sent keyless. The "cli_" prefix mirrors the "sdk-"
// prefix the Python and Node SDKs use, making the origin of a deduped request
// obvious in the API's idempotency records.
func NewIdempotencyKey() (string, error) {
	buf := make([]byte, 24)
	if _, err := cryptorand.Read(buf); err != nil {
		return "", fmt.Errorf("could not generate an idempotency key: %w", err)
	}
	return "cli_" + base64.RawURLEncoding.EncodeToString(buf), nil
}

// APIError is a non-2xx response surfaced with the API's error envelope
// ({"error":{type,code,message,param?,reason?}}) parsed into fields where
// possible, with the raw Body always retained as a fallback.
type APIError struct {
	StatusCode int
	Body       string
	Type       string
	Code       string
	Message    string
	Param      string
	Reason     string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("billkit API error (HTTP %d): %s", e.StatusCode, e.Body)
	}
	head := fmt.Sprintf("(HTTP %d): %s", e.StatusCode, e.Message)
	if e.Code != "" {
		head = fmt.Sprintf("%s (HTTP %d): %s", e.Code, e.StatusCode, e.Message)
	}
	var extra []string
	if e.Reason != "" {
		extra = append(extra, "reason: "+e.Reason)
	}
	if e.Param != "" {
		extra = append(extra, "param: "+e.Param)
	}
	if len(extra) > 0 {
		head += " [" + strings.Join(extra, ", ") + "]"
	}
	return head
}

// ParseError turns a non-2xx body into a structured APIError, tolerating
// bodies that aren't the standard envelope (Body stays populated regardless).
//
// Exported for the one call that does not go through Do: the `listen` SSE
// stream builds its own request, and its errors should still read like every
// other API error the CLI prints.
func ParseError(status int, raw []byte) *APIError {
	e := &APIError{StatusCode: status, Body: strings.TrimSpace(string(raw))}
	var env struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
			Param   string `json:"param"`
			Reason  string `json:"reason"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err == nil {
		e.Type = env.Error.Type
		e.Code = env.Error.Code
		e.Message = env.Error.Message
		e.Param = env.Error.Param
		e.Reason = env.Error.Reason
	}
	return e
}

// Retry policy for replayable requests.
const (
	// maxRetries is how many *extra* attempts a retryable failure gets.
	maxRetries = 2
	// retryBaseDelay is the first backoff step; each retry doubles it.
	retryBaseDelay = 500 * time.Millisecond
	// retryMaxDelay caps both the computed backoff and a server Retry-After,
	// so a misbehaving proxy can't park the CLI for an hour.
	retryMaxDelay = 20 * time.Second
)

// safeMethod reports whether the HTTP method is safe to replay on its own,
// without an idempotency key.
func safeMethod(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// Do sends a request and returns the decoded JSON body (as raw bytes).
// `body` is JSON-encoded when non-nil.
//
// Every mutating request (anything but GET/HEAD/OPTIONS) goes out with an
// Idempotency-Key: the caller's when one was supplied, otherwise a freshly
// minted one. That is what makes the bounded retry below safe. A retried
// create is deduplicated by the API instead of charged twice, so a dropped
// connection stops being an unanswerable "did that refund happen?".
//
// Retries cover connection errors, timeouts, 429 and 5xx, at most twice,
// with exponential backoff plus full jitter and honouring Retry-After. A 4xx
// other than 429 is never retried, and neither is a request that somehow
// went out without a key.
func (c *Client) Do(ctx context.Context, method, path string, body any, opts ...RequestOption) ([]byte, error) {
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}

	// One key for the whole call, minted once: a retry that minted a fresh
	// key would be exactly the double-spend it is meant to prevent.
	autoKey := ""
	if !safeMethod(method) {
		var err error
		if autoKey, err = NewIdempotencyKey(); err != nil {
			return nil, err
		}
	}

	for attempt := 0; ; attempt++ {
		req, err := c.newRequest(ctx, method, path, encoded, opts, autoKey)
		if err != nil {
			return nil, err
		}
		replayable := safeMethod(method) || req.Header.Get("Idempotency-Key") != ""

		data, status, retryAfter, err := c.roundTrip(req)
		if err == nil && (status < 200 || status >= 300) {
			err = ParseError(status, data)
		}
		if err == nil {
			return data, nil
		}
		if attempt >= maxRetries || !replayable || !retryableFailure(status) || ctx.Err() != nil {
			return nil, err
		}
		if !sleepBackoff(ctx, attempt, retryAfter) {
			// The deadline ran out mid-backoff; report the failure that
			// actually happened rather than a context error.
			return nil, err
		}
	}
}

// newRequest builds one attempt. The body is re-wrapped each time so a retry
// has a readable stream.
func (c *Client) newRequest(ctx context.Context, method, path string, encoded []byte, opts []RequestOption, autoKey string) (*http.Request, error) {
	var reader io.Reader
	if encoded != nil {
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "billkit-cli/"+c.Version)
	if encoded != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, opt := range opts {
		opt(req)
	}
	if autoKey != "" && req.Header.Get("Idempotency-Key") == "" {
		req.Header.Set("Idempotency-Key", autoKey)
	}
	return req, nil
}

// roundTrip performs one attempt and fully reads the body so the connection
// can be reused by the next one.
func (c *Client) roundTrip(req *http.Request) (data []byte, status int, retryAfter time.Duration, err error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	data, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, 0, err
	}
	return data, resp.StatusCode, ParseRetryAfter(resp.Header.Get("Retry-After")), nil
}

// retryableFailure reports whether a failed attempt is worth repeating.
// status 0 means the request never got an answer (connection refused, reset,
// TLS failure, timeout), which is precisely the ambiguous case retry exists
// for now that the request carries a key.
func retryableFailure(status int) bool {
	return status == 0 || status == http.StatusTooManyRequests || status >= 500
}

// ParseRetryAfter reads the header in either of its legal forms, seconds or
// an HTTP-date. Anything else is ignored rather than guessed at.
func ParseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if secs, err := strconv.Atoi(value); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

// BackoffCeiling is the longest a caller should wait before retry attempt n
// (0-based): retryBaseDelay doubled per attempt, capped at retryMaxDelay.
//
// Exported because the long-running `billkit listen` reconnect loop backs off
// on this same curve. One retry policy for the whole CLI is easier to reason
// about, and to tune, than two that drift apart.
func BackoffCeiling(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	// 500ms doubled six times is already past the 20s cap, and `listen` can
	// keep counting attempts for hours, so clamp before shifting rather than
	// letting the shift overflow into a negative duration.
	if attempt > 6 {
		attempt = 6
	}
	return min(retryBaseDelay<<attempt, retryMaxDelay)
}

// Backoff is BackoffCeiling with full jitter applied, so a fleet of CLIs
// retrying after an outage doesn't arrive in lockstep. A server-supplied
// Retry-After wins within the cap: the server said when to come back, so
// believe it.
func Backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return min(retryAfter, retryMaxDelay)
	}
	// Not security-sensitive, so math/rand is fine.
	return time.Duration(rand.Int64N(int64(BackoffCeiling(attempt))))
}

// sleepBackoff waits before the next attempt and reports whether the wait
// completed (false means the context ended first).
func sleepBackoff(ctx context.Context, attempt int, retryAfter time.Duration) bool {
	return SleepFor(ctx, Backoff(attempt, retryAfter))
}

// SleepFor waits for d and reports whether the wait completed. False means
// the context ended first, which every caller must treat as "stop", not as
// "the wait is over".
func SleepFor(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
