package cloudflare

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const baseURL = "https://api.cloudflare.com/client/v4"

// Client is a minimal Cloudflare API client for Custom Hostnames.
type Client struct {
	apiToken string
	zoneID   string
	http     *http.Client
}

// NewClient creates a new Cloudflare API client.
func NewClient(apiToken, zoneID string) *Client {
	return &Client{
		apiToken: apiToken,
		zoneID:   zoneID,
		http:     &http.Client{Timeout: 30 * time.Second},
	}
}

// CustomHostnameResult holds the response from the Custom Hostnames API.
type CustomHostnameResult struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	Status   string `json:"status"`
	SSL      SSL    `json:"ssl"`

	// OwnershipVerification contains the TXT record for domain verification.
	OwnershipVerification *OwnershipRecord `json:"ownership_verification,omitempty"`

	// OwnershipVerificationHTTP is an alternative HTTP-based verification.
	OwnershipVerificationHTTP *struct {
		HTTPUrl  string `json:"http_url"`
		HTTPBody string `json:"http_body"`
	} `json:"ownership_verification_http,omitempty"`
}

// SSL holds SSL-related fields from the Custom Hostname response.
type SSL struct {
	ID                   string      `json:"id"`
	Status               string      `json:"status"`
	Method               string      `json:"method"`
	Type                 string      `json:"type"`
	TxtName              string      `json:"txt_name,omitempty"`
	TxtValue             string      `json:"txt_value,omitempty"`
	ValidationRecords    []TXTRecord `json:"validation_records,omitempty"`
	ValidationErrors     []struct{ Message string } `json:"validation_errors,omitempty"`
}

// TXTRecord represents a DNS TXT record for verification.
type TXTRecord struct {
	Type  string `json:"type"`
	Name  string `json:"txt_name,omitempty"`
	Value string `json:"txt_value,omitempty"`
}

// OwnershipRecord represents the ownership verification record (uses name/value, not txt_name/txt_value).
type OwnershipRecord struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

// CreateCustomHostname creates a custom hostname on the zone.
func (c *Client) CreateCustomHostname(hostname string) (*CustomHostnameResult, error) {
	body := map[string]interface{}{
		"hostname": hostname,
		"ssl": map[string]interface{}{
			"method": "txt",
			"type":   "dv",
			"settings": map[string]interface{}{
				"min_tls_version": "1.2",
			},
		},
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/zones/%s/custom_hostnames", baseURL, c.zoneID)
	resp, err := c.do("POST", url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	var result struct {
		Success bool                 `json:"success"`
		Errors  []cfError            `json:"errors"`
		Result  CustomHostnameResult `json:"result"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	if !result.Success {
		return nil, fmt.Errorf("cloudflare API error: %v", result.Errors)
	}

	return &result.Result, nil
}

// CreateCustomHostnameHTTP creates a custom hostname using HTTP DCV validation.
// Use this for per-sandbox preview hostnames where a wildcard CNAME already points
// to CF, so CF can automatically validate domain ownership via HTTP.
func (c *Client) CreateCustomHostnameHTTP(hostname string) (*CustomHostnameResult, error) {
	body := map[string]interface{}{
		"hostname": hostname,
		"ssl": map[string]interface{}{
			"method":   "http",
			"type":     "dv",
			"wildcard": false,
			"settings": map[string]interface{}{
				"min_tls_version": "1.2",
			},
		},
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/zones/%s/custom_hostnames", baseURL, c.zoneID)
	resp, err := c.do("POST", url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	var result struct {
		Success bool                 `json:"success"`
		Errors  []cfError            `json:"errors"`
		Result  CustomHostnameResult `json:"result"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	if !result.Success {
		return nil, fmt.Errorf("cloudflare API error: %v", result.Errors)
	}

	return &result.Result, nil
}

// GetCustomHostname retrieves the current status of a custom hostname.
func (c *Client) GetCustomHostname(cfHostnameID string) (*CustomHostnameResult, error) {
	url := fmt.Sprintf("%s/zones/%s/custom_hostnames/%s", baseURL, c.zoneID, cfHostnameID)
	resp, err := c.do("GET", url, nil)
	if err != nil {
		return nil, err
	}

	var result struct {
		Success bool                 `json:"success"`
		Errors  []cfError            `json:"errors"`
		Result  CustomHostnameResult `json:"result"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	if !result.Success {
		return nil, fmt.Errorf("cloudflare API error: %v", result.Errors)
	}

	return &result.Result, nil
}

// DeleteCustomHostname removes a custom hostname from the zone.
func (c *Client) DeleteCustomHostname(cfHostnameID string) error {
	url := fmt.Sprintf("%s/zones/%s/custom_hostnames/%s", baseURL, c.zoneID, cfHostnameID)
	resp, err := c.do("DELETE", url, nil)
	if err != nil {
		return err
	}

	var result struct {
		Success bool      `json:"success"`
		Errors  []cfError `json:"errors"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("unmarshal response: %w", err)
	}
	if !result.Success {
		return fmt.Errorf("cloudflare API error: %v", result.Errors)
	}

	return nil
}

// do performs an HTTP request with the Cloudflare API token.
func (c *Client) do(method, url string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("cloudflare server error (status %d): %s", resp.StatusCode, string(data))
	}

	return data, nil
}

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e cfError) String() string {
	return fmt.Sprintf("[%d] %s", e.Code, e.Message)
}
