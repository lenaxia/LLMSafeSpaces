# Worklog: #2044 the heartbeat-orbit budget — no-advance renewal bound + response-arm guard pin (the #2040 incident's third channel)

**Issue:** [#2044](https://github.com/lenaxia/LLMSafeSpaces/issues/2044) (sub-issue of [#2040](https://github.com/lenaxia/LLMSafeSpaces/issues/2040), the 2026-11-15 FMD-Web incident)
**Branch:** `fix/2040-heartbeat-orbit-budget` (off `origin/main` @ c672e5656, post-#2043/#2042)

## What this lane is

The orbit the first two lanes could not reach: `tsk-polished-keystone-waits` held its slot for
hours by answering every lease sentinel with `board_heartbeat` 19s / 4s after it landed — the
nudge contract's letter (FIRST respond) with none of its substance (continue / advance / declare
failure). #2043's lapse arm bounds EVIDENCE-based renewal on a stale progress clock, but the
heartbeat RESPONSE renews above it at every rung (`hbAfterNudge`, "the STRONGEST alive signal")
and refreshes the clock with the worker's own DECLARED stamp — so a presence-only worker that
never advances past its claim mint renews forever: attempts 1, failure_count 0, zero work since
23:21.

A note on the issue's line numbers: #2043 had already reshaped the ladder when this lane started
(the lapse arm, the two-clock split, the renewal write's `busy_renews` CASE) — the issue's
references were re-anchored against origin/main before implementation, and the design follows the
issue's REQUIREMENTS over its pre-#2043 file map wherever the two diverge.

## The fix (one budget, two enforcement sites, one belt)

**The detector — server-side, one clock domain.** Forward motion per claim epoch =
`last_progress_at > stage_entered_at`. The claim mint writes both in the same statement (equal at
mint — the claim is never motion); heartbeat responses are the progress clock's only other writer
(#2043's split). A declared watermark past the mint is motion; a stale or nil stamp is motionless.
Computed in the victims query (`noEpochMotion`), never in Go against wall clocks.

**Migration 000057** (`tasks.no_advance_renews integer NOT NULL DEFAULT 0`, api + helm mirror via
`chart-sync-migrations`): consecutive motionless heartbeat responses per claim epoch. The 000055
header named this cohort ("sibling #2044 adds a no_advance_renew same-cohort column ... rides its
own [migration]") — renumbered 000056→000057 after the wrapup lane (#2071) merged ITS 000056 to main mid-flight (the exact number race repolint guards; also a worklog-number race: its 1277 forced this worklog to 1280). 000057 follows the 000053 busy_renews discipline: plain column add, no
backfill, so no populated-DB migration test (000055 needed one only for its backfill).

**EngineConfig.NoAdvanceHeartbeatCap (K, default 3)** — code default + `NewService` clamp (<=0 →
3, the AttemptsCap class), deploy knobs `board.noAdvanceHeartbeatCap` (helm values + api
deployment env) / `LLMSAFESPACES_BOARD_NOADVANCEHEARTBEATCAP`, viper default, app.go wiring.

**Site 1 — the response arm (`stage.go` Heartbeat).** The orbit's actual renewal engine (the
handler renews; the ladder rarely sees these rows — the response clears the slot it answered). The
guarded UPDATE now computes motion in SQL (`$2::timestamptz > stage_entered_at`) and: motion →
counter reset + renew + clear slot (unchanged behavior); motionless below cap → counter+1 + renew
+ clear slot (the budget counts, it does not choke); motionless at cap → counter+1 + DECLINE:
arrival recorded, cycle counted, sentinel KEPT armed, lease not extended, `Extended=false`, and
the result's reason names the budget ("N heartbeats without advancement (cap K) ... advance the
work or declare failure"). All CASEs read the PRE-update row (SQL assignment semantics) — the
K+1th response sees its own arrival counted but judges on the count before it. The renewal lease
event is emitted only on an actual renewal (the sink projects decisions; the wedge event carries
the decline's facts).

**Site 2 — the ladder (`reconciler.go`).** The victims query gains `no_advance_renews`,
`stage_entered_at`, and the motion detector. `noAdvanceAtCap(cap, count, motionless)` (package
helper, extracted for gocyclo) gates: `hbAfterNudge` folds `!noAdvAtCap` (skip-and-fall-through —
NOT a Go `fallthrough`, which would execute the next case's body unconditionally and demote
movers); the renewal write's second CASE (`hbNoMotionRenew`) counts the ladder heartbeat arm's
motionless renewals (+1) and zeroes on every activity-backed renewal (movement/busy/reaction —
activity implies advance); at cap the three observation facts (`MovedSinceLastSweep`, `Busy`,
`EventsAfterNudge`) are zeroed for the lapse — **the #2042 interaction**: the heartbeat's own
PART frames are worker-activity frames that set movement and ordinal evidence alike; demoting
only the heartbeat arm leaves the orbit alive through `RenewSourceReaction`. The wedge verdict's
evidence prose (budget facts via `setWedgeEvidence`, which outranks the generic defaults) and its
`board_events` payload (`no_advance_renews`, `no_advance_cap`) carry the budget.

**The belt.** `noAdvanceAtCap` requires `noEpochMotion` — a row whose progress clock HAS crossed
the mint never demotes, whatever the counter says. Unreachable through the write discipline
(motion resets the count at every write site); guards drifted or hand-edited states. A mover is
never wedged by the budget.

**Resets (the full epoch-counter lifecycle, the busy_renews discipline).** Claim mints
(engine.go ×2), the persona claim mint + persona re-arm (persona.go, reconciler.go), the
MoveStage advance extra, bounceTx, exitPendingReclaim, the wedge verdict, and the suspension
freeze (an epoch break — the worker could not respond while frozen).

**Observability (R3).** Counter `board_heartbeat_noadvance_cycles_total` (the issue's name, the
board family's spelling — `board_*` prefix like its siblings; accessor
`HeartbeatNoAdvanceCyclesCounter`) increments on every motionless response (renew or decline);
plus the decline reason prose, the wedge evidence prose, and the wedge payload facts.

## The mover boundary — the design decision this lane had to make honestly

The orphaned first attempt (an unpushed branch; see the issue comments) left a mover-exemption at
cap ("observation-seam movement still renews") that its own review then pinned. This lane rejects
it: after #2042 the seam CANNOT distinguish a mover's tool frames from the heartbeat's own frames
(one allowlist, one worker index), so a movement exemption at cap re-opens the exact orbit being
closed — keystone's heartbeats kept `MovedSinceLastSweep=true` forever. The issue's letter ("at
cap EVERY renewal arm is demoted; heartbeat's own PART frames are presence, not reaction") is the
correct and implemented semantics. The mover's protection is the respond-first contract the
sentinel prose already demands: its next heartbeat declares a watermark past the mint — MOTION,
reset + renewal in the same guarded write, at cap (pin 2044-D a2). Below cap, movement renewals
reset the budget continuously, so a worker that moves never approaches it. The residual window (a
worker that starts real work between its K+1th response and the next sweep, without responding
first) is the contract-violating shape; the wedge there is the same class as any unanswered
sentinel, and rearm is the exit.

## R5 — the response-arm guard, verdict: born-green, documented

The pin (`TestPin_2044E_ResponseGuard_CommittedNudgeSequence`) drives the incident's exact
committed sequence — sentinel armed and committed (row state spelled; `nudged_at` backdated the
observed 4s wall gap; no sleep: the guard's comparison is the row's own `nudged_at`, never wall
`now()`), then the live-token heartbeat — and asserts the guarded RESPONSE arm: slot cleared,
lease extended, budget reset. Green on the first run, and by code trace it cannot be otherwise:
the response and advisory predicates differ ONLY in `nudge_id IS NOT NULL` (same stage set, same
token binding, same pending_input leg) — the issue's "token identity mismatch" suspect does not
exist in the current code — and the guarded UPDATE evaluates its WHERE on the live locked row, so
a raced READ of `nudge_id` cannot influence it (the send-window pin proves the during-sweep race
lands RESPONSE by lock ordering). The reachable advisory outcome for an ANSWERED sentinel is the
EARLY-side race — the worker responds to a sentinel whose row write has not landed (at-least-once
bus redelivery of an old sentinel, or delivery preceding the sweep's commit): the row genuinely
carries no armed nudge, the advisory path is the predicate doing its job, and the worker's NEXT
response converges. The pin's second half drives that shape and its convergence; the test's
comment reconstructs the incident against it. Per the issue's "born-red pin either way", the pin
locks the contract and the anomaly's trigger is documented rather than "fixed" — there is no
misfiring leg to fix.

## Pins (born-red evidence, `pkg/board/heartbeat_orbit_2044_integration_test.go`)

Against pre-fix main (inert skeleton — knob, column, counter — applied first so the pins compiled;
behavior pins failed, compile never):

- 2044-A the orbit: cycles 1..K renew with counter 1..K (pre-fix: counter 0, expected 1 — first
  red), the K+1th declines with the slot kept (pre-fix: renews), the final sweep — liveness
  reporting the heartbeat-frames presence shape — lands `StageFailed` with the wedge event's
  payload + prose carrying the budget facts (pre-fix: never).
- 2044-B the demotion per arm (movement frames / post-sentinel reaction / busy presence): all
  wedged at cap (pre-fix: renewed via the reaction/movement arms).
- 2044-C fresh declarations (cap+3 cycles): every response renews, counter 0, row alive — the
  healthy review-loop shape never bounded. Green pre-fix too (nothing bounded it yet) — the
  REGRESSION guard for the budget's own overreach.
- 2044-D (a1) below-cap movement renewal resets; (a2) at cap, a fresh-declared response is MOTION
  — reset + renewal through the handler; (b) the ladder heartbeat belt counts below cap and skips
  at cap into the wedge (measured-silence observation). Pre-fix red on both counts.
- 2044-E R5: committed-sequence RESPONSE + early-side race convergence (born-green; the honest
  verdict above).
- 2044-F reviewer personas: in_review heartbeats are advisory every cycle (no sentinel ever arms
  on in_review — the ladder scans in_progress only), counter never moves. Green pre-fix;
  regression guard.

Suite health: full `go test -tags=integration -race ./pkg/board/` green (lease pins, #1847, #2043
lapse pins, persona suites — unchanged semantics; the #2043 boundary comments anticipated this
lane). `TestIntegrationBoardWorkerSemantics` green. `TestDefaultEngineConfigR6` extended with the
knob pin; board env-override pins extended (apply + invalid-ignored).

## Flake observation (honesty over tidiness)

One full-suite `-race` run failed `TestPin_LeaseLadder_HeartbeatInSendWindowIsNotLost` ("Should be
empty, but was <uuid>" — the slot-clear assert). Three subsequent full suites, 15/15 isolated
stress runs, and 3× the interaction set (ladder + 2043 + 2044 + persona pins) all green; by code
trace no interleaving my change introduces can produce it (the response arm's match is
lock-ordered against the sweep's nudge write, and a fresh claim's counter is 0 — no decline path).
The repo carries a known race-flake class (CI retrigger precedent c680938a8). If it recurs in CI,
re-run once before blaming the budget; if it recurs twice, the pin's goroutine/commit race is the
place to instrument — not the budget's CASEs.

## Skipped/deferred, with reasons

- No populated-DB migration test: 000057 has no backfill (the 000053 class, not the 000055 class).
- No depose.go change: `deposeShowsLife` reads `busy_renews` for its busy cap, but an at-cap
  orbiting worker's `last_heartbeat_at > nudged_at` still shows life (correctly — the worker IS
  alive; the wedge verdict, not depose, owns the orbit).
- The shared test DB carried a phantom `schema_migrations.version=56` from an earlier unpushed
  attempt (no column, no artifact — verified against the 55-schema) before this lane; restamped to
  55 and migrated forward with the real migration (now 000057). Noted here because the next lane on this DB
  should not be surprised by the history.
