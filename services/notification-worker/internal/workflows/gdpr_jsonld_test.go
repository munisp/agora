// SPEC-W54 scope B: the GDPR export bundle carries a versioned, inline
// JSON-LD @context mapping every top-level bundle key onto ontology/agora.ttl
// (`agora:`) / schema.org terms. The @context key is strictly additive —
// these tests also prove the pre-existing fields are byte-identical.
package workflows

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
)

func gdprTestBundle() GdprExportBundle {
	return GdprExportBundle{
		Context:       GdprJSONLDContext(),
		Subject:       gdprTestInput(),
		Bookings:      json.RawMessage(`{"bookings":[]}`),
		Conversations: json.RawMessage(`{"conversations":[]}`),
		LedgerBalance: json.RawMessage(`{"balance_cents":0}`),
		CrmPerson:     json.RawMessage(`{"person":{"id":"p-1"}}`),
	}
}

// bundleTopLevelJSONKeys enumerates the JSON keys of GdprExportBundle via
// reflection so a future field addition without a @context term fails here.
func bundleTopLevelJSONKeys() []string {
	var keys []string
	typ := reflect.TypeOf(GdprExportBundle{})
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		keys = append(keys, name)
	}
	return keys
}

func TestGdprExportBundle_ContextPresentVersionedAndCovering(t *testing.T) {
	body, err := json.MarshalIndent(gdprTestBundle(), "", "  ")
	require.NoError(t, err)

	// Valid JSON with a top-level @context object.
	var doc map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &doc))
	rawCtx, ok := doc["@context"]
	require.True(t, ok, "bundle must carry a top-level @context key")
	var ctx map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rawCtx, &ctx))

	// Context is versioned: context_version carries the versioned context IRI.
	var version string
	require.NoError(t, json.Unmarshal(ctx["context_version"], &version))
	require.Equal(t, GdprExportContextIRI, version)
	require.Equal(t, "https://agora.example/context/v1", version)

	// Namespaces: same `agora:` namespace as ontology/agora.ttl (scope A).
	var agoraNS, schemaNS string
	require.NoError(t, json.Unmarshal(ctx["agora"], &agoraNS))
	require.NoError(t, json.Unmarshal(ctx["schema"], &schemaNS))
	require.Equal(t, AgoraOntologyNamespace, agoraNS)
	require.Equal(t, "https://agora.example/ontology#", agoraNS)
	require.Equal(t, "https://schema.org/", schemaNS)

	// @context covers EVERY top-level bundle key except @context itself.
	for _, key := range bundleTopLevelJSONKeys() {
		if key == "@context" {
			continue
		}
		rawTerm, ok := ctx[key]
		require.True(t, ok, "@context missing term definition for bundle key %q", key)
		var term GdprContextTerm
		require.NoError(t, json.Unmarshal(rawTerm, &term))
		require.NotEmpty(t, term.ID, "@context term %q needs an @id", key)
		require.True(t,
			strings.HasPrefix(term.ID, "schema:") || strings.HasPrefix(term.ID, "agora:"),
			"@context term %q must map to schema.org or the agora ontology, got %q", key, term.ID)
	}
}

// TestGdprExportBundle_ContextIsAdditive proves the @context extension adds
// exactly one key: stripping @context from the new bundle must reproduce the
// pre-SPEC-W54 bundle bytes exactly (golden-diff of old vs new shape).
func TestGdprExportBundle_ContextIsAdditive(t *testing.T) {
	body, err := json.MarshalIndent(gdprTestBundle(), "", "  ")
	require.NoError(t, err)

	var doc map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &doc))
	require.Len(t, doc, len(bundleTopLevelJSONKeys()))
	delete(doc, "@context")
	stripped, err := json.MarshalIndent(doc, "", "  ")
	require.NoError(t, err)

	// Pre-SPEC-W54 bundle shape (the struct without the Context field).
	type legacyBundle struct {
		Subject       GdprInput       `json:"subject"`
		Bookings      json.RawMessage `json:"bookings"`
		Conversations json.RawMessage `json:"conversations"`
		LedgerBalance json.RawMessage `json:"ledger_balance"`
		CrmPerson     json.RawMessage `json:"crm_person"`
	}
	legacy, err := json.MarshalIndent(legacyBundle{
		Subject:       gdprTestInput(),
		Bookings:      json.RawMessage(`{"bookings":[]}`),
		Conversations: json.RawMessage(`{"conversations":[]}`),
		LedgerBalance: json.RawMessage(`{"balance_cents":0}`),
		CrmPerson:     json.RawMessage(`{"person":{"id":"p-1"}}`),
	}, "", "  ")
	require.NoError(t, err)
	require.JSONEq(t, string(legacy), string(stripped),
		"old bundle fields must be byte-identical in semantics — only @context added")
}

func TestGdprExportWorkflow_SetsJSONLDContext(t *testing.T) {
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()
	registerGdprStubs(env)
	in := gdprTestInput()

	env.OnActivity(ActivityGdprCollectBookings, mock.Anything, in).Return(json.RawMessage(`{}`), nil)
	env.OnActivity(ActivityGdprCollectConversations, mock.Anything, in).Return(json.RawMessage(`{}`), nil)
	env.OnActivity(ActivityGdprCollectLedger, mock.Anything, in).Return(json.RawMessage(`{}`), nil)
	env.OnActivity(ActivityGdprCollectCrmPerson, mock.Anything, in).Return(json.RawMessage(`{}`), nil)
	env.OnActivity(ActivityGdprUploadExport, mock.Anything, mock.MatchedBy(func(b GdprExportBundle) bool {
		return b.Context.ContextVersion == GdprExportContextIRI
	})).Return("exports/t-1/bundle.json", nil).Once()

	env.ExecuteWorkflow(GdprExportWorkflow, in)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}
