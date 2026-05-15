package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/db"
)

// Per-agent paywalled-feature subscription endpoints. v1 ships with one
// feature ("telegram") but the data model is feature-keyed so adding
// Discord / Slack / etc. later is a matter of a new env var + a new
// frontend tile, no schema change.
//
// Auth: cookie-protected (mounted under /api/dashboard) for end-user
// flows; sessions-api hits the API-key-protected entitlement endpoint
// (see dashboardAgentEntitlements wired in router.go).

const (
	telegramFeature = "telegram"
)

// reconcileAgentFeatureFromStripe checks Stripe for an existing
// subscription matching (agent_id, feature) and persists a local row
// if Stripe has it but we don't. Used to recover from a missing
// webhook (e.g. STRIPE_WEBHOOK_SECRET unset, transient network glitch).
//
// Returns the resulting active subscription, or nil if Stripe also
// has nothing.
func (s *Server) reconcileAgentFeatureFromStripe(ctx context.Context, orgID uuid.UUID, agentID, feature string) (*db.AgentSubscription, error) {
	if s.stripeClient == nil || s.store == nil {
		return nil, nil
	}
	org, err := s.store.GetOrg(ctx, orgID)
	if err != nil || org.StripeCustomerID == nil {
		return nil, err
	}
	stripeSub, err := s.stripeClient.FindAgentFeatureSubscription(*org.StripeCustomerID, agentID, feature)
	if err != nil || stripeSub == nil {
		return nil, err
	}

	// Idempotent: if we already have it, just nudge status.
	row, err := s.store.GetAgentSubscriptionByStripeID(ctx, stripeSub.ID)
	if err != nil {
		return nil, err
	}

	var currentPeriodEnd *time.Time
	if stripeSub.Items != nil && len(stripeSub.Items.Data) > 0 && stripeSub.Items.Data[0].CurrentPeriodEnd > 0 {
		t := time.Unix(stripeSub.Items.Data[0].CurrentPeriodEnd, 0).UTC()
		currentPeriodEnd = &t
	}
	priceID := ""
	if stripeSub.Items != nil && len(stripeSub.Items.Data) > 0 && stripeSub.Items.Data[0].Price != nil {
		priceID = stripeSub.Items.Data[0].Price.ID
	}

	if row != nil {
		// Update status from Stripe (in case we missed the webhook update).
		if err := s.store.UpdateAgentSubscriptionFromStripe(ctx, stripeSub.ID, string(stripeSub.Status), currentPeriodEnd, stripeSub.CancelAtPeriodEnd, nil); err != nil {
			return nil, err
		}
		row.Status = string(stripeSub.Status)
		row.CurrentPeriodEnd = currentPeriodEnd
		row.CancelAtPeriodEnd = stripeSub.CancelAtPeriodEnd
		return row, nil
	}

	saved, err := s.store.CreateAgentSubscription(ctx, db.AgentSubscription{
		OrgID:                orgID,
		AgentID:              agentID,
		Feature:              feature,
		StripeCustomerID:     *org.StripeCustomerID,
		StripeSubscriptionID: stripeSub.ID,
		StripePriceID:        priceID,
		Status:               string(stripeSub.Status),
		CurrentPeriodEnd:     currentPeriodEnd,
		CancelAtPeriodEnd:    stripeSub.CancelAtPeriodEnd,
	})
	if err != nil {
		return nil, err
	}
	log.Printf("billing: reconciled agent_subscription from Stripe (agent=%s feature=%s sub=%s status=%s)",
		agentID, feature, stripeSub.ID, stripeSub.Status)
	return saved, nil
}

