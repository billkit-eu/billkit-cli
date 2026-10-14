package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/billkit-eu/billkit-cli/internal/api"
)

// runSplit drives the real tree and returns stdout and stderr separately,
// which is what every output-contract assertion needs.
func runSplit(t *testing.T, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := rootCmd()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err = root.Execute()
	return out.String(), errOut.String(), err
}

// ── money formatting ──────────────────────────────────────────────────────

func TestFormatMoney(t *testing.T) {
	cases := []struct {
		minor    int64
		currency string
		want     string
	}{
		{1999, "EUR", "EUR 19.99"},
		{1999, "eur", "EUR 19.99"},
		{5, "EUR", "EUR 0.05"},
		{0, "USD", "USD 0.00"},
		{100, "GBP", "GBP 1.00"},
		{1999, "JPY", "JPY 1999"},
		{1999, "KRW", "KRW 1999"},
		{1999, "BHD", "BHD 1.999"},
		{1, "KWD", "KWD 0.001"},
		{-250, "EUR", "EUR -2.50"},
		// A well-formed code the API has no override for uses the API's
		// own default exponent, 2, because that is how the API charges it.
		{1999, "SEK", "SEK 19.99"},
		// Past 2^53: integer arithmetic, so not a digit is rounded.
		{9007199254740993, "EUR", "EUR 90071992547409.93"},
		// Not a currency code: nothing is guessed.
		{1999, "", "1999 minor units (currency unknown)"},
		{1999, "EURO", `1999 minor units of "EURO"`},
		{1999, "E1R", `1999 minor units of "E1R"`},
	}
	for _, tc := range cases {
		if got := formatMoney(tc.minor, tc.currency); got != tc.want {
			t.Errorf("formatMoney(%d, %q) = %q, want %q", tc.minor, tc.currency, got, tc.want)
		}
	}
}

// TestCurrencyExponentsMatchTheAPI reads the API's minor-unit table in a
// monorepo checkout and fails if this copy has drifted from it in either
// direction, the same way api/tests/test_cli_event_constants.py guards the
// event list. In the public mirror there is no api/ and it skips.
func TestCurrencyExponentsMatchTheAPI(t *testing.T) {
	path := filepath.Join("..", "..", "..", "api", "billkit", "services", "currency.py")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("no api/ beside cli/ (the public mirror); parity is checked in the monorepo")
	}
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	start := strings.Index(src, "_MINOR_UNITS: dict[str, int] = {")
	if start < 0 {
		t.Fatal("could not find _MINOR_UNITS in currency.py; update this test with the API")
	}
	end := strings.Index(src[start:], "}")
	entry := regexp.MustCompile(`"([A-Z]{3})":\s*(\d+)`)
	api := map[string]int{}
	for _, m := range entry.FindAllStringSubmatch(src[start:start+end], -1) {
		n, _ := strconv.Atoi(m[2])
		api[m[1]] = n
	}
	if len(api) == 0 {
		t.Fatal("parsed no entries from _MINOR_UNITS")
	}
	for code, exp := range api {
		if got, ok := currencyExponents[code]; !ok || got != exp {
			t.Errorf("the API scales %s by %d; currencyExponents has %d (present: %v)", code, exp, got, ok)
		}
	}
	for code := range currencyExponents {
		if _, ok := api[code]; !ok {
			t.Errorf("currencyExponents has %s, which the API does not override", code)
		}
	}
	def := regexp.MustCompile(`_DEFAULT_MINOR_UNITS\s*=\s*(\d+)`).FindStringSubmatch(src)
	if def == nil || def[1] != strconv.Itoa(defaultCurrencyExponent) {
		t.Errorf("the API's default exponent is %v; defaultCurrencyExponent is %d", def, defaultCurrencyExponent)
	}
}

// TestOneShotConfirmationShowsMoneyNotMinorUnits is the 1.0 bug: the
// confirmation read `charge 1999 EUR`, which is EUR 1999 to anyone reading
// it, for a charge of EUR 19.99. That line is the one a user reads in the
// live-mode banner before saying yes.
func TestOneShotConfirmationShowsMoneyNotMinorUnits(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	for _, tc := range []struct{ currency, want string }{
		{"EUR", "charge EUR 19.99 to cus_1 via ideal"},
		{"JPY", "charge JPY 1999 to cus_1 via ideal"},
	} {
		stderr, err := runCLI(t, "one-shots", "create", "--customer", "cus_1", "--amount", "1999",
			"--currency", tc.currency, "--payment-method", "ideal", "--success-url", "https://x.test/ok",
			"--api-key", "bk_test_UNIT0001", "--base-url", srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stderr, tc.want) {
			t.Errorf("stderr should say %q, got:\n%s", tc.want, stderr)
		}
	}
}

