# Worklog: #1759 — the three PVC mount points read-write by default in the opencode permission floor

**Date:** 2026-10-19
**Session:** continuation lane after #1753 (same worker; orchestrator ride-along dispatch, split as its own PR — different layer from #1758). Worktree `wt-1759`, branch `feat/1759-pvc-permissions`.
**Status:** Complete

---

## Objective

The opencode.json config writer's platform permission floor (`pkg/agent/opencode/permissiontiers.go`) must default-ALLOW all three PVC mount points — `/workspace`, `/tmp`, `/home/sandbox` — read-write, with every existing credential/system deny preserved and the deny-over-allow precedence pinned.

## Root cause

The 2026-09-19 ruling's tier map carved the home root back to ambient **ask** with three narrow subpath pre-allows (`.cache`/`.config`/`.local`), left `/tmp` children-only allow (bare `/tmp` fell to ambient), and carried NO `/workspace` entries (the harness exempts the project root from external_directory governance). Agents kept hitting permission walls for folders that are theirs by design — the live incident class the owner cites.

## Work Completed

### (a) RED-first pins (commit 9f5774a8)

Flipped/new rows in `TestPermissionTier_PrecedenceModel` (`/tmp` bare, `/workspace` bare + child, `/home/sandbox` + a deep child — allow) and the new `TestPermissionTier_PVCRootsDefaultAllow` (six allow keys + the full deny-preservation list on the tier map). Verified RED vs main @ 95c5782b: exactly 6 failing subtests (the flipped/new rows); every deny row green — the preservation half was already true and must stay true.

### (b) Implementation

`platformPermissionTiers` amendment:
- `/home/sandbox` + `/home/sandbox/*`: ask → **allow** (both keys retained — the bare key is the r0 finding-3 shield against the `/home/*` deny matching the bare path).
- `/tmp` bare **allow** added next to `/tmp/*`.
- `/workspace` + `/workspace/*` **allow** added (governs bash-typed/tool-resolved paths in the merged permission model; the project-root exemption itself is unchanged — residual 1).
- The superseded `.cache`/`.config`/`.local` sub-allows REMOVED (dead rules in a security floor are noise; the root allow covers them).
- Deny set untouched. Sort-precedence argument updated in the file header: every credential deny sorts after `/home/sandbox/*` because `'*' 0x2A < '.' 0x2E` — the deny is the last matching rule under findLast.
- The two carve-ordering rows referencing removed keys updated (`auth.json` deny now pinned against the ROOT allow — the stronger statement).

### (c) Key Decisions

- **Bare keys + children globs for all three roots** — the wildcardless-key rule (r0 finding 3) applies to every root: without the bare key, `/home/*`-style parent denies match the bare path.
- **Remove, don't keep, the superseded sub-allows**: identical-action dead rules dilute the security floor's readability; the sort-order tests now pin the STRONGER property (credential denies beat the ROOT allow directly).
- **No weakening of `allowReopensTierDeny`**: no denies were added or reshaped, so the floor-dominance soundness premise (deny-shape: exact or trailing-`/*`) is untouched; operator allows under the roots were never reopen-class.
- **`/sandbox-runtime` stays ambient-ask outside the denies** (rt/ssh, rt/git-credentials name-ask) — deliberately unchanged; #1759 is scoped to the three PVC roots.

## Tests Run

- `go test ./pkg/agent/opencode/ ./cmd/workspace-agentd/ -count=1` — green (the full tier suite incl. writer-render, floor-dominance, self-tamper, and agentd config pins).
- golangci --new-from-merge-base (pre-commit): 0 issues; gofmt/goimports clean; repolint clean.

## Files Modified

`pkg/agent/opencode/{permissiontiers.go,permissiontiers_test.go}`, `CHANGELOG.md`, this worklog.
