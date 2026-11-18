# Worklog: wait-event-broker — the declarative event-wait resolver (claim-wait lifecycle §3/§4)

Task `tsk-pristine-henna-loses` (wait-event-broker, board project
`prj-orient-mint-stencils`; patched by the prelim tsk-redolent-garden-rockets
from `reports/specs/claim-wait-lifecycle-{high,low}-level-design.md` — the
patched text governs). Branch `board-wait-event-broker` off `origin/main`
(583237c32) + merge of `board-wait-typed-state` (617741bcc — story 1, the
dependency: `wait_declared` events, `wait_on` rows, `parseWaitCondition`).
Issue #2100 referenced, counters untouched (its fixes file separately).

## What this story delivers (the patched charter's shape)

The **wait-broker** — a STATELESS reconciler at `api/internal/app/wait_broker.go`
(the board-reconciler posture: no in-memory scheduling state; a 30s pass over
Postgres + idempotent side effects) that turns declared event-waits into
platform-resolved facts:

1. **Read** `wait_declared` board events past a watermark (its own
   `board_sweeps` scope `wait-broker`; the RC2 pattern — deterministic ids
   make replays inert).
2. **Materialize** the compiled trigger per live event-wait (label select
   then create; a re-declaration UPDATEs the same-name trigger's script —
   the fresh seed reaches the sensor under the same ownership, never a
   second trigger).
3. **Orphan-clean** broker-owned triggers whose row wait is GONE (cleared /
   claim-end / wedge / terminal) + `wait_orphan_cleaned` event.
