// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_parity_test.go — the design 0067 §4.3/§6 parity matrix, Lane 2
// shape: rows = every §4.2 action; columns = REST / external platform
// MCP / agent-internal MCP. The REST column is LIVE (allow + deny arms
// per action), the agent column is LIVE against the Lane 1 internal
// automation seam (worker actions present; manager rows
// absent-by-design; guard-class parity on the shared action), and the
// external MCP column is PINNED ABSENT until Lane 3 registers board
// tools (a source-scan fails loudly the moment a board_* tool appears,
// forcing its matrix cell to be exercised — parity by construction).
//
// Construction guarantees (the "CI fails on any action lacking full
// surface coverage" clause): reflection pins EVERY exported method of
// *BoardHandler to exactly one matrix row's rest method set, and the
// route scans pin exactly which routes each surface registers. An
// endpoint added without a matrix row reddens TestBoardParity_Coverage;
// a matrix row without an endpoint reddens it too.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// parityCell is one (action × surface) outcome. Lane 2 ships three
// kinds: live (exercised below), absent (declared asymmetry — the
// surface refuses/does not expose the action BY DESIGN), and
// pendingLane3 (external MCP tools do not exist yet; the source-scan
// below proves the absence and will fail when Lane 3 lands them).
type parityCell struct {
	kind string // "live" | "absent" | "pendingLane3"
}

type parityRow struct {
	action string // §4.2 action name (board_events.action vocabulary)

	restMethods []string // exported *BoardHandler methods (the REST cell's live surface)
	rest        parityCell
	externalMCP parityCell
	agent       parityCell

	// denyClass, when set, is the representative guard the REST allow/
	// deny arms drive (the fake service returns it; the arm asserts the
	// mapped HTTP class).
	denyErr  error
	denyCode int
}

var (
	liveCell         = parityCell{kind: "live"}
	absentCell       = parityCell{kind: "absent"}
	pendingLane3Cell = parityCell{kind: "pendingLane3"}
)

