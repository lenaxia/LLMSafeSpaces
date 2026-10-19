// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_parity_test.go — the design 0067 §4.3/§6 parity matrix, Lane 2
// shape: rows = every §4.2 action; columns = REST / external platform
// MCP / agent-internal MCP. The REST column is LIVE (allow + deny arms
// per action), the agent column is LIVE on BOTH of its tiers — the
// Lane 1 internal automation seam (worker actions present; manager
// rows absent-by-design; guard-class parity on the shared action) and,
// since #1701, the agentd REGISTRY itself (the board_* tools the
// in-workspace worker sees in tools/list: exactly the four worker
// tools, claim_next and every manager row pinned ABSENT at the
// registry tier) — and the external MCP column is LIVE since #1700
// (Lane 3): the board_* tools ride the owner's REST projection
// through pkg/mcp's HTTPClient, so the LIVE arms below drive the REAL
// REST router fixture over the in-process MCP client — guard outcomes
// ride the identical renderBoardError path as the REST column (parity
// by construction, asserted per row). Audit stamping happens at the
// REST tier the tools ride (surface=rest); pkg/board's
// SurfaceExternalMCP stays reserved for a direct seam if one ever
// lands (recorded in this PR's worklog,
// worklogs/NNNN_2026-10-18_1700-external-board-mcp-tools.md §2 — the
// NNNN_ sentinel before the post-merge numbering bot runs).
//
// Construction guarantees (the "CI fails on any action lacking full
// surface coverage" clause): reflection pins EVERY exported method of
// *BoardHandler to exactly one matrix row's rest method set, the route
// scans pin exactly which routes each surface registers, and the
// external column's tool-set agreement pins every registered board_*
// tool to a matrix row. An endpoint or tool added without its row
// reddens TestBoardParity_Coverage / TestBoardParity_ExternalMCPColumn.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	mcpgoclient "github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpgoserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
	llmcp "github.com/lenaxia/llmsafespaces/pkg/mcp"
)

// parityCell is one (action × surface) outcome. live = exercised
// below; absent = declared asymmetry (the surface refuses/does not
// expose the action BY DESIGN, pinned as an outcome row per §6).
type parityCell struct {
	kind string // "live" | "absent"
}

type parityRow struct {
	action string // §4.2 action name (board_events.action vocabulary)

	restMethods []string // exported *BoardHandler methods (the REST cell's live surface)
	rest        parityCell
	// extTools names the pkg/mcp tools carrying the action on the
	// external MCP surface (shared tools may appear in several rows).
	extTools    []string
	externalMCP parityCell
	agent       parityCell

	// agentTool names the agentd registry tool for a live agent cell
	// (#1701): the tool the in-workspace worker sees in tools/list. An
	// absent agent cell names none — scanAgentdForBoardTools enforces
	// both directions against cmd/workspace-agentd's source.
	agentTool string

	// denyClass, when set, is the representative guard the REST allow/
	// deny arms drive (the fake service returns it; the arm asserts the
	// mapped HTTP class).
	denyErr  error
	denyCode int
}

var (
	liveCell   = parityCell{kind: "live"}
	absentCell = parityCell{kind: "absent"}
)

