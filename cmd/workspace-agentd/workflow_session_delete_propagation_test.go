// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// #1882 Bug 2 pins, agentd half. The ephemeral teardown this process
// performs must be VISIBLE and INVALIDATING:
//
//   - VISIBLE to the engine: the deleted session's id rides the node
//     envelope (deletedSessionId) so the API can run the platform-side
//     teardown (tombstone + session_index row + deleted broadcast).
//     Pre-#1882 the id was wiped (session_deleted in the payload, "" in
//     the envelope) — the engine never knew which row to clean.
//   - INVALIDATING locally: the pod-side short-id replica must forget
//     the dead session's phrase (live evidence: session_metadata
//     resolved a hard-deleted carrier's phrase post-delete).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubHarnessForEphemeral stands up an opencode-shaped backend: create
// → ses_fix1, message → "ok", delete → 204. The delete leg records.
func stubHarnessForEphemeral(t *testing.T) (sessionID string, deleted *[]string) {
	t.Helper()
	const fixed = "ses_fix1"
	var mu sync.Mutex
	var hits []string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			mu.Lock()
			hits = append(hits, strings.TrimPrefix(r.URL.Path, "/session/"))
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/message"):
			_, _ = w.Write([]byte(`{"info":{"role":"assistant","id":"m1","time":{"created":1786400000000}},"parts":[{"type":"text","text":"ok"}]}`))
		default:
			_, _ = w.Write([]byte(`{"id":"` + fixed + `"}`))
		}
	}))
	t.Cleanup(stub.Close)
	orig := agentAddrAtomic.Load()
	t.Cleanup(func() { agentAddrAtomic.Store(orig) })
	agentAddrAtomic.Store(stub.URL)
	return fixed, &hits
}

// TestExecAgentNode_EphemeralTeardown_EnvelopeReportsDeletedSessionID:
// the success envelope must carry deletedSessionId (the id agentd
// deleted) alongside the wiped sessionId — the engine's propagation
// key.
func TestExecAgentNode_EphemeralTeardown_EnvelopeReportsDeletedSessionID(t *testing.T) {
	fixed, deleted := stubHarnessForEphemeral(t)

	body := `{"nodeId":"a1","nodeType":"agent","spec":{"prompt":"reply ok"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/workflow/node/execute", strings.NewReader(body))
	req.SetBasicAuth("opencode", mcpTestPassword)
	w := httptest.NewRecorder()
	workflowExecuteHandler(mcpTestPassword)(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, []string{fixed}, *deleted, "the ephemeral must be deleted pod-side")

	var env struct {
		Output    json.RawMessage `json:"output"`
		SessionID string          `json:"sessionId,omitempty"`
		DeletedID string          `json:"deletedSessionId,omitempty"`
		ErrorCode string          `json:"errorCode,omitempty"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Empty(t, env.ErrorCode)
	assert.Empty(t, env.SessionID, "the deleted ephemeral must NOT ride sessionId (existence contract, #1470)")
	assert.Equal(t, fixed, env.DeletedID,
		"the deleted ephemeral's id must ride deletedSessionId — the engine's teardown key (#1882)")

	var payload map[string]any
	require.NoError(t, json.Unmarshal(env.Output, &payload))
	assert.Equal(t, true, payload["session_deleted"], "the payload's legacy leak flag is unchanged")
	assert.Equal(t, "", payload["session_id"], "the payload's session_id stays wiped")
}

// TestExecAgentNode_FailurePathTeardown_EnvelopeReportsDeletedSessionID:
// failAgentNode tears the ephemeral down on the failure leg too — the
// error envelope must report the deleted id exactly as the success
// envelope does (its row was minted the same way).
func TestExecAgentNode_FailurePathTeardown_EnvelopeReportsDeletedSessionID(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/message"):
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			_, _ = w.Write([]byte(`{"id":"ses_fix2"}`))
		}
	}))
	t.Cleanup(stub.Close)
	orig := agentAddrAtomic.Load()
	t.Cleanup(func() { agentAddrAtomic.Store(orig) })
	agentAddrAtomic.Store(stub.URL)

	body := `{"nodeId":"a1","nodeType":"agent","spec":{"prompt":"reply ok"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/workflow/node/execute", strings.NewReader(body))
	req.SetBasicAuth("opencode", mcpTestPassword)
	w := httptest.NewRecorder()
	workflowExecuteHandler(mcpTestPassword)(w, req)

	var env struct {
		SessionID string `json:"sessionId,omitempty"`
		DeletedID string `json:"deletedSessionId,omitempty"`
		ErrorCode string `json:"errorCode"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, "script_failed", env.ErrorCode)
	assert.Empty(t, env.SessionID, "the torn-down ephemeral did not survive")
	assert.Equal(t, "ses_fix2", env.DeletedID,
		"the failure leg's teardown must be reported too (#1882)")
}

