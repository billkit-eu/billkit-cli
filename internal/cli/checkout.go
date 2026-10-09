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
//
// This list *rejects* rather than merely documents, so it has to be kept in
// step with the API: a method the server accepts but this slice omits is one
// the CLI refuses on the client side, with an error that blames the user for
// a value that is in fact valid. applepay was added in the 2026-09 wallet
// work and is here for exactly that reason; banktransfer had been missing
// since the enum gained it, which is what
// api/tests/test_cli_one_shot_methods.py now makes impossible in either
// direction, the way test_cli_event_constants.py does for event types.
var oneShotMethods = []string{
	"creditcard",
	"directdebit",
	"ideal",
	"bancontact",
	"eps",
	"applepay",
	"paypal",
	// One-off only, and the only method here that settles in days rather
	// than seconds.
	"banktransfer",
}

func checkoutCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "checkout",
		Short: "Create and inspect one-off (one-shot) payments",
	}
	cmd.AddCommand(checkoutOneShotCmd(g), checkoutRetrieveCmd(g))
	return cmd
}

func checkoutOneShotCmd(g *globals) *cobra.Command {
	var (
		customer         string
		amount           int64
		currency         string
		method           string
		uiMode           string
		successURL       string
		cancelURL        string
		description      string
		refundWindowDays int
		taxBehavior      string
		metadata         []string
		idemKey          string
	)
	cmd := &cobra.Command{
		Use:   "one-shot",
		Short: "Create a single off-session charge (no subscription, no saved mandate)",
		Long: "Create a one-off `sequenceType=oneoff` charge and print the payment.\n\n" +
			"Hosted (the default) needs --method and prints the Mollie hosted\n" +
			"`redirect_url` to send the customer to:\n\n" +
			"  billkit checkout one-shot \\\n" +
			"    --customer cus_123 --amount 1999 --method ideal \\\n" +
			"    --success-url https://example.com/thanks\n\n" +
			"--ui-mode embedded takes no --method: the customer picks it in the\n" +
			"BillKit payment element on your own page, and the payment is printed\n" +
			"with a short-lived `client_secret` for that element instead of a\n" +
			"redirect URL:\n\n" +
			"  billkit checkout one-shot --ui-mode embedded \\\n" +
			"    --customer cus_123 --amount 1999 \\\n" +
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
				uiMode:           uiMode,
				successURL:       successURL,
				cancelURL:        cancelURL,
				description:      description,
				refundWindowDays: refundWindowDays,
				refundWindowSet:  cmd.Flags().Changed("refund-window-days"),
				taxBehavior:      taxBehavior,
				metadata:         meta,
			})
			if err != nil {
				return err
			}
			// What the confirmation line names. An embedded one-shot has no
			// method yet, and "via " followed by nothing reads like a bug.
			via := "via " + method
			if uiMode == "embedded" {
				via = "in the embedded payment element"
			}
			out, err := runMutating(cmd, g, mutatingCall{
				method:   "POST",
				path:     "/v1/checkout/one_shot",
				body:     body,
				moves:    true,
				what:     fmt.Sprintf("charge %d %s to %s %s", amount, currency, customer, via),
				checkCmd: "billkit events list --type one_shot_payment.created",
				idemKey:  idemKey,
			})
			if err != nil {
				return err
			}
			fprintJSON(cmd.OutOrStdout(), g.color, out)
			return nil
		},
	}
	cmd.Flags().StringVar(&customer, "customer", "", "customer id to charge (required)")
	cmd.Flags().Int64Var(&amount, "amount", 0, "amount to charge in cents (required)")
	cmd.Flags().StringVar(&currency, "currency", "EUR", "ISO-4217 currency code")
	cmd.Flags().StringVar(&method, "method", "", "payment method: "+joinMethods()+" (required for --ui-mode hosted, refused for embedded)")
	cmd.Flags().StringVar(&uiMode, "ui-mode", "", "hosted (redirect to Mollie, the default) or embedded (returns a client_secret for the payment element)")
	cmd.Flags().StringVar(&successURL, "success-url", "", "URL to redirect to after a successful payment (required)")
	cmd.Flags().StringVar(&cancelURL, "cancel-url", "", "URL to redirect to if the customer cancels")
	cmd.Flags().StringVar(&description, "description", "", "statement/description shown to the customer")
	cmd.Flags().IntVar(&refundWindowDays, "refund-window-days", 0, "days the charge stays refundable (0 disables refunds)")
	// A one-shot has no Price to carry the intent, so this is the one surface
	// with a per-request flag. Omitted means "inherit the country default",
	// which is not the same as either value, so it is a tri-state string and
	// not a bool.
	cmd.Flags().StringVar(&taxBehavior, "tax-behavior", "", "whether --amount is quoted gross (inclusive) or net (exclusive); default: the tenant's country default")
	cmd.Flags().StringArrayVar(&metadata, "metadata", nil, "metadata as key=value (repeatable)")
	cmd.Flags().StringVar(&idemKey, "idempotency-key", "", "Idempotency-Key for safe retries")
	_ = cmd.MarkFlagRequired("customer")
	_ = cmd.MarkFlagRequired("amount")
	_ = cmd.MarkFlagRequired("success-url")
	_ = cmd.RegisterFlagCompletionFunc("method", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return oneShotMethods, cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.RegisterFlagCompletionFunc("ui-mode", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return uiModes, cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.RegisterFlagCompletionFunc("tax-behavior", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return taxBehaviors, cobra.ShellCompDirectiveNoFileComp
	})
	return cmd
}

