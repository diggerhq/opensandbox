package controlplane

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// CFEventClient POSTs HMAC-signed batches of events to the Cloudflare
// events-ingest Worker.
//
// Headers:
//   - X-Cell-Id    full deployment identifier (e.g. azure-westus2-cell-a)
//   - X-Timestamp  unix seconds (used in signature, validated ±5min on the receiver)
//   - X-Signature  hex HMAC-SHA256(secret, fmt.Sprintf("%d.%s", timestamp, body))
//
// Retry semantics:
//   - 5xx / network: exponential 1s, 2s, 4s, max 3 attempts. Caller leaves the
//     batch in PEL (does not XAck) so it gets reclaimed.
//   - 429: respect Retry-After header once, then 5xx path.
//   - 4xx (non-429): ErrPermanent. Caller XAcks and logs (poison pill).
//   - 2xx: nil. Caller XAcks.
type CFEventClient struct {
	endpoint string
	secret   []byte
	cellID   string
	http     *http.Client
}

// ErrPermanent indicates the batch should never be retried (4xx non-429).
var ErrPermanent = errors.New("permanent error: do not retry")

// NewCFEventClient constructs a client. Endpoint and secret must be non-empty.
func NewCFEventClient(endpoint, secret, cellID string) *CFEventClient {
	return &CFEventClient{
		endpoint: endpoint,
		secret:   []byte(secret),
		cellID:   cellID,
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// SendBatch signs and POSTs a JSON batch.
// Body should be a JSON-encoded array of SandboxEvent envelopes.
func (c *CFEventClient) SendBatch(ctx context.Context, body []byte) error {
	const maxAttempts = 3
	delay := time.Second
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := c.postOnce(ctx, body)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrPermanent) {
			return err
		}

		// 429 path — honor Retry-After exactly once, then fall into 5xx backoff
		var ra retryAfterErr
		if errors.As(err, &ra) && ra.dur > 0 && attempt == 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(ra.dur):
			}
			continue
		}

		lastErr = err
		if attempt == maxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
	}
	return fmt.Errorf("send batch: %w", lastErr)
}

// retryAfterErr signals the server returned 429 with a Retry-After header.
type retryAfterErr struct {
	dur time.Duration
}

func (e retryAfterErr) Error() string { return fmt.Sprintf("rate limited, retry-after %s", e.dur) }

func (c *CFEventClient) postOnce(ctx context.Context, body []byte) error {
	ts := time.Now().Unix()

	mac := hmac.New(sha256.New, c.secret)
	fmt.Fprintf(mac, "%d.", ts)
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Cell-Id", c.cellID)
	req.Header.Set("X-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-Signature", sig)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests:
		ra := time.Duration(0)
		if v := resp.Header.Get("Retry-After"); v != "" {
			if secs, err := strconv.Atoi(v); err == nil && secs > 0 && secs < 60 {
				ra = time.Duration(secs) * time.Second
			}
		}
		return retryAfterErr{dur: ra}
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return fmt.Errorf("status %d: %w", resp.StatusCode, ErrPermanent)
	default:
		return fmt.Errorf("status %d", resp.StatusCode)
	}
}
