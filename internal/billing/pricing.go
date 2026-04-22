package billing

import (
	"github.com/opensandbox/opensandbox/internal/db"
)

// TierPricePerSecond maps memory_mb → USD per second.
// Reverted on 2026-04-22 back to the pre-10× rates established by the
// per-second correction on 2026-04-15. Orgs that signed up at the 10× rate
// between 2026-04-15 and 2026-04-22 are migrated onto these rates via
// cmd/migrate-prices (price_locked=FALSE by default).
var TierPricePerSecond = map[int]float64{
	1024:  0.000001080246914, // 1GB / 1 vCPU
	4096:  0.000005787037037, // 4GB / 1 vCPU
	8192:  0.00001350308642,  // 8GB / 2 vCPU
	16384: 0.00002700617284,  // 16GB / 4 vCPU
	32768: 0.0001929012346,   // 32GB / 8 vCPU
	65536: 0.0005401234568,   // 64GB / 16 vCPU
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
// The suffixes here are bumped one step above the 10× set (_v3 for most tiers,
// _v4 for 8GB) to force EnsureProducts to create fresh Stripe Prices at the
// reverted pre-10× rates below. Orgs still on the 10× Prices are moved onto
// these new Prices via cmd/migrate-prices after deploy.
var TierPriceKey = map[int]string{
	1024:  "sandbox_1gb_v4",
	4096:  "sandbox_4gb_v4",
	8192:  "sandbox_8gb_v5",
	16384: "sandbox_16gb_v4",
	32768: "sandbox_32gb_v4",
	65536: "sandbox_64gb_v4",
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
