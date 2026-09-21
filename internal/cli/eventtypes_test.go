package cli

import (
	"strings"
	"testing"
)

// TestValidateEventTypesNamesOnlyTheOffenders is the point of the whole
// mechanism: a typo'd --events filter used to be accepted locally, sent, and
// answered with a stream that stayed empty forever, which is indistinguishable
// from a quiet account.
func TestValidateEventTypesNamesOnlyTheOffenders(t *testing.T) {
	got := validateEventTypes("subscription.updated,customer.subscription.updated,invoice.paid")
	if len(got) != 1 || got[0] != "customer.subscription.updated" {
		t.Fatalf("want exactly the Stripe spelling reported, got %v", got)
	}
}

func TestValidateEventTypesAcceptsTheWholeCatalogue(t *testing.T) {
	all := make([]string, 0, len(knownEventTypes))
	for k := range knownEventTypes {
		all = append(all, k)
	}
	if got := validateEventTypes(strings.Join(all, ",")); len(got) != 0 {
		t.Fatalf("the catalogue must validate against itself, got %v", got)
	}
}

// A trailing comma is a slip, not a typo'd event name, and the server drops
// empty entries too — so reporting one would be a false alarm on input that
// works.
func TestValidateEventTypesIgnoresEmptyEntries(t *testing.T) {
	for _, in := range []string{"", ",", "invoice.paid,", " invoice.paid , "} {
		if got := validateEventTypes(in); len(got) != 0 {
			t.Fatalf("%q should be clean, got %v", in, got)
		}
	}
}

// Omitting --events already means "every event". Accepting "*" as a synonym
// would let an all-events filter spell two ways, and the server rejects it for
// the same reason.
func TestValidateEventTypesRejectsTheWildcard(t *testing.T) {
	got := validateEventTypes("*")
	if len(got) != 1 || got[0] != "*" {
		t.Fatalf("wildcard must be rejected, got %v", got)
	}
}

func TestValidateEventTypesDeduplicatesAndSorts(t *testing.T) {
	got := validateEventTypes("zeta.bogus,alpha.bogus,zeta.bogus")
	if len(got) != 2 || got[0] != "alpha.bogus" || got[1] != "zeta.bogus" {
		t.Fatalf("want a sorted, deduplicated report, got %v", got)
	}
}
