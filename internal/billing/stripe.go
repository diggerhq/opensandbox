package billing

import (
	"fmt"
	"log"
	"math"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/billing/meter"
	"github.com/stripe/stripe-go/v82/billing/meterevent"
	portalsession "github.com/stripe/stripe-go/v82/billingportal/session"
	checkoutsession "github.com/stripe/stripe-go/v82/checkout/session"
	"github.com/stripe/stripe-go/v82/customer"
	"github.com/stripe/stripe-go/v82/customerbalancetransaction"
	"github.com/stripe/stripe-go/v82/invoice"
	"github.com/stripe/stripe-go/v82/price"
	"github.com/stripe/stripe-go/v82/product"
	"github.com/stripe/stripe-go/v82/promotioncode"
	"github.com/stripe/stripe-go/v82/subscription"
	"github.com/stripe/stripe-go/v82/webhook"
)

// StripeClient wraps the Stripe API for billing.
type StripeClient struct {
	webhookSecret string
	successURL    string
	cancelURL     string

	// Populated by EnsureProducts — maps memoryMB → Stripe price ID
	PriceIDs map[int]string
	// Maps memoryMB → meter event_name (e.g. "sandbox_compute_4gb")
	MeterEventNames map[int]string

	// Disk overage meter / price (single dimension: GB-seconds above 20GB).
	DiskOveragePriceID       string
	DiskOverageMeterEventName string

	// Phase-3 unified-pipeline meters and prices. Two flat meters
	// (overage + reserved) used by new orgs (`billing_mode='unified'`).
	// Legacy per-tier meters above are untouched and continue to serve
	// existing orgs via UsageReporter.
	OveragePriceID         string // flat overage Price for `overage_usage` events
	OverageMeterEventName  string // "sandbox_compute_sandbox_overage"
	ReservedPriceID        string // flat reserved Price for `reserved_usage` events
	ReservedMeterEventName string // "sandbox_compute_sandbox_reserved"
}

// NewStripeClient creates a new Stripe client.
func NewStripeClient(secretKey, webhookSecret, successURL, cancelURL string) *StripeClient {
	stripe.Key = secretKey
	return &StripeClient{
		webhookSecret:   webhookSecret,
		successURL:      successURL,
		cancelURL:       cancelURL,
		PriceIDs:        make(map[int]string),
		MeterEventNames: make(map[int]string),
	}
}

