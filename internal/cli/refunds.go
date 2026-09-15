package cli

import (
	"context"
	"fmt"
	"net/url"

	"github.com/spf13/cobra"
)

func refundsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "refunds",
		Short: "Create and inspect refunds",
	}
	cmd.AddCommand(refundsCreateCmd(), refundsListCmd(), refundsRetrieveCmd())
	return cmd
}

func refundsCreateCmd() *cobra.Command {
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
			"full remaining balance, or pass it (in cents) for a partial refund.\n\n" +
			"  billkit refunds create --payment pay_123 --amount 500 --reason \"duplicate\"\n" +
			"  billkit refunds create --one-shot osp_123\n" +
			"  billkit refunds create --subscription sub_123",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			body, err := buildRefundBody(payment, oneShot, subscription, amount, cmd.Flags().Changed("amount"), reason)
			if err != nil {
				return err
			}
			out, err := runMutating(cmd, mutatingCall{
				method:   "POST",
				path:     "/v1/refunds",
				body:     body,
				moves:    true,
				what:     describeRefund(body),
				checkCmd: "billkit refunds list",
				idemKey:  idemKey,
			})
			if err != nil {
				return err
			}
			printJSON(out)
			return nil
		},
	}
	cmd.Flags().StringVar(&payment, "payment", "", "id of the payment to refund")
	cmd.Flags().StringVar(&oneShot, "one-shot", "", "id of the one-shot payment to refund")
	cmd.Flags().StringVar(&subscription, "subscription", "", "id of the subscription whose last charge to refund")
	cmd.Flags().Int64Var(&amount, "amount", 0, "amount to refund in cents (default: full remaining balance)")
	cmd.Flags().StringVar(&reason, "reason", "", "free-text reason recorded on the refund")
	cmd.Flags().StringVar(&idemKey, "idempotency-key", "", "Idempotency-Key for safe retries")
	return cmd
}

// buildRefundBody assembles the POST /v1/refunds body, enforcing the API's
// "exactly one target" rule client-side for a friendlier error.
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
		return nil, fmt.Errorf("pass exactly one of --payment, --one-shot, or --subscription")
	}
	if amountSet {
		if amount <= 0 {
			return nil, fmt.Errorf("--amount must be a positive number of cents")
		}
		body["amount_cents"] = amount
	}
	if reason != "" {
		body["reason"] = reason
	}
	return body, nil
}

// describeRefund renders the refund about to be created in one clause, for
// the pre-flight banner and the "this may already exist" warning.
func describeRefund(body map[string]any) string {
	target := "the target"
	for _, key := range []string{"payment_id", "one_shot_payment_id", "subscription_id"} {
		if id, ok := body[key].(string); ok {
			target = id
			break
		}
	}
	if cents, ok := body["amount_cents"].(int64); ok {
		return fmt.Sprintf("refund %d cents of %s", cents, target)
	}
	return fmt.Sprintf("refund the full remaining balance of %s", target)
}

func refundsListCmd() *cobra.Command {
	var (
		limit         int
		startingAfter string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List recent refunds",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			q := url.Values{}
			if limit > 0 {
				q.Set("limit", fmt.Sprintf("%d", limit))
			}
			if startingAfter != "" {
				q.Set("starting_after", startingAfter)
			}
			path := "/v1/refunds"
			if encoded := q.Encode(); encoded != "" {
				path += "?" + encoded
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), readTimeout)
			defer cancel()
			out, err := c.Do(ctx, "GET", path, nil)
			if err != nil {
				return err
			}
			printJSON(out)
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "max refunds to return (1–100)")
	cmd.Flags().StringVar(&startingAfter, "starting-after", "", "cursor: return refunds after this id")
	return cmd
}

func refundsRetrieveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "retrieve <refund-id>",
		Short: "Fetch one refund by id",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), readTimeout)
			defer cancel()
			out, err := c.Do(ctx, "GET", "/v1/refunds/"+url.PathEscape(args[0]), nil)
			if err != nil {
				return err
			}
			printJSON(out)
			return nil
		},
	}
}