// dashboardSubscribeAgentFeature creates a Stripe subscription for the
// given (agent_id, feature) pair. Two flows:
//
//  1. Customer has a saved default payment method → call
//     subscriptions.create directly. Returns {status:"active", agent_id}.
//  2. No card on file → create a Stripe Checkout session
//     (mode=subscription) and return its URL. Frontend redirects.
//
// Idempotent: if an active subscription already exists for this
// (agent_id, feature), returns the existing one with status:"already_subscribed".
func (s *Server) dashboardSubscribeAgentFeature(c echo.Context) error {
	if s.stripeClient == nil || s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "billing not configured"})
	}

	feature := c.Param("feature")
	agentID := c.Param("agentId")
	if agentID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "agent_id required"})
	}

	priceID, ok := s.priceIDForFeature(feature)
	if !ok {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("unknown feature %q or price not configured", feature),
		})
	}
	if priceID == "" {
		// Feature exists but is ungated on this deployment (dev mode)
		// — surface that explicitly so the caller knows there's nothing
		// to pay for. UI treats this like already-entitled.
		return c.JSON(http.StatusOK, map[string]any{
			"status":   "ungated",
			"feature":  feature,
			"agent_id": agentID,
		})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	ctx := c.Request().Context()

	// Already subscribed? Return existing.
	existing, err := s.store.GetActiveAgentSubscription(ctx, agentID, feature)
	if err != nil {
		log.Printf("billing: get active sub failed: %v", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "internal"})
	}
	if existing == nil {
		// Reconcile path: maybe Stripe has the sub but our DB lost the
		// webhook (or signature wasn't configured). Re-import before
		// telling the user to pay again.
		if reconciled, rerr := s.reconcileAgentFeatureFromStripe(ctx, orgID, agentID, feature); rerr == nil && reconciled != nil {
			existing = reconciled
		}
	}
	if existing != nil && existing.OrgID == orgID && db.AgentSubscriptionIsActive(existing.Status) {
		return c.JSON(http.StatusOK, map[string]any{
			"status":          "already_subscribed",
			"feature":         feature,
			"agent_id":        agentID,
			"subscription_id": existing.StripeSubscriptionID,
		})
	}

	// Need a Stripe customer ID for the org. Existing billing flow
	// creates one when the org first enters checkout for the pro plan.
	// For per-agent subs we may need to create one here lazily so the
	// user can pay for telegram even on a free plan.
	org, err := s.store.GetOrg(ctx, orgID)
	if err != nil {
		log.Printf("billing: get org failed: %v", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "org not found"})
	}
	customerID := ""
	if org.StripeCustomerID != nil {
		customerID = *org.StripeCustomerID
	}
	if customerID == "" {
		newCustomer, err := s.stripeClient.CreateCustomer(org.Name, "")
		if err != nil {
			log.Printf("billing: create customer failed: %v", err)
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "stripe customer create failed"})
		}
		customerID = newCustomer
		if err := s.store.SetStripeCustomerID(ctx, orgID, customerID); err != nil {
			log.Printf("billing: persist customer id failed: %v", err)
			// Don't fail the request — we have the ID locally and will
			// use it; persistence retry happens on next request.
		}
	}

	metadata := map[string]string{
		"type":     "agent_feature_subscription",
		"org_id":   orgID.String(),
		"agent_id": agentID,
		"feature":  feature,
	}

	pmID, err := s.stripeClient.GetCustomerDefaultPaymentMethod(customerID)
	if err != nil {
		log.Printf("billing: fetch default pm failed: %v", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "stripe lookup failed"})
	}

	if pmID == "" {
		// No card on file — bounce through Checkout. The webhook
		// handler will record the subscription on completion.
		successURL := s.stripeClient.SuccessURL()
		cancelURL := s.stripeClient.CancelURL()
		url, err := s.stripeClient.CreateAgentFeatureCheckoutSession(customerID, priceID, successURL, cancelURL, metadata)
		if err != nil {
			log.Printf("billing: create checkout failed: %v", err)
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "checkout create failed"})
		}
		return c.JSON(http.StatusOK, map[string]any{
			"status":      "checkout_required",
			"checkout_url": url,
			"feature":     feature,
			"agent_id":    agentID,
		})
	}

	// Saved card path — direct subscription, no redirect.
	sub, err := s.stripeClient.CreateAgentFeatureSubscription(customerID, priceID, metadata)
	if err != nil {
		log.Printf("billing: direct subscribe failed: %v", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error":   "subscribe failed",
			"detail":  err.Error(),
		})
	}

	// stripe-go v82 moved CurrentPeriodEnd off the Subscription onto each
	// SubscriptionItem; for our single-item subs we just read the first.
	var currentPeriodEnd time.Time
	if sub.Items != nil && len(sub.Items.Data) > 0 && sub.Items.Data[0].CurrentPeriodEnd > 0 {
		currentPeriodEnd = time.Unix(sub.Items.Data[0].CurrentPeriodEnd, 0).UTC()
	}
	row := db.AgentSubscription{
		OrgID:                orgID,
		AgentID:              agentID,
		Feature:              feature,
		StripeCustomerID:     customerID,
		StripeSubscriptionID: sub.ID,
		StripePriceID:        priceID,
		Status:               string(sub.Status),
		CancelAtPeriodEnd:    sub.CancelAtPeriodEnd,
	}
	if !currentPeriodEnd.IsZero() {
		row.CurrentPeriodEnd = &currentPeriodEnd
	}
	saved, err := s.store.CreateAgentSubscription(ctx, row)
	if err != nil {
		// Stripe sub exists but DB write failed — log loudly. The
		// webhook handler will reconcile on next event.
		log.Printf("billing: persist agent_subscription failed (stripe sub=%s, agent=%s): %v", sub.ID, agentID, err)
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "subscription created in Stripe but local persist failed; will reconcile on next webhook",
		})
	}

	return c.JSON(http.StatusOK, map[string]any{
		"status":          "active",
		"feature":         feature,
		"agent_id":        agentID,
		"subscription_id": saved.StripeSubscriptionID,
		"price_id":        saved.StripePriceID,
	})
}

