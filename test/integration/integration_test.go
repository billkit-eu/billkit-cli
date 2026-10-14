//go:build integration

// Package integration drives the built `billkit` binary against a real
// BillKit API: the same process a user runs, the same wire, no stubs.
//
// It is gated like the SDK integration suites (sdk/integration/SCENARIOS.md):
// it needs the build tag AND BILLKIT_INTEGRATION_BASE_URL, and without the
// variable every test skips, so `go test -tags integration ./...` on a laptop
// with no stack stays green. The API must be booted with
// BILLKIT_E2E_TEST_LOGIN=1 (sdk/integration/boot-api.sh does), because each
// run provisions a fresh tenant through POST /v1/console/auth/_test/login,
// exactly as the SDK suites do, so no two runs share an event log. Set
// BILLKIT_INTEGRATION_API_KEY to use an existing test-mode key instead.
//
//	BILLKIT_INTEGRATION_BASE_URL=http://127.0.0.1:8025 \
//	  go test -tags integration -count=1 ./test/integration/...
//
// or `mise run //cli:integration`, which boots the stack itself.
package integration

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	baseURL = strings.TrimRight(os.Getenv("BILLKIT_INTEGRATION_BASE_URL"), "/")
	binPath string
)

func TestMain(m *testing.M) {
	if baseURL == "" {
		fmt.Println("skipping: set BILLKIT_INTEGRATION_BASE_URL to run the CLI integration suite")
		os.Exit(m.Run()) // every test skips on its own
	}
	dir, err := os.MkdirTemp("", "billkit-cli-it-")
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	name := "billkit"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binPath = filepath.Join(dir, name)
	build := exec.Command("go", "build", "-o", binPath, "./cmd/billkit")
	build.Dir = filepath.Join("..", "..")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Println("go build failed:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// suite is one provisioned tenant and an isolated config directory.
type suite struct {
	t      *testing.T
	apiKey string
	home   string
}

func newSuite(t *testing.T) *suite {
	t.Helper()
	if baseURL == "" {
		t.Skip("set BILLKIT_INTEGRATION_BASE_URL to run the CLI integration suite")
	}
	key := os.Getenv("BILLKIT_INTEGRATION_API_KEY")
	if key == "" {
		key = provisionTenant(t)
	}
	return &suite{t: t, apiKey: key, home: t.TempDir()}
}

// provisionTenant mints a fresh tenant and a test-mode key, the way every SDK
// suite does, so list assertions see only this run's rows.
func provisionTenant(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 6)
	_, _ = rand.Read(buf)
	body, _ := json.Marshal(map[string]string{
		"email":       "cli-it-" + hex.EncodeToString(buf) + "@sdk-it.example.com",
		"mode":        "test",
		"tenant_name": "CLI IT",
	})
	resp, err := http.Post(baseURL+"/v1/console/auth/_test/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("provisioning a tenant: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		t.Fatal("the test-login backdoor answered 404; boot the API with BILLKIT_E2E_TEST_LOGIN=1 (sdk/integration/boot-api.sh)")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("provisioning a tenant: HTTP %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.APIKey == "" {
		t.Fatalf("provisioning a tenant: no api_key in %s", raw)
	}
	return out.APIKey
}

// env is the environment every invocation runs with: nothing inherited from
// the developer's own BILLKIT_* variables, a private config directory, and
// the harness API.
func (s *suite) env(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "BILLKIT_") && !strings.HasPrefix(kv, "NO_COLOR=") {
			env = append(env, kv)
		}
	}
	env = append(env,
		"BILLKIT_CONFIG_HOME="+s.home,
		"BILLKIT_BASE_URL="+baseURL,
		"BILLKIT_API_KEY="+s.apiKey,
	)
	return append(env, extra...)
}

type result struct {
	stdout, stderr string
	code           int
}

// run executes the binary to completion.
func (s *suite) run(stdin string, env []string, args ...string) result {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, args...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		s.t.Fatalf("running billkit %v: %v", args, err)
	}
	return result{out.String(), errOut.String(), code}
}

func (s *suite) mustRun(args ...string) result {
	s.t.Helper()
	r := s.run("", s.env(), args...)
	if r.code != 0 {
		s.t.Fatalf("billkit %v exited %d\nstdout: %s\nstderr: %s", args, r.code, r.stdout, r.stderr)
	}
	return r
}

