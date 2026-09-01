// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"time"

	stripe "github.com/stripe/stripe-go/v76"
	"github.com/stripe/stripe-go/v76/checkout/session"

	"github.com/lenaxia/llmsafespaces/api/internal/services/database"
	"github.com/lenaxia/llmsafespaces/pkg/billing"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// pendingOrgStore is the data-access surface the cleanup cron needs.
type pendingOrgStore interface {
	ListPendingOrgsOlderThan(ctx context.Context, maxAge time.Duration) ([]database.PendingOrgCleanup, error)
	UpdateOrgStatus(ctx context.Context, orgID string, status *types.OrgStatus, subStatus *types.OrgSubscriptionStatus, planID *types.OrgPlan) error
	HardDeleteOrg(ctx context.Context, orgID string) error
}

// PendingOrgCleaner reaps pending_activation orgs whose Stripe checkout was
// never completed. It runs on a ticker; each run lists stale pending orgs,
// verifies the checkout state with Stripe, and either activates (paid but
// webhook lost) or hard-deletes (checkout expired/canceled). On any Stripe API
// failure for a given org it skips that org and retries next cycle — never
// deletes an org it cannot verify.
type PendingOrgCleaner struct {
	store    pendingOrgStore
	logger   webHookLogger
	interval time.Duration
	maxAge   time.Duration
	// checkoutCompletedFn reports whether the given Stripe customer has a
	// completed (paid) checkout session. Defaults to the Stripe API lookup;
	// overridable in tests.
	checkoutCompletedFn func(ctx context.Context, customerID string) (bool, error)
}

// NewPendingOrgCleaner constructs the cleaner. interval is the tick period;
// maxAge is how old a pending_activation org must be before it is eligible.
// provider is used to build the default Stripe checkout lookup; pass nil only
// in tests that inject checkoutCompletedFn directly.
func NewPendingOrgCleaner(store pendingOrgStore, provider billing.CheckoutProvider, logger webHookLogger, interval, maxAge time.Duration) *PendingOrgCleaner {
	c := &PendingOrgCleaner{store: store, logger: logger, interval: interval, maxAge: maxAge}
	if provider != nil {
		c.checkoutCompletedFn = func(ctx context.Context, customerID string) (bool, error) {
			return stripeCheckoutCompleted(ctx, customerID)
		}
	}
	return c
}

// Run blocks until ctx is canceled, reaping on each tick.
func (c *PendingOrgCleaner) Run(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.runOnce(ctx)
		}
	}
}

// runOnce performs a single cleanup pass. It is exported-by-fact (called from
// Run) so tests can invoke a single pass without waiting for a tick.
func (c *PendingOrgCleaner) runOnce(ctx context.Context) {
	orgs, err := c.store.ListPendingOrgsOlderThan(ctx, c.maxAge)
	if err != nil {
		c.logger.Error("pending org cleanup: list failed", err)
		return
	}
	for _, org := range orgs {
		c.processOrg(ctx, org)
	}
}

func (c *PendingOrgCleaner) processOrg(ctx context.Context, org database.PendingOrgCleanup) {
	if org.StripeCustomerID == "" {
		// No Stripe customer linked — the checkout flow never started. Delete.
		if err := c.store.HardDeleteOrg(ctx, org.OrgID); err != nil {
			c.logger.Error("pending org cleanup: delete failed", err, "orgID", org.OrgID)
		}
		return
	}

	completed, err := c.checkoutCompletedFn(ctx, org.StripeCustomerID)
	if err != nil {
		// Stripe API failure: skip, retry next cycle. NEVER delete on failure.
		c.logger.Warn("pending org cleanup: stripe checkout lookup failed, skipping", "orgID", org.OrgID)
		return
	}
	if completed {
		active := types.OrgStatusActive
		sub := types.SubscriptionActive
		if err := c.store.UpdateOrgStatus(ctx, org.OrgID, &active, &sub, nil); err != nil {
			c.logger.Error("pending org cleanup: activate failed", err, "orgID", org.OrgID)
		}
		return
	}
	if err := c.store.HardDeleteOrg(ctx, org.OrgID); err != nil {
		c.logger.Error("pending org cleanup: delete failed", err, "orgID", org.OrgID)
	}
}

// stripeCheckoutCompleted queries Stripe for the customer's checkout sessions
// and returns true if any completed (paid) session exists.
func stripeCheckoutCompleted(ctx context.Context, customerID string) (bool, error) {
	params := &stripe.CheckoutSessionListParams{Customer: stripe.String(customerID)}
	params.Context = ctx
	iter := session.List(params)
	for iter.Next() {
		s := iter.CheckoutSession()
		if s.Status == stripe.CheckoutSessionStatusComplete && s.PaymentStatus == stripe.CheckoutSessionPaymentStatusPaid {
			return true, nil
		}
	}
	return false, iter.Err()
}