// boardParityMatrix — THE §4.2 action set. Declared asymmetries are
// pinned as absent cells, not missing coverage (§6):
//   - heartbeat / report_throttle: worker-only — absent on REST;
//     deny-or-absent on external MCP (Lane 3); live on the agent seam.
//   - claim_next: engine-internal (R9 claim-then-spawn) — absent on ALL
//     three surfaces, permanently.
//   - manager rows (CRUD/lifecycle/cancel/dependencies/task_start):
//     absent on the agent registry — agents are workers, not managers
//     (§4.3c).
//   - move_stage + board_read: on every surface (owner authority on
//     REST/external-MCP; token-guarded for the worker on the agent
//     registry).
var boardParityMatrix = []parityRow{
	{action: "create_project", restMethods: []string{"CreateProject"}, rest: liveCell, externalMCP: pendingLane3Cell, agent: absentCell,
		denyErr: &board.GuardError{Code: board.GuardWorkspaceBinding, Detail: "target workspace must be accessible"}, denyCode: http.StatusForbidden},
	{action: "update_project", restMethods: []string{"UpdateProject"}, rest: liveCell, externalMCP: pendingLane3Cell, agent: absentCell},
	{action: "archive_project", restMethods: []string{"ArchiveProject"}, rest: liveCell, externalMCP: pendingLane3Cell, agent: absentCell},
	{action: "unarchive_project", restMethods: []string{"UnarchiveProject"}, rest: liveCell, externalMCP: pendingLane3Cell, agent: absentCell},
	{action: "board_read", restMethods: []string{"ListProjects", "GetProject", "ListProjectTasks", "GetEpic", "GetTask"}, rest: liveCell, externalMCP: pendingLane3Cell, agent: liveCell},
	{action: "create_epic", restMethods: []string{"CreateEpic"}, rest: liveCell, externalMCP: pendingLane3Cell, agent: absentCell},
	{action: "update_epic", restMethods: []string{"UpdateEpic"}, rest: liveCell, externalMCP: pendingLane3Cell, agent: absentCell},
	{action: "epic_activate", restMethods: []string{"ActivateEpic"}, rest: liveCell, externalMCP: pendingLane3Cell, agent: absentCell,
		denyErr: &board.GuardError{Code: board.GuardEpicState, Detail: "not a lifecycle transition"}, denyCode: http.StatusConflict},
	{action: "epic_pause", restMethods: []string{"PauseEpic"}, rest: liveCell, externalMCP: pendingLane3Cell, agent: absentCell,
		denyErr: &board.GuardError{Code: board.GuardEpicState, Detail: "not a lifecycle transition"}, denyCode: http.StatusConflict},
	{action: "epic_archive", restMethods: []string{"ArchiveEpic"}, rest: liveCell, externalMCP: pendingLane3Cell, agent: absentCell},
	{action: "epic_unarchive", restMethods: []string{"UnarchiveEpic"}, rest: liveCell, externalMCP: pendingLane3Cell, agent: absentCell},
	{action: "cancel_epic", restMethods: []string{"CancelEpic"}, rest: liveCell, externalMCP: pendingLane3Cell, agent: absentCell},
	{action: "create_task", restMethods: []string{"CreateEpicTask"}, rest: liveCell, externalMCP: pendingLane3Cell, agent: absentCell},
	{action: "update_task", restMethods: []string{"UpdateTask"}, rest: liveCell, externalMCP: pendingLane3Cell, agent: absentCell},
	{action: "add_dependency", restMethods: []string{"AddDependency"}, rest: liveCell, externalMCP: pendingLane3Cell, agent: absentCell,
		denyErr: &board.DependencyError{Code: "cycle", Detail: "would create a cycle"}, denyCode: http.StatusConflict},
	{action: "remove_dependency", restMethods: []string{"RemoveDependency"}, rest: liveCell, externalMCP: pendingLane3Cell, agent: absentCell},
	{action: "move_stage", restMethods: []string{"MoveStageTask"}, rest: liveCell, externalMCP: pendingLane3Cell, agent: liveCell,
		denyErr: &board.GuardError{Code: board.GuardDone, Detail: "criteria unmet or evidence missing"}, denyCode: http.StatusConflict},
	{action: "task_start", restMethods: []string{"StartTask"}, rest: liveCell, externalMCP: pendingLane3Cell, agent: absentCell, // owner surface only (§4.3b)
		denyErr: &board.GuardError{Code: board.GuardEpicState, Detail: "paused epics refuse task_start"}, denyCode: http.StatusConflict},
	{action: "cancel_task", restMethods: []string{"CancelTask"}, rest: liveCell, externalMCP: pendingLane3Cell, agent: absentCell},
	// Worker-only rows: absent on REST BY DESIGN (no owner surface
	// delivers them — §4.7); deny-or-absent on external MCP until Lane 3
	// records which; live on the agent seam (claim-token guarded).
	{action: "heartbeat", restMethods: nil, rest: absentCell, externalMCP: pendingLane3Cell, agent: liveCell},
	{action: "report_throttle", restMethods: nil, rest: absentCell, externalMCP: pendingLane3Cell, agent: liveCell},
	// Engine-internal since R9 — absent on ALL surfaces, permanently.
	{action: "claim_next", restMethods: nil, rest: absentCell, externalMCP: absentCell, agent: absentCell},
}

// TestBoardParity_Coverage: the construction guarantee — every exported
// method on *BoardHandler belongs to exactly one matrix row (an
// endpoint without a row reddens here), every restMethods entry names a
// real method (a row without an endpoint reddens here), and the live/
// absent cells agree with the method set (a live REST cell with no
// methods, or methods on an absent cell, reddens here).
func TestBoardParity_Coverage(t *testing.T) {
	boardHandlerType := reflect.TypeOf(&BoardHandler{})
	exported := map[string]bool{}
	for i := range boardHandlerType.NumMethod() {
		m := boardHandlerType.Method(i)
		if m.IsExported() {
			exported[m.Name] = true
		}
	}

	seen := map[string]string{} // method → action
	for _, row := range boardParityMatrix {
		if row.rest.kind == "live" {
			require.NotEmpty(t, row.restMethods, "action %s: live REST cell must name its handler methods", row.action)
		} else {
			require.Empty(t, row.restMethods, "action %s: absent REST cell must name no handler methods", row.action)
		}
		for _, m := range row.restMethods {
			require.True(t, exported[m], "action %s: restMethods names %q which is not an exported *BoardHandler method", row.action, m)
			owner, dup := seen[m]
			require.False(t, dup, "method %s appears in two matrix rows (%s, %s)", m, owner, row.action)
			seen[m] = row.action
			delete(exported, m)
		}
	}
	leftover := make([]string, 0)
	for m := range exported {
		leftover = append(leftover, m)
	}
	sort.Strings(leftover)
	assert.Empty(t, leftover, "exported *BoardHandler methods with NO matrix row (parity by construction requires a row per surface addition): %v", leftover)
}

