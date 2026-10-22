package handlers

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

// #1832 item 5 (1/2) — the verdict REST surface: bind errors, the
// perimeter, the refused-approve-is-a-recorded-outcome shape, and the
// chain read.

func verdictRouter(svc *fakeBoardManager) *gin.Engine {
	r := gin.New()
	h := &BoardHandler{board: svc}
	k := r.Group("/api/v1/me/tasks")
	k.POST("/:id/verdict", h.RecordTaskVerdict)
	k.GET("/:id/verdicts", h.ListTaskVerdicts)
	return r
}

func TestBoardVerdict_Record_HappyPath(t *testing.T) {
	svc := newFakeBoardManager()
	r := verdictRouter(svc)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/me/tasks/tsk-cccc3333/verdict", strings.NewReader(
		`{"verdict":"fail","reviewer":"rev-1","findings":[{"severity":"blocker","pointer":"pkg/a.go:1"}],"requestId":"rq-1"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, svc.calls["record_verdict"])
	assert.Equal(t, "rq-1", svc.lastRequestID)
	assert.Equal(t, board.VerdictFail, svc.lastVerdictInput.Verdict)
	assert.Contains(t, w.Body.String(), `"applied":true`)
}

func TestBoardVerdict_Record_BindErrors(t *testing.T) {
	svc := newFakeBoardManager()
	r := verdictRouter(svc)
	for name, body := range map[string]string{
		"missing verdict":  `{"reviewer":"r"}`,
		"missing reviewer": `{"verdict":"approve"}`,
		"not an object":    `null`,
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/me/tasks/tsk-cccc3333/verdict", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusBadRequest, w.Code, name)
	}
	assert.Equal(t, 0, svc.calls["record_verdict"], "no service call on bind failure")
}

func TestBoardVerdict_Record_PerimeterAndServiceError(t *testing.T) {
	svc := newFakeBoardManager()
	svc.errFor["record_verdict"] = &board.NotFoundError{Item: "task", Identifier: "tsk-nope"}
	r := verdictRouter(svc)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/me/tasks/tsk-nope/verdict", strings.NewReader(
		`{"verdict":"approve","reviewer":"r"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)

	svc2 := newFakeBoardManager()
	svc2.errFor["record_verdict"] = &board.GuardError{Code: "validation", Detail: "severity must be blocker, major, or minor"}
	r2 := verdictRouter(svc2)
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/me/tasks/tsk-cccc3333/verdict", strings.NewReader(
		`{"verdict":"fail","reviewer":"r","findings":[{"severity":"cosmic","pointer":"x"}]}`))
	req2.Header.Set("Content-Type", "application/json")
	r2.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusConflict, w2.Code, "validation guard renders the guard-class status (the established mapping)")
}

func TestBoardVerdict_List(t *testing.T) {
	svc := newFakeBoardManager()
	svc.verdicts = []board.Verdict{{Verdict: board.VerdictFail, Reviewer: "r", Applied: true, Transition: "in_progress"}}
	r := verdictRouter(svc)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/tasks/tsk-cccc3333/verdicts", nil)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"verdict":"fail"`)
	assert.Equal(t, 1, svc.calls["task_verdicts"])
}
