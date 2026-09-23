package cli

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// stubServer is a plain-HTTP stub for the read commands, which need no TLS:
// a bk_test_ key over loopback http is the one cleartext case the transport
// rules allow.
func stubServer(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestEventsListSendsItsCursor. `events list` had no way to page: the API
// takes `starting_after` on every list route, `refunds list` already exposed
// it, and without it the only way past the first page of the event log was
// `billkit api`.
func TestEventsListSendsItsCursor(t *testing.T) {
	var query url.Values
	srv := stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	})

	if _, err := runCLI(t, "events", "list",
		"--type", "customer.created", "--limit", "5", "--starting-after", "evt_7",
		"--api-key", "bk_test_x", "--base-url", srv); err != nil {
		t.Fatal(err)
	}
	for param, want := range map[string]string{
		"type":           "customer.created",
		"limit":          "5",
		"starting_after": "evt_7",
	} {
		if got := query.Get(param); got != want {
			t.Errorf("%s = %q, want %q", param, got, want)
		}
	}
}

// TestEventsListRejectsAnUnknownType. The API's `type` filter matches
// exactly and never errors on a type that does not exist, so a typo returned
// an empty page — which reads as "nothing happened" rather than "no such
// event type". `listen --events` has refused this locally for a while; this
// is the same validator, with the same wording, on the other command that
// takes an event type.
func TestEventsListRejectsAnUnknownType(t *testing.T) {
	var hits int
	srv := stubServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"data":[]}`))
	})

	_, err := runCLI(t, "events", "list", "--type", "customer.crated",
		"--api-key", "bk_test_x", "--base-url", srv)
	if err == nil {
		t.Fatal("a typo'd event type was accepted")
	}
	if !strings.Contains(err.Error(), "customer.crated") {
		t.Errorf("the error must name the offender, got %v", err)
	}
	if hits != 0 {
		t.Error("a locally refused filter must not reach the wire")
	}
}
