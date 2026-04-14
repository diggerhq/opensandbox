package billing

import (
	"github.com/opensandbox/opensandbox/internal/db"
)

// TierPricePerSecond maps memory_mb → USD per second.
// Rates were 10×-ed on 2026-04-15 on top of the per-second correction that
// landed earlier the same day. Existing paying orgs are grandfathered via
// orgs.price_locked (see migration 022); cmd/migrate-prices skips locked orgs
// by default so only new signups see these rates.
var TierPricePerSecond = map[int]float64{
	1024:  0.00001080246914, // 1GB / 1 vCPU
	4096:  0.00005787037037, // 4GB / 1 vCPU
	8192:  0.0001350308642,  // 8GB / 2 vCPU
	16384: 0.0002700617284,  // 16GB / 4 vCPU
	32768: 0.001929012346,   // 32GB / 8 vCPU
	65536: 0.005401234568,   // 64GB / 16 vCPU
}

// TierMeterKey maps memory_mb → stable key used to derive the Stripe meter
// event_name ("sandbox_compute_" + value). NEVER change these values: meters
// hold historical usage and are shared across price versions.
var TierMeterKey = map[int]string{
	1024:  "sandbox_1gb",
	4096:  "sandbox_4gb",
	8192:  "sandbox_8gb",
	16384: "sandbox_16gb",
	32768: "sandbox_32gb",
	65536: "sandbox_64gb",
}

// TierPriceKey maps memory_mb → Stripe Price metadata["tier"] key.
// Bump the suffix (e.g. sandbox_8gb → sandbox_8gb_v2) whenever TierPricePerSecond
// changes for that tier: Stripe Prices are immutable, so a new key forces
// EnsureProducts to create a fresh Price at the new rate. Existing subscriptions
// must then be migrated to the new Price via cmd/migrate-prices — unless the
// org is marked price_locked=TRUE, in which case they are grandfathered.
//
// The suffixes here are bumped one step above the per-second-correction set
// (which established _v2 for most tiers and _v3 for 8GB). This forces
// EnsureProducts to create fresh Prices at the 10×-higher rates below. Every
// org currently paying is locked via migration 022, so migrate-prices skips
// them by default — only new signups subscribe at the 10× rate.
var TierPriceKey = map[int]string{
	1024:  "sandbox_1gb_v3",
	4096:  "sandbox_4gb_v3",
	8192:  "sandbox_8gb_v4",
	16384: "sandbox_16gb_v3",
	32768: "sandbox_32gb_v3",
	65536: "sandbox_64gb_v3",
}

// Disk overage billing — every GB above DiskFreeAllowanceMB is metered for the
// full lifetime of the sandbox (running OR hibernated, since the workspace
// qcow2 still occupies host disk).
const (
	DiskFreeAllowanceMB            = 20480      // 20GB included with every sandbox
	DiskOveragePricePerGBPerSecond = 0.0000001  // ~$0.26 per GB-month
	DiskOverageMetadataKey         = "sandbox_disk_overage"
)

// DiskOverageGBSeconds returns the chargeable GB-seconds for one usage summary
// row (zero if the sandbox stayed within the free allowance).
func DiskOverageGBSeconds(s db.OrgUsageSummary) float64 {
	overageMB := s.DiskMB - DiskFreeAllowanceMB
	if overageMB <= 0 || s.TotalSeconds <= 0 {
		return 0
	}
	return float64(overageMB) / 1024.0 * s.TotalSeconds
}

// CalculateUsageCostCents returns total cost in cents from usage summaries —
// memory tier compute plus per-GB-second disk overage above 20GB.
func CalculateUsageCostCents(summaries []db.OrgUsageSummary) float64 {
	var totalUSD float64
	for _, s := range summaries {
		if rate, ok := TierPricePerSecond[s.MemoryMB]; ok {
			totalUSD += s.TotalSeconds * rate
		}
		totalUSD += DiskOverageGBSeconds(s) * DiskOveragePricePerGBPerSecond
	}
	return totalUSD * 100.0
}
