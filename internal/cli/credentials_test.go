package cli

import (
	"bytes"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runCLIWithStdin is runCLI with something on stdin, for the login paths that
// read a key rather than taking one from a flag.
func runCLIWithStdin(t *testing.T, in io.Reader, args ...string) (string, error) {
	t.Helper()
	root := rootCmd()
	var errBuf bytes.Buffer
	root.SetOut(io.Discard)
	root.SetErr(&errBuf)
	root.SetIn(in)
	root.SetArgs(args)
	err := root.Execute()
	return errBuf.String(), err
}

// writeProfile plants a config file for the run, so the stored-profile tier
// has something in it to be beaten.
func writeProfile(t *testing.T, apiKey, baseURL string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BILLKIT_CONFIG_HOME", dir)
	body := `{"default_profile":"test","profiles":{"test":{"api_key":"` + apiKey + `","base_url":"` + baseURL + `"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestBillkitAPIKeyEnvIsHonoured is the P1-9 regression. Before it, --api-key
// was the only non-interactive way to pass a credential, so every CI job and
// every one-liner had to put a live secret into argv, where `ps auxww` and a
// world-readable /proc/<pid>/cmdline expose it for the life of the call and
// the shell appends it to ~/.zsh_history forever. The environment variable was
// read nowhere in the module, and this ran with "no BillKit profile
// configured".
func TestBillkitAPIKeyEnvIsHonoured(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	t.Setenv(envAPIKey, "sk_test_FROMENV")
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	if _, err := runCLI(t, "api", "GET", "/v1/customers", "--base-url", srv.URL); err != nil {
		t.Fatal(err)
	}
	all := rec.all()
	if len(all) != 1 {
		t.Fatalf("requests = %d, want 1", len(all))
	}
	if all[0].auth != "Bearer sk_test_FROMENV" {
		t.Fatalf("Authorization = %q, want the key from $%s", all[0].auth, envAPIKey)
	}
}

// TestEnvCredentialsAreTrimmed covers the shape a key actually arrives in:
// `export BILLKIT_API_KEY=$(cat /run/secrets/key)` or a mounted secret file
// leaves a trailing newline, and a newline inside an Authorization header is a
// protocol error rather than a 401 anyone can read.
func TestEnvCredentialsAreTrimmed(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	t.Setenv(envAPIKey, "  sk_test_PADDED\n")
	t.Setenv(envBaseURL, srv.URL+"\n")

	if _, err := runCLI(t, "api", "GET", "/v1/customers"); err != nil {
		t.Fatal(err)
	}
	all := rec.all()
	if len(all) != 1 || all[0].auth != "Bearer sk_test_PADDED" {
		t.Fatalf("Authorization = %+v, want the trimmed key", all)
	}
}

// TestAPIKeyFlagBeatsTheEnvironment pins the precedence. A flag is what the
// user typed on this invocation; the variable may have been exported by a
// profile script an hour ago, and it must not be able to beat a deliberate
// override.
func TestAPIKeyFlagBeatsTheEnvironment(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	t.Setenv(envAPIKey, "sk_test_FROMENV")
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	if _, err := runCLI(t, "api", "GET", "/v1/customers",
		"--api-key", "sk_test_FROMFLAG", "--base-url", srv.URL); err != nil {
		t.Fatal(err)
	}
	all := rec.all()
	if len(all) != 1 || all[0].auth != "Bearer sk_test_FROMFLAG" {
		t.Fatalf("Authorization = %+v, want the flag's key", all)
	}
}

// TestEnvKeyBeatsTheStoredProfile is the other half of the precedence: a
// variable scoped to this process tree says "run as this identity" without
// writing a key to the runner's disk, so it has to win over whatever an
// earlier `billkit login` left behind.
func TestEnvKeyBeatsTheStoredProfile(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	writeProfile(t, "sk_test_STORED", srv.URL)
	t.Setenv(envAPIKey, "sk_test_FROMENV")

	if _, err := runCLI(t, "api", "GET", "/v1/customers", "--base-url", srv.URL); err != nil {
		t.Fatal(err)
	}
	all := rec.all()
	if len(all) != 1 || all[0].auth != "Bearer sk_test_FROMENV" {
		t.Fatalf("Authorization = %+v, want the environment's key", all)
	}
}

// TestBaseURLFlagBeatsEnvBeatsProfile walks all three tiers of the host
// selector in one go.
func TestBaseURLFlagBeatsEnvBeatsProfile(t *testing.T) {
	stored := &recorder{}
	storedSrv := httptest.NewServer(stored.handler())
	defer storedSrv.Close()
	fromEnv := &recorder{}
	envSrv := httptest.NewServer(fromEnv.handler())
	defer envSrv.Close()
	fromFlag := &recorder{}
	flagSrv := httptest.NewServer(fromFlag.handler())
	defer flagSrv.Close()

	writeProfile(t, "sk_test_STORED", storedSrv.URL)

	// Profile only.
	if _, err := runCLI(t, "api", "GET", "/v1/customers"); err != nil {
		t.Fatal(err)
	}
	// Environment beats the profile.
	t.Setenv(envBaseURL, envSrv.URL)
	if _, err := runCLI(t, "api", "GET", "/v1/customers"); err != nil {
		t.Fatal(err)
	}
	// Flag beats the environment.
	if _, err := runCLI(t, "api", "GET", "/v1/customers", "--base-url", flagSrv.URL); err != nil {
		t.Fatal(err)
	}

	if got := len(stored.all()); got != 1 {
		t.Errorf("stored-profile host got %d requests, want 1", got)
	}
	if got := len(fromEnv.all()); got != 1 {
		t.Errorf("$%s host got %d requests, want 1", envBaseURL, got)
	}
	if got := len(fromFlag.all()); got != 1 {
		t.Errorf("--base-url host got %d requests, want 1", got)
	}
}

// TestBillkitProfileEnvSelectsTheProfile lets an environment pick which stored
// identity a run uses, the same way --profile does.
func TestBillkitProfileEnvSelectsTheProfile(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("BILLKIT_CONFIG_HOME", dir)
	body := `{"default_profile":"test","profiles":{` +
		`"test":{"api_key":"sk_test_DEFAULT","base_url":"` + srv.URL + `"},` +
		`"other":{"api_key":"sk_test_OTHER","base_url":"` + srv.URL + `"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv(envProfile, "other")
	if _, err := runCLI(t, "api", "GET", "/v1/customers"); err != nil {
		t.Fatal(err)
	}
	// And the flag still wins over it.
	if _, err := runCLI(t, "api", "GET", "/v1/customers", "--profile", "test"); err != nil {
		t.Fatal(err)
	}

	all := rec.all()
	if len(all) != 2 {
		t.Fatalf("requests = %d, want 2", len(all))
	}
	if all[0].auth != "Bearer sk_test_OTHER" {
		t.Errorf("$%s was ignored: Authorization = %q", envProfile, all[0].auth)
	}
	if all[1].auth != "Bearer sk_test_DEFAULT" {
		t.Errorf("--profile must beat $%s: Authorization = %q", envProfile, all[1].auth)
	}
}

// TestEnvLiveKeyOverPlainHTTPIsRefused is the rule that makes the new tier
// safe to add: a key from the environment is not trusted one step further than
// a key from a flag. Both go through config.CheckTransport before anything is
// dialled, so a live key aimed at a cleartext host is refused either way, and
// the refusal never repeats the key back.
func TestEnvLiveKeyOverPlainHTTPIsRefused(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	const liveKey = "sk_live_ENVSECRET"
	t.Setenv(envAPIKey, liveKey)

	for _, target := range []string{srv.URL, "http://billkit.internal:8000"} {
		stderr, err := runCLI(t, "api", "GET", "/v1/customers", "--base-url", target)
		if err == nil {
			t.Fatalf("a live key from $%s was accepted over %s", envAPIKey, target)
		}
		if !strings.Contains(err.Error(), "live API key") {
			t.Errorf("error should say why it refused, got %v", err)
		}
		if strings.Contains(err.Error(), liveKey) || strings.Contains(stderr, liveKey) {
			t.Error("the refusal echoed the secret key back")
		}
	}
	if got := len(rec.all()); got != 0 {
		t.Fatalf("the CLI dialled the cleartext host %d time(s) before refusing", got)
	}
}

// TestNoCredentialsErrorNamesEveryWayIn keeps the new tier discoverable. The
// config package can only name `billkit login`, because stored profiles are
// all it knows about; a script has no one to answer that prompt and needs to
// be told about the variable instead.
func TestNoCredentialsErrorNamesEveryWayIn(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())

	_, err := runCLI(t, "api", "GET", "/v1/customers")
	if err == nil {
		t.Fatal("expected an error with no credentials configured")
	}
	for _, want := range []string{"billkit login", envAPIKey, "--api-key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q, got %v", want, err)
		}
	}
}

// TestLoginReadsAPipedKey covers the case the echo fix must not break:
// `cat key.txt | billkit login` and a CI heredoc are ordinary, and stdin there
// is a pipe, which term.ReadPassword cannot read at all.
func TestLoginReadsAPipedKey(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	srv := tlsStub(t, rec.handler())

	for name, piped := range map[string]string{
		"with a trailing newline":    "sk_test_PIPED\n",
		"without a trailing newline": "sk_test_PIPED",
		"with surrounding blanks":    "  sk_test_PIPED  \n",
	} {
		t.Run(name, func(t *testing.T) {
			rec.reset()
			if _, err := runCLIWithStdin(t, strings.NewReader(piped), "login", "--base-url", srv.URL); err != nil {
				t.Fatal(err)
			}
			all := rec.all()
			if len(all) != 1 || all[0].auth != "Bearer sk_test_PIPED" {
				t.Fatalf("login validated %+v, want the piped key", all)
			}
		})
	}
}

// TestLoginUsesTheEnvironmentKeyWithoutPrompting is the scripted login: the
// key never touches argv and no prompt blocks a job nobody is watching. The
// stdin here would fail the read, which is the point -- it must not be read.
func TestLoginUsesTheEnvironmentKeyWithoutPrompting(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	t.Setenv(envAPIKey, "sk_test_FROMENV")
	rec := &recorder{}
	srv := tlsStub(t, rec.handler())

	stderr, err := runCLIWithStdin(t, strings.NewReader(""), "login", "--base-url", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	all := rec.all()
	if len(all) != 1 || all[0].auth != "Bearer sk_test_FROMENV" {
		t.Fatalf("login validated %+v, want the key from $%s", all, envAPIKey)
	}
	if !strings.Contains(stderr, envAPIKey) {
		t.Errorf("login should say which source it used, stderr = %q", stderr)
	}
	if strings.Contains(stderr, "sk_test_FROMENV") {
		t.Error("login echoed the key it was given")
	}
}

// TestLoginPromptDoesNotEchoOnATerminal is the P1-10 regression. The prompt
// used to read the key with bufio.NewReader(os.Stdin).ReadString('\n') and
// never touched the terminal's echo flag, so a pasted sk_live_ key was printed
// straight back onto the screen and into the scrollback, into tmux and
// `script` logs, and into any recorded demo -- which is exactly the setting a
// getting-started CLI is used in.
//
// The terminal branch is proved by what it does NOT do: the bytes sitting on
// the file descriptor are left unread, because the echo-suppressed read is the
// one that ran.
func TestLoginPromptDoesNotEchoOnATerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	const typed = "sk_live_TYPEDATTHEPROMPT\n"
	if _, err := w.WriteString(typed); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Pretend this file descriptor is a terminal, and answer the
	// echo-suppressed read the way a real one would.
	called := 0
	restore := stubTerminal(t, func(fd uintptr) bool { return fd == r.Fd() },
		func(uintptr) ([]byte, error) { called++; return []byte("sk_test_NOTECHOED"), nil })
	defer restore()

	var prompt bytes.Buffer
	got, err := readSecretLine(r, &prompt, "Enter your key: ")
	if err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Fatalf("the echo-suppressed read ran %d times, want 1; a terminal key is being echoed", called)
	}
	if got != "sk_test_NOTECHOED" {
		t.Fatalf("read %q, want the value from the echo-suppressed read", got)
	}

	// Nothing consumed the descriptor, so the echoing path never ran.
	left, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != typed {
		t.Fatalf("the buffered (echoing) read consumed the terminal: %q left of %q", left, typed)
	}

	// The Enter that ended the line was swallowed, so the prompt has to close
	// its own line or the next thing printed runs onto it.
	if !strings.HasSuffix(prompt.String(), "\n") {
		t.Errorf("no newline printed after the hidden read: %q", prompt.String())
	}
	if strings.Contains(prompt.String(), "sk_") {
		t.Errorf("the prompt writer saw a key: %q", prompt.String())
	}
}

// TestReadSecretLineUsesTheBufferedPathOffATerminal exercises the real
// golang.org/x/term check rather than a stub: a pipe is not a terminal, so the
// plain read has to take over. That is what keeps `echo $KEY | billkit login`
// working.
func TestReadSecretLineUsesTheBufferedPathOffATerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if _, err := w.WriteString("sk_test_PIPED\n"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := readSecretLine(r, io.Discard, "Enter your key: ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "sk_test_PIPED" {
		t.Fatalf("read %q from a pipe, want the piped key", got)
	}
}

// stubTerminal swaps the two terminal seams for the duration of a test.
func stubTerminal(t *testing.T, tty func(uintptr) bool, read func(uintptr) ([]byte, error)) func() {
	t.Helper()
	prevTTY, prevRead := isTerminal, readPassword
	isTerminal, readPassword = tty, read
	return func() { isTerminal, readPassword = prevTTY, prevRead }
}
