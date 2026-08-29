package handlers

import (
	"net/http"
	"testing"
	"time"

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
	backend.mu.Unlock()
	require.Equal(t, []string{"model", "admit"}, order,
		"the session model is set exactly once, BEFORE the single admission (the V2 prompt endpoint strips per-prompt overrides)")
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

// R5 bridge slice: session.status busy/idle derivation from V2 turn
// events, dual-published like the tracker's own emitters.

func TestV2Bridge_TurnLifecycleStatus(t *testing.T) {
	env := newVerifyEnv(t, &fakeAgentBackend{persistFirst: true})
	events := subscribeStatusEvents(t, env)
	h := env.handler

	// busy: prompted (turn running)
	h.onV2RawEvent("ws-1", "session.next.prompted",
		`{"type":"session.next.prompted","properties":{"sessionID":"ses_1","messageID":"m1","delivery":"queue"}}`)
	// terminal step: idle
	h.onV2RawEvent("ws-1", "session.next.step.ended",
		`{"type":"session.next.step.ended","properties":{"sessionID":"ses_1","finish":"stop"}}`)
	// tool step must NOT idle
	h.onV2RawEvent("ws-1", "session.next.step.ended",
		`{"type":"session.next.step.ended","properties":{"sessionID":"ses_1","finish":"tool"}}`)
	// failed step: idle
	h.onV2RawEvent("ws-1", "session.next.step.failed",
		`{"type":"session.next.step.failed","properties":{"sessionID":"ses_1"}}`)

	var got []string
	deadline := time.After(2 * time.Second)
	for len(got) < 3 {
		select {
		case e := <-events:
			if e.Type == "session.status" {
				got = append(got, e.Status)
			}
		case <-deadline:
			t.Fatalf("expected 3 status events (busy, idle, idle), got %v — channel consumed other event types first; check subscription filter", got)
		}
	}
	assert.Equal(t, []string{"busy", "idle", "idle"}, got, "prompted→busy; terminal step→idle; tool step ignored; failure→idle")
}
