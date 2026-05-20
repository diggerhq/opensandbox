// Package edgeclient is the CP's thin HTTP client to the api-edge Worker.
//
// Edge endpoints called from here are all HMAC-authenticated with EVENT_SECRET
// (same secret shared with the events-ingest forwarder + halt-list pull). The
// signing scheme matches internal/controlplane/admin_handlers.go.signGet for
// no-body requests and adds the body bytes for POST/PUT.
//
// Returned types intentionally mirror existing internal/db.* shapes so the
// rest of the CP doesn't have to learn a new model: GetTemplateByName previously
// returned db.DBTemplate; LookupTemplate here returns the same. Encrypted
// secret entries are returned as raw bytes — callers pair with internal/crypto.
package edgeclient

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/opensandbox/opensandbox/internal/crypto"
	"github.com/opensandbox/opensandbox/internal/db"
)

// ErrNotFound — returned for any 404 from the edge so callers can match it
// directly instead of parsing strings.
var ErrNotFound = errors.New("edgeclient: not found")

// Client is safe for concurrent use; it just wraps an HTTP client with HMAC
// signing.
type Client struct {
	BaseURL string // e.g. "https://app.dev.opensandbox.ai"
	Secret  string // CFEventSecret
	HTTP    *http.Client
}

// New returns a Client with a sane default http.Client. Pass an explicit
// http.Client (with a transport / timeout) for production use.
func New(baseURL, secret string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Secret:  secret,
		HTTP:    &http.Client{Timeout: 10 * time.Second},
	}
}

// ── HMAC ────────────────────────────────────────────────────────────────

// sign computes hex(HMAC-SHA256(secret, ts + "." + pathWithQuery [+ "." + body])).
// Matches the verifyHMAC implementations in cloudflare-workers/api-edge/src/.
func (c *Client) sign(ts, pathWithQuery string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(c.Secret))
	mac.Write([]byte(ts))
	mac.Write([]byte{'.'})
	mac.Write([]byte(pathWithQuery))
	if body != nil {
		mac.Write([]byte{'.'})
		mac.Write(body)
	}
	return hex.EncodeToString(mac.Sum(nil))
}

func (c *Client) do(ctx context.Context, method, pathWithQuery string, body []byte) ([]byte, int, error) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := c.sign(ts, pathWithQuery, body)
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+pathWithQuery, reader)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("X-Timestamp", ts)
	req.Header.Set("X-Signature", sig)
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b, resp.StatusCode, nil
}

// ── Templates ──────────────────────────────────────────────────────────

// edgeTemplate is the wire-format struct returned by /internal/templates/by-name.
// Field tags match cloudflare-workers/api-edge/src/templates.ts.
type edgeTemplate struct {
	ID                 string   `json:"id"`
	OrgID              *string  `json:"orgID"`
	Name               string   `json:"name"`
	Tag                string   `json:"tag"`
	TemplateType       string   `json:"templateType"`
	ImageRef           *string  `json:"imageRef"`
	RootfsS3Key        *string  `json:"rootfsS3Key"`
	WorkspaceS3Key     *string  `json:"workspaceS3Key"`
	Dockerfile         *string  `json:"dockerfile"`
	IsPublic           bool     `json:"isPublic"`
	Status             string   `json:"status"`
	CellsAvailable     []string `json:"cellsAvailable"`
	CreatedBySandboxID *string  `json:"createdBySandboxID"`
	CreatedAt          int64    `json:"createdAt"`
}

func (t *edgeTemplate) toDB() (*db.DBTemplate, error) {
	out := &db.DBTemplate{
		Name:               t.Name,
		Tag:                t.Tag,
		TemplateType:       t.TemplateType,
		Dockerfile:         t.Dockerfile,
		RootfsS3Key:        t.RootfsS3Key,
		WorkspaceS3Key:     t.WorkspaceS3Key,
		CreatedBySandboxID: t.CreatedBySandboxID,
		IsPublic:           t.IsPublic,
		Status:             t.Status,
		CreatedAt:          time.Unix(t.CreatedAt, 0),
	}
	if t.ImageRef != nil {
		out.ImageRef = *t.ImageRef
	}
	id, err := uuid.Parse(t.ID)
	if err != nil {
		return nil, fmt.Errorf("parse template id: %w", err)
	}
	out.ID = id
	if t.OrgID != nil && *t.OrgID != "" {
		oid, err := uuid.Parse(*t.OrgID)
		if err != nil {
			return nil, fmt.Errorf("parse template org_id: %w", err)
		}
		out.OrgID = &oid
	}
	return out, nil
}

