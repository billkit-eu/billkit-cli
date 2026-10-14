package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runCLIOutErr drives the real command tree and returns stdout and stderr
// separately, with Main's error printing applied, as a script would see them.
func runCLIOutErr(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := rootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetIn(strings.NewReader(""))
	root.SetArgs(args)
	err = root.Execute()
	if err != nil {
		writeError(&errBuf, err, slicesContains(args, "--json"))
	}
	return out.String(), errBuf.String(), err
}

func slicesContains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// TestJSONModeKeepsNoticesOffStderr: under --json, stderr is the machine
// channel. "> Using API host …", the mode line and the idempotency-key line
// used to precede the envelope, so a script parsing stderr as JSON broke on
// the first notice.
func TestJSONModeKeepsNoticesOffStderr(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	t.Setenv("BILLKIT_API_KEY", "")
	t.Setenv("BILLKIT_BASE_URL", "")
	t.Setenv("BILLKIT_PROFILE", "")
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"re_1","object":"refund"}`))
	}))
	defer ok.Close()
	missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"resource_missing","message":"No such refund."}}`))
	}))
	defer missing.Close()
	key := func(base string) []string { return []string{"--api-key", "bk_test_QUIET", "--base-url", base} }

	// A mutating call on a non-default host: three notices without --json.
	args := append([]string{"refunds", "create", "--payment", "pay_1", "--amount", "500"}, key(ok.URL)...)
	_, loud, err := runCLIOutErr(t, args...)
	if err != nil || !strings.Contains(loud, "> Using API host") || !strings.Contains(loud, "Idempotency-Key") {
		t.Fatalf("setup: without --json the notices are expected (err %v): %q", err, loud)
	}
	_, quiet, err := runCLIOutErr(t, append([]string{"--json"}, args...)...)
	if err != nil || quiet != "" {
		t.Fatalf("under --json a successful call must leave stderr empty (err %v): %q", err, quiet)
	}

	// A failed call: exactly one line, and it is the envelope.
	_, stderr, err := runCLIOutErr(t, append([]string{"--json", "refunds", "retrieve", "re_404"}, key(missing.URL)...)...)
	if err == nil {
		t.Fatal("expected the 404 to fail the command")
	}
	lines := strings.Split(strings.TrimRight(stderr, "\n"), "\n")
	var env errorEnvelope
	if len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &env) != nil || env.Error.Code != "resource_missing" {
		t.Fatalf("under --json stderr must be the envelope alone, got %q", stderr)
	}
}

// TestCompletionNeverMovesTheConfig: `config use <Tab>` loads the profile
// names on every key press. It used to go through Load, so the first Tab
// after upgrading moved the pre-1.0 config and wrote live keys to the
// keychain, from inside a shell's completion hook.
func TestCompletionNeverMovesTheConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BILLKIT_CONFIG_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	legacy := filepath.Join(home, ".billkit", "config.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte(`{"profiles":{"live":{"api_key":"bk_live_TAB"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	kr := useFakeKeyring(t)

	stdout, _, err := runCLIOutErr(t, "__complete", "config", "use", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "live") {
		t.Fatalf("completion did not offer the profile: %q", stdout)
	}
	if kr.len() != 0 {
		t.Fatal("completion wrote a key into the keychain")
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("completion moved the pre-1.0 config: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "xdg", "billkit")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completion created the config directory (%v)", err)
	}
}
