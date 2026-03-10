package controlplane

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/opensandbox/opensandbox/internal/dns"
)

// DNSCleaner periodically removes stale Route53 A records for workers that
// are no longer in the Redis registry. This handles the case where a worker
// crashes or is terminated without running its shutdown DNS cleanup script.
type DNSCleaner struct {
	dnsClient *dns.Route53Client
	registry  *RedisWorkerRegistry
	domain    string // e.g. "workers.opensandbox.ai"
	interval  time.Duration
	stop      chan struct{}
	stopOnce  sync.Once
}

// DNSCleanerConfig holds configuration for the DNS cleaner.
type DNSCleanerConfig struct {
	DNSClient *dns.Route53Client
	Registry  *RedisWorkerRegistry
	Domain    string        // e.g. "workers.opensandbox.ai"
	Interval  time.Duration // default 5 minutes
}

// NewDNSCleaner creates a DNS cleaner that removes stale worker A records.
func NewDNSCleaner(cfg DNSCleanerConfig) *DNSCleaner {
	if cfg.Interval == 0 {
		cfg.Interval = 5 * time.Minute
	}
	return &DNSCleaner{
		dnsClient: cfg.DNSClient,
		registry:  cfg.Registry,
		domain:    cfg.Domain,
		interval:  cfg.Interval,
		stop:      make(chan struct{}),
	}
}

// Start begins the periodic DNS cleanup loop.
func (c *DNSCleaner) Start() {
	go c.loop()
}

// Stop stops the cleanup loop. Safe to call multiple times.
func (c *DNSCleaner) Stop() {
	c.stopOnce.Do(func() { close(c.stop) })
}

func (c *DNSCleaner) loop() {
	// Run once after a short delay on startup (let workers register first)
	select {
	case <-time.After(2 * time.Minute):
	case <-c.stop:
		return
	}

	c.cleanup()

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.cleanup()
		case <-c.stop:
			return
		}
	}
}

func (c *DNSCleaner) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	suffix := "." + c.domain
	records, err := c.dnsClient.ListARecords(ctx, suffix)
	if err != nil {
		log.Printf("dns-cleaner: failed to list A records: %v", err)
		return
	}

	// Build set of live worker hostnames from registry
	liveWorkers := make(map[string]bool)
	for _, w := range c.registry.GetAllWorkers() {
		hostname := w.ID + "." + c.domain
		liveWorkers[hostname] = true
	}

	// Find and remove stale records
	stale := 0
	for _, rec := range records {
		// Only clean up worker records (w-*.domain)
		sub := strings.TrimSuffix(rec.Name, suffix)
		if !strings.HasPrefix(sub, "w-") {
			continue
		}

		if liveWorkers[rec.Name] {
			continue
		}

		log.Printf("dns-cleaner: removing stale A record %s -> %s (worker not in registry)", rec.Name, rec.Value)
		if err := c.dnsClient.DeleteARecord(ctx, rec.Name, rec.Value, rec.TTL); err != nil {
			log.Printf("dns-cleaner: failed to delete %s: %v", rec.Name, err)
		} else {
			stale++
		}
	}

	if stale > 0 {
		log.Printf("dns-cleaner: removed %d stale DNS records", stale)
	}
}
