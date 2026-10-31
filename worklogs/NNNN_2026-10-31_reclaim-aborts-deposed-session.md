# Lease reclaim aborts the deposed session (owner ruling 2026-10-30, S8)

Branch: `s8-reclaim-abort` (worktree `/tmp/opencode/s8-reclaim-abort`, off
`origin/main` @ `a345b8101`). Task `tsk-salted-poncho-snorts`, epic
"Liveness and observation: every claim observable, every lapse visible".

## §1 The ruling and the incident

Owner ruling (2026-10-30, verbatim): "When a lease is reclaimed, an abort
should be sent to the session just to ensure it does stop."

The incident's shape: healthy workers mid-marathon turns, lapsed at exactly
45:00 while still generating. Lease expiry freed the ROW (the claim drops,
the row re-enters the pool) but never told the old SESSION to stop in any
way it cannot ignore. The old worker kept running after reclaim — two
workers on one row, wasted quota, racing pushes. The 409 claim_token the
old worker would get only appears on its NEXT board WRITE; a session
mid-generation may not see it for a very long time.

The abort primitive already existed (`abort_session` semantics: cut the
in-flight generation, history preserved) and the engine already held the
seam (`WorkerAborter` — delete's in-flight abort, the spawn-stamp guard).
It was simply not wired to reclaim.

## §2 What landed (pkg/board/reconciler.go)

Every reconciler pass that definitively ENDS a claim now aborts the
deposed holder and audit-logs the deposal:

- **RC1 wall-clock reclaim** (dead-evidence, no-seam, delivery-failure
  fallback — the incident's path): abort after the reclaim transaction
  commits.
- **RC1 wedge verdict**: the verdict terminates the claim (failed +
  escalated); "believed silent" is not "stopped" — a wedged generation
  may still be holding pod resources. Aborted like any deposal.
- **Persona lapse pass** (in_review, both arms — persona-kept and
  config-removed): the reviewer's claim ends; the reviewer session is
  aborted post-commit of its own lapse transaction. The pass's SELECT now
  reads `assignee_session` (it didn't before — the identity was cleared
  but never consumed).

### The mechanics

- `deposeTx` — in the SAME transaction as the reclaim: a `reclaim_abort`
  board event (deterministic id `reconciler:reclaim-abort:<task>:<episode>`,
  the episode keyed like the reclaim's own id) with payload `{session,
  reclaim_event}` — S7's trail: WHICH session, WHEN (created_at), WHICH
  reclaim ended it. Returns the post-commit delivery entry.
- `abortDeposed` / `abortDeposedWorkers` — the delivery: post-commit,
  best-effort (the 409 arbitration stays the authority; this is the
  belt), bounded-concurrent at 8 (the nudge-batch bound; a mass lapse
  reclaims N sessions in ONE sweep), completing before the phase returns
  so pins observe a settled batch.

### The guards (criterion 2)

- **Post-commit ONLY** — a pre-commit abort could cut a session whose
  reclaim then rolls back: an abort of a LIVE claim. (delete.go's r2
  ordering rule, adopted.)
- **RowsAffected>0 under the expiry predicate** — the abort fires only
  when THIS sweep's guarded UPDATE ended the claim. A live/renewed claim
  renews/holds and never reaches the deposal. Renewals and nudges never
  touch the abort.
- **Idempotent** — a re-sweep finds the row no longer a candidate (no
  second abort); a deterministic-id replay (concurrent twin writer) is
  detected in `deposeTx` (recordEventTx's ON-CONFLICT no-op) and delivers
  nothing; the aborter itself is abort-idempotent (an already-stopped
  session is a harmless no-op).
- **Names the OLD holder by captured identity** — never a re-read of the
  row, which by delivery time may already name a fresh claimant (fresh
  sessions are spawner-minted per claim, R9 — the captured id can never
  be the new holder).

### What deliberately does NOT abort

Worker-initiated claim endings — the throttle self-report (the worker is
alive and exiting by its own action), the →in_review forward move and the
verdict settle (the writer's own successful write proves it alive), the
bounce mint (the reviewer superseded its own claim), rearm of a failed
row (claim already dead), `exitPendingReclaim` (ALREADY aborts via
`nudge.abortAssignee`). Platform-initiated endings abort; self-initiated
endings don't need to.

## §3 Transport (criterion 3)

Production rides the existing `WorkerAborter` seam — `newBoardAborter`
(api/internal/app/board_worker_comms.go): phrase-resolve then the V1
session abort POST. It was ALREADY installed on the reconciler (spawnOpts
feeds both the service and `NewReconciler`, app.go) — no wiring change
needed, only the call. NO dependency on the S4 bus (or any bus): the
v1-fallback requirement holds by construction; pinned by the nil-seam
degradation arm (S8-4: a reconciler with NO aborter still reclaims
byte-for-byte, writes no audit row).

## §4 Tests (pkg/board/reclaim_abort_pins_test.go, PG integration)

All positive pins born-RED (verified by reverting reconciler.go+ids.go:
S8-1/2/5 fail without the feature; S8-3/4 are guard pins, green on
absence, red only on over-abort):

- **S8-1** `TestPin_S8_1_ReclaimAbortsDeposedSession` — dead-evidence
  reclaim: exactly one abort naming the OLD session; AT ABORT TIME the
  row is already backlog with the token gone (post-commit ordering,
  observed from inside the abort hook); the audit triple (session /
  reclaim pointer that RESOLVES to the reclaim event / timestamp);
  duplicate sweep idempotent; a fresh epoch's lapse aborts the FRESH
  holder (unique-mint spawner — the suite default reuses task-keyed ids,
  which cannot discriminate holders).
- **S8-2** `TestPin_S8_2_WedgeVerdictAbortsDeposedSession` — the verdict
  path; the audit pointer names the WEDGE event; a nudged (still-claimed)
  row aborts nobody.
- **S8-3** `TestPin_S8_3_LiveClaimNeverAborted` — three arms: moving
  worker across 3 expiries, heartbeat-response renewal, and
  nudged-but-undecided: zero aborts, zero audit rows, at every rung.
- **S8-4** `TestPin_S8_4_NoSeamStillReclaims` — nil-seam degradation.
- **S8-5** `TestPin_S8_5_PersonaLapseAbortsReviewer` — the in_review
  twin: the REVIEWER session is aborted (never the long-gone
  implementer), audit pointer resolves to the persona lapse's reclaim
  event.

## §5 Verification

- `go test -tags=integration ./pkg/board/` — full PG suite green (42.4s),
  including every pre-existing lease pin (1–12) and the persona suites
  (the new aborts ride their sweeps without disturbing a single count).
- `go build ./...`; `go vet` (incl. `-tags=integration` on pkg/board);
  `make fmt-check` green; `golangci-lint run ./pkg/board/...` 0 issues;
  `-short ./api/internal/app ./api/internal/handlers ./pkg/mcp` green.
- Local PG for the suite: the pod's shared instance at
  `127.0.0.1:5433/llmsafespaces_test` (migrated schema).

## §6 Interplay (the epic's map)

- **S5 (DEPOSED)** is the orchestrator-initiated salvage handoff; THIS is
  the platform-automatic stop. Together: reclaim → old worker verifiably
  stopped (S8) + new worker inherits the salvage pointer (S5).
- **S7** (the observation trail) consumes the `reclaim_abort` events:
  which session, when, which reclaim.
- The abort is belt-and-suspenders on top of expiry + the 409
  arbitration: those remain the authority; this makes the stop enforced
  rather than assumed.
