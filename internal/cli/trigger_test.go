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
// POST /v1/coupons and POST /v1/tax_rates write an audit-log row and emit
// nothing. They were removed rather than documented around.
//
// So this list is pinned. Adding a name here is a claim that the API emits that
// event, and this test is where that claim gets made deliberately instead of by
// accident. Before changing it, confirm the emit site exists in the API
// (grep for emit_event / emit_for in the service that owns the resource).
func TestTriggersOnlyContainsEventsTheAPIEmits(t *testing.T) {
	want := []string{
		"customer.created",
		"customer.updated",
		"product.created",
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
