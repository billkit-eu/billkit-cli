package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// recorder is a stub BillKit API that remembers what the CLI actually sent.
type recorder struct {
	mu       sync.Mutex
	requests []recordedRequest
}

type recordedRequest struct {
	method string
	path   string
	auth   string
	idem   string
}

func (r *recorder) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.requests = append(r.requests, recordedRequest{
			method: req.Method,
			path:   req.URL.Path,
			auth:   req.Header.Get("Authorization"),
			idem:   req.Header.Get("Idempotency-Key"),
		})
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"re_1"}`))
	})
}

// reset forgets everything recorded so far, for a test that drives the same
// stub through several cases.
func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = nil
}

func (r *recorder) all() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedRequest(nil), r.requests...)
}

// mutations returns the non-GET requests, i.e. the ones that changed something.
func (r *recorder) mutations() []recordedRequest {
	var out []recordedRequest
	for _, req := range r.all() {
		if req.method != http.MethodGet {
			out = append(out, req)
		}
	}
	return out
}

// runCLI drives the real command tree with a non-terminal stdin, and returns
// everything the CLI wrote to stderr.
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := rootCmd()
	var errBuf bytes.Buffer
	root.SetOut(io.Discard)
	root.SetErr(&errBuf)
	root.SetIn(strings.NewReader(""))
	root.SetArgs(args)
	err := root.Execute()
	return errBuf.String(), err
}

// tlsStub starts an HTTPS stub and points the CLI's transport at it, so live
// keys can be exercised without weakening the https-only transport rule.
func tlsStub(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	prev := transportOverride
	transportOverride = srv.Client().Transport
	t.Cleanup(func() {
		transportOverride = prev
		srv.Close()
	})
	return srv
}

// TestRefundsCreateAlwaysSendsAnIdempotencyKey is the P0-2 regression for the
// refund path. Before the fix, `--idempotency-key` defaulted to "" and
// WithIdempotencyKey("") was an explicit no-op, so an ambiguous timeout left
// the user no safe retry and a re-run created a second real refund.
func TestRefundsCreateAlwaysSendsAnIdempotencyKey(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	stderr, err := runCLI(t,
		"refunds", "create", "--payment", "pay_123", "--amount", "500",
		"--api-key", "sk_test_x", "--base-url", srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	mutations := rec.mutations()
	if len(mutations) != 1 {
		t.Fatalf("mutating requests = %d, want 1", len(mutations))
	}
	if mutations[0].idem == "" {
		t.Fatal("POST /v1/refunds went out with no Idempotency-Key — a retry after a timeout refunds twice")
	}
	if !strings.Contains(stderr, mutations[0].idem) {
		t.Fatalf("the key must be printed before the call so a retry can reuse it; stderr = %q", stderr)
	}
}

func TestRefundsCreateKeepsAUserSuppliedKey(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	if _, err := runCLI(t,
		"refunds", "create", "--payment", "pay_123", "--amount", "500",
		"--idempotency-key", "mine-42",
		"--api-key", "sk_test_x", "--base-url", srv.URL); err != nil {
		t.Fatal(err)
	}
	mutations := rec.mutations()
	if len(mutations) != 1 || mutations[0].idem != "mine-42" {
		t.Fatalf("Idempotency-Key = %+v, want the user's own key", mutations)
	}
}

// TestCheckoutOneShotAlwaysSendsAnIdempotencyKey is the P0-2 regression for
// the charge path, where a blind re-run creates a second real Mollie charge.
func TestCheckoutOneShotAlwaysSendsAnIdempotencyKey(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	stderr, err := runCLI(t,
		"checkout", "one-shot", "--customer", "cus_1", "--amount", "1999",
		"--method", "ideal", "--success-url", "https://example.test/ok",
		"--api-key", "sk_test_x", "--base-url", srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	mutations := rec.mutations()
	if len(mutations) != 1 {
		t.Fatalf("mutating requests = %d, want 1", len(mutations))
	}
	if mutations[0].idem == "" {
		t.Fatal("POST /v1/checkout/one_shot went out with no Idempotency-Key — a retry charges the customer twice")
	}
	if !strings.Contains(stderr, mutations[0].idem) {
		t.Fatalf("the key must be printed before the call; stderr = %q", stderr)
	}
}

func TestCheckoutOneShotKeepsAUserSuppliedKey(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	if _, err := runCLI(t,
		"checkout", "one-shot", "--customer", "cus_1", "--amount", "1999",
		"--method", "ideal", "--success-url", "https://example.test/ok",
		"--idempotency-key", "mine-99",
		"--api-key", "sk_test_x", "--base-url", srv.URL); err != nil {
		t.Fatal(err)
	}
	mutations := rec.mutations()
	if len(mutations) != 1 || mutations[0].idem != "mine-99" {
		t.Fatalf("Idempotency-Key = %+v, want the user's own key", mutations)
	}
}

// TestAPICommandKeysNonGETRequests covers the generic escape hatch, which had
// no --idempotency-key flag at all and could POST /v1/refunds keyless.
func TestAPICommandKeysNonGETRequests(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	if _, err := runCLI(t, "api", "POST", "/v1/customers", "--data", "email=ada@example.test",
		"--api-key", "sk_test_x", "--base-url", srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, "api", "GET", "/v1/customers",
		"--api-key", "sk_test_x", "--base-url", srv.URL); err != nil {
		t.Fatal(err)
	}

	all := rec.all()
	if len(all) != 2 {
		t.Fatalf("requests = %d, want 2", len(all))
	}
	if all[0].idem == "" {
		t.Fatal("`billkit api POST` went out with no Idempotency-Key")
	}
	if all[1].idem != "" {
		t.Fatalf("a GET should not carry an Idempotency-Key, got %q", all[1].idem)
	}
}

// TestLoginMakesTheNewProfileDefault is the P0-3 regression, and it is the
// exact chain the auditor reproduced: log in live, log in test, then watch
// which key the next money command actually uses.
func TestLoginMakesTheNewProfileDefault(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	srv := tlsStub(t, rec.handler())

	if _, err := runCLI(t, "login", "--api-key", "sk_live_AAAA1111", "--base-url", srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, "login", "--api-key", "sk_test_BBBB2222", "--base-url", srv.URL); err != nil {
		t.Fatal(err)
	}

	// No --api-key and no --profile: exactly what a user types next.
	if _, err := runCLI(t, "refunds", "create", "--payment", "pay_123", "--amount", "500"); err != nil {
		t.Fatal(err)
	}

	mutations := rec.mutations()
	if len(mutations) != 1 {
		t.Fatalf("mutating requests = %d, want 1", len(mutations))
	}
	if got := mutations[0].auth; got != "Bearer sk_test_BBBB2222" {
		t.Fatalf("refund went out as %q, want the test key the CLI just said it logged into — "+
			"that is real money spent moments after the CLI reported test mode", got)
	}
}

// TestConfigUseSwitchesTheDefaultProfile covers the way out that used to
// require `logout` or hand-editing config.json.
func TestConfigUseSwitchesTheDefaultProfile(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	srv := tlsStub(t, rec.handler())

	if _, err := runCLI(t, "login", "--api-key", "sk_live_AAAA1111", "--base-url", srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, "login", "--api-key", "sk_test_BBBB2222", "--base-url", srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, "config", "use", "live"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, "refunds", "create", "--payment", "pay_123", "--amount", "500", "--yes"); err != nil {
		t.Fatal(err)
	}
	mutations := rec.mutations()
	if len(mutations) != 1 || mutations[0].auth != "Bearer sk_live_AAAA1111" {
		t.Fatalf("after `config use live` the refund went out as %+v", mutations)
	}

	if _, err := runCLI(t, "config", "use", "nope"); err == nil {
		t.Fatal("expected an error for an unknown profile")
	}
}

// TestLiveMoneyCommandRefusesWithoutConfirmation pins that live money
// movement is a deliberate act, and that a non-terminal stdin fails fast
// with a message naming the flag instead of blocking on a prompt.
func TestLiveMoneyCommandRefusesWithoutConfirmation(t *testing.T) {
	rec := &recorder{}
	srv := tlsStub(t, rec.handler())

	stderr, err := runCLI(t,
		"refunds", "create", "--payment", "pay_123", "--amount", "500",
		"--api-key", "sk_live_AAAA1111", "--base-url", srv.URL)
	if err == nil {
		t.Fatal("a live refund ran with no confirmation and no --yes")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("the error must name the flag that authorises this, got %v", err)
	}
	if len(rec.mutations()) != 0 {
		t.Fatalf("the refund was sent anyway: %+v", rec.mutations())
	}
	if !strings.Contains(stderr, "LIVE MODE") {
		t.Fatalf("live mode must announce itself before acting; stderr = %q", stderr)
	}

	// With --yes the same command goes through.
	if _, err := runCLI(t,
		"refunds", "create", "--payment", "pay_123", "--amount", "500", "--yes",
		"--api-key", "sk_live_AAAA1111", "--base-url", srv.URL); err != nil {
		t.Fatal(err)
	}
	if len(rec.mutations()) != 1 {
		t.Fatalf("mutating requests after --yes = %d, want 1", len(rec.mutations()))
	}
}

// TestTestModeMoneyCommandNeedsNoConfirmation: test mode is meant to be cheap
// to run, so it must not grow a prompt.
func TestTestModeMoneyCommandNeedsNoConfirmation(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	stderr, err := runCLI(t,
		"refunds", "create", "--payment", "pay_123", "--amount", "500",
		"--api-key", "sk_test_x", "--base-url", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.mutations()) != 1 {
		t.Fatalf("mutating requests = %d, want 1", len(rec.mutations()))
	}
	if !strings.Contains(stderr, "test mode") {
		t.Fatalf("the resolved mode must be stated in both modes; stderr = %q", stderr)
	}
}

// TestLiveKeyOverPlainHTTPIsRefused: a live key is scoped to a host, so it
// must never travel over a channel that can be read or redirected. The
// request must not leave the process.
func TestLiveKeyOverPlainHTTPIsRefused(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	_, err := runCLI(t,
		"refunds", "create", "--payment", "pay_123", "--amount", "500", "--yes",
		"--api-key", "sk_live_AAAA1111", "--base-url", srv.URL)
	if err == nil {
		t.Fatal("a live key was sent to a plain-HTTP host")
	}
	if !strings.Contains(err.Error(), "plain HTTP") {
		t.Fatalf("error = %v, want a refusal naming the cleartext transport", err)
	}
	if len(rec.all()) != 0 {
		t.Fatalf("the key reached the wire anyway: %+v", rec.all())
	}
}

// TestTestKeyOverPlainHTTPRemoteIsRefused: loopback is the only legitimate
// cleartext case, so a test key aimed at a remote http host is refused too.
func TestTestKeyOverPlainHTTPRemoteIsRefused(t *testing.T) {
	_, err := runCLI(t, "refunds", "list",
		"--api-key", "sk_test_x", "--base-url", "http://attacker.example")
	if err == nil {
		t.Fatal("expected a refusal for a remote plain-HTTP host")
	}
	if !strings.Contains(err.Error(), "plain HTTP") {
		t.Fatalf("error = %v", err)
	}
}

// TestNonDefaultHostIsAnnounced: a repointed CLI should say so rather than
// quietly carrying the key somewhere the user did not expect.
func TestNonDefaultHostIsAnnounced(t *testing.T) {
	var notices bytes.Buffer
	hostOnce = sync.Once{}
	prev := stderrOut
	stderrOut = &notices
	t.Cleanup(func() {
		stderrOut = prev
		hostOnce = sync.Once{}
	})

	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	if _, err := runCLI(t, "refunds", "list", "--api-key", "sk_test_x", "--base-url", srv.URL); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(notices.String(), srv.URL) {
		t.Fatalf("a non-default API host must be announced; stderr = %q", notices.String())
	}
}

func TestDescribeRefund(t *testing.T) {
	partial := describeRefund(map[string]any{"payment_id": "pay_1", "amount_cents": int64(500)})
	if partial != "refund 500 cents of pay_1" {
		t.Errorf("describeRefund(partial) = %q", partial)
	}
	full := describeRefund(map[string]any{"subscription_id": "sub_9"})
	if full != "refund the full remaining balance of sub_9" {
		t.Errorf("describeRefund(full) = %q", full)
	}
}

func TestRetryCommandCarriesTheKey(t *testing.T) {
	got := retryCommand("cli_abc")
	if !strings.Contains(got, "--idempotency-key cli_abc") {
		t.Fatalf("retryCommand = %q, want it to carry the key", got)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain":       "plain",
		"has space":   "'has space'",
		"it's":        `'it'\''s'`,
		"":            "''",
		"a;rm -rf /":  "'a;rm -rf /'",
		"--flag=safe": "--flag=safe",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
