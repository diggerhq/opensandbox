package api

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/billing"
)

// billingCreateCheckoutSession creates a Stripe Checkout session for purchasing credits.
func (s *Server) billingCreateCheckoutSession(c echo.Context) error {
	if s.store == nil || s.stripeClient == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "billing not configured"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	var req struct {
		AmountCents int64 `json:"amountCents"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	if req.AmountCents < 500 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "minimum purchase is $5.00 (500 cents)"})
	}

	ctx := c.Request().Context()
	org, err := s.store.GetOrg(ctx, orgID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "org not found"})
	}

	// Ensure org has a Stripe customer
	customerID := ""
	if org.StripeCustomerID != nil {
		customerID = *org.StripeCustomerID
	}
	if customerID == "" {
		email := "" // Org-level email not always available
		customerID, err = s.stripeClient.CreateCustomer(org.Name, email)
		if err != nil {
			log.Printf("billing: failed to create Stripe customer for org %s: %v", orgID, err)
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to create payment customer"})
		}
		if err := s.store.SetStripeCustomerID(ctx, orgID, customerID); err != nil {
			log.Printf("billing: failed to save Stripe customer ID for org %s: %v", orgID, err)
		}
	}

	url, sessionID, err := s.stripeClient.CreateCheckoutSession(customerID, req.AmountCents, orgID.String())
	if err != nil {
		log.Printf("billing: failed to create checkout session for org %s: %v", orgID, err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to create checkout session"})
	}

	return c.JSON(http.StatusOK, map[string]string{
		"url":       url,
		"sessionId": sessionID,
	})
}

// billingSetupPaymentMethod creates a Stripe Checkout session for adding a card only.
func (s *Server) billingSetupPaymentMethod(c echo.Context) error {
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

	customerID := ""
	if org.StripeCustomerID != nil {
		customerID = *org.StripeCustomerID
	}
	if customerID == "" {
		customerID, err = s.stripeClient.CreateCustomer(org.Name, "")
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to create payment customer"})
		}
		if err := s.store.SetStripeCustomerID(ctx, orgID, customerID); err != nil {
			log.Printf("billing: failed to save Stripe customer ID for org %s: %v", orgID, err)
		}
	}

	url, sessionID, err := s.stripeClient.CreateSetupCheckoutSession(customerID, orgID.String())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to create setup session"})
	}

	return c.JSON(http.StatusOK, map[string]string{
		"url":       url,
		"sessionId": sessionID,
	})
}

// billingGetSettings returns the org's billing settings.
func (s *Server) billingGetSettings(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "not configured"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	info, err := s.store.GetOrgBillingInfo(c.Request().Context(), orgID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to load billing info"})
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"autoTopupEnabled":        info.AutoTopupEnabled,
		"autoTopupThresholdCents": info.AutoTopupThresholdCents,
		"autoTopupAmountCents":    info.AutoTopupAmountCents,
		"monthlySpendCapCents":    info.MonthlySpendCapCents,
		"creditBalanceCents":      info.CreditBalanceCents,
		"hasPaymentMethod":        info.StripeCustomerID != nil,
	})
}

// billingUpdateSettings updates the org's billing settings.
func (s *Server) billingUpdateSettings(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "not configured"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	var req struct {
		AutoTopupEnabled        bool `json:"autoTopupEnabled"`
		AutoTopupThresholdCents int  `json:"autoTopupThresholdCents"`
		AutoTopupAmountCents    int  `json:"autoTopupAmountCents"`
		MonthlySpendCapCents    *int `json:"monthlySpendCapCents"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}

	if req.AutoTopupAmountCents < 500 && req.AutoTopupEnabled {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "minimum auto top-up amount is $5.00"})
	}

	err := s.store.UpdateOrgBillingSettings(c.Request().Context(), orgID,
		req.AutoTopupEnabled, req.AutoTopupThresholdCents, req.AutoTopupAmountCents, req.MonthlySpendCapCents)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to update settings"})
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

// billingGetUsage returns per-tier usage cost breakdown.
func (s *Server) billingGetUsage(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "not configured"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	// Default: current month
	now := time.Now()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	to := now

	if f := c.QueryParam("from"); f != "" {
		if t, err := time.Parse(time.RFC3339, f); err == nil {
			from = t
		}
	}
	if t := c.QueryParam("to"); t != "" {
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			to = parsed
		}
	}

	usage, err := s.store.GetOrgUsage(c.Request().Context(), orgID.String(), from, to)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to get usage"})
	}

	type tierCost struct {
		MemoryMB     int     `json:"memoryMB"`
		VCPUs        int     `json:"vcpus"`
		TotalSeconds float64 `json:"totalSeconds"`
		RatePerSec   float64 `json:"ratePerSecond"`
		CostCents    float64 `json:"costCents"`
	}

	var tiers []tierCost
	var totalCostCents float64
	for _, u := range usage {
		rate := billing.TierPricePerSecond[u.MemoryMB]
		cost := u.TotalSeconds * rate * 100.0
		totalCostCents += cost
		tiers = append(tiers, tierCost{
			MemoryMB:     u.MemoryMB,
			VCPUs:        u.CPUPercent / 100,
			TotalSeconds: u.TotalSeconds,
			RatePerSec:   rate,
			CostCents:    cost,
		})
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"from":           from,
		"to":             to,
		"tiers":          tiers,
		"totalCostCents": totalCostCents,
	})
}

