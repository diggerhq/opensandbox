package billing

import (
	"github.com/opensandbox/opensandbox/internal/db"
)

// TierPricePerSecond maps memory_mb → USD per second.
var TierPricePerSecond = map[int]float64{
	4096:  0.000001065, // 4GB / 1 vCPU  — $2.80/month
	8192:  0.000000380, // 8GB / 2 vCPU  — $3.62/month (note: not typo, see pricing table)
	16384: 0.000001521, // 16GB / 4 vCPU — $7.29/month
	32768: 0.000002282, // 32GB / 8 vCPU — $14.58/month
	65536: 0.000004563, // 64GB / 16 vCPU — $29.17/month
}

// CalculateUsageCostCents returns the total cost in cents (float64 for sub-cent precision)
// from a set of usage summaries (seconds per tier).
func CalculateUsageCostCents(summaries []db.OrgUsageSummary) float64 {
	var totalUSD float64
	for _, s := range summaries {
		rate, ok := TierPricePerSecond[s.MemoryMB]
		if !ok {
			continue
		}
		totalUSD += s.TotalSeconds * rate
	}
	return totalUSD * 100.0 // USD → cents
}
