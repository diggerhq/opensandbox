package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/db"
)

// billingSetup initiates the upgrade flow: creates Stripe customer + Checkout session.
func (s *Server) billingSetup(c echo.Context) error {
	if s.store == nil || s.stripeClient == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "billing not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}
	ctx := c.Request().Context()
	org, err := s.store.GetOrg(ctx, orgID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "org not found"})
	}

	// Ensure Stripe customer exists
	customerID := ""
	if org.StripeCustomerID != nil {
		customerID = *org.StripeCustomerID
	}
	if customerID == "" {
		customerID, err = s.stripeClient.CreateCustomer(org.Name, "")
		if err != nil {
			log.Printf("billing: create customer failed for org %s: %v", orgID, err)
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to create customer"})
		}
		if err := s.store.SetStripeCustomerID(ctx, orgID, customerID); err != nil {
			log.Printf("billing: save customer ID failed for org %s: %v", orgID, err)
		}
	}

	url, _, err := s.stripeClient.CreateSetupCheckoutSession(customerID, orgID.String())
	if err != nil {
		log.Printf("billing: create checkout session failed for org %s: %v", orgID, err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to create checkout"})
	}
	return c.JSON(http.StatusOK, map[string]string{"url": url})
}

// billingGet returns the billing state for the current org.
func (s *Server) billingGet(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}
	ctx := c.Request().Context()
	org, err := s.store.GetOrg(ctx, orgID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "org not found"})
	}

	// Stripe balance for pro users
	var stripeCreditCents int64
	if org.Plan == "pro" && org.StripeCustomerID != nil && s.stripeClient != nil {
		bal, err := s.stripeClient.GetCustomerBalance(*org.StripeCustomerID)
		if err == nil {
			stripeCreditCents = -bal // negative balance = credit
		}
	}

	// Cost estimates are intentionally not returned from here. Billing truth
	// (usage, price, invoice totals) lives in Stripe — the dashboard links to
	// the Stripe Billing Portal instead of re-computing cost locally against a
	// hardcoded rate table that would diverge for grandfathered orgs.
	return c.JSON(http.StatusOK, map[string]interface{}{
		"plan":                       org.Plan,
		"stripeCreditCents":          stripeCreditCents,
		"maxConcurrentSandboxes":     org.MaxConcurrentSandboxes,
		"hasPaymentMethod":           org.StripeCustomerID != nil,
		"freeCreditsRemainingCents":  org.FreeCreditsRemainingCents,
	})
}

// billingPortal creates a Stripe Billing Portal session and returns the URL.
// The frontend opens this URL so the customer can view authoritative usage,
// invoices, and manage their payment method — all served directly by Stripe.
func (s *Server) billingPortal(c echo.Context) error {
	if s.store == nil || s.stripeClient == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "billing not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}
	ctx := c.Request().Context()
	org, err := s.store.GetOrg(ctx, orgID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "org not found"})
	}
	if org.StripeCustomerID == nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "no billing customer — upgrade to Pro first"})
	}

	// Send the user back to wherever they came from when they close the portal.
	returnURL := c.Request().Header.Get("Referer")
	url, err := s.stripeClient.CreatePortalSession(*org.StripeCustomerID, returnURL)
	if err != nil {
		log.Printf("billing: create portal session failed for org %s: %v", orgID, err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to create portal session"})
	}
	return c.JSON(http.StatusOK, map[string]string{"url": url})
}

// billingRedeem redeems a promotion code and applies the credit to the org's Stripe balance.
func (s *Server) billingRedeem(c echo.Context) error {
	if s.store == nil || s.stripeClient == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "billing not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := c.Bind(&req); err != nil || req.Code == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "code is required"})
	}

	ctx := c.Request().Context()
	org, err := s.store.GetOrg(ctx, orgID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "org not found"})
	}
	if org.StripeCustomerID == nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "billing not set up — upgrade to Pro first"})
	}

	amountCents, err := s.stripeClient.RedeemPromotionCode(*org.StripeCustomerID, req.Code)
	if err != nil {
		log.Printf("billing: redeem failed for org %s: %v", orgID, err)
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	log.Printf("billing: org %s redeemed promo code %q for $%.2f", orgID, req.Code, float64(amountCents)/100)
	return c.JSON(http.StatusOK, map[string]interface{}{
		"creditAppliedCents": amountCents,
	})
}

