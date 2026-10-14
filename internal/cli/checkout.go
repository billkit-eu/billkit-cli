package cli

// The `one-shots` command group. The file keeps its old name because
// api/tests/test_cli_one_shot_methods.py reads `oneShotMethods` out of
// cli/internal/cli/checkout.go by path, and the API route it drives is
// /v1/checkout/one_shot.

import (
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
//
// No directdebit: SEPA Direct Debit collects against a mandate and a one-off
// charge has none, so the API's enum does not list it and neither does this.
var oneShotMethods = []string{
	"creditcard",
	"ideal",
	"bancontact",
	"eps",
	"applepay",
	"paypal",
	// One-off only, and the only method here that settles in days rather
	// than seconds.
	"banktransfer",
}

// oneShotsCmd is the resource group for one-off payments, shaped like
// `refunds` and `events`: create, list, retrieve.
//
// It replaced `checkout one-shot` / `checkout retrieve` before 1.0. A
// `checkout` group whose only member was not a checkout session read as
// though one-shots were a kind of session, and its `retrieve` could only
// ever fetch one-shots. `checkout` is left free for checkout sessions, the
// way the Stripe CLI spells them (`checkout sessions create`).
func oneShotsCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "one-shots",
		Short: "Create and inspect one-off (one-shot) payments",
	}
	cmd.AddCommand(oneShotsCreateCmd(g), oneShotsListCmd(g), oneShotsRetrieveCmd(g))
	return cmd
}

func oneShotsCreateCmd(g *globals) *cobra.Command {
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
		Use:   "create",
		Short: "Create a single one-off charge (no subscription, no saved mandate)",
		Long: "Create a single one-off charge and print the payment. The customer pays\n" +
			"it on a hosted page or in the payment element; nothing is saved for later.\n\n" +
			"Hosted (the default) needs --payment-method and prints the Mollie hosted\n" +
			"`redirect_url` to send the customer to:\n\n" +
			"  billkit one-shots create \\\n" +
			"    --customer cus_123 --amount 1999 --payment-method ideal \\\n" +
			"    --success-url https://example.com/thanks\n\n" +
			"--ui-mode embedded takes no --payment-method: the customer picks it in the\n" +
			"BillKit payment element on your own page, and the payment is printed\n" +
			"with a short-lived `client_secret` for that element instead of a\n" +
			"redirect URL:\n\n" +
			"  billkit one-shots create --ui-mode embedded \\\n" +
			"    --customer cus_123 --amount 1999 \\\n" +
			"    --success-url https://example.com/thanks",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			meta, err := parseMetadata(metadata)
			if err != nil {
				return &UsageError{err}
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
				what:     fmt.Sprintf("charge %s to %s %s", formatMoney(amount, currency), customer, via),
				checkCmd: oneShotCheckCmd(customer),
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
	cmd.Flags().Int64Var(&amount, "amount", 0, "amount to charge, in the currency's minor units (cents for EUR: 1999 is EUR 19.99) (required)")
	cmd.Flags().StringVar(&currency, "currency", "EUR", "ISO-4217 currency code")
	cmd.Flags().StringVar(&method, "payment-method", "", "payment method: "+joinMethods()+" (required for --ui-mode hosted, refused for embedded)")
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
	_ = cmd.RegisterFlagCompletionFunc("payment-method", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
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
	// Every refusal here is a bad invocation (exit 2), not a failed call:
	// the server would answer 400 for each, and saying so here names the
	// flag to fix rather than an API field.
	if p.amount <= 0 {
		return nil, usageErrorf("--amount must be a positive whole number of minor units (cents for EUR), got %d", p.amount)
	}
	if p.uiMode != "" && !slices.Contains(uiModes, p.uiMode) {
		return nil, &UsageError{fmt.Errorf("--ui-mode %q is not one of %s", p.uiMode, strings.Join(uiModes, ", "))}
	}
	embedded := p.uiMode == "embedded"
	switch {
	case embedded && p.method != "":
		return nil, &UsageError{fmt.Errorf("--payment-method cannot be used with --ui-mode embedded: the customer picks the method in the payment element")}
	case !embedded && p.method == "":
		return nil, &UsageError{fmt.Errorf("--payment-method is required unless --ui-mode is embedded (one of %s)", joinMethods())}
	case !embedded && !validMethod(p.method):
		return nil, usageErrorf("--payment-method %q is not one of %s", p.method, joinMethods())
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
		body["payment_method"] = p.method
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
			return nil, usageErrorf("--tax-behavior %q is not one of %s", p.taxBehavior, strings.Join(taxBehaviors, ", "))
		}
		body["tax_behavior"] = p.taxBehavior
	}
	if len(p.metadata) > 0 {
		body["metadata"] = p.metadata
	}
	return body, nil
}

func oneShotsListCmd(g *globals) *cobra.Command {
	var (
		customer      string
		status        string
		limit         int
		startingAfter string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List one-shot payments, newest first",
		Long: "List one-shot payments, newest first.\n\n" +
			"  billkit one-shots list --customer cus_123\n" +
			"  billkit one-shots list --status paid --limit 50",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{}
			if err := setLimit(cmd, q, limit); err != nil {
				return err
			}
			if customer != "" {
				q.Set("customer_id", customer)
			}
			if status != "" {
				q.Set("status", status)
			}
			if startingAfter != "" {
				q.Set("starting_after", startingAfter)
			}
			return getAndPrint(cmd, g, withQuery("/v1/checkout/one_shot", q))
		},
	}
	cmd.Flags().StringVar(&customer, "customer", "", "only this customer's one-shot payments")
	cmd.Flags().StringVar(&status, "status", "", "only one-shot payments in this status")
	cmd.Flags().IntVar(&limit, "limit", 0, limitUsage("one-shot payments"))
	cmd.Flags().StringVar(&startingAfter, "starting-after", "", "cursor: return one-shot payments after this id")
	return cmd
}

func oneShotsRetrieveCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "retrieve <one-shot-id>",
		Short: "Fetch one one-shot payment by id",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return getAndPrint(cmd, g, "/v1/checkout/one_shot/"+url.PathEscape(args[0]))
		},
	}
}

// oneShotCheckCmd is the command that shows whether an ambiguous one-shot
// create went through: the customer's one-shot payments, newest first.
//
// It used to name `billkit events list --type one_shot_payment.created`, an
// event type the API has never emitted, so the one line printed at the moment
// a user most needs to know whether they were charged sent them to a filter
// the CLI itself rejects. It then named a raw `billkit api GET`, from before
// one-shots had a list command of their own.
func oneShotCheckCmd(customer string) string {
	return "billkit one-shots list --customer " + shellQuote(customer)
}

func validMethod(method string) bool {
	return slices.Contains(oneShotMethods, method)
}

func joinMethods() string {
	return strings.Join(oneShotMethods, ", ")
}
