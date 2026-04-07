// Package analytics ships per-org usage metrics to Segment.
//
// The only metric we care about is GB-seconds of memory consumed, tagged with
// the org_id (the entity being billed). Everything in this package is a no-op
// when the client is nil, so call sites don't need to branch on configuration.
package analytics

import (
	"log"

	"github.com/segmentio/analytics-go/v3"
)

// Client is a thin wrapper around the Segment Go client. The zero value and a
// nil pointer are both safe to use — Track and Close become no-ops.
type Client struct {
	c analytics.Client
}

// New returns a Client. If writeKey is empty, returns nil (no-op client).
func New(writeKey string) *Client {
	if writeKey == "" {
		return nil
	}
	return &Client{c: analytics.New(writeKey)}
}

// TrackGBSeconds enqueues a "Sandbox Memory Usage" event with GB-seconds for
// the given org. sandboxID is included as an event property for debugging but
// the metric of record is gb_seconds bucketed by org.
func (c *Client) TrackGBSeconds(orgID, sandboxID string, gbSeconds float64) {
	if c == nil || c.c == nil || orgID == "" || gbSeconds <= 0 {
		return
	}
	if err := c.c.Enqueue(analytics.Track{
		UserId: orgID,
		Event:  "Sandbox Memory Usage",
		Properties: analytics.NewProperties().
			Set("gb_seconds", gbSeconds).
			Set("sandbox_id", sandboxID).
			Set("org_id", orgID),
	}); err != nil {
		log.Printf("segment: enqueue failed: %v", err)
	}
}

// Close flushes pending events. Safe on nil.
func (c *Client) Close() {
	if c == nil || c.c == nil {
		return
	}
	if err := c.c.Close(); err != nil {
		log.Printf("segment: close failed: %v", err)
	}
}
