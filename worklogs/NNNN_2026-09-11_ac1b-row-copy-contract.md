# Worklog: AC-1b pool row follows the copy-not-symlink XDG design

**Date:** 2026-09-11
**Session:** pool-green umbrella (epic-71 #1314, flagged in 5628098598): root-cause the remaining AC-1b delivery-pool red (agent: opencode-vesper)
**Status:** In Review

---

## Objective

Explain and fix the AC-1b cluster row ("XDG registry layer target != child OPENCODE_CONFIG") that has kept the delivery pool red on main alongside F1 (F1 fixed by #1321; pool run 34566214570 confirms F1+F6 green at budget 8 — this row is the only remaining red).

## Root cause

- The row's assertion (3) pinned the **symlink-era** mechanism (#1301, 1f8b1663): `readlink -f ~/.config/opencode/opencode.json` == child `OPENCODE_CONFIG`.
- **1d0e5be1 (2026-09-09 21:49, #1310) changed the mechanism**: the XDG layer is a **COPY, not symlink** — opencode WRITES to the XDG path (model-selection persistence) and the 0.27.5 symlink pointed into the read-only `/agentd-config` mount; the watcher now re-copies on sidecar version changes.
- In the pool's sidecar deploy, `readlink -f` on the regular-file copy returns the path itself → the equality can never hold. The pool's last green (2026-09-09) predates/straddles 1d0e5be1; every run since has this row red (34549292454 on main, 34566214570 on my branch — byte-same failure).
- The product is fine: F6 (registry admits the model after a faulted boot — the same #1300 path) PASSED in 34566214570, and the delivery's assertion (4) (`GET /api/model`) is the binding contract.

## Fix (row follows the as-built design)

Replace the symlink equality with the copy-contract pins, deliberately NOT byte-equality (opencode's own writes legitimately diverge the copy):
1. XDG path is a **regular file** (`file`/`symlink`/`missing` trichotomy; fails loudly on either stale mechanism).
2. The copy **carries the credential's provider block** (seeded from the live config).
3. The child's `OPENCODE_CONFIG` carries it too (existing grep, kept).
4. Assertion (4) — the registry admits the model — unchanged (the check that caught #1300's lying view).

## Key Decisions

- Content-equality (`cmp` XDG vs sidecar config) was rejected: opencode writes to the XDG copy by design (1d0e5be1's rationale) — byte-equality would flake on any user model-selection write.
- No pin-test change needed (no AC-1b/XDG pins exist in `us70_harness_script_test.go`).

## Assumptions stated and validated (Rule 7)

1. The XDG layer is a copy in sidecar mode post-1d0e5be1 — validated: `xdg_config_layer.go:88-118` (comment + legacy-symlink removal).
2. The copy is seeded from the live config (provider block present) — validated: the watcher re-copy path + F6's pass (registry fed through the copy).
3. The pool's last green predates the mechanism change — validated: 1d0e5be1 landed 2026-09-09 21:49; last green runs 34365633303/34333163267 same-day earlier; every run since red on this row.

## Tests Run

`bash -n` clean; `go test ./local/` green (all harness pins). Row verification rides the pool dispatch on this branch.

## Next Steps

- Pool dispatch on this branch (with #1321's budget or after it merges — either order works; the rows are independent) to confirm AC-1b green.
- Then the pool is fully green: F1 (#1321) + AC-1b (this PR) — the "get the pool green" mandate closes.

## Files Modified

- `local/us-70-secret-delivery-e2e.sh`
- `worklogs/NNNN_2026-09-11_ac1b-row-copy-contract.md` (this file)
