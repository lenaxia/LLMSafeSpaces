# Worklog 1228 — #1948: refuse loud — task/plan creation into terminal epics

- **Lane**: lane-1948 (worker; orchestrator novel-nutmeg-verifies)
- **Branch**: `fix/1948-terminal-epic-create-guard`
- **Issue**: #1948 — tasks created into a COMPLETED epic sit silently unfed
- **Date**: 2026-10-25

## Validate-first (owner ruling): the silent-success path on current main

Confirmed on origin/main @ c5f68348d BEFORE any fix. Red-first pin file
`pkg/board/terminal_epic_create_integration_test.go` run against unmodified
main (local PG 15 via zonky portable binaries, migrations 000001→000049
applied, `TEST_DATABASE_URL` harness):

- create into COMPLETED epic → **nil error, row lands** (the live bug
  reproduced: epc-marshy-archer-waits shape from the issue)
- create into ARCHIVED epic → nil error, row lands
- create into ARCHIVED project's epic → nil error, row lands
- import_plan into ARCHIVED project → nil error, epic + tasks land

Root cause: `candidateSQL` gates claims on `e.status='active'`
(engine.go:60) and `eligibleProjectsSQL` on `p.status='active'`
(engine.go:30), but `CreateTask`/`ImportPlan` gated on NOTHING — terminal
targets create healthy-looking backlog rows the engine can never feed, with
no error and no audit signal. No pre-existing guard found in git log
(post-#1763 set swept: lifecycle transitions refuse at `epicLifecycle`, but
create never consulted epic status).

## The fix (refuse-only, per charter)

`pkg/board/service.go` — two guards after owner/manager checks, before any
write:

- `refuseTerminalEpicCreateTarget(e)`: completed → 409 `epic_state`
  "…reopen the epic first (epic_activate; completed→active)"; archived →
  409 `epic_state` "…unarchive the epic first". Error names the epic short
  id + its state.
