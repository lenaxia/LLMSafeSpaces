// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// #1882: the ephemeral carrier teardown must REPORT which session it
// deleted — deletedSessionId in the envelope — so the engine can run
// the platform delete effects (tombstone, session_index row removal,
// session.status:deleted SSE) for a session whose id never appeared in
// any earlier envelope field (sessionId stays the #1470
// exists-after-the-node field: omitted exactly when the session died).
// Without the report, every prompt-routine fire leaves a ghost
// session_index row minted by the live event bridge during the
// carrier's brief life.

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestExecAgentNode_SuccessEnvelope_EphemeralReportsDeletedSessionId is
// the #1882 primary pin: a successful ephemeral teardown reports the
// DELETED session's id alongside the omitted sessionId.
func TestExecAgentNode_SuccessEnvelope_EphemeralReportsDeletedSessionId(t *testing.T) {
	h := newDedupeHarness(t)
	pointAgentAddrAt(t, h.srv.URL)

	w := dispatchAgentNode(t, "n1", "wf-1", "run-1", agentSpecEphemeral)
	require.Equal(t, http.StatusOK, w.Code)

	out := decodeEnvelope(t, w)
	require.Empty(t, out.ErrorCode)
	require.Empty(t, out.SessionID, "#1470 contract unchanged: no session exists after the node")
	require.Equal(t, "ses_1", out.DeletedSessionID,
		"the torn-down carrier's id must ride the envelope so the engine can propagate the delete (#1882)")
	require.Equal(t, []string{"ses_1"}, h.deletes())
}

// TestExecAgentNode_FailureEnvelope_EphemeralReportsDeletedSessionId:
// the failure-path teardown (failAgentNode) reports the deleted id too.
func TestExecAgentNode_FailureEnvelope_EphemeralReportsDeletedSessionId(t *testing.T) {
	h := newDedupeHarness(t)
	h.messageStatus = http.StatusInternalServerError
	pointAgentAddrAt(t, h.srv.URL)

	w := dispatchAgentNode(t, "n1", "wf-1", "run-1", agentSpecEphemeral)
	require.Equal(t, http.StatusOK, w.Code)

	out := decodeEnvelope(t, w)
	require.Equal(t, "script_failed", out.ErrorCode)
	require.Empty(t, out.SessionID)
	require.Equal(t, "ses_1", out.DeletedSessionID,
		"the failure-leg teardown must report the deleted carrier id as well (#1882)")
	require.Equal(t, []string{"ses_1"}, h.deletes())
}

// TestExecAgentNode_SuccessEnvelope_Delete404_ReportsDeletedSessionId:
// a DELETE 404 means the session is already gone (#1470: no phantom
// survivor) — for #1882 it is equally a deletion the platform must
// hear about, so the already-gone id is reported as deleted.
func TestExecAgentNode_SuccessEnvelope_Delete404_ReportsDeletedSessionId(t *testing.T) {
	h := newDedupeHarness(t)
	h.deleteStatus = http.StatusNotFound
	pointAgentAddrAt(t, h.srv.URL)

	w := dispatchAgentNode(t, "n1", "wf-1", "run-1", agentSpecEphemeral)
	require.Equal(t, http.StatusOK, w.Code)

	out := decodeEnvelope(t, w)
	require.Empty(t, out.ErrorCode)
	require.Empty(t, out.SessionID)
	require.Equal(t, "ses_1", out.DeletedSessionID, "already-gone is still gone — report it")
}

// TestExecAgentNode_SuccessEnvelope_TeardownFailure_OmitsDeletedSessionId:
// a FAILED teardown leaves the session alive (the #1470 leak report) —
// nothing was deleted, so no deleted id may be claimed.
func TestExecAgentNode_SuccessEnvelope_TeardownFailure_OmitsDeletedSessionId(t *testing.T) {
	h := newDedupeHarness(t)
	h.failDeletes = true
	pointAgentAddrAt(t, h.srv.URL)

	w := dispatchAgentNode(t, "n1", "wf-1", "run-1", agentSpecEphemeral)
	require.Equal(t, http.StatusOK, w.Code)

	out := decodeEnvelope(t, w)
	require.Empty(t, out.ErrorCode)
	require.Equal(t, "ses_1", out.SessionID, "the leaked session stays discoverable (#1470)")
	require.Empty(t, out.DeletedSessionID, "nothing was deleted — no deleted id")
}

// TestExecAgentNode_PreservedEnvelope_OmitsDeletedSessionId: preserved
// modes never delete — the field must stay absent.
func TestExecAgentNode_PreservedEnvelope_OmitsDeletedSessionId(t *testing.T) {
	h := newDedupeHarness(t)
	pointAgentAddrAt(t, h.srv.URL)

	w := dispatchAgentNode(t, "n1", "wf-1", "run-1", agentSpecPreserved)
	require.Equal(t, http.StatusOK, w.Code)

	out := decodeEnvelope(t, w)
	require.Empty(t, out.ErrorCode)
	require.Equal(t, "ses_1", out.SessionID)
	require.Empty(t, out.DeletedSessionID, "preserved session was not deleted")
}