// billingInvoices returns past Stripe invoices.
func (s *Server) billingInvoices(c echo.Context) error {
	if s.store == nil || s.stripeClient == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}
	org, err := s.store.GetOrg(c.Request().Context(), orgID)
	if err != nil || org.StripeCustomerID == nil {
		return c.JSON(http.StatusOK, map[string]interface{}{"invoices": []interface{}{}})
	}

	limit := 10
	if l := c.QueryParam("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil && v > 0 && v <= 50 {
			limit = v
		}
	}

	invoices, err := s.stripeClient.ListInvoices(*org.StripeCustomerID, limit)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to list invoices"})
	}

	type inv struct {
		ID          string  `json:"id"`
		Number      string  `json:"number"`
		Status      string  `json:"status"`
		AmountDue   int64   `json:"amountDue"`
		AmountPaid  int64   `json:"amountPaid"`
		Currency    string  `json:"currency"`
		Created     int64   `json:"created"`
		HostedURL   string  `json:"hostedUrl"`
		PDFURL      string  `json:"pdfUrl"`
	}
	var result []inv
	for _, i := range invoices {
		hostedURL := ""
		pdfURL := ""
		if i.HostedInvoiceURL != "" {
			hostedURL = i.HostedInvoiceURL
		}
		if i.InvoicePDF != "" {
			pdfURL = i.InvoicePDF
		}
		result = append(result, inv{
			ID:         i.ID,
			Number:     i.Number,
			Status:     string(i.Status),
			AmountDue:  i.AmountDue,
			AmountPaid: i.AmountPaid,
			Currency:   string(i.Currency),
			Created:    i.Created,
			HostedURL:  hostedURL,
			PDFURL:     pdfURL,
		})
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"invoices": result})
}

// stripeWebhook handles Stripe webhook events.
func (s *Server) stripeWebhook(c echo.Context) error {
	if s.stripeClient == nil || s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "not configured"})
	}
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "read body failed"})
	}
	sig := c.Request().Header.Get("Stripe-Signature")
	event, err := s.stripeClient.VerifyWebhookSignature(body, sig)
	if err != nil {
		log.Printf("billing: webhook sig failed: %v", err)
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid signature"})
	}

	ctx := c.Request().Context()

	switch event.Type {
	case "checkout.session.completed":
		var sess struct {
			ID       string            `json:"id"`
			Mode     string            `json:"mode"`
			Customer string            `json:"customer"`
			Metadata map[string]string `json:"metadata"`
		}
		if err := json.Unmarshal(event.Data.Raw, &sess); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "bad event data"})
		}
		switch sess.Metadata["type"] {
		case "setup":
			orgIDStr := sess.Metadata["org_id"]
			orgID, err := uuid.Parse(orgIDStr)
			if err != nil {
				return c.JSON(http.StatusOK, map[string]string{"status": "ignored"})
			}

			// Create subscription with all metered prices
			subID, itemIDs, err := s.stripeClient.CreateSubscription(sess.Customer)
			if err != nil {
				log.Printf("billing: create subscription failed for org %s: %v", orgID, err)
				return c.JSON(http.StatusInternalServerError, map[string]string{"error": "subscription failed"})
			}
			if err := s.store.SetStripeSubscriptionID(ctx, orgID, subID); err != nil {
				log.Printf("billing: save subscription ID failed: %v", err)
			}
			if err := s.store.SaveSubscriptionItems(ctx, orgID, itemIDs); err != nil {
				log.Printf("billing: save subscription items failed: %v", err)
			}

			// Apply $30 promotional credit
			if err := s.stripeClient.ApplyPromotionalCredit(sess.Customer, 3000); err != nil {
				log.Printf("billing: apply credit failed for org %s: %v", orgID, err)
			}

			// Upgrade plan
			if err := s.store.UpdateOrgPlan(ctx, orgID, "pro"); err != nil {
				log.Printf("billing: upgrade plan failed for org %s: %v", orgID, err)
			}

			log.Printf("billing: org %s upgraded to pro (subscription=%s, $30 credit applied)", orgID, subID)

		case "agent_feature_subscription":
			// Customer paid for a per-agent feature via Checkout. The
			// Stripe-side subscription is already created (Checkout
			// mode=subscription); we just persist the local row so
			// the entitlement check finds it. Subscription metadata
			// carries (org_id, agent_id, feature).
			s.recordAgentFeatureCheckoutCompletion(ctx, sess.Metadata, event.Data.Raw)
		}

	case "customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted":
		s.handleAgentFeatureSubscriptionEvent(ctx, string(event.Type), event.Data.Raw)

	case "invoice.paid":
		log.Printf("billing: invoice paid: %s", string(event.Data.Raw)[:100])

	case "invoice.payment_failed":
		log.Printf("billing: invoice payment failed: %s", string(event.Data.Raw)[:100])
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