// TestBoardParity_RESTColumn: allow arm (fake succeeds → 2xx) and, for
// rows carrying a representative guard, the deny arm (fake returns the
// guard → the mapped HTTP class) — driven through the same router
// fixture the handler pins use.
func TestBoardParity_RESTColumn(t *testing.T) {
	for _, row := range boardParityMatrix {
		if row.rest.kind != "live" {
			continue
		}
		t.Run(row.action, func(t *testing.T) {
			// Allow arm: one representative request per action (the
			// full request surface is pinned by board_rest_test.go; the
			// matrix pins the column's outcome classes).
			svc := newFakeBoardManager()
			svc.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "user-1", Status: board.ProjectActive}}
			svc.tasks = []board.Task{{ID: "t1", ShortID: "tsk-cccc3333", ProjectID: "p1", EpicID: "e1", Stage: board.StageBacklog, Tags: []string{}}}
			r := newBoardRESTRouter(t, svc)
			w := boardParityRESTCall(t, r, row.action)
			assert.Less(t, w.Code, 300, "allow arm: %s → %s", row.action, w.Body.String())

			// Deny arm (rows with a representative guard).
			if row.denyErr != nil {
				svc2 := newFakeBoardManager()
				svc2.tasks = []board.Task{{ID: "t1", ShortID: "tsk-cccc3333", ProjectID: "p1", EpicID: "e1", Stage: board.StageBacklog, Tags: []string{}}}
				svc2.errFor[boardParitySeam(row.action)] = row.denyErr
				r2 := newBoardRESTRouter(t, svc2)
				w2 := boardParityRESTCall(t, r2, row.action)
				assert.Equal(t, row.denyCode, w2.Code, "deny arm: %s → %s", row.action, w2.Body.String())
			}
		})
	}
}

// boardParitySeam maps a matrix action to the fake service's errFor key.
func boardParitySeam(action string) string {
	switch action {
	case "create_project":
		return "create_project"
	case "epic_activate":
		return "epic_activate"
	case "epic_pause":
		return "epic_pause"
	case "add_dependency":
		return "add_dependency"
	case "move_stage":
		return "move_stage"
	case "task_start":
		return "task_start"
	default:
		return action
	}
}

