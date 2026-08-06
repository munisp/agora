package lending

// Optional credit-bureau sidecar client (SPEC-W33 §3 B2, wave W33-B).
//
// This file is an ADDITIVE seam: it changes NOTHING about the existing
// naive rule score — Score() and Store.ComputeScore() behave exactly as
// before and remain the authoritative fallback (invariant I1 honest
// degradation). No existing file was modified to add this seam.
//
// Behavior:
//   - CREDIT_BUREAU_URL unset → local Score() (unchanged).
//   - set → POST {url}/v1/credit/score with the ScoreSignals and a hard
//     500ms budget; on timeout, transport error, non-200, malformed
//     JSON, or a payload failing sanity validation (score outside the
//     bureau band [300,900] or empty model_version) → local Score().
//   - CREDIT_BUREAU_TENANT_ID (optional) is forwarded as X-Tenant-Id —
//     the bureau's dev-mode tenant seam (invariant I4). Unset means no
//     header; a 401 then simply falls back to local rules.
//
// Scale note (documented deviation, see services/credit-bureau/README):
// the local rule score is the naive 0..100 (lending.go:405-426) while
// the bureau blends on the 300..900 band. Callers must use Source /
// ModelVersion to interpret the returned score (I2 provenance): a
// "credit-bureau-sidecar" score is bureau-scale, a "local-rules" score
// is naive-scale. ModelVersion is carried so the lending decision can
// record provenance additively ("heuristic-v1" for rules,
// "credit-ml-v{N}" for the learned blend).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// SidecarModelVersionHeuristic is the provenance id of the local
	// rule path (I2: rules = heuristic-v1).
	SidecarModelVersionHeuristic = "heuristic-v1"
	// SidecarSourceLocal / SidecarSourceSidecar identify which path
	// produced a SidecarDecision (scale + provenance interpretation).
	SidecarSourceLocal   = "local-rules"
	SidecarSourceSidecar = "credit-bureau-sidecar"
	// sidecarTimeout is the hard budget for the bureau call; anything
	// slower degrades to local rules (I1).
	sidecarTimeout = 500 * time.Millisecond
	// Bureau score band (services/credit-bureau: clamp [300,900]).
	sidecarScoreMin = 300
	sidecarScoreMax = 900
)

// SidecarDecision is the additive lending score decision: the score plus
// its provenance. The response model_version is plumbed here ONLY as an
// additive field — nothing in the existing Application/Score path is
// altered.
type SidecarDecision struct {
	Score        int      `json:"score"`
	Reasons      []string `json:"reasons"`
	ModelVersion string   `json:"model_version"`
	Source       string   `json:"source"` // SidecarSourceLocal | SidecarSourceSidecar
}

// sidecarScoreResponse mirrors POST /v1/credit/score's payload (only the
// fields this client consumes).
type sidecarScoreResponse struct {
	Score        int      `json:"score"`
	Reasons      []string `json:"reasons"`
	ModelVersion string   `json:"model_version"`
}

// sidecarHTTPClient is the shared transport (no internal timeout — the
// per-call 500ms budget rides on the request context).
var sidecarHTTPClient = &http.Client{}

// localSidecarDecision is the I1 fallback: the unchanged local rule
// score with honest provenance.
func localSidecarDecision(sig ScoreSignals) SidecarDecision {
	return SidecarDecision{
		Score:        Score(sig),
		Reasons:      []string{},
		ModelVersion: SidecarModelVersionHeuristic,
		Source:       SidecarSourceLocal,
	}
}

// ScoreWithSidecar returns (score, reasons, modelVersion): the credit-
// bureau blended score when the sidecar is configured and healthy, else
// the local Score() unchanged with model_version "heuristic-v1".
func ScoreWithSidecar(ctx context.Context, sig ScoreSignals) (int, []string, string) {
	d := ScoreDecisionWithSidecar(ctx, sig)
	return d.Score, d.Reasons, d.ModelVersion
}

// ScoreDecisionWithSidecar is ScoreWithSidecar with the full additive
// decision struct (source + provenance).
func ScoreDecisionWithSidecar(ctx context.Context, sig ScoreSignals) SidecarDecision {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("CREDIT_BUREAU_URL")), "/")
	if base == "" {
		return localSidecarDecision(sig)
	}
	body, err := json.Marshal(map[string]any{"signals": sig})
	if err != nil {
		return localSidecarDecision(sig)
	}
	callCtx, cancel := context.WithTimeout(ctx, sidecarTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, base+"/v1/credit/score", bytes.NewReader(body))
	if err != nil {
		return localSidecarDecision(sig)
	}
	req.Header.Set("Content-Type", "application/json")
	if tenantID := strings.TrimSpace(os.Getenv("CREDIT_BUREAU_TENANT_ID")); tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	resp, err := sidecarHTTPClient.Do(req)
	if err != nil {
		return localSidecarDecision(sig) // timeout / transport error
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return localSidecarDecision(sig)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return localSidecarDecision(sig)
	}
	var decoded sidecarScoreResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return localSidecarDecision(sig) // malformed JSON
	}
	// Sanity validation: bureau-band score + non-empty provenance.
	if decoded.Score < sidecarScoreMin || decoded.Score > sidecarScoreMax ||
		strings.TrimSpace(decoded.ModelVersion) == "" {
		return localSidecarDecision(sig)
	}
	reasons := decoded.Reasons
	if reasons == nil {
		reasons = []string{}
	}
	return SidecarDecision{
		Score:        decoded.Score,
		Reasons:      reasons,
		ModelVersion: decoded.ModelVersion,
		Source:       SidecarSourceSidecar,
	}
}
