// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_worker_compat_1746_test.go — the N-1 arm of the worker-tool
// seam's skew matrix (#1746): under agentdDelivery.updatePolicy
// OnRefresh, a platform upgrade rolls the API to N while running pods
// keep agentd N-1 until their next natural recreation. The worker tools
// (board_heartbeat / board_report_throttle / board_move_stage /
// board_read) ride the versioned routes /internal/v1/automation/board/*,
// so agentd(N-1) ↔ API(N) is REPLAYED HERE, not assumed:
//
// Every frozen wire set in board.WorkerSeamWireSets() (captured from the
// agentd release that emitted it — see pkg/board/worker_compat.go) is
// replayed verbatim — same method, path, body, bearer SA token — against
// THIS API's board seam mounted exactly as the router mounts it (real
// resolve: TokenReview → SA principal → workspace pin → delegated
// owner). The service outcomes asserted are the seam's contract, not
// fakes' whims: success shape on the happy path and the claim_token
// guard class (409, code "claim_token") on refusal — the outcomes an
// N-1 worker's tool calls actually depend on.
//
// The mirror arm (pkg/agent/opencode loopback_board_wire_1746_test.go)
// pins the agentd CLIENT to emit exactly the newest frozen set, so
// neither side of the seam can drift alone: a wire change must land as
// a NEW frozen set (or a new /internal/v2 route), never as a silent
// mutation.

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// The frozen fixtures stamp workspace wsCompat (a UUID, as production
// workspace ids are) owned by user-7; the SA principal is pinned to it.
const (
	wsCompatOwner = "user-7"
	wsCompat      = "11111111-1111-1111-1111-111111111101"
)

// newBoardCompatRouter mounts the board seam exactly as the router does
// (router.go's /internal/v1/automation/board group), with the resolve
// chain REAL (fake TokenReview + workspace lookup pinned to wsCompat).
func newBoardCompatRouter(t *testing.T, svc *fakeBoardService) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := &PodAutomationHandler{
		tokenReviewer: &fakeTokenReviewer{
			username: "system:serviceaccount:" + testRenameNamespace + ":workspace-" + wsCompat,
		},
		lookup:            &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: wsCompat, UserID: wsCompatOwner}},
		expectedNamespace: testRenameNamespace,
		board:             svc,
		boardEngine:       board.EngineConfig{ThrottleBackoff: 5 * time.Minute},
	}
	r := gin.New()
	b := r.Group("/internal/v1/automation/board")
	b.GET("/tasks/:id", h.BoardTaskGet)
	b.POST("/tasks/heartbeat", h.BoardHeartbeat)
	b.POST("/tasks/throttle", h.BoardReportThrottle)
	b.POST("/tasks/move-stage", h.BoardMoveStage)
	return r
}

// TestBoardWorkerCompat_N1WireAgainstCurrentAPI replays every frozen
// wire set (agentd N-1..N-k still in the supported skew window) against
// the CURRENT API handlers. The happy path proves route + body + auth
// shape acceptance AND that the resolved actor carries the N-1 body's
// claim token and task ref into the arbitration (the delegation the
// worker's tool calls rely on).
func TestBoardWorkerCompat_N1WireAgainstCurrentAPI(t *testing.T) {
	for _, set := range board.WorkerSeamWireSets() {
		t.Run(set.AgentdVersion, func(t *testing.T) {
			svc := newFakeBoardService()
			svc.getTask = &board.Task{ID: "t-1", ShortID: "tsk-aaaa1111", Stage: board.StageInProgress}
			r := newBoardCompatRouter(t, svc)

			for _, req := range set.Requests {
				var body string
				if req.Body != "" {
					body = req.Body
				}
				w := doBoard(t, r, req.Method, req.Path, body)
				require.Equal(t, http.StatusOK, w.Code,
					"%s %s: API(N) must accept agentd(%s)'s frozen wire form: %s", req.Method, req.Path, set.AgentdVersion, w.Body.String())
				assert.NotEmpty(t, w.Body.String(), "a 200 on this seam always carries a JSON body")
			}

			// The delegation the worker depends on: the N-1 body's claim
			// token and task ref reached the arbitration verbatim (the
			// last replay was move-stage; heartbeat/throttle hit the same
			// actor stamp — pinned per-route in board_automation_test.go).
			assert.Equal(t, wsCompat, svc.lastActor.WorkspaceID,
				"the pod identity resolves to the stamped workspace")
			assert.Equal(t, wsCompatOwner, svc.lastActor.UserID,
				"the pod's resolved owner delegates (R7)")
			assert.Equal(t, board.SurfaceAgent, svc.lastActor.Surface)
			assert.Equal(t, "tok-1", svc.lastActor.ClaimToken,
				"the N-1 body's claimToken rides the actor to arbitration")
			assert.Equal(t, "tsk-aaaa1111", svc.lastShortID)
		})
	}
}

