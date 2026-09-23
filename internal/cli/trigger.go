package cli

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/billkit-eu/billkit-cli/internal/api"
	"github.com/billkit-eu/billkit-cli/internal/config"
	"github.com/spf13/cobra"
)

// triggerWebhookURL is where a triggered webhook endpoint points. It is never
// called by this CLI; the endpoint exists only so creating, updating and
// deleting it emits the three webhook_endpoint.* events. example.com is
// IANA-reserved for exactly this, so the deliveries the API will attempt
// against it cannot reach anyone's real service.
const triggerWebhookURL = "https://example.com/billkit-cli-trigger"

// triggers maps an event type to the real test-mode API call(s) that emit it
// (the same approach the Stripe CLI takes). Kept honest: only events we can
// produce with plain resource writes and no payment. Payment-dependent events
// (invoice.paid, payment.failed, subscription.*) need a real Mollie payment
// and are intentionally omitted from v1.
//
// The bar for membership here is that `billkit listen` actually forwards
// something. `coupon.created` and `tax_rate.created` shipped once without
// clearing it: creating those resources wrote an audit-log entry and emitted
// no event, so the trigger appeared to work and the listener stayed silent,
// which reads as a broken webhook handler. That is the exact failure this
// command exists to rule out.
//
// `coupon.*` has since cleared it: services/coupons.py emits both
// `coupon.created` and `coupon.updated`, so it is back. `tax_rate.*` still
// emits nothing and stays out. Every entry below was checked against the
// emit site in the service that owns the resource; see trigger_test.go,
// which pins the list so the next addition is a deliberate act.
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
	// The emit site is purge, not delete: DELETE /v1/customers/{id} hides the
	// customer and emits nothing, while POST /v1/customers/{id}/purge is the
	// GDPR erasure and carries the pre-purge snapshot
	// (services/customers.py). `confirmed` must be true or the API refuses.
	"customer.deleted": func(ctx context.Context, c *api.Client) ([]byte, error) {
		created, err := c.Do(ctx, "POST", "/v1/customers", map[string]any{"email": "cli-del@example.com"})
		if err != nil {
			return nil, err
		}
		return c.Do(ctx, "POST", "/v1/customers/"+idOf(created)+"/purge", map[string]any{"confirmed": true})
	},
	"product.created": func(ctx context.Context, c *api.Client) ([]byte, error) {
		return c.Do(ctx, "POST", "/v1/products", map[string]any{"name": "CLI Trigger Product"})
	},
	"product.updated": func(ctx context.Context, c *api.Client) ([]byte, error) {
		id, err := triggerProduct(ctx, c)
		if err != nil {
			return nil, err
		}
		return c.Do(ctx, "POST", "/v1/products/"+id, map[string]any{"description": "Updated by the BillKit CLI"})
	},
	// `active: false` is what splits product.archived from product.updated
	// (services/products.py: `archiving = command.active is False and
	// product.active`), so it has to run against a product that is still
	// active, which a freshly created one is.
	"product.archived": func(ctx context.Context, c *api.Client) ([]byte, error) {
		id, err := triggerProduct(ctx, c)
		if err != nil {
			return nil, err
		}
		return c.Do(ctx, "POST", "/v1/products/"+id, map[string]any{"active": false})
	},
	"price.created": func(ctx context.Context, c *api.Client) ([]byte, error) {
		productID, err := triggerProduct(ctx, c)
		if err != nil {
			return nil, err
		}
		return c.Do(ctx, "POST", "/v1/prices", triggerPriceBody(productID))
	},
	// PriceUpdate refuses amount/currency/interval by design, and the service
	// emits nothing at all when no field actually changes, so this sets
	// metadata, which a price created moments ago does not have.
	"price.updated": func(ctx context.Context, c *api.Client) ([]byte, error) {
		id, err := triggerPrice(ctx, c)
		if err != nil {
			return nil, err
		}
		return c.Do(ctx, "POST", "/v1/prices/"+id, map[string]any{
			"metadata": map[string]string{"source": "billkit-cli-trigger"},
		})
	},
	"price.archived": func(ctx context.Context, c *api.Client) ([]byte, error) {
		id, err := triggerPrice(ctx, c)
		if err != nil {
			return nil, err
		}
		return c.Do(ctx, "POST", "/v1/prices/"+id, map[string]any{"active": false})
	},
	"coupon.created": func(ctx context.Context, c *api.Client) ([]byte, error) {
		return c.Do(ctx, "POST", "/v1/coupons", triggerCouponBody())
	},
	// CouponUpdate cannot touch the discount itself, so this moves
	// max_redemptions, which a coupon created with none does not have.
	"coupon.updated": func(ctx context.Context, c *api.Client) ([]byte, error) {
		created, err := c.Do(ctx, "POST", "/v1/coupons", triggerCouponBody())
		if err != nil {
			return nil, err
		}
		return c.Do(ctx, "POST", "/v1/coupons/"+idOf(created), map[string]any{"max_redemptions": 5})
	},
	"webhook_endpoint.created": func(ctx context.Context, c *api.Client) ([]byte, error) {
		return c.Do(ctx, "POST", "/v1/webhook_endpoints", triggerEndpointBody())
	},
	"webhook_endpoint.updated": func(ctx context.Context, c *api.Client) ([]byte, error) {
		id, err := triggerEndpoint(ctx, c)
		if err != nil {
			return nil, err
		}
		return c.Do(ctx, "POST", "/v1/webhook_endpoints/"+id, map[string]any{
			"description": "Updated by the BillKit CLI",
		})
	},
	"webhook_endpoint.deleted": func(ctx context.Context, c *api.Client) ([]byte, error) {
		id, err := triggerEndpoint(ctx, c)
		if err != nil {
			return nil, err
		}
		return c.Do(ctx, "DELETE", "/v1/webhook_endpoints/"+id, nil)
	},
}

