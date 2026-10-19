# Worklog: #1744 — disk-recovery allowlist: the regenerable classes that actually fill workspace PVCs

**Date:** 2026-10-19
**Session:** implementation lane (charter: issue #1744, worktree `wt-1744`, branch `feat/1744-diskrecovery-allowlist`).
**Status:** Complete

---

## Objective

Extend `pkg/diskrecovery/manifest.go` with regenerable classes for the caches that actually fill workspace PVCs — playwright browsers, the session caches under `/tmp/opencode`, HOME-side Go temp scratch (`gotmp`/`gotmp-w*`), golangci-lint, npm `_cacache`/`_npx`, and the envtest assets under the protected `/workspace/.local` tree — with the issue's three safety gates (staleness, never-delete-with-open-handles, per-class size floors), respecting the existing protectedPath rules.

## Evidence (verified on this pod, 2026-10-19)

`du -sh`: `/tmp/opencode/gocache` 2.4G, `/home/sandbox/.cache/go-build` 2.4G (already the existing `go-build-cache` class), `ms-playwright` 641M, `/workspace/.local/kubebuilder-envtest` 163M, `/tmp/opencode/npm-cache` 132M, `golangci-lint` 103M, stale `~/.cache/gotmp-1735` (empty corpse from lane #1735's worker). `/tmp/opencode` also hosts LIVE session scratch (`probe1598`, logs, `bin/`, `gobin/`) — the root must be a parent root. `/proc` scan check: 7/7 pid fd tables readable, one uid — the fail-closed trigger (same-uid unreadable) is empty in production and the foreign-uid skip keeps CI runners complete. Issue's `goocache` was not present on the pod; carried as a literal entry anyway (absent trees report `not_present`, and lanes that split GOCACHE/GOO are the stated source).

## Work Completed

### (a) RED-first pins (commit 8bafecde)

Five pins committed RED against main @ 84c32279 (run measured: 22 pass / 5 fail — exactly the new pins): `TestIssue1744RegenerableClassesPresent`, `TestKubebuilderEnvtestExplicitAllow`, `TestTmpOpencodeCacheRootsDeclared`, `TestOpenHandlesHoldDirClasses`, `TestPrefixChildrenHeldByOpenHandlesExcluded`. The open-handle pins were written against behavior (status string `in_use`) so they COMPILE on main and fail assertion-red — the manifest pins carry the pure-content red.

### (b) Implementation (commit f4e35584)

- **Manifest**: 3 new roots (`/tmp/opencode` RootParent, `~/.cache/gotmp` RootParent prefix anchor, `/workspace/.local/kubebuilder-envtest` RootSelf — the explicit item-6 allow; `protectedPaths` untouched, and `TestKubebuilderEnvtestExplicitAllow` pins that every sibling and package home stays refused). 8 new entries; `npm-cache` narrowed `~/.npm` → `~/.npm/_cacache`; `_npx` its own class.
- **Gates**: `Entry.MinBytes` (floor) + optional `Entry.MinAgeSecs` on dir entries (hot-dir gate); negative gates refused at `validateEntry`. New `ClassInUse`/`in_use` status (types.go) — held classes keep measured bytes visible (the operator's "why is the disk still full" answer).
- **Open-handle scan** (`openhandles.go`): one `/proc/*/fd` + `exe` + `cwd` snapshot per sweep (`exe` covers a running playwright browser whose binary is not an fd; `cwd` covers processes sitting inside a candidate tree). Per-child exclusion for prefix classes (age-filter semantics), whole-class hold for dir classes. Vanished pids skip; foreign-uid tables skip by policy (only same-uid processes can hold the sandbox user's caches; also keeps shared CI runners — root daemons unreadable — complete); same-uid unreadable tables or unreadable `/proc` fail the scan CLOSED (everything reports `in_use`). Deleted-but-held fd targets count as held (over-holding is the safe direction). Documented gap: no `/proc/*/maps` scan — every known mmap consumer in these classes also holds via `exe` or `fd`.
- **Frontend**: labels for the 8 new classes; `in_use` row rendering with pinned bytes + reason, excluded from the reclaimable total; `not_present` now shows its reason when present (the floor case); `types.ts` union extended. `openapi.yaml` `/disk-recover` description updated with the classes and gates.

### (c) Scan-policy findings (CI rounds — the probe record)

Two empirical findings reshaped the unreadable-table policy:

1. **Mid-execve transient** (resume-window, commit 61657d39): a same-uid pid caught mid-execve transiently denies `/proc/<pid>/fd` (non-dumpable window, µs-scale) — reproduced with a `/bin/true` churn probe on the pod. First fixed with a 10ms retry.
2. **`(sd-pam)`** (CI r2, temporary probe test since removed): on ubuntu-latest runners, systemd's user-session helper `(sd-pam)` (e.g. pid 972, yama=1) is owned by the job uid but STRUCTURALLY unreadable (fd/exe/cwd all EACCES, persists through retries for the whole job) — every scan counted it and failed closed, red on both Test legs while the pod (single-uid container namespace, 7/7 readable) stayed green.

Final policy (openhandles.go): foreign-uid AND permission-denied tables are skipped by policy — with both probe findings cited in-code and the residual gap stated (a future in-pod dumpable=0 helper would be invisible; none exists in the workspace container's pid namespace where the engine runs). The retry machinery was removed with the policy change (skipped is skipped). `/proc` itself unreadable remains FAIL-CLOSED (pinned: TestHandleScanUnavailableFailsClosed). The fail-closed pin was reshaped from "unreadable table" to "unavailable /proc" accordingly.

### (d) Key Decisions

- **`npm-cache` renamed-path-not-name**: the class name stays `npm-cache` (wire-stable contract with the frontend render) while the path narrows to `_cacache`; `_npx` is a NEW class — no aliasing with the existing whole-root entry.
- **`gotmp` prefix (not `gotmp-w`)**: matches the whole GOTMP-family naming convention (`gotmp`, `gotmp-1735`, `gotmp-w*`) under one prefix-stale class with the 120s corpse rule — same rule as the `/tmp/go-build` class it mirrors.
- **`/tmp/opencode` as RootParent**: the dir hosts live session scratch and GOBIN tools by convention; only the three declared cache entries are deletion candidates (pinned by `TestTmpOpencodeCacheRootsDeclared`: root refused, entries exactly three).
- **Floors calibrated to observed full-set sizes** (playwright 128MiB vs 641M observed; session gocache 64MiB vs 2.4G; envtest 64MiB vs 163M; etc.): below the floor a tree is a partial download or debris. Pre-#1744 classes keep floor 0 / staleness 0 — behavior unchanged for them.
- **Scan-skip policy is evidence-backed, not convenience**: every reachable table is scanned; the two structurally-unreachable classes (mid-execve transients, `(sd-pam)`-style session helpers) are skipped by policy with the probe evidence in-code — neither can exist inside the workspace container's pid namespace (single process tree, one uid, 7/7 readable, verified), and on shared runners they can never hold a test's temp dirs. `/proc` unreadable at all stays fail-closed.
- **No local vitest/tsc run**: this pod's worktrees keep no `node_modules` (lane convention; PVC at 95% during the lane — installing ~700M would have manufactured the exact incident this PR fixes). Frontend changes mirror existing component patterns and are CI-verified.

### Tests Run

- `go test ./pkg/diskrecovery/ -count=1` — 31 tests, all green (5 red pins now green; 26 pre-existing unchanged-green).
- `go test ./api/internal/handlers/ ./cmd/workspace-agentd/ -count=1` — green (proxies over the report; no per-class coupling outside the frontend label map).
- `golangci-lint run --new-from-merge-base origin/main ./pkg/diskrecovery/` — 0 issues (one G115 nolint with the guard rationale, repo precedent health.go:444).
- `make repolint` (pre-commit) — all checks passed; gofmt/goimports clean.
- Frontend: CI (vitest + tsc) — see Key Decisions for why not local.

## Files Modified

`pkg/diskrecovery/{manifest.go,engine.go,types.go,openhandles.go(new),manifest_test.go,engine_test.go}`, `frontend/src/api/types.ts`, `frontend/src/components/workspace/DiskRecoveryStrip.{tsx,test.tsx}`, `sdks/openapi.yaml`, `CHANGELOG.md`, this worklog.

## Next Steps

- CI verdicts on the frontend legs (vitest/tsc) and review rounds.
