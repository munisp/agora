// Package embed computes entity-resolution embeddings via Ollama's
// OpenAI-compatible API (SPEC-W28 §1: OLLAMA_BASE_URL, default
// http://localhost:11434/v1; model nomic-embed-text). Unreachable Ollama is
// NOT an error: the client reports Degraded and the consumer skips
// embedding-based merge proposals (exact phone_hash merges still work).
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Embedder computes one embedding vector for a text.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	// Degraded reports whether the backend is currently considered
	// unreachable (last call failed) — used for metrics/health surfacing.
	Degraded() bool
}

// ErrDegraded is returned when Ollama is unreachable; callers treat it as
// "skip embeddings" (graceful degradation, SPEC-W28 §4), never a poison
// message.
var ErrDegraded = fmt.Errorf("ollama embeddings unavailable (degraded)")

// Ollama is an Embedder over the OpenAI-compatible /embeddings endpoint.
type Ollama struct {
	base   string
	model  string
	http   *http.Client
	failed atomic.Bool
}

// NewOllama builds the client. baseURL is the OpenAI-compatible base
// ("/v1" suffix included), e.g. http://localhost:11434/v1.
func NewOllama(baseURL, model string, httpClient *http.Client) *Ollama {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Ollama{
		base:  strings.TrimRight(baseURL, "/"),
		model: model,
		http:  httpClient,
	}
}

// Degraded implements Embedder.
func (o *Ollama) Degraded() bool { return o.failed.Load() }

// embeddingsRequest mirrors POST {base}/embeddings (OpenAI-compatible).
type embeddingsRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embeddingsResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed implements Embedder. Any transport/HTTP/shape failure flips the
// degraded flag and returns ErrDegraded-wrapped so the consumer can skip
// the entity-resolution branch without dead-lettering the event.
func (o *Ollama) Embed(ctx context.Context, text string) ([]float32, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("embedding input is empty")
	}
	body, err := json.Marshal(embeddingsRequest{Model: o.model, Input: text})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.base+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.http.Do(req)
	if err != nil {
		o.failed.Store(true)
		return nil, fmt.Errorf("%w: %v", ErrDegraded, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		o.failed.Store(true)
		return nil, fmt.Errorf("%w: response unreadable", ErrDegraded)
	}
	if resp.StatusCode != http.StatusOK {
		o.failed.Store(true)
		return nil, fmt.Errorf("%w: ollama answered %d", ErrDegraded, resp.StatusCode)
	}
	var decoded embeddingsResponse
	if err := json.Unmarshal(raw, &decoded); err != nil || len(decoded.Data) == 0 || len(decoded.Data[0].Embedding) == 0 {
		o.failed.Store(true)
		return nil, fmt.Errorf("%w: malformed embeddings response", ErrDegraded)
	}
	o.failed.Store(false) // a successful call clears the degraded flag
	return decoded.Data[0].Embedding, nil
}