// triggerProduct creates the parent a price or a product update needs.
func triggerProduct(ctx context.Context, c *api.Client) (string, error) {
	created, err := c.Do(ctx, "POST", "/v1/products", map[string]any{"name": "CLI Trigger Product"})
	if err != nil {
		return "", err
	}
	return idOf(created), nil
}

// triggerPrice creates a product and a price on it, for the two triggers that
// need a price that already exists.
func triggerPrice(ctx context.Context, c *api.Client) (string, error) {
	productID, err := triggerProduct(ctx, c)
	if err != nil {
		return "", err
	}
	created, err := c.Do(ctx, "POST", "/v1/prices", triggerPriceBody(productID))
	if err != nil {
		return "", err
	}
	return idOf(created), nil
}

// triggerPriceBody is PriceCreate's required shape: a product, exactly one of
// amount_cents/unit_amount_decimal, and an interval.
func triggerPriceBody(productID string) map[string]any {
	return map[string]any{
		"product_id":   productID,
		"amount_cents": 1000,
		"currency":     "EUR",
		"interval":     "month",
	}
}

// triggerCouponBody mints a fresh coupon body. The code has to be unique per
// run: a tenant cannot hold two coupons with the same code, and a trigger
// that works once and then 409s for the rest of the day is worse than none.
func triggerCouponBody() map[string]any {
	return map[string]any{
		"code":           "CLITRIGGER" + strings.ToUpper(randomSuffix()),
		"discount_type":  "percent",
		"discount_value": 10,
	}
}

// triggerEndpointBody points at a reserved domain and subscribes to one event
// type rather than the wildcard, so the endpoint this creates cannot turn
// into a fan-out of every event in the account.
func triggerEndpointBody() map[string]any {
	return map[string]any{
		"url":            triggerWebhookURL,
		"enabled_events": []string{"customer.created"},
		"description":    "Created by the BillKit CLI trigger",
	}
}

// triggerEndpoint creates the endpoint the update and delete triggers act on.
func triggerEndpoint(ctx context.Context, c *api.Client) (string, error) {
	created, err := c.Do(ctx, "POST", "/v1/webhook_endpoints", triggerEndpointBody())
	if err != nil {
		return "", err
	}
	return idOf(created), nil
}

// randomSuffix is 8 hex characters of entropy, for the one trigger body that
// needs to be unique per run. crypto/rand because it is the only source that
// cannot repeat across two CLIs started in the same second; this is not a
// security boundary, it is a collision one.
func randomSuffix() string {
	buf := make([]byte, 4)
	if _, err := cryptorand.Read(buf); err != nil {
		// Unreachable in practice, and a timestamp is still unique enough to
		// keep the trigger working rather than failing on entropy.
		return strconv.FormatInt(time.Now().UnixNano()%1e8, 16)
	}
	return hex.EncodeToString(buf)
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
				out := cmd.OutOrStdout()
				fmt.Fprintln(out, "Supported trigger events:")
				for _, name := range sortedTriggerNames() {
					fmt.Fprintln(out, "  "+name)
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
	return slices.Sorted(maps.Keys(triggers))
}
