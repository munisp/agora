package embed

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEmbed_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/embeddings", r.URL.Path)
		var req embeddingsRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		require.Equal(t, "nomic-embed-text", req.Model)
		require.Equal(t, "Adaeze Okafor | web", req.Input)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"embedding": []float32{0.5, 0.5}}},
		})
	}))
	defer srv.Close()

	c := NewOllama(srv.URL, "nomic-embed-text", srv.Client())
	vec, err := c.Embed(context.Background(), "Adaeze Okafor | web")
	require.NoError(t, err)
	require.Equal(t, []float32{0.5, 0.5}, vec)
	require.False(t, c.Degraded())
}

func TestEmbed_HTTPError_Degraded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewOllama(srv.URL, "nomic-embed-text", srv.Client())
	_, err := c.Embed(context.Background(), "x")
	require.ErrorIs(t, err, ErrDegraded)
	require.True(t, c.Degraded())
}

func TestEmbed_Unreachable_DegradedThenRecovers(t *testing.T) {
	c := NewOllama("http://127.0.0.1:1/v1", "nomic-embed-text", nil)
	_, err := c.Embed(context.Background(), "x")
	require.ErrorIs(t, err, ErrDegraded)
	require.True(t, c.Degraded())

	// A later healthy backend clears the flag (recovery path).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"embedding": []float32{1}}},
		})
	}))
	defer srv.Close()
	c2 := NewOllama(srv.URL, "nomic-embed-text", srv.Client())
	_, err = c2.Embed(context.Background(), "x")
	require.NoError(t, err)
	require.False(t, c2.Degraded())
}

func TestEmbed_MalformedResponse_Degraded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	c := NewOllama(srv.URL, "nomic-embed-text", srv.Client())
	_, err := c.Embed(context.Background(), "x")
	require.ErrorIs(t, err, ErrDegraded)
}

func TestEmbed_EmptyInput_NotCalled(t *testing.T) {
	c := NewOllama("http://127.0.0.1:1/v1", "m", nil)
	_, err := c.Embed(context.Background(), "  ")
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrDegraded), "empty input is a caller bug, not degradation")
}
