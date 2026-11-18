# Worklog: the wait lifecycle taught at claim time (tsk-petite-seasoning-wiggles)

## Context

The wait-lifecycle epic's TEACHING half, landed first for immediate
relief (the engine half lands separately): workers were ending turns at
checkpoints — pushing, reporting, stopping while actionable work
remained — and the nudge contract rode only the nudge text a worker
sees AFTER going quiet, never the charter that greets it at claim time.

Owner direction (the task charter): four teachings, carried by BOTH the
board worker charter template and the board-patterns docs —

1. **Turn-budget rule**: a turn is a unit of WORK, not progress.
   Turn-end is for completion, blockage, or a declared external wait —
   never a checkpoint or a "natural pause point".
2. **Wait-type decision table**: USER decision → `pending_input` with
   the question stated (never for events — legality verified in the
   stage machine: in_progress → pending_input sets the flag with
   `pending_origin='worker'`); EVENT (CI/review/PR) → keep the turn
   alive with a bounded watch (`gh pr checks --watch`, cap ~12 min) OR
   declare the event-wait with its condition; nothing → keep working.
3. **Nudge contract at claim time**: heartbeat ONCE, FIRST — then
   continue working / advance the stage / declare failure with reason;
   never speculate about heartbeats (they answer nudges only); stated
   in the charter, not just the nudge text.
4. **Report-as-you-go stays**; stopping-after-the-report while work
   remains is explicitly named as the anti-pattern.

## What changed

- `api/internal/app/board_spawner.go` (`boardCharterPrompt`): a shared
  TURN BUDGET & WAITS block renders on BOTH arms (worker and persona —
  a reviewer persona fails these rules exactly as an implementer does),
  placed between the arm trailer and the BOARD DIRECTIVES block. The
  per-arm nudge lines moved into it at full strength. The worker
  trailer keeps its pinned "Work the task to completion" opening (now
  stating the in_review forward exit); the persona arm keeps dropping
  the implementer loop.
- `api/internal/app/board_review_charter_test.go`: NEW
  `TestCharterPrompt_RendersWaitLifecycle` — nine fragments asserted on
  both arms (block title, turn-budget rule, never-a-checkpoint,
  pending_input-with-question, bounded watch, declaring the event-wait,
  stopping-after-the-report, heartbeat-ONCE-FIRST, answer-nudges-only)
  plus the persona arm still dropping the implementer loop. The stale
  byte-identity comment on the old trailer test reworded to the
  phrase-level pin that actually holds.
- `docs/automation/sprint-board-patterns/process-discipline.md` +
  byte-identical embedded copy: "Push and turn discipline" →
  "Turn budget, waits, and push discipline" — the old "push, verify the
  run started, END the turn" refined into the watch-or-declare pair
  (ending the turn on a declared event-wait remains legal; the bare
  checkpoint never was); "No sleep-polling" stays, with the capped
  watch named as the sanctioned waiting tool and sleep-then-check
  loops named as the banned shape; the claim-discipline nudge bullet
  states the full contract; the charter-writers note records that the
  template itself carries the core teaching.
- `cmd/workspace-agentd/pattern_docs_test.go`: board-catalog content
  pins +5 for the doc teachings.

Deliberately untouched: the nudge message text (`pkg/board` /
agentd eventbus copies — the charter now complements it, the task
says not-just-the-nudge-text, not instead-of), the lifecycle docs
(`lifecycle-happy-path.md` step 3 already defers to
process-discipline.md and its nudge line is the contract's subset),
and every engine surface (the engine half's lane).

## Coordination with the 2058 prelim-pattern lane

Adjacent PR, per the task's worker's-judgment latitude: the doc files
are disjoint (this lane touches process-discipline.md; 2058 adds
shape-prelim-story.md and edits the two shape docs), and the one
shared file (pattern_docs_test.go) is extended mid-list in the
process-discipline pin block — clear of 2058's end-of-list append, so
either merge order lands clean (a textual conflict would be trivial
either way). origin/main is merged before push per the binding
process; if 2058 merges first its pins and mine coexist by design.

## Verification

- `go test ./api/internal/app/ -run 'TestCharterPrompt'` — green
  (all charter pins incl. the new wait-lifecycle test).
- `go test ./cmd/workspace-agentd/ -run 'TestPatternDocs|TestEnsureEmbeddedPatternDir|TestInitFS'`
  — green (format/catalog/content/drift pins; the embedded copy
  verified byte-identical by the mirror semantics).
- `go build ./...`, `go vet` on the touched packages, `make fmt-check`
  — green.

## Union with the engine half (2104, 2026-11-18 main wave)

Tonight's wave merged `board_wait` (typed waits, migration 000060):
`waitOn='user'` IS the pending_input flag-op (typed dress:
wait_on/wait_reason; move_stage toStage=pending_input stays the alias,
implicit wait_on='user'); `waitOn='event'` parks the claim on a closed
declarative condition (`pr_merged(<pr>)` / `ci_green(<pr>)` /
`story_done(tsk-…)`) — the row stays in_progress with its claim, the
ladder does not freeze. The teaching union: the charter block's
wait-type table and both process-discipline copies now teach the typed
verb for BOTH rows (the legacy move_stage form named as the alias);
pins updated in the same change (charter test: `board_wait`,
`waitOn: "user"`, `waitOn: "event"`, `pr_merged(<pr>)`; catalog:
`board_wait`). Merge conflicts with the wave were CHANGELOG/COORDINATE
append-at-anchor (keep-both); no engine file overlap — 2104 touched
none of this PR's files.