// TestLiveBannerShowsTheFormattedAmount: the same, in the banner itself.
func TestLiveBannerShowsTheFormattedAmount(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	srv := tlsStub(t, rec.handler())
	_, stderr, err := runSplit(t, "", "one-shots", "create", "--customer", "cus_1", "--amount", "1999",
		"--payment-method", "ideal", "--success-url", "https://x.test/ok",
		"--api-key", "bk_live_UNIT0001", "--base-url", srv.URL)
	if ExitCode(err) != ExitUsage {
		t.Fatalf("an unconfirmed live charge in a script must be refused as a bad invocation, got %v", err)
	}
	if !strings.Contains(stderr, "About to charge EUR 19.99 to cus_1") {
		t.Fatalf("the banner does not show the amount as money:\n%s", stderr)
	}
	if len(rec.mutations()) != 0 {
		t.Fatal("an unconfirmed live charge reached the API")
	}
}

// TestRefundBannerLooksUpTheTargetsCurrency: a refund body carries no
// currency, so the CLI reads the payment's before it describes the refund.
func TestRefundBannerLooksUpTheTargetsCurrency(t *testing.T) {
	var gets []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			gets = append(gets, r.URL.Path)
			_, _ = w.Write([]byte(`{"id":"pay_1","currency":"BHD"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"re_1"}`))
	}))
	defer srv.Close()

	stderr, err := runCLI(t, "refunds", "create", "--payment", "pay_1", "--amount", "1500",
		"--api-key", "bk_test_UNIT0001", "--base-url", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "refund BHD 1.500 of pay_1") {
		t.Fatalf("want the amount in the payment's currency, got:\n%s", stderr)
	}
	if len(gets) != 1 || gets[0] != "/v1/payments/pay_1" {
		t.Fatalf("lookups = %v, want one GET /v1/payments/pay_1", gets)
	}
}