// boardParityRESTCall issues one representative request for the action.
func boardParityRESTCall(t *testing.T, r *gin.Engine, action string) *httptest.ResponseRecorder {
	t.Helper()
	switch action {
	case "create_project":
		return doBoardREST(t, r, http.MethodPost, "/api/v1/me/projects", `{"name":"B","targetWorkspaceId":"ws-1"}`)
	case "update_project":
		return doBoardREST(t, r, http.MethodPatch, "/api/v1/me/projects/prj-aaaa1111", `{"name":"x"}`)
	case "archive_project":
		return doBoardREST(t, r, http.MethodPost, "/api/v1/me/projects/prj-aaaa1111/archive", `{}`)
	case "unarchive_project":
		return doBoardREST(t, r, http.MethodPost, "/api/v1/me/projects/prj-aaaa1111/unarchive", `{}`)
	case "board_read":
		return doBoardREST(t, r, http.MethodGet, "/api/v1/me/projects", "")
	case "create_epic":
		return doBoardREST(t, r, http.MethodPost, "/api/v1/me/projects/prj-aaaa1111/epics", `{"title":"E"}`)
	case "update_epic":
		return doBoardREST(t, r, http.MethodPatch, "/api/v1/me/epics/epc-bbbb2222", `{"outcome":"o"}`)
	case "epic_activate":
		return doBoardREST(t, r, http.MethodPost, "/api/v1/me/epics/epc-bbbb2222/activate", `{}`)
	case "epic_pause":
		return doBoardREST(t, r, http.MethodPost, "/api/v1/me/epics/epc-bbbb2222/pause", `{}`)
	case "epic_archive":
		return doBoardREST(t, r, http.MethodPost, "/api/v1/me/epics/epc-bbbb2222/archive", `{}`)
	case "epic_unarchive":
		return doBoardREST(t, r, http.MethodPost, "/api/v1/me/epics/epc-bbbb2222/unarchive", `{}`)
	case "cancel_epic":
		return doBoardREST(t, r, http.MethodPost, "/api/v1/me/epics/epc-bbbb2222/cancel", `{"reason":"r"}`)
	case "create_task":
		return doBoardREST(t, r, http.MethodPost, "/api/v1/me/epics/epc-bbbb2222/tasks", `{"title":"T"}`)
	case "update_task":
		return doBoardREST(t, r, http.MethodPatch, "/api/v1/me/tasks/tsk-cccc3333", `{"title":"x"}`)
	case "add_dependency":
		return doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/tsk-cccc3333/dependencies", `{"dependsOn":"tsk-dddd4444"}`)
	case "remove_dependency":
		return doBoardREST(t, r, http.MethodDelete, "/api/v1/me/tasks/tsk-cccc3333/dependencies/tsk-dddd4444", "")
	case "move_stage":
		return doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/tsk-cccc3333/move-stage", `{"toStage":"done"}`)
	case "task_start":
		return doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/tsk-cccc3333/start", `{}`)
	case "cancel_task":
		return doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/tsk-cccc3333/cancel", `{"reason":"r"}`)
	default:
		t.Fatalf("no representative REST call for action %q — the matrix row is mis-declared", action)
		return nil
	}
}

// TestBoardParity_AgentColumn: the Lane 1 internal automation seam —
// worker actions live (heartbeat / report_throttle / move_stage /
// board_read), manager rows absent by design (the route table carries
// exactly the four worker routes), and the SHARED action (move_stage)
// renders guard classes IDENTICALLY to the REST column (the §4.3
// identical-guard-outcomes pin, both surfaces through renderBoardError).
func TestBoardParity_AgentColumn(t *testing.T) {
	svc := newFakeBoardService()
	r := newBoardAutomationRouter(t, svc)

	// Route table: exactly the four worker routes — manager rows are
	// absent BY DESIGN (§4.3c), and this pin fails if either side
	// drifts (a manager route added here, or a worker route dropped).
	var got []string
	for _, ri := range r.Routes() {
		got = append(got, ri.Method+" "+ri.Path)
	}
	sort.Strings(got)
	want := []string{
		"GET /internal/v1/automation/board/tasks/:id",
		"POST /internal/v1/automation/board/tasks/heartbeat",
		"POST /internal/v1/automation/board/tasks/move-stage",
		"POST /internal/v1/automation/board/tasks/throttle",
	}
	assert.Equal(t, want, got, "agent seam route table: worker actions live, manager rows absent")

	// Worker actions are live: one call each (fake succeeds).
	w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/heartbeat",
		`{"workspaceID":"ws-1","shortId":"tsk-cccc3333","claimToken":"tok","observedProgressAt":"2026-10-17T00:00:00Z"}`)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/throttle",
		`{"workspaceID":"ws-1","shortId":"tsk-cccc3333","claimToken":"tok","retryAfterSeconds":30}`)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// Guard-class parity on the shared action (move_stage): the same
	// BoardService error renders the same (status, code) on both
	// surfaces.
	parityCases := []struct {
		name     string
		err      error
		status   int
		codeJSON string
	}{
		{"done_guard", &board.GuardError{Code: board.GuardDone, Detail: "criteria unmet"}, http.StatusConflict, `"done_guard"`},
		{"claim_token", &board.GuardError{Code: board.GuardClaimToken, Detail: "stale claim"}, http.StatusConflict, `"claim_token"`},
		{"manager_surface", &board.GuardError{Code: board.GuardManagerSurface, Detail: "pending_input exits are human decisions"}, http.StatusConflict, `"manager_surface_only"`},
		{"authz", &board.GuardError{Code: board.GuardAuthz, Detail: "R7"}, http.StatusForbidden, `"authz"`},
	}
	for _, tc := range parityCases {
		t.Run("move_stage/"+tc.name, func(t *testing.T) {
			agentSvc := newFakeBoardService()
			agentSvc.moveErr = tc.err
			aw := doBoard(t, newBoardAutomationRouter(t, agentSvc), http.MethodPost, "/internal/v1/automation/board/tasks/move-stage",
				`{"workspaceID":"ws-1","shortId":"tsk-cccc3333","claimToken":"tok","toStage":"in_review"}`)
			restSvc := newFakeBoardManager()
			restSvc.errFor["move_stage"] = tc.err
			rw := doBoardREST(t, newBoardRESTRouter(t, restSvc), http.MethodPost, "/api/v1/me/tasks/tsk-cccc3333/move-stage", `{"toStage":"in_review"}`)
			assert.Equal(t, tc.status, aw.Code, "agent surface: "+aw.Body.String())
			assert.Equal(t, tc.status, rw.Code, "REST surface: "+rw.Body.String())
			assert.Contains(t, aw.Body.String(), tc.codeJSON, "agent surface code")
			assert.Contains(t, rw.Body.String(), tc.codeJSON, "REST surface code")
		})
	}
}

