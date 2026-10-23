# Worklog: #1808 — sandbox filesystem zero-fill corruption (second class after #1793)

**Date:** 2026-10-23
**Session:** lane-1808 (P1 data-integrity), branch `fix/1808-nul-fill-corruption`.
**Status:** In progress — claim filed, red pins next.

---

## Objective

Issue #1808: five managed files in a workspace clone were silently
replaced with NUL bytes (length preserved) mid-session and reached the
remote via a pushed commit. Second sandbox-filesystem corruption class
after #1793 (SSH trailing spaces). Deliver the #1793-analogous belt at
the seams the platform controls, per the design offered in the issue's
read-only analysis (2026-10-21) which the 2026-10-23 triage marked
"fix unclaimed".

## Root-cause position (honest boundary)

The issue's verified analysis holds: no platform writer is in the
corrupted files' path, so the zero-fill enters below the file API —
ext4 delayed-allocation loss on an unclean stop (H1) or Longhorn
replica divergence (H2) are the ranked candidates; discriminating them
needs cluster-side evidence (Longhorn volume events, node dmesg) not
reachable from a workspace pod. This lane therefore does NOT claim a
single storage-layer root cause; it closes every platform-controllable
gap:

1. **Detection** — `pkg/fsaudit` + `POST /v1/fs-audit` (the issue's
   Ask #2, fsck-style verification).
2. **Propagation block** — pre-push template hook (the "before
   `git push`" half of Ask #2): the remote is the last uncorrupted
   copy; the belt keeps it that way.
3. **Flush discipline** — bounded `sync()` on the platform's own
   shutdown paths (found missing): the H1 window is dirty pages at
   unclean stop; the suspend path deletes the pod and never flushes.

## Work log

(filled as work lands)

- Claim filed in COORDINATE.md.