// EnsureProducts creates Billing Meters, a Product, and metered Prices if they don't exist.
// This is idempotent — uses metadata to find existing resources.
func (s *StripeClient) EnsureProducts() error {
	// 1. Find or create meters (one per tier)
	meterIDs := make(map[int]string) // memoryMB → meter ID
	existingMeters := make(map[string]*stripe.BillingMeter)
	iter := meter.List(&stripe.BillingMeterListParams{
		Status: stripe.String(string(stripe.BillingMeterStatusActive)),
	})
	for iter.Next() {
		m := iter.BillingMeter()
		existingMeters[m.EventName] = m
	}

	for memMB, meterKey := range TierMeterKey {
		eventName := "sandbox_compute_" + meterKey
		s.MeterEventNames[memMB] = eventName

		if m, ok := existingMeters[eventName]; ok {
			meterIDs[memMB] = m.ID
			log.Printf("billing: found existing meter %s (id=%s)", eventName, m.ID)
			continue
		}

		m, err := meter.New(&stripe.BillingMeterParams{
			DisplayName: stripe.String(fmt.Sprintf("Sandbox Compute %s", meterKey)),
			EventName:   stripe.String(eventName),
			DefaultAggregation: &stripe.BillingMeterDefaultAggregationParams{
				Formula: stripe.String(string(stripe.BillingMeterDefaultAggregationFormulaSum)),
			},
			CustomerMapping: &stripe.BillingMeterCustomerMappingParams{
				EventPayloadKey: stripe.String("stripe_customer_id"),
				Type:            stripe.String(string(stripe.BillingMeterCustomerMappingTypeByID)),
			},
			ValueSettings: &stripe.BillingMeterValueSettingsParams{
				EventPayloadKey: stripe.String("value"),
			},
		})
		if err != nil {
			return fmt.Errorf("create meter %s: %w", eventName, err)
		}
		meterIDs[memMB] = m.ID
		log.Printf("billing: created meter %s (id=%s)", eventName, m.ID)
	}

	// 2. Find or create product
	productID := ""
	prodIter := product.List(&stripe.ProductListParams{})
	for prodIter.Next() {
		p := prodIter.Product()
		if p.Metadata["opensandbox"] == "compute" {
			productID = p.ID
			break
		}
	}
	if productID == "" {
		p, err := product.New(&stripe.ProductParams{
			Name:     stripe.String("Sandbox Compute"),
			Metadata: map[string]string{"opensandbox": "compute"},
		})
		if err != nil {
			return fmt.Errorf("create product: %w", err)
		}
		productID = p.ID
		log.Printf("billing: created product %s", productID)
	}

	// 3. Find or create prices (one per tier, linked to meter)
	existingPrices := make(map[string]string) // tier metadata key → price ID
	priceIter := price.List(&stripe.PriceListParams{
		Product: stripe.String(productID),
	})
	for priceIter.Next() {
		p := priceIter.Price()
		if key, ok := p.Metadata["tier"]; ok {
			existingPrices[key] = p.ID
		}
	}

	for memMB, priceKey := range TierPriceKey {
		if id, ok := existingPrices[priceKey]; ok {
			s.PriceIDs[memMB] = id
			log.Printf("billing: found existing price for %s (id=%s)", priceKey, id)
			continue
		}

		meterID := meterIDs[memMB]
		// Truncate to 12 decimal places to avoid Stripe precision errors
		ratePerSecondCents := TierPricePerSecond[memMB] * 100
		truncated := math.Floor(ratePerSecondCents*1e12) / 1e12

		p, err := price.New(&stripe.PriceParams{
			Product:           stripe.String(productID),
			Currency:          stripe.String("usd"),
			UnitAmountDecimal: stripe.Float64(truncated),
			BillingScheme:     stripe.String(string(stripe.PriceBillingSchemePerUnit)),
			Recurring: &stripe.PriceRecurringParams{
				Interval:  stripe.String(string(stripe.PriceRecurringIntervalMonth)),
				UsageType: stripe.String(string(stripe.PriceRecurringUsageTypeMetered)),
				Meter:     stripe.String(meterID),
			},
			Metadata: map[string]string{
				"tier":        priceKey,
				"memory_mb":   fmt.Sprintf("%d", memMB),
				"opensandbox": "compute",
			},
		})
		if err != nil {
			return fmt.Errorf("create price for %s: %w", priceKey, err)
		}
		s.PriceIDs[memMB] = p.ID
		log.Printf("billing: created price for %s (id=%s)", priceKey, p.ID)
	}

	// 4. Disk overage meter + price (single dimension, billed per GB-second above 20GB).
	diskEventName := "sandbox_compute_" + DiskOverageMetadataKey
	s.DiskOverageMeterEventName = diskEventName

	var diskMeterID string
	if m, ok := existingMeters[diskEventName]; ok {
		diskMeterID = m.ID
		log.Printf("billing: found existing disk overage meter %s (id=%s)", diskEventName, diskMeterID)
	} else {
		m, err := meter.New(&stripe.BillingMeterParams{
			DisplayName: stripe.String("Sandbox Disk Overage (GB-seconds)"),
			EventName:   stripe.String(diskEventName),
			DefaultAggregation: &stripe.BillingMeterDefaultAggregationParams{
				Formula: stripe.String(string(stripe.BillingMeterDefaultAggregationFormulaSum)),
			},
			CustomerMapping: &stripe.BillingMeterCustomerMappingParams{
				EventPayloadKey: stripe.String("stripe_customer_id"),
				Type:            stripe.String(string(stripe.BillingMeterCustomerMappingTypeByID)),
			},
			ValueSettings: &stripe.BillingMeterValueSettingsParams{
				EventPayloadKey: stripe.String("value"),
			},
		})
		if err != nil {
			return fmt.Errorf("create disk overage meter: %w", err)
		}
		diskMeterID = m.ID
		log.Printf("billing: created disk overage meter %s (id=%s)", diskEventName, diskMeterID)
	}

	if id, ok := existingPrices[DiskOverageMetadataKey]; ok {
		s.DiskOveragePriceID = id
		log.Printf("billing: found existing disk overage price (id=%s)", id)
	} else {
		ratePerGBPerSecondCents := DiskOveragePricePerGBPerSecond * 100
		truncated := math.Floor(ratePerGBPerSecondCents*1e12) / 1e12

		p, err := price.New(&stripe.PriceParams{
			Product:           stripe.String(productID),
			Currency:          stripe.String("usd"),
			UnitAmountDecimal: stripe.Float64(truncated),
			BillingScheme:     stripe.String(string(stripe.PriceBillingSchemePerUnit)),
			Recurring: &stripe.PriceRecurringParams{
				Interval:  stripe.String(string(stripe.PriceRecurringIntervalMonth)),
				UsageType: stripe.String(string(stripe.PriceRecurringUsageTypeMetered)),
				Meter:     stripe.String(diskMeterID),
			},
			Metadata: map[string]string{
				"tier":        DiskOverageMetadataKey,
				"opensandbox": "compute",
				"unit":        "gb_second_above_20gb",
			},
		})
		if err != nil {
			return fmt.Errorf("create disk overage price: %w", err)
		}
		s.DiskOveragePriceID = p.ID
		log.Printf("billing: created disk overage price (id=%s)", p.ID)
	}

	// 5. Phase-3 unified-pipeline meters. Two flat meters (overage +
	//    reserved) at unit GB-seconds. Code creates the *meters* (their
	//    event names are stable wire-protocol coupling) but **does not
	//    create Prices** — those are configured in the Stripe Dashboard
	//    so pricing changes don't need a code deploy. The Price IDs
	//    are discovered below if present; if missing, meter events
	//    still flow but won't appear on invoices until a Price is
	//    linked to the meter in Stripe.
	s.OverageMeterEventName = ensureMeter(existingMeters, "sandbox_compute_"+OverageMeterKey, "Sandbox Instant Compute (GB-seconds)")
	s.ReservedMeterEventName = ensureMeter(existingMeters, "sandbox_compute_"+ReservedMeterKey, "Sandbox Reserved Capacity (GB-seconds)")

	if id, ok := existingPrices[OveragePriceKey]; ok {
		s.OveragePriceID = id
		log.Printf("billing: found existing overage price (id=%s)", id)
	} else {
		log.Printf("billing: no overage price configured for meter %s — meter events will flow but won't appear on invoices until a Price is created in Stripe", s.OverageMeterEventName)
	}
	if id, ok := existingPrices[ReservedPriceKey]; ok {
		s.ReservedPriceID = id
		log.Printf("billing: found existing reserved price (id=%s)", id)
	} else {
		log.Printf("billing: no reserved price configured for meter %s — meter events will flow but won't appear on invoices until a Price is created in Stripe", s.ReservedMeterEventName)
	}

	return nil
}