// LookupTemplate fetches a template by (orgID, name). Mirrors the semantics
// of db.Store.GetTemplateByName: org-scoped lookup with public fallback.
// Passing uuid.Nil for orgID skips the private lookup and goes straight to
// public templates.
func (c *Client) LookupTemplate(ctx context.Context, orgID uuid.UUID, name string) (*db.DBTemplate, error) {
	q := "/internal/templates/by-name?name=" + escapeQuery(name)
	if orgID != uuid.Nil {
		q += "&org_id=" + escapeQuery(orgID.String())
	}
	body, status, err := c.do(ctx, "GET", q, nil)
	if err != nil {
		return nil, err
	}
	if status == 404 {
		return nil, ErrNotFound
	}
	if status != 200 {
		return nil, fmt.Errorf("edge status %d: %s", status, string(body))
	}
	var et edgeTemplate
	if err := json.Unmarshal(body, &et); err != nil {
		return nil, fmt.Errorf("decode template: %w", err)
	}
	return et.toDB()
}

// RegisterTemplate inserts a template row in D1. Called from the CP's "save
// sandbox as template" flow after the rootfs + workspace are uploaded to
// Tigris. The id field is honored if non-Nil (so the CP can reuse one it
// already generated for the Tigris keys); otherwise edge generates one.
type RegisterArgs struct {
	ID                 uuid.UUID // optional; edge mints if Nil
	OrgID              *uuid.UUID
	Name               string
	Tag                string
	TemplateType       string
	ImageRef           string
	RootfsS3Key        *string
	WorkspaceS3Key     *string
	Dockerfile         *string
	IsPublic           bool
	Status             string
	CreatedBySandboxID *string
	CellsAvailable     []string
}

func (c *Client) RegisterTemplate(ctx context.Context, args RegisterArgs) (*db.DBTemplate, error) {
	payload := map[string]any{
		"name":         args.Name,
		"tag":          args.Tag,
		"templateType": args.TemplateType,
		"imageRef":     args.ImageRef,
		"isPublic":     args.IsPublic,
		"status":       args.Status,
	}
	if args.ID != uuid.Nil {
		payload["id"] = args.ID.String()
	}
	if args.OrgID != nil {
		payload["orgID"] = args.OrgID.String()
	}
	if args.RootfsS3Key != nil {
		payload["rootfsS3Key"] = *args.RootfsS3Key
	}
	if args.WorkspaceS3Key != nil {
		payload["workspaceS3Key"] = *args.WorkspaceS3Key
	}
	if args.Dockerfile != nil {
		payload["dockerfile"] = *args.Dockerfile
	}
	if args.CreatedBySandboxID != nil {
		payload["createdBySandboxID"] = *args.CreatedBySandboxID
	}
	if args.CellsAvailable != nil {
		payload["cellsAvailable"] = args.CellsAvailable
	}
	body, _ := json.Marshal(payload)
	respBody, status, err := c.do(ctx, "POST", "/internal/templates", body)
	if err != nil {
		return nil, err
	}
	if status != 201 {
		return nil, fmt.Errorf("edge status %d: %s", status, string(respBody))
	}
	var resp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(respBody, &resp)
	// Re-fetch so the caller gets a fully-formed DBTemplate.
	if args.OrgID != nil {
		return c.LookupTemplate(ctx, *args.OrgID, args.Name)
	}
	return c.LookupTemplate(ctx, uuid.Nil, args.Name)
}

// UpdateTemplateStatus flips a template's status (e.g. processing → ready).
func (c *Client) UpdateTemplateStatus(ctx context.Context, id uuid.UUID, status string) error {
	body, _ := json.Marshal(map[string]string{"status": status})
	respBody, code, err := c.do(ctx, "PUT", "/internal/templates/"+escapeQuery(id.String())+"/status", body)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("edge status %d: %s", code, string(respBody))
	}
	return nil
}

// ── Secret stores ──────────────────────────────────────────────────────

