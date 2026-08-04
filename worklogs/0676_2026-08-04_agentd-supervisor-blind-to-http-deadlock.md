# workspace-agentd supervisor restarts only on process exit, not on HTTP-health failure

**Date:** 2026-08-04
**Status:** Diagnosis only — fix not yet implemented
**Severity:** Availability — wedged opencode process is never restarted by its own supervisor

---

## Objective

Document a supervision gap in `cmd/workspace-agentd` discovered during the workspace-wedge incident documented in sibling worklog `liveness-probe-wrong-target`: the `--supervise` mode triggers an opencode restart **only when the opencode process exits**. If opencode's process is alive but its HTTP server has deadlocked (listener still accepts connections, but no handler ever services a request), the supervisor cannot detect this and the workspace remains permanently wedged until an external actor (K8s liveness, the controller, a human) intervenes.

---

## Evidence

### Production pod — workspace `5c25e2ef-3f07-48f9-ae50-9769382e6da8`

Observed during the workspace-wedge incident (full timeline in `liveness-probe-wrong-target`). The relevant excerpt:

- opencode PID 38 was `State: S (sleeping)` in `do_epoll_wait`, alive and listening on `:4096`.
- agentd's healthz cache reported **38+ consecutive failures** trying to fetch `http://localhost:4096/global/health` — every attempt returned `context deadline exceeded`.
- agentd's session tracker simultaneously reported `failed to fetch connected providers` on every poll.
- `restartCount: 0` on the container — opencode never exited, agentd never restarted it.
- Pod was wedged for at least 34 minutes before user reported.

### Root cause

agentd's `managed_process.go` implements opencode supervision as:
1. Spawn opencode.
2. `Wait()` on the process.
3. If `Wait()` returns (process exited), increment `restartCount` and spawn a new opencode.

There is no path from "opencode's HTTP server is not responding" to "restart opencode". The supervisor's failure detector is process-exit only.

### Cross-reference: agentd already polls opencode's HTTP health

