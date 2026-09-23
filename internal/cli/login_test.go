package cli

import (
	"net/http"
	"strings"
	"testing"

	"github.com/billkit-eu/billkit-cli/internal/config"
)

// TestLoginValidatesAgainstAScopeExemptRoute is the regression for a login
// that refused perfectly good credentials.
//
// It used to validate with GET /v1/tenant/capabilities, which needs
// `tenant:read`. A key minted for refunds, or for checkout, authenticated
// fine and then got a 403 from that one route -- so `billkit login` reported
// a validation failure and never wrote the profile. The key was valid for
// every command the person actually intended to run.
//
// /v1/ping is the fix: it is the one route on the scoped /v1 router that is
// exempt from the scope check while still requiring a principal, so no scope
// set can turn login into a failure.
func TestLoginValidatesAgainstAScopeExemptRoute(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	srv := tlsStub(t, rec.handler())

	if _, err := runCLI(t, "login", "--api-key", "bk_test_SCOPED", "--base-url", srv.URL); err != nil {
		t.Fatal(err)
	}

	all := rec.all()
	if len(all) != 1 {
		t.Fatalf("login made %d requests, want exactly one", len(all))
	}
	if all[0].path != "/v1/ping" {
		t.Errorf("login validated against %q, want /v1/ping -- a scoped route "+
			"makes login fail for keys that are entirely valid", all[0].path)
	}
}

// TestLoginAcceptsAKeyRefusedForScope is the belt to /v1/ping's braces.
//
// A 403 proves the credential authenticated: the server identifies the key
// before it authorizes the call, so being refused for scope is not evidence
// that the key is bad. Login must not treat it as such.
func TestLoginAcceptsAKeyRefusedForScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BILLKIT_CONFIG_HOME", home)
	srv := tlsStub(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error",` +
			`"code":"insufficient_scope","message":"Key lacks tenant:read."}}`))
	}))

	if _, err := runCLI(t, "login", "--api-key", "bk_live_SCOPED", "--base-url", srv.URL); err != nil {
		t.Fatalf("login refused a key that authenticated: %v", err)
	}

	// Saved, not merely tolerated: the bug was that the profile never landed.
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, ok := cfg.Profiles["live"]
	if !ok || p.APIKey != "bk_live_SCOPED" {
		t.Fatalf("login did not store the profile: %+v", cfg.Profiles)
	}
}

// TestLoginStillRefusesABadKey is the other half. Tolerating a 403 must not
// widen into tolerating a 401 -- an unauthenticated key is exactly what this
// validation exists to catch, and storing one would leave every later command
// failing with no clue where the bad credential came from.
func TestLoginStillRefusesABadKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BILLKIT_CONFIG_HOME", home)
	srv := tlsStub(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"authentication_error",` +
			`"code":"invalid_api_key","message":"No such API key."}}`))
	}))

	_, err := runCLI(t, "login", "--api-key", "bk_test_BOGUS", "--base-url", srv.URL)
	if err == nil {
		t.Fatal("login accepted a key the server did not recognise")
	}
	if !strings.Contains(err.Error(), "could not validate") {
		t.Errorf("error should say validation failed, got %q", err)
	}

	cfg, cfgErr := config.Load()
	if cfgErr != nil {
		t.Fatal(cfgErr)
	}
	if _, ok := cfg.Profiles["test"]; ok {
		t.Error("login stored a profile for a key the server rejected")
	}
}

// TestLoginHonoursTheProfileFlag: --profile was accepted on the command line
// and silently ignored, so `billkit login --profile staging` stored the key
// under "test" and the next `--profile staging` could not find it.
func TestLoginHonoursTheProfileFlag(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	srv := tlsStub(t, rec.handler())

	if _, err := runCLI(t, "login", "--profile", "staging",
		"--api-key", "bk_test_STAGING1", "--base-url", srv.URL); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Profiles["staging"]; !ok {
		t.Fatalf("the key was not stored under the name that was asked for: %v", cfg.Profiles)
	}
	if _, ok := cfg.Profiles["test"]; ok {
		t.Fatalf("the key must not also land under the mode name: %v", cfg.Profiles)
	}
	if cfg.DefaultProfile != "staging" {
		t.Fatalf("DefaultProfile = %q, want staging", cfg.DefaultProfile)
	}

	// And the whole point: later commands can select it back.
	if _, err := runCLI(t, "--profile", "staging", "refunds", "create",
		"--payment", "pay_1", "--amount", "5"); err != nil {
		t.Fatal(err)
	}
	m := rec.mutations()
	if len(m) != 1 || m[0].auth != "Bearer bk_test_STAGING1" {
		t.Fatalf("the named profile did not drive the call: %+v", m)
	}
}

// BILLKIT_PROFILE is the same tier, so it has to reach the same place.
func TestLoginHonoursTheProfileEnvironmentVariable(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	t.Setenv(envProfile, "ci")
	srv := tlsStub(t, (&recorder{}).handler())

	if _, err := runCLI(t, "login", "--api-key", "bk_test_CI000001", "--base-url", srv.URL); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Profiles["ci"]; !ok {
		t.Fatalf("BILLKIT_PROFILE did not name the stored profile: %v", cfg.Profiles)
	}
}

// The one name login refuses: a live key under the name the CLI reserves for
// test keys would make `config list`, `logout` and the live-mode banner all
// report a reassurance that is false.
func TestLoginRefusesToMislabelTheMode(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	srv := tlsStub(t, (&recorder{}).handler())

	_, err := runCLI(t, "login", "--profile", "test",
		"--api-key", "bk_live_DANGER01", "--base-url", srv.URL)
	if err == nil {
		t.Fatal("storing a live key under the name \"test\" must be refused")
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("the error must say why: %v", err)
	}
	cfg, _ := config.Load()
	if len(cfg.Profiles) != 0 {
		t.Fatalf("nothing should have been written: %v", cfg.Profiles)
	}

	// The matching name is still fine, and so is any name of the user's own.
	if _, err := runCLI(t, "login", "--profile", "live",
		"--api-key", "bk_live_DANGER01", "--base-url", srv.URL); err != nil {
		t.Fatal(err)
	}
}
