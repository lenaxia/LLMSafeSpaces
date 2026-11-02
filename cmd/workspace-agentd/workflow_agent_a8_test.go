// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// workflow_agent_a8_test.go — the A8 pre-validation pins (review round
// 2 of the role-only model, PR #1990 F1): opencode silently falls back
// to the build agent for an unknown agentID (NODE-EXECUTE-CONTRACT
// §A8), so agentd validates the agent name against the live registry
// (GET /agent) BEFORE the session create and rejects unknown names
// with error_code agent_not_found in the 200 envelope — the code the
// board spawner branches on to degrade the dispatch persona-less.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// dispatchSpec posts a raw agent spec through the real handler.
func dispatchSpec(t *testing.T, nodeID, workflowID, runID, spec string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"nodeId":%q,"nodeType":"agent","spec":%s,"input":{},"workflowId":%q,"runId":%q}`,
		nodeID, spec, workflowID, runID)
	req := authedReq(http.MethodPost, "/v1/workflow/node/execute", testAuthPassword, strings.NewReader(body))
	w := httptest.NewRecorder()
	workflowExecuteHandler(testAuthPassword)(w, req)
	return w
}

func decodeA8Envelope(t *testing.T, w *httptest.ResponseRecorder) struct {
	ErrorCode string `json:"errorCode"`
	Detail    string `json:"detail"`
	SessionID string `json:"sessionId"`
} {
	t.Helper()
	var out struct {
		ErrorCode string `json:"errorCode"`
		Detail    string `json:"detail"`
		SessionID string `json:"sessionId"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	return out
}

// TestAgentNode_A8_RejectsUnknownAgent — the core gate: an agent name
// absent from opencode's registry is refused agent_not_found (200
// envelope), and NO session is created (the gate runs before the
// create — nothing to leak or tear down).
func TestAgentNode_A8_RejectsUnknownAgent(t *testing.T) {
	h := newDedupeHarness(t)
	pointAgentAddrAt(t, h.srv.URL)

	w := dispatchSpec(t, "board-worker", "wf-a8", "run-a8-1",
		`{"agent":"99999999-9999-9999-9999-999999999999","prompt":"work the charter","session":"new"}`)
	require.Equal(t, http.StatusOK, w.Code, "the A8 rejection rides the 200 envelope so the engine sees ErrorCode")

	out := decodeA8Envelope(t, w)
	require.Equal(t, "agent_not_found", out.ErrorCode)
	require.Contains(t, out.Detail, "99999999-9999-9999-9999-999999999999")
	require.Empty(t, out.SessionID, "the gate fires before the session create — no session exists")
	require.Equal(t, 0, h.created(), "no session may be created for a refused agent")
	require.Empty(t, h.messages, "no charter message may be dispatched for a refused agent")
}

// TestAgentNode_A8_KnownRoleAgentDispatchesOnIt — the positive arm: a
// role agent the harness registry lists (the materialized-role shape)
// passes the gate and the message POST carries the ROLE's id as
// agentID — not build, not the default. This is the dispatch half of
// "the session runs the referenced role".
func TestAgentNode_A8_KnownRoleAgentDispatchesOnIt(t *testing.T) {
	h := newDedupeHarness(t)
	const roleID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	h.agentNames = []string{roleID}
	pointAgentAddrAt(t, h.srv.URL)

	// A DAG agent node (not board-worker): #1986 made board-worker
	// delivery detached (identity answers before the turn), so the
	// synchronous graph node is the right vehicle for asserting the
	// message POST's agentID the moment the dispatch answers.
	w := dispatchSpec(t, "graph-node", "wf-a8", "run-a8-2",
		fmt.Sprintf(`{"agent":%q,"prompt":"work the charter","session":"new"}`, roleID))
	require.Equal(t, http.StatusOK, w.Code)
	out := decodeA8Envelope(t, w)
	require.Empty(t, out.ErrorCode, "a registry-listed role agent must pass the gate: %s", out.Detail)
	require.NotEmpty(t, out.SessionID)

	msgs := h.acceptedMessages()
	require.Len(t, msgs, 1)
	agentID, _ := msgs[0].body["agentID"].(string)
	require.Equal(t, roleID, agentID, "the dispatch must carry the role id as the opencode agentID")
	require.NotEqual(t, "build", agentID, "a persona'd dispatch must never silently degrade to the build agent")
}

// TestAgentNode_A8_ListFailureFailsLoud — an unusable registry probe
// (opencode down / 5xx / unparseable) fails the node with
// agent_list_failed rather than passing through unvalidated (the
// silent-fallback leg is exactly what this gate exists to close).
func TestAgentNode_A8_ListFailureFailsLoud(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	pointAgentAddrAt(t, srv.URL)

	w := dispatchSpec(t, "board-worker", "wf-a8", "run-a8-3",
		`{"agent":"build","prompt":"work the charter","session":"new"}`)
	require.Equal(t, http.StatusOK, w.Code)
	out := decodeA8Envelope(t, w)
	require.Equal(t, "agent_list_failed", out.ErrorCode)
}

// TestAgentNode_A8_EmptyAgentSkipsGate — the default dispatch (no
// agent key) never consults the registry: persona-less stages stay
// byte-identical to pre-persona (and pre-A8) behavior.
func TestAgentNode_A8_EmptyAgentSkipsGate(t *testing.T) {
	registryProbes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/agent" {
			registryProbes++
			t.Error("the registry must not be probed when the dispatch carries no agent")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// The session/message legs answer so the node completes.
		if r.Method == http.MethodPost && r.URL.Path == "/session" {
			fmt.Fprint(w, `{"id":"ses_skip"}`)
			return
		}
		fmt.Fprint(w, `{"info":{"id":"msg_1","agent":"build"},"parts":[{"type":"text","text":"ok"}]}`)
	}))
	t.Cleanup(srv.Close)
	pointAgentAddrAt(t, srv.URL)
	t.Cleanup(func() {
		require.Zero(t, registryProbes, "the registry must not be probed when the dispatch carries no agent")
	})

	w := dispatchSpec(t, "graph-node", "wf-a8", "run-a8-4",
		`{"agent":"","prompt":"work the charter","session":"ephemeral"}`)
	require.Equal(t, http.StatusOK, w.Code)
	out := decodeA8Envelope(t, w)
	require.Empty(t, out.ErrorCode, "the no-agent dispatch must complete without any A8 involvement: %s", out.Detail)
}

// TestAgentNode_A8_RegistryCachedPerPassword — the A8 contract's cache
// clause: the second dispatch against the same registry does not
// re-probe (the harness counts /agent reads).
func TestAgentNode_A8_RegistryCachedPerPassword(t *testing.T) {
	h := newDedupeHarness(t)
	pointAgentAddrAt(t, h.srv.URL)

	for i := 0; i < 2; i++ {
		w := dispatchSpec(t, "board-worker", "wf-a8", fmt.Sprintf("run-a8-cache-%d", i),
			`{"agent":"build","prompt":"probe","session":"ephemeral"}`)
		require.Equal(t, http.StatusOK, w.Code)
		require.Empty(t, decodeA8Envelope(t, w).ErrorCode)
	}
	require.Equal(t, 1, h.agentListReads(), "the registry must be fetched once and cached (A8: agents don't change at runtime)")
}
