package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDoSetsHeadersAndDecodesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer bk_test_x" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "billkit-cli/1.2.3" {
			t.Errorf("User-Agent = %q", got)
		}
		if r.Method == http.MethodPost {
			if ct := r.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q", ct)
			}
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			if body["email"] != "a@b.co" {
				t.Errorf("body = %v", body)
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"cus_1"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "bk_test_x", "1.2.3", srv.Client())
	out, err := c.Do(context.Background(), http.MethodPost, "/v1/customers", map[string]any{"email": "a@b.co"})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"id":"cus_1"}` {
		t.Fatalf("out = %s", out)
	}
}

func TestDoMapsNonSuccessToAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "bk_test_x", "1.0", srv.Client())
	_, err := c.Do(context.Background(), http.MethodGet, "/v1/customers/x", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Fatalf("StatusCode = %d", apiErr.StatusCode)
	}
	if apiErr.Body != `{"error":{"message":"nope"}}` {
		t.Fatalf("Body = %q", apiErr.Body)
	}
}

func TestDoParsesErrorEnvelopeFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"parameter_invalid",` +
			`"message":"Missing required parameter.","param":"amount_cents","reason":"no_active_mandate"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "bk_test_x", "1.0", srv.Client())
	_, err := c.Do(context.Background(), http.MethodPost, "/v1/refunds", map[string]any{})
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("error type = %T", err)
	}
	if apiErr.Code != "parameter_invalid" || apiErr.Param != "amount_cents" || apiErr.Reason != "no_active_mandate" {
		t.Fatalf("parsed fields = %+v", apiErr)
	}
	msg := apiErr.Error()
	for _, want := range []string{"parameter_invalid", "HTTP 422", "Missing required parameter", "reason: no_active_mandate", "param: amount_cents"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("Error() = %q, missing %q", msg, want)
		}
	}
}

func TestAPIErrorMessageWithoutCodeKeepsStatus(t *testing.T) {
	e := &APIError{StatusCode: 500, Message: "boom", Body: `{"error":{"message":"boom"}}`}
	got := e.Error()
	if !strings.Contains(got, "HTTP 500") || !strings.Contains(got, "boom") {
		t.Fatalf("Error() = %q, want it to keep the status + message", got)
	}
}

func TestParseAPIErrorToleratesNonEnvelope(t *testing.T) {
	e := ParseError(502, []byte("<html>bad gateway</html>"))
	if e.StatusCode != 502 || e.Code != "" || e.Message != "" {
		t.Fatalf("parsed = %+v, want raw body only", e)
	}
	if !strings.Contains(e.Error(), "HTTP 502") {
		t.Fatalf("Error() = %q", e.Error())
	}
}

func TestWithIdempotencyKeySetsHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Idempotency-Key")
		_, _ = w.Write([]byte(`{"id":"re_1"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "bk_test_x", "1.0", srv.Client())
	if _, err := c.Do(context.Background(), http.MethodPost, "/v1/refunds", map[string]any{"payment_id": "pay_1"}, WithIdempotencyKey("idem-123")); err != nil {
		t.Fatal(err)
	}
	if got != "idem-123" {
		t.Fatalf("Idempotency-Key = %q", got)
	}
	// A blank key means "I didn't pick one", not "send none": the client
	// mints one rather than letting a mutating call go out unprotected.
	got = "unset"
	if _, err := c.Do(context.Background(), http.MethodPost, "/v1/refunds", map[string]any{}, WithIdempotencyKey("")); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "cli_") {
		t.Fatalf("blank key should still send a generated key, got %q", got)
	}
}

