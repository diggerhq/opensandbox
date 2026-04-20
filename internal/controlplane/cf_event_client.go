package controlplane

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// CFEventClient posts event batches to the Cloudflare events-ingest Worker.
// Authentication is HMAC-SHA256 over "timestamp.body" where timestamp is unix
// seconds — same pattern as Stripe webhook verify. The Worker rejects
// timestamps more than 5 minutes old to block replay.
type CFEventClient struct {
	endpoint string
	secret   []byte
	cellID   string
	http     *http.Client
}

// PermanentError wraps a 4xx response from the ingest Worker. The forwarder
// treats this as a poison batch: ack the messages and move on, because
// retrying will just produce the same failure.
type PermanentError struct {
	Status int
	Body   string
}

func (e *PermanentError) Error() string {
	return fmt.Sprintf("cf ingest rejected batch: %d %s", e.Status, e.Body)
}

// NewCFEventClient returns a client with a 30s timeout. Endpoint example:
// "https://events.opencomputer.workers.dev/ingest".
func NewCFEventClient(endpoint, secret, cellID string) *CFEventClient {
	return &CFEventClient{
		endpoint: endpoint,
		secret:   []byte(secret),
		cellID:   cellID,
		http:     &http.Client{Timeout: 30 * time.Second},
	}
}

// SendBatch ships a JSON-encoded batch. Events must already be enriched with
// org_id/plan by the caller. Retry policy:
//   - 5xx or network error: exponential 1s, 2s, 4s (max 3 attempts)
//   - 429: honor Retry-After once, then fall back to the 5xx path
//   - 4xx (non-429): PermanentError, no retry
//   - 2xx: nil
//
// ctx cancellation aborts retries immediately.
func (c *CFEventClient) SendBatch(ctx context.Context, events []map[string]interface{}) error {
	body, err := json.Marshal(map[string]interface{}{"events": events})
	if err != nil {
		return fmt.Errorf("marshal batch: %w", err)
	}

	backoff := []time.Duration{0, 1 * time.Second, 2 * time.Second, 4 * time.Second}
	var retryAfterHonored bool
	var lastErr error

	for attempt := 0; attempt < len(backoff); attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff[attempt]):
			}
		}

		ts := strconv.FormatInt(time.Now().Unix(), 10)
		sig := c.sign(ts, body)

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Cell-Id", c.cellID)
		req.Header.Set("X-Timestamp", ts)
		req.Header.Set("X-Signature", sig)

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		status := resp.StatusCode
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		switch {
		case status >= 200 && status < 300:
			return nil
		case status == http.StatusTooManyRequests && !retryAfterHonored:
			retryAfterHonored = true
			if wait := parseRetryAfter(resp.Header.Get("Retry-After")); wait > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(wait):
				}
			}
			// Re-do this attempt slot rather than consuming the exponential budget.
			attempt--
			continue
		case status >= 400 && status < 500:
			return &PermanentError{Status: status, Body: string(respBody)}
		default:
			lastErr = fmt.Errorf("ingest returned %d: %s", status, string(respBody))
		}
	}

	if lastErr == nil {
		lastErr = errors.New("ingest request failed without a specific error")
	}
	return lastErr
}

func (c *CFEventClient) sign(timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, c.secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func parseRetryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if s, err := strconv.Atoi(h); err == nil && s > 0 {
		// Cap at 30s to avoid unbounded waits from misconfigured upstreams.
		if s > 30 {
			s = 30
		}
		return time.Duration(s) * time.Second
	}
	return 0
}
