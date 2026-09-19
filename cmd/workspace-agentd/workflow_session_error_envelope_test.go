// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// #1470: the agent-node error envelope must carry the session that
// survived the failed turn (empty/omitted when none did), so the engine
// can origin-record and index exactly the sessions that still exist.
// The contract: sessionId in the envelope ⟺ the session exists after
// execAgentNode finished — preserved modes report it, ephemeral modes
// tear down and omit it, a failed teardown reports reality (the leak).
// Success envelopes carry the same first-class field so drifted output
// shapes can't orphan a delivered session either.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// envelopeOutcome decodes the full workflow envelope: error fields and
// the first-class sessionId, plus the success output payload.
type envelopeOutcome struct {
	ErrorCode string
	Detail    string
	SessionID string
	Output    map[string]any
}

func decodeEnvelope(t *testing.T, w *httptest.ResponseRecorder) envelopeOutcome {
	t.Helper()
	var resp struct {
		Output    json.RawMessage `json:"output"`
		ErrorCode string          `json:"errorCode"`
		Detail    string          `json:"detail"`
		SessionID string          `json:"sessionId"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	out := envelopeOutcome{ErrorCode: resp.ErrorCode, Detail: resp.Detail, SessionID: resp.SessionID}
	if len(resp.Output) > 0 {
		require.NoError(t, json.Unmarshal(resp.Output, &out.Output))
	}
	return out
}

func TestExecAgentNode_FailureEnvelope_ReportsPreservedSession(t *testing.T) {
	h := newDedupeHarness(t)
	h.messageStatus = http.StatusInternalServerError
	pointAgentAddrAt(t, h.srv.URL)

	w := dispatchAgentNode(t, "n1", "wf-1", "run-1", agentSpecPreserved)
	require.Equal(t, http.StatusOK, w.Code)

	out := decodeEnvelope(t, w)
	require.Equal(t, "script_failed", out.ErrorCode)
	require.NotEmpty(t, out.SessionID, "preserved session must ride the error envelope")
	require.Empty(t, h.deletes(), "preserved session must NOT be torn down on failure")
}

func TestExecAgentNode_FailureEnvelope_EphemeralTornDownAndOmitted(t *testing.T) {
	h := newDedupeHarness(t)
	h.messageStatus = http.StatusInternalServerError
	pointAgentAddrAt(t, h.srv.URL)

	w := dispatchAgentNode(t, "n1", "wf-1", "run-1", agentSpecEphemeral)
	require.Equal(t, http.StatusOK, w.Code)

	out := decodeEnvelope(t, w)
	require.Equal(t, "script_failed", out.ErrorCode)
	require.Empty(t, out.SessionID, "torn-down ephemeral session must be omitted")
	require.Equal(t, []string{"ses_1"}, h.deletes(), "ephemeral session must be torn down on failure")
}

func TestExecAgentNode_FailureEnvelope_EphemeralDeleteFails_ReportsLeak(t *testing.T) {
	h := newDedupeHarness(t)
	h.messageStatus = http.StatusInternalServerError
	h.failDeletes = true
	pointAgentAddrAt(t, h.srv.URL)

	w := dispatchAgentNode(t, "n1", "wf-1", "run-1", agentSpecEphemeral)
	require.Equal(t, http.StatusOK, w.Code)

	out := decodeEnvelope(t, w)
	require.Equal(t, "script_failed", out.ErrorCode)
	require.Equal(t, "ses_1", out.SessionID, "a failed teardown leaks the session — the envelope must report reality")
}

func TestExecAgentNode_FailureEnvelope_SessionNotFound_OmitsSession(t *testing.T) {
	h := newDedupeHarness(t)
	h.messageStatus = http.StatusNotFound
	pointAgentAddrAt(t, h.srv.URL)

	w := dispatchAgentNode(t, "n1", "wf-1", "run-1", agentSpecPreserved)
	require.Equal(t, http.StatusOK, w.Code)

	out := decodeEnvelope(t, w)
	require.Equal(t, "session_not_found", out.ErrorCode)
	require.Empty(t, out.SessionID, "a 404 means the session is gone — nothing to report")
}

func TestExecAgentNode_FailureEnvelope_DriftedOutput_PreservesSession(t *testing.T) {
	h := newDedupeHarness(t)
	h.garbageBody = true
	pointAgentAddrAt(t, h.srv.URL)

	w := dispatchAgentNode(t, "n1", "wf-1", "run-1", agentSpecPreserved)
	require.Equal(t, http.StatusOK, w.Code)

	out := decodeEnvelope(t, w)
	require.Equal(t, "script_output_invalid", out.ErrorCode)
	require.NotEmpty(t, out.SessionID, "the drift class keeps the session — it must be discoverable")
	require.Empty(t, h.deletes())
}

func TestExecAgentNode_FailureEnvelope_SchemaMismatch_ReportsPreservedSession(t *testing.T) {
	h := newDedupeHarness(t)
	pointAgentAddrAt(t, h.srv.URL)

	w := dispatchAgentNode(t, "n1", "wf-1", "run-1", agentSpecStructuredBad)
	require.Equal(t, http.StatusOK, w.Code)

	out := decodeEnvelope(t, w)
	require.Equal(t, "schema_mismatch", out.ErrorCode)
	require.NotEmpty(t, out.SessionID, "schema failure preserves the session — the turn ran")
	require.Empty(t, h.deletes())
}

func TestExecAgentNode_SuccessEnvelope_CarriesPreservedSession(t *testing.T) {
	h := newDedupeHarness(t)
	pointAgentAddrAt(t, h.srv.URL)

	w := dispatchAgentNode(t, "n1", "wf-1", "run-1", agentSpecPreserved)
	require.Equal(t, http.StatusOK, w.Code)

	out := decodeEnvelope(t, w)
	require.Empty(t, out.ErrorCode)
	require.Equal(t, "ses_1", out.SessionID, "success envelope carries the session first-class")
	require.Equal(t, "ses_1", out.Output["session_id"], "output payload field unchanged (back-compat)")
}

func TestExecAgentNode_SuccessEnvelope_EphemeralOmitsSession(t *testing.T) {
	h := newDedupeHarness(t)
	pointAgentAddrAt(t, h.srv.URL)

	w := dispatchAgentNode(t, "n1", "wf-1", "run-1", agentSpecEphemeral)
	require.Equal(t, http.StatusOK, w.Code)

	out := decodeEnvelope(t, w)
	require.Empty(t, out.ErrorCode)
	require.Empty(t, out.SessionID, "ephemeral success ends with no session — omitted")
	require.Equal(t, "", out.Output["session_id"], "output payload still clears it (back-compat)")
	require.Equal(t, []string{"ses_1"}, h.deletes())
}

const agentSpecPreserved = `{"agent":"build","prompt":"summarize {{.topic}}","session":"new"}`
const agentSpecStructuredBad = `{"agent":"build","prompt":"summarize {{.topic}}","session":"new","enforceStructuredOutput":true,"outputSchema":{"type":"object","properties":{"x":{"type":"number"}},"required":["x"]}}`

// dispatchAgentNodeTimeout drives the real handler with an explicit
// dispatch timeout (#1470: the script_timeout leg).
func dispatchAgentNodeTimeout(t *testing.T, nodeID, workflowID, runID, spec, timeout string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"nodeId":%q,"nodeType":"agent","spec":%s,"input":{"topic":"kv"},"workflowId":%q,"runId":%q,"timeout":%q}`,
		nodeID, spec, workflowID, runID, timeout)
	req := authedReq(http.MethodPost, "/v1/workflow/node/execute", testAuthPassword, strings.NewReader(body))
	w := httptest.NewRecorder()
	workflowExecuteHandler(testAuthPassword)(w, req)
	return w
}

