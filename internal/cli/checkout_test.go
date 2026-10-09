package cli

import "testing"

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
		if _, ok := body["method"]; ok {
			t.Fatalf("method must be omitted for embedded, body = %v", body)
		}
	})

	t.Run("embedded with a method is a usage error", func(t *testing.T) {
		_, err := buildOneShotBody(oneShotParams{
			customer: "cus_1", amount: 1, currency: "EUR", uiMode: "embedded", method: "ideal",
			successURL: "https://x.test/ok",
		})
		if err == nil {
			t.Fatal("expected an error for --method with --ui-mode embedded")
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
		if explicit["ui_mode"] != "hosted" || explicit["method"] != "ideal" {
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
			body["currency"] != "EUR" || body["method"] != "ideal" || body["success_url"] != "https://x.test/ok" {
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
		if body["method"] != "paypal" {
			t.Fatalf("method = %v", body["method"])
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
