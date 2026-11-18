# Claim wait lifecycle — high-level design (owner-settled direction, 2026-11-18)

**Status:** Design (2026-11-18) — every product decision below is owner-settled in the epic
charter ("Claim wait lifecycle: typed waits, trigger-backed event resolution, honest terminals").
This document and its companion low-level design (`claim-wait-lifecycle-low-level-design.md`)
elaborate **HOW**, not whether. The three implementation stories on board project
`prj-orient-mint-stencils` (wait-typed-state = tsk-ebony-javelin-tramps,
wait-event-broker = tsk-pristine-henna-loses, wait-salvage-terminal =
tsk-retro-textile-picnics) are patched from these designs; their patched charters are binding.
The teaching half (wait-prompting-package = tsk-petite-seasoning-wiggles) is DONE and lands
independently.
**Companions:** `claim-lifecycle-design.md` (owns the lifecycle state VOCABULARY, the
epoch-evidence model, event-driven entry — this doc extends that vocabulary with TYPED waits),
`seamless-lease-design.md` (owns the ladder semantics — this doc does not touch them; §6 states
the one boundary), and GitHub issue **#2100** (the 2026-11-17 live verification this epic's
prelim performed — due-marking dead in prod and masked when firing; the wait lifecycle's
honesty depends on its fixes).
**Seam verification:** file/line anchors verified against `9f74d3020` (main, 2026-11-18).

---

## 1. The problem, in half a page

`pending_input` is a single boolean wait with no TYPE and no REASON. When a worker parks on
2026-11-17 holding the v0.44.17 cut for three PRs to merge, the row said only "flag on" —
nothing on the story, in board health, or on the wire said WHAT was needed to move forward;
the orchestrator discovered the buried wait by reading the worker's transcript and fixed it
with a manual protocol-correction message (the incident that spawned this epic). Workers
waiting on EVENTS (CI, a review, a PR merge) have no sanctioned state at all: they either
keep a bounded in-turn watch (burning the turn on `gh pr checks --watch`) or park invisibly
and eat the full lease-ladder cost — which PART A measured at **90.5 minutes park→nudge**
because #2065's due-marking is dead in the deployed topology (issue #2100 bug A) and would be
masked by movement-delta renewal even when it fires (#2100 bug B).

Meanwhile the terminals lie in the other direction: attempts-exhaustion writes
`stage='failed'` even when the claim leaves salvageable work behind (unpushed commits on the
lane branch, an open PR, uncommitted charter-relevant changes) — the #2064-shape salvage
machinery (S5's DEPOSED directive, pointer inheritance on every spawn, rearm) exists but the
exhaustion path bypasses it. A row that holds a perfectly good open PR reads as a failure.

The settled direction: make the wait **typed and visible**, make event-waits **resolvable by
the platform** (declarative conditions compiled to gate-scripted triggers), make exhaustion
**prefer salvage** when signals exist, and keep the **ladder governing death** throughout.

## 2. Typed wait — `wait_on` USER|EVENT, `wait_reason`, visible everywhere

### 2.1 The vocabulary (extends claim-lifecycle-design.md, no machine rewrite)

Per the 2026-11-15 audit ruling the lifecycle states are LEXICON, not a new column set; typed
waits follow the same discipline: **no new stage**. The storable stage set is untouched;
`pending_input` remains a wire literal flag-op (000050). What changes is that a wait now
carries two facts on the row:

- `wait_on` — `'user'` | `'event'` | NULL (no wait). NULL is today's default.
- `wait_reason` — a short human-readable string (cap 512 chars, the textguard seam), e.g.
  `"PR 2083 merge"` or `"owner decision: runtime bump scope"`.

The state machine the vocabulary names:

```
working ──declare──▶ waiting(user)  ──answer/advance──▶ working | terminal
working ──declare──▶ waiting(event) ──resolve/advance─▶ working | terminal
```

- **Entry moves are the worker's** (claim token required — a wait is a claim-holder's
  declaration, never a manager imposition; the manager pending_input exits of 000050 are
  unchanged and set `wait_on='user'` implicitly).
- **Any worker-anchored advance clears it**: a stage move, a verdict write, a token-bearing
  criteria write, and every claim mint (the mint's slot-reset list grows `wait_on`,
  `wait_reason`) — the same discipline that resets nudge/heartbeat evidence per episode.

### 2.2 USER waits: byte-identical semantics, better dress

`waiting(user)` IS today's `pending_input`: ladder frozen (the victims scan's
`NOT t.pending_input` predicate is untouched), surfaced, human-exited via the enumerated
manager exits. The only change is dress: the flag-op sets `wait_on='user'` and records
`wait_reason` when the worker states one; pre-existing flagged rows read as `wait_on='user'`
by COALESCE (no backfill migration). Every existing pending_input suite must stay green
without edit — the pins for "user-waits byte-identical".