// boardParityMatrix — THE §4.2 action set. Declared asymmetries are
// pinned as absent cells, not missing coverage (§6):
//   - heartbeat / report_throttle: worker-only — absent on REST and on
//     external MCP (§4.3b: "no claim/heartbeat here"); live on the
//     agent seam.
//   - claim_next: engine-internal (R9 claim-then-spawn) — absent on ALL
//     three surfaces, permanently.
//   - manager rows (CRUD/lifecycle/cancel/dependencies/task_start):
//     absent on the agent registry — agents are workers, not managers
//     (§4.3c).
//   - move_stage + board_read: on every surface (owner authority on
//     REST/external-MCP; token-guarded for the worker on the agent
//     registry).
var boardParityMatrix = []parityRow{
	{action: "create_project", restMethods: []string{"CreateProject"}, rest: liveCell,
		externalMCP: liveCell, extTools: []string{"board_create_project"}, agent: absentCell,
		denyErr: &board.GuardError{Code: board.GuardWorkspaceBinding, Detail: "target workspace must be accessible"}, denyCode: http.StatusForbidden},
	{action: "update_project", restMethods: []string{"UpdateProject"}, rest: liveCell,
		externalMCP: liveCell, extTools: []string{"board_update_item"}, agent: absentCell},
	{action: "archive_project", restMethods: []string{"ArchiveProject"}, rest: liveCell,
		externalMCP: liveCell, extTools: []string{"board_project_archive"}, agent: absentCell},
	{action: "unarchive_project", restMethods: []string{"UnarchiveProject"}, rest: liveCell,
		externalMCP: liveCell, extTools: []string{"board_project_unarchive"}, agent: absentCell},
	{action: "board_read", restMethods: []string{"ListProjects", "GetProject", "ListProjectTasks", "GetEpic", "GetTask"}, rest: liveCell,
		externalMCP: liveCell, extTools: []string{"board_list_projects", "board_get", "board_read"}, agent: liveCell,
		agentTool: "board_read"}, // v1 item form (GetTaskInWorkspace); list/search/health ride the #1699 engine seams
	{action: "create_epic", restMethods: []string{"CreateEpic"}, rest: liveCell,
		externalMCP: liveCell, extTools: []string{"board_create_epic"}, agent: absentCell},
	{action: "update_epic", restMethods: []string{"UpdateEpic"}, rest: liveCell,
		externalMCP: liveCell, extTools: []string{"board_update_item"}, agent: absentCell},
	{action: "epic_activate", restMethods: []string{"ActivateEpic"}, rest: liveCell,
		externalMCP: liveCell, extTools: []string{"board_epic_activate"}, agent: absentCell,
		denyErr: &board.GuardError{Code: board.GuardEpicState, Detail: "not a lifecycle transition"}, denyCode: http.StatusConflict},
	{action: "epic_pause", restMethods: []string{"PauseEpic"}, rest: liveCell,
		externalMCP: liveCell, extTools: []string{"board_epic_pause"}, agent: absentCell,
		denyErr: &board.GuardError{Code: board.GuardEpicState, Detail: "not a lifecycle transition"}, denyCode: http.StatusConflict},
	{action: "epic_archive", restMethods: []string{"ArchiveEpic"}, rest: liveCell,
		externalMCP: liveCell, extTools: []string{"board_epic_archive"}, agent: absentCell},
	{action: "epic_unarchive", restMethods: []string{"UnarchiveEpic"}, rest: liveCell,
		externalMCP: liveCell, extTools: []string{"board_epic_unarchive"}, agent: absentCell},
	{action: "cancel_epic", restMethods: []string{"CancelEpic"}, rest: liveCell,
		externalMCP: liveCell, extTools: []string{"board_cancel"}, agent: absentCell},
	{action: "create_task", restMethods: []string{"CreateEpicTask"}, rest: liveCell,
		externalMCP: liveCell, extTools: []string{"board_create_task"}, agent: absentCell},
	{action: "update_task", restMethods: []string{"UpdateTask"}, rest: liveCell,
		externalMCP: liveCell, extTools: []string{"board_update_item"}, agent: absentCell},
	{action: "add_dependency", restMethods: []string{"AddDependency"}, rest: liveCell,
		externalMCP: liveCell, extTools: []string{"board_add_dependency"}, agent: absentCell,
		denyErr: &board.DependencyError{Code: "cycle", Detail: "would create a cycle"}, denyCode: http.StatusConflict},
	{action: "remove_dependency", restMethods: []string{"RemoveDependency"}, rest: liveCell,
		externalMCP: liveCell, extTools: []string{"board_remove_dependency"}, agent: absentCell},
	{action: "move_stage", restMethods: []string{"MoveStageTask"}, rest: liveCell,
		externalMCP: liveCell, extTools: []string{"board_move_stage"}, agent: liveCell,
		agentTool: "board_move_stage",
		denyErr:   &board.GuardError{Code: board.GuardDone, Detail: "criteria unmet or evidence missing"}, denyCode: http.StatusConflict},
	{action: "task_start", restMethods: []string{"StartTask"}, rest: liveCell,
		externalMCP: liveCell, extTools: []string{"board_start_task"}, agent: absentCell, // owner surface only (§4.3b)
		denyErr: &board.GuardError{Code: board.GuardEpicState, Detail: "paused epics refuse task_start"}, denyCode: http.StatusConflict},
	{action: "cancel_task", restMethods: []string{"CancelTask"}, rest: liveCell,
		externalMCP: liveCell, extTools: []string{"board_cancel"}, agent: absentCell},
	// Worker-only rows: absent on REST BY DESIGN (no owner surface
	// delivers them — §4.7) and absent on external MCP BY DESIGN (§4.3b:
	// external actors hold no claim tokens); live on the agent seam
	// (claim-token guarded).
	{action: "heartbeat", restMethods: nil, rest: absentCell, externalMCP: absentCell, agent: liveCell,
		agentTool: "board_heartbeat"},
	{action: "report_throttle", restMethods: nil, rest: absentCell, externalMCP: absentCell, agent: liveCell,
		agentTool: "board_report_throttle"},
	// Engine-internal since R9 — absent on ALL surfaces, permanently.
	// Since #1701 the agent surface's absence is enforced at BOTH tiers:
	// the seam route table (TestBoardParity_AgentColumn) and the agentd
	// registry scan (TestBoardParity_AgentRegistryColumn) — a
	// board_claim_next tool registered in agentd reddens here.
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

// TestBoardParity_AgentRegistryColumn: #1701's tier of the agent column —
// the agentd REGISTRY (cmd/workspace-agentd's mcpToolCatalog + the
// callMCPTool dispatcher), the tool surface the in-workspace worker
// actually sees. Construction guarantee, the scanMCPPkgForBoardTools
// discipline applied to the agent tier: the board_* tools registered in
// agentd are EXACTLY the matrix's live agent cells' tools — a tool
// added to agentd without a matrix row reddens here (an unregistered
// live cell reddens too), and the declared absences are enforced AT the
// registry: claim_next (engine-internal, R9 — absent on all surfaces,
// permanently) and every manager row have NO registered tool.
func TestBoardParity_AgentRegistryColumn(t *testing.T) {
	catalog, dispatch := scanAgentdForBoardTools(t)

	want := map[string]bool{}
	for _, row := range boardParityMatrix {
		if row.agent.kind == "live" {
			require.NotEmpty(t, row.agentTool, "action %s: live agent cell must name its registry tool", row.action)
			want[row.agentTool] = true
		} else {
			require.Empty(t, row.agentTool, "action %s: absent agent cell must name no registry tool", row.action)
		}
	}
	assert.Equal(t, want, catalog,
		"agentd's board_* registry must be exactly the matrix's live agent tools (manager rows and claim_next absent BY DESIGN, §4.3c/R9)")

	for tool := range catalog {
		assert.True(t, dispatch[tool], "registry tool %s has no dispatcher case — the catalog would be a brochure", tool)
	}
	assert.NotEmpty(t, dispatch, "the board dispatcher wiring must exist")
}

// scanAgentdForBoardTools extracts (catalog, dispatch) board_* tool
// name sets from cmd/workspace-agentd's source: catalog names from
// mcpTool entries (`Name: "board_…"`), dispatch names from switch case
// clauses. Both directions feed the parity construction guarantee.
func scanAgentdForBoardTools(t *testing.T) (catalog, dispatch map[string]bool) {
	t.Helper()
	dir := filepath.Join("..", "..", "..", "cmd", "workspace-agentd")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "cmd/workspace-agentd not found — repo layout changed?")
	catalog, dispatch = map[string]bool{}, map[string]bool{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, err)
		for _, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			for _, name := range quotedBoardNames(trimmed) {
				if strings.HasPrefix(trimmed, "Name:") {
					catalog[name] = true
				}
				if strings.HasPrefix(trimmed, "case ") {
					dispatch[name] = true
				}
			}
		}
	}
	return catalog, dispatch
}

