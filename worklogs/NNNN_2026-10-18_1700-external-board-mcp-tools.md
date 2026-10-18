# Worklog: #1700 — Board: external platform MCP server tools (board.*)

**Date:** 2026-10-18
**Status:** Complete (PR pending review)
**Branch:** `feat/1700-external-board-tools`
**Design:** 0067 (§4.3b external tool surface — normative; §6 parity matrix)

---

## Objective

Flip the external-MCP column of the board parity matrix live: implement
the customer-facing `board_*` tools on the platform MCP server
(`pkg/mcp`), per design 0067 §4.3b, with owner-scoped semantics (R7) on
the external surface, and update the #1728 parity harness
(`api/internal/handlers/board_parity_test.go`) — the source-scan pin +
matrix cells — in the same PR.

## What landed

### 1. The tools (`pkg/mcp/board_tools.go`, 18 tools)

`board_list_projects`, `board_get`, `board_read`, `board_create_project`,
`board_create_epic`, `board_create_task`, `board_update_item`,
`board_move_stage`, `board_start_task`, `board_epic_activate`,
`board_epic_pause`, `board_epic_archive`, `board_epic_unarchive`,
`board_project_archive`, `board_project_unarchive`, `board_cancel`,
`board_add_dependency`, `board_remove_dependency` — registered by
`AddBoardTools(srv, h)` from `NewServer` (the `AddWorkflowTools`
pattern). Deliberately absent, per §4.3b/§4.3c: `board_heartbeat`,
`board_report_throttle`, and any claim tool — claiming is engine-side
(claim-then-spawn, R9) and the in-flight worker actions live on the
agent registry only. **External actors hold NO claim tokens** — pinned
as a schema scan (no board_* tool carries a claim_token/claim parameter
or a user/org-scope parameter; R7: the API key IS the owner).

### 2. The architecture decision (recorded)

The tools ride the **owner's REST projection** through `pkg/mcp`'s
`HTTPClient` (`/api/v1/me/projects|epics|tasks`) — the Epic-64
workflow/trigger pattern, and the mechanical reading of §4.3b's "same
handlers": every tool call is an authenticated owner request against
the REST routes PR #1728 pinned. Consequences, accepted deliberately:

- **Guard outcomes, idempotent replay, and the read perimeter are
  identical by construction** — the same `BoardHandler` +
  `renderBoardError` render both columns.
- **Audit rows are stamped by the REST tier** (`surface=rest`):
  `pkg/board.SurfaceExternalMCP` stays reserved for a direct seam if
  one ever lands (the design's one-audit-trail/four-surfaces intent is
  met at the API ingress; the transport distinction external-MCP-vs-UI
  is not distinguishable at the service today — same class as Epic 64's
  workflow tools).
- **R7 owner scope is the route shape**: pinned live — every request
  the tools issue is a `/api/v1/me/*` path (never org, never internal
  seams).

### 3. Naming decisions (design left open — recorded)

- `board_create_*` → `board_create_project` / `board_create_epic` /
  `board_create_task` (REST vocabulary).
- `board_read` (list/search) → the **tasks-listing reader** (project +
  optional stage filter). Search/health are engine follow-ups on #1699
  (no ListEpics/search/health seams exist — same gaps the REST adapter
  documents); they join this tool when the seams land.
- The unified `board_get` / `board_update_item` / `board_cancel`
  dispatch on the **prj-/epc-/tsk- prefix**; bare UUIDs are refused
  with the prefix rule (the prefix selects the REST resource;
  short-id-first addressing is §4.3a's contract).
- `board_update_item` accepts only fields applying to the addressed
  kind — a field for another kind is an explicit error, never a silent
  drop.
- `board_cancel` on a `prj-` id explains projects are archived, not
  cancelled (the epic enum has no cancel value — R2).

### 4. OR4 anchor semantics at this tier

Tags ride **verbatim** — a well-formed `pr:N` anchor AND malformed ones
(`pr:0`, `pr:abc`, `pr:`) all pass through unmodified. The tool neither
parses nor validates anchors: extraction is the engine's
(conservative by design, `store.go prAnchor`), and a projection adding
validation the engine lacks would own business rules (§4.2 violation).
Pinned in `TestBoardTools_OR4AnchorPassthrough`; the schema description
documents `^pr:[1-9][0-9]*$` as the only consumed form.

### 5. The parity flip (`board_parity_test.go`)

- `pendingLane3Cell` deleted; every manager/read row's external cell is
  `live` with named tools (`extTools`); `heartbeat`/`report_throttle`
  and `claim_next` flip to declared `absent` (§6: asymmetries are
  pinned outcome rows).
- **Tool-set agreement, both directions**: every registered `board_*`
  tool belongs to ≥1 matrix row; every live external cell names really
  registered tools. A tool without a row (or a ghost tool in a row)
  reddens — the "CI fails on any action lacking full surface coverage"
  clause, mechanism preserved from the interlock.
- **Live arms**: each row's representative tool is driven through the
  REAL REST router fixture (`newBoardRESTRouter` + fake service) over
  the in-process MCP client (`mark3labs/mcp-go/client`) with
  `HTTPClient` pointed at the fixture — asserting allow outcomes, the
  SAME guard classes the REST column renders (deny arms:
  `API error {403,409}` + engine code verbatim), R7 owner scope (every
  issued path is a me-route), and foreign-row non-disclosure (plain 404
  body passes through; no owner identity, no suggestions).
- The guard-class parity case set on `move_stage` (done_guard /
  claim_token / manager_surface_only / authz) now runs against all
  three columns' renderers.

### 6. Red-first sequencing (the commit graph is the record)

1. `6e0b7802 test(mcp): #1700 red-first external board tool pins` —
   tool definitions + stub handlers + the full pin suite. Red: 122
   failing assertions across 6 behavior families, AND the #1728
   interlock (`TestBoardParity_ExternalMCPColumn` source-scan)
   reddened exactly as designed — board tools registered while cells
   were still `pendingLane3`.
2. `053d2e7b feat(mcp): ...` — real handlers + the matrix flip; both
   suites green.

## Verification (run in this lane)

- `go test ./pkg/mcp/ -count=1` — ok (full package incl. integration
  tests; the registration count pin 25 → 43).
- `go test ./api/internal/handlers/ -count=1` — ok (89s, full handler
  suite incl. the flipped parity matrix).
- `go vet ./pkg/mcp/ ./api/internal/handlers/` — clean;
  `gofmt`/`goimports`/`golangci-lint` (new-only) — clean via the
  pre-commit gates.

## Coordination notes

- No `pkg/board` edits (ListEpics/search/health engine follow-ups stay
  on #1699; nothing in the required tool set blocked on them).
- Surfaces disjoint from #1701 (agent-internal, `cmd/workspace-agentd`)
  and #1702 (frontend) by design; the only shared file is
  `board_parity_test.go`, whose agent-column tests are untouched.
