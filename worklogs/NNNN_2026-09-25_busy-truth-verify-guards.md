
## Round 1 — the guards + the definition (landed)

- **Verify guards (exit 87/88, NOT 83/84 — those are opencode-overlay's
  codes in the same container; the collision was caught mid-round and
  the codes moved before any push)**: config≠tamper
  (empty/malformed pin/unknown arch → `verifyConfigError`, exit 87,
  "operator signal, not a tamper verdict"); baked self-denial
  (`bakedPathRefusal` — LLMSAFESPACES_AGENTD_BINARY authoritative,
  overlay-mount prefix fallback so a sanitized env cannot disable it;
  exit 88). Tamper's 81 + exact message shape pinned unchanged.
  Controller detection: two new reasons + metric outcomes
  (verify_config/baked_refused); crashloop-recovery skip covers all
  four classes (restart fixes none).
- **#1574 busy-from-data**: `BusyComponents` (additive proto: streaming
  / in_flight_parts / queue_depth / pending_user_inputs / busy) on
  SessionSnapshot; `deriveBusyComponents` is the ONE definition
  (busy := streaming || parts>0 || queue>0; QUESTION/PERMISSION
  carved out — pills keep pending_inputs as their surface, pinned);
  `enrichBusyLocked` feeds State() AND the snapshots (the derived
  status flips BUSY on every surface). statusz: `busyTruthFrom`
  overlays the authority truth over the tracker BOTH directions
  (tracker-idle+tool-running → busy; tracker-busy+permission-wait →
  idle), tracker stays the fallback for projection-unknown sessions.
- **Doctrine move, recorded**: reconcile_test pinned "queued ≠ running"
  (#1312) — #1574's owner rule supersedes: a queued delivery IS busy
  (autonomous progress pending; MESSAGE_START re-marks when it
  starts). The BusyCleared reconcile STAT survives (it clears the
  stale mark); the rendered status now stays BUSY with the queue leg
  surfaced. #998/D6 semantics interact (long-legit busy must not
  auto-escalate as hung) — PR4's surface.

## The re-exec hunt (post-PR round 1) — the class pinned, one leg closed

The current tree has NO agentd re-exec site (audited every exec.Command/
syscall.Exec: opencode serve via the sha256-verified overlay factory,
mise/git/bash helpers — none resolve agentd). The plausible class: a
bare-name `workspace-agentd` invocation from a SANITIZED tool-shell env
(session harness envs are scrubbed — no pin envs) on an old factory base
where /usr/local/bin/workspace-agentd still exists on PATH. The stale
pre-#1021 artifact then verified against sha256("") and group-killed.

One residual that class exposed, closed at this round: a scrubbed env
that loses the MARKER but keeps LLMSAFESPACES_AGENTD_BINARY previously
skipped verify entirely (marker-first short-circuit) — a CURRENT binary
would have silently run as the baked fallback. New leg: marker absent +
overlay coordinate present = sanitized-environment config error (exit
87) — the pod is overlay-wired; refusing loudly beats silently
degrading. Legacy pods (neither marker nor coordinate) still skip.

## r1 worked (all three blockers + the four test asks)

1. **The unreachable guard, fixed at the entry point**: the sanitized-env
   leg lived in selfVerifyDecision behind runSupervisorSelfVerify's
   legacy early-return — dead on the production path (the reviewer
   reproduced the binary supervising instead of exiting 87). The check
   now runs INSIDE the early return, before the legacy skip; the
   subprocess pin covers marker-absent + coordinate-present → exit 87
   through the real entry point.
2. **statusz internal consistency**: busy_ages/oldest_busy_seconds now
   filtered by the SAME authority truth the statuses render from (one
   snapshot per render, hoisted — never two truths in one response):
   authority-unbusied sessions stop contributing fictional age;
   projection-blind tracker sessions keep the tracker's age
   (pre-#1574 fallback); derived-busy-unstamped sessions carry no
   fictional clock. The D6 input can no longer contradict the status
   line next to it.
3. **Stale 83/84 comments** corrected to 87/88 everywhere they were
   introduced (verifyExitCode doc, main.go, supervise_opencode.go, the
   file-top contract block — which now also states the 87 config class
   and the sanitized-env shape).
4. Test asks: the entry-point subprocess pin; the handler-level
   non-nil-busyTruth test (through the REAL handler, both directions +
   the age-fixture); the busyTruthFrom adapter against a real Authority
   (wire-op-delivered queue leg + at-rest leg); the vacuous
   TestBusyQueueDepthCounts replaced with a real Deliver-driven
   queue-leg pin. Robustness: the O(N) per-session State() hoisted to
   one snapshot per render (busyTruthFn is now batch).
5. Alignment: the hand-numbered 1068 duplicate worklog removed (the
   sentinel file is the only one — the numbering bot owns assignment);
   the PR title scoped (refs #1573 — asks 2-4 are the umbrella PRs';
   closes #1574 only, which the reviewer assessed SUBSTANTIALLY
   ADDRESSED).

## r2 worked (the race + two residuals)

- **The data race**: `len(tracker.statuses)` on the polled statusz path
  was an unlocked read against the SSE goroutine's writes (a regression
  of the fix commit — r0 took the length from the RLock'd copy). Fixed:
  `busyAges := tracker.busyDurations()` first, size and range the
  local. Race-detector clean.
- **The duplicated sanitized-env leg**: removed from
  selfVerifyDecision (production-unreachable there); the legacy branch
  of runSupervisorSelfVerify is the class's one owner, pinned at the
  entry point by the subprocess test.
- **The unreachable compat branch** in busyTruthFrom removed (State()
  always enriches BusyComponents — the doc comment now says so).

## r3 worked (the terminal-veto regression + the dead field)

- **The ERROR mask**: EVENT_TYPE_ERROR deliberately leaves the turn's
  parts renderable in the record; the derived-busy overlay then flipped
  a TERMINAL error back to BUSY on every surface, unbounded (the
  reconcile sweep skips busy==false records — only a reseed cleared
  it). The reviewer A/B-reproduced against main. Fix: the terminal
  veto in enrichBusyLocked — status ERROR forces busy=false; components
  keep reporting the residual parts as data (honest), only the
  busy/status flip is vetoed; a new turn re-marks via status events.
  Pinned red-first (BUSY → PART_START → ERROR(with payload) → status
  stays ERROR, busy false, in_flight_parts reported, snapshot surface
  too).
- The write-only selfVerifyEnv.overlayBin dropped (the r2 de-dup
  orphaned it).
