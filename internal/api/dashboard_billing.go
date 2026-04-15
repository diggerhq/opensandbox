package api

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/auth"
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
		"plan":                   org.Plan,
		"stripeCreditCents":      stripeCreditCents,
		"maxConcurrentSandboxes": org.MaxConcurrentSandboxes,
		"hasPaymentMethod":       org.StripeCustomerID != nil,
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
		if sess.Metadata["type"] == "setup" {
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
		}

	case "invoice.paid":
		log.Printf("billing: invoice paid: %s", string(event.Data.Raw)[:100])

	case "invoice.payment_failed":
		log.Printf("billing: invoice payment failed: %s", string(event.Data.Raw)[:100])
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}