### 2.3 EVENT waits: the new state

`waiting(event)` is a worker-anchored claim state: `pending_input` stays FALSE, the row stays
`in_progress`, and `wait_on='event'` + `wait_reason` (the declared condition prose) go on the
row. The worker's turn ends; the claim persists; the platform owns resolution (§3). The
ladder does NOT freeze (§6). Surfaces: GetTask wire (`waitOn`, `waitReason`, camelCase per
house precedent), `board_health` gains a `waiting` section (per-row wait facts beside the
lease facts — the "no indication what is needed to move forward" fix), and the board UI
renders a badge ("waiting on: PR 2083 merge") on the card.

### 2.4 The entry verb

One new worker verb, `board_wait` (MCP + REST + agentd strict-args parity, the house
three-surface pattern): `waitOn: 'user'|'event'`, `waitReason: string`, claim token required.
`waitOn='user'` is the flag-op's exact semantics (it sets `pending_input=true,
pending_origin='worker', wait_on='user'`). `waitOn='event'` additionally carries the
declarative `condition` (§3.1) — v1 refuses anything the DSL cannot parse, at the verb edge,
loudly. `move_stage(toStage=pending_input)` remains a legal user-wait entry (alias, implicit
`wait_on='user'`); a second concurrent event-wait on the same story refuses (cap: ONE active
event-wait per story).

## 3. Event resolution via triggers — the wait-broker

### 3.1 Declarative conditions, v1 primitives only

The worker declares a condition as DATA, never code:

```
condition := primitive "(" arg ")"
primitive := "pr_merged" | "ci_green" | "story_done"
arg       := <pr number> | <story short_id>
```

`pr_merged(number)` — GitHub PR `number` is merged. `ci_green(pr:<number>)` — that PR's head
SHA has a completed, non-failing check run. `story_done(<short_id>)` — the referenced board
story's stage is `done`. The grammar is closed at the verb edge (strict validation, refused
loudly with the accepted vocabulary — the textguard discipline); **no worker-authored gate
code can ever reach a trigger** (the broker compiles; the worker supplies only the parsed
primitive + argument).

### 3.2 The broker, beside the engine — pkg/board stays poor

The engine writes the wait row and records a `wait_declared` board event (condition payload
in the event, not a new engine dependency); it knows nothing about triggers, GitHub, or
resolution. A thin **wait-broker** at the app seam (`api/internal/app/`, the board_spawner /
board_liveness posture) owns the trigger lifecycle:

- **Materialize**: on a declared event-wait, compile the condition to a gate-scripted
  trigger (the #1812 external-polling worked pattern) owned and labeled by the broker
  (`wait-broker/<short_id>/<claim-token-hash>`) so lifecycle management can never mistake it
  for user automation — the dead-watcher litter class (2026-10-30 live-fire) stays closed.
- **Fire = resume**: the trigger is script-gated with **no prompt leg** (a script trigger
  with no prompt never calls the LLM — it is a pure condition sensor). The broker consumes
  the fire audit (`trigger_fires`), then sends the resume signal itself: a
  `wait-resolved` message to the claimant session ("your event-wait resolved: <condition> —
  resume work or advance the stage") and the wait-clear write path. Deterministic, no LLM
  leg, testable.
- **Cleanup**: on resolve, claim-end (mint/reset/deletes — the sites that clear `wait_on`),
  or wedge — the broker's pass finds rows whose wait is gone and deletes the orphan trigger.
  Orphan sweeps are idempotent; a delete failure retries next pass and surfaces loudly.
- **Caps and storm guards**: one active event-wait per story (verb edge); trigger poll
  cadence ≥ 2 min; the existing `autoDisableAfter` machinery bounds failure storms, and the
  broker treats a disabled/failed trigger as an unresolved wait it re-materializes once,
  then reports (never silent).
- **Cross-workspace honesty**: triggers are the workspace owner's (R7 — workers delegate as
  the owning user); the broker's label namespace keeps board-owned rows auditable and
  deletable without touching user automation.

### 3.3 What resolution races

The resume signal races the ladder (by design, §6): if the ladder wedges first, the wedge's
reset writes clear the wait and the broker's next pass deletes the orphan trigger; if the
resume lands first, the worker heartbeats/continues and the advance clears the wait. Both
orders converge; neither strand state.

## 4. Salvage-prefer terminal

At BOTH attempts-exhaustion sites (the dispatch claim's escalate arm and the manual
`task_start` escalate arm — the two places that write `stage='failed',
pending_origin='escalated'` today), the policy changes from "always fail" to **"depose when
there is something to deposit"**:

- **Salvage signals** (any present ⇒ salvage path): unpushed commits on the lane's worktree
  branch; an open PR from the lane; uncommitted charter-relevant worktree changes. The
  engine reads them through a POOR injectable seam (the liveness-source house pattern — Nil
  = inert = today's behavior), implemented at the app seam where the git/GitHub truth lives;
  pkg/board stays repo-blind.
- **Salvage present ⇒ depose-with-salvage instead of StageFailed**: record the pointer
  (branch/head/PR/state — the S5 machinery, verbatim), park the row backlog behind the
  failure backoff, attempts reset (the rearm posture), the stale assignee gets the DEPOSED
  directive, and the next claimant's charter inherits the pointer (the existing
  inherit-not-restart contract — it already rides every spawn).
- **Nothing present ⇒ unchanged**: the ordinary failed terminal, byte-identical.
- **Loop bound**: the salvage path requires facts that DIFFER from the row's existing
  pointer (new head, new PR, or first sighting) — a row whose signals are identical to its
  recorded pointer reads as "no NEW salvage" and fails terminally. One salvage round per
  real change, never an infinite depose loop.

This is a policy change at the exhaustion sites plus one seam; the depose/rearm/inherit
machinery is reused as-is.

## 5. Why these three together

The incident's two halves are one shape: the worker waited on events with no visible,
typed, platform-resolvable state — and the machinery that should have noticed the park
(due-marking) was itself dead (issue #2100). Typed waits make the wait HONEST on the row;
the broker makes EVENT waits resolve without a human in the loop; salvage-prefer makes the
terminal HONEST about leftover work. The ladder boundary (§6) keeps death detection
unbluffed by any of it.

## 6. The ladder still governs death (the boundary, stated once)

- **User-waits freeze the ladder** — unchanged: `pending_input` rows are excluded from the
  victims scan; no observation, no sentinel, no verdict while a human is awaited.
- **Event-waits do NOT freeze the ladder**: `wait_on='event'` is not `pending_input`; the
  row stays a full ladder citizen — nudged when the lease lapses, wedged honestly when the
  sentinel goes unanswered, reclaimed by the ordinary arms. An event-waiting worker that
  ignores its nudge dies like any other quiet claim; the trigger's resume signal is a
  courtesy that races the ladder, never a lease extension (it carries no renewal authority —
  the #2042 weak-channel rule).
- **No new renewal authority anywhere in this epic**: waits, reasons, and resume signals
  are row facts and messages. Renewal stays movement/heartbeat/busy per
  seamless-lease-design.md, untouched.

## 7. Dependency note — issue #2100 (from PART A of this epic's prelim)

The live verification (2026-11-17 incident, evidence in #2100) found the #2065 due-marking
DEAD in prod (native-sidecar registry split: `boardWorkerSessions.record()` writes the
sidecar's memory, its `/workspace` file append EROFS-fails silently, and the emitting half
hydrates from the stale file) and MASKED when it fires (the first-observation zero-baseline
and the parking turn's own frames read as `MovedSinceLastSweep`, renewing the just-marked
lease). The wait lifecycle does not functionally depend on those fixes (event-waits resolve
by trigger; user-waits freeze the ladder regardless), but its HONESTY story does — a
parked-for-a-user wait should still be one sweep from a nudge. The #2100 fixes (registry
under the dual-RW exchange subtree + loud degradation; cursor rebaseline at due-mark time)
file as their own stories in this epic's wake; the implementation stories below reference
the issue and must not regress its counters.
