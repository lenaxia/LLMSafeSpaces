# Workspace pod liveness/readiness probes target agentd, not opencode

**Date:** 2026-08-04
**Status:** Diagnosis only — fix not yet implemented. Production pod recovered manually (suspend + resume).
**Severity:** Availability — wedged opencode goes undetected by kubelet indefinitely

---

## Summary

A production workspace pod ran an opencode process whose HTTP server deadlocked under memory pressure during a Playwright e2e run. The process stayed alive (kernel `State: S` in `do_epoll_wait`, TCP listener accepting connections) but no HTTP request was ever serviced. agentd's own polls of opencode timed out 38+ times consecutively. kubelet did not restart the pod because its liveness probe points at agentd (`:4098/v1/healthz`), which remained healthy throughout. The wedge persisted for at least 34 minutes until a human reported the chat was hung.

This worklog documents the incident and the underlying probe-target bug. Two adjacent bugs were discovered in the same investigation and are filed separately:
- **Sibling `agentd-zombie-reaping-pid1`** — agentd doesn't reap zombie children
- **Sibling `agentd-supervisor-blind-to-http-deadlock`** — agentd supervisor restarts only on process exit, not on HTTP-health failure

---

## Incident

### Workspace

- Tenant: `fcbce94d-a798-4803-af74-a0ca154ddf0a`
- Workspace: `5c25e2ef-3f07-48f9-ae50-9769382e6da8`
- Chat URL: `https://chat.safespaces.dev/chat/5c25e2ef-3f07-48f9-ae50-9769382e6da8`
- Runtime: `ghcr.io/lenaxia/llmsafespaces-images/ws:s-7a95adbcbc5eef34-0.6.0`
- Pod: `5c25e2ef-3f07-48f9-ae50-9769382e6da8-57d20880` (namespace `llmsafespaces`)
- Booted: 2026-08-04 03:36:05 UTC
- Reported wedged by user at ~04:50 UTC (≥34 min in wedged state)

### User activity leading up

Per user: "I was running playwright tests in pod". Pod process list confirmed `npm run dev` (vite) for `frontend/` and a tree of chrome-headless + pkill processes consistent with a Playwright e2e teardown.

### Memory state at time of inspection

| Metric | Value | Limit | % of limit |
|---|---|---|---|
| `memory.peak` (cgroup max ever) | 6.24 GiB | 8 GiB | 78% |
| `memory.current` (at inspection) | 821 MiB | 8 GiB | 10% |
| `memory.events: oom` | 0 | — | — |
| `memory.events: oom_kill` | 0 | — | — |
| opencode RSS (`/proc/38/VmRSS`) | 476 MiB | — | — |
| Container `restartCount` | 0 | — | — |

**Conclusion:** No kernel OOM kill occurred. The chrome processes exited normally (Playwright teardown — `pkill` invocations present in the zombie list confirm cleanup ran). Memory pressure was real (78% of limit) but did not trigger the kernel OOM killer.

### opencode process state at inspection

```
/proc/38/status: State: S (sleeping)
/proc/38/wchan: do_epoll_wait
/proc/38/stat: utime=21891 stime=4976 threads=7
```

opencode was alive, in its event loop, listening on `:4096`. But every HTTP request agentd made to it timed out:

```
{"level":"warn","msg":"readyz refresh failed","consecutiveFailures":38,
 "error":"Get \"http://localhost:4096/global/health\": context deadline exceeded"}
{"level":"warn","msg":"failed to fetch connected providers",
 "error":"Get \"http://localhost:4096/provider\": context deadline exceeded"}
```

38 consecutive 5s timeouts = at least 3 minutes of unresponsiveness at the moment of capture; total wedge duration was ≥34 minutes based on user report.

### Why kubelet didn't restart

Workspace pod template (verified on the live pod via `kubectl get pod -o yaml`):