// TestRefundBannerFallsBackToMinorUnits: a key without payments:read, or any
// failed lookup, still refunds, and the amount is shown without a guessed
// exponent.
func TestRefundBannerFallsBackToMinorUnits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"insufficient_scope","message":"no"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"re_1"}`))
	}))
	defer srv.Close()
	stderr, err := runCLI(t, "refunds", "create", "--one-shot", "osp_1", "--amount", "500",
		"--api-key", "bk_test_UNIT0001", "--base-url", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "refund 500 minor units (currency unknown) of osp_1") {
		t.Fatalf("want the minor-units fallback, got:\n%s", stderr)
	}
}

// ── one-shots ─────────────────────────────────────────────────────────────

func TestOneShotsListAndRetrieve(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	auth := []string{"--api-key", "bk_test_UNIT0001", "--base-url", srv.URL}

	if _, err := runCLI(t, append([]string{"one-shots", "list", "--customer", "cus 1", "--status", "paid",
		"--limit", "100", "--starting-after", "osp_9"}, auth...)...); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, append([]string{"one-shots", "retrieve", "osp/1"}, auth...)...); err != nil {
		t.Fatal(err)
	}
	all := rec.all()
	if len(all) != 2 {
		t.Fatalf("requests = %+v", all)
	}
	if all[0].path != "/v1/checkout/one_shot" {
		t.Errorf("list path = %s", all[0].path)
	}
	if all[1].path != "/v1/checkout/one_shot/osp/1" && all[1].path != "/v1/checkout/one_shot/osp%2F1" {
		t.Errorf("retrieve path = %s", all[1].path)
	}
}

func TestOneShotsListSendsItsFilters(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv.Close()
	if _, err := runCLI(t, "one-shots", "list", "--customer", "cus_1", "--status", "paid", "--limit", "1",
		"--starting-after", "osp_9", "--api-key", "bk_test_UNIT0001", "--base-url", srv.URL); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"customer_id=cus_1", "status=paid", "limit=1", "starting_after=osp_9"} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q is missing %s", query, want)
		}
	}
}

// TestLimitIsValidatedNotDropped: `--limit -5` used to be silently dropped,
// so the user got the API's default page and read it as their own.
func TestLimitIsValidatedNotDropped(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	for _, cmd := range [][]string{{"events", "list"}, {"refunds", "list"}, {"one-shots", "list"}} {
		for _, bad := range []string{"0", "-5", "101"} {
			args := append(append([]string{}, cmd...), "--limit", bad, "--api-key", "bk_test_UNIT0001", "--base-url", srv.URL)
			_, err := runCLI(t, args...)
			if ExitCode(err) != ExitUsage || !strings.Contains(fmt.Sprint(err), "--limit") {
				t.Errorf("%v --limit %s: want a usage error naming --limit, got %v", cmd, bad, err)
			}
		}
	}
	if len(rec.all()) != 0 {
		t.Fatalf("an invalid --limit reached the wire: %+v", rec.all())
	}
}

// ── output contract ───────────────────────────────────────────────────────

// TestLoginAndConfigWriteMessagesToStderrOnly. A confirmation is a message,
// not data: on stdout it lands in whatever a script is capturing.
func TestLoginAndConfigWriteMessagesToStderrOnly(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	for _, args := range [][]string{
		{"login", "--api-key", "bk_test_UNIT0001", "--base-url", srv.URL},
		{"login", "--profile", "other", "--api-key", "bk_test_UNIT0002", "--base-url", srv.URL},
		{"config", "use", "other"},
		{"logout", "--profile", "other"},
	} {
		stdout, stderr, err := runSplit(t, "", args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if stdout != "" {
			t.Errorf("%v wrote to stdout: %q", args, stdout)
		}
		if !strings.Contains(stderr, "✓") {
			t.Errorf("%v: the confirmation should be on stderr, got %q", args, stderr)
		}
	}
}

func TestConfigListJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BILLKIT_CONFIG_HOME", dir)
	useFakeKeyring(t)
	rec := &recorder{}
	srv := tlsStub(t, rec.handler())

	if _, err := runCLI(t, "login", "--api-key", "bk_live_ABCDEFGHSECRET", "--base-url", srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, "login", "--api-key", "bk_test_ABCDEFGHSECRET", "--base-url", srv.URL); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runSplit(t, "", "config", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "SECRET") {
		t.Fatalf("config list --json printed a key:\n%s", stdout)
	}
	var got struct {
		Path     string `json:"config_path"`
		Default  string `json:"default_profile"`
		Profiles []struct {
			Name    string `json:"name"`
			Default bool   `json:"default"`
			Mode    string `json:"mode"`
			APIKey  string `json:"api_key"`
			Storage string `json:"key_storage"`
			BaseURL string `json:"base_url"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if got.Path != filepath.Join(dir, "config.json") || got.Default != "test" || len(got.Profiles) != 2 {
		t.Fatalf("unexpected: %+v", got)
	}
	live, test := got.Profiles[0], got.Profiles[1]
	if live.Name != "live" || live.Mode != "live" || live.Storage != "keychain" || live.Default {
		t.Errorf("live = %+v", live)
	}
	if test.Name != "test" || test.Mode != "test" || test.Storage != "file" || !test.Default || test.APIKey != "bk_test_ABCD…" {
		t.Errorf("test = %+v", test)
	}

	// The table form masks the same way and goes to stdout.
	table, _, err := runSplit(t, "", "config", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(table, "bk_live_… (keychain)") || !strings.Contains(table, "* test") || strings.Contains(table, "SECRET") {
		t.Fatalf("table:\n%s", table)
	}

	// An empty config is an empty list under --json, not a sentence.
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	stdout, _, err = runSplit(t, "", "config", "list", "--json")
	if err != nil || !strings.Contains(stdout, `"profiles": []`) {
		t.Fatalf("empty config list --json = %q, %v", stdout, err)
	}
}

func TestTriggerListingIsDataOnStdout(t *testing.T) {
	stdout, stderr, err := runSplit(t, "", "trigger")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != len(triggers) || lines[0] != sortedTriggerNames()[0] {
		t.Fatalf("stdout should be exactly one event type per line, got:\n%s", stdout)
	}
	if !strings.Contains(stderr, "Supported trigger events") {
		t.Errorf("the heading is a message and belongs on stderr; stderr = %q", stderr)
	}

	stdout, _, err = runSplit(t, "", "trigger", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var items []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(stdout), &items); err != nil || len(items) != len(triggers) {
		t.Fatalf("trigger --json = %q (%v)", stdout, err)
	}
}