// recordAgentFeatureCheckoutCompletion persists a freshly-created
// per-agent feature subscription that was set up via Checkout. The
// Subscription object is already on Stripe; we just need to insert
// the local row. Metadata is propagated from the Checkout Session
// into the Subscription, so the subsequent customer.subscription.*
// events will also trigger handleAgentFeatureSubscriptionEvent which
// fills in current_period_end / status / etc.
func (s *Server) recordAgentFeatureCheckoutCompletion(ctx context.Context, metadata map[string]string, raw []byte) {
	orgID, err := uuid.Parse(metadata["org_id"])
	if err != nil {
		log.Printf("billing: agent_feature_subscription checkout missing org_id: %v", err)
		return
	}
	agentID := metadata["agent_id"]
	feature := metadata["feature"]
	if agentID == "" || feature == "" {
		log.Printf("billing: agent_feature_subscription checkout missing agent_id/feature in metadata")
		return
	}

	// Pull the subscription_id out of the checkout session payload —
	// Stripe attaches it after Checkout creates the subscription.
	var sess struct {
		Subscription string `json:"subscription"`
		Customer     string `json:"customer"`
	}
	if err := json.Unmarshal(raw, &sess); err != nil {
		log.Printf("billing: agent_feature_subscription checkout: unmarshal: %v", err)
		return
	}
	if sess.Subscription == "" {
		log.Printf("billing: agent_feature_subscription checkout has no subscription id")
		return
	}

	// Idempotent: skip if we already recorded it.
	existing, err := s.store.GetAgentSubscriptionByStripeID(ctx, sess.Subscription)
	if err == nil && existing != nil {
		return
	}

	priceID := ""
	if s.stripeClient != nil {
		switch feature {
		case "telegram":
			priceID = s.stripeClient.TelegramAgentPriceID
		}
	}

	if _, err := s.store.CreateAgentSubscription(ctx, db.AgentSubscription{
		OrgID:                orgID,
		AgentID:              agentID,
		Feature:              feature,
		StripeCustomerID:     sess.Customer,
		StripeSubscriptionID: sess.Subscription,
		StripePriceID:        priceID,
		Status:               "incomplete", // will be updated by subscription.updated event
	}); err != nil {
		log.Printf("billing: persist agent_subscription from checkout failed: %v", err)
	} else {
		log.Printf("billing: agent_feature_subscription recorded: agent=%s feature=%s sub=%s", agentID, feature, sess.Subscription)
	}
}

// handleAgentFeatureSubscriptionEvent reconciles status on every
// customer.subscription.* event. We filter to subscriptions that we
// have a local row for — anything else is the org-level pro-plan
// subscription which is handled elsewhere.
//
// `customer.subscription.deleted` fires when Stripe finally removes
// the subscription (after period end if cancel_at_period_end was
// scheduled, or immediately for a hard cancel). At that point we
// flip the local row to canceled and disconnect any active Telegram
// webhook so the user actually loses access.
func (s *Server) handleAgentFeatureSubscriptionEvent(ctx context.Context, eventType string, raw []byte) {
	var sub struct {
		ID                string            `json:"id"`
		Status            string            `json:"status"`
		CancelAtPeriodEnd bool              `json:"cancel_at_period_end"`
		CanceledAt        *int64            `json:"canceled_at"`
		Metadata          map[string]string `json:"metadata"`
		Items             struct {
			Data []struct {
				CurrentPeriodEnd int64 `json:"current_period_end"`
			} `json:"data"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &sub); err != nil {
		log.Printf("billing: subscription event unmarshal: %v", err)
		return
	}

	row, err := s.store.GetAgentSubscriptionByStripeID(ctx, sub.ID)
	if err != nil {
		log.Printf("billing: lookup agent_subscription %s failed: %v", sub.ID, err)
		return
	}
	if row == nil {
		// Not a per-agent subscription — ignore.
		return
	}

	var currentPeriodEnd *time.Time
	if len(sub.Items.Data) > 0 && sub.Items.Data[0].CurrentPeriodEnd > 0 {
		t := time.Unix(sub.Items.Data[0].CurrentPeriodEnd, 0).UTC()
		currentPeriodEnd = &t
	}
	var canceledAt *time.Time
	if sub.CanceledAt != nil && *sub.CanceledAt > 0 {
		t := time.Unix(*sub.CanceledAt, 0).UTC()
		canceledAt = &t
	}

	if err := s.store.UpdateAgentSubscriptionFromStripe(
		ctx, sub.ID, sub.Status, currentPeriodEnd, sub.CancelAtPeriodEnd, canceledAt,
	); err != nil {
		log.Printf("billing: update agent_subscription %s failed: %v", sub.ID, err)
		return
	}

	// On terminal statuses, the entitlement check will start denying
	// new connect attempts immediately. We deliberately don't auto-
	// disconnect a currently-connected Telegram channel from here —
	// that side-effect needs an authenticated call into sessions-api,
	// and the cleanest way to do it is from the user-facing flow
	// (e.g. show "subscription expired" in the UI and let them click
	// Disconnect, or have sessions-api re-check entitlement on each
	// inbound webhook). Phase-1 leaves this as a "soft" gate.
	if eventType == "customer.subscription.deleted" {
		log.Printf("billing: agent_subscription terminated: agent=%s feature=%s sub=%s status=%s",
			row.AgentID, row.Feature, sub.ID, sub.Status)
	}
}