```yaml
Liveness:   http-get http://:4098/v1/healthz delay=15s timeout=5s period=30s #success=1 #failure=6
Readiness:  http-get http://:4098/v1/readyz  delay=2s  timeout=2s period=2s  #success=1 #failure=30
Startup:    http-get http://:4098/v1/readyz  delay=1s  timeout=2s period=1s  #success=1 #failure=120
```

All three probes hit port **4098**, which is agentd's admin server. agentd remained responsive throughout the incident — its own HTTP handlers were fine; only its ability to proxy opencode was impaired.

Meanwhile the workspace CRD status condition `AgentHealthy` was correctly set to `True` because the controller's deep-status check considers agentd alive as a sufficient signal:

```yaml
- lastTransitionTime: "2026-08-04T03:27:53Z"
  message: agentd alive, uptime=4451s
  reason: AgentHealthy
  status: "True"
  type: AgentHealthy
```

(That message is stale by hours, but the underlying logic is "agentd answers → healthy", which was true.)

### Recovery

Per user direction: suspend + resume the workspace via the CRD.

```
kubectl -n llmsafespaces patch workspace 5c25e2ef-... -p '{"spec":{"suspend":true}}' --type=merge
# → phase: Suspended in 2s

kubectl -n llmsafespaces patch workspace 5c25e2ef-... -p '{"spec":{"suspend":false}}' --type=merge
# → phase: Active in 19s
```

Post-resume verification (inside the new pod):

```
=== processes (no zombies expected) ===
PID 1: workspace-agentd --supervise
PID 39: opencode serve (CPU 18% — actively serving)

=== HTTP status codes ===
/global/health:    401  ← correct (requires Bearer auth — means HTTP server is alive and routing)
/provider:         401
/config/providers: 401
/ (root):          401
```

Agentd logs showed clean startup gate sequence:
- `gate: providers_connected` @ 2.36s
- `gate: opencode_up` @ 5.00s
- `gate: readyz_first_200` @ 5.69s

PVC at `/workspace` was retained across suspend/resume; user's `LLMSafeSpaces/` repo, opencode session history (`/workspace/.local/opencode/opencode.db`), and credentials survived.

The `relay injector: failed to fetch free models, skipping` warning post-resume is unrelated — the relay probe requires auth in this configuration; the workspace operates in direct-to-Zen mode by default.

---

## Root Cause

### Primary (this worklog)

The pod template's liveness probe points at agentd's admin port (`:4098`), not at opencode (`:4096`). This means kubelet can only detect failures where agentd itself crashes — not failures where agentd is alive but opencode is wedged. That is the wrong layer of supervision for the failure mode users actually experience.

### Adjacent (filed separately)

- **Sibling `agentd-supervisor-blind-to-http-deadlock`:** agentd's in-process supervisor (`--supervise`) has the same blind spot — it restarts opencode only on process exit, not on HTTP-health failure. So neither kubelet (this worklog) nor agentd (sibling) caught the wedge.
- **Sibling `agentd-zombie-reaping-pid1`:** Compounding the diagnostic confusion, 91 un-reaped zombie processes (chrome-headless, bash, pkill) were present due to agentd-as-PID-1 not reaping adopted orphans.

---

## Impact

A deadlocked opencode process is invisible to all automated recovery mechanisms. The workspace remains in `phase: Active` with `Ready: True` indefinitely. User sees chat returning errors forever; operator sees nothing in alerts. Recovery requires manual `kubectl` intervention.

This affects every workspace pod in production today.

---

## Proposed Fix (not yet implemented)

### Background: the current probe design is intentional

The liveness/readiness/startup probe configuration is not an oversight — it is a deliberate split validated against the source:

| Probe | Path | Auth | Source |
|---|---|---|---|
| Readiness | `/v1/readyz` | `Authorization: Bearer <adminToken>` attached | `controller/internal/workspace/pod_builder.go:91-114` |
| Startup | `/v1/readyz` | `Authorization: Bearer <adminToken>` attached | `controller/internal/workspace/pod_builder.go:123-138` |
| Liveness | `/v1/healthz` | none | `controller/internal/workspace/pod_builder.go:140-148` |

