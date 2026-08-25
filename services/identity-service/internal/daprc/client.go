// Package daprc is a minimal Dapr HTTP API client implemented with net/http
// only (no Dapr SDK dependency). It supports pub/sub publish, service
// invocation and state store operations against a daprd sidecar.
package daprc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client talks to a Dapr sidecar over HTTP.
type Client struct {
	baseURL string
	hc      *http.Client
}

// New builds a Client for the given sidecar host/port.
func New(host string, port int) *Client {
	return &Client{
		baseURL: fmt.Sprintf("http://%s:%d", host, port),
		hc:      &http.Client{Timeout: 15 * time.Second},
	}
}

// PublishEvent publishes data (typically a CloudEvents envelope) to a pubsub
// component topic. Content-Type application/cloudevents+json is used so daprd
// forwards the envelope as-is.
func (c *Client) PublishEvent(ctx context.Context, pubsub, topic string, data any) error {
	body, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	url := fmt.Sprintf("%s/v1.0/publish/%s/%s", c.baseURL, pubsub, topic)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/cloudevents+json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("publish %s/%s: %w", pubsub, topic, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("publish %s/%s: status %d: %s", pubsub, topic, resp.StatusCode, string(b))
	}
	return nil
}

// InvokeService performs Dapr service-to-service invocation:
// POST /v1.0/invoke/{appID}/method/{method}.
func (c *Client) InvokeService(ctx context.Context, appID, method string, payload any, out any) error {
	return c.InvokeServiceWithHeaders(ctx, appID, method, payload, nil, out)
}

// InvokeServiceWithHeaders is InvokeService with optional extra request
// headers (SPEC-W44 K2): internal-token-gated targets (e.g. notification-
// worker's /dev/* triggers) require X-Internal-Token, which Dapr HTTP
// invoke passes through verbatim. Nil/empty headers behave exactly like
// InvokeService.
func (c *Client) InvokeServiceWithHeaders(ctx context.Context, appID, method string, payload any, headers map[string]string, out any) error {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal invoke payload: %w", err)
		}
		body = bytes.NewReader(b)
	}
	url := fmt.Sprintf("%s/v1.0/invoke/%s/method/%s", c.baseURL, appID, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("invoke %s/%s: %w", appID, method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("invoke %s/%s: status %d: %s", appID, method, resp.StatusCode, string(b))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
			return fmt.Errorf("decode invoke response: %w", err)
		}
	}
	return nil
}

// GetState fetches a key from a Dapr state store, unmarshalling into out.
// Returns (false, nil) when the key does not exist.
func (c *Client) GetState(ctx context.Context, store, key string, out any) (bool, error) {
	url := fmt.Sprintf("%s/v1.0/state/%s/%s", c.baseURL, store, key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return false, fmt.Errorf("state get %s/%s: %w", store, key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return false, nil
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return false, fmt.Errorf("state get %s/%s: status %d: %s", store, key, resp.StatusCode, string(b))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return false, fmt.Errorf("decode state: %w", err)
	}
	return true, nil
}

// SaveState writes key/value pairs to a Dapr state store.
func (c *Client) SaveState(ctx context.Context, store string, items map[string]any) error {
	type kv struct {
		Key   string `json:"key"`
		Value any    `json:"value"`
	}
	arr := make([]kv, 0, len(items))
	for k, v := range items {
		arr = append(arr, kv{Key: k, Value: v})
	}
	body, err := json.Marshal(arr)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/v1.0/state/%s", c.baseURL, store)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("state save %s: %w", store, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("state save %s: status %d: %s", store, resp.StatusCode, string(b))
	}
	return nil
}
