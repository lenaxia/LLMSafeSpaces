# Claim wait lifecycle — low-level design (implementation contract)

**Status:** Design (2026-11-18) — the HOW companion to
`reports/specs/claim-wait-lifecycle-high-level-design.md` (the WHAT/WHY, owner-settled).
Anchors verified against `9f74d3020` (main, 2026-11-18). The three implementation stories
carry these exact anchors in their patched charters; where a story name appears in brackets
below, that story owns the change. Issue **#2100** (due-marking dead/masked in prod) is a
referenced dependency, not scope here.

---

## 1. Schema — ONE additive migration [wait-typed-state]

`000060_board_typed_wait.up.sql` (mirrored under `helm/migrations/`, the 000059 additive
pattern):

```sql
ALTER TABLE tasks ADD COLUMN wait_on TEXT
    CHECK (wait_on IS NULL OR wait_on IN ('user','event'));
ALTER TABLE tasks ADD COLUMN wait_reason TEXT;
```

- `wait_on` is NULL by default; `pending_input` remains the user-wait flag (000050) —
  `wait_on='user'` is dress on the flag, `wait_on='event'` is the new state and ALWAYS
  coexists with `pending_input=false`.
- No backfill: reads render `waitOn = COALESCE(wait_on, CASE WHEN pending_input THEN 'user' END)`.
- The claim mint's slot-reset statements (engine.go:274 claim path, engine.go:443
  task_start path) grow `wait_on=NULL, wait_reason=NULL` — every mint clears the wait
  (episode discipline).
- The `.down.sql` drops both columns (additive-only contract).

## 2. The verb and the wire [wait-typed-state]

### 2.1 `board_wait` (worker surface, claim token required)

`DeclareWait(ctx, actor, taskID, waitOn, waitReason, condition, requestID)` in a new
`pkg/board/wait.go`:

- **waitOn='user'**: identical write shape to the move_stage flag-op enter
  (`pending_input=true, pending_origin='worker', wait_on='user', wait_reason=?, updated_at`),
  same guard chain (stage machine: only in_progress; claim token; the 000050 exits
  unchanged). `move_stage(toStage=pending_input)` keeps working as the alias (implicit
  `wait_on='user'`; a `waitReason` may ride the same patch).
- **waitOn='event'**: validates `stage='in_progress'`, claim token, `NOT pending_input`
  (a user-wait converts: the flag clears, `wait_on='event'`, reason replaced), and **no
  live event-wait already on the row** (`wait_on='event'` present ⇒ guard
  `wait_already_declared` — cap ONE active event-wait per story). Writes
  `wait_on='event', wait_reason=?, pending_input=false`. The `condition` argument is
  validated HERE (§4.1 grammar) and recorded in the `wait_declared` event payload — the
  row stores only the human prose (`wait_reason`); the condition is event-carried so the
  broker owns its lifecycle without a row dependency.
- **waitReason** runs the textguard seam (cap 512, the placeholder-token scan).
- Audit: `recordEventTx` action `wait_declared` (or `wait_cleared` on conversion),
  deterministic id `wait:<taskID>:<claimToken>` (one declaration per episode; replays
  converge) — this is the event the broker consumes.

### 2.2 Clearing — "any worker-anchored advance clears it"

No new sweep: the existing forward-write sites add the clear (each already holds the token
guard): `moveStage` forward arms, `RecordVerdict`, the #1946 criteria grant path, and the
reconciler's reclaim/wedge/reset writes (the wedge statement engine-side and the reclaim
statement both grow `wait_on=NULL, wait_reason=NULL`; the pendingCoupling resume clears
`wait_reason` when it re-arms a user-wait's exit). User-wait ENTRY sites (worker verb,
manager exits, escalation writes that set `pending_input=true`) stamp `wait_on='user'`.

### 2.3 Wire + surfaces

- `GetTask` wire (reads.go + the REST/MCP adapters): `waitOn`, `waitReason` (camelCase,
  the `pendingInput` precedent); `waitingOn` renders the composed prose for UIs
  (`"PR 2083 merge"` / `"user: runtime bump scope"`).