// ensureMeter is a small helper that returns the event_name after
// creating or finding the meter. Used only for the phase-3 flat
// meters; legacy per-tier meters retain their inline creation so the
// unrelated stripe.go diff stays small.
func ensureMeter(existing map[string]*stripe.BillingMeter, eventName, displayName string) string {
	if m, ok := existing[eventName]; ok {
		log.Printf("billing: found existing meter %s (id=%s)", eventName, m.ID)
		return eventName
	}
	m, err := meter.New(&stripe.BillingMeterParams{
		DisplayName: stripe.String(displayName),
		EventName:   stripe.String(eventName),
		DefaultAggregation: &stripe.BillingMeterDefaultAggregationParams{
			Formula: stripe.String(string(stripe.BillingMeterDefaultAggregationFormulaSum)),
		},
		CustomerMapping: &stripe.BillingMeterCustomerMappingParams{
			EventPayloadKey: stripe.String("stripe_customer_id"),
			Type:            stripe.String(string(stripe.BillingMeterCustomerMappingTypeByID)),
		},
		ValueSettings: &stripe.BillingMeterValueSettingsParams{
			EventPayloadKey: stripe.String("value"),
		},
	})
	if err != nil {
		log.Printf("billing: WARN failed to create meter %s: %v", eventName, err)
		return eventName // return name anyway; sender will fail loudly if used
	}
	log.Printf("billing: created meter %s (id=%s)", eventName, m.ID)
	return eventName
}

