package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
)

type contextKey struct{}
type sessionsContextKey struct{}

// Client is an HTTP client for the OpenComputer API.
type Client struct {
	baseURL    string
	apiKey     string
	token      string // Bearer token for direct worker access
	httpClient *http.Client
}

// APIError represents an error response from the API.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("API error %d: %s", e.StatusCode, e.Message)
}

// New creates a new API client.
func New(baseURL, apiKey string) *Client {
	baseURL = strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(baseURL, "/api") {
		baseURL += "/api"
	}
	return &Client{
		baseURL:    baseURL,
		apiKey:     apiKey,
		httpClient: &http.Client{},
	}
}

// WithClient stores the client in the context.
func WithClient(ctx context.Context, c *Client) context.Context {
	return context.WithValue(ctx, contextKey{}, c)
}

// FromContext retrieves the client from the context.
func FromContext(ctx context.Context) *Client {
	return ctx.Value(contextKey{}).(*Client)
}

// WithSessionsClient stores the sessions-api client in the context.
func WithSessionsClient(ctx context.Context, c *Client) context.Context {
	return context.WithValue(ctx, sessionsContextKey{}, c)
}

// SessionsFromContext retrieves the sessions-api client from the context.
func SessionsFromContext(ctx context.Context) *Client {
	v := ctx.Value(sessionsContextKey{})
	if v == nil {
		return nil
	}
	return v.(*Client)
}

// NewSessionsAPI creates a client for the sessions-api service.
// Uses X-API-Key auth but no /api prefix (sessions-api routes are /v1/...).
func NewSessionsAPI(baseURL, apiKey string) *Client {
	baseURL = strings.TrimRight(baseURL, "/")
	return &Client{
		baseURL:    baseURL,
		apiKey:     apiKey,
		httpClient: &http.Client{},
	}
}

// NewWorker creates a client that authenticates with a Bearer token directly to a worker.
// Worker routes have no /api prefix (unlike the control plane).
func NewWorker(connectURL, token string) *Client {
	connectURL = strings.TrimRight(connectURL, "/")
	return &Client{
		baseURL:    connectURL,
		token:      token,
		httpClient: &http.Client{},
	}
}

func (c *Client) headers() http.Header {
	h := http.Header{}
	if c.token != "" {
		h.Set("Authorization", "Bearer "+c.token)
	} else if c.apiKey != "" {
		h.Set("X-API-Key", c.apiKey)
	}
	return h
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	for k, v := range c.headers() {
		req.Header[k] = v
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var errResp struct {
			Error string `json:"error"`
		}
		msg := string(body)
		if json.Unmarshal(body, &errResp) == nil && errResp.Error != "" {
			msg = errResp.Error
		}
		return nil, &APIError{StatusCode: resp.StatusCode, Message: msg}
	}
	return resp, nil
}

// Get performs a GET request and decodes the JSON response.
func (c *Client) Get(ctx context.Context, path string, result interface{}) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if result != nil {
		return json.NewDecoder(resp.Body).Decode(result)
	}
	return nil
}

// Post performs a POST request with a JSON body and decodes the response.
func (c *Client) Post(ctx context.Context, path string, body, result interface{}) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+path, bodyReader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if result != nil {
		return json.NewDecoder(resp.Body).Decode(result)
	}
	return nil
}

// PostStream performs a POST request and returns the raw response so the
// caller can stream chunks (e.g. SSE / chat-completions). Caller is
// responsible for closing resp.Body.
func (c *Client) PostStream(ctx context.Context, path string, body interface{}) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "text/event-stream")
	return c.do(req)
}

// Put performs a PUT request with a raw body.
func (c *Client) Put(ctx context.Context, path string, body io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, "PUT", c.baseURL+path, body)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// PutJSON performs a PUT request with a JSON body and decodes the response.
func (c *Client) PutJSON(ctx context.Context, path string, body, result interface{}) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, "PUT", c.baseURL+path, bodyReader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if result != nil {
		return json.NewDecoder(resp.Body).Decode(result)
	}
	return nil
}

// Delete performs a DELETE request.
func (c *Client) Delete(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, "DELETE", c.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// DeleteIgnoreNotFound performs a DELETE request, ignoring 404 errors.
func (c *Client) DeleteIgnoreNotFound(ctx context.Context, path string) error {
	err := c.Delete(ctx, path)
	if err != nil {
		if apiErr, ok := err.(*APIError); ok && apiErr.StatusCode == 404 {
			return nil
		}
	}
	return err
}

// DialWebSocket opens a WebSocket connection.
func (c *Client) DialWebSocket(ctx context.Context, path string) (*websocket.Conn, error) {
	wsURL := c.baseURL + path
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)

	dialer := websocket.Dialer{}
	conn, _, err := dialer.DialContext(ctx, wsURL, c.headers())
	if err != nil {
		return nil, fmt.Errorf("websocket dial: %w", err)
	}
	return conn, nil
}

// PostRaw performs a POST with no body and returns no result (for simple actions).
func (c *Client) PostRaw(ctx context.Context, path string) error {
	return c.Post(ctx, path, nil, nil)
}

// BaseURL returns the client's base URL.
func (c *Client) BaseURL() string {
	return c.baseURL
}
