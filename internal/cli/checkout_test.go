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
		if _, err := buildOneShotBody(oneShotParams{
			customer: "cus_1", amount: 1, currency: "EUR", method: "paypal", successURL: "https://x.test/ok",
		}); err == nil {
			t.Fatal("expected error for unsupported method")
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
