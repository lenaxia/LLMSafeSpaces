// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// workflow_dedupe_test.go — #1327 regression suite: the workflow
// agent-node harness POST must carry the execution-derived dedupe key
// (messageID), the same idempotency seam #1315 gave the outbox
// admission path (PR #1323). The pinned opencode 1.18.15 harness
// validates the msg_-prefix, uses a repeated messageID verbatim as the
// user message's store ID, and UPSERTS on collision — so a key stable
// across the retry/re-drive of the SAME logical node execution bounds
// the session transcript to one message per execution. Pre-fix, every
// retry re-POSTed keylessly and appended unconditionally (this suite
// fails against that code by construction: the dispatch fields and the
// keyed body did not exist).
//
// The fake harness reproduces the G1-probed semantics: message POSTs
// are recorded (one entry per accepted append), session create/delete
// ride the same seam (getAgentAddr) the production calls use.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// dedupeHarness is a minimal opencode twin for the agent-node flow:
// POST /session mints ses_N, POST /session/{id}/message records the
// body (the transcript append), DELETE /session/{id} records the
// teardown. failMessages makes the message POST return 500 (the
// retry-path leg). messageStatus (when non-zero and failMessages is
// off) overrides the message response status — 404 drives the
// session_not_found leg, 5xx the script_failed leg (#1470 envelope
// tests). garbageBody returns a 200 the strict decode must reject
// (script_output_invalid). failDeletes makes the teardown DELETE fail
// (the leaked-ephemeral reality #1470's envelope contract reports).
type dedupeHarness struct {
	mu            sync.Mutex
	nextSession   int
	messages      []dedupeHarnessMessage // one per accepted POST
	createdSess   int
	deletedSess   []string
	failMessages  bool
	messageStatus int
	garbageBody   bool
	failDeletes   bool
	srv           *httptest.Server
}

type dedupeHarnessMessage struct {
	sessionID string
	body      map[string]any
	raw       string
}

func newDedupeHarness(t *testing.T) *dedupeHarness {
	t.Helper()
	h := &dedupeHarness{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			h.createdSess++
			h.nextSession++
			fmt.Fprintf(w, `{"id":"ses_%d"}`, h.nextSession)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/message"):
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			h.messages = append(h.messages, dedupeHarnessMessage{
				sessionID: r.URL.Path[len("/session/") : len(r.URL.Path)-len("/message")],
				body:      body,
				raw:       string(raw),
			})
			if h.failMessages {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"boom"}`))
				return
			}
			if h.messageStatus != 0 {
				w.WriteHeader(h.messageStatus)
				_, _ = w.Write([]byte(`{"error":"boom"}`))
				return
			}
			if h.garbageBody {
				_, _ = w.Write([]byte(`not-json{`))
				return
			}
			// The pinned V1 shape execAgentNode strictly decodes.
			_, _ = w.Write([]byte(`{"info":{"id":"msg_asst_1","agent":"build","tokens":{"input":1,"output":2,"total":3}},"parts":[{"type":"text","text":"done"}]}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/session/"):
			h.deletedSess = append(h.deletedSess, strings.TrimPrefix(r.URL.Path, "/session/"))
			if h.failDeletes {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *dedupeHarness) messageIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.messages))
	for i, m := range h.messages {
		if id, ok := m.body["messageID"].(string); ok {
			out[i] = id
		}
	}
	return out
}

func (h *dedupeHarness) rawBodies() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.messages))
	for i, m := range h.messages {
		out[i] = m.raw
	}
	return out
}

func (h *dedupeHarness) sessionIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.messages))
	for i, m := range h.messages {
		out[i] = m.sessionID
	}
	return out
}

func (h *dedupeHarness) deletes() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.deletedSess))
	copy(out, h.deletedSess)
	return out
}

// pointAgentAddrAt routes the production harness calls (create, message,
// delete — all through getAgentAddr) at the fake.
func pointAgentAddrAt(t *testing.T, url string) {
	t.Helper()
	orig := agentAddrAtomic.Load()
	t.Cleanup(func() { agentAddrAtomic.Store(orig) })
	agentAddrAtomic.Store(url)
}