// quotedBoardNames returns every "board_…" string literal on the line.
func quotedBoardNames(line string) []string {
	var out []string
	for i := 0; i < len(line); i++ {
		if line[i] != '"' {
			continue
		}
		rest := line[i+1:]
		end := strings.IndexByte(rest, '"')
		if end < 0 {
			break
		}
		if strings.HasPrefix(rest[:end], "board_") {
			out = append(out, rest[:end])
		}
		i += end + 1
	}
	return out
}

// TestBoardParity_ExternalMCPColumn: the Lane 3 flip (#1700) — every
// manager/read row is LIVE on the external MCP surface, the worker
// rows (heartbeat / report_throttle / claim_next) are absent BY DESIGN,
// and the tool-set agreement holds in BOTH directions: every registered
// board_* tool belongs to a matrix row, and every live external cell
// names real registered tools (a tool without a row, or a row naming a
// ghost tool, reddens here — the matrix cannot silently miss a
// surface). The LIVE arms then drive each row's representative tool
// through the REAL REST router fixture (pkg/mcp tools → HTTPClient →
// /api/v1/me routes → BoardHandler → renderBoardError), asserting:
// allow outcomes, the SAME guard classes the REST column renders
// (deny arms), the R7 owner scope (every request path is a me-route),
// and foreign-row non-disclosure.
func TestBoardParity_ExternalMCPColumn(t *testing.T) {
	// (a) Cell-kind agreement: manager/read rows live with named tools;
	// declared-asymmetric rows absent with none.
	for _, row := range boardParityMatrix {
		switch row.action {
		case "heartbeat", "report_throttle":
			assert.Equal(t, "absent", row.externalMCP.kind,
				"%s: worker-only action — external actors hold no claim tokens (§4.3b)", row.action)
			assert.Empty(t, row.extTools, "%s: absent cells name no tools", row.action)
		case "claim_next":
			assert.Equal(t, "absent", row.externalMCP.kind,
				"%s: engine-internal since R9 — permanently absent on every surface", row.action)
			assert.Empty(t, row.extTools, "%s: absent cells name no tools", row.action)
		default:
			assert.Equal(t, "live", row.externalMCP.kind, "%s: external MCP cell", row.action)
			assert.NotEmpty(t, row.extTools, "%s: live external cell must name its tools", row.action)
		}
	}

	// (b) Tool-set agreement, both directions, against the live
	// registration (plus the source scan: registrations live in pkg/mcp).
	registered := map[string]bool{}
	for name := range externalMCPTestServer(t).ListTools() {
		if strings.HasPrefix(name, "board_") {
			registered[name] = true
		}
	}
	inMatrix := map[string]bool{}
	for _, row := range boardParityMatrix {
		for _, tool := range row.extTools {
			inMatrix[tool] = true
			assert.True(t, registered[tool],
				"matrix row %s names tool %s which pkg/mcp does not register — a ghost cell", row.action, tool)
		}
	}
	var unregistered []string
	for name := range registered {
		if !inMatrix[name] {
			unregistered = append(unregistered, name)
		}
	}
	sort.Strings(unregistered)
	assert.Empty(t, unregistered,
		"pkg/mcp registers board tools with NO matrix row (parity by construction requires a row per surface addition): %v", unregistered)
	for _, banned := range []string{"board_heartbeat", "board_report_throttle"} {
		assert.False(t, registered[banned], "%s must not exist on the external surface (worker-only, §4.3b)", banned)
	}
	for name := range registered {
		assert.NotContains(t, name, "claim", "no board_* claim tool exists on any surface (claim_next is engine-internal, R9)")
	}
	sourceTools := scanMCPPkgForBoardTools(t)
	assert.NotEmpty(t, sourceTools, "the source scan no longer finds board tool registrations — pkg/mcp layout changed?")

	// (c) LIVE arms: allow + deny per row, driven through the real REST
	// fixture over the in-process MCP client.
	for _, row := range boardParityMatrix {
		if row.externalMCP.kind != "live" {
			continue
		}
		t.Run(row.action, func(t *testing.T) {
			// Allow arm.
			svc := newFakeBoardManager()
			svc.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "user-1", Status: board.ProjectActive}}
			svc.tasks = []board.Task{{ID: "t1", ShortID: "tsk-cccc3333", ProjectID: "p1", EpicID: "e1", Stage: board.StageBacklog, Tags: []string{}}}
			cl, paths := newExternalMCPFixture(t, svc)
			res, text := boardParityExtCall(t, cl, row.action)
			assert.False(t, res.IsError, "allow arm %s: %s", row.action, text)

			// R7 owner scope: every request the tool issued rides the
			// authenticated me-routes (never org, never internal seams).
			for _, p := range paths() {
				path := p
				if i := strings.Index(p, " "); i >= 0 {
					path = p[i+1:]
				}
				assert.True(t, strings.HasPrefix(path, "/api/v1/me/"),
					"%s: external tools ride the owner's REST projection only — saw %q", row.action, p)
			}

			// Deny arm (rows with a representative guard): the tool
			// result carries the SAME class the REST column renders.
			if row.denyErr != nil {
				svc2 := newFakeBoardManager()
				svc2.tasks = []board.Task{{ID: "t1", ShortID: "tsk-cccc3333", ProjectID: "p1", EpicID: "e1", Stage: board.StageBacklog, Tags: []string{}}}
				svc2.errFor[boardParitySeam(row.action)] = row.denyErr
				cl2, _ := newExternalMCPFixture(t, svc2)
				res2, text2 := boardParityExtCall(t, cl2, row.action)
				assert.True(t, res2.IsError, "deny arm %s: a guard denial renders a tool error", row.action)
				assert.Contains(t, text2, fmt.Sprintf("API error %d", row.denyCode),
					"%s: the mapped HTTP class rides verbatim", row.action)
			}
		})
	}

	// (d) Guard-class parity on the shared action (move_stage): every
	// class the agent/REST parity cases pin renders the same (status,
	// code) through the external tool.
	parityCases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"done_guard", &board.GuardError{Code: board.GuardDone, Detail: "criteria unmet"}, http.StatusConflict, "done_guard"},
		{"claim_token", &board.GuardError{Code: board.GuardClaimToken, Detail: "stale claim"}, http.StatusConflict, "claim_token"},
		{"manager_surface", &board.GuardError{Code: board.GuardManagerSurface, Detail: "pending_input exits are human decisions"}, http.StatusConflict, "manager_surface_only"},
		{"authz", &board.GuardError{Code: board.GuardAuthz, Detail: "R7"}, http.StatusForbidden, "authz"},
	}
	for _, tc := range parityCases {
		t.Run("move_stage/"+tc.name, func(t *testing.T) {
			svc := newFakeBoardManager()
			svc.errFor["move_stage"] = tc.err
			cl, _ := newExternalMCPFixture(t, svc)
			res, text := boardParityExtCall(t, cl, "move_stage")
			assert.True(t, res.IsError)
			assert.Contains(t, text, fmt.Sprintf("API error %d", tc.status), "same status class as REST/agent columns")
			assert.Contains(t, text, tc.code, "engine code rides verbatim")
		})
	}

	// (e) Foreign-row non-disclosure through the tool: the REST
	// perimeter's plain 404 (no suggestions, no owner identity) is what
	// the external caller sees — a foreign board is indistinguishable
	// from a missing one at this tier too.
	svc := newFakeBoardManager()
	svc.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "someone-else", Status: board.ProjectActive}}
	cl, _ := newExternalMCPFixture(t, svc)
	res, text := boardParityExtCall(t, cl, "board_read_project_get")
	assert.True(t, res.IsError, "foreign row: %s", text)
	assert.Contains(t, text, "API error 404")
	assert.NotContains(t, text, "someone-else", "foreign owner identity never disclosed")
	assert.NotContains(t, text, "suggestions", "foreign rows never carry did-you-mean suggestions")
}