// dashboardCancelAgentFeature schedules the per-agent subscription to
// stop renewing at period end. Customer keeps the feature until the
// current billing period ends (no proration, no immediate cutoff).
// The webhook handler flips the local row to canceled when Stripe
// confirms the deletion.
func (s *Server) dashboardCancelAgentFeature(c echo.Context) error {
	if s.stripeClient == nil || s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "billing not configured"})
	}

	feature := c.Param("feature")
	agentID := c.Param("agentId")
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	ctx := c.Request().Context()
	existing, err := s.store.GetActiveAgentSubscription(ctx, agentID, feature)
	if err != nil {
		log.Printf("billing: get active sub failed: %v", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "internal"})
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "no active subscription"})
	}
	if existing.OrgID != orgID {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "no active subscription"})
	}

	if _, err := s.stripeClient.CancelAgentFeatureSubscription(existing.StripeSubscriptionID, false); err != nil {
		log.Printf("billing: cancel subscription failed: %v", err)
		return c.JSON(http.StatusBadGateway, map[string]string{
			"error":  "stripe cancel failed",
			"detail": err.Error(),
		})
	}
	// Optimistic local update — webhook will reconcile.
	if err := s.store.UpdateAgentSubscriptionFromStripe(ctx, existing.StripeSubscriptionID, existing.Status, existing.CurrentPeriodEnd, true, nil); err != nil {
		log.Printf("billing: persist cancel-at-period-end failed: %v", err)
	}
	return c.JSON(http.StatusOK, map[string]any{
		"status":               "scheduled_cancel",
		"feature":              feature,
		"agent_id":             agentID,
		"cancel_at_period_end": true,
		"current_period_end":   existing.CurrentPeriodEnd,
	})
}

// dashboardListAgentEntitlements returns which paywalled features the
// caller's org has paid for on a given agent. Used by the agent detail
// page to decide whether to show the paywall or the connect flow.
func (s *Server) dashboardListAgentEntitlements(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "not configured"})
	}
	agentID := c.Param("agentId")
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	type entRow struct {
		Feature              string     `json:"feature"`
		Entitled             bool       `json:"entitled"`
		Reason               string     `json:"reason,omitempty"`
		PriceMonthlyCents    int64      `json:"price_monthly_cents,omitempty"`
		Status               string     `json:"status,omitempty"`
		CurrentPeriodEnd     *time.Time `json:"current_period_end,omitempty"`
		CancelAtPeriodEnd    bool       `json:"cancel_at_period_end,omitempty"`
		StripeSubscriptionID string     `json:"stripe_subscription_id,omitempty"`
	}

	out := []entRow{}
	for _, feature := range []string{telegramFeature} {
		priceID, _ := s.priceIDForFeature(feature)
		row := entRow{Feature: feature}
		if priceID == "" {
			// Ungated on this deployment.
			row.Entitled = true
			row.Reason = "ungated"
			out = append(out, row)
			continue
		}
		sub, err := s.store.GetActiveAgentSubscription(c.Request().Context(), agentID, feature)
		if err != nil {
			log.Printf("billing: list entitlements lookup failed: %v", err)
		}
		if sub == nil {
			// Reconcile path — see subscribe handler.
			if reconciled, rerr := s.reconcileAgentFeatureFromStripe(c.Request().Context(), orgID, agentID, feature); rerr == nil && reconciled != nil {
				sub = reconciled
			}
		}
		if sub != nil && sub.OrgID == orgID && db.AgentSubscriptionIsActive(sub.Status) {
			row.Entitled = true
			row.Status = sub.Status
			row.CurrentPeriodEnd = sub.CurrentPeriodEnd
			row.CancelAtPeriodEnd = sub.CancelAtPeriodEnd
			row.StripeSubscriptionID = sub.StripeSubscriptionID
		} else {
			row.Entitled = false
			row.Reason = "subscription_required"
		}
		row.PriceMonthlyCents = priceMonthlyCentsForFeature(feature)
		out = append(out, row)
	}
	return c.JSON(http.StatusOK, map[string]any{"agent_id": agentID, "entitlements": out})
}