// CreateCustomer creates a Stripe customer for an org.
func (s *StripeClient) CreateCustomer(orgName, email string) (string, error) {
	params := &stripe.CustomerParams{
		Name: stripe.String(orgName),
	}
	if email != "" {
		params.Email = stripe.String(email)
	}
	c, err := customer.New(params)
	if err != nil {
		return "", fmt.Errorf("create customer: %w", err)
	}
	return c.ID, nil
}

// CreateSetupCheckoutSession creates a Checkout session for adding a payment method.
func (s *StripeClient) CreateSetupCheckoutSession(customerID, orgID string) (url, sessionID string, err error) {
	params := &stripe.CheckoutSessionParams{
		Customer:   stripe.String(customerID),
		Mode:       stripe.String(string(stripe.CheckoutSessionModeSetup)),
		Currency:   stripe.String("usd"),
		SuccessURL: stripe.String(s.successURL),
		CancelURL:  stripe.String(s.cancelURL),
		Metadata: map[string]string{
			"org_id": orgID,
			"type":   "setup",
		},
	}
	sess, err := checkoutsession.New(params)
	if err != nil {
		return "", "", fmt.Errorf("create setup session: %w", err)
	}
	return sess.URL, sess.ID, nil
}

// CreatePortalSession creates a Stripe Billing Portal session so the customer
// can self-serve: view subscription, usage, invoices, and update payment method.
// returnURL is where Stripe sends the user after they close the portal.
func (s *StripeClient) CreatePortalSession(customerID, returnURL string) (string, error) {
	sess, err := portalsession.New(&stripe.BillingPortalSessionParams{
		Customer:  stripe.String(customerID),
		ReturnURL: stripe.String(returnURL),
	})
	if err != nil {
		return "", fmt.Errorf("create portal session: %w", err)
	}
	return sess.URL, nil
}

// CreateSubscription creates a subscription with metered prices for all tiers.
// Returns subscription ID and map of memoryMB → subscription item ID
// (legacy per-second tier items only; phase-3 overage/reserved items
// are added to the subscription but not returned in the map since
// `org_subscription_items` only tracks the legacy mapping).
func (s *StripeClient) CreateSubscription(customerID string) (string, map[int]string, error) {
	var items []*stripe.SubscriptionItemsParams
	for _, priceID := range s.PriceIDs {
		items = append(items, &stripe.SubscriptionItemsParams{
			Price: stripe.String(priceID),
		})
	}
	if s.DiskOveragePriceID != "" {
		items = append(items, &stripe.SubscriptionItemsParams{
			Price: stripe.String(s.DiskOveragePriceID),
		})
	}
	// Phase-3 unified-pipeline items: flat overage + flat reserved.
	// New orgs default to `billing_mode='unified'` so meter events
	// flow to these items. Existing legacy orgs sit at zero usage
	// on these line items (Stripe omits zero-usage rows from invoices,
	// so they're invisible to the customer).
	if s.OveragePriceID != "" {
		items = append(items, &stripe.SubscriptionItemsParams{
			Price: stripe.String(s.OveragePriceID),
		})
	}
	if s.ReservedPriceID != "" {
		items = append(items, &stripe.SubscriptionItemsParams{
			Price: stripe.String(s.ReservedPriceID),
		})
	}

	sub, err := subscription.New(&stripe.SubscriptionParams{
		Customer: stripe.String(customerID),
		Items:    items,
	})
	if err != nil {
		return "", nil, fmt.Errorf("create subscription: %w", err)
	}

	// Map subscription items back to memory tiers
	itemIDs := make(map[int]string)
	for _, si := range sub.Items.Data {
		if si.Price != nil {
			for memMB, priceID := range s.PriceIDs {
				if si.Price.ID == priceID {
					itemIDs[memMB] = si.ID
					break
				}
			}
		}
	}

	return sub.ID, itemIDs, nil
}