- `board_health` gains a `waiting` section: per-claimed-row `{short_id, wait_on,
  wait_reason, since}` — beside `active_leases`, the same projection pass (reads.go's
  health assembly; `since` = the declaration event's timestamp).
- Frontend: the story card badge (`waiting on: <reason>`), the worker-tree tooltip, and
  the board_health panel row. Color posture: user-waits amber, event-waits blue —
  distinguishable at a glance (the incident's "nobody can see what the story needs").
- MCP tool description + agentd strict-args + origin-plugin stamp sets grow `board_wait`
  (parity-pinned, the `board_ack_epic_wrapup` pattern).

## 3. Ladder boundary — pinned, not re-derived [wait-typed-state]

The victims scan (reconciler.go:326 `AND NOT t.pending_input`) is **untouched**: event-wait
rows (`pending_input=false`) stay full ladder citizens by construction; user-wait rows stay
frozen by construction. The pins state it anyway (a future "helpful" predicate change is the
regression class): `TestPin_EventWait_StaysLadderCitizen` (event-waiting row with lapsed
lease ⇒ nudged), `TestPin_UserWait_FreezesLadder_Unchanged` (flagged row ⇒ never a victim).

## 4. The wait-broker [wait-event-broker]

### 4.1 Condition grammar (closed, validated at the verb edge)

```
condition := primitive "(" arg ")"
primitive := "pr_merged" | "ci_green" | "story_done"
arg       := 1..9 digit run (PR number) | tsk-[a-z-]+ (story short id)
```

`parseWaitCondition(string) (WaitCondition, error)` in `pkg/board/wait.go` — shared by the
verb edge (refuse unparseable loudly, the accepted vocabulary in the error) and the broker.
`WaitCondition{Primitive, Arg string}` is DATA; nothing executable ever crosses the worker
surface.

### 4.2 Broker shape (api/internal/app/wait_broker.go)

A stateless reconciler (the board reconciler posture — no in-memory scheduling state; one
Postgres pass + idempotent side effects):

```
type WaitBroker struct {
    db      *pgxpool.Pool          // board_events watermark read (the RC2 pattern)
    triggers TriggerPlane           // create/delete/fires — the platform trigger API
    sender  BoardTextSender        // the deliverBoardTextV1 seam (v1 route, fast-fail)
    clock   func() time.Time
}
Run(ctx) — cadence 30s (SweepInterval); one pass:
  1. read wait_declared events past the watermark (deterministic ids make replays inert)
  2. for each declared event-wait still live on its row (wait_on='event', claim token
     matches): ensure the compiled trigger exists (label select then create — idempotent)
  3. for each broker-owned trigger whose row wait is GONE (cleared / claim ended / wedged /
     story terminal): trigger delete, event `wait_orphan_cleaned`
  4. for each delivered-but-unconsumed fire (trigger_fires audit, delivered class, fire
     timestamp > the row's wait declaration): send the resume message to the claimant
     session, event `wait_resolved`; the row's wait clear happens on the worker's advance
     (or the wedge reset if the worker never answers — both orders converge)
  5. advance the watermark
```

- **Trigger naming/ownership**: name `wait-broker/<short_id>/<claimTokenHash8>`, topics
  `["board-wait"]`, cron `*/2 * * * *` (poll cadence ≥ 2 min, the storm guard).
- **No prompt leg**: `script` only — a script trigger with no prompt never calls the LLM;
  the trigger is a pure condition sensor and the broker is the only consumer of its fires.
- **Failure posture**: a disabled (`autoDisableAfter`) or repeatedly-failing trigger is
  re-materialized ONCE, then the broker records `wait_broker_degraded` (loud event + a
  health surface line) and leaves the wait to the ladder (honest silence, never a stuck
  green badge).
- **Cross-replica**: broker passes are guarded the same way as sweeps (the deterministic
  event ids + trigger name idempotence arbitrate; two replicas converge).

### 4.3 Compilation — primitive ⇒ gate script (declarative, broker-generated)

The #1812 external-polling shape: a python handler returning `{"proceed": bool, "vars": {}}`,
state deduped across fires via the trigger-vars channel. The broker renders the script from
the primitive — three templates, versioned in-tree (`api/internal/app/wait_broker_scripts.go`,
unit-pinned byte shapes):

- `pr_merged(N)`: GET `https://api.github.com/repos/<owner>/<repo>/pulls/N` — proceed iff
  `"merged": true` and `"merge_commit_sha"` non-null; vars carry the merge SHA.
- `ci_green(pr:N)`: GET the PR (head SHA), then
  `GET /repos/<owner>/<repo>/commits/<sha>/check-runs` — proceed iff total>0 and every
  check's conclusion is `success|skipped|neutral`; vars carry the SHA + counts.
- `story_done(<short_id>)`: the platform's own board read (in-cluster service URL, the
  broker's env) — proceed iff the story's stage is `done`.

State dedupe (each template): a fingerprint var (merge SHA / check-run total+sha / stage+updated_at)
compared against the trigger's stored last-seen var — proceed only on CHANGE to the
satisfied state, so a 2-min poll of a long-merged PR fires once, not every sweep. Repo
identity (`owner/repo`) resolves from the workspace's project facts at declaration time
(rides the `wait_declared` payload); the broker never guesses.

### 4.4 The resume message

`wait-resolved` sentinel (the lease-nudge composer pattern, `agentMessageSentinelLine`):
mode `wait-resolved`, taskId, condition prose; text: "your event-wait resolved: <reason> —
resume work, advance the stage, or re-declare a wait; if you no longer hold the claim,
ignore (the row moved on)". Delivered through the v1 seam (fast-fail); a bounced delivery
retries next broker pass (the fire stays unconsumed until a `wait_resolved` event lands).

## 5. Salvage-prefer terminal [wait-salvage-terminal]

### 5.1 The seam (engine-blind, app-wired)

```
// pkg/board/salvage.go
type SalvageSource interface {
    // SalvageFor returns the salvage signals for a claim's lane, or
    // nil when nothing salvageable exists. ~2s budget (the observe
    // seam's hygiene); failure = nil (no signals), loudly logged.
    SalvageFor(ctx context.Context, t *Task) *SalvageSignals
}
type SalvageSignals struct {
    Branch string; Head string; OpenPR string
    Uncommitted bool   // charter-relevant worktree changes exist
}
```

`WithSalvageSource(src)` Option; Nil = inert = today's behavior (pinned). The production
implementation (api/internal/app/board_salvage.go) resolves the lane's worktree (the
workspace repo + the branch recorded in the row's existing pointer when present, else the
charter's lane convention) and asks git/GitHub the three questions. **pkg/board imports
nothing new** — the poverty line holds.

### 5.2 The policy diff (BOTH exhaustion sites)

`claimOnce` (engine.go:388) and `TaskStart` (engine.go:231) — the escalate arms
(`attempts >= s.cfg.AttemptsCap`):

```
signals := s.salvage.SalvageFor(ctx, task)
if signals != nil && signals.DifferFrom(task.Salvage) {
    // salvage-prefer: depose-with-salvage, NOT StageFailed
    write the S5 SalvagePointer{Branch, Head, PR, State: "exhaustion-salvage ..."}
    stage='backlog' behind failureBackoff(cooldown), attempts=0 (the rearm posture),
    pending_origin=NULL, pending_input=false, wait cleared
    transition + audit event 'salvage_depose' {facts, attempts_at_cap: N}
    (the DEPOSED directive delivery rides the existing depose machinery's sender seam)
} else {
    today's failed terminal, byte-identical
}
```

- `DifferFrom` (the loop bound): identical {Branch, Head, OpenPR} to the row's recorded
  pointer ⇒ NOT new salvage ⇒ the ordinary terminal. New head, new PR, or first sighting ⇒
  salvage. One round per real change.
- **Rearm inheritance is already built**: every spawn rides `claimSalvage`
  (engine.go:533-539) — the reset attempts + recorded pointer compose into exactly
  "pointer + rearm path".
- The wedge path's own terminal is untouched (a wedge is a liveness verdict, not
  exhaustion; its rows keep today's semantics — scope discipline).

### 5.3 Pins

`TestPin_ExhaustionWithOpenPR_SalvageDeposes` (the #2064 shape: attempts at cap, open PR ⇒
backlog + pointer + attempts 0, never `stage='failed'`);
`TestPin_ExhaustionWithNothing_StaysFailed` (byte-identical today);
`TestPin_SalvageSamePointer_Terminal` (the loop bound);
`TestPin_NilSalvageSource_ByteIdentical` (inert contract).

## 6. Test map (red-first, per story)

- **wait-typed-state**: user-wait suites green WITHOUT edits (byte-identical pin);
  `board_wait` verb matrix (user/event/refusals: no token, second event-wait, bad grammar,
  placeholder reason); wire/health/UI field presence; mint-clears-wait; ladder pins (§3);
  the 000050 exits interplay (manager exits set `wait_on='user'`).
- **wait-event-broker**: grammar parser table; script-render byte pins (three templates);
  broker pass matrix (materialize / dedupe / orphan-clean on clear+claim-end+wedge /
  fire-consume → resume → `wait_resolved`); no-prompt-leg pin (the trigger payload carries
  script, never prompt); caps (one wait/story; cron ≥ 2min; disabled-trigger re-materialize
  once then degrade-loud); cross-replica double-pass convergence.
- **wait-salvage-terminal**: §5.3's four pins, plus the existing salvage/depose/rearm
  suites green (machinery reused, not changed).

## 7. Story ownership and ordering

1. **wait-typed-state** (tsk-ebony-javelin-tramps) — migration, verb, wire/health/UI,
   clear-on-advance, ladder pins. No broker dependency (event-waits declare and sit; the
   ladder treats them as ordinary claims — honest by §3 even before the broker lands).
2. **wait-event-broker** (tsk-pristine-henna-loses) — the broker + templates + resume
   message + cleanup; depends on 1 (reads `wait_on='event'` rows and the `wait_declared`
   event).
3. **wait-salvage-terminal** (tsk-retro-textile-picnics) — seam + policy diff + pins;
   independent of 1-2 (orderable any time after).
4. Referenced: #2100's two fixes (registry EROFS + due-mark rebaseline) as follow-up
   stories in this epic's wake — not blocked on, noted in every charter above.