// dashboardListOrgAgentSubscriptions returns every per-agent subscription
// the org owns (active and historical). Powers the Billing → Agents tab.
//
// We deliberately do not filter by status here — past subscriptions
// (canceled, etc.) are useful billing context. The UI groups by agent
// and feature and shows the most-recent per pair.
func (s *Server) dashboardListOrgAgentSubscriptions(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}
	subs, err := s.store.ListAgentSubscriptionsByOrg(c.Request().Context(), orgID)
	if err != nil {
		log.Printf("billing: list org agent subscriptions failed: %v", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "internal"})
	}

	type row struct {
		AgentID              string     `json:"agent_id"`
		Feature              string     `json:"feature"`
		Status               string     `json:"status"`
		Active               bool       `json:"active"`
		PriceMonthlyCents    int64      `json:"price_monthly_cents"`
		CurrentPeriodEnd     *time.Time `json:"current_period_end,omitempty"`
		CancelAtPeriodEnd    bool       `json:"cancel_at_period_end"`
		CanceledAt           *time.Time `json:"canceled_at,omitempty"`
		CreatedAt            time.Time  `json:"created_at"`
		StripeSubscriptionID string     `json:"stripe_subscription_id"`
	}
	out := make([]row, 0, len(subs))
	for _, sub := range subs {
		out = append(out, row{
			AgentID:              sub.AgentID,
			Feature:              sub.Feature,
			Status:               sub.Status,
			Active:               db.AgentSubscriptionIsActive(sub.Status),
			PriceMonthlyCents:    priceMonthlyCentsForFeature(sub.Feature),
			CurrentPeriodEnd:     sub.CurrentPeriodEnd,
			CancelAtPeriodEnd:    sub.CancelAtPeriodEnd,
			CanceledAt:           sub.CanceledAt,
			CreatedAt:            sub.CreatedAt,
			StripeSubscriptionID: sub.StripeSubscriptionID,
		})
	}
	return c.JSON(http.StatusOK, map[string]any{"subscriptions": out})
}

// apiAgentEntitlement is the sessions-api-callable variant — accepts
// JWT identity auth (aud=opencomputer-api) and returns a tight
// {entitled: bool, reason} payload. Used by sessions-api right before
// allowing a connect-channel operation.
func (s *Server) apiAgentEntitlement(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "not configured"})
	}
	agentID := c.Param("agentId")
	feature := c.Param("feature")
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	priceID, knownFeature := s.priceIDForFeature(feature)
	if !knownFeature {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "unknown feature"})
	}
	if priceID == "" {
		// Feature ungated on this deployment.
		return c.JSON(http.StatusOK, map[string]any{
			"entitled":   true,
			"reason":     "ungated",
			"feature":    feature,
			"agent_id":   agentID,
		})
	}

	sub, err := s.store.GetActiveAgentSubscription(c.Request().Context(), agentID, feature)
	if err != nil {
		log.Printf("billing: entitlement lookup failed: %v", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "internal"})
	}
	if sub == nil {
		// Same reconcile path as the dashboard handler — Stripe is the
		// source of truth, so look there before telling the CLI/sessions-api
		// the user hasn't paid.
		if reconciled, rerr := s.reconcileAgentFeatureFromStripe(c.Request().Context(), orgID, agentID, feature); rerr == nil && reconciled != nil {
			sub = reconciled
		}
	}
	if sub == nil || sub.OrgID != orgID || !db.AgentSubscriptionIsActive(sub.Status) {
		return c.JSON(http.StatusPaymentRequired, map[string]any{
			"entitled":            false,
			"reason":              "subscription_required",
			"feature":             feature,
			"agent_id":            agentID,
			"price_monthly_cents": priceMonthlyCentsForFeature(feature),
		})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"entitled":              true,
		"feature":               feature,
		"agent_id":              agentID,
		"status":                sub.Status,
		"current_period_end":    sub.CurrentPeriodEnd,
		"cancel_at_period_end":  sub.CancelAtPeriodEnd,
	})
}

// priceIDForFeature maps a feature string to its configured Stripe
// price ID. Returns ("", true) if the feature is known but ungated on
// this deployment (used as the dev-mode escape). Returns ("", false)
// for unknown features.
func (s *Server) priceIDForFeature(feature string) (string, bool) {
	if s.stripeClient == nil {
		return "", feature == telegramFeature // unknown stripe → behave as ungated for the known features
	}
	switch feature {
	case telegramFeature:
		return s.stripeClient.TelegramAgentPriceID, true
	default:
		return "", false
	}
}

// priceMonthlyCentsForFeature is the human-readable price the UI
// surfaces. Hardcoded for now — Stripe is the source of truth for
// what the customer is actually charged; this is just for display.
// If the two diverge, that's a config bug.
func priceMonthlyCentsForFeature(feature string) int64 {
	switch feature {
	case telegramFeature:
		return 2000 // $20.00/mo
	}
	return 0
}

