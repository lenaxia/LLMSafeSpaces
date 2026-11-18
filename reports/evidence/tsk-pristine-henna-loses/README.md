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

## Verification logs (this sandbox)

- `checks.log` — build (exit 0), fmt-check (exit 0), goimports (clean),
  go vet incl. test compile (clean), full `api/internal/app` suite ok,
  `pkg/board` + `pkg/workflows` + `pkg/agent/opencode` ok,
  `api/internal/handlers` ok.
- `pins.log` — the 16 Wait-named broker pins, verbose, all PASS.
- `gate-pins.log` — the 8 gate-template/byte pins, verbose, all PASS.

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
