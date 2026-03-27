package billing

import (
	"fmt"
	"log"
	"math"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/billing/meter"
	"github.com/stripe/stripe-go/v82/billing/meterevent"
	checkoutsession "github.com/stripe/stripe-go/v82/checkout/session"
	"github.com/stripe/stripe-go/v82/customer"
	"github.com/stripe/stripe-go/v82/invoice"
	"github.com/stripe/stripe-go/v82/price"
	"github.com/stripe/stripe-go/v82/product"
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

	for memMB, metaKey := range TierMetadataKey {
		eventName := "sandbox_compute_" + metaKey
		s.MeterEventNames[memMB] = eventName

		if m, ok := existingMeters[eventName]; ok {
			meterIDs[memMB] = m.ID
			log.Printf("billing: found existing meter %s (id=%s)", eventName, m.ID)
			continue
		}

		m, err := meter.New(&stripe.BillingMeterParams{
			DisplayName: stripe.String(fmt.Sprintf("Sandbox Compute %s", metaKey)),
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

	for memMB, metaKey := range TierMetadataKey {
		if id, ok := existingPrices[metaKey]; ok {
			s.PriceIDs[memMB] = id
			log.Printf("billing: found existing price for %s (id=%s)", metaKey, id)
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
				"tier":        metaKey,
				"memory_mb":   fmt.Sprintf("%d", memMB),
				"opensandbox": "compute",
			},
		})
		if err != nil {
			return fmt.Errorf("create price for %s: %w", metaKey, err)
		}
		s.PriceIDs[memMB] = p.ID
		log.Printf("billing: created price for %s (id=%s)", metaKey, p.ID)
	}

	return nil
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

// CreateSubscription creates a subscription with metered prices for all tiers.
// Returns subscription ID and map of memoryMB → subscription item ID.
func (s *StripeClient) CreateSubscription(customerID string) (string, map[int]string, error) {
	var items []*stripe.SubscriptionItemsParams
	for _, priceID := range s.PriceIDs {
		items = append(items, &stripe.SubscriptionItemsParams{
			Price: stripe.String(priceID),
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