// boardParityExtToolArgs maps a matrix action to its representative
// external tool + arguments (the same representative shape
// boardParityRESTCall uses for the REST column).
func boardParityExtToolArgs(action string) (string, map[string]any, bool) {
	switch action {
	case "create_project":
		return "board_create_project", map[string]any{"name": "B", "target_workspace_id": "ws-1"}, true
	case "update_project":
		return "board_update_item", map[string]any{"item_id": "prj-aaaa1111", "name": "x"}, true
	case "archive_project":
		return "board_project_archive", map[string]any{"project_id": "prj-aaaa1111"}, true
	case "unarchive_project":
		return "board_project_unarchive", map[string]any{"project_id": "prj-aaaa1111"}, true
	case "board_read":
		return "board_list_projects", map[string]any{}, true
	case "create_epic":
		return "board_create_epic", map[string]any{"project_id": "prj-aaaa1111", "title": "E"}, true
	case "update_epic":
		return "board_update_item", map[string]any{"item_id": "epc-bbbb2222", "outcome": "o"}, true
	case "epic_activate":
		return "board_epic_activate", map[string]any{"epic_id": "epc-bbbb2222"}, true
	case "epic_pause":
		return "board_epic_pause", map[string]any{"epic_id": "epc-bbbb2222"}, true
	case "epic_archive":
		return "board_epic_archive", map[string]any{"epic_id": "epc-bbbb2222"}, true
	case "epic_unarchive":
		return "board_epic_unarchive", map[string]any{"epic_id": "epc-bbbb2222"}, true
	case "cancel_epic":
		return "board_cancel", map[string]any{"item_id": "epc-bbbb2222", "reason": "r"}, true
	case "create_task":
		return "board_create_task", map[string]any{"epic_id": "epc-bbbb2222", "title": "T"}, true
	case "update_task":
		return "board_update_item", map[string]any{"item_id": "tsk-cccc3333", "title": "x"}, true
	case "add_dependency":
		return "board_add_dependency", map[string]any{"task_id": "tsk-cccc3333", "depends_on": "tsk-dddd4444"}, true
	case "remove_dependency":
		return "board_remove_dependency", map[string]any{"task_id": "tsk-cccc3333", "depends_on": "tsk-dddd4444"}, true
	case "move_stage":
		return "board_move_stage", map[string]any{"task_id": "tsk-cccc3333", "to_stage": "done"}, true
	case "task_start":
		return "board_start_task", map[string]any{"task_id": "tsk-cccc3333"}, true
	case "cancel_task":
		return "board_cancel", map[string]any{"item_id": "tsk-cccc3333", "reason": "r"}, true
	case "board_read_project_get":
		return "board_get", map[string]any{"item_id": "prj-aaaa1111"}, true
	default:
		return "", nil, false
	}
}