// ApplyPromotionalCredit sets a negative customer balance (credit).
func (s *StripeClient) ApplyPromotionalCredit(customerID string, amountCents int64) error {
	_, err := customer.Update(customerID, &stripe.CustomerParams{
		Balance: stripe.Int64(-amountCents),
	})
	if err != nil {
		return fmt.Errorf("apply credit: %w", err)
	}
	return nil
}

// ReportUsage sends a meter event for a specific tier.
// customerID is the Stripe customer ID. seconds is the usage quantity.
func (s *StripeClient) ReportUsage(customerID string, memoryMB int, seconds int64, timestamp int64) error {
	eventName, ok := s.MeterEventNames[memoryMB]
	if !ok {
		return fmt.Errorf("no meter for tier %dMB", memoryMB)
	}

	_, err := meterevent.New(&stripe.BillingMeterEventParams{
		EventName: stripe.String(eventName),
		Timestamp: stripe.Int64(timestamp),
		Payload: map[string]string{
			"stripe_customer_id": customerID,
			"value":              fmt.Sprintf("%d", seconds),
		},
	})
	if err != nil {
		return fmt.Errorf("report usage for %s: %w", eventName, err)
	}
	return nil
}

// ReportMeterEvent sends a single Stripe meter event with an
// idempotency identifier. Used by the phase-3 sender to ship outbox
// rows; the identifier (typically `billable_events.id`) makes
// at-least-once shipping safe — Stripe dedups repeated submissions
// of the same identifier within 24h.
//
// Returns the resulting BillingMeterEvent.Identifier echo'd by Stripe
// (which equals the input on successful submission). Caller stores
// this in `billable_events.stripe_event_id` for traceability.
func (s *StripeClient) ReportMeterEvent(eventName, customerID string, value float64, identifier string, timestamp int64) (string, error) {
	resp, err := meterevent.New(&stripe.BillingMeterEventParams{
		EventName:  stripe.String(eventName),
		Identifier: stripe.String(identifier),
		Timestamp:  stripe.Int64(timestamp),
		Payload: map[string]string{
			"stripe_customer_id": customerID,
			// Stripe accepts string-encoded values; use a high-precision
			// format so fractional GB-seconds (the proportional split
			// produces them) don't get truncated to integers.
			"value": fmt.Sprintf("%.6f", value),
		},
	})
	if err != nil {
		return "", fmt.Errorf("report meter event %s: %w", eventName, err)
	}
	return resp.Identifier, nil
}

// ReportDiskOverageUsage sends a meter event for disk overage GB-seconds
// (provisioned disk above 20GB, integrated over time).
func (s *StripeClient) ReportDiskOverageUsage(customerID string, gbSeconds int64, timestamp int64) error {
	if s.DiskOverageMeterEventName == "" {
		return fmt.Errorf("disk overage meter not provisioned")
	}
	_, err := meterevent.New(&stripe.BillingMeterEventParams{
		EventName: stripe.String(s.DiskOverageMeterEventName),
		Timestamp: stripe.Int64(timestamp),
		Payload: map[string]string{
			"stripe_customer_id": customerID,
			"value":              fmt.Sprintf("%d", gbSeconds),
		},
	})
	if err != nil {
		return fmt.Errorf("report disk overage: %w", err)
	}
	return nil
}