// TestBoardWorkerCompat_N1GuardOutcomesUnchanged pins the OUTCOME
// classes an N-1 worker's tools must keep seeing on API(N): a refused
// token is the 409 claim_token guard (the single-use/expiry
// arbitration), never a reshape or a 500; a foreign task stays a plain
// 404 with no suggestions. Replayed on the frozen wire forms.
func TestBoardWorkerCompat_N1GuardOutcomesUnchanged(t *testing.T) {
	set := board.WorkerSeamWireV1

	t.Run("claim_token refusal is 409 with the frozen code", func(t *testing.T) {
		svc := newFakeBoardService()
		svc.heartbeatErr = &board.GuardError{Code: board.GuardClaimToken, Detail: "nope"}
		r := newBoardCompatRouter(t, svc)
		req := findCompatRequest(t, set, "board_heartbeat")
		w := doBoard(t, r, req.Method, req.Path, req.Body)
		require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), board.GuardClaimToken,
			"the guard code string an N-1 worker (and its operator) matches on is frozen")
	})

	t.Run("foreign task read stays a plain 404", func(t *testing.T) {
		svc := newFakeBoardService()
		svc.getErr = &board.NotFoundError{Item: "task", Identifier: "tsk-zzz"}
		r := newBoardCompatRouter(t, svc)
		req := findCompatRequest(t, set, "board_read")
		w := doBoard(t, r, req.Method, req.Path, req.Body)
		require.Equal(t, http.StatusNotFound, w.Code)
		assert.False(t, strings.Contains(w.Body.String(), "suggestions\":["), "no cross-tenant suggestions on the scoped reader")
	})

	t.Run("stale-token move_stage is the same 409 class", func(t *testing.T) {
		svc := newFakeBoardService()
		svc.moveErr = &board.GuardError{Code: board.GuardClaimToken, Detail: "lapsed"}
		r := newBoardCompatRouter(t, svc)
		req := findCompatRequest(t, set, "board_move_stage")
		w := doBoard(t, r, req.Method, req.Path, req.Body)
		require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), board.GuardClaimToken)
	})
}

// findCompatRequest locates one tool's frozen request in a wire set.
func findCompatRequest(t *testing.T, set *board.WorkerSeamWireSet, tool string) board.WorkerSeamRequest {
	t.Helper()
	for _, req := range set.Requests {
		if req.Tool == tool {
			return req
		}
	}
	t.Fatalf("tool %q not present in frozen wire set %s — the fixture and the arm drifted", tool, set.AgentdVersion)
	return board.WorkerSeamRequest{}
}

// TestBoardWorkerCompat_WireSetsRouteVersion pins the seam's route
// version: every frozen request addresses /internal/v1/automation/board.
// A future breaking route must be /internal/v2 (a new seam), not a v1
// mutation — this catches an accidental in-place path reshape.
func TestBoardWorkerCompat_WireSetsRouteVersion(t *testing.T) {
	for _, set := range board.WorkerSeamWireSets() {
		for _, req := range set.Requests {
			assert.True(t, strings.HasPrefix(req.Path, "/internal/v1/automation/board/"),
				"%s %s: frozen worker-seam requests ride the versioned v1 routes", req.Method, req.Path)
		}
	}
}