agentd is not blind to HTTP health entirely — `healthz_cache.go` polls `/global/health` on a 5s cadence and exposes the result via `/v1/readyz` on port 4098 (consumed by the API server and by kubelet's readiness probe). But this signal feeds **observability only**, not **restart decisions**.

So agentd knew opencode was wedged (logged `consecutiveFailures: 38`) and reported it (its own `/v1/readyz` would have returned unhealthy), but it did not act on the signal to recover.

### Cross-reference: kubelet also failed to detect this

See sibling worklog `liveness-probe-wrong-target` — kubelet's liveness probe points at agentd `/v1/healthz` (open, no auth headers), not at `/v1/readyz` (Bearer-gated, reflects opencode health). So even though agentd's readyz was unhealthy, kubelet's liveness probe was passing. The pod never got restarted by K8s either.

---

## Impact

A workspace pod whose opencode process deadlocks (goroutine leak, lock contention, runtime stall) becomes permanently unusable with no automatic recovery path. The only signals of the failure are:
- agentd logs (`consecutiveFailures` warnings, continuously emitted).
- The API server's `/v1/readyz` fetcher observing unhealthy (the controller uses this to set `AgentHealthy=false` on the CR — but the controller does not delete the pod on this signal, only annotates).

User-visible symptom: chat returns 502/504 forever; the workspace shows Active in the UX. Manual intervention is required.

This class of failure is not rare: Go HTTP services deadlock under memory pressure (long GC stalls), under goroutine leaks (long-running tools that spawn goroutines that block), and under lock contention. opencode specifically runs many concurrent goroutines for tool execution, MCP servers, and SSE streams — all of which are candidates for the deadlock we observed.

---

## Root Cause

The supervisor's failure model assumes opencode fails by exiting. It does not handle "alive but wedged" — the more common production failure mode for long-running HTTP services.

---

## Proposed Fix (not yet implemented)

Add an HTTP-health-based restart trigger to the supervisor.

### Design

In `cmd/workspace-agentd`, alongside the existing `--supervise` loop:

1. **Poll opencode's `/global/health` on a fixed cadence** (suggest 30s — matches kubelet liveness `periodSeconds`).
2. **Track consecutive failures** — the `healthz_cache.go` already does this for readyz; reuse or mirror its counter.
3. **On N consecutive failures, signal the supervisor to restart opencode.** Suggested threshold: 6 failures = 3 minutes of unresponsiveness. This is conservative enough to avoid restart loops during transient network blips and matches the liveness probe `#failure=6` convention already used in the workspace pod template.
4. **Reset the counter on first success after restart** — standard pattern.

### Code touchpoints

- `cmd/workspace-agentd/managed_process.go` — extend the supervisor to consume a `restartRequested` signal from the health watcher.
- `cmd/workspace-agentd/healthz_cache.go` — add an emission path: when `consecutiveFailures` crosses the threshold, send on the supervisor's `restartRequested` channel. (The cache already polls every 5s and tracks the counter.)
- Existing `/global/health` polling logic in `healthz_cache.go` — reuse as the failure source.
- Race safety: the supervisor's restart path must accept both the existing exit-trigger (`Wait()` returning) and the new health-trigger without racing. `managed_process_concurrency_test.go` already tests racing restart paths — extend it.

### Configuration

Make the threshold env-overridable, consistent with the existing precedent (e.g. `DISK_WARNING_THRESHOLD` in the API):
- `AGENTD_HEALTH_RESTART_FAILURES` (default 6)
- `AGENTD_HEALTH_RESTART_PERIOD_SECONDS` (default 30)

This lets operators tune for their environment without rebuilding.

---

## Alternative Considered

**Make agentd's liveness probe hit opencode, not agentd.** This is the fix in sibling worklog `liveness-probe-wrong-target`. It's necessary (it lets kubelet catch the wedge) but not sufficient — kubelet restarts the entire container, which loses in-flight agentd state (credential reload cache, session tracking) and is slower than an in-process opencode restart.

The two fixes are complementary:
- This worklog — agentd restarts only opencode, preserving agentd state. Fast recovery, in-process.
- `liveness-probe-wrong-target` — kubelet restarts the entire container as a last-resort if agentd itself wedges.

Both should ship.

---

## Risks

1. **False positives causing restart loops.** Mitigated by the conservative threshold (3 min) and bounded by the existing `maxRestarts`-style logic in the supervisor (verify it exists before implementing — if not, add a restart-rate limiter).
2. **Restarting during legitimate slow requests.** opencode's `/global/health` is fast (<10 ms typical); a 5s timeout already filters this. A 3-minute window is more than generous.
3. **Hidden bug in opencode that causes the wedge will recur after restart.** True but acceptable — restart recovers availability, which is the immediate goal; root-cause investigation continues in parallel.

---

## Files to Modify (when fix is implemented)

- `cmd/workspace-agentd/managed_process.go` — accept restart-from-health signal
- `cmd/workspace-agentd/healthz_cache.go` — emit signal on threshold crossing
- `cmd/workspace-agentd/managed_process_concurrency_test.go` — extend with health-trigger racing scenarios
- `cmd/workspace-agentd/supervisor_health_restart_test.go` (new) — end-to-end: stub opencode with a server that wedges after N requests, assert supervisor restarts it

---

## Assumptions (per Rule 7)

| Assumption | Validation |
|---|---|
| The supervisor restarts only on process exit | Confirmed by reading `managed_process.go`: restart path is driven by `Wait()` returning; no HTTP-health trigger exists |
| agentd already polls opencode HTTP health and tracks consecutive failures | Confirmed by reading `healthz_cache.go`: it polls `/global/health` every 5s, logs `consecutiveFailures`, and feeds `/v1/readyz`. The signal is already there; it just doesn't reach the supervisor. |
| A deadlocked opencode listener still accepts connections | Confirmed in the wedge incident: agentd's `dial tcp` succeeded (no "connection refused"), only the request timed out — meaning the socket was open but no handler was servicing it |
| Restarting opencode in-process preserves agentd's credential/session state | Verified by reading the existing restart path: agentd's reload cache (`last-reload-secrets.json`) and session tracking survive an opencode-only restart (this is the same path the relay injector uses when it kills opencode for config reload) |

---

## Tests Run (during diagnosis)

None — diagnosis only. Implementation tests will include:
- Stub opencode server that stops handling requests after N successful calls
- Assert supervisor restarts within `threshold * period + epsilon`
- Assert restart counter increments
- Assert restart does NOT fire on transient failures (below threshold)
- Race test: restart-from-health vs restart-from-exit occurring simultaneously

---

## Related

- **Sibling worklog `agentd-zombie-reaping-pid1`** — agentd doesn't reap zombie children (orthogonal, same incident)
- **Sibling worklog `liveness-probe-wrong-target`** — kubelet liveness probe (complementary fix)