// TestRetryReusesTheSameIdempotencyKey is the point of the whole retry
// design: a second attempt that minted a fresh key would be the double-spend
// it is supposed to prevent.
func TestRetryReusesTheSameIdempotencyKey(t *testing.T) {
	var (
		mu   sync.Mutex
		keys []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		n := len(keys)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"try later"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"re_1"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "bk_test_x", "1.0", srv.Client())
	out, err := c.Do(context.Background(), http.MethodPost, "/v1/refunds", map[string]any{"payment_id": "pay_1"})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"id":"re_1"}` {
		t.Fatalf("out = %s", out)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 2 {
		t.Fatalf("attempts = %d, want 2 (one failure + one retry)", len(keys))
	}
	if keys[0] == "" {
		t.Fatal("the first attempt went out with no Idempotency-Key")
	}
	if keys[0] != keys[1] {
		t.Fatalf("retry used a different key: %q then %q — that is a second real charge", keys[0], keys[1])
	}
}

func TestRetryStopsAfterTwoExtraAttempts(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := New(srv.URL, "bk_test_x", "1.0", srv.Client())
	if _, err := c.Do(context.Background(), http.MethodPost, "/v1/refunds", map[string]any{}); err == nil {
		t.Fatal("expected the 502 to surface after the retries were exhausted")
	}
	if got := atomic.LoadInt64(&hits); got != 3 {
		t.Fatalf("attempts = %d, want 3 (initial + 2 retries)", got)
	}
}

func TestNoRetryOnClientError(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":{"message":"bad amount"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "bk_test_x", "1.0", srv.Client())
	if _, err := c.Do(context.Background(), http.MethodPost, "/v1/refunds", map[string]any{}); err == nil {
		t.Fatal("expected an error")
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("attempts = %d, want 1 — a 4xx is the caller's fault and never improves on retry", got)
	}
}

func TestRetryHonoursRetryAfterOn429(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt64(&hits, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "bk_test_x", "1.0", srv.Client())
	start := time.Now()
	if _, err := c.Do(context.Background(), http.MethodGet, "/v1/refunds", nil); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("waited %s, want at least the 1s the server asked for", elapsed)
	}
	if got := atomic.LoadInt64(&hits); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if got := ParseRetryAfter("3"); got != 3*time.Second {
		t.Errorf("ParseRetryAfter(\"3\") = %v", got)
	}
	if got := ParseRetryAfter(""); got != 0 {
		t.Errorf("ParseRetryAfter(\"\") = %v", got)
	}
	if got := ParseRetryAfter("-5"); got != 0 {
		t.Errorf("ParseRetryAfter(\"-5\") = %v", got)
	}
	if got := ParseRetryAfter("nonsense"); got != 0 {
		t.Errorf("ParseRetryAfter(\"nonsense\") = %v", got)
	}
	future := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	if got := ParseRetryAfter(future); got <= 0 {
		t.Errorf("ParseRetryAfter(http-date) = %v, want a positive wait", got)
	}
}

func TestNewIdempotencyKeyIsUniqueAndPrefixed(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		key, err := NewIdempotencyKey()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(key, "cli_") {
			t.Fatalf("key = %q, want a cli_ prefix", key)
		}
		if seen[key] {
			t.Fatalf("duplicate key %q", key)
		}
		seen[key] = true
	}
}

// TestBackoffCeilingGrowsAndCaps pins the curve that both the request retry
// and the `billkit listen` reconnect loop wait on.
func TestBackoffCeilingGrowsAndCaps(t *testing.T) {
	want := []time.Duration{
		500 * time.Millisecond,
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		20 * time.Second, // capped from 32s
	}
	for attempt, expect := range want {
		if got := BackoffCeiling(attempt); got != expect {
			t.Errorf("BackoffCeiling(%d) = %v, want %v", attempt, got, expect)
		}
	}
	// `listen` keeps counting attempts for as long as the API is down, so a
	// large attempt number must stay at the cap rather than overflow.
	for _, attempt := range []int{-1, 7, 64, 1 << 20} {
		got := BackoffCeiling(attempt)
		if got <= 0 || got > retryMaxDelay {
			t.Errorf("BackoffCeiling(%d) = %v, want a positive delay no larger than %v", attempt, got, retryMaxDelay)
		}
	}
}

func TestBackoffStaysUnderItsCeilingAndPrefersRetryAfter(t *testing.T) {
	for attempt := range 8 {
		for range 200 {
			got := Backoff(attempt, 0)
			if got < 0 || got >= BackoffCeiling(attempt) {
				t.Fatalf("Backoff(%d, 0) = %v, want [0, %v)", attempt, got, BackoffCeiling(attempt))
			}
		}
	}
	if got := Backoff(0, 3*time.Second); got != 3*time.Second {
		t.Errorf("Backoff with Retry-After = %v, want 3s", got)
	}
	if got := Backoff(0, time.Hour); got != retryMaxDelay {
		t.Errorf("Backoff with an absurd Retry-After = %v, want the %v cap", got, retryMaxDelay)
	}
}

func TestSleepForReportsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if SleepFor(ctx, 50*time.Millisecond) {
		t.Error("SleepFor must report false when the context ended first")
	}
	if !SleepFor(context.Background(), time.Millisecond) {
		t.Error("SleepFor must report true when the wait completed")
	}
}