// dispatchAgentNode drives the REAL handler with an agent-node dispatch
// carrying the given identity, returning the recorder for assertions.
func dispatchAgentNode(t *testing.T, nodeID, workflowID, runID, spec string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"nodeId":%q,"nodeType":"agent","spec":%s,"input":{"topic":"kv"},"workflowId":%q,"runId":%q}`,
		nodeID, spec, workflowID, runID)
	req := authedReq(http.MethodPost, "/v1/workflow/node/execute", testAuthPassword, strings.NewReader(body))
	w := httptest.NewRecorder()
	workflowExecuteHandler(testAuthPassword)(w, req)
	return w
}

// decodeAgentNodeOutcome returns (errorCode, response text) from the
// handler's workflowExecuteResponse body.
func decodeAgentNodeOutcome(t *testing.T, w *httptest.ResponseRecorder) (errorCode string, output map[string]any) {
	t.Helper()
	var resp struct {
		Output    json.RawMessage `json:"output"`
		ErrorCode string          `json:"errorCode"`
		Detail    string          `json:"detail"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	if len(resp.Output) > 0 {
		require.NoError(t, json.Unmarshal(resp.Output, &output))
	}
	return resp.ErrorCode, output
}

const agentSpecFixedSession = `{"agent":"build","prompt":"summarize {{.topic}}","sessionId":"ses_fixed"}`
const agentSpecEphemeral = `{"agent":"build","prompt":"summarize {{.topic}}"}`

// TestExecAgentNode_DedupeKeyGolden pins the exact key shape for the
// canonical identity (wf-1, n1, run-1): readable prefix from sanitized
// components, 8-hex-char sha256 suffix over the raw identity. Any drift
// in prefix grammar, separator, or hash framing fails here.
func TestExecAgentNode_DedupeKeyGolden(t *testing.T) {
	h := newDedupeHarness(t)
	pointAgentAddrAt(t, h.srv.URL)

	w := dispatchAgentNode(t, "n1", "wf-1", "run-1", agentSpecFixedSession)
	require.Equal(t, http.StatusOK, w.Code, "handler must succeed: %s", w.Body.String())
	errCode, output := decodeAgentNodeOutcome(t, w)
	require.Empty(t, errCode)
	require.Equal(t, "done", output["response"], "handler must surface the harness text: %v", output)

	ids := h.messageIDs()
	require.Len(t, ids, 1, "one dispatch = one harness POST")
	// sha256("wf-1\x1fn1\x1frun-1")[:4] hex — computed independently of
	// the production code under test.
	require.Equal(t, "msg_wf_wf-1_n1_run-1-5a812c0d", ids[0])

	// The keyed body carries exactly the fields the harness expects:
	// messageID (the dedupe key), agentID, parts.
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(h.rawBodies()[0]), &body))
	require.Len(t, body, 3)
	require.Equal(t, "build", body["agentID"])
	parts, ok := body["parts"].([]any)
	require.True(t, ok, "parts must be an array: %v", body["parts"])
	require.Len(t, parts, 1)
	part, ok := parts[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "text", part["type"])
	require.Equal(t, "summarize kv", part["text"])
}

// TestExecAgentNode_RetryRepostsSameKey is the #1327 core regression:
// a retry of the SAME logical node execution (same workflow/node/run,
// engine.go's attempt loop) must re-POST the SAME key so the harness
// upsert bounds the transcript to one message.
func TestExecAgentNode_RetryRepostsSameKey(t *testing.T) {
	h := newDedupeHarness(t)
	pointAgentAddrAt(t, h.srv.URL)

	w1 := dispatchAgentNode(t, "n1", "wf-1", "run-1", agentSpecFixedSession)
	require.Equal(t, http.StatusOK, w1.Code)
	w2 := dispatchAgentNode(t, "n1", "wf-1", "run-1", agentSpecFixedSession)
	require.Equal(t, http.StatusOK, w2.Code)

	ids := h.messageIDs()
	require.Len(t, ids, 2, "the retry re-POSTs (harness decides upsert)")
	require.Equal(t, ids[0], ids[1], "retry key must be identical")
	require.Equal(t, "ses_fixed", h.sessionIDs()[0])
	require.Equal(t, "ses_fixed", h.sessionIDs()[1], "fixed-session retries land in the same session")
}

