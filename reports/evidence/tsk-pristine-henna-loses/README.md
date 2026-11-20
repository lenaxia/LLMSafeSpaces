# tsk-pristine-henna-loses — wait-event-broker evidence (2026-11-18)

**PR #2109** (`board-wait-event-broker` → `main`; foundation PR #2104 MERGED — this branch merges main at aa4869bad, plus the #2106 listing-latency and audit merges).

Branch `board-wait-event-broker` off `origin/main` (583237c32) + merge of
`board-wait-typed-state` (617741bcc, story 1 — the dependency). Worklog:
`worklogs/1303_2026-11-18_wait-event-broker.md`. Governing designs:
`reports/specs/claim-wait-lifecycle-{high,low}-level-design.md` §3/§4 (the
prelim's patched charters — patched text governs; PR #2101). Issue #2100
referenced per charter, counters untouched, fixes file separately.

## What landed (the charter's five steps + patched changes)

- `api/internal/app/wait_broker.go` — the stateless reconciler (30s pass):
  watermark read (board_sweeps scope `wait-broker`, the RC2 pattern) →
  materialize (label-select-then-create; re-declaration UPDATEs the
  same-name trigger's script) → orphan-clean (cleared/claim-end/wedge/
  terminal + `wait_orphan_cleaned`) → fire-consume (`wait-resolved`
  sentinel via deliverBoardTextV1 + `wait_resolved` keyed on the fire id;
  bounced delivery retries next pass) → watermark advance. Degrade
  posture: re-materialize-once (`wait_broker_rematerialized` = the ONCE
  ledger) then `wait_broker_degraded` (loud + the board_health waiting
  section's `broker_degraded` line) — the wait falls to the ladder.
- `api/internal/app/wait_broker_scripts.go` — the three in-tree
  byte-pinned templates (pr_merged / ci_green / story_done), Pattern-3
  marker dedupe (flat `/exchange/wait-broker-<short>-<hash8>.marker`,
  seed = template version + declaration event id, persists past proceed),
  quiet poller. **No worker-authored gate code anywhere** — declarative
  input only; the broker re-parses the event payload's condition before
  compiling (`board.ParseWaitCondition`).
- The patched repo-identity change: `board_wait` grows the `repo` argument
  (closed grammar, required for pr-anchored conditions) riding the
  `wait_declared` payload — the broker compiles from it and never guesses.
- Trigger shape: name `wait-broker-<short_id>-<claimTokenHash8>` (the DB's
  name CHECK forbids slashes — the canonical slash label rides the
  description; documented in the worklog), cron `*/2` (>= 2 min storm
  guard), `auto_disable_after` 5, owner = the row's user (R7), routine
  target = the claimant's workspace, **Prompt EMPTY** (the no-prompt-leg
  pin: a script trigger with no prompt never calls the LLM).
- Cross-replica: deterministic names + the (owner, name) unique constraint
  arbitrate; the create's 23505 arm is inert convergence; double-pass
  convergence pinned.

## Acceptance criteria → evidence map

1. **"Event-wait with declarative condition auto-creates a gate-scripted
   trigger; fire resumes the claimant; cleanup verified on
   resolve/claim-end/wedge"** — `TestWaitBroker_Materializes_NoPromptLeg_Caps`
   (script-only, no prompt, cron */2, owner/target/auto-disable),
   `TestWaitBroker_FireConsumed_ResumeAndEvent` (sentinel to claimant +
   `wait_resolved` keyed on fire id + no re-send),
   `TestWaitBroker_OrphanClean_WaitCleared` / `_ClaimEndAndTerminal`
   (claim_ended / terminal / wedge_reset_cleared arms, deterministic
   event ids, idempotent re-pass), `TestWaitBroker_FireBounced_Retries`
   (the fire stays unconsumed until the event lands).
2. **"Declarative-only conditions; broker-owned trigger lifecycle; caps
   and storm guards pinned"** — `TestRenderWaitGateScript_RefusesUnknownPrimitive`
   + the grammar re-parse at consume; the `wait-broker-` prefix namespace
   (`ListTriggersByPrefix`, internal-only store seam);
   `TestWaitBroker_ReDeclaration_UpdatesScript` (ONE trigger — the
   one-wait/story cap's broker half); the cron >= 2min pin on every
   create; `TestWaitBroker_Degraded_RematerializeOnceThenLoud` (ONCE then
   loud, sensor deleted, wait stays for the ladder);
   `TestGateTemplate_*` executable pins under real python3
   (satisfied-state-fires-once incl. the marker persisting past proceed,
   unsatisfied-then-satisfied transition, merged-fires-once with SHA
   vars, ci_green all-green + zero-checks refusals, re-declared-seed
   re-fires, quiet-poller-no-marker) + byte pins
   (`TestWaitGateScriptRender_BytePins` / `_Reproducible`).
3. **"AI reviewer APPROVED and CI green"** — `make fmt-check` clean,
   build/vet/imports clean, all affected suites green (below); the
   review rides this evidence at stage in_review.

## Review round r0 (CHANGES_REQUESTED) — findings addressed

- **F1 integration compile** — the PR-2104-merged `wait_integration_test.go`
  carried 10 old-arity `DeclareWait` calls; fixed (`repo` arg per intent:
  `testRepo` for pr-anchored, `""` for user arms/refusals).
  `go vet -tags integration ./pkg/board/` exit 0 (in checks.log).
- **F2 synthetic id** — `createTrigger` RETURNS the built row; the
  materializing pass carries the STORE-ISSUED uuid into its phases
  (`trigger_fires.trigger_id` is uuid NOT NULL). The fake no longer
  rewrites ids and records every `ListFires` key; new pin
  `TestWaitBroker_MaterializedRow_KeepsStoreIssuedID`.
- **F3 evidence truth** — checks.log re-stamped with TRUE exit codes
  (PIPESTATUS-captured build, fmt, imports, vet, vet -tags integration,
  suites). Honest note: the integration suite COMPILES under the tag and
  SKIPS at pool.Ping in this no-PG sandbox; CI executes it (the
  pkg/secrets-integration job's "Run -tags=integration kill-tests for
  pkg/board" step).
- **F4 tautology** — the event-action vocabulary is SHARED CONSTANTS
  (`EventActionWait*` at the recordEvent call sites;
  `board.HealthWaitBrokerDegradedAction` spliced INTO the hoisted
  `healthWaitingQuery`). Pins: app-side cross-package equality +
  board-side `TestHealthWaitingQuery_Vocabulary` — renames redden.
- **F5 permanent 4xx** — the templates catch `urllib.error.HTTPError`
  first: 4xx RE-RAISES (fail-closed into `consecutive_failures` → the
  remat/degrade ladder); 5xx/network stay the quiet poller. Executable
  pin `TestGateTemplate_PermanentHTTPError_Fails` (401/403/404 →
  non-zero exit, no marker).

## The parked-PR CI reds (diagnosed, 2026-11-19)

The Lint (44398375265) and pkg/secrets-integration (44398375216) reds
both ran on the PRE-r0 head `0b02524153d6` — the same F1 compile break
(the secrets job's failing step is "Run -tags=integration kill-tests
for pkg/board", which could not compile the arity-broken suite). The
r0 fixes sat unpushed through the parking window; this head carries
them and re-triggers both jobs.

## Verification logs (this sandbox, post-r0 + origin/main merge at 38eb2fa75)

- `checks.log` — every gate with its TRUE exit code: build 0, fmt-check 0,
  goimports 0, vet 0, **vet -tags integration 0**, unit suites ok (app,
  board, workflows, handlers), and the integration-tagged run (compiles;
  skips without PG — CI executes it).
- `pins.log` — the Wait-named broker pins, verbose, all PASS.
- `gate-pins.log` — the 9 gate-template/byte pins (incl.
  `TestGateTemplate_PermanentHTTPError_Fails`), verbose, all PASS.

## Honest environment notes

- `cmd/workspace-agentd`: `TestSupervisePiSubprocess_GracefulShutdown` and
  `TestSupervisePiSubprocess_RepeatSignalExitsAtOnce` fail IN THIS SANDBOX
  **pre-existing** — verified identical on the unmodified story-1 worktree
  before any change of mine (subprocess signal supervision is
  environment-dependent). The package's Board/Strict/mcp suites pass.
- The board-level integration suites (`//go:build integration`) skip here
  (no Postgres); the broker's facts SQL mirrors the reconciler
  watermark/scan patterns it was cut from, and the pass logic is fully
  pinned on fakes.
- `make imports-check` itself cannot run in this sandbox (it installs
  goimports into the read-only mise tree); verified equivalent with
  GOBIN-pointed goimports — clean.
