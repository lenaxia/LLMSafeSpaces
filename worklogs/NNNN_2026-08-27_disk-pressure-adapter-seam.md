# Worklog: disk-pressure nudges at the Adapter seam + typed disk-full (#944)

**Date:** 2026-08-27
**Session:** Land #944's spec — the nudge feature was dead on the main chat path (orphaned twice by path migrations), and disk-full sends surfaced as a bare "Failed to fetch". Implemented exactly per the issue's design: systemnotices decorator at the Adapter seam, typed 507, single wording source. Note: the issue said "implemented on a branch" — no such branch exists on origin (checked all 100+ branches, no PR); the spec is the design, so I landed it.
**Status:** Complete

---

## Objective

Every entrypoint (HTTP chat, MCP, SDK, future triggers) gets the disk-pressure nudge, and disk-full failures are typed — via the ONE seam that can't be orphaned again.

---

## Work Completed

### 1. `pkg/agent/systemnotices` decorator (new package)

- `Wrap(a agent.Adapter, usage WorkspaceDiskUsage) agent.Adapter` — intercepts ONLY `Send`/`SendAsync`, prepending `Notice` at/above the warning threshold; all other methods pure delegation (never touch the usage source — pinned by test).
- Fail-open by construction: usage read error, unknown total (≤0), below-threshold, nil usage source → text unchanged.
- `WorkspaceDiskUsage` interface; app.go wires `crdDiskUsage` (Workspace CRD status — same numbers the controller mirrors and the frontend renders; no new telemetry).
- Thresholds/level/ratio/wording live here as the single source: env overrides (`DISK_WARNING_THRESHOLD`/`DISK_CRITICAL_THRESHOLD`), normalization, floored-% display, byte-for-byte original tier text.
- Wiring: app.go wraps the opencode adapter at the single construction point (`SetAdapter(systemnotices.Wrap(...))`).

### 2. Handlers delegation (legacy raw-proxy path kept, cannot drift)

`proxy_disk_pressure.go`'s level/threshold/ratio/notice machinery replaced with thin delegations to systemnotices (`diskPressureLevel = systemnotices.Level` alias; tier tests pass unchanged through the delegation — wording preserved). The threshold-normalization + env-parsing tests moved with the logic.

### 3. Typed disk-full failure

`SendPromptAsync` adapter-error branch: CRD disk ratio at/above critical → `507 {"code":"disk_full","message":...,"diskUsedBytes":...,"diskTotalBytes":...}`. Below critical → generic 502 preserved (unrelated provider/pod errors not mislabeled). ENOSPC is undetectable from the upstream 500 body — classification uses the CRD status already in scope, per the issue's design.

---

## Key Decisions

- **Decorator over handler-level injection**: the issue's class analysis — a must-apply-to-every-message concern in a *transport* gets orphaned by every path migration (happened twice: US-63.7, #755). The Adapter seam is the only invariant.
- **Text prepend, not body rewrite**: the decorator works on the Send text (the platform-owned message), unlike the legacy raw-proxy injector which rewrites agent-shaped JSON bodies. Rule 12: no agent wire knowledge in the new code.
- **507 Insufficient Storage**: semantically correct, and distinct from 429/502 so the frontend can key inline display off `code=="disk_full"`.

## Assumptions (stated + validated)

1. No maintainer branch exists for this (checked origin branches + open PRs) — landing per spec won't collide. If a local branch surfaces, PR review reconciles.
2. The `resolveWorkspaceForAdapter` workspace carries current CRD status (controller's ~60s deep-status poll) — validated: same source the incident's DiskPressure=True condition came from; the exact-staleness bound is one poll cycle.
3. prepended `\n\n` separator matches how the frontend renders system notices in transcripts.

## Blockers

None.

## Tests Run

- `go test -race ./pkg/agent/systemnotices/` — 16 tests ok (tiers, floored %, prepend both legs, below-threshold, usage-error, zero-total, nil-source, non-message delegation ×11 methods, threshold normalization, env parsing)
- `go test -race ./api/internal/handlers/` — ok (existing wording/tier tests green through delegation + new `TestSendPromptAsync_DiskFull_ReturnsTyped507` / `..._DiskNotFull_KeepsGeneric502` red-first)
- `golangci-lint run` on all touched packages — 0 issues; build + gofmt clean

## Next Steps

- PR this branch; closes #944 (remaining follow-ups are listed on the issue: frontend banner, controller warning-tier conditions, TEST rollout verification, upstream opencode ordering bug — none block close of the platform-side fix).
- Track D remainder: #1019 options C+D (preStop drain + blockedByInFlight surfacing), #935 (Creating-phase wedge — controller recovery, needs its own session).

## Files Modified

- `pkg/agent/systemnotices/systemnotices.go`, `systemnotices_test.go` — new
- `api/internal/handlers/proxy_disk_pressure.go`, `proxy_disk_pressure_test.go` — delegation + moved tests
- `api/internal/handlers/proxy_handlers.go` — typed 507
- `api/internal/handlers/adapter_crosscutting_test.go` — 507/502 tests
- `api/internal/app/app.go` — Wrap wiring + crdDiskUsage
