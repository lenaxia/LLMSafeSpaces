package handlers

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/outbox"
)

// R4 (design 0054): model fidelity under V2. The V2 prompt endpoint
// strips per-prompt overrides (verified live 2026-08-29), so the outbox
// must apply the entry's model to the SESSION before admission — the
// same mechanism the SPA uses.

func TestOutboxDeliver_V2ModelAppliedToSessionBeforeAdmission(t *testing.T) {
	shrinkOutboxTimers(t)
	shrinkV2PromotionTimers(t)
	backend := &fakeAgentBackend{persistFirst: true, promoteDelay: 0}
	env := newVerifyEnv(t, backend)
	env.handler.SetV2Delivery(true)

	// Accept with a model selector — the raw shape the prompt route stores.
	body := `{"clientMessageID":"cm-m4","parts":[{"type":"text","text":"model four"}],"model":{"modelID":"glm-5.3","providerID":"thekaocloud"}}`
	w := postPrompt(t, env, body)
	require.Equal(t, http.StatusAccepted, w.Code)

	require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"))
	assert.Empty(t, listOutbox(t, env), "delivery completes at promotion")

	backend.mu.Lock()
	order := append([]string{}, backend.callOrder...)
	modelID := backend.lastModelID
	backend.mu.Unlock()
	require.Equal(t, []string{"model", "admit"}, order,
		"the session model is set exactly once, BEFORE the single admission (the V2 prompt endpoint strips per-prompt overrides)")
	assert.Equal(t, "glm-5.3", modelID,
		"the picked model is what reaches the session — the fidelity regression was every turn silently running the default")
}

func TestOutboxDeliver_V2ModelSetFailureClassifications(t *testing.T) {
	t.Run("model-set 5xx is definitive — bounded retry, no admission", func(t *testing.T) {
		shrinkOutboxTimers(t)
		shrinkV2PromotionTimers(t)
		backend := &fakeAgentBackend{modelSetStatus: http.StatusBadGateway}
		env := newVerifyEnv(t, backend)
		env.handler.SetV2Delivery(true)

		w := postPrompt(t, env, `{"clientMessageID":"cm-m5","parts":[{"type":"text","text":"model five"}],"model":{"modelID":"glm-5.3","providerID":"thekaocloud"}}`)
		require.Equal(t, http.StatusAccepted, w.Code)

		require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"))
		entries := listOutbox(t, env)
		require.Len(t, entries, 1)
		assert.Equal(t, outbox.StatusPending, entries[0].Status, "HTTP rejection is definitive: retry with backoff")
		backend.mu.Lock()
		admits := backend.admits
		backend.mu.Unlock()
		assert.Zero(t, admits, "no admission when the model cannot be set — the turn would run on the wrong model")
	})

	t.Run("no model on entry — no model-set call", func(t *testing.T) {
		shrinkOutboxTimers(t)
		shrinkV2PromotionTimers(t)
		backend := &fakeAgentBackend{persistFirst: true, promoteDelay: 0}
		env := newVerifyEnv(t, backend)
		env.handler.SetV2Delivery(true)

		w := postPrompt(t, env, `{"clientMessageID":"cm-m6","parts":[{"type":"text","text":"model six"}]}`)
		require.Equal(t, http.StatusAccepted, w.Code)
		require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"))

		backend.mu.Lock()
		models := backend.modelSets
		backend.mu.Unlock()
		assert.Zero(t, models, "entries without a model must not touch the session model")
	})
}

// R5/G2 slice: the busy/idle DERIVATION from V2 turn events was deleted
// with onV2RawEvent (US-69.11: session state derives from the usage
// bridge and statusz). What survives is the bounded-decay contract the
// reaper (v2BusyReap, still Start()-wired) consumes.
