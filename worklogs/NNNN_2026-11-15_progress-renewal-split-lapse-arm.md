# Worklog — #2043: the two-clock split (last_renewed_at) + the heartbeat-lapse arm

Lane: `tsk-elder-tornado-waits` (epic "#2040 remediation: honest lease liveness (FMD-Web
incident)"). Branch `fix/2040-progress-renewal-split`, off `origin/main` @ `71cb54f80` in a
worktree. Binding spec: issue #2043 (read in full first; its R1-R4 and acceptance criteria are the
charter). Sibling sub-issues of the #2040 remediation epic: #2042 (honest movement evidence —
landed), #2044 (no-advance budget for heartbeat responses), #2046 (directive delivery discipline —
landed).

## 1. The incident, restated as the code's fault

The RC1 ladder's `ladderRenew` write stamped `last_progress_at=now()` on every evidence-based
renewal (`reconciler.go`, the write at the old line ~516). `last_progress_at` therefore meant "some
writer touched the row", and the victims query only ever sees LEASE-DUE rows — so a row the platform
kept renewing looked like a row the worker kept progressing. During the 2026-11-15 incident, renewal
stamps (00:27/00:32/01:12) were indistinguishable from progress; each renewal deferred wedge/reclaim
detection by a full lease window per renewal, indefinitely.

## 2. What landed (R1-R4)

- **R1 — the two-clock split** (migration `000055_task_last_renewed_at`): new column
  `tasks.last_renewed_at timestamptz`. The ladder renewal write stamps `last_renewed_at=now()` and
  does NOT touch `last_progress_at`. The progress clock's writers are now exactly: claim mints
  (`stage.go` x2, `engine.go` x2, `persona.go`) and heartbeat responses (`stage.go` — the
  worker-declared, #1847-clamped `observedProgressAt`). `idx_tasks_reclaim` untouched (keys on
  `lease_expires_at`). The suspension freeze (`freezeLeaseSQL`) keeps its deliberate no-clock
  discipline — it never touched the progress clock either; documented in the design doc.
- **R2 — the heartbeat-lapse arm**: `board.EngineConfig.HeartbeatLapseThreshold` (default 90m = 2
  lease windows, validated like `LeaseDuration`; deploy knob
  `LLMSAFESPACES_BOARD_HEARTBEATLAPSESECONDS` / `board.heartbeatLapseSeconds`, helm-wired like
  `leaseSeconds`). The victims query computes the lapse SERVER-side in the same Postgres `now()`
  clock domain as every other ladder comparison: `last_progress_at IS NULL OR last_progress_at <
  now() - threshold` — nil = lapsed-by-default (the honest direction; no evidence of worker progress
  is no movement veto). In the decision loop, BEFORE the evidence rungs and AFTER the
  heartbeat-response rung, a lapsed row's `MovedSinceLastSweep`/`EventsAfterNudge`/`Busy` are read
  as false — the ladder then flows through its ORDINARY arms: dead-evidence reclaim preserved,
  no-evidence floor unchanged, nudge → heartbeat-or-wedge. The response channel still wins
  (`hbAfterNudge` precedes the override), and because a genuine response also refreshes
  `last_progress_at`, the arm is STATELESS — any worker-anchored write resets it for free. The arm
  touches no accounting (attempts / failure_count / busy_renews).