// TestLoginValidatesAgainstPing logs in the way a user does, with the key on
// stdin and nothing in the environment, and checks the stored profile.
func TestLoginValidatesAgainstPing(t *testing.T) {
	s := newSuite(t)
	env := s.env()
	// Login must take the key from stdin here, not from BILLKIT_API_KEY.
	for i, kv := range env {
		if strings.HasPrefix(kv, "BILLKIT_API_KEY=") {
			env[i] = "BILLKIT_API_KEY="
		}
	}
	r := s.run(s.apiKey+"\n", env, "login")
	if r.code != 0 {
		t.Fatalf("login exited %d: %s", r.code, r.stderr)
	}
	if r.stdout != "" {
		t.Errorf("login wrote to stdout: %q", r.stdout)
	}
	if !strings.Contains(r.stderr, "Logged in (test mode)") {
		t.Errorf("stderr = %q", r.stderr)
	}

	list := s.run("", env, "config", "list", "--json")
	var cfg struct {
		Default  string `json:"default_profile"`
		Profiles []struct {
			Name    string `json:"name"`
			Storage string `json:"key_storage"`
			BaseURL string `json:"base_url"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(list.stdout), &cfg); err != nil {
		t.Fatalf("config list --json: %v\n%s", err, list.stdout)
	}
	if cfg.Default != "test" || len(cfg.Profiles) != 1 || cfg.Profiles[0].Storage != "file" || cfg.Profiles[0].BaseURL != baseURL {
		t.Fatalf("stored config = %+v", cfg)
	}

	// The stored profile alone is enough for a call.
	if r := s.run("", env, "events", "list", "--limit", "1"); r.code != 0 {
		t.Fatalf("a call on the stored profile exited %d: %s", r.code, r.stderr)
	}

	// And a key the API rejects is an authentication failure, exit 4.
	bad := s.run("bk_test_notarealkey000000000000\n", env, "login", "--profile", "bad")
	if bad.code != 4 {
		t.Fatalf("login with a rejected key exited %d, want 4: %s", bad.code, bad.stderr)
	}
}

// TestListsRunAgainstTheRealRoutes: each list command reaches its route with
// the query it builds and prints the API's list envelope on stdout.
func TestListsRunAgainstTheRealRoutes(t *testing.T) {
	s := newSuite(t)
	for _, args := range [][]string{
		{"refunds", "list", "--limit", "5"},
		{"one-shots", "list", "--limit", "5"},
		{"one-shots", "list", "--customer", "cus_doesnotexist"},
		{"events", "list", "--limit", "5", "--type", "customer.created"},
	} {
		r := s.mustRun(args...)
		var page struct {
			Object string            `json:"object"`
			Data   []json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(r.stdout), &page); err != nil || page.Data == nil {
			t.Errorf("%v: stdout is not a list envelope: %v\n%s", args, err, r.stdout)
		}
	}
	// A bad invocation never reaches the API and exits 2.
	if r := s.run("", s.env(), "refunds", "list", "--limit", "0"); r.code != 2 {
		t.Errorf("--limit 0 exited %d, want 2", r.code)
	}
	// --json errors are one JSON object on stderr.
	r := s.run("", s.env(), "refunds", "retrieve", "re_doesnotexist", "--json")
	// The last line: a non-default host is announced on stderr first.
	lines := strings.Split(strings.TrimSpace(r.stderr), "\n")
	var env struct {
		Error struct {
			Code       string `json:"code"`
			HTTPStatus int    `json:"http_status"`
		} `json:"error"`
		ExitCode int `json:"exit_code"`
	}
	if r.code != 1 || json.Unmarshal([]byte(lines[len(lines)-1]), &env) != nil || env.Error.HTTPStatus != 404 || env.ExitCode != 1 {
		t.Errorf("a 404 under --json: exit %d, stderr %q", r.code, r.stderr)
	}
}

// delivery is one POST the forwarder made to the local app.
type delivery struct {
	body    []byte
	sig     string
	eventID string
	typ     string
}

// receiver is the "local app" listen forwards to.
type receiver struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []delivery
	ch  chan delivery
}

func newReceiver(t *testing.T) *receiver {
	r := &receiver{ch: make(chan delivery, 64)}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		d := delivery{
			body:    body,
			sig:     req.Header.Get("BillKit-Signature"),
			eventID: req.Header.Get("BillKit-Event-Id"),
			typ:     req.Header.Get("BillKit-Event-Type"),
		}
		r.mu.Lock()
		r.got = append(r.got, d)
		r.mu.Unlock()
		r.ch <- d
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) count(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, d := range r.got {
		if d.eventID == id {
			n++
		}
	}
	return n
}

// listenProc is a running `billkit listen`.
type listenProc struct {
	cmd    *exec.Cmd
	secret string
	mu     sync.Mutex
	stderr strings.Builder
}

var secretLine = regexp.MustCompile(`Your webhook signing secret is (bkwhsec_\S+)`)

// startListen runs `billkit listen` in the background and waits for it to
// print its signing secret.
func (s *suite) startListen(apiBase string, args ...string) *listenProc {
	s.t.Helper()
	cmd := exec.Command(binPath, append([]string{"listen"}, args...)...)
	cmd.Env = s.env("BILLKIT_BASE_URL=" + apiBase)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		s.t.Fatal(err)
	}
	cmd.Stdout = io.Discard
	if err := cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	p := &listenProc{cmd: cmd}
	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			line := sc.Text()
			p.mu.Lock()
			p.stderr.WriteString(line + "\n")
			p.mu.Unlock()
			if m := secretLine.FindStringSubmatch(line); m != nil {
				found <- m[1]
			}
		}
	}()
	s.t.Cleanup(func() {
		if runtime.GOOS == "windows" {
			_ = cmd.Process.Kill()
		} else {
			_ = cmd.Process.Signal(os.Interrupt)
		}
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
		}
	})
	select {
	case p.secret = <-found:
	case <-time.After(30 * time.Second):
		s.t.Fatalf("listen printed no signing secret; stderr:\n%s", p.log())
	}
	return p
}

func (p *listenProc) log() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stderr.String()
}

// verify checks a delivery's signature with the session secret, using its
// own HMAC rather than the CLI's sign package, so the CLI is not grading its
// own homework.
func verify(t *testing.T, secret string, d delivery) {
	t.Helper()
	var ts, v1 string
	for _, part := range strings.Split(d.sig, ",") {
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			v1 = v
		}
	}
	if _, err := strconv.ParseInt(ts, 10, 64); err != nil || v1 == "" {
		t.Fatalf("malformed BillKit-Signature %q", d.sig)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(d.body)
	if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(v1)) {
		t.Fatalf("the forwarded event does not verify with the session secret")
	}
	var ev struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(d.body, &ev); err != nil || ev.ID != d.eventID || ev.Type != d.typ {
		t.Fatalf("headers %q/%q do not match the body %s", d.eventID, d.typ, d.body)
	}
}

// triggerUntilDelivered fires `trigger customer.created` until one arrives.
//
// `listen` announces itself before its first connection is up, so the very
// first trigger can land before the stream is open on a fresh tenant whose
// event log was empty at start-up (nothing to resume from). Re-triggering is
// cheaper and more honest than sleeping and hoping.
func (s *suite) triggerUntilDelivered(r *receiver) delivery {
	s.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		out := s.mustRun("trigger", "customer.created")
		if !strings.Contains(out.stderr, "Triggered customer.created") {
			s.t.Fatalf("trigger stderr = %q", out.stderr)
		}
		select {
		case d := <-r.ch:
			return d
		case <-time.After(5 * time.Second):
		}
	}
	s.t.Fatal("no event was forwarded within 60s")
	return delivery{}
}

// TestTriggerListenForwardVerify is the flagship loop: trigger -> the event
// log -> the stream -> listen -> a local app, signed with the session secret.
func TestTriggerListenForwardVerify(t *testing.T) {
	s := newSuite(t)
	r := newReceiver(t)
	p := s.startListen(baseURL, "--forward-to", r.srv.URL, "--events", "customer.created")

	d := s.triggerUntilDelivered(r)
	if d.typ != "customer.created" {
		t.Fatalf("forwarded %q, filtered to customer.created", d.typ)
	}
	verify(t, p.secret, d)

	// And the event log holds the same event.
	out := s.mustRun("events", "list", "--type", "customer.created", "--limit", "100")
	if !strings.Contains(out.stdout, `"`+d.eventID+`"`) {
		t.Fatalf("events list does not show %s:\n%s", d.eventID, out.stdout)
	}
	one := s.mustRun("events", "retrieve", d.eventID)
	if !strings.Contains(one.stdout, `"customer.created"`) {
		t.Fatalf("events retrieve %s:\n%s", d.eventID, one.stdout)
	}
}

// proxy is a TCP relay in front of the API that the test can cut, so the
// stream drops the way a flaky network drops it.
type proxy struct {
	ln       net.Listener
	upstream string
	mu       sync.Mutex
	conns    []net.Conn
	refusing bool
}

func newProxy(t *testing.T, upstream string) *proxy {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{ln: ln, upstream: upstream}
	go p.serve()
	t.Cleanup(func() { _ = ln.Close(); p.cut() })
	return p
}

func (p *proxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		refusing := p.refusing
		p.mu.Unlock()
		if refusing {
			_ = c.Close()
			continue
		}
		up, err := net.Dial("tcp", p.upstream)
		if err != nil {
			_ = c.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, c, up)
		p.mu.Unlock()
		go func() { _, _ = io.Copy(up, c); _ = up.Close() }()
		go func() { _, _ = io.Copy(c, up); _ = c.Close() }()
	}
}

// cut drops every open connection.
func (p *proxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

func (p *proxy) refuse(on bool) {
	p.mu.Lock()
	p.refusing = on
	p.mu.Unlock()
}

// TestListenResumesFromTheLastEventAfterADrop: the stream is cut, an event
// is recorded while it is down, and on reconnect `listen` sends
// starting_after=<the last event it forwarded>, so the server replays the
// missed event before going live, once.
func TestListenResumesFromTheLastEventAfterADrop(t *testing.T) {
	s := newSuite(t)
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "http" {
		t.Skip("the drop is simulated with a plain TCP relay, which needs an http:// base URL")
	}
	px := newProxy(t, u.Host)
	r := newReceiver(t)
	p := s.startListen("http://"+px.ln.Addr().String(), "--forward-to", r.srv.URL, "--events", "customer.created")

	first := s.triggerUntilDelivered(r)
	verify(t, p.secret, first)

	// Cut the stream and keep it down while an event is recorded.
	px.refuse(true)
	px.cut()
	s.mustRun("trigger", "customer.created") // straight to the API, not through the relay
	// The newest customer.created in the log is the one listen missed.
	newest := s.mustRun("events", "list", "--type", "customer.created", "--limit", "1")
	var page struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(newest.stdout), &page); err != nil || len(page.Data) != 1 {
		t.Fatalf("could not read the missed event's id: %v\n%s", err, newest.stdout)
	}
	missedID := page.Data[0].ID
	time.Sleep(1500 * time.Millisecond)
	px.refuse(false)

	var replayed delivery
	deadline := time.After(60 * time.Second)
	for replayed.eventID != missedID {
		select {
		case replayed = <-r.ch:
		case <-deadline:
			t.Fatalf("event %s, recorded while the stream was down, was never replayed; listen stderr:\n%s", missedID, p.log())
		}
	}
	verify(t, p.secret, replayed)
	if replayed.eventID == first.eventID {
		t.Fatalf("the replay re-delivered the event that was already forwarded (%s)", first.eventID)
	}
	if !strings.Contains(p.log(), "Replaying anything recorded after "+first.eventID) {
		t.Fatalf("listen did not resume from %s; stderr:\n%s", first.eventID, p.log())
	}
	// Give a duplicate a moment to show up if there is going to be one.
	time.Sleep(2 * time.Second)
	if n := r.count(replayed.eventID); n != 1 {
		t.Fatalf("event %s was forwarded %d times, want once", replayed.eventID, n)
	}
	if n := r.count(first.eventID); n != 1 {
		t.Fatalf("event %s was forwarded %d times after the reconnect, want once", first.eventID, n)
	}
}
