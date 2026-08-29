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

// G2 (design 0054): bridge-derived busy states must decay — a lost
// terminal event (agent restart mid-turn) cannot hang the indicator.
func TestV2BusySessionsLifecycle(t *testing.T) {
	v := v2BusySessions{}

	t.Run("remember initializes lazily and overwrites", func(t *testing.T) {
		v.remember("ws", "s1")
		v.remember("ws", "s1") // same key: overwrite, not duplicate
		v.mu.Lock()
		n := len(v.entries)
		v.mu.Unlock()
		assert.Equal(t, 1, n)
	})

	t.Run("clear is a no-op on missing keys", func(t *testing.T) {
		v.clear("ws", "never-existed")
		v.mu.Lock()
		n := len(v.entries)
		v.mu.Unlock()
		assert.Equal(t, 1, n)
	})

	t.Run("expired returns only stale entries and removes them", func(t *testing.T) {
		// Force one entry stale by backdating its deadline.
		v.mu.Lock()
		v.entries["ws|stale"] = time.Now().Add(-time.Minute)
		v.mu.Unlock()
		got := v.expired()
		require.Len(t, got, 1)
		assert.Equal(t, "stale", got[0].Ses)
		v.mu.Lock()
		_, stillThere := v.entries["ws|stale"]
		n := len(v.entries)
		v.mu.Unlock()
		assert.False(t, stillThere, "expired entries are removed by expired()")
		assert.Equal(t, 1, n, "the fresh entry survives")
	})

	t.Run("empty and all-fresh maps expire nothing", func(t *testing.T) {
		empty := v2BusySessions{}
		assert.Empty(t, empty.expired())
		fresh := v2BusySessions{}
		fresh.remember("w", "s")
		assert.Empty(t, fresh.expired())
	})
}

// Review follow-ups: boundary and multi-session semantics.
func TestV2BusySessionsBoundary(t *testing.T) {
	v := v2BusySessions{entries: map[string]time.Time{}}
	v.mu.Lock()
	// Strictly-future deadlines must NOT expire; strictly-past ones MUST.
	// (now.After is strict; an exact-equality deadline is untestable
	// against wall time — a margin stands in for the boundary.)
	v.entries["w|edge"] = time.Now().Add(50 * time.Millisecond)
	v.entries["w|past"] = time.Now().Add(-time.Nanosecond)
	v.mu.Unlock()
	got := v.expired()
	var ses []string
	for _, e := range got {
		ses = append(ses, e.Ses)
	}
	assert.Contains(t, ses, "past", "deadline 1ns in the past expires")
	assert.NotContains(t, ses, "edge", "a strictly-future deadline does not expire (strict After)")
}

func TestV2BusySessionsMultiExpire(t *testing.T) {
	v := v2BusySessions{entries: map[string]time.Time{}}
	v.mu.Lock()
	for _, s := range []string{"a", "b", "c"} {
		v.entries["w|"+s] = time.Now().Add(-time.Minute)
	}
	// one fresh survivor
	v.entries["w|fresh"] = time.Now().Add(time.Minute)
	v.mu.Unlock()
	got := v.expired()
	require.Len(t, got, 3, "every expired session is returned in one pass, not just the first")
	assert.Empty(t, v.expired(), "fresh survivor untouched")
}

// First-idle-wins: a real terminal event clears the entry; a later reap
// of that same session must not double-publish idle.
func TestV2Bridge_TerminalPreemptsReap(t *testing.T) {
	orig := v2BusyTimeout
	v2BusyTimeout = 40 * time.Millisecond
	t.Cleanup(func() { v2BusyTimeout = orig })

	env := newVerifyEnv(t, &fakeAgentBackend{persistFirst: true})
	h := env.handler
	h.onV2RawEvent("ws-1", "session.next.prompted",
		`{"type":"session.next.prompted","properties":{"sessionID":"ses_1","messageID":"m1","delivery":"queue"}}`)
	// Terminal arrives before the deadline.
	h.onV2RawEvent("ws-1", "session.next.step.ended",
		`{"type":"session.next.step.ended","properties":{"sessionID":"ses_1","finish":"stop"}}`)
	time.Sleep(80 * time.Millisecond) // past the original deadline
	assert.Empty(t, h.v2Busy.expired(), "terminal cleared the entry — the reaper has nothing to double-publish")
}

// TestV2Bridge_BusyDecaysWithoutTerminalEvent is the G2 property at the
// handler level: busy published, terminal never arrives, deadline passes
// → the entry becomes reapable (the v2BusyReap loop itself is Start()
// wiring with a 30s tick; the reapability transition is what it consumes).
func TestV2Bridge_BusyDecaysWithoutTerminalEvent(t *testing.T) {
	orig := v2BusyTimeout
	v2BusyTimeout = 60 * time.Millisecond
	t.Cleanup(func() { v2BusyTimeout = orig })

	env := newVerifyEnv(t, &fakeAgentBackend{persistFirst: true})
	events := subscribeStatusEvents(t, env)
	h := env.handler

	h.onV2RawEvent("ws-1", "session.next.prompted",
		`{"type":"session.next.prompted","properties":{"sessionID":"ses_1","messageID":"m1","delivery":"queue"}}`)

	select {
	case e := <-events:
		assert.Equal(t, "busy", e.Status)
	case <-time.After(2 * time.Second):
		t.Fatal("busy event not published")
	}

	// No terminal event ever arrives. After the deadline the busy entry
	// must be expired() — exactly what the reaper loop consumes to
	// publish idle.
	time.Sleep(100 * time.Millisecond)
	reaped := h.v2Busy.expired()
	require.NotEmpty(t, reaped, "busy must become reapable after the deadline without a terminal event")
	assert.Equal(t, "ses_1", reaped[0].Ses)
	assert.Empty(t, h.v2Busy.expired(), "reaping is single-shot — no double idle")
}