The agentd admin server gates these endpoints accordingly (`cmd/workspace-agentd/server.go:194-197`):

```go
adminMux.HandleFunc("/v1/healthz", healthzHandler(...))                       // open
adminMux.Handle("/v1/readyz", requireBearerToken(adminToken, buildReadyzHandler(deps)))  // Bearer-gated
```

The comment at `server.go:177-184` makes the design explicit: `/v1/healthz` is left open **because** the kubelet liveness probe targets it without auth headers. `/v1/readyz` is Bearer-gated because it returns session metadata and provider-config (sensitive). `requireBearerToken` has no localhost exception — it checks only the `Authorization` header (`server.go:162-175`).

**Implication:** any fix that moves the liveness probe to `/v1/readyz` MUST also add `HTTPHeaders: [{Name: Authorization, Value: Bearer <adminToken>}]` to the liveness probe — matching the readiness and startup probes. Implementations that change only the path will cause kubelet to receive 401 on every liveness check → every workspace pod enters a CrashLoopBackoff-like restart storm. (This was the original recommendation in this worklog's first draft; it was wrong. Caught by the automated reviewer.)

### The actual gap

The current design is **layered but incomplete**:

- `readiness` on `/v1/readyz` (with Bearer) → reflects opencode health via `healthz_cache.go`. When opencode wedges, readiness fails → pod becomes `NotReady` → endpoints controller removes it from the workspace Service → API server can't route traffic to it.
- But `NotReady` does **not** restart the pod. For a single-replica workspace (no replica replacement path), `NotReady` just means "unreachable forever." The pod stays alive, in `Ready=False` limbo, until something else restarts it.
- `liveness` on `/v1/healthz` (no auth) → checks only that agentd's own HTTP server is up. Never reflects opencode health. So liveness never fails when opencode wedges → kubelet never restarts.

The gap: **no probe path connects "opencode wedged" to "kubelet restarts the pod."** Readiness detects it but only marks NotReady; liveness would restart but doesn't detect it.

### Recommended change

**Option A (preferred) — Add Bearer headers to the liveness probe and change its path to `/v1/readyz`.**

```yaml
LivenessProbe:
  ProbeHandler:
    HTTPGet:
      Path: /v1/readyz                                          # was /v1/healthz
      Port: 4098
      HTTPHeaders:                                              # NEW — match readiness/startup
        - Name: Authorization
          Value: "Bearer <adminToken>"
  InitialDelaySeconds: 15, PeriodSeconds: 30, TimeoutSeconds: 5, FailureThreshold: 6
```

Reuse the existing `HTTPHeaders: func() ... { if adminToken == "" ... }` pattern from readiness/startup (`pod_builder.go:96-103`). This is a ~5-line change, not a one-line change.

Risk: if `adminToken` is ever empty in production (misconfigured deploy), `/v1/readyz` becomes open and leaks session metadata. Mitigation: the `HTTPHeaders` closure returns `nil` when `adminToken == ""`, so the probe still works unauthenticated against an unauthenticated endpoint — but the endpoint itself becoming unauthenticated is the separate concern. Existing `requireBearerToken` handles this by passing through when `token == ""` (`server.go:163-165`); that is the existing behavior for readiness/startup and is acceptable for liveness too.

**Option B (alternative) — Rely on agentd's in-process supervisor (see sibling worklog `agentd-supervisor-blind-to-http-deadlock`), leave kubelet liveness as-is.**

If the supervisor gains HTTP-health-based restart, kubelet-level restart is only needed when agentd itself dies (rare). The current `/v1/healthz` liveness is sufficient for that case. This avoids touching the liveness probe entirely.

Trade-off: slower recovery (in-process restart still takes seconds), no defense if agentd itself wedges.

**Recommendation: ship both.** Option A in this worklog as kubelet-level backstop, Option B in the sibling worklog as the fast in-process recovery path. Defense in depth.

### Tuning

Current `FailureThreshold=6, PeriodSeconds=30` means 3 minutes of failures before restart. That's reasonable. `healthz_cache.go` polls opencode every 5s, so the readyz signal is at most 5s stale; the 3-minute kubelet window is more than generous.

---

## Alternative Considered (rejected)

**Add a second liveness probe directly on opencode (`:4096`).** Kubernetes does not natively support multiple HTTP liveness probes per container (cohabiting multiple `HTTPGet` actions requires multiple containers or sidecar probes). Even if it did, opencode's endpoints are all Bearer-gated and the bootstrap token is delivered via projected volume — wiring that into a probe header is more complex than reusing agentd's existing `Authorization: Bearer <adminToken>` plumbing.

---

## Files to Investigate / Modify (when fix is implemented)

- Locate the pod template: search `controller/` for where liveness/readiness probes are rendered onto the workspace Pod spec. Likely `controller/internal/workspace/phase_creating.go` or similar.
- Change `livenessProbe.httpGet.path` from `/v1/healthz` to `/v1/readyz`.
- Add a regression test asserting the liveness probe path equals readyz.
- Verify (read the agentd admin server source) that `/v1/readyz` returns non-200 when opencode is wedged — i.e. that it consults `healthz_cache.go`'s failure counter, not just agentd-local state.

---

## Lessons

1. **A liveness probe on the supervisor does not cover the supervised process.** agentd's `/v1/healthz` answers "is agentd alive", which is necessary but not sufficient. The probe must consult the supervised process's health too.
2. **Readyz and healthz must mean different things, and the liveness probe must use the right one.** `healthz` = "is this component itself running"; `readyz` = "is this component ready to serve, including its dependencies". For a supervisor, readyz is the right probe.
3. **Three layers of supervision (kubelet → agentd → opencode-internal) all assumed the lower layer would catch the failure.** None did. Defense in depth requires each layer's failure detector to cover the union, not disjoint subsets.

---

## Assumptions (per Rule 7)

| Assumption | Validation |
|---|---|
| opencode's listener stayed up while handlers were wedged | Confirmed: agentd's logs show `dial tcp` succeeded (no "connection refused"); only the request timed out via `context deadline exceeded` |
| kubelet's liveness probe was passing throughout the incident | Confirmed: `restartCount: 0` on the container; pod was `Ready: True` for the entire 34+ min wedge |
| `memory.events: oom_kill = 0` means no kernel OOM kill happened | Confirmed: cgroup v2 `memory.events` is the authoritative counter for kernel OOM kills against the cgroup |
| agentd's `/v1/readyz` reflects opencode health | **Confirmed by source read during diagnosis:** `healthz_cache.go` polls opencode `/global/health` every 5s and feeds `buildReadyzHandler` (consumed by `/v1/readyz`). However, `/v1/readyz` is Bearer-gated (`server.go:197`) — the implementing PR must add auth headers to the liveness probe, not just change the path. |
| The deadlock was triggered by memory pressure during Playwright | Inferred — not proven. Memory.peak hit 78% of limit during the run; chrome processes were torn down at end of test (zombie evidence); opencode's goroutine/lock state post-pressure is consistent with a runtime stall. Not a confirmed root cause — would need opencode profiling to confirm, which we don't have. |

---

## Tests Run (during diagnosis)

None — diagnosis only. The implementing agent should add:
- A controller test that renders the workspace pod spec and asserts `livenessProbe.httpGet.path == "/v1/readyz"`.
- An agentd test that asserts `/v1/readyz` returns 503 when `healthz_cache.consecutiveFailures > threshold`.

---

## Related

- **Sibling worklog `agentd-zombie-reaping-pid1`** — agentd doesn't reap zombie children (orthogonal, same incident)
- **Sibling worklog `agentd-supervisor-blind-to-http-deadlock`** — agentd supervisor restart path (complementary fix — in-process recovery)
