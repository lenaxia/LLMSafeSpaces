# Worklog: AC-1b pool row follows the copy-not-symlink XDG design

**Date:** 2026-09-11
**Session:** pool-green umbrella (epic-71 #1314, flagged in 5628098598): root-cause the remaining AC-1b delivery-pool red (agent: opencode-vesper)
**Status:** In Review

---

## Objective

Explain and fix the AC-1b cluster row ("XDG registry layer target != child OPENCODE_CONFIG") that has kept the delivery pool red on main alongside F1 (F1 fixed by #1321; pool run 34566214570 confirms F1+F6 green at budget 8 — this row is the only remaining red).

## Review r2 correction (record hygiene)

The r1 grep-guard INVERTED the misattribution it claimed to fix: `grep -c` exits 1 on zero matches, so a copy lacking the provider block fired the EXEC-failure die and the content die was dead code. Fixed by normalizing the remote exit (`; true` inside the sh -c — transport failure remains the only nonzero path) plus a numeric-validated count check; the pins now EXECUTE THE SCRIPT'S EXTRACTED TEXT (regex-extracted probe + grep command with the outer-expansion `${XDG_CFG}` substitution replicated) instead of re-implementing the logic inline — an inline copy stays green while the script drifts. New pin `TestUS70AC1BRow_SeedGrepSemantics` fails if the `; true` normalization is ever dropped.

## Root cause

- The row's assertion (3) pinned the **symlink-era** mechanism (#1301, 1f8b1663): `readlink -f ~/.config/opencode/opencode.json` == child `OPENCODE_CONFIG`.
- **1d0e5be1 (2026-09-09 21:49, #1310) changed the mechanism**: the XDG layer is a **COPY, not symlink** — opencode WRITES to the XDG path (model-selection persistence) and the 0.27.5 symlink pointed into the read-only `/agentd-config` mount; the watcher now re-copies on sidecar version changes.
- In the pool's sidecar deploy, `readlink -f` on the regular-file copy returns the path itself → the equality can never hold. The pool's last green (2026-09-09) predates/straddles 1d0e5be1; every run since has this row red (34549292454 on main, 34566214570 on my branch — byte-same failure).
- The product is fine: F6 (registry admits the model after a faulted boot — the same #1300 path) PASSED in 34566214570, and the delivery's assertion (4) (`GET /api/model`) is the binding contract.

## Fix (row follows the as-built design)

Replace the symlink equality with the copy-contract pins, deliberately NOT byte-equality (opencode's own writes legitimately diverge the copy):
1. XDG path is a **writable regular file** (the `file`/`readonly`/`symlink`/`missing` quadrotomy, r2; fails loudly on the read-only class 1d0e5be1 exists to close and on either stale mechanism). The probe capture runs under an `if !` guard (r13/r30) so a transient kc exec failure dies with context instead of killing the leg.
2. The copy **carries the credential's provider block** (seeded from the live config) — grep with `; true` remote-exit normalization (r2: `grep -c` exits 1 on zero matches; without it the content path fires the exec-failure die) and a numeric count check.
3. The child's `OPENCODE_CONFIG` carries it too (existing grep, kept).
4. Assertion (4) — the registry admits the model — unchanged (the check that caught #1300's lying view).

## Key Decisions

- Content-equality (`cmp` XDG vs sidecar config) was rejected: opencode writes to the XDG copy by design (1d0e5be1's rationale) — byte-equality would flake on any user model-selection write.
- Pins EXECUTE the script's extracted text — the probe payload, the grep command (with the normalization asserted), and (r3) the `case` decision block — so script drift fails the pins (an earlier inline Go re-implementation stayed green under mutation; caught in r2 review).

## Assumptions stated and validated (Rule 7)

1. The XDG layer is a copy in sidecar mode post-1d0e5be1 — validated: `xdg_config_layer.go:88-118` (comment + legacy-symlink removal).
2. The copy is seeded from the live config (provider block present) — validated: the watcher re-copy path + F6's pass (registry fed through the copy).
3. The pool's last green predates the mechanism change — validated: 1d0e5be1 landed 2026-09-09 21:49; last green runs 34365633303/34333163267 same-day earlier; every run since red on this row.

## Tests Run

`bash -n` clean; `go test ./local/` green (all harness pins). Row verification rides the pool dispatch — [r6 correction: never on THIS branch; the evidence rides the sibling `pool-green-combined` head 31632400. At dispatch time (2026-09-11), that head's delivery-script and worklog blobs were identical to this PR's and its test-file delta was purely #1321's additions — the provenance held as verified in #1326's r6 review; r7's later edits to this PR's worklog and test file changed those blobs AFTER the run, so the identity claim is historical, scoped to the dispatched head, not a statement about the current tree].

## Next Steps

- Pool dispatch on this branch (with #1321's budget or after it merges — either order works; the rows are independent) to confirm AC-1b green.
- Then the pool is fully green: F1 (#1321) + AC-1b and AC-1e (both this PR) — the "get the pool green" mandate closes. [r6 correction: the original line omitted this PR's own AC-1e row.]

## Files Modified

- `local/us-70-secret-delivery-e2e.sh`
- `local/us70_harness_script_test.go` — six executed/structural pins (r1–r4): probe trichotomy, grep normalization, copy-contract strings, case-block execution, guard discipline, seed-decision execution
- `worklogs/NNNN_2026-09-11_ac1b-row-copy-contract.md` (this file)

## AC-1e scope addition (r5 record)

Unmasked by the AC-1b fix (full-build run 34611591753: delivery + registry + turn all healthy; only the llmsafespaces_ grep missed): **AC-1e was born unsatisfiable** — landed in 75a3f802 (2026-09-10 03:46) with the mock LLM always replying the canned MOCK-TURN-OK, so the grep target could never appear; the row never executed in a green run (AC-1b died earlier in every run since). Fix: the mock parses the request's tools array (function.name + flat name shapes) and echoes the names one per line after the marker — the stated contract (definitions travel in the request); the #1313 V2-steer regression yields the bare marker and fails the row, as designed. AC-1d unaffected (substring grep; marker stays first). Pinned by `TestUS70MockLLM_ToolEchoPins`, which EXECUTES the serve.py echo block EXTRACTED from the script (common-prefix dedent, executed verbatim in python3 against the three shapes — r5: the first draft's inline duplicate was the exact anti-pattern this PR's history outlawed, caught in review).
