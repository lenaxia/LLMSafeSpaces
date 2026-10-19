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

## Round 1 remediation (CHANGES_REQUESTED on PR #1737, reviewer-pre-specified)

The organic verdict (review run 39174263533, commit `95df9bf6`) blocked
on: the §6 per-cell replay + audit arms missing on the external column,
the `BoardRemoveDependency` `dependsOn` path-interpolation skipping
`validateID`, and the CHANGELOG overclaiming replay coverage. The first
review run (39172618124) had failed on reviewer-side model availability
("No deployments available" — infra, not the diff); the docs-parity CI
fix push (synchronize event) naturally re-fired it. Remediated in ONE
commit set:

1. **`validateID` on `dependsOn`** (`BoardRemoveDependency`) — the clean
   validation error before any request; pinned by
   `TestHTTPClient_BoardRemoveDependency_ValidatesPathIDs` (malformed
   `?`/`#`/`..`/whitespace/empty ids never issue HTTP). Also split
   `boardStartBody` off `boardCancelBody` (the reviewer's minor
   semantic-mismatch note).
2. **`TestBoardParity_ExternalMCPIdempotentReplay`** — the REST replay
   driver mirrored through the tool path (same `replayExceptions` skip
   list): two same-key calls render the original outcome verbatim, the
   key delivered on both.
3. **`TestBoardParity_ExternalMCPAuditRow`** — pins the recorded
   deviation itself: external-arriving mutating actions audit as
   (kind=user, owner identity, `SurfaceREST`, no claim token). The
   deviation is now durably tracked as issue #1740.
4. **`TestBoardParity_ExternalMCPReadVariants`** — `board_read`'s stage
   filter rides the wire (query encoding pinned at the client tier too,
   `TestHTTPClient_BoardListTasks_EncodesStageFilter`) and `board_get`
   resolves every kind through the real router.
5. **CHANGELOG** reworded to exactly what is pinned (replay + audit
   included post-remediation).

Advisory (not taken, recorded): `intArg` silently coerces wrong-typed
numerics to absent — the pre-existing `strArg` helper pattern; noted by
the reviewer as advisory-only.

## Round 2 remediation (CHANGES_REQUESTED on PR #1741, reviewer-pre-specified)

The v2 verdict (head `72b2d872`) blocked on three validated findings;
remediated in ONE commit set on `feat/1700-external-board-tools-v2`
(the v1 branch #1737 died of repeated synchronize event-loss — the
orchestrator's close-and-reopen recovery cherry-picked the full lineage
onto v2 over main-with-#1736; the parity-harness union preserved both
lanes' pins, verified locally before this round):

1. **Task tags-clear fidelity** (the real bug): `BoardTaskBody.Tags`
   became presence-aware (`*[]string` + `stringSlicePtrArg`) so an
   explicit `tags: []` survives to the wire ("tags":[] — the engine
   clears) while omission stays nil (keep) — the exact distinction the
   REST binding carries and r1's `stringSliceArg` collapsed. Pinned at
   three tiers: `TestBoardTools_TaskTagsClear` (mock), 
   `TestBoardTools_TaskTagsClearWire` (JSON serialization), and the
   descriptions now state the per-kind truth (task: omitted keeps / []
   clears; project/epic: written unconditionally, omitted CLEARS —
   pre-existing engine behavior, REST-identical, now honestly
   documented in tool + docs rows instead of the overpromising
   "omitted fields keep existing values").
2. **`board_move_stage` vocabulary**: split the shared six-stage enum —
   `boardMoveTargets` (the stage machine's legal manager targets:
   in_progress, in_review, done, pending_input, backlog) for the move
   schema+validation vs `boardStages` (ruling R2's full six) for
   `board_read`'s filter. `cancelled` is no longer advertised as a
   move target (permanently-illegal per stage.go's map — cancel is its
   own tool), and the description no longer implies a claim-only
   backlog→in_progress manager write. Single-sourced (the stage const +
   set builders; the r2 advisory about four-way duplication addressed).
   Pinned by `TestBoardTools_AdapterStageValidation` (both tools' valid
   + invalid branches, incl. the cancelled rejection).
3. **Worklog pointer**: board_parity_test.go's header comment now names
   this PR's own worklog file (NNNN_ sentinel) — "1135" belonged to
   #1701's lane.
4. Missing pins landed: `board_cancel` on `prj-` refusal
   (`TestBoardTools_CancelProjectRefused`).

## Coordination notes

- No `pkg/board` edits (ListEpics/search/health engine follow-ups stay
  on #1699; nothing in the required tool set blocked on them).
- Surfaces disjoint from #1701 (agent-internal, `cmd/workspace-agentd`)
  and #1702 (frontend) by design; the only shared file is
  `board_parity_test.go`, whose agent-column tests are untouched.
