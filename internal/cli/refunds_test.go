package cli

import "testing"

func TestBuildRefundBody(t *testing.T) {
	t.Run("payment full refund", func(t *testing.T) {
		body, err := buildRefundBody("pay_1", "", "", 0, false, "")
		if err != nil {
			t.Fatal(err)
		}
		if body["payment_id"] != "pay_1" {
			t.Fatalf("body = %v", body)
		}
		if _, ok := body["amount_cents"]; ok {
			t.Fatal("amount should be omitted for a full refund")
		}
	})

	t.Run("one-shot partial with reason", func(t *testing.T) {
		body, err := buildRefundBody("", "osp_1", "", 500, true, "duplicate")
		if err != nil {
			t.Fatal(err)
		}
		if body["one_shot_payment_id"] != "osp_1" || body["amount_cents"] != int64(500) || body["reason"] != "duplicate" {
			t.Fatalf("body = %v", body)
		}
	})

	t.Run("no target is rejected", func(t *testing.T) {
		if _, err := buildRefundBody("", "", "", 0, false, ""); err == nil {
			t.Fatal("expected error when no target given")
		}
	})

	t.Run("two targets are rejected", func(t *testing.T) {
		if _, err := buildRefundBody("pay_1", "osp_1", "", 0, false, ""); err == nil {
			t.Fatal("expected error when two targets given")
		}
	})

	t.Run("non-positive amount is rejected", func(t *testing.T) {
		if _, err := buildRefundBody("pay_1", "", "", 0, true, ""); err == nil {
			t.Fatal("expected error for zero amount when set")
		}
	})
}
