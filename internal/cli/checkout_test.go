package cli

import "testing"

func TestBuildOneShotBody(t *testing.T) {
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
		for _, k := range []string{"cancel_url", "description", "refund_window_days", "metadata"} {
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
