package cli

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/billkit-eu/billkit-cli/internal/api"
	"github.com/billkit-eu/billkit-cli/internal/config"
	"github.com/spf13/cobra"
)

// triggers maps an event type to the real test-mode API call(s) that emit it
// (the same approach the Stripe CLI takes). Kept honest: only events we can
// produce with plain resource creates. Payment-dependent events
// (invoice.paid, payment.failed, subscription.*) need a real Mollie payment
// and are intentionally omitted from v1.
//
// The bar for membership here is that `billkit listen` actually forwards
// something. `coupon.created` and `tax_rate.created` were listed once and did
// not clear it: creating those resources writes an audit-log entry and emits
// no event, so the trigger appeared to work and the listener stayed silent,
// which reads as a broken webhook handler. That is the exact failure this
// command exists to rule out. Re-add them when, and only when, the API emits
// them (services/coupons.py and services/tax_rates.py are the emit sites).
var triggers = map[string]func(ctx context.Context, c *api.Client) ([]byte, error){
	"customer.created": func(ctx context.Context, c *api.Client) ([]byte, error) {
		return c.Do(ctx, "POST", "/v1/customers", map[string]any{
			"email": "billkit-cli-trigger@example.com",
			"name":  "BillKit CLI Trigger",
		})
	},
	"customer.updated": func(ctx context.Context, c *api.Client) ([]byte, error) {
		created, err := c.Do(ctx, "POST", "/v1/customers", map[string]any{"email": "cli-upd@example.com"})
		if err != nil {
			return nil, err
		}
		return c.Do(ctx, "POST", "/v1/customers/"+idOf(created), map[string]any{"name": "Renamed by CLI"})
	},
	"product.created": func(ctx context.Context, c *api.Client) ([]byte, error) {
		return c.Do(ctx, "POST", "/v1/products", map[string]any{"name": "CLI Trigger Product"})
	},
}

func triggerCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "trigger [event-type]",
		Short: "Fire a test event by making the real API call that emits it",
		Long: "Create real test-mode resources so BillKit emits the corresponding event\n" +
			"(which `billkit listen` then forwards to your app). Run without an argument\n" +
			"to list the supported event types.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Println("Supported trigger events:")
				for _, name := range sortedTriggerNames() {
					fmt.Println("  " + name)
				}
				return nil
			}

			fn, ok := triggers[args[0]]
			if !ok {
				return fmt.Errorf("unsupported trigger %q — run `billkit trigger` to list supported events", args[0])
			}

			c, err := client(cmd, g)
			if err != nil {
				return err
			}
			if config.Mode(c.APIKey) == "live" {
				return fmt.Errorf("refusing to trigger against a live key — trigger is test-mode only")
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			out, err := fn(ctx, c)
			if err != nil {
				return err
			}

			fmt.Fprintf(cmd.ErrOrStderr(), "Triggered %s\n", args[0])
			fprintJSON(cmd.OutOrStdout(), g.color, out)
			return nil
		},
	}
}

func sortedTriggerNames() []string {
	names := make([]string, 0, len(triggers))
	for name := range triggers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
