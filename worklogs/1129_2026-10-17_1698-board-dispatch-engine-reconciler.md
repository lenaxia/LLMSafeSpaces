# Worklog: #1698 — Task Board dispatch engine + reconciler (Lane 1: atomic claims, leases, DRR fairness, self-healing reconciler, red-first kill-tests)

**Date:** 2026-10-17
**Lane:** lenaxia/LLMSafeSpaces#1698 (implementation), design 0067 at PR #1703 head r13 (26052740), charter #1696
**Status:** Complete — PR open, orchestrator merges on APPROVED

---

## Objective

Lane 1 of the Task Board (design 0067 §7): migration 000036 + `pkg/board` (BoardService action layer, Postgres-arbitrated dispatch engine, convergence reconciler) + the internal automation seam + the R6 platform knobs — **no external surface** (REST/SDKs are #1699; MCP projections Lane 3; frontend Lane 4). Every behavioral pin red-first; the self-healing invariants pinned as real-Postgres kill-tests.

## What shipped (4 commits, test → impl ordering)

1. `test(board)` — migration 000036 + type skeleton + the full kill-test suite. **Verified RED first: 53 failures, all `board: not implemented`** (the stubs) — the pins fail for the right reason before any behavior exists.
2. `feat(board)` — the engine/reconciler implementation (below).
3. `feat(board)` — internal seam (`/internal/v1/automation/board/*` on the pod-identity resolve), R6 config knobs (helm → env → api/internal/config), app.go dark-run wiring (pool + service + reconciler + shutdown), chart values + deployment env, secrets-integration.yml CI wiring.
4. `docs` — this worklog + CHANGELOG.

## The kill-test evidence (the issue's three named proofs + §6's four invariants)

All against a real PostgreSQL (`-tags=integration`, `TEST_DATABASE_URL`; local PG 15.18 for the lane run; CI applies all 36 migrations then runs the suite race-enabled). Final tally: **34 suite subtests + 5 unit tests + 1 grep-level source pin — green, `-race -count=2` clean.**

- **Killing the reconciler mid-operation leaves no stuck/orphaned claims** (`TestKill_ReconcilerCrashMidOperationRecovers`): a fault at every named failpoint (rc1.pre/post, pending.pre, rc2.pre, rc3.pre, rc4.pre, watermark.pre) aborts exactly that phase's transaction; after each fault the row is either fully untouched (in_progress with its complete claim state) or fully reclaimed — never a token-less in_progress orphan, never a partial transition. The next clean sweep converges, and the pass after that writes zero new events (deterministic `reconciler:*` ids).
- **Concurrent claims on one task resolve atomically** (`TestKill_ConcurrentClaimsSingleWinner`, `TestKill_WorkerSlotsCapRace`): 8 racers × 1 ready task → exactly 1 winner, 1 spawn (R9), 1 attempts increment, 7 clean empties; a project at capacity under 6 concurrent surplus claimers never exceeds `worker_slots` (the p CTE's row lock serializes the count read).
- **Expired leases are reclaimed exactly once** (`TestKill_ExpiredLeasesReclaimedExactlyOnce`): 3 lapsed rows × 4 concurrent sweeps → exactly 3 reclaims total (SKIP LOCKED divides the work; the stage+lease predicate re-checked at write time arbitrates the rest), one transition and one event per lapse episode, attempts untouched; reclaim-then-reclaim increments on the next claim; the dead token then fails the guard (`TestKill_WorkerDiedMidLeaseTaskReclaims`, §6 invariant 1).
- **Invariant 2** (`TestKill_MergePRConvergesToDone`): merged PR + criteria met → done within one sweep with the PR link as `evidence_url` + reverse-edge unblock + RC4 check; the companion (merged, criteria unmet) lands `pending_input/drift_review` — external truth never bypasses R2's Done guard.
- **Invariant 3** (`TestKill_CancelMidFlightNeverResurrects`): cancel mid-flight aborts the worker (seam recorded), revokes token+assignee, stages dependents (`dependency_abandoned`, already-pending rows keep their origin); every reconciler path then writes zero; a direct SQL stage write is rejected by the `stage_update_guard` trigger.
- **Invariant 4** (`TestKill_IdempotentReplayConverges`): a fault between effect and event-write aborts both (nothing lands); replay with the same requestId returns 200-with-original; create/cancel replays converge with exactly one effect + one event per requestId.
- **Engine pins**: throttle provenance both ways (incrementing grant rolls its increment back, net 0; bounce-token re-claim never decrements — never undercounted, never negative); a pending_input row's held token cannot self-exit via the 429 path; heartbeat evidence gate (watermark-moved extends, silent does not — #1685); escalation withholds the grant at claim N+1 (attempts stay at N, no token/lease/spawn); caller-workspace binding (403/no-candidate both forms); lease-lifecycle totality incl. the RC1 NULL-lease belt; spawn-failure never revokes a claim (lapse heals); pending_input resume is the **no-mint preserve-case** (r11 fix) with the lease re-armed and BOTH token provenances preserved through the cycle (`TestKill_PendingInputResumeRearmsLease`).
- **Fairness pins** (`fairness_kill_test.go`): two-epic DRR starvation (3/3 across 6 grants), quantum-skip, two-level rollover (bounded: no reset without claimable candidates; dispatcher-level reset grants on the retry), priority-read-fresh-first, task + project-slot cooldown exclusion, **candidate-less least-served skip** at both the project level (the p CTE's EXISTS) and the epic-anchor level (the ORDER BY key) — the r5/r6 fixtures the doc calls out as uncatchable by the naive shapes.
- **Stage machine** (`TestKill_StageMachineGuards` + six-provenance exits): exactly the enumerated transitions; the in_review→in_progress bounce re-claims (fresh token+lease, flag=false, identity-keyed); the pending_input exit is manager-surface-only, resets attempts (the ONE write that does), and branches on **identity presence, never token presence** — escalated/backlog-lapsed take the backlog branch (dead worker never re-nudged), live-claim/in_review/drift-review take the re-claim+nudge branch (nudge seam, not a fresh spawn — R9); Done = ALL criteria met + evidence.
- **Dependencies**: cycle rejection at write (409 with did-you-mean; the trigger's DFS walks the **depends-on forward closure** — the a→c cycle against existing b→a, c→b is caught), self-edge, cross-project, duplicate-inert; N-ary ALL-Done semantics (one of two done → still blocked); cancelled-dependency flip (skipping already-pending rows); RC3 recompute belt repairs corrupted `is_blocked` and never writes terminal rows.
- **RC4**: event-fed completion (all-terminal + ≥1 done → completed; stable), all-cancelled epics never complete, task-less epics never complete, completed→active re-open stays active (exempt), done-while-draft → activate → completes immediately (r13 pin).
- **Source pins**: no `user_id` read in engine.go/reconciler.go (§4.7 ownership-agnostic audit), no stored progress/story-points column on epics (R2 arity pin).

## Design deltas found under the reconcile clause (r13 → implementation)

The r13 head (26052740) was the implementation target; R6-R9 + the r11 no-mint resume semantics all conformed. Deltas, all additive/interpretive, flagged for the design lane:

1. **`board_sweeps` watermark table** — §4.5 requires an incremental `board_events.created_at` watermark but the §4.1 schema (four tables + event log) has no home for it. Added the smallest additive table (scope PK + cursor). Seeded at the epoch so the first sweep replays history once (bounded by the in_review/done predicates).
2. **0066 mint/suggest is a local seam** — §4.1 says "imports its mint + suggest library" from #1693; 0066's implementation (issue #1691) hasn't landed a shared package in main yet. `pkg/board/ids.go` implements the identical shape (crypto/rand [0-9a-z]×8, check-and-retry, case-insensitive, top-3 ≤2 edits, 404-class-only) and collapses into an import when the library lands.
3. **Manager move_stage on in-flight rows snaps back** — §4.2's clause "stage/lease mutations on an in-flight task by caller surfaces require the token held by the caller" implemented verbatim: manager drags on `in_progress` rows fail the claim-token guard (the optimistic-UI snap-back case the doc names); the pending_input exit and cancel are the two named overrides. The transition-legality table reports its own guard class before the token guard (an illegal drag is a stage error, not a token error).
4. **Project-level rollover (a) is a belt** — with the p CTE locking the project row first, the "locked p, candidate raced away" window is only reachable through the SKIP LOCKED race the doc describes; the pin asserts the observable bounded contract instead (no reset without claimable candidates — never a loop).
5. **`in_progress→pending_input` via manager move_stage sets origin `worker`** — the four-origin taxonomy has no manager origin; a manager staging a claimed row mid-question is the worker-origin class (the auto-stage writes the same origin).

## Verification

- `go test -count=1 ./...` — 100 packages, exit 0.
- `TEST_DATABASE_URL=… go test -tags=integration -race -count=2 ./pkg/board/` — green.
- Migration round-trip on a scratch DB: all 36 ups → 64 tables; all 36 downs in reverse → 0 tables (the migration-safety contract, run manually — docker absent in this env).
- Pre-commit gates green (repolint incl. sequence + chart mirror, gofmt/goimports, golangci-lint new-only: 0 issues).

## Environment notes

- Local PG 15 server for the kill-tests (extracted debs into /tmp — no docker/sudo in this sandbox); CI gets the same coverage via secrets-integration.yml.
- goimports hook needs `GOBIN=/tmp/opencode/bin` + PATH (read-only mise GOBIN — the known env note).

---

## r1 remediation (2026-10-17, review on 8812108a — CHANGES_REQUESTED)

Per-finding fixes, one remediation commit set on the same branch:

- **S1 (cross-tenant seam)** — the agent surface now carries the resolved
  workspace on the actor (`Actor.WorkspaceID`, stamped by
  `board_automation.go`'s `boardActor`); reads resolve through a new
  workspace-scoped path (`resolveTaskInWorkspace`/`GetTaskInWorkspace`:
  JOIN on `projects.target_workspace_id` — a foreign tenant's task is a
  404 that cannot even be addressed); did-you-mean candidates REQUIRE a
  scope (project or workspace; the unscoped suggestion list is gone);
  heartbeat/throttle on the agent surface resolve scoped. Pinned at both
  layers: handler tests (actor stamping, scoped reader, identity
  mismatch, error classes) + integration (leaked-token leaker gets 404,
  row untouched; foreign read 404 with empty suggestions).
- **S2 (identifier disclosure)** — cross-project dependency rejection
  pre-checked in Go with a clean, UUID-free detail; the DB trigger's
  RAISE mapped through sanitized details only.
- **S3 (kill-switch)** — `dig "board" "enabled" true .Values` (the
  chart's sanctioned false-honoring pattern, same as
  relayOnlyKeyDelivery.api.enabled): board.enabled=false now renders
  "false".
- **C1 (RemoveDependency no-op)** — the target resolves like every other
  addressing surface (short-ID-first); red-first pin covers short-ID,
  UUID, and the pending_input abandoned-edge resolution path.
- **C2 (dispatch drive)** — `dispatchDrive`: the sweep's backstop phase.
  With a spawn seam installed, each sweep feeds claimable work through
  the engine (per-workspace ClaimNext loops; the drive's pre-scan
  deliberately does NOT pre-filter DRR quota — ClaimNext's own rollover
  owns that). Production wiring: `board_spawner.go` (the workflow
  engine's agentd node-execution class — activator + HTTPAgentExecutor;
  charter prompt = description + criteria + token + addressing;
  `session: "new"` per R9) + `WithAsyncSpawn()` (pod activation and
  charter dispatch outlive the sweep; the guarded stamp lands when the
  session id exists; a lost spawn heals by lease lapse). Pin:
  reclaimed-by-RC1 rows re-dispatch in the SAME sweep; escalation
  composes (drive escalates and stops granting at cap).
- **C3 (watermark skip)** — the sweep's high-water is captured BEFORE any
  phase; the watermark advances to that pre-captured max (concurrent
  commits above it are re-scanned next sweep). Pin: a mid-sweep injected
  event (created_at above the capture) stays above the advanced
  watermark, and a late-committing in_review anchor converges on the
  next sweep.
- **C4 (assignee stamping)** — conformed to the design: the claim leaves
  `assignee_session` NULL ("the fresh session's identity, minted at
  spawn", §4.4/R9); the spawn stamps identity AND re-arms the lease from
  stamp time (spawn latency never burns the worker's window); the
  `tasks_claim_needs_assignee` CHECK (mine, undeclared) is dropped from
  migration 000036 — the interim state is representable. The synthetic
  `worker-<ws>`/`manual:<user>` ids are gone (the pending-coupling probe
  only ever sees real spawned identities now). DECLARED delta resolved.
- **C5 minors** — `SweepResult.Reclaims` counts applied writes not
  scanned victims; `ClaimNext` returns `ErrNoCandidate` (the declared
  contract; the empty-&Claim{} trap for Lane 2/3 is gone);
  `TaskStart` re-checks stage AND is_blocked under the row lock; the
  re-claim nudge fires only AFTER commit (a rolled-back mint can never
  strand a worker holding a dead token).
- **Tests (verdict's missing levels)** — handler-level seam suite
  (4 tests: actor stamping, workspace-scoped read + suggestions,
  error-class mapping, identity mismatch); TaskStart r3 pins (all-draft
  dispatch, paused/completed/archived + archived-project refusals, DRR
  bypass with vft/deficit advancing); RemoveDependency (both id forms +
  the resolution path); dispatch-drive pin; watermark no-skip pin;
  in-flight workspace-binding pin.
- **CI reds** — worklog renamed to the bot-assigned `1127_`; the
  `"cancel" + "led"` composition makes `misspell -w` a no-op on the stage
  literal (verified locally with the exact fixer); the
  usagestream short-test failure did not reproduce locally (5× + full
  package green) — treated as the CI-load flake its family's history
  (dbc1eab1) suggests; re-validated on the push.

## Reconcile against the MERGED doc (461cf3ab, post-r1)

The merged text changed more than prose for this lane; conformed in the
same remediation set:

- **Re-claim → re-spawn** (§4.1/§4.4): the bounce, RC2 re-entry, and the
  pending_input exit re-claim now deliver by SPAWNING the fresh worker
  that receives the token in its charter — the recorded identity is
  cleared at mint (`assignee_session=NULL`, the interim shape), the
  superseded live session is aborted on the exit path (interrupt seam),
  and there are no nudges to dead sessions anywhere (the nudger seam is
  deleted). The six-provenance pin now asserts spawn+stamp on the
  re-claim branch.
- **Interim state + guarded spawn-stamp** (§4.4): post-claim rows are
  in_progress + token + lease + assignee NULL; the stamp re-arms the
  lease from stamp time and carries the doc's exact guard
  (`stage='in_progress' AND claim_token=:token`) — a race-loser stamp
  now ABORTS the just-spawned dead-charter session. Pinned
  (TestKill_InterimStateClaimShape).
- **Engine-side workspace binding** (§4.4): the dispatch drive binds the
  claim's workspace FROM THE PROJECT ROW; `task_start` takes no
  caller-supplied identity/workspace on any surface (owner-surface only;
  the agent registry never sees it). The seam's per-pod identity check
  covers worker calls structurally.
- **Dispatch drive, event half** (§4.4): create_task, epic_activate/
  unarchive, →done unblocks, remove_dependency, and throttle requeue poke
  a drive queue; the app's consumer drives the affected project via
  `DriveProject`; the reconciler sweep is the backstop. Pinned
  (TestKill_EventDrivenDispatchDrive + TestKill_DispatchDriveResweepFeedsWork).
- **OR4 intersection (flagged per orchestrator)**: RC2's anchor extraction
  implements the `pr:N` explicit-tag form only — the conservative subset
  of OR4's proposed "either, both conservative". The structured-field
  variant is additive when the ruling lands; no free-text guessing exists.

The five r13-flagged deltas were re-verified against the merged text:
(1) board_sweeps watermark — text unchanged, delta stands; (2) the 0066
mint/suggest library is still absent from main (design PR merged, the
#1691 implementation has not landed) — the local seam stands with the
same collapse-into-import plan; (3) the in-flight token/workspace clause
— the merged text's structural rejection wording is what the S1 fix
implements (per-pod identity check + scoped reads); (4) the
project-level rollover belt — text unchanged; (5) the pending-origin
taxonomy note — the four-origin set is unchanged.

## r3 (2026-10-17): the S3 survivor + rebase onto main

The r2 review verified every r1 item remediated except one: the helm
kill-switch fix was ABSENT from the tree (an edit lost in the remediation
session — the r1 commit message described a dig fix that never landed in
the file). Fixed for real this time, red-first:

- **Render pin first** (`helm/board_killswitch_chart_test.go`): three
  tests — explicit `board.enabled: false` renders `"false"`, chart
  defaults render untouched, knob overrides thread. Verified TRUE RED on
  the broken head (`default true` coerced false → rendered `"true"`),
  then green after the fix.
- **The fix**: `dig "enabled" true .Values.board` (one-level dig into the
  submap — root `.Values` is chartutil.Values, which dig's map assertion
  rejects; the relayOnlyKeyDelivery.api.enabled precedent digs submaps
  too), applied to all six board env knobs.

Rebased onto main (f17cd11b, post design-merge 461cf3ab): CHANGELOG
conflict resolved (my Added block into the fresh [Unreleased]; the
0.38.0 release section preserved — a first-pass resolution had dropped
it, caught by the chart appVersion drift pin), and the worklog renumbered
1127→1129 (main assigned 1127 to #1716 and 1128 to the design lane while
this branch was out).
