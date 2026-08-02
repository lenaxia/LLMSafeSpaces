# Worklog: Disk-pressure prompt injection for LLM disk-space nudges

**Date:** 2026-08-02
**Session:** Implemented the disk-space check with prompt injection at the request boundary — when a workspace's /workspace PVC is >=90% full, the API proxy prepends a notice part to LLM-bound chat requests so the agent nudges the user to free space; at >=95% the notice escalates to safe-cleanup guidance (build artifacts + caches only, logs as last resort).
**Status:** Complete

---

## Objective

Add a disk space check with two thresholds (90% and 95%) that injects a nudge into the LLM request stream. Per requirement discussion:

- At 90%: inject a notice so the LLM nudges the user to clean up disk space.
- At 95%: inject a stronger notice that suggests cleaning up ONLY easily-replaceable things (build artifacts, true temp files/caches). Logs must be the last item on the list because logs cannot be reproduced once deleted.
- The user explicitly ruled out: (a) new statusz/telemetry surfacing — disk % is already shown in the UX; (b) putting the notice into the system prompt / agent-config.json (opencode does not hot-reload config, so that would require an opencode restart); instead, inject at the request boundary, for all requests where disk pressure is >90%, without needing to persist it into history.

## Work Completed

### 1. Design

- The disk ratio is read from the **Workspace CRD status** (`status.diskUsedBytes` / `status.diskTotalBytes`), which the controller already mirrors from agentd `/v1/statusz` on its deep-status poll (~60s). This is the same data the frontend `DiskUsageBar` renders as a % — no new telemetry, no agentd changes, no opencode restart.
- Injection happens in the API proxy (`api/internal/handlers`), the single chokepoint between the user and the LLM: rewrite the request body before it reaches opencode on port 4096.
- The notice is injected as a leading `text` part (opencode has no "system" part type). Original parts and any caller-supplied `messageID` are preserved. It rides the request; opencode stores what it receives.

### 2. Pure functions — `api/internal/handlers/proxy_disk_pressure.go`

- `diskPressureRatio(used, total)` — used/total; 0 when total <= 0 (fail-safe: un-scraped workspace never trips the warning).
- `diskPressureLevelForRatio` — `none` (<90%), `warning` (>=90%), `critical` (>=95%).
- `diskPressureNotice(level, ratio)` — warning text (nudge only, no deletion authority) vs critical text (informs user, may delete ONLY easily-replaceable files with approval, logs explicitly the last resort because they cannot be reproduced).
- `injectDiskPressureNotice(body, ratio)` — parses `{parts, messageID?}`, prepends the notice part, re-marshals. Fail-open: empty/malformed body or ratio below warning passes through byte-identical.
- `workspaceDiskRatio(ctx, wsID)` — ProxyHandler helper reading the CRD status; 0 on any read error (fail-open).
- `isLLMPromptPath(path)` — gates injection to `/message` and `/prompt_async` only (session CRUD, question/permission replies, abort etc. never get it).
- Thresholds are package-level vars, env-overridable: `DISK_WARNING_THRESHOLD` (default 0.90), `DISK_CRITICAL_THRESHOLD` (default 0.95) — mirrors the memory-pressure monitor's env-override pattern.

### 3. Wiring

- `proxyToWorkspaceWithErrBody` (proxy.go): after the 10 MiB body read, before `doProxy` — `bodyBytes = injectDiskPressureNotice(bodyBytes, diskPressureRatio(workspace.Status.DiskUsedBytes, workspace.Status.DiskTotalBytes))` when the path is LLM-bound. Covers both `SendMessage` and `SendPromptAsync`.
- `sendQueuedToOpencode` (proxy_events.go): queue-drain parity — queued messages bypass `proxyToWorkspaceWithErrBody`, so the same injection is applied there (extra best-effort CRD read).

### 4. Tests (TDD)

- `proxy_disk_pressure_test.go` — unit tests for the pure functions (threshold boundaries incl. exact 90%/95%, zero/negative totals, warning vs critical text content, fail-open on malformed/empty bodies, preservation of parts + messageID) and proxy integration tests (upstream receives injected body at 90%/95%, byte-identical pass-through below 90% and on unknown disk, prompt_async injection, no injection on non-LLM paths, queue-drain injection + no-injection).

## Key Decisions

1. **Inject at the API proxy boundary, not the system prompt.** opencode reads agent-config.json only at startup (no hot reload); a prompt rewrite would need an opencode restart, killing in-flight streams. Proxy-side injection reaches the LLM on the very next request with zero disruption. This matches the user's direction ("inject before it goes to the llm").
2. **Reuse the existing disk telemetry (Workspace CRD status).** No new statusz fields, no agentd changes, no controller changes — the UX already surfaces disk %, so this feature just consumes the same mirrored data. Freshness ~60s is fine for a nudge.
3. **Injection as a leading text part.** opencode has no system-role part type in the message body; a `text` part with an explicit "System notice:" prefix is the least-surprising shape and survives the proxy→opencode contract unchanged.
4. **Fail-open everywhere.** Unknown disk state, CRD read errors, and malformed bodies all pass the request through unchanged — a telemetry hiccup must never break chat.
5. **Deletion authority only at critical, and only with user approval.** The 90% notice is a nudge; the 95% notice grants conditional cleanup of easily-replaceable files only, with logs explicitly the last resort.
6. **Thresholds env-overridable** (API env vars), consistent with the memory-pressure monitor precedent.

## Blockers

None.

## Tests Run

- `go test ./api/internal/handlers/ -run "TestDiskPressure|TestInjectDiskPressure|TestDrainQueuedMessage_DiskPressure|TestDrainQueuedMessage_DiskBelow"` — pass (new tests).
- `go test ./api/internal/handlers/ -timeout 600s` — pass (full package, 120.6s).
- `go build ./api/...` — pass.
- `go vet ./api/internal/handlers/` — pass.

## Next Steps

- PR review. Consider whether the notice text should be operator-configurable (e.g. a platform setting) — currently hardcoded constants with env-overridable thresholds only.
- Optional: surface a counter/metric for how often injection fires (e.g. `workspace_disk_pressure_injections_total`) if observability is wanted.

---

## Files Modified

- `api/internal/handlers/proxy_disk_pressure.go` (new) — pure injection functions + workspaceDiskRatio helper.
- `api/internal/handlers/proxy_disk_pressure_test.go` (new) — unit + proxy integration + queue-drain tests.
- `api/internal/handlers/proxy.go` — wired injection into `proxyToWorkspaceWithErrBody`.
- `api/internal/handlers/proxy_events.go` — wired injection into `sendQueuedToOpencode`.
- `README-LLM.md` — version 1.23; new "Disk-pressure prompt injection" subsection under Storage Settings + changelog entry.
