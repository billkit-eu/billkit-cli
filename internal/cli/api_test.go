package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCoerce(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{"true", true},
		{"false", false},
		{"null", nil},
		{"42", int64(42)},
		{"-7", int64(-7)},
		{"0", int64(0)},
		{"12abc", "12abc"}, // partial int stays a string
		{"3.14", "3.14"},   // floats stay strings (BillKit deals in integer cents)
		{"hello", "hello"},
		{"", ""},
		// A number is only a number when the text is exactly what it prints
		// back as. Otherwise a zero-padded reference reaches the API as a
		// different value than the one that was typed, which is a corrupted
		// request the user cannot see in what they wrote.
		{"01234", "01234"},
		{"+5", "+5"},
		{"-0", "-0"},
		{"007", "007"},
	}
	for _, c := range cases {
		if got := coerce(c.in); got != c.want {
			t.Errorf("coerce(%q) = %v (%T), want %v (%T)", c.in, got, got, c.want, c.want)
		}
	}
}

// TestIsMoneyPath pins which routes `billkit api` treats as money commands,
// i.e. which ones get the live-mode confirmation instead of going straight
// out. Getting this wrong is silent in both directions: too narrow and a real
// charge needs no confirmation, too wide and the banner stops being read.
func TestIsMoneyPath(t *testing.T) {
	money := []string{
		"/v1/refunds",
		"/v1/refunds?limit=1",
		"/v1/checkout/one_shot",
		"/v1/checkout/sessions",
		// Charges the prorated difference against the saved payment method
		// inside the request (api/billkit/api/subscriptions.py).
		"/v1/subscriptions/sub_1/update",
		// Refunds automatically when the price's refund_on_cancel is `full`
		// or `prorated` (services/subscriptions/lifecycle.py,
		// _refund_on_cancel_best_effort). It was on the ungated list, which
		// meant `billkit api POST /v1/subscriptions/sub_1/cancel` could issue
		// a real refund in live mode with no confirmation at all.
		"/v1/subscriptions/sub_1/cancel",
		// Creates a sequenceType=first Mollie payment for a verification
		// amount (services/subscriptions/reauthorization.py).
		"/v1/subscriptions/sub_1/reauthorize_payment_method",
	}
	for _, p := range money {
		if !isMoneyPath(p) {
			t.Errorf("%s spends real money and must be confirmed in live mode", p)
		}
	}
	// These change billing state without taking a payment. A banner here
	// teaches people to click through the one that matters.
	for _, p := range []string{
		"/v1/customers",
		"/v1/subscriptions/sub_1/pause",
		"/v1/subscriptions/sub_1/resume",
		"/v1/subscriptions/sub_1/reactivate",
		"/v1/subscriptions/sub_1/preview_update",
		"/v1/products",
	} {
		if isMoneyPath(p) {
			t.Errorf("%s takes no payment and must not raise the money banner", p)
		}
	}
}

// TestAPIJSONBodySendsArraysAndNestedObjects is the gap --data could not
// cover. `scopes` on an API key, `enabled_events` on a webhook endpoint and
// `metadata` on almost everything are arrays and objects, and several of them
// are required, so `billkit api` could not reach routes it is documented as
// the escape hatch for.
func TestAPIJSONBodySendsArraysAndNestedObjects(t *testing.T) {
	rec := &recorder{}
	srv := tlsStub(t, rec.handler())

	const payload = `{"name":"ci","scopes":["events:read","refunds:write"],"metadata":{"team":"payments"}}`
	if _, err := runCLI(t, "api", "POST", "/v1/api_keys",
		"--json", payload,
		"--api-key", "bk_test_UNIT0001", "--base-url", srv.URL); err != nil {
		t.Fatal(err)
	}

	m := rec.mutations()
	if len(m) != 1 {
		t.Fatalf("want one request, got %+v", m)
	}
	var got struct {
		Name     string            `json:"name"`
		Scopes   []string          `json:"scopes"`
		Metadata map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(m[0].body), &got); err != nil {
		t.Fatalf("body was not sent as JSON: %v (%q)", err, m[0].body)
	}
	if got.Name != "ci" || len(got.Scopes) != 2 || got.Scopes[0] != "events:read" ||
		got.Metadata["team"] != "payments" {
		t.Fatalf("the body did not survive the trip: %q", m[0].body)
	}
	// Still a mutating call, so the safety rails apply.
	if m[0].idem == "" {
		t.Error("--json must not become the one keyless way to POST")
	}
}

// The body travels verbatim. Decoding into map[string]any and re-encoding
// turns every number into a float64, which silently rewrites an integer past
// 2^53 — and an amount in minor units is exactly that shape.
func TestAPIJSONBodyIsNotRoundTrippedThroughFloat64(t *testing.T) {
	rec := &recorder{}
	srv := tlsStub(t, rec.handler())

	const big = "9007199254740993" // 2^53 + 1
	if _, err := runCLI(t, "api", "POST", "/v1/anything",
		"--json", `{"amount_cents":`+big+`}`,
		"--api-key", "bk_test_UNIT0001", "--base-url", srv.URL); err != nil {
		t.Fatal(err)
	}
	m := rec.mutations()
	if len(m) != 1 || !strings.Contains(m[0].body, big) {
		t.Fatalf("the integer did not survive: %+v", m)
	}
}

