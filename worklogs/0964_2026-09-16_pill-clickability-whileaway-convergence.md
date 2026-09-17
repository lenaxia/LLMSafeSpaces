# 0917 — 2026-09-16: #1365 frontend half — pill clickability + whileAway convergence (PR #1370)

## Session context

Follow-up to the 2026-09-14 permission-pill incident cluster (#1365,
#1396). Production 0.31.0 carries the server fixes (#1396: Act answers no
longer serialize behind the session lock; the 2m05s hangs are gone).
Remaining user-visible defect: immortal pills — stale prompts that render
forever and (during the #1396 era) felt unclickable.

## What shipped (PR #1370)

1. **whileAway staleness bound, timer-driven.** The inbox reply paths
   (#1313) emit no resolved events yet, so a whileAway pill had no
   convergence path. `useWhileAwayStalenessSweep` (SessionActivityProvider,
   module scope) drops pills older than 10min via `dropPendingAction` —
   no tombstone, so a genuine re-presentation can re-add. Review r2
   caught the first implementation being LAZY (in-effect check; a
   healthy-but-quiet stream never re-runs the fold-sync effect) — the
   bound is now interval-driven while any whileAway pill is pending.
   `receivedAt` is stamped on store entry; replays preserve the original
   stamp (a replay never extends the lease).
2. **`submitting` reset in `finally`** on both prompt components —
   defensive hygiene, per review r2's correction: the error path already
   reset in `catch`, and the success-survival path is blocked by the
   pre-existing tombstone. The honest incident attribution for
   "wouldn't let me click" is the #1396 hang (buttons disabled for the
   2-minute in-flight request), fixed server-side.
3. **Tests:** clickability regression set (surviving-instance + error
   path), stamp preservation, and the fake-timer sweep pins — including
   the exact r2 scenario (no dep changes; the interval alone drops the
   stale pill). ChatPage hookcount guard updated (+1 hook).

## Process notes

- The review's case-4 fake-timer request was initially declined as
  disproportionate — and the r2 catch (lazy bound) proved the reviewer
  right. Lesson recorded: when a reviewer names a test that would catch
  a class of bug, write the test before defending the omission.
- Rebase note: PR #1363's unified `InputRequest` shape landed under this
  branch mid-review; the rebase ported all three legs onto the new shape
  (`StoredInputRequest` extension carries `whileAway`/`receivedAt`).

## Verification

- `vitest run` chat/ + providers/ + hookcount: 494/494; `tsc --noEmit`
  clean. Post-deploy box: frozen-tab whileAway pills drop within ~11min
  of their stamp without any interaction.