// TestExecAgentNode_DistinctRunsGetDistinctKeys: a separate run (or a
// rerun, which mints a new run row) is a distinct logical execution —
// its POSTs must never upsert another run's transcript message.
func TestExecAgentNode_DistinctRunsGetDistinctKeys(t *testing.T) {
	h := newDedupeHarness(t)
	pointAgentAddrAt(t, h.srv.URL)

	w1 := dispatchAgentNode(t, "n1", "wf-1", "run-1", agentSpecFixedSession)
	require.Equal(t, http.StatusOK, w1.Code)
	w2 := dispatchAgentNode(t, "n1", "wf-1", "run-2", agentSpecFixedSession)
	require.Equal(t, http.StatusOK, w2.Code)

	ids := h.messageIDs()
	require.Len(t, ids, 2)
	require.NotEqual(t, ids[0], ids[1], "distinct runs must key apart")
	require.True(t, strings.HasPrefix(ids[1], "msg_wf_wf-1_n1_run-2"), "run component must change: %q", ids[1])
}

// TestExecAgentNode_NoIdentityStaysKeyless pins the mixed-fleet compat
// shape: a dispatch from an older API server (no workflowId/runId)
// must POST the exact pre-#1327 keyless body — no messageID field.
func TestExecAgentNode_NoIdentityStaysKeyless(t *testing.T) {
	h := newDedupeHarness(t)
	pointAgentAddrAt(t, h.srv.URL)

	w := dispatchAgentNode(t, "n1", "", "", agentSpecFixedSession)
	require.Equal(t, http.StatusOK, w.Code, "keyless dispatch must still succeed: %s", w.Body.String())

	raws := h.rawBodies()
	require.Len(t, raws, 1)
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(raws[0]), &body))
	require.NotContains(t, body, "messageID", "identity-less dispatch must stay keyless")
	require.Equal(t, "build", body["agentID"])
	require.Contains(t, body, "parts")
}

// TestExecAgentNode_PartialIdentityStaysKeyless: one identity component
// alone must NOT derive a key — a partial key would be identical for
// every dispatch missing the same component and let distinct
// executions upsert each other's messages.
func TestExecAgentNode_PartialIdentityStaysKeyless(t *testing.T) {
	for name, tc := range map[string]struct{ workflowID, runID string }{
		"workflow-only": {"wf-1", ""},
		"run-only":      {"", "run-1"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newDedupeHarness(t)
			pointAgentAddrAt(t, h.srv.URL)

			w := dispatchAgentNode(t, "n1", tc.workflowID, tc.runID, agentSpecFixedSession)
			require.Equal(t, http.StatusOK, w.Code)

			raws := h.rawBodies()
			require.Len(t, raws, 1)
			var body map[string]any
			require.NoError(t, json.Unmarshal([]byte(raws[0]), &body))
			require.NotContains(t, body, "messageID", "partial identity must stay keyless")
		})
	}
}

// TestExecAgentNode_HarnessFailureKeepsKeyAcrossRetry: the failure leg
// of the retry loop — a 500 from the harness fails the node
// (script_failed), and the re-dispatch of the same logical execution
// reuses the SAME key (the first POST may already be in the
// transcript; only the upsert keeps cardinality at one).
func TestExecAgentNode_HarnessFailureKeepsKeyAcrossRetry(t *testing.T) {
	h := newDedupeHarness(t)
	pointAgentAddrAt(t, h.srv.URL)

	h.mu.Lock()
	h.failMessages = true
	h.mu.Unlock()
	w := dispatchAgentNode(t, "n1", "wf-1", "run-1", agentSpecFixedSession)
	require.Equal(t, http.StatusOK, w.Code, "agent-node failures surface in-band")
	errCode, _ := decodeAgentNodeOutcome(t, w)
	require.Equal(t, "script_failed", errCode)

	h.mu.Lock()
	h.failMessages = false
	h.mu.Unlock()
	w2 := dispatchAgentNode(t, "n1", "wf-1", "run-1", agentSpecFixedSession)
	require.Equal(t, http.StatusOK, w2.Code)
	errCode2, _ := decodeAgentNodeOutcome(t, w2)
	require.Empty(t, errCode2)

	ids := h.messageIDs()
	require.Len(t, ids, 2)
	require.Equal(t, ids[0], ids[1], "the retry after a harness 500 must reuse the key")
}

