package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// Resolution statuses (SPEC-W12 §5 response contract).
const (
	StatusVerified = "verified"
	StatusMismatch = "mismatch"
	StatusPending  = "pending"
)

// ID types accepted by POST /v1/kyc/resolve.
var validIDTypes = map[string]bool{"bvn": true, "nin": true}

// Resolver resolves one BVN/NIN against an identity provider. The live
// provider is the default (SPEC-W34 GF8 — KYC_MOCK defaults off);
// MockResolver is explicit dev opt-in (KYC_MOCK=1); LiveResolver is the
// ASSUMPTION-shaped live client behind KYC_PROVIDER_URL (no live keys in
// this wave — docs/kyc.md).
type Resolver interface {
	Resolve(ctx context.Context, idType, idValue string) (string, error)
}

// MockResolver is the deterministic contract mock (SPEC-W12 §5): id_value
// all digits and length >= 10 resolves "verified"; anything else resolves
// "mismatch". It never returns "pending" and never errors.
type MockResolver struct{}

// Resolve implements Resolver.
func (MockResolver) Resolve(_ context.Context, _, idValue string) (string, error) {
	if isAllDigits(idValue) && len(idValue) >= 10 {
		return StatusVerified, nil
	}
	return StatusMismatch, nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// LiveResolver calls a generic identity-resolution provider.
//
// ASSUMPTION (no live keys in this wave): the provider accepts
// POST {base}/resolve {"id_type":"bvn"|"nin","id_value":"..."} with an
// Authorization: Bearer {apiKey} header and answers
// {"status":"verified"|"mismatch"|"pending"}. Any transport failure,
// non-2xx, or unknown status maps to "pending" (the caller retries later)
// rather than a wrong hard verdict.
type LiveResolver struct {
	base   string
	apiKey string
	hc     *http.Client
}

// NewLiveResolver builds a LiveResolver with the resolution timeout budget.
func NewLiveResolver(base, apiKey string, timeout time.Duration) *LiveResolver {
	return &LiveResolver{base: base, apiKey: apiKey, hc: &http.Client{Timeout: timeout}}
}

// Resolve implements Resolver.
func (l *LiveResolver) Resolve(ctx context.Context, idType, idValue string) (string, error) {
	body, err := json.Marshal(map[string]string{"id_type": idType, "id_value": idValue})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.base+"/resolve", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if l.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+l.apiKey)
	}
	resp, err := l.hc.Do(req)
	if err != nil {
		return StatusPending, fmt.Errorf("provider request: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return StatusPending, fmt.Errorf("provider status %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return StatusPending, fmt.Errorf("provider decode: %w", err)
	}
	switch out.Status {
	case StatusVerified, StatusMismatch, StatusPending:
		return out.Status, nil
	default:
		return StatusPending, fmt.Errorf("provider returned unknown status %q", out.Status)
	}
}

// hashIDValue returns the SHA-256 hex digest of a raw BVN/NIN. Only the
// digest is ever stored (kyc_audit.id_value_hash) — NDPA data minimization.
func hashIDValue(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// referenceFor derives the deterministic resolution reference (uuid5 of
// tenant+subject+id). Deterministic per contract: the same request replayed
// yields the same reference, so callers can correlate retries.
func referenceFor(tenantID uuid.UUID, subject, idType, idValueHash string) string {
	return "kyc_" + uuid.NewSHA1(uuid.NameSpaceURL,
		[]byte(tenantID.String()+"|"+subject+"|"+idType+"|"+idValueHash)).String()
}