// GetCustomerBalance returns the customer's balance in cents (negative = credit).
func (s *StripeClient) GetCustomerBalance(customerID string) (int64, error) {
	c, err := customer.Get(customerID, nil)
	if err != nil {
		return 0, err
	}
	return c.Balance, nil
}

// ListInvoices returns past invoices for a customer.
func (s *StripeClient) ListInvoices(customerID string, limit int) ([]*stripe.Invoice, error) {
	params := &stripe.InvoiceListParams{
		Customer: stripe.String(customerID),
	}
	params.Filters.AddFilter("limit", "", fmt.Sprintf("%d", limit))

	var invoices []*stripe.Invoice
	iter := invoice.List(params)
	for iter.Next() {
		invoices = append(invoices, iter.Invoice())
	}
	return invoices, iter.Err()
}

// GetUpcomingInvoice returns a preview of the upcoming invoice.
func (s *StripeClient) GetUpcomingInvoice(customerID string) (*stripe.Invoice, error) {
	return invoice.CreatePreview(&stripe.InvoiceCreatePreviewParams{
		Customer: stripe.String(customerID),
	})
}

// RedeemPromotionCode validates a promotion code and applies the credit to the customer's balance.
// Returns the credit amount in cents.
func (s *StripeClient) RedeemPromotionCode(customerID, code string) (int64, error) {
	// Look up the promotion code
	iter := promotioncode.List(&stripe.PromotionCodeListParams{
		Code:   stripe.String(code),
		Active: stripe.Bool(true),
	})
	if !iter.Next() {
		return 0, fmt.Errorf("invalid or expired promotion code")
	}
	promo := iter.PromotionCode()

	// Check redemption limits
	if promo.MaxRedemptions > 0 && promo.TimesRedeemed >= promo.MaxRedemptions {
		return 0, fmt.Errorf("promotion code has already been redeemed")
	}

	// Check if restricted to a specific customer
	if promo.Customer != nil && promo.Customer.ID != customerID {
		return 0, fmt.Errorf("promotion code is not valid for this account")
	}

	// Get credit amount from coupon
	if promo.Coupon == nil || promo.Coupon.AmountOff == 0 {
		return 0, fmt.Errorf("promotion code has no credit amount")
	}
	amountCents := promo.Coupon.AmountOff

	// Deactivate first to prevent concurrent redemptions
	_, err := promotioncode.Update(promo.ID, &stripe.PromotionCodeParams{
		Active: stripe.Bool(false),
	})
	if err != nil {
		return 0, fmt.Errorf("failed to redeem promotion code: %w", err)
	}

	// Apply credit to customer balance (negative = credit in Stripe)
	_, err = customerbalancetransaction.New(&stripe.CustomerBalanceTransactionParams{
		Customer:    stripe.String(customerID),
		Amount:      stripe.Int64(-amountCents),
		Currency:    stripe.String("usd"),
		Description: stripe.String(fmt.Sprintf("Promotion code: %s", code)),
	})
	if err != nil {
		// Re-activate the code since credit application failed
		if _, reErr := promotioncode.Update(promo.ID, &stripe.PromotionCodeParams{
			Active: stripe.Bool(true),
		}); reErr != nil {
			log.Printf("billing: CRITICAL: promo code %s deactivated but credit failed — manual fix needed: %v", promo.ID, reErr)
		}
		return 0, fmt.Errorf("apply credit: %w", err)
	}

	return amountCents, nil
}

// VerifyWebhookSignature verifies a Stripe webhook event.
func (s *StripeClient) VerifyWebhookSignature(payload []byte, sigHeader string) (*stripe.Event, error) {
	event, err := webhook.ConstructEventWithOptions(payload, sigHeader, s.webhookSecret, webhook.ConstructEventOptions{
		IgnoreAPIVersionMismatch: true,
	})
	if err != nil {
		return nil, err
	}
	return &event, nil
}
