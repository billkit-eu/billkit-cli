package cli

import (
	"reflect"
	"testing"
)

// The contract of `trigger` is not "make an API call". It is "make an API call
// that `billkit listen` then forwards something for". A trigger whose resource
// create emits no event is worse than no trigger at all: the command reports
// success, the listener stays silent, and the user goes looking for a bug in
// their own webhook handler. That is the one afternoon this CLI exists to save.
//
// `coupon.created` and `tax_rate.created` shipped in that broken state, because
// POST /v1/coupons and POST /v1/tax_rates wrote an audit-log row and emitted
// nothing. They were removed rather than documented around.
//
// `coupon.*` has since earned its place back: services/coupons.py emits
// `coupon.created` and `coupon.updated`. `tax_rate.*` still emits nothing and
// stays out.
//
// So this list is pinned. Adding a name here is a claim that the API emits that
// event, and this test is where that claim gets made deliberately instead of by
// accident. Before changing it, confirm the emit site exists in the API
// (grep for emit_event / emit_for in the service that owns the resource).
//
// The emit site for each entry below, all verified:
//
//	customer.created/.updated    services/customers.py
//	customer.deleted             services/customers.py (purge, not delete)
//	product.created              services/products.py
//	product.updated/.archived    services/products.py (the `archiving` split)
//	price.created                services/pricing.py
//	price.updated/.archived      services/pricing.py (the `archiving` split)
//	coupon.created/.updated      services/coupons.py
//	webhook_endpoint.*           services/webhook_endpoints.py
func TestTriggersOnlyContainsEventsTheAPIEmits(t *testing.T) {
	want := []string{
		"coupon.created",
		"coupon.updated",
		"customer.created",
		"customer.deleted",
		"customer.updated",
		"price.archived",
		"price.created",
		"price.updated",
		"product.archived",
		"product.created",
		"product.updated",
		"webhook_endpoint.created",
		"webhook_endpoint.deleted",
		"webhook_endpoint.updated",
	}

	got := sortedTriggerNames()

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trigger list changed.\n got: %v\nwant: %v\n\n"+
			"If you added one, verify the API actually emits it: a trigger that "+
			"creates a resource but emits no event makes `billkit listen` look "+
			"broken. See the comment above this test.", got, want)
	}
}

// Every trigger must also be dispatchable. A name in sortedTriggerNames() that
// is missing from the map would list in the help output and then fail as an
// unsupported trigger.
func TestEveryListedTriggerIsDispatchable(t *testing.T) {
	for _, name := range sortedTriggerNames() {
		if _, ok := triggers[name]; !ok {
			t.Errorf("trigger %q is listed but has no handler", name)
		}
	}
}

// Every trigger name must also be an event type the CLI's own catalogue knows
// about. The catalogue is parity-tested against the API's (see
// api/tests/test_cli_event_constants.py), so this chains the trigger list to
// the same source of truth without a second copy of it.
func TestEveryTriggerNamesAKnownEventType(t *testing.T) {
	for _, name := range sortedTriggerNames() {
		if _, ok := knownEventTypes[name]; !ok {
			t.Errorf("trigger %q is not an event type the API emits", name)
		}
	}
}

// A coupon code has to be unique per run: a tenant cannot hold two coupons
// with the same code, so a fixed one would work once and then 409 for the
// rest of the day.
func TestCouponTriggerCodeIsUniquePerCall(t *testing.T) {
	first, _ := triggerCouponBody()["code"].(string)
	second, _ := triggerCouponBody()["code"].(string)
	if first == "" || first == second {
		t.Fatalf("coupon codes %q and %q must differ", first, second)
	}
}
