// AWS spot interruption monitor.
//
// AWS publishes interruption notices on IMDSv2 at
//   GET /latest/meta-data/spot/instance-action
// returning HTTP 200 with a JSON body shaped:
//   { "action": "stop|terminate|hibernate", "time": "2026-05-19T12:34:56Z" }
//
// Until interruption is scheduled, the endpoint returns 404. We poll every
// `pollInterval` (default 5s). AWS guarantees the notice lands at least
// ~2 minutes before the instance is reclaimed, so 5s polling gives ample
// drain budget.
//
// IMDSv2 requires a session token (PUT /latest/api/token). Tokens are
// scoped per-request and expire in 6h, but we re-fetch on every poll for
// simplicity — IMDS is link-local, the round-trip cost is negligible
// compared to the network operations the worker is otherwise doing.

package preemption

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type awsMonitor struct {
	pollInterval time.Duration
	imdsEndpoint string
}

func (m *awsMonitor) Name() string { return "aws-imds" }

func (m *awsMonitor) Watch(ctx context.Context) <-chan Notice {
	ch := make(chan Notice, 1)

	go func() {
		defer close(ch)

		// Standalone client so the global default client's settings can't
		// stall our 1-second IMDS round-trips with unrelated transport
		// configuration.
		client := &http.Client{Timeout: 2 * time.Second}

		ticker := time.NewTicker(m.pollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				notice, found, err := m.probe(ctx, client)
				if err != nil {
					// Don't spam — IMDS unreachability is interesting once
					// per minute, not once per 5s.
					log.Printf("preemption: aws probe error (will retry): %v", err)
					continue
				}
				if !found {
					continue
				}
				// Buffered channel — non-blocking send. If the consumer is
				// somehow not reading, drop the second-and-later notices
				// (the first one is what counts).
				select {
				case ch <- notice:
				default:
				}
				// Keep polling — the action / time can change between
				// notice issuance and reclaim, and we want the consumer to
				// see the freshest data. Drain is idempotent.
			}
		}
	}()

	return ch
}

// probe issues one IMDSv2 token-then-get round trip. Returns (Notice, true,
// nil) when interruption is imminent, (zero, false, nil) when healthy, and
// (zero, false, err) on transport / decode failures.
func (m *awsMonitor) probe(ctx context.Context, client *http.Client) (Notice, bool, error) {
	token, err := m.fetchToken(ctx, client)
	if err != nil {
		return Notice{}, false, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		m.imdsEndpoint+"/latest/meta-data/spot/instance-action", nil)
	if err != nil {
		return Notice{}, false, err
	}
	req.Header.Set("X-aws-ec2-metadata-token", token)

	resp, err := client.Do(req)
	if err != nil {
		return Notice{}, false, err
	}
	defer resp.Body.Close()

	// 404 = healthy. AWS specifies this endpoint returns 404 until
	// interruption is scheduled.
	if resp.StatusCode == http.StatusNotFound {
		return Notice{}, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		// Drain body to keep the connection reusable.
		_, _ = io.Copy(io.Discard, resp.Body)
		return Notice{}, false, nil
	}

	var body struct {
		Action string `json:"action"`
		Time   string `json:"time"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Notice{}, false, err
	}

	eta, err := time.Parse(time.RFC3339, body.Time)
	if err != nil {
		// AWS guarantees RFC3339; if they ever change format we'd rather
		// fire a Notice with the zero time than miss the signal — the
		// drain budget still applies, and the caller can fallback to
		// "drain now".
		log.Printf("preemption: aws unexpected time format %q: %v", body.Time, err)
	}

	return Notice{
		Action: Action(strings.ToLower(body.Action)),
		ETA:    eta,
		Source: "aws-imds",
	}, true, nil
}

// fetchToken obtains an IMDSv2 session token. The token is single-use here;
// see package doc for why that's fine.
func (m *awsMonitor) fetchToken(ctx context.Context, client *http.Client) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		m.imdsEndpoint+"/latest/api/token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", &imdsHTTPError{code: resp.StatusCode}
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

type imdsHTTPError struct{ code int }

func (e *imdsHTTPError) Error() string {
	return "imds token fetch returned HTTP " + http.StatusText(e.code)
}