// boardParityExtCall issues the action's representative external tool
// call through the in-process MCP client.
func boardParityExtCall(t *testing.T, cl *mcpgoclient.Client, action string) (*mcpgo.CallToolResult, string) {
	t.Helper()
	tool, args, ok := boardParityExtToolArgs(action)
	require.True(t, ok, "no representative external tool call for action %q — the matrix row is mis-declared", action)
	return boardParityExtToolCall(t, cl, tool, args)
}

// externalMCPTestServer builds the external MCP server (registration
// listing only — its client is never exercised).
func externalMCPTestServer(t *testing.T) *mcpgoserver.MCPServer {
	t.Helper()
	return llmcp.NewServer(&llmcp.HTTPClient{BaseURL: "http://127.0.0.1:1", HTTPClient: &http.Client{}}, 30*time.Second)
}

// newExternalMCPFixture stands the external column up LIVE against the
// REAL REST router fixture: pkg/mcp tools → HTTPClient → httptest
// server (the same newBoardRESTRouter mounting the REST column pins)
// → BoardHandler → renderBoardError. The returned probe reports every
// request path the tools issued (the R7 owner-scope assertion's feed).
func newExternalMCPFixture(t *testing.T, svc *fakeBoardManager) (*mcpgoclient.Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	r := newBoardRESTRouter(t, svc)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		paths = append(paths, req.Method+" "+req.URL.Path)
		mu.Unlock()
		r.ServeHTTP(w, req)
	}))
	t.Cleanup(api.Close)

	mcpSrv := llmcp.NewServer(&llmcp.HTTPClient{
		BaseURL: api.URL, HTTPClient: &http.Client{}, APIKey: "parity-key",
	}, 30*time.Second)
	cl, err := mcpgoclient.NewInProcessClient(mcpSrv)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cl.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, cl.Start(ctx))
	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "board-parity", Version: "1.0"}
	_, err = cl.Initialize(ctx, initReq)
	require.NoError(t, err)
	return cl, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
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

// --- §6 cell-case kinds beyond allow/deny: idempotent replay + audit ---