// TestExecAgentNode_TimeoutLeg_TearsDownEphemeralUnderDeadContext pins
// the WithoutCancel teardown (#1470 round-1 finding 2): the dispatch
// context dies at 300ms while the message POST parks for 2s — the
// ephemeral cleanup DELETE must STILL land (reverting WithoutCancel
// makes it fail instantly on the canceled context and this test fails).
func TestExecAgentNode_TimeoutLeg_TearsDownEphemeralUnderDeadContext(t *testing.T) {
	h := newDedupeHarness(t)
	h.blockMessages = true
	pointAgentAddrAt(t, h.srv.URL)

	w := dispatchAgentNodeTimeout(t, "n1", "wf-1", "run-1", agentSpecEphemeral, "300ms")
	require.Equal(t, http.StatusGatewayTimeout, w.Code)

	out := decodeEnvelope(t, w)
	require.Equal(t, "script_timeout", out.ErrorCode)
	require.Empty(t, out.SessionID, "the ephemeral session must be torn down despite the dead context")
	require.Equal(t, []string{"ses_1"}, h.deletes(), "the cleanup DELETE must outlive the dispatch deadline")
}

func TestExecAgentNode_TimeoutLeg_PreservedSessionRidesEnvelope(t *testing.T) {
	h := newDedupeHarness(t)
	h.blockMessages = true
	pointAgentAddrAt(t, h.srv.URL)

	w := dispatchAgentNodeTimeout(t, "n1", "wf-1", "run-1", agentSpecPreserved, "300ms")
	require.Equal(t, http.StatusGatewayTimeout, w.Code)

	out := decodeEnvelope(t, w)
	require.Equal(t, "script_timeout", out.ErrorCode)
	require.Equal(t, "ses_1", out.SessionID, "a preserved session survives a timeout and is reported")
	require.Empty(t, h.deletes())
}

