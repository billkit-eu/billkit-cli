package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// colorizeJSON must always emit valid JSON, including for edge-case inputs
// (control chars, empty containers, top-level scalars, floats/negatives).
func TestColorizeJSONAlwaysValid(t *testing.T) {
	cases := []string{
		`{}`,
		`[]`,
		`42`,
		`-3.14e10`,
		`null`,
		`"top-level"`,
		`{"tab":"a\tb\nc"}`,
		// Control chars (NUL, DEL) arrive as \u escapes and are decoded to
		// real runes by json.Decoder — the case the strconv.Quote bug broke.
		"{\"ctrl\":\"a\\u0000b\\u007fc\"}",
		`{"unicode":"héllo 🎉","html":"<a>&</a>"}`,
		`{"nested":{"a":[1,2,{"b":null}],"c":{}},"n":-9007199254740991}`,
		`[{"id":"re_1","amount_cents":500,"livemode":false}]`,
	}
	for _, in := range cases {
		var sb strings.Builder
		if err := colorizeJSON(&sb, []byte(in), false); err != nil {
			t.Fatalf("colorizeJSON(%s) error: %v", in, err)
		}
		got := sb.String()
		if !json.Valid([]byte(got)) {
			t.Fatalf("colorizeJSON(%s) produced invalid JSON:\n%s", in, got)
		}
		var b any
		if err := json.Unmarshal([]byte(got), &b); err != nil {
			t.Fatalf("re-parse of colorized %s failed: %v", in, err)
		}
	}
}

func TestColorizeJSONPreservesKeyOrderPlain(t *testing.T) {
	// Object key order must survive (a map round-trip would scramble it).
	raw := []byte(`{"id":"re_1","amount_cents":500,"livemode":false,"reason":null,"nested":{"b":1,"a":2},"list":[1,"two",true]}`)
	var sb strings.Builder
	if err := colorizeJSON(&sb, raw, false); err != nil {
		t.Fatal(err)
	}
	got := sb.String()

	if strings.Contains(got, "\x1b[") {
		t.Fatalf("unexpected ANSI in plain output:\n%s", got)
	}
	order := []string{`"id"`, `"amount_cents"`, `"livemode"`, `"reason"`, `"nested"`, `"list"`}
	last := -1
	for _, k := range order {
		i := strings.Index(got, k)
		if i < 0 {
			t.Fatalf("missing key %s in:\n%s", k, got)
		}
		if i < last {
			t.Fatalf("key %s out of order in:\n%s", k, got)
		}
		last = i
	}
	for _, want := range []string{`"re_1"`, "500", "false", "null", "[", "]", "{", "}"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "\n  ") {
		t.Fatalf("expected 2-space indentation in:\n%s", got)
	}
}

func TestColorizeJSONColorWraps(t *testing.T) {
	raw := []byte(`{"k":"v"}`)
	var sb strings.Builder
	if err := colorizeJSON(&sb, raw, true); err != nil {
		t.Fatal(err)
	}
	got := sb.String()
	if !strings.Contains(got, ansiKey) || !strings.Contains(got, ansiStr) {
		t.Fatalf("expected key/string colour codes in:\n%q", got)
	}
	if !strings.Contains(got, ansiReset) {
		t.Fatalf("expected reset code in:\n%q", got)
	}
}

func TestColorEnabledRespectsNever(t *testing.T) {
	if colorEnabled(&strings.Builder{}, "never") {
		t.Fatal("never should disable color")
	}
	if !colorEnabled(&strings.Builder{}, "always") {
		t.Fatal("always should enable color")
	}
}

func TestColorEnabledAutoOffForNonTerminal(t *testing.T) {
	if colorEnabled(&strings.Builder{}, "auto") {
		t.Fatal("auto should disable color for a non-terminal writer")
	}
}

func TestParseMetadata(t *testing.T) {
	m, err := parseMetadata([]string{"order=42", "tier=gold"})
	if err != nil {
		t.Fatal(err)
	}
	if m["order"] != "42" || m["tier"] != "gold" {
		t.Fatalf("metadata = %v", m)
	}
	if _, err := parseMetadata([]string{"bad"}); err == nil {
		t.Fatal("expected error for malformed pair")
	}
	if m, _ := parseMetadata(nil); m != nil {
		t.Fatal("empty input should yield nil map")
	}
}

// A command's JSON goes to the writer cobra was handed, not to os.Stdout.
//
// This is what the fprintJSON(w, ...) shape buys: before it, every command
// wrote straight to os.Stdout, so a test (or any caller embedding the command
// tree) could redirect the command and still see nothing. The assertion is on
// the plumbing, not on the API — hence the stubbed transport.
func TestCommandJSONGoesToTheCobraWriter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"evt_1"}]}`))
	}))
	defer srv.Close()

	g := &globals{apiKey: "bk_test_unit", baseURL: srv.URL, color: "never"}
	cmd := eventsCmd(g)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("events list: %v", err)
	}

	if !strings.Contains(out.String(), `"evt_1"`) {
		t.Fatalf("the event JSON did not reach the command's own writer:\n%q", out.String())
	}
	if !json.Valid([]byte(strings.TrimSpace(out.String()))) {
		t.Fatalf("stdout is not valid JSON:\n%s", out.String())
	}
	// The pre-request notices are stderr's, so stdout stays pipeable.
	if strings.Contains(out.String(), "Using API host") {
		t.Fatalf("a human-readable notice leaked onto stdout:\n%s", out.String())
	}
}

// TestPlainTextOutputGoesToTheCobraWriter is the other half of the same
// invariant, and the half that had quietly rotted: `config list`, `config
// use`, `config path`, `login` and a bare `trigger` all still called
// fmt.Println, so their output went to os.Stdout no matter where the caller
// pointed the command. A test could not read them, and an embedding caller
// could not capture them.
func TestPlainTextOutputGoesToTheCobraWriter(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	g := &globals{color: "never"}

	for _, tc := range []struct {
		name string
		cmd  func() *cobra.Command
		args []string
		want string
	}{
		{"config path", func() *cobra.Command { return configCmd(g) }, []string{"path"}, "config.json"},
		{"config list", func() *cobra.Command { return configCmd(g) }, []string{"list"}, "No profiles configured"},
		{"trigger", func() *cobra.Command { return triggerCmd(g) }, nil, "customer.created"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := tc.cmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(tc.args)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Fatalf("%s wrote nothing containing %q to the command's own writer, got %q",
					tc.name, tc.want, out.String())
			}
		})
	}
}

// The --color mode is a value the command carries, not process state. Two
// trees in one process must be able to disagree about it.
func TestColorModeIsPerCommandTree(t *testing.T) {
	var plain, painted strings.Builder
	fprintJSON(&plain, "never", []byte(`{"a":1}`))
	fprintJSON(&painted, "always", []byte(`{"a":1}`))

	if strings.Contains(plain.String(), "\x1b[") {
		t.Fatalf("mode=never still emitted ANSI: %q", plain.String())
	}
	if !strings.Contains(painted.String(), "\x1b[") {
		t.Fatalf("mode=always emitted no ANSI: %q", painted.String())
	}
}
