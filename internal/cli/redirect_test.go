package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/billkit-eu/billkit-cli/internal/sign"
)

// sink is a plain-http server that records whether anything reached it, and
// with which Authorization header. It stands in for the place a redirect
// would have carried the key to.
type sink struct {
	srv  *httptest.Server
	hits atomic.Int32
	auth atomic.Value // string
}

func newSink(t *testing.T) *sink {
	t.Helper()
	s := &sink{}
	s.auth.Store("")
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		s.auth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"re_1","object":"refund"}`))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *sink) assertUntouched(t *testing.T) {
	t.Helper()
	if n := s.hits.Load(); n != 0 {
		t.Fatalf("the plain-http host received %d request(s), Authorization %q: "+
			"a redirect carried the key off the https host it was checked against", n, s.auth.Load())
	}
}

// redirectingTLSStub is an https API that answers every request with a 307 to
// the same path on the sink: same host (127.0.0.1), plain http. That is the
// shape of a framework's trailing-slash redirect behind a proxy that does not
// forward the scheme, and net/http re-sends Authorization on it, because the
// host has not changed.
func redirectingTLSStub(t *testing.T, to *sink) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := tlsStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, to.srv.URL+strings.TrimSuffix(r.URL.Path, "/"), http.StatusTemporaryRedirect)
	}))
	return srv, &hits
}

// TestLiveMoneyCallBehindARedirectNeverLeavesHTTPS is the exact audit
// scenario: a live key, no terminal, no --yes, and a money route spelled with
// a trailing slash against an https host that 307s to plain http.
//
// Two things used to go wrong at once. The trailing slash made isMoneyPath
// miss the route, so there was no banner and no --yes check; and the client
// followed the redirect, re-sending `Authorization: Bearer bk_live_…` in
// clear text to a URL CheckTransport never saw.
func TestLiveMoneyCallBehindARedirectNeverLeavesHTTPS(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	plain := newSink(t)
	srv, tlsHits := redirectingTLSStub(t, plain)
	t.Setenv("BILLKIT_API_KEY", "bk_live_SECRETSECRET")
	t.Setenv("BILLKIT_BASE_URL", srv.URL)

	stderr, err := runCLI(t, "api", "POST", "/v1/subscriptions/sub_1/cancel/")
	if err == nil {
		t.Fatal("a live cancel with no --yes and no terminal must be refused")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("the refusal should name --yes, got %q", err)
	}
	if !strings.Contains(stderr, "LIVE MODE") {
		t.Errorf("a live cancel must show the money banner; stderr = %q", stderr)
	}
	if n := tlsHits.Load(); n != 0 {
		t.Errorf("an unconfirmed live money call reached the API %d time(s)", n)
	}
	plain.assertUntouched(t)
}

// TestRedirectIsRefusedNotFollowed covers everything the confirmation gate
// does not: a confirmed money call, a write that is not money, and a read.
// Each reaches the https host once, gets its 307, and stops there with an
// error that says why, rather than retrying it or following it.
func TestRedirectIsRefusedNotFollowed(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"confirmed live money call", []string{"--yes", "api", "POST", "/v1/refunds/", "--data", "payment_id=pay_1"}},
		// A full refund: a partial one first reads the payment for its
		// currency, which is a second, separate request to the same host.
		{"refunds create", []string{"--yes", "refunds", "create", "--payment", "pay_1"}},
		{"plain write", []string{"api", "POST", "/v1/customers", "--data", "email=a@b.co"}},
		{"read", []string{"events", "list"}},
		{"raw read", []string{"api", "GET", "/v1/customers"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
			plain := newSink(t)
			srv, tlsHits := redirectingTLSStub(t, plain)
			t.Setenv("BILLKIT_API_KEY", "bk_live_SECRETSECRET")
			t.Setenv("BILLKIT_BASE_URL", srv.URL)

			stderr, err := runCLI(t, tc.args...)
			if err == nil {
				t.Fatal("a redirected call must fail")
			}
			plain.assertUntouched(t)
			if n := tlsHits.Load(); n != 1 {
				t.Errorf("the https host saw %d request(s), want exactly 1: a refused redirect is a definite answer and is not retried", n)
			}
			if !strings.Contains(err.Error(), "refused to follow it") {
				t.Errorf("the error should say the redirect was refused, got %q", err)
			}
			if ExitCode(err) != ExitFailure {
				t.Errorf("ExitCode = %d, want %d", ExitCode(err), ExitFailure)
			}
			if strings.Contains(stderr, "outcome is unknown") {
				t.Errorf("a redirect is not an ambiguous outcome; stderr = %q", stderr)
			}
		})
	}
}

// TestLoginDoesNotFollowARedirect. Login sends the key it is about to store,
// to a base URL nothing has validated against yet.
func TestLoginDoesNotFollowARedirect(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	t.Setenv("BILLKIT_API_KEY", "")
	plain := newSink(t)
	srv, _ := redirectingTLSStub(t, plain)

	if _, err := runCLI(t, "login", "--api-key", "bk_live_SECRETSECRET", "--base-url", srv.URL); err == nil {
		t.Fatal("login against a redirecting host must fail")
	}
	plain.assertUntouched(t)
}

// TestListenTreatsARedirectAsFatal. The stream client carries the key too,
// and it reconnects forever on anything it thinks is transient, so a refused
// redirect has to end the command rather than be redialled.
func TestListenTreatsARedirectAsFatal(t *testing.T) {
	plain := newSink(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, plain.srv.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	errOut := &syncBuf{}
	l := testListener(srv.URL, newTarget(t).forwarder(), errOut)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := l.run(ctx)
	if err == nil || ctx.Err() != nil {
		t.Fatalf("a redirected stream must end the command, got err=%v ctxErr=%v", err, ctx.Err())
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("a redirected stream was dialled %d times, want 1", n)
	}
	plain.assertUntouched(t)
	if !strings.Contains(errOut.String(), "does not follow redirects") {
		t.Errorf("stderr should explain the refusal, got %q", errOut.String())
	}
}

// TestForwarderFollowsASameOriginTrailingSlashRedirect is the regression for
// a local app that answers POST /webhooks with 307 to /webhooks/, which is
// what FastAPI, Rails and Laravel do. The event has to arrive at the
// redirected path with the same body and every webhook header, the signature
// still verifying, and the log has to report the final answer.
func TestForwarderFollowsASameOriginTrailingSlashRedirect(t *testing.T) {
	const secret = "bkwhsec_test"
	raw := []byte(`{"id":"evt_1","type":"customer.created"}`)
	var got atomic.Value // http.Header
	var gotBody atomic.Value
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/webhooks" {
			http.Redirect(w, r, "/webhooks/", http.StatusTemporaryRedirect)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/webhooks/" {
			t.Errorf("redirected delivery arrived as %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		got.Store(r.Header.Clone())
		gotBody.Store(string(body))
		w.WriteHeader(http.StatusOK)
	}))
	defer local.Close()

	logOut := &syncBuf{}
	fwd := &forwarder{url: local.URL + "/webhooks", secret: secret, out: &syncBuf{}, logOut: logOut}
	fwd.client = newForwarderHTTPClient()
	fwd.handle(context.Background(), raw)

	h, _ := got.Load().(http.Header)
	if h == nil {
		t.Fatalf("the event never reached /webhooks/; log = %q", logOut.String())
	}
	if b, _ := gotBody.Load().(string); b != string(raw) {
		t.Errorf("redirected body = %q, want %q", b, raw)
	}
	sig := h.Get("BillKit-Signature")
	ts, ok := strings.CutPrefix(strings.Split(sig, ",")[0], "t=")
	tsN, perr := strconv.ParseInt(ts, 10, 64)
	if !ok || perr != nil || sign.Header(secret, tsN, raw) != sig {
		t.Errorf("BillKit-Signature %q does not verify over the redirected body", sig)
	}
	for _, name := range []string{"BillKit-Event-Id", "BillKit-Event-Type", "BillKit-Delivery-Attempt", "Content-Type"} {
		if h.Get(name) == "" {
			t.Errorf("%s was not re-sent on the redirected delivery", name)
		}
	}
	if !strings.Contains(logOut.String(), "-> 200") {
		t.Errorf("log should report the final 200, got %q", logOut.String())
	}
}

// TestForwarderRefusesRedirectsItCannotFollowFaithfully covers the two shapes
// that are refused: another origin, where the signed body would go somewhere
// the user did not name, and a 302 on the same origin, which net/http would
// replay as a bodiless GET. Both must say so, and neither may read like a
// delivery.
func TestForwarderRefusesRedirectsItCannotFollowFaithfully(t *testing.T) {
	plain := newSink(t)
	cases := []struct {
		name   string
		status int
		target func(local string) string
	}{
		{"other origin 307", http.StatusTemporaryRedirect, func(string) string { return plain.srv.URL + "/elsewhere" }},
		{"same origin 302", http.StatusFound, func(local string) string { return local + "/webhooks/" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			var local *httptest.Server
			local = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if r.URL.Path == "/webhooks" {
					http.Redirect(w, r, tc.target(local.URL), tc.status)
					return
				}
				t.Errorf("the redirect was followed to %s %s", r.Method, r.URL.Path)
			}))
			defer local.Close()

			logOut := &syncBuf{}
			fwd := &forwarder{url: local.URL + "/webhooks", secret: "bkwhsec_test", out: &syncBuf{}, logOut: logOut}
			fwd.client = newForwarderHTTPClient()
			fwd.handle(context.Background(), []byte(`{"id":"evt_1","type":"customer.created"}`))

			plain.assertUntouched(t)
			if n := hits.Load(); n != 1 {
				t.Errorf("the local app saw %d request(s), want 1: a refused redirect is not retried", n)
			}
			want := fmt.Sprintf("-> %d redirect to %s not followed; point --forward-to at it", tc.status, tc.target(local.URL))
			if !strings.Contains(logOut.String(), want) {
				t.Errorf("log should contain %q, got %q", want, logOut.String())
			}
			if strings.Contains(logOut.String(), fmt.Sprintf("-> %d\n", tc.status)) {
				t.Errorf("a refused redirect was logged like a delivery: %q", logOut.String())
			}
		})
	}
}