// replayExceptions names the mutating actions whose requestId does NOT
// reach an event writer TODAY, so the replay case skips them BY NAME
// (a conscious, reviewable list — not silent coverage loss):
//   - archive_project/unarchive_project/epic_activate/epic_pause/
//     epic_archive/epic_unarchive: the engine's lifecycle writers
//     discard the requestID parameter (pkg/board service.go:309/:462 —
//     flagged on PR #1728 as an engine follow-up; no pkg/board edit
//     from this lane).
//   - remove_dependency: DELETE carries no body at the REST tier (the
//     engine seam itself would thread one).
//
// When the engine follow-up lands, entries come OFF this list and the
// replay case covers them.
var replayExceptions = map[string]string{
	"archive_project":   "engine lifecycle writer discards requestId (service.go:309)",
	"unarchive_project": "engine lifecycle writer discards requestId (service.go:309)",
	"epic_activate":     "engine lifecycle writer discards requestId (service.go:462)",
	"epic_pause":        "engine lifecycle writer discards requestId (service.go:462)",
	"epic_archive":      "engine lifecycle writer discards requestId (service.go:462)",
	"epic_unarchive":    "engine lifecycle writer discards requestId (service.go:462)",
	"remove_dependency": "DELETE carries no body at the REST tier",
}

// boardReadActions are the non-mutating rows (no replay/audit arms).
func boardReadActions() map[string]bool {
	return map[string]bool{"board_read": true, "heartbeat": true, "report_throttle": true, "claim_next": true}
}

// TestBoardParity_IdempotentReplay: the clientMessageID discipline at
// the REST tier — a mutating action re-invoked with the SAME requestId
// renders 200-with-original, never an error, and the adapter delivers
// the identical key on both calls (SDK clients reuse one key per
// logical operation). The fake resolves replays the way the engine's
// event writer does (Duplicate{AcceptedID} → 200-with-original), so a
// handler that translated a replay into an error class would redden.
func TestBoardParity_IdempotentReplay(t *testing.T) {
	for _, row := range boardParityMatrix {
		if row.rest.kind != "live" || boardReadActions()[row.action] {
			continue
		}
		t.Run(row.action, func(t *testing.T) {
			if reason, skipped := replayExceptions[row.action]; skipped {
				t.Skipf("requestId does not reach an event writer today: %s", reason)
			}
			svc := newFakeBoardManager()
			svc.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "user-1", Status: board.ProjectActive}}
			svc.tasks = []board.Task{{ID: "t1", ShortID: "tsk-cccc3333", ProjectID: "p1", EpicID: "e1", Stage: board.StageBacklog, Tags: []string{}}}
			r := newBoardRESTRouter(t, svc)

			const key = "11111111-2222-3333-4444-555555555555"
			w1 := boardParityRESTCallReqID(t, r, row.action, key)
			require.Less(t, w1.Code, 300, "first call: %s", w1.Body.String())
			require.Equal(t, key, svc.lastRequestID, "first call delivers the caller's key")
			w2 := boardParityRESTCallReqID(t, r, row.action, key)
			require.Equal(t, w1.Code, w2.Code, "replay renders the original outcome's status (2xx-with-original — 201 stays 201), never an error: %s", w2.Body.String())
			require.Equal(t, key, svc.lastRequestID, "replay delivers the SAME key")
			assert.Equal(t, w1.Body.String(), w2.Body.String(), "replay returns the original outcome verbatim")
		})
	}
}

// TestBoardParity_AuditRowInputs: one audit trail, four surfaces — the
// board_events.actor inputs (kind, user, surface) are stamped by the
// adapter on every mutating action; the shared action (move_stage)
// delivers kind=user + the acting user on BOTH the REST and agent
// surfaces (workers delegate as the owning user, R7), differing only in
// the surface marker.
func TestBoardParity_AuditRowInputs(t *testing.T) {
	for _, row := range boardParityMatrix {
		if row.rest.kind != "live" || boardReadActions()[row.action] {
			continue
		}
		t.Run(row.action, func(t *testing.T) {
			svc := newFakeBoardManager()
			svc.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "user-1", Status: board.ProjectActive}}
			svc.tasks = []board.Task{{ID: "t1", ShortID: "tsk-cccc3333", ProjectID: "p1", EpicID: "e1", Stage: board.StageBacklog, Tags: []string{}}}
			r := newBoardRESTRouter(t, svc)
			_ = boardParityRESTCall(t, r, row.action)
			assert.Equal(t, board.ActorUser, svc.lastActor.Kind, row.action)
			assert.NotEmpty(t, svc.lastActor.UserID, row.action)
			assert.Equal(t, board.SurfaceREST, svc.lastActor.Surface, row.action)
			assert.Empty(t, svc.lastActor.ClaimToken, row.action+" — manager surface holds no token")
		})
	}

	// Shared action across surfaces: identical audit inputs modulo the
	// surface marker (the §4.3 identical-guard/audit shape).
	agentSvc := newFakeBoardService()
	aw := doBoard(t, newBoardAutomationRouter(t, agentSvc), http.MethodPost, "/internal/v1/automation/board/tasks/move-stage",
		`{"workspaceID":"ws-1","shortId":"tsk-cccc3333","claimToken":"tok","toStage":"in_review"}`)
	require.Equal(t, http.StatusOK, aw.Code, aw.Body.String())

	restSvc := newFakeBoardManager()
	restSvc.tasks = []board.Task{{ID: "t1", ShortID: "tsk-cccc3333", ProjectID: "p1", EpicID: "e1", Stage: board.StageBacklog, Tags: []string{}}}
	rw := doBoardREST(t, newBoardRESTRouter(t, restSvc), http.MethodPost, "/api/v1/me/tasks/tsk-cccc3333/move-stage", `{"toStage":"in_review"}`)
	require.Equal(t, http.StatusOK, rw.Code, rw.Body.String())

	assert.Equal(t, agentSvc.lastActor.Kind, restSvc.lastActor.Kind, "move_stage: same actor kind on both surfaces")
	assert.Equal(t, board.ActorUser, agentSvc.lastActor.Kind, "workers delegate as the owning user (R7)")
	assert.NotEmpty(t, agentSvc.lastActor.UserID, "agent surface resolves the delegating user")
	assert.NotEmpty(t, restSvc.lastActor.UserID, "REST surface stamps the JWT user")
	assert.Equal(t, board.SurfaceAgent, agentSvc.lastActor.Surface)
	assert.Equal(t, board.SurfaceREST, restSvc.lastActor.Surface)
}

