package cli

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

// oneShotMethods mirrors the API's OneShotMethod enum (schemas/one_shot_payment.py).
// giropay was dropped when the scheme shut down at the end of 2024.
var oneShotMethods = []string{"creditcard", "directdebit", "ideal", "bancontact", "eps"}

func checkoutCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "checkout",
		Short: "Create and inspect one-off (one-shot) payments",
	}
	cmd.AddCommand(checkoutOneShotCmd(), checkoutRetrieveCmd())
	return cmd
}

func checkoutOneShotCmd() *cobra.Command {
	var (
		customer         string
		amount           int64
		currency         string
		method           string
		successURL       string
		cancelURL        string
		description      string
		refundWindowDays int
		metadata         []string
		idemKey          string
	)
	cmd := &cobra.Command{
		Use:   "one-shot",
		Short: "Create a single off-session charge (no subscription, no saved mandate)",
		Long: "Create a one-off `sequenceType=oneoff` charge and print the payment,\n" +
			"including the Mollie hosted `redirect_url` to send the customer to.\n\n" +
			"  billkit checkout one-shot \\\n" +
			"    --customer cus_123 --amount 1999 --method ideal \\\n" +
			"    --success-url https://example.com/thanks",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			meta, err := parseMetadata(metadata)
			if err != nil {
				return err
			}
			body, err := buildOneShotBody(oneShotParams{
				customer:         customer,
				amount:           amount,
				currency:         currency,
				method:           method,
				successURL:       successURL,
				cancelURL:        cancelURL,
				description:      description,
				refundWindowDays: refundWindowDays,
				refundWindowSet:  cmd.Flags().Changed("refund-window-days"),
				metadata:         meta,
			})
			if err != nil {
				return err
			}
			out, err := runMutating(cmd, mutatingCall{
				method:   "POST",
				path:     "/v1/checkout/one_shot",
				body:     body,
				moves:    true,
				what:     fmt.Sprintf("charge %d %s to %s via %s", amount, currency, customer, method),
				checkCmd: "billkit events list --type one_shot_payment.created",
				idemKey:  idemKey,
			})
			if err != nil {
				return err
			}
			printJSON(out)
			return nil
		},
	}
	cmd.Flags().StringVar(&customer, "customer", "", "customer id to charge (required)")
	cmd.Flags().Int64Var(&amount, "amount", 0, "amount to charge in cents (required)")
	cmd.Flags().StringVar(&currency, "currency", "EUR", "ISO-4217 currency code")
	cmd.Flags().StringVar(&method, "method", "", "payment method: "+joinMethods()+" (required)")
	cmd.Flags().StringVar(&successURL, "success-url", "", "URL to redirect to after a successful payment (required)")
	cmd.Flags().StringVar(&cancelURL, "cancel-url", "", "URL to redirect to if the customer cancels")
	cmd.Flags().StringVar(&description, "description", "", "statement/description shown to the customer")
	cmd.Flags().IntVar(&refundWindowDays, "refund-window-days", 0, "days the charge stays refundable (0 disables refunds)")
	cmd.Flags().StringArrayVar(&metadata, "metadata", nil, "metadata as key=value (repeatable)")
	cmd.Flags().StringVar(&idemKey, "idempotency-key", "", "Idempotency-Key for safe retries")
	_ = cmd.MarkFlagRequired("customer")
	_ = cmd.MarkFlagRequired("amount")
	_ = cmd.MarkFlagRequired("method")
	_ = cmd.MarkFlagRequired("success-url")
	_ = cmd.RegisterFlagCompletionFunc("method", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return oneShotMethods, cobra.ShellCompDirectiveNoFileComp
	})
	return cmd
}

type oneShotParams struct {
	customer         string
	amount           int64
	currency         string
	method           string
	successURL       string
	cancelURL        string
	description      string
	refundWindowDays int
	refundWindowSet  bool
	metadata         map[string]string
}

func buildOneShotBody(p oneShotParams) (map[string]any, error) {
	if p.amount <= 0 {
		return nil, fmt.Errorf("--amount must be a positive number of cents")
	}
	if !validMethod(p.method) {
		return nil, fmt.Errorf("--method %q is not one of %s", p.method, joinMethods())
	}
	body := map[string]any{
		"customer_id":  p.customer,
		"amount_cents": p.amount,
		"currency":     p.currency,
		"method":       p.method,
		"success_url":  p.successURL,
	}
	if p.cancelURL != "" {
		body["cancel_url"] = p.cancelURL
	}
	if p.description != "" {
		body["description"] = p.description
	}
	if p.refundWindowSet {
		body["refund_window_days"] = p.refundWindowDays
	}
	if len(p.metadata) > 0 {
		body["metadata"] = p.metadata
	}
	return body, nil
}

func checkoutRetrieveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "retrieve <one-shot-id>",
		Short: "Fetch one one-shot payment by id",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), readTimeout)
			defer cancel()
			out, err := c.Do(ctx, "GET", "/v1/checkout/one_shot/"+url.PathEscape(args[0]), nil)
			if err != nil {
				return err
			}
			printJSON(out)
			return nil
		},
	}
}

func validMethod(method string) bool {
	return slices.Contains(oneShotMethods, method)
}

func joinMethods() string {
	return strings.Join(oneShotMethods, ", ")
}