type edgeStore struct {
	ID              string   `json:"id"`
	OrgID           string   `json:"orgID"`
	Name            string   `json:"name"`
	EgressAllowlist []string `json:"egressAllowlist"`
	CreatedAt       int64    `json:"createdAt"`
	UpdatedAt       int64    `json:"updatedAt"`
}

type edgeEntry struct {
	Name              string   `json:"name"`
	AllowedHosts      []string `json:"allowedHosts"`
	EncryptedValueB64 string   `json:"encryptedValueB64"`
	CreatedAt         int64    `json:"createdAt"`
	UpdatedAt         int64    `json:"updatedAt"`
}

type edgeStoreBundle struct {
	Store   edgeStore   `json:"store"`
	Entries []edgeEntry `json:"entries"`
}

// SecretStoreBundle is what callers consume: the store metadata plus already-
// decrypted entries. The client handles base64 decode + AES-GCM decrypt so
// the calling code looks the same as the old `s.store.GetSecretStoreByName +
// DecryptSecretEntries` pair.
type SecretStoreBundle struct {
	Store   db.SecretStore
	Entries []db.DecryptedSecret
}

func (c *Client) lookupStoreByQuery(ctx context.Context, query string, enc *crypto.Encryptor) (*SecretStoreBundle, error) {
	body, status, err := c.do(ctx, "GET", query, nil)
	if err != nil {
		return nil, err
	}
	if status == 404 {
		return nil, ErrNotFound
	}
	if status != 200 {
		return nil, fmt.Errorf("edge status %d: %s", status, string(body))
	}
	var raw edgeStoreBundle
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode store: %w", err)
	}
	storeID, err := uuid.Parse(raw.Store.ID)
	if err != nil {
		return nil, fmt.Errorf("parse store id: %w", err)
	}
	orgID, err := uuid.Parse(raw.Store.OrgID)
	if err != nil {
		return nil, fmt.Errorf("parse store org_id: %w", err)
	}
	out := &SecretStoreBundle{
		Store: db.SecretStore{
			ID:              storeID,
			OrgID:           orgID,
			Name:            raw.Store.Name,
			EgressAllowlist: raw.Store.EgressAllowlist,
			CreatedAt:       time.Unix(raw.Store.CreatedAt, 0),
			UpdatedAt:       time.Unix(raw.Store.UpdatedAt, 0),
		},
	}
	for _, e := range raw.Entries {
		ct, err := base64.StdEncoding.DecodeString(e.EncryptedValueB64)
		if err != nil {
			return nil, fmt.Errorf("base64 entry %q: %w", e.Name, err)
		}
		pt, err := enc.Decrypt(ct)
		if err != nil {
			return nil, fmt.Errorf("decrypt entry %q: %w", e.Name, err)
		}
		out.Entries = append(out.Entries, db.DecryptedSecret{
			Name:         e.Name,
			Value:        string(pt),
			AllowedHosts: e.AllowedHosts,
		})
	}
	return out, nil
}

// LookupSecretStore fetches a secret store by (orgID, name) and returns it
// with all entries decrypted. Replaces the legacy
// `s.store.GetSecretStoreByName + DecryptSecretEntries` callsite in
// resolveSecretStoreInto.
func (c *Client) LookupSecretStore(ctx context.Context, orgID uuid.UUID, name string, enc *crypto.Encryptor) (*SecretStoreBundle, error) {
	q := "/internal/secret-stores/by-name?org_id=" + escapeQuery(orgID.String()) + "&name=" + escapeQuery(name)
	return c.lookupStoreByQuery(ctx, q, enc)
}

// LookupSecretStoreByID — same but keyed by uuid, for callsites that already
// have the resolved ID (e.g. the secret-refresh fan-out path).
func (c *Client) LookupSecretStoreByID(ctx context.Context, id uuid.UUID, enc *crypto.Encryptor) (*SecretStoreBundle, error) {
	return c.lookupStoreByQuery(ctx, "/internal/secret-stores/"+escapeQuery(id.String()), enc)
}

// ── helpers ────────────────────────────────────────────────────────────

// escapeQuery is a minimal URL-querystring encoder for values we control
// (UUIDs, store names). Avoids pulling net/url just for QueryEscape.
func escapeQuery(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