func TestAPIJSONBodySources(t *testing.T) {
	rec := &recorder{}
	srv := tlsStub(t, rec.handler())
	auth := []string{"--api-key", "bk_test_UNIT0001", "--base-url", srv.URL}

	t.Run("from a file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "body.json")
		if err := os.WriteFile(path, []byte(`{"from":"file"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := runCLI(t, append([]string{"api", "POST", "/v1/x", "--json", "@" + path}, auth...)...); err != nil {
			t.Fatal(err)
		}
		if m := rec.mutations(); !strings.Contains(m[len(m)-1].body, `"from":"file"`) {
			t.Fatalf("file body not sent: %+v", m[len(m)-1])
		}
	})

	t.Run("from stdin", func(t *testing.T) {
		if _, err := runCLIWithStdin(t, strings.NewReader(`{"from":"stdin"}`),
			append([]string{"api", "POST", "/v1/x", "--json", "-"}, auth...)...); err != nil {
			t.Fatal(err)
		}
		if m := rec.mutations(); !strings.Contains(m[len(m)-1].body, `"from":"stdin"`) {
			t.Fatalf("stdin body not sent: %+v", m[len(m)-1])
		}
	})

	t.Run("bad input is refused locally", func(t *testing.T) {
		for _, bad := range []string{`{"a":`, `[1,2]`, `"just a string"`, `   `} {
			_, err := runCLI(t, append([]string{"api", "POST", "/v1/x", "--json", bad}, auth...)...)
			if err == nil {
				t.Errorf("--json %q should not have reached the wire", bad)
			}
		}
	})

	t.Run("data and json cannot be combined", func(t *testing.T) {
		if _, err := runCLI(t, append([]string{"api", "POST", "/v1/x",
			"--json", `{"a":1}`, "--data", "b=2"}, auth...)...); err == nil {
			t.Fatal("--data and --json describe the same thing two ways and must be exclusive")
		}
	})
}

// TestAPISendsNoBodyWhenNoneWasGiven. `var body map[string]any` left nil and
// passed as an `any` is a non-nil interface holding a nil map, so the client's
// `body != nil` check was true and every bodyless call — every `billkit api
// GET` — went out with a literal `null` payload and a JSON content type. Some
// proxies refuse a GET with a body outright.
func TestAPISendsNoBodyWhenNoneWasGiven(t *testing.T) {
	rec := &recorder{}
	srv := tlsStub(t, rec.handler())

	if _, err := runCLI(t, "api", "GET", "/v1/customers",
		"--api-key", "bk_test_UNIT0001", "--base-url", srv.URL); err != nil {
		t.Fatal(err)
	}
	all := rec.all()
	if len(all) != 1 {
		t.Fatalf("want one request, got %+v", all)
	}
	if all[0].body != "" {
		t.Fatalf("a call with no --data and no --json must send no body, got %q", all[0].body)
	}
}

// TestAPIRefusesABodyOnAReadMethod. `billkit api GET /v1/customers --data
// email=x` used to build the body and send it: net/http will put a payload on
// a GET, the API ignores it, and the user reads a full unfiltered list as
// though their filter had been applied. Refused before any credential is
// read, for the same reason `listen` checks its flags first.
func TestAPIRefusesABodyOnAReadMethod(t *testing.T) {
	rec := &recorder{}
	srv := tlsStub(t, rec.handler())

	for _, tc := range []struct{ method, flag, value string }{
		{"GET", "--data", "email=ada@example.test"},
		{"GET", "--json", `{"email":"ada@example.test"}`},
		{"HEAD", "--data", "email=ada@example.test"},
	} {
		t.Run(tc.method+tc.flag, func(t *testing.T) {
			_, err := runCLI(t, "api", tc.method, "/v1/customers", tc.flag, tc.value,
				"--api-key", "bk_test_UNIT0001", "--base-url", srv.URL)
			if err == nil {
				t.Fatalf("%s with %s was accepted", tc.method, tc.flag)
			}
			if !strings.Contains(err.Error(), tc.flag) {
				t.Errorf("the error must name the flag to drop, got %v", err)
			}
		})
	}
	if len(rec.all()) != 0 {
		t.Fatalf("a refused invocation must not reach the wire: %+v", rec.all())
	}
}

// TestAPITreatsOPTIONSAsARead. api.SafeMethod already classes OPTIONS as safe
// to replay, so routing it through runMutating gave one method two answers:
// keyless and unretried to the client, and an idempotency key plus a
// live-mode confirmation gate to the command.
func TestAPITreatsOPTIONSAsARead(t *testing.T) {
	rec := &recorder{}
	srv := tlsStub(t, rec.handler())

	if _, err := runCLI(t, "api", "OPTIONS", "/v1/customers",
		"--api-key", "bk_test_UNIT0001", "--base-url", srv.URL); err != nil {
		t.Fatal(err)
	}
	all := rec.all()
	if len(all) != 1 {
		t.Fatalf("want one request, got %+v", all)
	}
	if all[0].idem != "" {
		t.Errorf("OPTIONS is a read and must not carry an Idempotency-Key, got %q", all[0].idem)
	}
}
