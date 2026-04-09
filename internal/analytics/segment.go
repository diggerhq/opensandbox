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

// UsageEvent holds the identity fields shipped alongside a usage metric.
type UsageEvent struct {
	OrgID        string
	UserID       string
	UserEmail    string
	WorkosUserID string
	WorkosOrgID  string
	SandboxID    string
	GBSeconds    float64
}

// TrackGBSeconds enqueues a "Sandbox Memory Usage" event with GB-seconds for
// the given org. User fields are included as properties for downstream
// filtering; the metric of record is gb_seconds bucketed by org.
func (c *Client) TrackGBSeconds(evt UsageEvent) {
	if c == nil || c.c == nil || evt.OrgID == "" || evt.GBSeconds <= 0 {
		return
	}
	props := analytics.NewProperties().
		Set("gb_seconds", evt.GBSeconds).
		Set("sandbox_id", evt.SandboxID).
		Set("org_id", evt.OrgID)
	if evt.UserID != "" {
		props = props.Set("user_id", evt.UserID)
	}
	if evt.UserEmail != "" {
		props = props.Set("user_email", evt.UserEmail)
	}
	if evt.WorkosUserID != "" {
		props = props.Set("workos_user_id", evt.WorkosUserID)
	}
	if evt.WorkosOrgID != "" {
		props = props.Set("workos_org_id", evt.WorkosOrgID)
	}
	if err := c.c.Enqueue(analytics.Track{
		UserId:     evt.OrgID,
		Event:      "Sandbox Memory Usage",
		Properties: props,
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