// billingGetTransactions returns paginated credit transaction history.
func (s *Server) billingGetTransactions(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "not configured"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	limit := 20
	offset := 0
	if l := c.QueryParam("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil && v > 0 && v <= 100 {
			limit = v
		}
	}
	if o := c.QueryParam("offset"); o != "" {
		if v, err := strconv.Atoi(o); err == nil && v >= 0 {
			offset = v
		}
	}

	txns, err := s.store.GetCreditTransactions(c.Request().Context(), orgID, limit, offset)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to get transactions"})
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"transactions": txns,
		"limit":        limit,
		"offset":       offset,
	})
}

// stripeWebhook handles Stripe webhook events.
func (s *Server) stripeWebhook(c echo.Context) error {
	if s.stripeClient == nil || s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "not configured"})
	}

	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "failed to read body"})
	}

	sigHeader := c.Request().Header.Get("Stripe-Signature")
	event, err := s.stripeClient.VerifyWebhookSignature(body, sigHeader)
	if err != nil {
		log.Printf("billing: webhook signature verification failed: %v", err)
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid signature"})
	}

	ctx := c.Request().Context()

	switch event.Type {
	case "checkout.session.completed":
		var session struct {
			ID       string            `json:"id"`
			Mode     string            `json:"mode"`
			Metadata map[string]string `json:"metadata"`
			PaymentIntent string       `json:"payment_intent"`
		}
		if err := json.Unmarshal(event.Data.Raw, &session); err != nil {
			log.Printf("billing: failed to parse checkout session: %v", err)
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid event data"})
		}

		// Only process payment mode (credit purchase), not setup mode
		if session.Mode == "payment" {
			orgIDStr := session.Metadata["org_id"]
			amountStr := session.Metadata["amount_cents"]
			orgID, err := uuid.Parse(orgIDStr)
			if err != nil {
				log.Printf("billing: invalid org_id in checkout metadata: %s", orgIDStr)
				return c.JSON(http.StatusOK, map[string]string{"status": "ignored"})
			}
			amountCents, _ := strconv.Atoi(amountStr)
			if amountCents <= 0 {
				log.Printf("billing: invalid amount_cents in checkout metadata: %s", amountStr)
				return c.JSON(http.StatusOK, map[string]string{"status": "ignored"})
			}

			err = s.store.AddCredits(ctx, orgID, amountCents, "purchase",
				"Credit purchase via Stripe Checkout", session.PaymentIntent, session.ID)
			if err != nil {
				log.Printf("billing: failed to add credits for org %s: %v", orgID, err)
				return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to add credits"})
			}
			log.Printf("billing: added %d cents to org %s via checkout %s", amountCents, orgID, session.ID)
		}

	case "payment_intent.succeeded":
		var pi struct {
			ID       string            `json:"id"`
			Metadata map[string]string `json:"metadata"`
		}
		if err := json.Unmarshal(event.Data.Raw, &pi); err != nil {
			log.Printf("billing: failed to parse payment intent: %v", err)
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid event data"})
		}

		// Auto top-ups include metadata
		orgIDStr := pi.Metadata["org_id"]
		amountStr := pi.Metadata["amount_cents"]
		txnType := pi.Metadata["type"]
		if txnType == "auto_topup" && orgIDStr != "" && amountStr != "" {
			orgID, err := uuid.Parse(orgIDStr)
			if err != nil {
				return c.JSON(http.StatusOK, map[string]string{"status": "ignored"})
			}
			amountCents, _ := strconv.Atoi(amountStr)
			if amountCents > 0 {
				err = s.store.AddCredits(ctx, orgID, amountCents, "auto_topup",
					"Auto top-up", pi.ID, "")
				if err != nil {
					log.Printf("billing: failed to add auto-topup credits for org %s: %v", orgID, err)
				} else {
					log.Printf("billing: auto-topup %d cents for org %s via PI %s", amountCents, orgID, pi.ID)
				}
			}
		}

	case "payment_intent.payment_failed":
		var pi struct {
			ID       string            `json:"id"`
			Metadata map[string]string `json:"metadata"`
		}
		if err := json.Unmarshal(event.Data.Raw, &pi); err == nil {
			log.Printf("billing: payment failed for PI %s (org: %s)", pi.ID, pi.Metadata["org_id"])
		}
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}
