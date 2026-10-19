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

### (a) RED-first pins (commit 34c40acc) — CORRECTED in review r1

`controller/internal/workspace/build_cache_test.go` — `TestGoCacheEconomy_RelocatedToEmptyDir` (env + volume + mounts). Verified RED against main @ 95c5782b: this ONE pin fails (GOCACHE absent at build_cache_test.go:35); all pre-existing workspace pins green. **Correction (review r1, verified by the reviewer's execution and admitted here): the commit's "the two new pins fail" claim was FALSE** — `TestGoCacheEconomy_SubPathMountsGuaranteeDirs` passed vacuously on main (zero `build-cache` mounts → zero assertions executed). The vacuous test is DELETED in the r1 remediation (its subPath assertion already lives, discriminating, inside the main pin) and replaced by the sidecar-mode pin below.

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

### (d) Review r1 remediation (commit on 02c5b51e)

The verdict on 02c5b51e (verifier-executed) confirmed the seam correct and findings: (1) the standing ephemeral-storage decision record was silently contradicted — RESOLVED by explicit amendment, not by restoring per-pod limits: the storage.md warning block is rewritten as the #1758 amendment (per-volume 8Gi SizeLimit bounds exactly the new surface; a container ephemeral limit would re-blend logs + writable-layer noise into one spurious-eviction trigger; scan-based enforcement documented honestly — overshoot-tolerant by design, bound pinned by test); the "What writes to node-local ephemeral storage" table gains the /sandbox-cache row; the "only consumer is container logs" claims in storage.md and pod_builder.go updated. (2) The stale pkg/diskrecovery manifest header ("GOCACHE unset") now states the #1758 reality: HOME-side go-build/gotmp classes are legacy-debris sweepers. (3) The vacuous subPath test deleted (see the (a) correction); added the discriminating `TestGoCacheEconomy_SurvivesAgentdSidecarOverlay` (sidecar-mode main container keeps GOCACHE env + both mounts). (4) Docs surfaces: mermaid gains the emptyDir node, "five mounts" → six, `---` separator restored, kubelet SafeMakeDir/0777 mechanism cited instead of bare "guaranteed". (5) Style nits: CHANGELOG double-blank. The kind-level e2e (workspace create → go install on the volume) is offered as a follow-up ruling to the reviewer — the nightly kind harness is its own audited lane.

### (e) Review r2 remediation (commit after 80c45c21)

Verifier-executed findings on 80c45c21 (both new pins verified discriminating by their execution against main @ eb3a4374): (1) the workspace-setup half of the main pin ran VACUOUSLY — newWorkspaceForSecurity carries no Packages/InitScript, so buildPod never creates the init (their instrumented count: 6 assertions executed for main, 0 for setup). Fixed the security_test.go:108-110 way: the fixture sets InitScript, and the pin now REQUIRES the init's presence so it can never degrade silently. (2) The stale "agentd-config.json" filename in the sandbox-runtime comment corrected to agent-config.json. (3) README-LLM.md's verbatim pre-#1758 ephemeral warning replaced with the amended decision (mirrors storage.md; the table gains the /sandbox-cache row). (4) TestSandboxPod_VolumeFootprint inventory gains build-cache. (5) The sidecar pin asserts GOTMPDIR's value too. The kind-level e2e: DEFERRED BY ORCHESTRATOR RULING to its own audited lane (nightly kind harness) — recorded in the PR reply; the decision record's honesty about scan-based enforcement was the finding's demand and stands.

## Tests Run

- `go test ./controller/internal/workspace/ -count=1` — FULL suite green (68s, envtest included): the pod-spec pins, envtest reconciles, cross-uid matrix, sidecar pins — no existing pin contradicts the new volume/env (all contains-style).
- `golangci-lint --new-from-merge-base` (pre-commit, new-only): 0 issues; gofmt/goimports clean; repolint clean.

## Files Modified

`controller/internal/workspace/{platform_env.go,pod_builder.go,build_cache_test.go(new)}`, `docs/operator/storage.md`, `CHANGELOG.md`, this worklog.

## Relationship to #1753

Complementary by design: #1753 is the rescue lever for what IS on the PVC (its `go-build-cache`/`gotmp-residue`/`tmp-opencode-gocache` classes now sweep LEGACY debris from pre-#1758 pods and lanes that override the env); #1758 stops the regrowth at the platform layer. #1744's allowlist stays necessary for the $HOME-convention caches (playwright, npm, envtest, golangci) the emptyDir does not cover.