// ── errors under --json ───────────────────────────────────────────────────

func TestErrorsAreMachineReadableUnderJSON(t *testing.T) {
	apiErr := api.ParseError(404, []byte(`{"error":{"type":"invalid_request_error","code":"resource_missing","message":"No such refund.","param":"id"}}`))
	paymentRefused := api.ParseError(402, []byte(`{"error":{"type":"payment_error","code":"payment_refused","message":"The payment provider refused the refund.","reason":"payment_refused"}}`))
	cases := []struct {
		name     string
		err      error
		wantText string
		wantJSON errorEnvelope
	}{
		{
			"api error", fmt.Errorf("wrapped: %w", apiErr),
			"Error: wrapped: resource_missing (HTTP 404): No such refund. [param: id]\n",
			errorEnvelope{Error: errorBody{Type: "invalid_request_error", Code: "resource_missing", Message: "No such refund.", Param: "id", HTTPStatus: 404}, ExitCode: ExitFailure},
		},
		{
			// A provider refusal is a call that ran and failed, not a bad
			// invocation: exit 1, with the API's own type and reason relayed.
			"payment refused", paymentRefused,
			"Error: payment_refused (HTTP 402): The payment provider refused the refund. [reason: payment_refused]\n",
			errorEnvelope{Error: errorBody{Type: "payment_error", Code: "payment_refused", Message: "The payment provider refused the refund.", Reason: "payment_refused", HTTPStatus: 402}, ExitCode: ExitFailure},
		},
		{
			"usage", usageErrorf("--limit must be between 1 and 100, got 0"),
			"Error: --limit must be between 1 and 100, got 0\n",
			errorEnvelope{Error: errorBody{Type: cliErrorType, Code: codeUsage, Message: "--limit must be between 1 and 100, got 0"}, ExitCode: ExitUsage},
		},
		{
			"auth", errNoAPIKey(),
			"Error: " + errNoAPIKey().Error() + "\n",
			errorEnvelope{Error: errorBody{Type: cliErrorType, Code: codeAuth, Message: errNoAPIKey().Error()}, ExitCode: ExitAuth},
		},
		{
			"plain failure", errors.New("dial tcp: connection refused"),
			"Error: dial tcp: connection refused\n",
			errorEnvelope{Error: errorBody{Type: cliErrorType, Code: codeFailure, Message: "dial tcp: connection refused"}, ExitCode: ExitFailure},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var text bytes.Buffer
			writeError(&text, tc.err, false)
			if text.String() != tc.wantText {
				t.Errorf("text = %q, want %q", text.String(), tc.wantText)
			}
			var js bytes.Buffer
			writeError(&js, tc.err, true)
			if strings.Count(js.String(), "\n") != 1 {
				t.Errorf("the JSON error must be one line, got %q", js.String())
			}
			var got errorEnvelope
			if err := json.Unmarshal(js.Bytes(), &got); err != nil {
				t.Fatalf("not JSON: %v (%q)", err, js.String())
			}
			if got != tc.wantJSON {
				t.Errorf("json = %+v, want %+v", got, tc.wantJSON)
			}
		})
	}
}

// ── version ───────────────────────────────────────────────────────────────

