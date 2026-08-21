package zen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

type Client struct {
	BaseURL string
	Key     string
	HTTP    *http.Client
}

func New(baseURL, key string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Key: key, HTTP: &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}}
}

// Do performs an authenticated request against the Zen API. A nil body means
// no request body is sent.
func (c *Client) Do(ctx context.Context, method, path string, body []byte, accept string) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return nil, err
	}
	if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	req.Header.Set("User-Agent", "opencode-proxy")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request to OpenCode Zen failed: %w", err)
	}
	return resp, nil
}

type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

type modelList struct {
	Data []Model `json:"data"`
}

// Models lists the models the account can use through Zen. The catalog
// endpoint is public, so this works with or without a key.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	resp, err := c.Do(ctx, http.MethodGet, "/models", nil, "application/json")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{Status: resp.StatusCode, Body: data}
	}
	var list modelList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}
	return list.Data, nil
}

type HTTPError struct {
	Status int
	Body   []byte
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("OpenCode Zen returned HTTP %d: %s", e.Status, strings.TrimSpace(string(e.Body)))
}

func DefaultHTTPClient() *http.Client {
	return &http.Client{Transport: otelhttp.NewTransport(&http.Transport{Proxy: http.ProxyFromEnvironment, MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: 10 * time.Minute})}
}
