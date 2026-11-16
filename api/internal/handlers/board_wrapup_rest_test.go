// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_wrapup_rest_test.go — the wrap-up close + ack surfaces (epic
// wrap-up review, low-level design §§8-9): the owner REST verbs
// (POST /api/v1/me/tasks/:id/wrapup-verdict, POST /api/v1/me/epics/:id/
// ack-wrapup) and the automation seam twins (the worker's token-guarded
// verdict route; the manager's epic ack route). The wire schema is
// ENGINE-OWNED and closed: verdict pass|gaps, reportUrl required both,
// gapRefs iff gaps — the handlers are thin projections, and the claim
// token rides the actor on every surface (the worker-surface pattern).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

func wrapupVerdictRouter(svc *fakeBoardManager) *gin.Engine {
	r := gin.New()
	h := &BoardHandler{board: svc}
	k := r.Group("/api/v1/me/tasks")
	k.POST("/:id/wrapup-verdict", h.RecordTaskWrapUpVerdict)
	e := r.Group("/api/v1/me/epics")
	e.POST("/:id/ack-wrapup", h.AckEpicWrapUp)
	return r
}

func TestBoardWrapUpVerdictREST_HappyPath(t *testing.T) {
	svc := newFakeBoardManager()
	r := wrapupVerdictRouter(svc)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/me/tasks/tsk-wwww1111/wrapup-verdict", strings.NewReader(
		`{"verdict":"gaps","reportUrl":"https://ev.test/wr","gapRefs":["tsk-gggg2222"],"notes":"seams","reviewer":"wrapup-reviewer","claimToken":"tok-1","requestId":"rq-w1"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, svc.calls["record_wrapup_verdict"])
	require.NotNil(t, svc.lastWrapUpVerdict)
	assert.Equal(t, board.WrapUpVerdictGaps, svc.lastWrapUpVerdict.Verdict)
	assert.Equal(t, "https://ev.test/wr", svc.lastWrapUpVerdict.ReportURL)
	assert.Equal(t, []string{"tsk-gggg2222"}, svc.lastWrapUpVerdict.GapRefs)
	assert.Equal(t, "seams", svc.lastWrapUpVerdict.Notes)
	assert.Equal(t, "wrapup-reviewer", svc.lastWrapUpVerdict.Reviewer)
	assert.Equal(t, "rq-w1", svc.lastRequestID)
	// The claim token rides the actor (the engine's unconditional guard).
	assert.Equal(t, "tok-1", svc.lastActor.ClaimToken)
}

func TestBoardWrapUpVerdictREST_BindErrors(t *testing.T) {
	svc := newFakeBoardManager()
	r := wrapupVerdictRouter(svc)
	for name, body := range map[string]string{
		"missing verdict":  `{"reportUrl":"u","reviewer":"r"}`,
		"missing reviewer": `{"verdict":"pass","reportUrl":"u"}`,
		"not an object":    `null`,
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/me/tasks/tsk-wwww1111/wrapup-verdict", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusBadRequest, w.Code, name)
	}
	assert.Equal(t, 0, svc.calls["record_wrapup_verdict"], "no service call on bind failure")
	// The verdict ENUM is engine-arbitrated (GuardInput → 400 through the
	// real service; the closed-schema pins live in pkg/board's unit +
	// integration suites) — a thin projection never re-validates it.
}

func TestBoardAckEpicWrapUpREST(t *testing.T) {
	svc := newFakeBoardManager()
	r := wrapupVerdictRouter(svc)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/me/epics/epc-eeee3333/ack-wrapup", strings.NewReader(`{"requestId":"rq-a1"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, svc.calls["ack_epic_wrapup"])
	assert.Equal(t, "epc-eeee3333", svc.lastEpicRef)
	assert.Equal(t, "rq-a1", svc.lastRequestID)

	// An empty body acks too (requestID optional).
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/me/epics/epc-eeee3333/ack-wrapup", nil)
	r.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, 2, svc.calls["ack_epic_wrapup"])
}

// The automation seam twins: the worker's verdict route stamps the claim
// token on the actor; the manager's ack route stamps the workspace.
func TestBoardAutomation_WrapUpVerdictAndAck(t *testing.T) {
	svc := newFakeBoardService()
	mgr := newFakeBoardManager()
	r := newBoardAutomationRouterWithManager(t, svc, mgr)

	// The worker verdict: claimToken from the body rides the actor verbatim.
	w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/wrapup-verdict",
		`{"workspaceID":"ws-1","taskId":"tsk-wwww1111","claimToken":"tok-live","verdict":"pass","reportUrl":"https://ev.test/w","reviewer":"wrapup-reviewer"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, svc.calls["record_wrapup_verdict"])
	assert.Equal(t, "tok-live", svc.lastActor.ClaimToken)
	require.NotNil(t, svc.lastWrapUpVerdict)
	assert.Equal(t, board.WrapUpVerdictPass, svc.lastWrapUpVerdict.Verdict)

	// The manager ack: the epic ack route speaks the manager projection.
	w2 := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/epics/epc-eeee3333/ack-wrapup",
		`{"workspaceID":"ws-1","requestId":"rq-a2"}`)
	require.Equal(t, http.StatusOK, w2.Code, w2.Body.String())
	assert.Equal(t, 1, mgr.calls["ack_epic_wrapup"])
	assert.Equal(t, "epc-eeee3333", mgr.lastEpicRef)
	assert.Equal(t, board.SurfaceAgentManager, mgr.lastActor.Surface)
}
