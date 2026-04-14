// backfill-disk-overage attaches the disk-overage metered price to every
// existing pro-tier subscription that was created before disk overage billing
// shipped. Idempotent — safe to re-run.
//
// Usage:
//
//	DATABASE_URL=postgres://... STRIPE_SECRET_KEY=sk_live_... \
//	    go run ./cmd/backfill-disk-overage [--dry-run]
//
// New subscriptions created via the normal checkout flow already include the
// disk overage price (see billing.StripeClient.CreateSubscription), so this
// script only needs to run once after deploying the disk-overage migration.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/subscription"
	"github.com/stripe/stripe-go/v82/subscriptionitem"

	"github.com/opensandbox/opensandbox/internal/billing"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "report what would change without calling Stripe write APIs")
	flag.Parse()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	stripeKey := os.Getenv("STRIPE_SECRET_KEY")
	if stripeKey == "" {
		log.Fatal("STRIPE_SECRET_KEY is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	stripe.Key = stripeKey
	client := billing.NewStripeClient(stripeKey, "", "", "")
	if err := client.EnsureProducts(); err != nil {
		log.Fatalf("ensure products: %v", err)
	}
	if client.DiskOveragePriceID == "" {
		log.Fatal("disk overage price not provisioned by EnsureProducts — aborting")
	}
	log.Printf("disk overage price id: %s", client.DiskOveragePriceID)

	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		log.Fatalf("connect db: %v", err)
	}
	defer conn.Close(ctx)

	rows, err := conn.Query(ctx,
		`SELECT id, name, stripe_subscription_id
		   FROM orgs
		  WHERE plan = 'pro'
		    AND stripe_subscription_id IS NOT NULL`)
	if err != nil {
		log.Fatalf("query orgs: %v", err)
	}
	defer rows.Close()

	type target struct {
		orgID, name, subID string
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.orgID, &t.name, &t.subID); err != nil {
			log.Fatalf("scan: %v", err)
		}
		targets = append(targets, t)
	}
	if err := rows.Err(); err != nil {
		log.Fatalf("rows: %v", err)
	}

	log.Printf("found %d pro org(s) with active subscriptions", len(targets))

	var attached, alreadyHad, failed int
	for _, t := range targets {
		sub, err := subscription.Get(t.subID, &stripe.SubscriptionParams{
			Expand: []*string{stripe.String("items.data.price")},
		})
		if err != nil {
			log.Printf("org %s (%s): fetch subscription %s: %v", t.orgID, t.name, t.subID, err)
			failed++
			continue
		}

		hasDisk := false
		for _, it := range sub.Items.Data {
			if it.Price != nil && it.Price.ID == client.DiskOveragePriceID {
				hasDisk = true
				break
			}
		}
		if hasDisk {
			alreadyHad++
			continue
		}

		if *dryRun {
			log.Printf("[dry-run] org %s (%s): would attach disk overage price to sub %s", t.orgID, t.name, t.subID)
			attached++
			continue
		}

		newItem, err := subscriptionitem.New(&stripe.SubscriptionItemParams{
			Subscription: stripe.String(t.subID),
			Price:        stripe.String(client.DiskOveragePriceID),
			// ProrationBehavior=none — metered prices have no proration anyway
			// and we don't want any surprise immediate invoice.
			ProrationBehavior: stripe.String("none"),
		})
		if err != nil {
			log.Printf("org %s (%s): attach disk overage to sub %s: %v", t.orgID, t.name, t.subID, err)
			failed++
			continue
		}
		log.Printf("org %s (%s): attached disk overage item %s to sub %s", t.orgID, t.name, newItem.ID, t.subID)
		attached++
	}

	fmt.Println()
	fmt.Println("=== summary ===")
	fmt.Printf("orgs scanned:           %d\n", len(targets))
	fmt.Printf("already had disk price: %d\n", alreadyHad)
	if *dryRun {
		fmt.Printf("would attach (dry-run): %d\n", attached)
	} else {
		fmt.Printf("attached:               %d\n", attached)
	}
	fmt.Printf("failed:                 %d\n", failed)

	if failed > 0 {
		os.Exit(1)
	}
}
