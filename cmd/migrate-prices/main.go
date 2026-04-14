// migrate-prices is a one-shot tool that migrates every org's subscription
// item for a given memory tier to the current (v-latest) Stripe Price ID
// returned by billing.EnsureProducts.
//
// Usage:
//   DATABASE_URL=... STRIPE_SECRET_KEY=... \
//     go run ./cmd/migrate-prices --tier=8192 --dry-run
//   DATABASE_URL=... STRIPE_SECRET_KEY=... \
//     go run ./cmd/migrate-prices --tier=8192 --live
//
// By default, orgs with orgs.price_locked=TRUE are skipped (grandfathered).
// Pass --force to override and migrate locked orgs too.
//
// The command is idempotent: items already attached to the target price are
// skipped. Usage already accrued this cycle stays at the previous rate because
// we pass proration_behavior=none; metered events are matched to whichever
// price was attached at event time.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/subscriptionitem"

	"github.com/opensandbox/opensandbox/internal/billing"
)

func main() {
	tier := flag.Int("tier", 8192, "memory_mb tier to migrate (must be a key in billing.TierPriceKey)")
	dryRun := flag.Bool("dry-run", true, "if true, prints what would change without calling Stripe")
	live := flag.Bool("live", false, "must be set to actually mutate Stripe; overrides --dry-run")
	force := flag.Bool("force", false, "also migrate orgs with price_locked=TRUE (off by default — grandfathered orgs stay on their current price)")
	flag.Parse()

	if *live {
		*dryRun = false
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	stripeKey := os.Getenv("STRIPE_SECRET_KEY")
	if stripeKey == "" {
		log.Fatal("STRIPE_SECRET_KEY is required")
	}

	if _, ok := billing.TierPriceKey[*tier]; !ok {
		log.Fatalf("tier %d is not defined in billing.TierPriceKey", *tier)
	}

	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("connect db: %v", err)
	}
	defer pool.Close()

	client := billing.NewStripeClient(stripeKey, "", "", "")
	if err := client.EnsureProducts(); err != nil {
		log.Fatalf("ensure products: %v", err)
	}

	targetPriceID, ok := client.PriceIDs[*tier]
	if !ok || targetPriceID == "" {
		log.Fatalf("no price id for tier %d after EnsureProducts", *tier)
	}
	log.Printf("target price for tier %d: %s (key=%s)", *tier, targetPriceID, billing.TierPriceKey[*tier])

	query := `SELECT osi.org_id, osi.stripe_subscription_item_id, o.stripe_subscription_id, o.price_locked
		   FROM org_subscription_items osi
		   JOIN orgs o ON o.id = osi.org_id
		  WHERE osi.memory_mb = $1
		    AND o.stripe_subscription_id IS NOT NULL`
	if !*force {
		query += ` AND o.price_locked = FALSE`
	}
	rows, err := pool.Query(ctx, query, *tier)
	if err != nil {
		log.Fatalf("query items: %v", err)
	}
	defer rows.Close()

	type target struct {
		orgID, itemID, subID string
		locked               bool
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.orgID, &t.itemID, &t.subID, &t.locked); err != nil {
			log.Fatalf("scan: %v", err)
		}
		targets = append(targets, t)
	}
	if err := rows.Err(); err != nil {
		log.Fatalf("rows err: %v", err)
	}
	if *force {
		log.Printf("found %d candidate subscription item(s) (--force: price_locked ignored)", len(targets))
	} else {
		log.Printf("found %d candidate subscription item(s) (price_locked=FALSE)", len(targets))
	}

	stripe.Key = stripeKey

	var migrated, skipped, failed int
	for _, t := range targets {
		item, err := subscriptionitem.Get(t.itemID, nil)
		if err != nil {
			log.Printf("[FAIL] org=%s item=%s fetch: %v", t.orgID, t.itemID, err)
			failed++
			continue
		}
		currentPrice := ""
		if item.Price != nil {
			currentPrice = item.Price.ID
		}
		if currentPrice == targetPriceID {
			skipped++
			continue
		}

		lockTag := ""
		if t.locked {
			lockTag = " [LOCKED-override]"
		}
		log.Printf("[PLAN]%s org=%s item=%s price %s → %s", lockTag, t.orgID, t.itemID, currentPrice, targetPriceID)
		if *dryRun {
			continue
		}

		_, err = subscriptionitem.Update(t.itemID, &stripe.SubscriptionItemParams{
			Price:             stripe.String(targetPriceID),
			ProrationBehavior: stripe.String("none"),
		})
		if err != nil {
			log.Printf("[FAIL] org=%s item=%s update: %v", t.orgID, t.itemID, err)
			failed++
			continue
		}
		migrated++
	}

	mode := "DRY-RUN"
	if !*dryRun {
		mode = "LIVE"
	}
	fmt.Printf("\n%s complete: migrated=%d skipped=%d failed=%d total=%d\n",
		mode, migrated, skipped, failed, len(targets))
	if failed > 0 {
		os.Exit(1)
	}
}