- `refuseArchivedProjectCreateTarget(p)`: archived project → 409
  `project_state` (the project-level twin — same starvation shape via
  `eligibleProjectsSQL`; engine precedent: task_start's own "terminal
  storage stays closed").

`pkg/board/plan.go` — `ImportPlan` calls the project guard BEFORE the
dry-run branch and the transaction: a terminal target fails the WHOLE
transaction (guard precedes any write → nothing can strand).

**Decisions made explicit (charter required deciding archived):**

- **completed epic: REFUSE** (the live bug; no path back into the claimable
  set without an explicit reopen).
- **archived epic: REFUSE** (identical starvation shape; unarchive restores
  active, after which creation works — refusal names the remedy).
- **paused epic: GREEN, pinned explicitly** (pause is a scheduling lever;
  resume re-feeds — the trap a naive "not active → refuse" guard falls
  into; `board_epic_pause` documents resume re-feeding).
- **draft epic: GREEN** (create-then-activate is the documented flow;
  `task_start` likewise allows draft∪active).
- **archived project: REFUSE** (both create and import_plan — the whole
  project is outside the claimable set).

**import_plan / "terminal epic mid-payload" decomposition (honesty note):**
import_plan cannot target an existing epic — it mints a FRESH draft epic
inside its single transaction. A terminal TARGET EPIC is structurally
unreachable on this surface; the truthful terminal target is the archived
project. Pinned the invariant the charter actually wants: (a) archived
project → whole transaction refused before any write (epic/task counts
unchanged); (b) mid-transaction failure via the existing
`import_plan.effect` failpoint (armed between effect and marker, inside the
open tx) → epic + tasks + edges ALL rolled back — all-or-nothing holds,
nothing strands; (c) the minted epic lands `draft` (green pin into an
active project).

**Replay-first:** the guards sit before the eventWriter, so a replayed
requestId whose target went terminal AFTER the original write would have
rendered a guard error instead of 200-with-original — the house convention
(`replayOf`, "replay-first, then validate"; projectLifecycle's own comment)
now probes the caller key first in both CreateTask and ImportPlan; only
FRESH keys hit the guards.

**Refuse-only vs reopen-with-audit:** the stage-machine decision was
trivially separable and reopen-with-audit is a bigger semantic change
(auto-reactivating a human-closed epic on a bot's create) — NOT implemented,
flagged as follow-up in the PR body per charter.

**Audit events:** no new audit event kind is introduced BY DESIGN — a
refused create writes nothing (the refusal IS the signal; a
snapshot-bearing event for a row that never existed would be noise). This
composes with #1747's convention: events embed full final-state snapshots
of rows that EXIST.

## Pins (`pkg/board/terminal_epic_create_integration_test.go`, 9 tests)

1. `TestGuard_1948_CreateIntoCompletedEpicRefuses` — 409 epic_state, names
   epic + state; zero task rows; zero create_task events.
2. `TestGuard_1948_CreateIntoArchivedEpicRefuses` — 409 epic_state.
3. `TestGuard_1948_CreateIntoActiveEpicGreen` — backlog row lands.
4. `TestGuard_1948_CreateIntoPausedEpicGreen` — **the trap pin**.
5. `TestGuard_1948_CreateIntoArchivedProjectRefuses` — 409 project_state.
6. `TestGuard_1948_ImportPlanIntoArchivedProjectRefusesWhole` — whole
   transaction refused; epic/task counts unchanged (all-or-nothing from the
   guard).
7. `TestGuard_1948_ImportPlanMidPayloadFailureRollsBackWhole` — armed
   `import_plan.effect` failpoint; epic/tasks/edges all rolled back.
8. `TestGuard_1948_ImportPlanMintsDraftEpicGreen` — fresh epic lands draft.
9. `TestGuard_1948_RefusalNeverTurnsAReplayIntoAnError` — replayed
   requestId after the epic went terminal renders the original task; a
   fresh request still refuses.

All 4 refusal arms RED on unmodified main (silent success = the bug); all 9
GREEN after the fix.

## Fixture adaptations (intent preserved — required by the new guard)

- `pkg/board/remediation_test.go` `TestKill_TaskStartR3Pins` (b) loop:
  seeded tasks INTO paused/completed/archived epics before flipping status —
  now seeds draft → task → status flip. The pin still exercises
  task_start's own refusal per status (assertions unchanged).
- `pkg/board/service_kill_test.go` `TestKill_IdempotentReplayConverges`:
  its only task reaching done AUTO-COMPLETED the epic (epicCompletionCheck:
  ≥1 done, none non-done) — the exact #1948 shape — so the later create_task
  replay hit the new guard. Added a keeper backlog task holding the epic
  active; the pin stays about requestId idempotency (assertions unchanged).

## Surfaces

Guard lives in `pkg/board` (single source; "never handler-side" per
plan.go's header) → covers user REST, agentd manager REST, loopback MCP,
external platform MCP identically. 409 render pre-pinned
(`board_rest_test.go:815` guard_epic_state_409). MCP tool descriptions for
`board_create_task` + `board_import_plan` now document the guard
(agent-facing contract).

## Verification

- `go build ./pkg/... ./api/...` — clean
- `go vet` on touched packages — clean; `gofmt -l` — clean
- `go test -tags=integration ./pkg/board/` — **FULL suite green** (33.3s;
  includes the two adapted kill-tests + the replay pin)
- `go test ./pkg/board/... ./pkg/mcp/... ./api/internal/handlers/...` — green
- Adjacent observation (NOT changed here, follow-up material): ImportPlan's
  pre-existing dup-path returns a hollow result (on a replayed requestId
  `fn` never runs, yet the response points at the fresh never-inserted
  epic uuid with empty TaskKeys) — pre-dates this change; the replay probe
  preserves that behavior rather than altering it.
- Local PG note: `TestMintUniqueOwnerWideLadder` fails on ANY second run
  against one DB (fixed workspace uuid `1111…`; its cleanup truncates
  projects CASCADE but never `workspaces`) — pre-existing isolation flaw,
  unrelated to this change (no svc.CreateTask in it); CI runs on fresh DBs.
  Worked around locally with `TRUNCATE workspaces CASCADE` between runs.

## Review r1 (CHANGES_REQUESTED on 5ab2b1a6f — both findings fixed)

- **Finding 1 (concurrent terminalization)**: the guards were
  check-then-act OUTSIDE the write transaction — a concurrent
  `epicCompletionCheck`/archive/`ArchiveProject` between the read and
  the commit could still strand a row. Fixed with the TaskStart in-tx
  precedent: `CreateTask`'s fn re-reads `e.status, p.status` under
  `FOR UPDATE OF e, p` inside the write tx; `ImportPlan`'s fn re-reads
  the project status under `FOR UPDATE`; guard helpers split into
  pre-write (struct) + in-tx (status-string) forms so both layers
  render the identical error. Deterministic race pins arm new
  `create_task.recheck` / `import_plan.recheck` failpoints (firing
  between the two checks) to terminalize mid-flight — both refuse, both
  land nothing. CreateEpic's fn re-checks the project row too.
- **Finding 2 (CreateEpic seam)**: `create_epic` into an archived
  project now refuses (pre-guard + in-tx recheck + replay probe) — was
  the last unguarded creation seam of this class; MCP description
  updated; pinned (refusal + zero rows + zero events).
- **Missing pins** (all three): dry-run into an archived project →
  guard error (never a rendered approval moment); archived-epic refusal
  pin now asserts the no-row/no-event symmetry its completed twin had;
  the two concurrency pins above.

Pin count 9 → 13; full `pkg/board` integration suite green (33.4s) on
the r1 head; `pkg/mcp` green; vet/gofmt clean.

## Files

- `pkg/board/service.go` — the two guards (pre-write + in-tx status forms) + CreateTask/CreateEpic wiring (locked re-checks, replay probes)
- `pkg/board/plan.go` — ImportPlan wiring (pre-write + in-tx locked re-check, replay probe)
- `pkg/board/terminal_epic_create_integration_test.go` — NEW, 13 pins
- `pkg/board/remediation_test.go` — fixture adaptation (task_start pins)
- `pkg/board/service_kill_test.go` — fixture adaptation (keeper task)
- `pkg/mcp/board_tools.go` — tool description updates (create_task, create_epic, import_plan)
- `CHANGELOG.md` — [Unreleased] entry (directly under the header)
- `worklogs/1228_2026-10-25_1948-terminal-epic-create-guard.md` — this file
- `COORDINATE.md` — lane claim row
