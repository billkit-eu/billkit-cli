package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// TestExitCodeSeparatesUsageFromFailure. Every error used to leave the
// process with status 1, so a script could not tell "I typed the command
// wrong" from "the call ran and failed" from "I am not logged in", and each
// of those wants a different reaction.
//
// Every row here is refused before a request is built unless it says
// otherwise, so most need no server; the rows that do are pointed at stub
// that answers whatever they ask.
func TestExitCodeSeparatesUsageFromFailure(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer ok.Close()
	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","code":"invalid_api_key","message":"Invalid API key."}}`))
	}))
	defer unauthorized.Close()
	serverError := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"resource_missing","message":"No such refund."}}`))
	}))
	defer serverError.Close()
	testKey := func(base string) []string { return []string{"--api-key", "bk_test_UNIT0001", "--base-url", base} }
	oneShot := func(extra ...string) []string {
		return append([]string{"one-shots", "create", "--customer", "cus_1", "--success-url", "https://x.test/ok"}, extra...)
	}

	cases := []struct {
		name string
		args []string
		want int
	}{
		// ── cobra's own parsing ────────────────────────────────────────
		{"unknown flag", []string{"events", "list", "--nope"}, ExitUsage},
		{"too few arguments", []string{"api", "GET"}, ExitUsage},
		{"too many arguments", []string{"events", "retrieve", "evt_1", "evt_2"}, ExitUsage},
		{"a non-numeric flag value", []string{"events", "list", "--limit", "banana"}, ExitUsage},
		{"-v is not a version flag", []string{"-v"}, ExitUsage},
		// An unknown subcommand. The root used to exit 1 for this, the
		// status for a call that ran and failed, and every group below it
		// printed its help and exited 0, which a script reads as success.
		{"unknown top-level command", []string{"notacommand"}, ExitUsage},
		{"the removed checkout group", []string{"checkout", "one-shot"}, ExitUsage},
		{"unknown refunds subcommand", []string{"refunds", "notasub"}, ExitUsage},
		{"unknown one-shots subcommand", []string{"one-shots", "bogus"}, ExitUsage},
		{"unknown config subcommand", []string{"config", "bogus"}, ExitUsage},
		{"unknown events subcommand", []string{"events", "bogus"}, ExitUsage},
		{"unknown completion shell", []string{"completion", "bogus"}, ExitUsage},
		{"unknown help topic", []string{"help", "bogus"}, ExitUsage},
		{"unknown nested help topic", []string{"help", "refunds", "bogus"}, ExitUsage},

		// ── a flag value this CLI validates itself ─────────────────────
		{"--color bogus", []string{"--color", "bogus", "config", "path"}, ExitUsage},
		{"events list --type bogus", append([]string{"events", "list", "--type", "bogus.event"}, testKey(ok.URL)...), ExitUsage},
		{"events list --limit 0", append([]string{"events", "list", "--limit", "0"}, testKey(ok.URL)...), ExitUsage},
		{"events list --limit 101", append([]string{"events", "list", "--limit", "101"}, testKey(ok.URL)...), ExitUsage},
		{"refunds list --limit -5", append([]string{"refunds", "list", "--limit", "-5"}, testKey(ok.URL)...), ExitUsage},
		{"one-shots list --limit 1000", append([]string{"one-shots", "list", "--limit", "1000"}, testKey(ok.URL)...), ExitUsage},
		{"listen --events bogus", []string{"listen", "--events", "bogus.event"}, ExitUsage},
		{"listen --forward-to ftp://x", []string{"listen", "--forward-to", "ftp://x"}, ExitUsage},
		{"listen --forward-to with no scheme", []string{"listen", "--forward-to", "localhost:3000/hook"}, ExitUsage},
		{"trigger bogus.event", append([]string{"trigger", "bogus.event"}, testKey(ok.URL)...), ExitUsage},
		// cobra checks these two itself and returned its errors untagged.
		{"a missing required flag", []string{"one-shots", "create", "--amount", "100", "--payment-method", "ideal",
			"--success-url", "https://x.test/ok"}, ExitUsage},
		{"one-shots create --payment-method bogus", oneShot("--amount", "100", "--payment-method", "bogus"), ExitUsage},
		{"embedded one-shot with a method", oneShot("--amount", "100", "--ui-mode", "embedded", "--payment-method", "ideal"), ExitUsage},
		{"hosted one-shot with no method", oneShot("--amount", "100"), ExitUsage},
		{"one-shots create --amount 0", oneShot("--amount", "0", "--payment-method", "ideal"), ExitUsage},
		{"one-shots create --amount -1", oneShot("--amount", "-1", "--payment-method", "ideal"), ExitUsage},
		{"one-shots create --ui-mode bogus", oneShot("--amount", "100", "--ui-mode", "bogus"), ExitUsage},
		{"one-shots create --tax-behavior bogus", oneShot("--amount", "100", "--payment-method", "ideal", "--tax-behavior", "gross"), ExitUsage},
		{"one-shots create --metadata without =", oneShot("--amount", "100", "--payment-method", "ideal", "--metadata", "oops"), ExitUsage},
		{"refunds create with no target", []string{"refunds", "create"}, ExitUsage},
		{"refunds create with two targets", []string{"refunds", "create", "--payment", "pay_1", "--one-shot", "osp_1"}, ExitUsage},
		{"refunds create --amount 0", []string{"refunds", "create", "--payment", "pay_1", "--amount", "0"}, ExitUsage},
		{"refunds create --amount -500", []string{"refunds", "create", "--payment", "pay_1", "--amount", "-500"}, ExitUsage},
		{"api GET with a body", []string{"api", "GET", "/v1/customers", "--data", "a=b"}, ExitUsage},
		{"api --data without =", append([]string{"api", "POST", "/v1/customers", "--data", "oops"}, testKey(ok.URL)...), ExitUsage},
		// An empty --input read as no --input and sent a bodiless POST.
		{"api --input= (empty)", append([]string{"api", "POST", "/v1/customers", "--input="}, testKey(ok.URL)...), ExitUsage},
		{"api --input naming a missing file", append([]string{"api", "POST", "/v1/customers", "--input", "/nonexistent/billkit/body.json"}, testKey(ok.URL)...), ExitUsage},

		// ── a profile name that is not configured, everywhere ──────────
		{"logout --profile nope", []string{"logout", "--profile", "nope"}, ExitUsage},
		{"config use nope", []string{"config", "use", "nope"}, ExitUsage},
		{"--profile nope on a call", []string{"--profile", "nope", "events", "list"}, ExitUsage},

		// ── live money with no terminal and no --yes ───────────────────
		{"live refund without --yes in a script", []string{"refunds", "create", "--payment", "pay_1",
			"--api-key", "bk_live_UNIT0001", "--base-url", "https://api.example.test"}, ExitUsage},

		// ── no usable credential ───────────────────────────────────────
		{"no credentials", []string{"events", "list", "--base-url", "https://api.example.test"}, ExitAuth},
		{"no credentials for listen", []string{"listen"}, ExitAuth},
		{"a key the API rejects", append([]string{"events", "list"}, testKey(unauthorized.URL)...), ExitAuth},
		{"a key login is refused with", append([]string{"login"}, testKey(unauthorized.URL)...), ExitAuth},
		{"something that is not a key", []string{"login", "--api-key", "sk_live_stripe"}, ExitAuth},

		// ── ran, and the call failed ───────────────────────────────────
		{"an API error", append([]string{"refunds", "retrieve", "re_404"}, testKey(serverError.URL)...), ExitFailure},
		{"trigger with a live key", []string{"trigger", "customer.created",
			"--api-key", "bk_live_UNIT0001", "--base-url", "https://api.example.test"}, ExitFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
			t.Setenv("BILLKIT_API_KEY", "")
			t.Setenv("BILLKIT_BASE_URL", "")
			t.Setenv("BILLKIT_PROFILE", "")

			root := rootCmd()
			root.SetOut(io.Discard)
			root.SetErr(&bytes.Buffer{})
			root.SetIn(strings.NewReader(""))
			root.SetArgs(tc.args)

			err := root.Execute()
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := ExitCode(err); got != tc.want {
				t.Fatalf("ExitCode = %d, want %d (err: %v)", got, tc.want, err)
			}
		})
	}
}

func TestExitCodeOfNilIsZero(t *testing.T) {
	if got := ExitCode(nil); got != 0 {
		t.Fatalf("ExitCode(nil) = %d, want 0", got)
	}
}

// TestEveryGroupRefusesAnUnknownSubcommand walks the real tree rather than
// listing groups by hand, so a group added later is covered without anyone
// remembering to add it here.
func TestEveryGroupRefusesAnUnknownSubcommand(t *testing.T) {
	var walk func(c *cobra.Command)
	groups := 0
	walk = func(c *cobra.Command) {
		if c.HasSubCommands() {
			groups++
			path := strings.Fields(c.CommandPath())[1:]
			t.Run(c.CommandPath(), func(t *testing.T) {
				root := rootCmd()
				root.SetOut(io.Discard)
				root.SetErr(io.Discard)
				root.SetArgs(append(path, "zzz-not-a-command"))
				err := root.Execute()
				if got := ExitCode(err); got != ExitUsage {
					t.Fatalf("ExitCode = %d, want %d (err: %v)", got, ExitUsage, err)
				}
				if !strings.Contains(err.Error(), "--help") {
					t.Errorf("the error should point at the group's help, got %q", err)
				}
			})
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd())
	if groups < 5 {
		t.Fatalf("found %d group commands, which means the walk missed the tree", groups)
	}
}

// TestBareGroupPrintsHelpAndSucceeds. `billkit refunds` on its own asks what
// the group can do; answering with its help and exit 0 is what git, gh and
// the Stripe CLI do, and it must not become a usage error by accident.
func TestBareGroupPrintsHelpAndSucceeds(t *testing.T) {
	for _, args := range [][]string{{}, {"refunds"}, {"one-shots"}, {"config"}, {"events"}} {
		root := rootCmd()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(io.Discard)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(out.String(), "Available Commands") {
			t.Errorf("%v printed no help: %q", args, out.String())
		}
	}
}

// TestUnknownSubcommandSuggestsTheNearestName keeps the "did you mean" cobra
// gives for an unknown root command, now that the check is ours.
func TestUnknownSubcommandSuggestsTheNearestName(t *testing.T) {
	root := rootCmd()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"refunds", "lst"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "Did you mean this?\n\tlist") {
		t.Fatalf("want a suggestion of `list`, got %v", err)
	}
}

// TestHelpForARealTopicStillSucceeds keeps the help command working for every
// name it should know, now that it validates its topic.
func TestHelpForARealTopicStillSucceeds(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"help", "refunds"}, {"help", "refunds", "create"}, {"help", "completion", "bash"}} {
		root := rootCmd()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(io.Discard)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(out.String(), "Usage:") {
			t.Errorf("%v printed no help: %q", args, out.String())
		}
	}
}

// TestGroupUsageHasNoFlagsLine. A group is runnable only so it can refuse an
// unknown subcommand, and cobra prints `billkit refunds [flags]` for anything
// runnable, which reads as though the group did something on its own. A leaf
// command keeps its line.
func TestGroupUsageHasNoFlagsLine(t *testing.T) {
	cases := []struct {
		args    []string
		want    string
		notWant string
	}{
		{[]string{"--help"}, "\n  billkit [command]", "billkit [flags]"},
		{[]string{"refunds", "--help"}, "\n  billkit refunds [command]", "billkit refunds [flags]"},
		{[]string{"config", "--help"}, "\n  billkit config [command]", "billkit config [flags]"},
		{[]string{"refunds", "create", "--help"}, "\n  billkit refunds create [flags]", ""},
	}
	for _, tc := range cases {
		root := rootCmd()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(io.Discard)
		root.SetArgs(tc.args)
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("%v: usage should contain %q, got %q", tc.args, tc.want, out.String())
		}
		if tc.notWant != "" && strings.Contains(out.String(), tc.notWant) {
			t.Errorf("%v: a group's usage should not contain %q", tc.args, tc.notWant)
		}
	}
}

// TestANonBillKitKeyIsNeverSent. `login` refused a key without a bk_test_ or
// bk_live_ prefix, but every other command sent whatever it was given and
// let the API answer 401: the exit code matched, and a foreign secret (a
// Stripe key in the wrong variable) had already gone over the wire. resolve
// now makes the same check for every command and every tier.
func TestANonBillKitKeyIsNeverSent(t *testing.T) {
	// TLS, so the transport check (plain http only for a loopback test key)
	// is not what stops the request: only the prefix check can.
	var hits atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv.Close()
	prev := transportOverride
	transportOverride = srv.Client().Transport
	t.Cleanup(func() { transportOverride = prev })

	commands := [][]string{
		{"events", "list"},
		{"events", "retrieve", "evt_1"},
		{"refunds", "list"},
		{"refunds", "create", "--payment", "pay_1"},
		{"one-shots", "create", "--customer", "cus_1", "--amount", "100", "--payment-method", "ideal", "--success-url", "https://x.test/ok"},
		{"api", "GET", "/v1/customers"},
		{"trigger", "customer.created"},
		{"listen"},
	}
	for _, args := range commands {
		for _, viaEnv := range []bool{false, true} {
			name := strings.Join(args[:min(2, len(args))], " ")
			if viaEnv {
				name += " (env)"
			}
			t.Run(name, func(t *testing.T) {
				t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
				t.Setenv("BILLKIT_API_KEY", "")
				t.Setenv("BILLKIT_BASE_URL", "")
				t.Setenv("BILLKIT_PROFILE", "")
				full := append(slices.Clone(args), "--base-url", srv.URL)
				if viaEnv {
					t.Setenv("BILLKIT_API_KEY", "sk_test_stripe_key")
				} else {
					full = append(full, "--api-key", "sk_test_stripe_key")
				}
				before := hits.Load()

				root := rootCmd()
				root.SetOut(io.Discard)
				var stderr bytes.Buffer
				root.SetErr(&stderr)
				root.SetIn(strings.NewReader(""))
				root.SetArgs(full)
				// Bounded, so `listen` cannot hold the suite if the check
				// regresses and the stream is opened after all.
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				err := root.ExecuteContext(ctx)

				if got := ExitCode(err); got != ExitAuth {
					t.Fatalf("ExitCode = %d, want %d (err: %v)", got, ExitAuth, err)
				}
				if !strings.Contains(err.Error(), "doesn't look like a BillKit key") {
					t.Errorf("the error must say why: %v", err)
				}
				if strings.Contains(err.Error()+stderr.String(), "sk_test_stripe_key") {
					t.Errorf("the key itself must never be printed: %v / %q", err, stderr.String())
				}
				if n := hits.Load() - before; n != 0 {
					t.Fatalf("%d request(s) reached the host carrying a non-BillKit key", n)
				}
			})
		}
	}
}