- **R3 — observability**: new always-on counter `board_ladder_heartbeat_lapse_total` (suppressed
  renewal evidence — the incident's counterfactual in one scrape) + `NudgePayload.Reason`
  (`heartbeat_lapse`) so the sentinel itself is attributed. DEVIATION from the issue's primary R3
  phrasing ("renewal-source counter gains a heartbeat_lapse reason"), taken via the issue's own
  parenthetical alternative ("or the nudge payload carries it") and argued in the design doc: a
  lapse suppression is the OPPOSITE of a renewal decision, and `board_lease_renewals_total`'s
  forensic value is its honesty (the #2042 lane's whole point) — a non-renewal label there would
  blur the exact scrape the incident made necessary. Flagged for the reviewer's judgment.
- **R4 — backfill**: `last_renewed_at` initialized from `last_progress_at` in the migration up
  (conservative mid-deploy: today's semantics persist for in-flight rows; the arm first fires only
  after the threshold genuinely elapses without worker progress); down drops the column. Populated-DB
  pin on a throwaway database parked at 000054 (the 000052 pattern): verbatim copy asserted on a
  fresh-claim row and an old-renewal row, NULL preserved on the legacy NULL row, down drops the
  column without disturbing `last_progress_at`, and the backfill's `WHERE last_renewed_at IS NULL`
  guard is pinned never to clobber a real renewal stamp.

## 3. Migration cohort coordination (the charter's boundary note)

- Next-free number confirmed at implementation: 000052-000054 taken → `000055` (re-confirmed against
  `api/migrations/` and `helm/migrations/`, both synced via `make chart-sync-migrations`).
- #2044 (no-advance budget, `no_advance_renewsame` cohort) lands AFTER this story; a single migration
  carrying both cohorts is acceptable per the issue — this one stays single-purpose so #2044 can
  either extend 000055 or carry its own (their call at their implementation time).
- Boundary STATED (design doc §#2043 amendment + the pins' header comment): heartbeat responses
  write worker-DECLARED progress — a compliant-but-stuck worker could declare fresh progress every
  nudge episode and keep this arm reset. That orbit is #2044's jurisdiction. This story trusts the
  progress clock exactly as far as `last_heartbeat_at` semantics already do (an ordinal arrival
  signal), no further.

## 4. TDD record (born-red, the red observed before the fix)

`pkg/board/heartbeat_lapse_2043_integration_test.go`:

- **2043-A (born-red observed)**: movement renewal leaves `last_progress_at` byte-identical to the
  claim-mint stamp and stamps `last_renewed_at` — pre-fix the watermark ADVANCED (red), post-fix
  unchanged (green).
- **2043-B (born-red observed, all three shapes)**: lapsed row + `MovedSinceLastSweep` / `Busy` /
  `EventsAfterNudge` evidence → NO renewal; the sentinel lands (payload Reason `heartbeat_lapse`,
  counter +1); the next silent window past the sentinel lands the wedge verdict with its audit
  event. Pre-fix: nudger.count()==0 for every shape (red — the evidence renewed the lapsed row).
- **2043-C (reset)**: heartbeat response on the nudged lapsed row renews (the response channel IS
  the reset), its declared stamp anchors the progress clock, and the NEXT expiry's movement evidence
  renews with NO fresh nudge — and still never advances the progress clock.
- **2043-D (born-red observed)**: nil progress → lapsed-by-default → nudge, and the row's watermark
  stays nil (pre-fix the renewal WROTE a stamp — the incident's lie, red).
- **2043-E (guard)**: the arm itself touches neither attempts nor failure overlay (the wedge verdict
  it feeds does the escalation — asserted in B's close).
- **2043-F (guard)**: the ORDINARY quiet nudge carries no lapse reason and arms no lapse count.

Migration pin: `api/migrations/migration_000055_populated_test.go` (populated-DB up/down + backfill
verbatim + NULL preservation + idempotent re-apply guard).

## 5. Verification (re-verified on re-claim, against a live Postgres 16)

The first attempt's local run lost its database before pushing; the re-claim re-ran everything against
a fresh rootless Postgres 16.5 (`TEST_DATABASE_URL` up on 127.0.0.1:5433) and caught two real defects
the first pass had reported as green because the migration pin SKIP-MASKS without a reachable
database ("ok [no tests to run]" is not a pass — recorded here so the lesson survives):

1. **The migration pin's seed violated the 000043 phrase-id checks** (`prj-mig55` fails
   `projects_short_id_check`; the constraint accepts `prj-[0-9a-z]{8}` or the three-word phrase
   form). Seeds now use the 8-char form (`prj-mig00055`, `tsk-mig0001a`, …) — the 000052 pin's
   `prj-migrated` idiom followed exactly.
2. **The post-down assertion reused `backfill()`**, whose SELECT names `last_renewed_at` — the very
   column the DOWN just dropped — so the check died with `column "last_renewed_at" does not exist`
   instead of asserting its point. Replaced with a progress-clock-only read (`SELECT
   last_progress_at::text`), which is what "down must not disturb last_progress_at" means.
3. **The deploy knob was helm-wired and CHANGELOG-documented but never parsed or plumbed** —
   `LLMSAFESPACES_BOARD_HEARTBEATLAPSESECONDS` had no `Board.HeartbeatLapseSeconds` field, no viper
   default, no `applyBoardEnvOverrides` leg, and no `app.go` engine-config plumbing: the knob would
   have silently done nothing in every deploy. Wired end to end (field + `SetDefault` 5400 + env
   override + `board.EngineConfig.HeartbeatLapseThreshold` plumbing) and pinned in
   `TestBoardEnvOverrides_R6` / `_R6_InvalidIgnored` (override lands; non-numeric never clobbers).

Post-fix battery, all green:

- `TEST_DATABASE_URL=… go test -tags=integration -race -count=1 -timeout 900s ./pkg/board/` — ok
  (17.9s): the 2043 pins and the full pre-existing lease/ladder/persona/kill suites (nothing ever
  depended on the renewal's progress stamp — it was the lie, nothing legitimated it).
- Born-red RE-verified mechanically: with `pkg/board/reconciler.go` stashed, pins 2043-A and 2043-B
  FAIL (2/2); restored, the suite is green.
- `TEST_DATABASE_URL=… go test -tags=integration -run TestMigration000055 -v ./api/migrations/` —
  PASS (populated up/down, verbatim backfill, NULL preservation, boundary-shape reads, idempotent
  re-apply guard), plus the full `./api/migrations/` package green.
- `go build ./...`, `go vet` (board/config/app/migrations), `go test -race ./api/internal/config/
  ./pkg/board/` — ok; `golangci-lint` on the touched packages — 0 issues (one ST1022 const-comment
  form fixed); `make fmt-check` — clean.

## 6. Rejected alternatives (recorded per the repo's worklog discipline)

- **Renewal-source label on `board_lease_renewals_total`**: rejected — pollutes the #2040 forensic
  signature (a suppression is not a renewal); the issue's own alternative arm chosen (see R3).
- **Zeroing evidence by restructuring the switch (a dedicated lapse rung short-circuiting to
  nudge/wedge)**: rejected — it would also override dead-evidence reclaim (pin 5's fast path) and
  the no-evidence floor's semantics; zeroing the three flags before the rungs preserves every
  existing arm verbatim.
- **Computing the lapse in Go from the scanned timestamp**: rejected — cross-process clock read; the
  house discipline is one clock domain (SQL `now()`), same as the expiry comparisons.
- **Writing `last_renewed_at` from every lease extension (freeze, nudge measurement window, mints)**:
  rejected for v1 scope — the column's purpose is the incident's lie (evidence-based renewal
  masquerading as progress); the freeze never lied (documented), and the nudge window is not a
  renewal decision. Enumerated in the design doc so the boundary is deliberate, not accidental.

## 7. Process

AI-reviewer loop until APPROVED (never self-merge); orchestrator merges; `make fmt-check` clean; no
sleep-polling in tests (the pins drive the sweep deterministically via the rc1.post failpoint seam —
the house pattern).
