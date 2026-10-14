package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/billkit-eu/billkit-cli/internal/api"
	"github.com/spf13/cobra"
)

func refundsCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "refunds",
		Short: "Create and inspect refunds",
	}
	cmd.AddCommand(refundsCreateCmd(g), refundsListCmd(g), refundsRetrieveCmd(g))
	return cmd
}

func refundsCreateCmd(g *globals) *cobra.Command {
	var (
		payment      string
		oneShot      string
		subscription string
		amount       int64
		reason       string
		idemKey      string
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Refund a payment, one-shot payment, or subscription's last charge",
		Long: "Create a refund against exactly one target. Omit --amount to refund the\n" +
			"full remaining balance, or pass it in the currency's minor units (cents\n" +
			"for EUR: 500 is EUR 5.00) for a partial refund.\n\n" +
			"  billkit refunds create --payment pay_123 --amount 500 --reason \"duplicate\"\n" +
			"  billkit refunds create --one-shot osp_123\n" +
			"  billkit refunds create --subscription sub_123",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			body, err := buildRefundBody(payment, oneShot, subscription, amount, cmd.Flags().Changed("amount"), reason)
			if err != nil {
				return err
			}
			out, err := runMutating(cmd, g, mutatingCall{
				method: "POST",
				path:   "/v1/refunds",
				body:   body,
				moves:  true,
				what:   describeRefund(body, ""),
				describe: func(ctx context.Context, c *api.Client) string {
					return describeRefund(body, refundCurrency(ctx, c, body))
				},
				checkCmd: "billkit refunds list",
				idemKey:  idemKey,
			})
			if err != nil {
				return err
			}
			fprintJSON(cmd.OutOrStdout(), g.color, out)
			return nil
		},
	}
	cmd.Flags().StringVar(&payment, "payment", "", "id of the payment to refund")
	cmd.Flags().StringVar(&oneShot, "one-shot", "", "id of the one-shot payment to refund")
	cmd.Flags().StringVar(&subscription, "subscription", "", "id of the subscription whose last charge to refund")
	cmd.Flags().Int64Var(&amount, "amount", 0, "amount to refund, in the currency's minor units (cents for EUR) (default: the full remaining balance)")
	cmd.Flags().StringVar(&reason, "reason", "", "free-text reason recorded on the refund")
	cmd.Flags().StringVar(&idemKey, "idempotency-key", "", "Idempotency-Key for safe retries")
	return cmd
}

// buildRefundBody assembles the POST /v1/refunds body, enforcing the API's
// "exactly one target" rule client-side for a friendlier error. Every
// refusal is a bad invocation (exit 2): nothing was sent.
func buildRefundBody(payment, oneShot, subscription string, amount int64, amountSet bool, reason string) (map[string]any, error) {
	targets := 0
	body := map[string]any{}
	if payment != "" {
		body["payment_id"] = payment
		targets++
	}
	if oneShot != "" {
		body["one_shot_payment_id"] = oneShot
		targets++
	}
	if subscription != "" {
		body["subscription_id"] = subscription
		targets++
	}
	if targets != 1 {
		return nil, usageErrorf("pass exactly one of --payment, --one-shot, or --subscription (got %d)", targets)
	}
	if amountSet {
		if amount <= 0 {
			return nil, usageErrorf("--amount must be a positive whole number of minor units (cents for EUR), got %d", amount)
		}
		body["amount_cents"] = amount
	}
	if reason != "" {
		body["reason"] = reason
	}
	return body, nil
}

// describeRefund renders the refund about to be created in one clause, for
// the pre-flight banner and the "this may already exist" warning. currency
// is the target's, when it is known; without it a partial amount is shown in
// minor units and says so, rather than being formatted with a guessed
// exponent.
func describeRefund(body map[string]any, currency string) string {
	target := "the target"
	for _, key := range []string{"payment_id", "one_shot_payment_id", "subscription_id"} {
		if id, ok := body[key].(string); ok {
			target = id
			break
		}
	}
	if _, isSub := body["subscription_id"]; isSub {
		target += "'s latest charge"
	}
	if minor, ok := body["amount_cents"].(int64); ok {
		return fmt.Sprintf("refund %s of %s", formatMoney(minor, currency), target)
	}
	return fmt.Sprintf("refund the full remaining balance of %s", target)
}

// refundCurrency looks up the currency of a partial refund's target, so the
// banner can say `EUR 5.00` instead of `500`. Best effort and quick: it is a
// read the user's key may not be scoped for, and any failure leaves the
// amount in minor units, which is still correct, just less friendly. A
// subscription target is not looked up, since what it refunds is "the latest
// charge" and naming that charge's currency would take a second call.
func refundCurrency(ctx context.Context, c *api.Client, body map[string]any) string {
	if _, partial := body["amount_cents"]; !partial {
		return ""
	}
	var path string
	switch {
	case body["payment_id"] != nil:
		path = "/v1/payments/" + url.PathEscape(fmt.Sprint(body["payment_id"]))
	case body["one_shot_payment_id"] != nil:
		path = "/v1/checkout/one_shot/" + url.PathEscape(fmt.Sprint(body["one_shot_payment_id"]))
	default:
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	raw, err := c.Do(ctx, "GET", path, nil)
	if err != nil {
		return ""
	}
	var obj struct {
		Currency string `json:"currency"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	return obj.Currency
}

func refundsListCmd(g *globals) *cobra.Command {
	var (
		limit         int
		startingAfter string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List recent refunds",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{}
			if err := setLimit(cmd, q, limit); err != nil {
				return err
			}
			if startingAfter != "" {
				q.Set("starting_after", startingAfter)
			}
			return getAndPrint(cmd, g, withQuery("/v1/refunds", q))
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, limitUsage("refunds"))
	cmd.Flags().StringVar(&startingAfter, "starting-after", "", "cursor: return refunds after this id")
	return cmd
}

func refundsRetrieveCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "retrieve <refund-id>",
		Short: "Fetch one refund by id",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return getAndPrint(cmd, g, "/v1/refunds/"+url.PathEscape(args[0]))
		},
	}
}