// TestBoardParity_ExternalMCPColumn: Lane 3 has not registered board
// tools yet — every external cell is pendingLane3, and this source-scan
// PROVES the absence (it greps pkg/mcp's tool registrations for board_*
// names). The moment Lane 3 registers a board tool this pin reddens and
// the tool's matrix cell must flip to a live row with its own guard
// arms — the matrix cannot silently miss a surface.
func TestBoardParity_ExternalMCPColumn(t *testing.T) {
	for _, row := range boardParityMatrix {
		if row.action == "claim_next" {
			// Engine-internal since R9 — permanently absent on every
			// surface, external MCP included.
			assert.Equal(t, "absent", row.externalMCP.kind, "action %s: external MCP cell", row.action)
			continue
		}
		assert.Equal(t, "pendingLane3", row.externalMCP.kind, "action %s: external MCP cell", row.action)
	}

	boardTools := scanMCPPkgForBoardTools(t)
	assert.Empty(t, boardTools,
		"pkg/mcp registers board tools %v while every external-MCP matrix cell is pendingLane3 — flip the cells to live rows with guard arms (design §4.3 parity)", boardTools)
}

// scanMCPPkgForBoardTools extracts tool names from pkg/mcp's tool
// registrations (mcp.NewTool("name", ...)) and returns the board_* ones.
func scanMCPPkgForBoardTools(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join("..", "..", "..", "pkg", "mcp")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "pkg/mcp not found — repo layout changed?")
	var boardTools []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, err)
		for _, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.Contains(trimmed, "mcp.NewTool(") {
				continue
			}
			open := strings.Index(trimmed, `"`)
			close_ := strings.Index(trimmed[open+1:], `"`)
			if open < 0 || close_ < 0 {
				continue
			}
			name := trimmed[open+1 : open+1+close_]
			if strings.HasPrefix(name, "board_") {
				boardTools = append(boardTools, name+" ("+e.Name()+")")
			}
		}
	}
	return boardTools
}

// TestBoardParity_WorkerOnlyAbsentOnREST: the declared asymmetry —
// heartbeat / report_throttle / claim_next have NO route on the REST
// surface (worker actions live on the agent seam only; claim_next is
// engine-internal everywhere, R9).
func TestBoardParity_WorkerOnlyAbsentOnREST(t *testing.T) {
	svc := newFakeBoardManager()
	r := newBoardRESTRouter(t, svc)
	for _, ri := range r.Routes() {
		lower := strings.ToLower(ri.Path)
		assert.NotContains(t, lower, "heartbeat", "worker-only action routed on REST (§4.7)")
		assert.NotContains(t, lower, "throttle", "worker-only action routed on REST (§4.7)")
		assert.NotContains(t, lower, "claim", "claim_next is engine-internal (R9) — never routed")
	}
	// And the direct calls 404 (no route, no handler).
	for _, path := range []string{"/api/v1/me/tasks/tsk-x/heartbeat", "/api/v1/me/tasks/tsk-x/throttle", "/api/v1/me/tasks/tsk-x/claim"} {
		w := doBoardREST(t, r, http.MethodPost, path, `{}`)
		assert.Equal(t, http.StatusNotFound, w.Code, path)
	}
}