4. **Consume** delivered-but-unconsumed fires (trigger_fires audit) ⇒ the
   `wait-resolved` sentinel to the claimant session + `wait_resolved` event
   keyed on the fire id (the consume dedupe belt; the row clears on the
   worker's advance or the wedge reset — both orders converge, pinned).
5. **Advance** the watermark (pre-captured high — the reconciler's r1-C3
   never-skip discipline).

The BROKER consumes fires and sends the resume itself (deterministic, no LLM
leg); the trigger is a pure condition sensor — script-only, never a prompt.

## The patched changes vs the design baseline (all three landed)

- **No prompt leg**: the created trigger carries `Script` and an EMPTY
   `Prompt` (pinned: `TestWaitBroker_Materializes_NoPromptLeg_Caps`) — a
   script trigger with no prompt never calls the LLM (engine.go's #1812
   script-only arm records the fire `delivered`, `scriptOnly`).
- **The broker sends the resume** through the `deliverBoardTextV1` seam
   (fast-fail; a bounced delivery retries next pass — the fire stays
   unconsumed until a `wait_resolved` event lands; pinned both arms).
- **Repo identity resolves at declaration time and rides the
   `wait_declared` payload**: `board_wait` grows the `repo` argument
   ("owner/repo", closed grammar `validateWaitRepo`, required for
   pr_merged/ci_green, refused for story_done/user waits — the verb-edge
   pin `TestDeclareWait_RepoGrammar`). The broker compiles the GitHub
   templates from the payload's repo and never guesses; it refuses to
   materialize a pr-anchored condition whose payload carries none
   (unreachable through the verb, the belt is the grammar re-parse at
   consume time).
- **Disabled-trigger posture is re-materialize-once-then-degrade-loud**:
   a disabled/failing sensor is deleted+recreated ONCE (deterministic
   `wait_broker_rematerialized` event = the ONCE ledger), and the second
   failure deletes it + lands `wait_broker_degraded` (loud log + the
   board_health waiting section's `broker_degraded` line — events-driven
   SQL in `healthWaiting`) and leaves the wait to the ladder. Honest
   silence, never a stuck green badge. Pinned end-to-end
   (`TestWaitBroker_Degraded_RematerializeOnceThenLoud`).

## The trigger shape (and the one honest deviation, named)

- name `wait-broker-<short_id>-<claimTokenHash8>` — hash8 = first 8 hex of
  sha256(claimToken): the ownership label can never leak the in-flight
  guard. **Deviation from the design's literal `wait-broker/<short>/<hash>`**:
  the `triggers` table CHECK constraint pins names to
  `^[a-zA-Z0-9][a-zA-Z0-9 _-]{0,127}$` (no slashes — 000016). The canonical
  slash label `wait-broker/<short_id>/<hash8>` rides the DESCRIPTION; the
  hyphen form IS the name/ownership key. The `wait-broker-` prefix is the
  lifecycle namespace (`ListTriggersByPrefix`, the internal-only store seam
  this story adds).
- cron `*/2 * * * *` (poll ≥ 2 min — the storm guard, pinned on every
  create), `auto_disable_after` 5, owner = the row's user (R7), target
  workspace = the claimant's pod (the marker + pod env live there).
- **Cross-replica**: deterministic names + the (owner, name) unique
  constraint arbitrate — the create's 23505 arm is inert convergence; two
  passes converge (pinned). The resume sentinel is at-least-once like every
  board delivery (no renewal authority — §6).

## The templates (`api/internal/app/wait_broker_scripts.go`, byte-pinned)

Three in-tree templates, versioned (`wgv1` rides every rendered seed):
`pr_merged(N)` (proceed iff merged+merge_commit_sha; vars carry the SHA),
`ci_green(N)` (PR head SHA → check-runs; proceed iff total>0 and every
conclusion ∈ success|skipped|neutral; vars carry SHA+counts),
`story_done(short_id)` (the in-cluster board read over the pod identity —
`LLMSAFESPACE_API_URL` + the bootstrap SA token, the agentd MCP pattern;
workspace-bound like every board read). State dedupe = Pattern 3's marker
file at the FLAT per-episode `/exchange/wait-broker-<short>-<hash8>.marker`,
signature `seed|observed` (seed = template version + the declaration's event
id — a re-declared wait re-arms on an unchanged condition, pinned), PERSISTS
past proceed, quiet poller on transient errors. Litter posture stated in the
file: one flat ≤256 B marker per episode, platform-unlinkable by name.

NO worker-authored gate code anywhere: the worker surface supplies the
parsed primitive + argument + repo string (all closed grammars); the broker
renders from the three templates and re-parses the event payload's condition
before compiling (`board.ParseWaitCondition` — exported this story).

## Files

- `api/internal/app/wait_broker.go` (new) — the broker: waitFacts seam +
  pgxpool impl, TriggerPlane seam + workflows-store impl, the 5-step pass,
  the resume sentinel composer, degrade posture, Start loop.
- `api/internal/app/wait_broker_scripts.go` (new) — the three templates +
  name/marker/label/seed renderers (`waitBrokerGitHubAPI` package var = the
  imagefactory dispatchURL precedent, for the executable pins).
- `api/internal/app/wait_broker_test.go`, `wait_broker_scripts_test.go`
  (new) — the pass matrix + the byte/executable pins (26 tests).
- `pkg/board/wait.go` — the repo argument (validateWaitRepo +
  ConditionNeedsRepo + ParseWaitCondition export); the `wait_declared`
  payload grows repo/short_id/assignee/claim_token (the broker's compile
  inputs — its "workspace project facts" at declaration time).
- `pkg/workflows/store.go` — `ListTriggersByPrefix` (the broker's
  cross-owner namespace scan; internal-only).
- `pkg/board/reads.go` + `rows.go` — `healthWaiting` grows the
  `broker_degraded` line (events-driven; newer than the declaration).
- `api/internal/handlers/board_automation.go` (+test) — the seam + route
  carry `repo`.
- `cmd/workspace-agentd/{mcp_server,mcp_tools,strict_board_args}.go` — the
  `board_wait` tool schema/dispatch grow `repo` (admitted-args are
  schema-derived; dispatch passes the body through).
- `api/internal/app/app.go` — wiring: construct beside the board reconciler
  (same pool, same phrase-resolving sender, `LLMSAFESPACE_API_URL` as the
  story_done default belt) + Start in Run.

## Verification (this environment)

- `go build ./...` clean; `make fmt-check` + `make imports-check` clean;
  `go vet` clean on every touched package.
- `go test ./api/internal/app/` — full package GREEN (141.9s), including
  the 26 new pins (byte pins for all three templates; the executable
  fire-once pins under real python3 3.12: satisfied-state-fires-once,
  unsatisfied-then-satisfied, merged-fires-once with SHA vars,
  all-green-fires-once, re-declared-seed-refires, quiet-poller-no-marker).
- `go test ./pkg/board/ ./pkg/workflows/ ./pkg/agent/opencode/
  ./api/internal/handlers/` — GREEN. `cmd/workspace-agentd` board/strict/mcp
  suites GREEN; the package's `TestSupervisePiSubprocess_GracefulShutdown` +
  `_RepeatSignalExitsAtOnce` fail IN THIS SANDBOX **pre-existing** (verified
  identical on the unmodified story-1 worktree before my changes —
  subprocess signal supervision is environment-dependent; not this story's
  regression).
- The board-level integration suites (`//go:build integration`) SKIP here
  (no Postgres in this sandbox); the broker's facts SQL mirrors the
  reconciler's watermark/scan patterns it was cut from.

## Honest residue

- The `story_done` sensor reads the board through the WAITING workspace's
  pod identity — workspace-bound by ruling (#1747); a cross-workspace
  short_id reads not-found and skips quietly (the ladder governs). Stated
  in the template.
- Marker litter: one flat file per episode outlives its fire by design
  (it IS the dedupe state); the API process cannot sweep workspace
  filesystems. Bounded, named, platform-unlinkable.
- The remat/degrade ONCE ledger is per (trigger name) deterministic event
  id — a NEW claim (new token ⇒ new name) starts a fresh ledger, which is
  the correct episode semantics.
