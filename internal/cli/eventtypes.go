package cli

import (
	"sort"
	"strings"
)

// Code generated from api/billkit/core/events.py. DO NOT EDIT BY HAND.
//
// The catalogue is the API's, and this is a copy of it, so the copy can go
// stale in exactly the way the catalogue exists to prevent. That is what
// api/tests/test_cli_event_constants.py is for: it parses this file and
// fails if the two disagree in either direction.
//
// Why have it at all: `billkit listen --events <typo>` used to be accepted
// locally, sent, and answered with a stream that stayed empty forever. The
// server now rejects an unknown type, and this turns that round trip into an
// immediate local error naming the offender.

// knownEventTypes is every event type the API can deliver.
var knownEventTypes = map[string]struct{}{
	// Checkout
	"checkout.session.completed": {},
	"checkout.session.expired":   {},
	// Customer
	"customer.created": {},
	"customer.updated": {},
	"customer.deleted": {},
	// Subscription lifecycle
	"subscription.created":                {},
	"subscription.updated":                {},
	"subscription.canceled":               {},
	"subscription.past_due":               {},
	"subscription.paused":                 {},
	"subscription.resumed":                {},
	"subscription.reactivated":            {},
	"subscription.plan_changed":           {},
	"subscription.trial_started":          {},
	"subscription.trial_will_end":         {},
	"subscription.trial_ended":            {},
	"subscription.payment_method_updated": {},
	"subscription.coupon_applied":         {},
	"subscription.coupon_expired":         {},
	// Payments
	"payment.succeeded":          {},
	"payment.failed":             {},
	"one_shot_payment.succeeded": {},
	"one_shot_payment.failed":    {},
	"one_shot_payment.refunded":  {},
	// Refunds and disputes
	"refund.created":   {},
	"refund.succeeded": {},
	"refund.failed":    {},
	"dispute.created":  {},
	"dispute.closed":   {},
	// Documents
	"invoice.created":              {},
	"invoice.paid":                 {},
	"invoice.payment_failed":       {},
	"invoice.voided":               {},
	"invoice.marked_uncollectible": {},
	"credit_note.created":          {},
	// Catalogue
	"product.created":  {},
	"product.updated":  {},
	"product.archived": {},
	"price.created":    {},
	"price.updated":    {},
	"price.archived":   {},
	"coupon.created":   {},
	"coupon.updated":   {},
	// Endpoint management
	"webhook_endpoint.created": {},
	"webhook_endpoint.updated": {},
	"webhook_endpoint.deleted": {},
	// Operational
	"dunning.email_required": {},
}

// validateEventTypes reports the unknown entries in a --events filter.
//
// Returns them sorted so the message is stable, and does NOT accept the "*"
// wildcard: omitting --events already means "every event", and the stream
// endpoint rejects the wildcard for the same reason — an all-events filter
// that spells two ways is one a reader has to be taught.
//
// An empty or whitespace-only entry is skipped rather than reported: a
// trailing comma is a slip, not a typo'd event name, and the server drops
// empties too.
func validateEventTypes(filter string) []string {
	var unknown []string
	seen := map[string]struct{}{}
	for _, raw := range strings.Split(filter, ",") {
		t := strings.TrimSpace(raw)
		if t == "" {
			continue
		}
		if _, ok := knownEventTypes[t]; ok {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		unknown = append(unknown, t)
	}
	sort.Strings(unknown)
	return unknown
}
