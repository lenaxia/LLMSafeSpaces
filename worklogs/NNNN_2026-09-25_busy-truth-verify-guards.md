
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
