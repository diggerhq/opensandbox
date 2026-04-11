package billing

import (
	"github.com/opensandbox/opensandbox/internal/db"
)

// TierPricePerSecond maps memory_mb → USD per second.
var TierPricePerSecond = map[int]float64{
	1024:  0.000001080246914, // 1GB / 1 vCPU
	4096:  0.000005787037037, // 4GB / 1 vCPU
	8192:  0.000005015432099, // 8GB / 2 vCPU
	16384: 0.00002700617284,  // 16GB / 4 vCPU
	32768: 0.0001929012346,   // 32GB / 8 vCPU
	65536: 0.0005401234568,   // 64GB / 16 vCPU
}

// TierMetadataKey maps memory_mb → Stripe metadata key for price lookup.
var TierMetadataKey = map[int]string{
	1024:  "sandbox_1gb",
	4096:  "sandbox_4gb",
	8192:  "sandbox_8gb",
	16384: "sandbox_16gb",
	32768: "sandbox_32gb",
	65536: "sandbox_64gb",
}

// CalculateUsageCostCents returns total cost in cents from usage summaries.
func CalculateUsageCostCents(summaries []db.OrgUsageSummary) float64 {
	var totalUSD float64
	for _, s := range summaries {
		rate, ok := TierPricePerSecond[s.MemoryMB]
		if !ok {
			continue
		}
		totalUSD += s.TotalSeconds * rate
	}
	return totalUSD * 100.0
}