// TestExecAgentNode_EphemeralTeardown_ForgetsReplicaPhrase: agentd's
// confirmed delete must retract the dead session's replica mapping —
// the live-evidence defect (a dead carrier's phrase kept resolving).
func TestExecAgentNode_EphemeralTeardown_ForgetsReplicaPhrase(t *testing.T) {
	fixed, _ := stubHarnessForEphemeral(t)
	replica := withShortIDStore(t)
	replica.put("amber-falcon-glides", fixed)
	require.True(t, replica.size() > 0)

	body := `{"nodeId":"a1","nodeType":"agent","spec":{"prompt":"reply ok"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/workflow/node/execute", strings.NewReader(body))
	req.SetBasicAuth("opencode", mcpTestPassword)
	w := httptest.NewRecorder()
	workflowExecuteHandler(mcpTestPassword)(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	_, ok := replica.resolveShortID("amber-falcon-glides")
	assert.False(t, ok, "a deleted session's phrase must stop resolving (#1882 live evidence)")
	assert.Empty(t, replica.shortIDFor(fixed), "the dead session must have no phrase in the replica")
}

// TestWorkflowDeleteSessionHandler_ForgetsReplicaEntry: the authorized
// delete route (the engine's deleteSessionAuthorized target) forgets
// too — it is the same confirmed pod-side delete.
func TestWorkflowDeleteSessionHandler_ForgetsReplicaEntry(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	t.Cleanup(stub.Close)
	orig := agentAddrAtomic.Load()
	t.Cleanup(func() { agentAddrAtomic.Store(orig) })
	agentAddrAtomic.Store(stub.URL)

	replica := withShortIDStore(t)
	replica.put("quiet-meadow-listens", "ses_auth1")
	require.NotEmpty(t, replica.shortIDFor("ses_auth1"))

	req := httptest.NewRequest(http.MethodDelete, "/v1/workflow/session/delete?sessionId=ses_auth1", nil)
	req.SetBasicAuth("opencode", mcpTestPassword)
	w := httptest.NewRecorder()
	workflowDeleteSessionHandler(mcpTestPassword)(w, req)

	require.Equal(t, http.StatusNoContent, w.Code)
	_, ok := replica.resolveShortID("quiet-meadow-listens")
	assert.False(t, ok, "the authorized delete route must invalidate the replica entry (#1882)")
}

// TestSessionShortIDs_Forget is the store-level pin: forget removes
// BOTH directions, persists, and leaves unrelated entries intact.
func TestSessionShortIDs_Forget(t *testing.T) {
	path := t.TempDir() + "/session-shortids.json"
	s := newSessionShortIDStore(path)
	s.put("amber-falcon-glides", "ses_a")
	s.put("quiet-meadow-listens", "ses_b")

	s.forget("ses_a")

	_, ok := s.resolveShortID("amber-falcon-glides")
	assert.False(t, ok, "the forgotten session's phrase must not resolve")
	assert.Empty(t, s.shortIDFor("ses_a"), "the forgotten session must not map to a phrase")
	got, ok := s.resolveShortID("quiet-meadow-listens")
	assert.True(t, ok, "unrelated entries survive the forget")
	assert.Equal(t, "ses_b", got)

	// Persistence: a fresh store over the same file must NOT re-learn
	// the forgotten mapping (the boot pull rebuild shape).
	s2 := newSessionShortIDStore(path)
	_, ok = s2.resolveShortID("amber-falcon-glides")
	assert.False(t, ok, "the forget must survive a reload (persisted)")
	_, ok = s2.resolveShortID("quiet-meadow-listens")
	assert.True(t, ok, "the surviving entry must survive a reload")
}

// TestSessionShortIDs_Forget_NilAndUnknownSafe: nil store and unknown
// ids are no-ops (the replica-is-advisory posture).
func TestSessionShortIDs_Forget_NilAndUnknownSafe(t *testing.T) {
	var nilStore *sessionShortIDStore
	assert.NotPanics(t, func() { nilStore.forget("ses_x") })
	s := newSessionShortIDStore(t.TempDir() + "/s.json")
	assert.NotPanics(t, func() { s.forget("never-seen") })
	assert.NotPanics(t, func() { s.forget("") })
}