// TestExecAgentNode_FailureEnvelope_Delete404_CountsAsGone pins the
// round-1 finding-1 fix: opencode's DELETE 404 means the session is
// already gone — the envelope must NOT mint a phantom survivor.
func TestExecAgentNode_FailureEnvelope_Delete404_CountsAsGone(t *testing.T) {
	h := newDedupeHarness(t)
	h.messageStatus = http.StatusInternalServerError
	h.deleteStatus = http.StatusNotFound
	pointAgentAddrAt(t, h.srv.URL)

	w := dispatchAgentNode(t, "n1", "wf-1", "run-1", agentSpecEphemeral)
	require.Equal(t, http.StatusOK, w.Code)

	out := decodeEnvelope(t, w)
	require.Equal(t, "script_failed", out.ErrorCode)
	require.Empty(t, out.SessionID, "404 on DELETE = already gone; no phantom survivor")
}

func TestExecAgentNode_SuccessEnvelope_Delete404_NoPhantomLeak(t *testing.T) {
	h := newDedupeHarness(t)
	h.deleteStatus = http.StatusNotFound
	pointAgentAddrAt(t, h.srv.URL)

	w := dispatchAgentNode(t, "n1", "wf-1", "run-1", agentSpecEphemeral)
	require.Equal(t, http.StatusOK, w.Code)

	out := decodeEnvelope(t, w)
	require.Empty(t, out.ErrorCode)
	require.Empty(t, out.SessionID, "a 404 delete is GONE — the success envelope reports no survivor")
	require.Equal(t, "", out.Output["session_id"], "payload matches the envelope (no phantom leak)")
}

// TestWorkflowDeleteSessionRoute_PropagatesReality pins the route form
// of the existence contract: 204 only when the session is gone; a
// failed delete answers 502 so the engine's PreserveOnFailure leg can
// record the survivor instead of trusting a blanket 204.
func TestWorkflowDeleteSessionRoute_PropagatesReality(t *testing.T) {
	h := newDedupeHarness(t)
	pointAgentAddrAt(t, h.srv.URL)

	doDelete := func() int {
		req := authedReq(http.MethodDelete, "/v1/workflow/session/delete?sessionId=ses_1", testAuthPassword, nil)
		w := httptest.NewRecorder()
		workflowDeleteSessionHandler(testAuthPassword)(w, req)
		return w.Code
	}

	require.Equal(t, http.StatusNoContent, doDelete(), "successful delete → 204 (gone)")

	h.failDeletes = true
	require.Equal(t, http.StatusBadGateway, doDelete(), "failed delete → 502 (session survives)")
}
