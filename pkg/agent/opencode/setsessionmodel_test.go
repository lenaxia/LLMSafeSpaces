package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// SetSessionModel is the V2 model-fidelity mechanism (design 0054 R4):
// the V2 prompt endpoint strips per-prompt overrides entirely (verified
// live 2026-08-29 — a prompt with model {id:"totally-bogus-model"}
// admitted cleanly and ran on the session default), so the delivery path
// applies the selection to the SESSION before admission.

func TestSetSessionModel_IDShapeOnFloor(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("POST", "/api/session/ses_1/model", `null`, http.StatusNoContent)

	a := newTestAdapter(t, srv.Server)
	require.NoError(t, a.SetSessionModel(context.Background(), "u-1", "ws-1", "ses_1",
		&session.ModelRef{ID: "glm-5.3", Provider: "thekaocloud"}))

	body := srv.bodies["POST /api/session/ses_1/model"]
	require.NotNil(t, body, "model-set POST must fire")
	var sent map[string]any
	require.NoError(t, json.Unmarshal(body, &sent))
	m, ok := sent["model"].(map[string]any)
	require.True(t, ok, "body nests the ref under model")
	// The fake serves no probe-distinguishable /model errors, so the
	// capability probe is indeterminate → the pinned floor shape (id-key).
	assert.Equal(t, "glm-5.3", m["id"])
	assert.Equal(t, "thekaocloud", m["providerID"])
}

func TestSetSessionModel_NilAndEmptyAreNoOps(t *testing.T) {
	srv := newFakeOpencode(t)
	a := newTestAdapter(t, srv.Server)
	require.NoError(t, a.SetSessionModel(context.Background(), "u-1", "ws-1", "ses_1", nil))
	require.NoError(t, a.SetSessionModel(context.Background(), "u-1", "ws-1", "ses_1", &session.ModelRef{}))
	assert.Empty(t, srv.requests, "nil/empty refs must not hit the agent")
}

func TestSetSessionModel_UnexpressibleRefRejected(t *testing.T) {
	srv := newFakeOpencode(t)
	a := newTestAdapter(t, srv.Server)
	// modelOverride rejects bare refs (no provider-authoritative split).
	err := a.SetSessionModel(context.Background(), "u-1", "ws-1", "ses_1",
		&session.ModelRef{ID: "bare-model-id"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpressible")
	assert.Empty(t, srv.requests, "rejected refs must not hit the agent")
}

func TestSetSessionModel_ServerErrorSurfaces(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("POST", "/api/session/ses_1/model", `{"error":"nope"}`, http.StatusBadGateway)

	a := newTestAdapter(t, srv.Server)
	err := a.SetSessionModel(context.Background(), "u-1", "ws-1", "ses_1",
		&session.ModelRef{ID: "glm-5.3", Provider: "thekaocloud"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/model")
}
