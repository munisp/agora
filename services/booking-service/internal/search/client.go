package search

// Minimal OpenSearch HTTP client — stdlib net/http ONLY (SPEC-W119 §2:
// no new third-party Go module dependencies). Speaks the small REST subset
// the search surface and the projection syncer need: cluster health, _search,
// doc upsert, doc delete.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxResponseBytes caps a cluster answer (search pages are <= 100 docs; the
// cap is a safety valve, never an operational bound).
const maxResponseBytes = 32 << 20

// Client is a tiny OpenSearch REST client.
type Client struct {
	base string
	hc   *http.Client
}

// NewClient builds a client for addr ("http://opensearch:9200"); the
// trailing slash is trimmed. Requests carry a 15s timeout — search calls
// sit behind HTTP handlers and must fail fast rather than hang a request.
func NewClient(addr string) *Client {
	return &Client{
		base: strings.TrimRight(addr, "/"),
		hc:   &http.Client{Timeout: 15 * time.Second},
	}
}

// Error is a non-2xx cluster answer.
type Error struct {
	Status int
	Body   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("opensearch: HTTP %d: %s", e.Status, e.Body)
}

// do performs one request. There is deliberately no backoff machinery: the
// only retry is ONE immediate re-attempt on a transport-level connection
// reset (a half-open keep-alive racing a cluster restart), everything else
// surfaces to the caller (503 fail-closed at the HTTP layer; logged + next
// tick for the syncer).
func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("opensearch: marshal: %w", err)
		}
		rdr = bytes.NewReader(raw)
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
		if err != nil {
			return nil, fmt.Errorf("opensearch: request: %w", err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			lastErr = err
			// One immediate retry on connection reset only.
			if attempt == 0 && strings.Contains(err.Error(), "connection reset") {
				continue
			}
			return nil, fmt.Errorf("opensearch: %s %s: %w", method, path, err)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		resp.Body.Close() //nolint:errcheck
		if err != nil {
			return nil, fmt.Errorf("opensearch: read: %w", err)
		}
		if resp.StatusCode >= 300 {
			snippet := string(raw)
			if len(snippet) > 512 {
				snippet = snippet[:512]
			}
			return nil, &Error{Status: resp.StatusCode, Body: snippet}
		}
		_ = lastErr
		return raw, nil
	}
	return nil, fmt.Errorf("opensearch: %s %s: %w", method, path, lastErr)
}

// Ping answers cluster health (GET /_cluster/health).
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodGet, "/_cluster/health", nil)
	return err
}

// Search runs body against index and returns the hit _source docs plus the
// total hit count (track_total_hits is set by the query builders).
func (c *Client) Search(ctx context.Context, index string, body map[string]any) ([]json.RawMessage, int64, error) {
	raw, err := c.do(ctx, http.MethodPost, "/"+index+"/_search", body)
	if err != nil {
		return nil, 0, err
	}
	var parsed struct {
		Hits struct {
			Total struct {
				Value int64 `json:"value"`
			} `json:"total"`
			Hits []struct {
				Source json.RawMessage `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, 0, fmt.Errorf("opensearch: decode search response: %w", err)
	}
	hits := make([]json.RawMessage, 0, len(parsed.Hits.Hits))
	for _, h := range parsed.Hits.Hits {
		hits = append(hits, h.Source)
	}
	return hits, parsed.Hits.Total.Value, nil
}

// IndexDoc upserts doc as /{index}/_doc/{id}.
func (c *Client) IndexDoc(ctx context.Context, index, id string, doc any) error {
	_, err := c.do(ctx, http.MethodPut, "/"+index+"/_doc/"+id, doc)
	return err
}

// DeleteDoc removes /{index}/_doc/{id}; a 404 is tolerated (the doc may
// never have been indexed — deletes must be idempotent for the purge
// sweep).
func (c *Client) DeleteDoc(ctx context.Context, index, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/"+index+"/_doc/"+id, nil)
	if err != nil {
		if osErr, ok := err.(*Error); ok && osErr.Status == http.StatusNotFound {
			return nil
		}
		return err
	}
	return nil
}