type oneShotParams struct {
	customer         string
	amount           int64
	currency         string
	method           string
	uiMode           string
	successURL       string
	cancelURL        string
	description      string
	refundWindowDays int
	refundWindowSet  bool
	taxBehavior      string
	metadata         map[string]string
}

// taxBehaviors mirrors the API's `tax_behavior` literal on
// OneShotPaymentCreate. "inclusive" backs VAT out of the amount sent;
// "exclusive" adds it on top, so the payer is charged more than the number on
// the command line and the response's amount_cents says so.
var taxBehaviors = []string{"inclusive", "exclusive"}

// uiModes mirrors the API's `ui_mode` literal on OneShotPaymentCreate.
// "hosted" redirects the customer to Mollie and needs a method up front;
// "embedded" returns a client_secret for the BillKit payment element, where
// the customer picks the method, so it must be sent without one.
var uiModes = []string{"hosted", "embedded"}

func buildOneShotBody(p oneShotParams) (map[string]any, error) {
	if p.amount <= 0 {
		return nil, fmt.Errorf("--amount must be a positive number of cents")
	}
	// Both refusals below are a bad invocation (exit 2), not a failed call:
	// the server would answer 422 for either, and saying so here names the
	// flag to fix rather than an API field.
	if p.uiMode != "" && !slices.Contains(uiModes, p.uiMode) {
		return nil, &UsageError{fmt.Errorf("--ui-mode %q is not one of %s", p.uiMode, strings.Join(uiModes, ", "))}
	}
	embedded := p.uiMode == "embedded"
	switch {
	case embedded && p.method != "":
		return nil, &UsageError{fmt.Errorf("--method cannot be used with --ui-mode embedded: the customer picks the method in the payment element")}
	case !embedded && p.method == "":
		return nil, &UsageError{fmt.Errorf("--method is required unless --ui-mode is embedded (one of %s)", joinMethods())}
	case !embedded && !validMethod(p.method):
		return nil, fmt.Errorf("--method %q is not one of %s", p.method, joinMethods())
	}
	body := map[string]any{
		"customer_id":  p.customer,
		"amount_cents": p.amount,
		"currency":     p.currency,
		"success_url":  p.successURL,
	}
	if embedded {
		body["ui_mode"] = p.uiMode
	} else {
		body["method"] = p.method
		// An explicit --ui-mode hosted is sent, so what was typed is what the
		// server sees; an omitted one is not, which keeps the body of every
		// existing invocation byte-for-byte unchanged.
		if p.uiMode != "" {
			body["ui_mode"] = p.uiMode
		}
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
	if p.taxBehavior != "" {
		if !slices.Contains(taxBehaviors, p.taxBehavior) {
			return nil, fmt.Errorf("--tax-behavior %q is not one of %s", p.taxBehavior, strings.Join(taxBehaviors, ", "))
		}
		body["tax_behavior"] = p.taxBehavior
	}
	if len(p.metadata) > 0 {
		body["metadata"] = p.metadata
	}
	return body, nil
}

func checkoutRetrieveCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "retrieve <one-shot-id>",
		Short: "Fetch one one-shot payment by id",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client(cmd, g)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), readTimeout)
			defer cancel()
			out, err := c.Do(ctx, "GET", "/v1/checkout/one_shot/"+url.PathEscape(args[0]), nil)
			if err != nil {
				return err
			}
			fprintJSON(cmd.OutOrStdout(), g.color, out)
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