// TestExecAgentNode_SanitizationHashesApartRawIdentity pins WHY the
// hash suffix exists: a/b and a_b sanitize to the same readable prefix,
// but they are different nodes — the raw-identity hash keeps their keys
// distinct so one node's POST can never upsert another's message.
func TestExecAgentNode_SanitizationHashesApartRawIdentity(t *testing.T) {
	h := newDedupeHarness(t)
	pointAgentAddrAt(t, h.srv.URL)

	w1 := dispatchAgentNode(t, "a/b", "wf-1", "run-1", agentSpecFixedSession)
	require.Equal(t, http.StatusOK, w1.Code)
	w2 := dispatchAgentNode(t, "a_b", "wf-1", "run-1", agentSpecFixedSession)
	require.Equal(t, http.StatusOK, w2.Code)

	ids := h.messageIDs()
	require.Len(t, ids, 2)
	require.Equal(t, "msg_wf_wf-1_a_b_run-1-03e33967", ids[0], "a/b key: sanitized prefix + its own hash")
	require.Equal(t, "msg_wf_wf-1_a_b_run-1-9aa2488a", ids[1], "a_b key: same prefix, different hash")
}

// TestExecAgentNode_EphemeralFlowKeyIndependentOfSession: the ephemeral
// leg — session created, keyed message POSTed, session deleted — and
// the key is a function of the execution identity ONLY (two dispatches
// mint two different ephemeral sessions yet share the key).
func TestExecAgentNode_EphemeralFlowKeyIndependentOfSession(t *testing.T) {
	h := newDedupeHarness(t)
	pointAgentAddrAt(t, h.srv.URL)

	w1 := dispatchAgentNode(t, "n1", "wf-1", "run-1", agentSpecEphemeral)
	require.Equal(t, http.StatusOK, w1.Code, "%s", w1.Body.String())
	errCode, output := decodeAgentNodeOutcome(t, w1)
	require.Empty(t, errCode)
	require.Equal(t, true, output["session_deleted"], "ephemeral session must be torn down: %v", output)

	w2 := dispatchAgentNode(t, "n1", "wf-1", "run-1", agentSpecEphemeral)
	require.Equal(t, http.StatusOK, w2.Code)

	ids := h.messageIDs()
	require.Len(t, ids, 2)
	require.Equal(t, ids[0], ids[1], "key must not depend on the session")

	sessions := h.sessionIDs()
	require.Len(t, sessions, 2)
	require.NotEqual(t, sessions[0], sessions[1], "each ephemeral dispatch mints its own session")

	h.mu.Lock()
	created := h.createdSess
	h.mu.Unlock()
	require.Equal(t, 2, created, "both dispatches create their ephemeral session through the seam")
	require.Len(t, h.deletes(), 2, "both ephemeral sessions are deleted")
}

// TestWorkflowAgentMessageKey_BoundedAndDeterministic: unit pins on the
// derivation — deterministic, msg_-prefixed, bounded to 64 chars even
// with pathological (long, unsanitizable) components, empty on any
// missing component.
func TestWorkflowAgentMessageKey_BoundedAndDeterministic(t *testing.T) {
	require.Equal(t, "", workflowAgentMessageKey("", "n", "r"))
	require.Equal(t, "", workflowAgentMessageKey("w", "", "r"))
	require.Equal(t, "", workflowAgentMessageKey("w", "n", ""))

	long := strings.Repeat("x", 300)
	key := workflowAgentMessageKey(long, long+"/../", long)
	require.Len(t, key, 64)
	require.True(t, strings.HasPrefix(key, "msg_wf_"), "prefix grammar must survive truncation: %q", key)
	require.Equal(t, key, workflowAgentMessageKey(long, long+"/../", long), "derivation is deterministic")
	require.NotEqual(t, key, workflowAgentMessageKey(long, long+"/../.", long))
}
