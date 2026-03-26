package billing

import (
	"fmt"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/checkout/session"
	"github.com/stripe/stripe-go/v82/customer"
	"github.com/stripe/stripe-go/v82/paymentintent"
	"github.com/stripe/stripe-go/v82/webhook"
)

// StripeClient wraps the Stripe API for billing operations.
type StripeClient struct {
	webhookSecret string
	successURL    string
	cancelURL     string
}

// NewStripeClient creates a new Stripe client. It sets the global Stripe API key.
func NewStripeClient(secretKey, webhookSecret, successURL, cancelURL string) *StripeClient {
	stripe.Key = secretKey
	return &StripeClient{
		webhookSecret: webhookSecret,
		successURL:    successURL,
		cancelURL:     cancelURL,
	}
}

// CreateCustomer creates a Stripe customer for an org.
func (s *StripeClient) CreateCustomer(orgName, orgEmail string) (string, error) {
	params := &stripe.CustomerParams{
		Name:  stripe.String(orgName),
		Email: stripe.String(orgEmail),
	}
	c, err := customer.New(params)
	if err != nil {
		return "", fmt.Errorf("stripe create customer: %w", err)
	}
	return c.ID, nil
}

// CreateCheckoutSession creates a Stripe Checkout session for a one-time credit purchase.
// The payment method is saved for future off-session charges (auto top-up).
func (s *StripeClient) CreateCheckoutSession(customerID string, amountCents int64, orgID string) (url string, sessionID string, err error) {
	params := &stripe.CheckoutSessionParams{
		Customer: stripe.String(customerID),
		Mode:     stripe.String(string(stripe.CheckoutSessionModePayment)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
					Currency:   stripe.String("usd"),
					UnitAmount: stripe.Int64(amountCents),
					ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
						Name:        stripe.String("OpenSandbox Credits"),
						Description: stripe.String(fmt.Sprintf("$%.2f in credits", float64(amountCents)/100.0)),
					},
				},
				Quantity: stripe.Int64(1),
			},
		},
		PaymentIntentData: &stripe.CheckoutSessionPaymentIntentDataParams{
			SetupFutureUsage: stripe.String(string(stripe.PaymentIntentSetupFutureUsageOffSession)),
		},
		SuccessURL: stripe.String(s.successURL),
		CancelURL:  stripe.String(s.cancelURL),
		Metadata: map[string]string{
			"org_id":       orgID,
			"amount_cents": fmt.Sprintf("%d", amountCents),
			"type":         "purchase",
		},
	}

	sess, err := session.New(params)
	if err != nil {
		return "", "", fmt.Errorf("stripe create checkout session: %w", err)
	}
	return sess.URL, sess.ID, nil
}

// CreateSetupCheckoutSession creates a Stripe Checkout session for adding a payment method only (no charge).
func (s *StripeClient) CreateSetupCheckoutSession(customerID string, orgID string) (url string, sessionID string, err error) {
	params := &stripe.CheckoutSessionParams{
		Customer: stripe.String(customerID),
		Mode:     stripe.String(string(stripe.CheckoutSessionModeSetup)),
		SuccessURL: stripe.String(s.successURL),
		CancelURL:  stripe.String(s.cancelURL),
		Metadata: map[string]string{
			"org_id": orgID,
			"type":   "setup",
		},
	}

	sess, err := session.New(params)
	if err != nil {
		return "", "", fmt.Errorf("stripe create setup session: %w", err)
	}
	return sess.URL, sess.ID, nil
}

// ChargeCustomer creates a PaymentIntent using the customer's default payment method (for auto top-up).
func (s *StripeClient) ChargeCustomer(customerID string, amountCents int64, description string) (string, error) {
	params := &stripe.PaymentIntentParams{
		Amount:   stripe.Int64(amountCents),
		Currency: stripe.String("usd"),
		Customer: stripe.String(customerID),
		OffSession:          stripe.Bool(true),
		Confirm:             stripe.Bool(true),
		Description:         stripe.String(description),
		PaymentMethodTypes:  stripe.StringSlice([]string{"card"}),
	}

	pi, err := paymentintent.New(params)
	if err != nil {
		return "", fmt.Errorf("stripe charge customer: %w", err)
	}
	return pi.ID, nil
}

// VerifyWebhookSignature verifies a Stripe webhook signature and parses the event.
func (s *StripeClient) VerifyWebhookSignature(payload []byte, sigHeader string) (*stripe.Event, error) {
	event, err := webhook.ConstructEvent(payload, sigHeader, s.webhookSecret)
	if err != nil {
		return nil, fmt.Errorf("stripe webhook signature verification failed: %w", err)
	}
	return &event, nil
}
