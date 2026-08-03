// Package daprc is a minimal Dapr HTTP API client implemented with net/http
// only (no Dapr SDK dependency) — the same shape as identity-service's
// daprc, plus an InvokeGET helper used for the identity consent gate.
package daprc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
// component topic. Content-Type application/cloudevents+json is used so
// daprd forwards the envelope as-is.
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

// InvokeGET performs Dapr service-to-service invocation with the GET verb:
// GET /v1.0/invoke/{appID}/method/{method}?{query}, forwarding the given
// headers (e.g. X-Tenant-ID). It returns the raw status code and body so the
// caller can map upstream 403s to its own consent-denied response.
func (c *Client) InvokeGET(ctx context.Context, appID, method string, query url.Values, headers map[string]string) (int, []byte, error) {
	u := fmt.Sprintf("%s/v1.0/invoke/%s/method/%s", c.baseURL, appID, method)
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return DoGET(ctx, c.hc, u, headers)
}

// DoGET issues a GET with headers against a fully-qualified URL, returning
// the status code and a bounded body. Shared by the Dapr invoke path and
// the direct IDENTITY_BASE_URL path.
func DoGET(ctx context.Context, hc *http.Client, u string, headers map[string]string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("get %s: %w", u, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, b, nil
}