// boardParityRESTCallReqID issues the representative request with a
// requestId woven into the body (the replay/audit arms' driver).
func boardParityRESTCallReqID(t *testing.T, r *gin.Engine, action, requestID string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"requestId":%q}`, requestID)
	switch action {
	case "create_project":
		return doBoardREST(t, r, http.MethodPost, "/api/v1/me/projects", `{"name":"B","targetWorkspaceId":"ws-1","requestId":`+q(requestID)+`}`)
	case "update_project":
		return doBoardREST(t, r, http.MethodPatch, "/api/v1/me/projects/prj-aaaa1111", `{"name":"x","requestId":`+q(requestID)+`}`)
	case "create_epic":
		return doBoardREST(t, r, http.MethodPost, "/api/v1/me/projects/prj-aaaa1111/epics", `{"title":"E","requestId":`+q(requestID)+`}`)
	case "update_epic":
		return doBoardREST(t, r, http.MethodPatch, "/api/v1/me/epics/epc-bbbb2222", `{"outcome":"o","requestId":`+q(requestID)+`}`)
	case "cancel_epic":
		return doBoardREST(t, r, http.MethodPost, "/api/v1/me/epics/epc-bbbb2222/cancel", `{"reason":"r","requestId":`+q(requestID)+`}`)
	case "create_task":
		return doBoardREST(t, r, http.MethodPost, "/api/v1/me/epics/epc-bbbb2222/tasks", `{"title":"T","requestId":`+q(requestID)+`}`)
	case "update_task":
		return doBoardREST(t, r, http.MethodPatch, "/api/v1/me/tasks/tsk-cccc3333", `{"title":"x","requestId":`+q(requestID)+`}`)
	case "add_dependency":
		return doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/tsk-cccc3333/dependencies", `{"dependsOn":"tsk-dddd4444","requestId":`+q(requestID)+`}`)
	case "move_stage":
		return doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/tsk-cccc3333/move-stage", `{"toStage":"done","requestId":`+q(requestID)+`}`)
	case "task_start":
		return doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/tsk-cccc3333/start", body)
	case "cancel_task":
		return doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/tsk-cccc3333/cancel", `{"reason":"r","requestId":`+q(requestID)+`}`)
	default:
		t.Fatalf("no replay-driver call for action %q", action)
		return nil
	}
}

// TestBoardParity_ExternalMCPIdempotentReplay (r1): the §6 per-cell
// contract's replay arm on the external column — the same driver as
// the REST column's TestBoardParity_IdempotentReplay, driven through
// the tool path (in-process MCP client → HTTPClient → the real REST
// router). A mutating tool re-invoked with the SAME request_id renders
// the original outcome, never an error, and the key reaches the event
// writer's seam on BOTH calls — an HTTPClient regression that drops or
// mangles requestId per-method reddens here, through the real wiring.
func TestBoardParity_ExternalMCPIdempotentReplay(t *testing.T) {
	for _, row := range boardParityMatrix {
		if row.externalMCP.kind != "live" || boardReadActions()[row.action] {
			continue
		}
		t.Run(row.action, func(t *testing.T) {
			if reason, skipped := replayExceptions[row.action]; skipped {
				t.Skipf("requestId does not reach an event writer today: %s", reason)
			}
			svc := newFakeBoardManager()
			svc.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "user-1", Status: board.ProjectActive}}
			svc.tasks = []board.Task{{ID: "t1", ShortID: "tsk-cccc3333", ProjectID: "p1", EpicID: "e1", Stage: board.StageBacklog, Tags: []string{}}}
			cl, _ := newExternalMCPFixture(t, svc)

			const key = "11111111-2222-3333-4444-555555555555"
			res1, text1 := boardParityExtCallReqID(t, cl, row.action, key)
			assert.False(t, res1.IsError, "first call: %s", text1)
			assert.Equal(t, key, svc.lastRequestID, "first call delivers the caller's key")
			res2, text2 := boardParityExtCallReqID(t, cl, row.action, key)
			assert.False(t, res2.IsError, "replay renders the original outcome's success (200-with-original), never an error: %s", text2)
			assert.Equal(t, key, svc.lastRequestID, "replay delivers the SAME key")
			assert.Equal(t, text1, text2, "replay returns the original outcome verbatim")
		})
	}
}

// TestBoardParity_ExternalMCPAuditRow (r1): the §6 audit arm on the
// external column — a mutating tool driven through the real wiring
// stamps the SAME audit inputs the REST column stamps (workers
// delegate as the owning user, R7; manager surfaces hold no claim
// token). The audit row's surface marker is the REST projection's
// ("rest") BY CONSTRUCTION: the external tools ride the owner's REST
// routes (the recorded #1700 architecture decision; the
// SurfaceExternalMCP reservation is tracked in the follow-up issue
// referenced from the worklog). This pin holds the deviation honest —
// if the stamping ever changes, this reddens and the record moves.
func TestBoardParity_ExternalMCPAuditRow(t *testing.T) {
	svc := newFakeBoardManager()
	svc.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "user-1", Status: board.ProjectActive}}
	svc.tasks = []board.Task{{ID: "t1", ShortID: "tsk-cccc3333", ProjectID: "p1", EpicID: "e1", Stage: board.StageBacklog, Tags: []string{}}}
	cl, _ := newExternalMCPFixture(t, svc)
	res, text := boardParityExtCall(t, cl, "move_stage")
	assert.False(t, res.IsError, text)

	assert.Equal(t, board.ActorUser, svc.lastActor.Kind, "external-arriving actions audit as the owning user (R7 delegation shape)")
	assert.NotEmpty(t, svc.lastActor.UserID, "the authenticated owner's identity reaches the audit row")
	assert.Equal(t, board.SurfaceREST, svc.lastActor.Surface,
		"the REST tier the tools ride stamps the surface — the recorded deviation (see worklog + follow-up issue)")
	assert.Empty(t, svc.lastActor.ClaimToken, "manager surfaces hold no claim token (§4.2's in-flight guard)")
}

// TestBoardParity_ExternalMCPReadVariants (r1): the read tools' full
// wire coverage — board_read's stage filter (query encoding through
// HTTPClient), and board_get on every kind (prj-/epc-/tsk- resolution
// through the real router).
func TestBoardParity_ExternalMCPReadVariants(t *testing.T) {
	svc := newFakeBoardManager()
	svc.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "user-1", Status: board.ProjectActive}}
	svc.epics = []board.Epic{{ID: "e1", ShortID: "epc-bbbb2222", ProjectID: "p1", Status: board.EpicActive}}
	svc.tasks = []board.Task{{ID: "t1", ShortID: "tsk-cccc3333", ProjectID: "p1", EpicID: "e1", Stage: board.StageBacklog, Tags: []string{}}}
	cl, _ := newExternalMCPFixture(t, svc)

	// board_read with the stage filter: the query parameter survives
	// the tool → client → router hop (the listing seam sees it).
	res, text := boardParityExtToolCall(t, cl, "board_read", map[string]any{"project_id": "prj-aaaa1111", "stage": "backlog"})
	assert.False(t, res.IsError, text)
	assert.Equal(t, 1, svc.calls["list_tasks"], "board_read reaches the listing seam")

	// board_get on epic and task kinds: the prefix dispatch resolves
	// through the real router (ownership through the project row).
	res, text = boardParityExtToolCall(t, cl, "board_get", map[string]any{"item_id": "epc-bbbb2222"})
	assert.False(t, res.IsError, "epic kind: %s", text)
	assert.Equal(t, 1, svc.calls["get_epic"])
	res, text = boardParityExtToolCall(t, cl, "board_get", map[string]any{"item_id": "tsk-cccc3333"})
	assert.False(t, res.IsError, "task kind: %s", text)
	assert.Equal(t, 1, svc.calls["get_task"])
}

// boardParityExtToolCall issues one raw tool call through the
// in-process MCP client.
func boardParityExtToolCall(t *testing.T, cl *mcpgoclient.Client, tool string, args map[string]any) (*mcpgo.CallToolResult, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := cl.CallTool(ctx, mcpgo.CallToolRequest{
		Params: mcpgo.CallToolParams{Name: tool, Arguments: args},
	})
	require.NoError(t, err, "tool %s", tool)
	text := ""
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(mcpgo.TextContent); ok {
			text = tc.Text
		}
	}
	return res, text
}

// boardParityExtCallReqID issues the action's representative tool call
// with a request_id woven into the arguments (the replay arm's driver).
func boardParityExtCallReqID(t *testing.T, cl *mcpgoclient.Client, action, requestID string) (*mcpgo.CallToolResult, string) {
	t.Helper()
	_, args, ok := boardParityExtToolArgs(action)
	require.True(t, ok, "no representative external tool call for action %q", action)
	if args == nil {
		args = map[string]any{}
	}
	args["request_id"] = requestID
	tool, _, _ := boardParityExtToolArgs(action)
	return boardParityExtToolCall(t, cl, tool, args)
}

func q(s string) string { return `"` + s + `"` }