func TestVersionCommandAndFlagAgree(t *testing.T) {
	prevC, prevD := Commit, Date
	Commit, Date = "0123456789abcdef0123456789abcdef01234567", "2026-10-13T12:00:00Z"
	t.Cleanup(func() { Commit, Date = prevC, prevD })

	cmdOut, _, err := runSplit(t, "", "version")
	if err != nil {
		t.Fatal(err)
	}
	flagOut, _, err := runSplit(t, "", "--version")
	if err != nil {
		t.Fatal(err)
	}
	want := "billkit version " + Version + " (commit 0123456789ab, built 2026-10-13T12:00:00Z, "
	if !strings.HasPrefix(cmdOut, want) {
		t.Fatalf("version = %q, want prefix %q", cmdOut, want)
	}
	if cmdOut != flagOut {
		t.Fatalf("`version` and `--version` disagree:\n%q\n%q", cmdOut, flagOut)
	}

	js, _, err := runSplit(t, "", "version", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var d buildDetails
	if err := json.Unmarshal([]byte(js), &d); err != nil {
		t.Fatal(err)
	}
	if d.Version != Version || d.Commit != Commit || d.Date != Date || d.OS == "" || d.Go == "" {
		t.Fatalf("version --json = %+v", d)
	}
}

// ── keychain through the commands ─────────────────────────────────────────

func TestLoginPutsALiveKeyInTheKeychainAndLogoutTakesItOut(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BILLKIT_CONFIG_HOME", dir)
	kr := useFakeKeyring(t)
	rec := &recorder{}
	srv := tlsStub(t, rec.handler())

	_, stderr, err := runSplit(t, "", "login", "--api-key", "bk_live_KEYCHAINSECRET", "--base-url", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "OS keychain") {
		t.Errorf("login should say where the key went: %q", stderr)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if strings.Contains(string(raw), "KEYCHAINSECRET") {
		t.Fatalf("the live key is in the config file:\n%s", raw)
	}
	if kr.len() != 1 {
		t.Fatalf("keychain items = %d", kr.len())
	}

	// And every command can use it.
	if _, err := runCLI(t, "events", "list", "--base-url", srv.URL); err != nil {
		t.Fatal(err)
	}
	last := rec.all()[len(rec.all())-1]
	if last.auth != "Bearer bk_live_KEYCHAINSECRET" {
		t.Fatalf("the keychain key was not used: %q", last.auth)
	}

	if _, _, err := runSplit(t, "", "logout"); err != nil {
		t.Fatal(err)
	}
	if kr.len() != 0 {
		t.Fatalf("logout left %d item(s) in the keychain", kr.len())
	}
}

// TestLogoutAllEmptiesTheKeychainToo: --all used to replace the profile map
// wholesale, which would have stranded every keychain entry.
func TestLogoutAllEmptiesTheKeychainToo(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	kr := useFakeKeyring(t)
	srv := tlsStub(t, (&recorder{}).handler())
	for _, args := range [][]string{
		{"login", "--api-key", "bk_live_ONE", "--base-url", srv.URL},
		{"login", "--profile", "live2", "--api-key", "bk_live_TWO", "--base-url", srv.URL},
	} {
		if _, err := runCLI(t, args...); err != nil {
			t.Fatal(err)
		}
	}
	if kr.len() != 2 {
		t.Fatalf("setup: keychain items = %d", kr.len())
	}
	if _, err := runCLI(t, "logout", "--all"); err != nil {
		t.Fatal(err)
	}
	if kr.len() != 0 {
		t.Fatalf("logout --all left %d item(s) in the keychain", kr.len())
	}
}

func TestLoginWarnsWhenTheKeychainIsUnavailable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BILLKIT_CONFIG_HOME", dir)
	kr := useFakeKeyring(t)
	kr.failSet = errors.New("no secret service")
	srv := tlsStub(t, (&recorder{}).handler())

	_, stderr, err := runSplit(t, "", "login", "--api-key", "bk_live_FALLBACKSECRET", "--base-url", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "keychain was not available") || !strings.Contains(stderr, "0600") {
		t.Fatalf("the fallback must be announced: %q", stderr)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if !strings.Contains(string(raw), "FALLBACKSECRET") {
		t.Fatal("the fallback must still store the key")
	}
}

func TestLoginInsecureStorageSkipsTheKeychain(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	kr := useFakeKeyring(t)
	srv := tlsStub(t, (&recorder{}).handler())
	if _, err := runCLI(t, "login", "--insecure-storage", "--api-key", "bk_live_PLAINSECRET", "--base-url", srv.URL); err != nil {
		t.Fatal(err)
	}
	if kr.len() != 0 {
		t.Fatal("--insecure-storage wrote to the keychain")
	}
}

// TestNoNetworkForLocalCommands: version, config and help answer without a
// request. There is no telemetry and no update check; a stub that fails the
// test on any request is the cheapest way to keep it that way.
func TestNoNetworkForLocalCommands(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer srv.Close()
	t.Setenv("BILLKIT_BASE_URL", srv.URL)
	t.Setenv("BILLKIT_API_KEY", "bk_test_UNIT0001")
	for _, args := range [][]string{{"version"}, {"--version"}, {"config", "list"}, {"config", "path"}, {"trigger"}, {"--help"}} {
		if _, _, err := runSplit(t, "", args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if hit {
		t.Fatal("a local command made a network request")
	}
}
