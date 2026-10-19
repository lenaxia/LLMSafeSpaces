# Worklog: #1758 — the Go build-cache economy moves off the PVC (dedicated 8Gi emptyDir)

**Date:** 2026-10-19
**Session:** continuation lane after #1753/#1744 (same worker; orchestrator dispatch). Worktree `wt-1758`, branch `feat/1758-gocache-emptydir`.
**Status:** Complete

---

## Objective

Implement the #1744 maintainer addendum (tracked as #1758): point `GOCACHE` and `GOTMPDIR` at a dedicated disk-backed emptyDir (8Gi `SizeLimit`) instead of the PVC-backed `$HOME/.cache` and `/tmp` defaults — one env change in the platform env seam plus one volume, so the regenerable cache economy dies with the pod and the PVC holds durable data only.

## Evidence

The addendum's numbers (from the repeat-incident evening): 5 manual sweeps, 55%→93% in minutes under 5 concurrent Go lanes, ~5G/hour cache regrowth. The mounts audit: every writable surface agents get is PVC-backed — agents cannot self-relocate; this is a platform fix. On this pod pre-fix: `$HOME/.cache/go-build` 2.4G, `/tmp/opencode/gocache` 2.4G (lanes' self-convention).

## Work Completed

### (a) RED-first pins (commit 34c40acc)

`controller/internal/workspace/build_cache_test.go` — `TestGoCacheEconomy_RelocatedToEmptyDir` (env + volume + mounts) and `TestGoCacheEconomy_SubPathMountsGuaranteeDirs`. Verified RED against main @ 95c5782b: exactly these two fail (GOCACHE absent at build_cache_test.go:35; no build-cache volume); all pre-existing workspace pins green.

### (b) Implementation

- `platform_env.go` (`platformBaseEnv`): `GOCACHE=/sandbox-cache/go-build`, `GOTMPDIR=/sandbox-cache/gotmp`. The env seam reaches exactly the two containers that run platform tooling (main via pod_builder:229, workspace-setup via :739) — no other surface.
- `pod_builder.go`: volume `build-cache` — emptyDir, DISK-backed (no `Medium`), `SizeLimit 8Gi` (the addendum's bound). Mounted as TWO subPath mounts (`go-build`, `gotmp`) on BOTH containers. SubPath mounts are load-bearing: emptyDir starts empty, and the `go` tool MkdirTemps INSIDE `GOTMPDIR` without creating it — the mounts guarantee both dirs exist at start. On workspace-setup this is not just hygiene: its root filesystem is READ-ONLY, so without the mount every `go install` in the init fails.
- `docs/operator/storage.md`: the volume table gains the sixth row (six mounts now: three PVC, two tmpfs, one disk emptyDir), a new section "Disk-backed emptyDir: the build cache (#1758)" with the trade-off notes, the /tmp paragraph notes the relocation, TOC updated.

### (c) Key Decisions

- **GOPATH stays on the PVC** (module cache = re-downloadable but network-expensive; durable across suspend/resume; the #1753 disk-recovery class sweeps it under pressure). The addendum names "GOCACHE (and equivalents) and GOTMPDIR" — the build cache is the churn engine (5G/hour); the module cache is the steady state.
- **npm/pip/playwright caches NOT moved**: they follow `$HOME`/npm conventions; moving them means env surface per toolchain with weaker standards than Go's two vars. The disk-recovery allowlist (#1753, merged) is the sweep for those. Stated in the docs section.
- **Disk-backed, not Memory**: the addendum's explicit call — the credential tmpfs volumes are the opposite trade (capacity vs RAM).
- **SizeLimit eviction is the correct failure mode**: exceeding 8Gi evicts the pod; a cache that dies with the pod anyway has no better bound. Documented.
- **Naming**: `/sandbox-cache` + volume `build-cache` — matches the `/sandbox-*` platform namespace and leaves room for future cache relocations without a second volume.

## Tests Run

- `go test ./controller/internal/workspace/ -count=1` — FULL suite green (68s, envtest included): the pod-spec pins, envtest reconciles, cross-uid matrix, sidecar pins — no existing pin contradicts the new volume/env (all contains-style).
- `golangci-lint --new-from-merge-base` (pre-commit, new-only): 0 issues; gofmt/goimports clean; repolint clean.

## Files Modified

`controller/internal/workspace/{platform_env.go,pod_builder.go,build_cache_test.go(new)}`, `docs/operator/storage.md`, `CHANGELOG.md`, this worklog.

## Relationship to #1753

Complementary by design: #1753 is the rescue lever for what IS on the PVC (its `go-build-cache`/`gotmp-residue`/`tmp-opencode-gocache` classes now sweep LEGACY debris from pre-#1758 pods and lanes that override the env); #1758 stops the regrowth at the platform layer. #1744's allowlist stays necessary for the $HOME-convention caches (playwright, npm, envtest, golangci) the emptyDir does not cover.
