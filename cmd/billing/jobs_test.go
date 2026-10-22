package main

import (
	"testing"

	"github.com/catalyst/billing/internal/credit"
)

// TestSettlementApplierRoutesDeposits pins the one wiring that makes a credit deposit work.
//
// A deposit is a payment with no invoice behind it: the customer's money becomes a balance the
// shop owes them, not revenue. That is only true if the deposit applier is in the chain. With the
// plain fulfilment applier the deposit settles, the invoice is marked paid, the customer's balance
// stays at zero, and nothing errors — the customer has paid and received nothing.
//
// This test exists because the failure is invisible. Every other test in the repository builds its
// own applier, so they all pass whether or not production wires the right one. This is the only
// place that asserts what the worker actually constructs.
func TestSettlementApplierRoutesDeposits(t *testing.T) {
	// A nil pool is safe: construction only stores the pointer, and the assertion is about the
	// type of the chain rather than its behaviour. Using a real pool would make this a database
	// test for no gain — the routing behaviour itself is covered by
	// credit.TestDepositSettlesThroughTheWebhookPipeline against a real database.
	applier := newSettlementApplier(nil)

	if _, ok := applier.(*credit.DepositApplier); !ok {
		t.Fatalf("the settlement applier is %T, want *credit.DepositApplier: a credit deposit "+
			"settled through this chain would mark its invoice paid and never credit the customer",
			applier)
	}
}
