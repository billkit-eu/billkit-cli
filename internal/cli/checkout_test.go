package cli

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestBuildOneShotBody(t *testing.T) {
	t.Run("embedded sends ui_mode and no method", func(t *testing.T) {
		body, err := buildOneShotBody(oneShotParams{
			customer: "cus_1", amount: 1999, currency: "EUR", uiMode: "embedded",
			successURL: "https://x.test/ok",
		})
		if err != nil {
			t.Fatal(err)
		}
		if body["ui_mode"] != "embedded" {
			t.Fatalf("ui_mode = %v, body = %v", body["ui_mode"], body)
		}
		if _, ok := body["payment_method"]; ok {
			t.Fatalf("method must be omitted for embedded, body = %v", body)
		}
	})

	t.Run("embedded with a method is a usage error", func(t *testing.T) {
		_, err := buildOneShotBody(oneShotParams{
			customer: "cus_1", amount: 1, currency: "EUR", uiMode: "embedded", method: "ideal",
			successURL: "https://x.test/ok",
		})
		if err == nil {
			t.Fatal("expected an error for --payment-method with --ui-mode embedded")
		}
		if ExitCode(err) != ExitUsage {
			t.Fatalf("ExitCode = %d, want %d (err: %v)", ExitCode(err), ExitUsage, err)
		}
	})

	t.Run("hosted without a method is a usage error", func(t *testing.T) {
		for _, mode := range []string{"", "hosted"} {
			_, err := buildOneShotBody(oneShotParams{
				customer: "cus_1", amount: 1, currency: "EUR", uiMode: mode, successURL: "https://x.test/ok",
			})
			if err == nil || ExitCode(err) != ExitUsage {
				t.Fatalf("ui-mode %q: want a usage error, got %v", mode, err)
			}
		}
	})

	t.Run("unknown ui mode is a usage error", func(t *testing.T) {
		_, err := buildOneShotBody(oneShotParams{
			customer: "cus_1", amount: 1, currency: "EUR", uiMode: "iframe", successURL: "https://x.test/ok",
		})
		if err == nil || ExitCode(err) != ExitUsage {
			t.Fatalf("want a usage error, got %v", err)
		}
	})

	t.Run("explicit hosted is sent, omitted is not", func(t *testing.T) {
		explicit, err := buildOneShotBody(oneShotParams{
			customer: "cus_1", amount: 1, currency: "EUR", uiMode: "hosted", method: "ideal",
			successURL: "https://x.test/ok",
		})
		if err != nil {
			t.Fatal(err)
		}
		if explicit["ui_mode"] != "hosted" || explicit["payment_method"] != "ideal" {
			t.Fatalf("body = %v", explicit)
		}
		omitted, err := buildOneShotBody(oneShotParams{
			customer: "cus_1", amount: 1, currency: "EUR", method: "ideal", successURL: "https://x.test/ok",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := omitted["ui_mode"]; ok {
			t.Fatalf("ui_mode must be omitted when not given, body = %v", omitted)
		}
	})

	t.Run("minimal required fields", func(t *testing.T) {
		body, err := buildOneShotBody(oneShotParams{
			customer:   "cus_1",
			amount:     1999,
			currency:   "EUR",
			method:     "ideal",
			successURL: "https://x.test/ok",
		})
		if err != nil {
			t.Fatal(err)
		}
		if body["customer_id"] != "cus_1" || body["amount_cents"] != int64(1999) ||
			body["currency"] != "EUR" || body["payment_method"] != "ideal" || body["success_url"] != "https://x.test/ok" {
			t.Fatalf("body = %v", body)
		}
		// Optional fields are omitted when unset.
		for _, k := range []string{"cancel_url", "description", "refund_window_days", "metadata", "ui_mode"} {
			if _, ok := body[k]; ok {
				t.Fatalf("expected %q omitted, body = %v", k, body)
			}
		}
	})

	t.Run("zero refund window survives when set", func(t *testing.T) {
		body, err := buildOneShotBody(oneShotParams{
			customer: "cus_1", amount: 1, currency: "EUR", method: "creditcard",
			successURL: "https://x.test/ok", refundWindowDays: 0, refundWindowSet: true,
			metadata: map[string]string{"k": "v"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if body["refund_window_days"] != 0 {
			t.Fatalf("refund_window_days should be 0, body = %v", body)
		}
		if m, ok := body["metadata"].(map[string]string); !ok || m["k"] != "v" {
			t.Fatalf("metadata = %v", body["metadata"])
		}
	})

	t.Run("bad method rejected", func(t *testing.T) {
		// Was "paypal" until the server promoted it to a real one-shot and
		// recurring method. "giropay" is the durable example: Paydirekt shut
		// the scheme down on 2024-12-31 and it is never coming back, so this
		// case cannot be invalidated by the vocabulary growing again.
		if _, err := buildOneShotBody(oneShotParams{
			customer: "cus_1", amount: 1, currency: "EUR", method: "giropay", successURL: "https://x.test/ok",
		}); err == nil {
			t.Fatal("expected error for unsupported method")
		}
	})

	t.Run("paypal accepted", func(t *testing.T) {
		body, err := buildOneShotBody(oneShotParams{
			customer: "cus_1", amount: 1, currency: "EUR", method: "paypal", successURL: "https://x.test/ok",
		})
		if err != nil {
			t.Fatalf("paypal should be accepted: %v", err)
		}
		if body["payment_method"] != "paypal" {
			t.Fatalf("method = %v", body["payment_method"])
		}
	})

	// Every method in the slice must actually build a body. The slice is the
	// client-side gate, so an entry that is in it but rejected by validMethod
	// would be a method nobody can use and nothing would say why.
	t.Run("every offered method is accepted", func(t *testing.T) {
		for _, m := range oneShotMethods {
			if _, err := buildOneShotBody(oneShotParams{
				customer: "cus_1", amount: 1, currency: "EUR", method: m, successURL: "https://x.test/ok",
			}); err != nil {
				t.Errorf("method %q is offered but refused: %v", m, err)
			}
		}
	})

	// directdebit is not in the API's one-shot enum (SEPA needs a mandate),
	// so it is refused locally as an unknown method and never offered.
	t.Run("directdebit is refused locally and never offered", func(t *testing.T) {
		if slices.Contains(oneShotMethods, "directdebit") {
			t.Fatal("directdebit is offered, but a one-off charge cannot use it")
		}
		_, err := buildOneShotBody(oneShotParams{
			customer: "cus_1", amount: 1, currency: "EUR", method: "directdebit", successURL: "https://x.test/ok",
		})
		var usage *UsageError
		if !errors.As(err, &usage) || !strings.Contains(err.Error(), `--payment-method "directdebit" is not one of`) {
			t.Fatalf("want a usage error refusing directdebit, got %v", err)
		}
		help := oneShotsCreateCmd(&globals{}).Flags().Lookup("payment-method").Usage
		if strings.Contains(help, "directdebit") {
			t.Fatalf("--payment-method help still lists directdebit: %q", help)
		}
		if strings.Contains(oneShotsCreateCmd(&globals{}).Short, "off-session") {
			t.Fatal("a one-shot is paid by the customer on a hosted page or in the element, not off-session")
		}
	})

	t.Run("tax behavior is passed through, and only if valid", func(t *testing.T) {
		for _, want := range taxBehaviors {
			body, err := buildOneShotBody(oneShotParams{
				customer: "cus_1", amount: 1, currency: "EUR", method: "ideal",
				successURL: "https://x.test/ok", taxBehavior: want,
			})
			if err != nil {
				t.Fatal(err)
			}
			if body["tax_behavior"] != want {
				t.Fatalf("tax_behavior = %v, want %q", body["tax_behavior"], want)
			}
		}
		// Unset is a third state, not a default: it inherits the tenant's
		// country default, which is neither "inclusive" nor "exclusive".
		body, err := buildOneShotBody(oneShotParams{
			customer: "cus_1", amount: 1, currency: "EUR", method: "ideal", successURL: "https://x.test/ok",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := body["tax_behavior"]; ok {
			t.Fatalf("an unset --tax-behavior must not be sent, body = %v", body)
		}
		if _, err := buildOneShotBody(oneShotParams{
			customer: "cus_1", amount: 1, currency: "EUR", method: "ideal",
			successURL: "https://x.test/ok", taxBehavior: "gross",
		}); err == nil {
			t.Fatal("expected error for an unsupported tax behavior")
		}
	})

	t.Run("non-positive amount rejected", func(t *testing.T) {
		if _, err := buildOneShotBody(oneShotParams{
			customer: "cus_1", amount: 0, currency: "EUR", method: "ideal", successURL: "https://x.test/ok",
		}); err == nil {
			t.Fatal("expected error for zero amount")
		}
	})
}

// TestOneShotCheckHintIsARunnableCommand. After an ambiguous one-shot create,
// the CLI prints a command to check whether the charge went through. It used
// to print `billkit events list --type one_shot_payment.created`, an event
// type the API does not have. This takes the printed line, parses it the way
// a shell would, runs it through the real command tree, and checks it lists
// that customer's one-shot payments.
func TestOneShotCheckHintIsARunnableCommand(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	const customer = "cus_1 &x=y"
	var mu sync.Mutex
	var gets []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		mu.Lock()
		gets = append(gets, r)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv.Close()
	t.Setenv("BILLKIT_API_KEY", "bk_test_x")
	t.Setenv("BILLKIT_BASE_URL", srv.URL)

	stderr, err := runCLI(t, "one-shots", "create", "--customer", customer, "--amount", "100",
		"--payment-method", "ideal", "--success-url", "https://x.test/ok")
	if err == nil {
		t.Fatal("a 503 must fail the create")
	}
	_, hint, ok := strings.Cut(stderr, "Check first:")
	if !ok {
		t.Fatalf("no check hint printed; stderr = %q", stderr)
	}
	hint, _, _ = strings.Cut(strings.TrimSpace(hint), "\n")
	words := shellWords(t, hint)
	if len(words) < 2 || words[0] != "billkit" {
		t.Fatalf("the hint is not a billkit command: %q", hint)
	}
	// The first-class list command, now that one-shots have one, rather
	// than a raw `billkit api GET` a user has to read a query string in.
	if words[1] != "one-shots" || words[2] != "list" {
		t.Errorf("the hint should be `billkit one-shots list`, got %q", hint)
	}

	root := rootCmd()
	root.SetOut(io.Discard)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader(""))
	root.SetArgs(words[1:])
	if err := root.Execute(); err != nil {
		t.Fatalf("the printed hint %q does not run: %v", hint, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gets) != 1 {
		t.Fatalf("the hint made %d GET requests, want 1", len(gets))
	}
	if got := gets[0].URL.Path; got != "/v1/checkout/one_shot" {
		t.Errorf("the hint reads %s, want /v1/checkout/one_shot", got)
	}
	if got := gets[0].URL.Query().Get("customer_id"); got != customer {
		t.Errorf("customer_id = %q, want %q", got, customer)
	}
}

// shellWords splits a command line the way a POSIX shell does for the only
// quoting shellQuote produces: bare words and single-quoted runs.
func shellWords(t *testing.T, line string) []string {
	t.Helper()
	var words []string
	var cur strings.Builder
	inWord, quoted := false, false
	for _, r := range line {
		switch {
		case quoted && r == '\'':
			quoted = false
		case quoted:
			cur.WriteRune(r)
		case r == '\'':
			quoted, inWord = true, true
		case r == ' ' || r == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			if strings.ContainsRune("\"\\$`|&;<>()*?[]{}#~!", r) {
				t.Fatalf("unquoted shell metacharacter %q in %q", r, line)
			}
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quoted {
		t.Fatalf("unterminated quote in %q", line)
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}
