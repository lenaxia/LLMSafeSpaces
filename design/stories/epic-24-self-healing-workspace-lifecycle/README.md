# Epic 24: Self-Healing Workspace Lifecycle

> **#760 amendment (2026-09-16):** the SafeMode machinery prescribed by
> this epic (entry triggers, condition, metrics funnel, US-24.13 safe-mode
> pod) was retired before the pod side was ever built — it was a
> write-only signal. Escalation is now the derived `RecoveryExhausted`
> condition + warning Event + `WorkspaceRecoveryExhaustedTotal` counter
> with per-class thresholds (Infrastructure=10, Resource/Process=6,
> Configuration=3). See `docs/architecture/lifecycle.md`. This README is
> design history.

**Status:** Planning
**Created:** 2026-06-01
**Priority:** Critical
**Supersedes:** Epic 21 (workspace recovery state machine), Epic 23 Stories 2+3 (retry-on-conflict, single-writer)
**Depends on:** Epic 22 (shipped), Epic 23 Story 1 (shipped), Epic 23 Story 4 (shipped)

---

## Design Principles

1. **Always accessible**: A workspace is ALWAYS accessible to the user — either in normal mode or safe mode. The only terminal state is explicit user deletion.
2. **Self-healing**: The system recovers from all failures without human intervention. Unknown failure modes are handled by safe mode.
3. **Self-stabilizing**: The system converges to a healthy state regardless of the failure sequence that preceded it.
4. **No terminal failure**: The `Failed` phase is removed entirely. All failure paths lead to recovery attempts → safe mode. Safe mode is guaranteed to boot (SHA-pinned minimal image, no dependencies on user config).
5. **The controller never makes things worse**: Controller-initiated actions (health-check restarts) do not count against the workspace's failure budget.
6. **Observable**: Every state transition, failure classification, and recovery attempt is metricked and logged with sufficient context for post-hoc diagnosis.
7. **Defense in depth**: Image pinning (SHA digests) prevents runtime image breakage. Secret self-healing prevents missing-credential failures. Safe mode catches everything else.

---

## Stated Assumptions

Each assumption is verified against the current codebase (commit as of 2026-05-31 21:14).

| # | Assumption | Verification |
|---|---|---|
| A1 | Reconcile is serialized per-workspace (no concurrent reconciles of the same Workspace CR) | `SetupWithManager` uses `ctrl.NewControllerManagedBy(mgr)` with no `WithOptions(controller.Options{MaxConcurrentReconciles: N})` override → defaults to 1. Verified: `controller/internal/workspace/controller.go:1130`, `controller/internal/controller/controller.go:22-28` |
| A2 | Five paths currently write `Failed`: PendingTimeout (L154), PVCBindTimeout (L184), PodBuildFailed (L225), PodFailedDuringCreation (L276), TransientPodLoss (L528) | Verified: `grep -n "markFailed" controller.go` returns exactly these 5 sites |
| A3 | `MaxTransientFailures = 3` with `healthCheckInterval = 15s` means 3 pod losses in 5 minutes → terminal | Verified: `constants.go:24,28` — `MaxTransientFailures = 3`, `TransientFailureResetWindow = 5 * 60` |
| A4 | `checkAgentHealth` deletes the pod on 3 consecutive health failures (L1247-1256, L1274-1283), sets phase=Creating, and resets ConsecutiveHealthFailures to 0. The next reconcile enters `handleCreating` (not `handleActive`) because phase is now Creating. | Verified: `controller.go:1247-1256` deletes pod + sets phase=Creating + resets counter; next reconcile dispatches on phase at L63 |
| A5 | Controller-initiated pod deletion (health restart) does NOT currently trigger `recoverFromTransientPodLoss` because it sets phase=Creating before returning. However, if the NEW pod also fails (e.g., same network issue persists), `handleCreating` → pod enters Failed phase → `markFailed(PodFailedDuringCreation)` which IS terminal. The indirect path to terminal failure from health-check restarts is: health restart → new pod fails → terminal. | Verified: `controller.go:1252` sets phase=Creating; `handleCreating:276` calls markFailed on PodFailed |
| A6 | Resource limits = requests (QoS=Guaranteed): `Limits: rl, Requests: rl.DeepCopy()` | Verified: `controller.go:891-894` — `resourceRequirementsFor` returns identical Limits and Requests |
| A7 | Default resources: 500m CPU, 512Mi memory, 1Gi ephemeral storage | Verified: `controller.go:870-872` — `defaultCPU = "500m"`, `defaultMemory = "512Mi"`, `defaultEphemeral = "1Gi"` |
| A8 | Kubelet liveness probe: `/v1/healthz` on port 4098, InitialDelay=15s, Period=30s, Timeout=5s, FailureThreshold=6 → kills pod after ~3 min of failures | Verified: `controller.go:793` |
| A9 | Kubelet readiness probe: `/v1/readyz` on port 4098, InitialDelay=10s, Period=15s, Timeout=3s, FailureThreshold=5 → marks NotReady after ~75s | Verified: `controller.go:784` |
| A10 | No production users; CRD field changes have no backwards-compatibility constraints | Stated by user (Epic 21 A7 precedent) |
| A11 | `recoverFromTransientPodLoss` is called from 3 sites in `handleActive`: pod NotFound (L318), pod phase != Running (L334), CrashLoopBackOff (L356) | Verified: `grep -n "recoverFromTransientPodLoss" controller.go` |
| A12 | The controller's `healthCheckGracePeriod = 30s` prevents health checks during pod startup | Verified: `controller.go:958` — `healthCheckGracePeriod = 30 * time.Second` |
| A13 | `handleCreating` transitions to Active as soon as pod is Running + has IP, without waiting for readiness probe to pass | Verified: `controller.go:258-268` — checks `PodRunning && PodIP != ""`, no readiness gate |

### Hypotheses Considered and Refuted

| # | Hypothesis | Refutation |
|---|---|---|
| R1 | "QoS=Guaranteed prevents OOM kills" | False. Memory limit is a hard cgroup cap enforced by the kernel regardless of QoS class. QoS only affects `oom_score_adj` (eviction priority under node pressure). The fix is raising the limit, not changing QoS class. |
| R2 | "Epic 21 Change B (exponential backoff) solves workspace stability" | Partially. It delays terminal failure but doesn't prevent it. The correct design removes the terminal path entirely for transient causes. |
| R3 | "CrashLoopBackOff is always transient" | False. CrashLoopBackOff from a broken runtime image or missing dependency is permanent. The controller must distinguish infrastructure-caused crashes from code/config-caused crashes. |

---

## Design Questions (Must Answer Before Implementation)

| # | Question | Answer | Rationale |
|---|---|---|---|
| DQ1 | Should `Failed` phase be removed entirely from the CRD? | **Yes.** All failure paths lead to recovery → safe mode. Safe mode is Active (workspace accessible). The only terminal state is Terminated (user deleted). `WorkspacePhaseFailed` const kept as deprecated for API compatibility but never set by the controller. | Safe mode guarantees the workspace is always accessible. There is no failure that justifies making a workspace permanently inaccessible. |
| DQ2 | What memory limit should Burstable workspaces get? | **4× the request by default** (request=512Mi, limit=2Gi). Configurable via `spec.resources.memoryLimit`. | 4× provides headroom for build/test spikes. The webhook's `MaxMemoryMi` cap still applies to the limit. |
| DQ3 | Should CPU also be Burstable? | **Yes.** Request=500m, Limit=2000m (4× burst). CPU throttling (not kill) is the consequence of exceeding limit on CPU, so the risk is lower than memory. | CPU overcommit is standard K8s practice. Throttling degrades performance but doesn't kill the pod. |
| DQ4 | How many retries for infrastructure failures before any human signal? | **Unlimited.** Infrastructure failures (node eviction, preemption) are never the workspace's fault. Backoff caps at 2 minutes between attempts. | A workspace should survive a 30-minute node maintenance window without human intervention. |
| DQ5 | How many retries for process failures (CrashLoopBackOff, agentd unhealthy)? | **12 attempts** with exponential backoff (10s → 20s → 40s → ... → cap at 10 min). Total budget: ~2 hours before entering `Degraded` (not `Failed`). | 12 attempts with 10-min cap gives the system ~2 hours to self-heal. If it hasn't recovered by then, something is genuinely wrong — but it's still not terminal. |
| DQ6 | What is `Degraded`? | **Not a new phase.** It's a Condition (`WorkspaceConditionDegraded = True`) on an Active or Creating workspace. The workspace remains in the recovery loop with maximum backoff. User sees "workspace is experiencing issues, recovering automatically." | Adding a new phase would complicate the state machine. A Condition is the K8s-idiomatic way to express a non-terminal degradation. |
| DQ7 | Should controller-initiated restarts (from `checkAgentHealth`) increment any counter? | **Yes — `RestartCount` (observability) but NOT `ConsecutiveFailures` (failure budget).** Currently, health-check restarts set phase=Creating and reset ConsecutiveHealthFailures. If the replacement pod also fails to start, `handleCreating` can call `markFailed(PodFailedDuringCreation)` — which is terminal. Epic 24 makes `PodFailedDuringCreation` non-terminal for transient causes (image pull backoff, node scheduling delay). | The controller chose to restart the pod; downstream failures from that restart should be classified and retried, not immediately terminal. |
| DQ8 | When does `TransientFailureCount` reset? | **After 2 minutes of continuous Active phase with healthy agent.** | 2 minutes of stability proves the last failure was transient. The current 5-minute window is too conservative. |
| DQ9 | Should the file split happen in the same epic or separately? | **Same epic.** The split is prerequisite for proper unit testing of the new recovery logic. | Testing recovery policies in isolation requires the recovery logic to be in its own file with injectable dependencies. |
| DQ10 | What about Epic 23 Story 2 (retry-on-conflict) and Story 3 (single-writer)? | **Deferred.** Epic 23 Story 1's metrics (`WorkspaceStatusUpdateConflictsTotal`) will reveal whether these are needed. If conflict rate is >10/day after Epic 24 ships, they become Epic 25. | Data-driven decision. Don't add complexity for an unproven problem. |

---

## Domains

### Domain 1: Resource Management (QoS)
- Pod resource requirements (request vs limit)
- Burstable QoS semantics
- Webhook validation of new limit fields
- CRD schema changes for separate request/limit

### Domain 2: Failure Classification
- Typed failure taxonomy (Infrastructure, Resource, Process, Configuration)
- Pod observation and classification logic
- Per-class recovery policies

### Domain 3: Recovery State Machine
- Backoff calculation and enforcement
- Stability reset logic
- Controller-initiated restart vs external failure distinction
- Transition rules (what can reach Failed, what cannot)

### Domain 4: Health Management
- Readiness gate before health checking
- Health check → restart → recovery flow
- Separation of liveness restart from failure counting

### Domain 5: Controller Architecture
- File decomposition for testability
- Interface extraction for dependency injection
- Reconciler structure

### Domain 6: Observability
- Prometheus metrics for failure classification, recovery attempts, backoff state
- Structured logging at decision points
- Condition management on the CRD

---

## Scope

### In Scope

| # | What | Domain |
|---|---|---|
| 1 | Burstable QoS: separate request/limit with 4× burst headroom | Resource Management |
| 2 | Failure classification enum and pod observation logic | Failure Classification |
| 3 | Per-class recovery policies (unlimited infra, capped process, immediate config) | Recovery State Machine |
| 4 | Remove `Failed` as reachable from transient/infrastructure/process causes | Recovery State Machine |
| 5 | Controller-initiated restarts don't count as failures | Recovery State Machine |
| 6 | Stability reset at 2 minutes | Recovery State Machine |
| 7 | Readiness gate before health checking | Health Management |
| 8 | Controller file split into focused modules | Controller Architecture |
| 9 | Interface extraction for health checker and pod observer | Controller Architecture |
| 10 | Prometheus metrics for all recovery decisions | Observability |
| 11 | SSE tracker context timeout to prevent goroutine leak | Health Management |

### Out of Scope

| # | What | Why |
|---|---|---|
| 1 | Status().Update retry-on-conflict (Epic 23 Story 2) | Deferred pending conflict-rate metrics |
| 2 | Single-writer migration (Epic 23 Story 3) | Deferred pending conflict-rate metrics |
| 3 | Warm pool / pre-provisioned pods | Different epic (performance, not robustness) |
| 4 | Hot migration (Epic 18) | Independent concern |

---

## CRD Changes

### New fields on `WorkspaceSpec`

```go
type ResourceRequirements struct {
    // Request fields (scheduler reservation)
    CPU              string `json:"cpu,omitempty"`              // default "500m"
    Memory           string `json:"memory,omitempty"`           // default "512Mi"
    EphemeralStorage string `json:"ephemeralStorage,omitempty"` // default "1Gi"
    // Limit fields (hard cap — OOM kill boundary for memory)
    CPULimit         string `json:"cpuLimit,omitempty"`         // default "2000m" (4× request)
    MemoryLimit      string `json:"memoryLimit,omitempty"`      // default "2Gi" (4× request)
}
```

### New fields on `WorkspaceStatus`

```go
// FailureClass is the typed classification of why recovery was triggered.
type FailureClass string
const (
    FailureClassNone           FailureClass = ""
    FailureClassInfrastructure FailureClass = "Infrastructure" // node eviction, preemption, pod vanished
    FailureClassResource       FailureClass = "Resource"       // OOM kill, ephemeral storage eviction
    FailureClassProcess        FailureClass = "Process"        // CrashLoopBackOff, agentd unhealthy
    FailureClassConfiguration  FailureClass = "Configuration"  // bad image, missing secret, build error
)

type WorkspaceStatus struct {
    // ... existing fields ...

    // Recovery state (replaces TransientFailureCount / LastTransientFailureAt)
    ConsecutiveFailures    int32        `json:"consecutiveFailures,omitempty"`
    LastFailureClass       FailureClass `json:"lastFailureClass,omitempty"`
    LastFailureAt          *metav1.Time `json:"lastFailureAt,omitempty"`
    NextRetryAt            *metav1.Time `json:"nextRetryAt,omitempty"`
    LastStableAt           *metav1.Time `json:"lastStableAt,omitempty"` // last time workspace was healthy for >2min
    ControllerRestartCount int32        `json:"controllerRestartCount,omitempty"` // restarts initiated by health checks (not failures)
}
```

### Removed fields

```go
// REMOVED (replaced by ConsecutiveFailures + LastFailureAt):
// TransientFailureCount     int32
// LastTransientFailureAt    *metav1.Time
```

---

## Recovery Policy Design

```go
type RecoveryPolicy struct {
    MaxAttempts      int           // 0 = unlimited
    BackoffBase      time.Duration // first retry delay
    BackoffMax       time.Duration // cap on exponential growth
    BackoffFactor    int           // multiplier per attempt (2 = doubling)
    StabilityReset   time.Duration // how long healthy before counter resets
    SafeModeAfter    int           // enter safe mode after this many failures (0 = never)
}

// #760 (2026-09-16): SafeModeAfter and this whole SafeMode contract were
// RETIRED. The live policy table replaces SafeModeAfter with
// ExhaustionAfter (Infrastructure=10, Resource/Process=6,
// Configuration=3) — a derived RecoveryExhausted condition + Event +
// counter, no mode, no gate. See docs/architecture/lifecycle.md
// ("Recovery exhaustion"). The snippets below are design history.

var recoveryPolicies = map[FailureClass]RecoveryPolicy{
    FailureClassInfrastructure: {
        MaxAttempts:    0,              // unlimited — not the workspace's fault
        BackoffBase:    5 * time.Second,
        BackoffMax:     2 * time.Minute,
        BackoffFactor:  2,
        StabilityReset: 2 * time.Minute,
        SafeModeAfter:  0,             // never — infra issues resolve themselves
    },
    FailureClassResource: {
        MaxAttempts:    0,              // unlimited — safe mode is the escape hatch
        BackoffBase:    10 * time.Second,
        BackoffMax:     5 * time.Minute,
        BackoffFactor:  2,
        StabilityReset: 2 * time.Minute,
        SafeModeAfter:  6,             // after 6 resource failures, enter safe mode
    },
    FailureClassProcess: {
        MaxAttempts:    0,              // unlimited — safe mode is the escape hatch
        BackoffBase:    10 * time.Second,
        BackoffMax:     5 * time.Minute,
        BackoffFactor:  2,
        StabilityReset: 2 * time.Minute,
        SafeModeAfter:  6,             // after 6 process failures, enter safe mode
    },
    FailureClassConfiguration: {
        MaxAttempts:    0,              // unlimited — safe mode is the escape hatch
        BackoffBase:    30 * time.Second,
        BackoffMax:     5 * time.Minute,
        BackoffFactor:  2,
        StabilityReset: 2 * time.Minute,
        SafeModeAfter:  3,             // after 3 config failures, enter safe mode (fast)
    },
}
```

### State Transition Rules (revised — no terminal `Failed` for ANY class)

```
ANY failure class → retry with backoff → after SafeModeAfter attempts → enter Safe Mode

Safe Mode means:
  - Phase = Active (workspace IS accessible)
  - Condition WorkspaceConditionSafeMode = True
  - Minimal alpine-based pod (hardcoded image, guaranteed to boot)
  - PVC mounted read-only (corrupt data can't crash it)
  - No init containers (skip package install, credential setup)
  - No opencode/agentd (the things that crash)
  - Simple HTTP server for healthz + diagnostic endpoint
  - Terminal/shell access still works via proxy
  - User can see files, salvage data, diagnose issues
  - spec.restartGeneration bump exits safe mode → retries normal mode

"Failed" phase is REMOVED from the state machine entirely.
  - buildPod() errors → enter safe mode (not Failed)
  - PVC bind timeout → enter safe mode with diagnostic message
  - The only truly terminal state is Terminated (user explicitly deleted)
```

### Safe Mode Design

**When**: The runtime pod CANNOT run (crashes, image broken, init fails). The pod is not starting.
**Action**: Replace with minimal fallback pod. User gets shell + file access.
**Phase**: Active (with SafeMode condition)

**Image**: A purpose-built minimal image baked into the controller binary as a constant. Alpine + busybox + a tiny Go HTTP server that serves `/v1/healthz` (always OK) and `/v1/diagnostic` (JSON with failure reason, last error, attempt count). This image is:
- Pinned by SHA digest (immutable, can't be broken by tag push)
- Pulled from the same registry as the runtime images (no additional registry dependency)
- ~15MB (alpine base + static Go binary)
- Guaranteed to boot because it does almost nothing

**Pod spec in safe mode**:
```go
func (r *WorkspaceReconciler) buildSafeModePod(workspace *v1.Workspace) *corev1.Pod {
    // Hardcoded safe-mode image (SHA-pinned)
    // PVC mounted — read-write if disk pressure triggered safe mode,
    //               read-only if process crash triggered safe mode,
    //               omitted entirely if PVC never bound (P3)
    // No init containers
    // No password secret mount (may be missing)
    // Minimal resources (100m CPU, 64Mi memory)
    // Same security context (non-root, read-only root fs, drop all caps)
    // Single container: safe-mode-server
    //   - Serves /v1/healthz (always 200)
    //   - Serves /v1/diagnostic (JSON: why safe mode, last error, attempts)
    //   - Exposes shell via /bin/sh for terminal proxy
    // Controller skips checkAgentHealth and enrichAgentStatus for safe-mode pods
}
```

**Entering safe mode**:
- `ConsecutiveFailures >= policy.SafeModeAfter`
- OR `ControllerRestartCount` exceeds threshold without stability (A13 — persistent unreachability)
- Set `workspace.Status.SafeMode = true`
- Set condition `WorkspaceConditionSafeMode = True` with message
- `handleCreating` calls `buildSafeModePod` instead of `buildPod`
- Pod boots → transitions to Active (safe mode IS active — workspace is accessible)

**Exiting safe mode**:
- User bumps `spec.restartGeneration`
- Controller clears `SafeMode = false`, clears recovery state
- Normal pod creation attempted
- If it fails again → re-enters recovery loop → safe mode again after N attempts

### Degraded Mode Design

**When**: The runtime pod IS running and healthy, but the workspace is not fully functional. The pod should NOT be restarted — that won't help.
**Action**: Detect via application-level signals, set Condition, surface to user. Keep pod running.
**Phase**: Active (with Degraded condition — NOT a phase change)

**Degraded signals (detected by `enrichAgentStatus` on 60s cadence):**

| Signal | Detection | Condition Set | User Message | Auto-Recoverable? |
|---|---|---|---|---|
| Disk >95% full | `statusz.Disk.UsedBytes / TotalBytes > 0.95` | `WorkspaceConditionDiskPressure = True` | "Workspace disk almost full. Free space or files may not save." | No — user must delete files |
| Disk 100% full | `statusz.Disk.UsedBytes >= TotalBytes` | `WorkspaceConditionDiskPressure = True` (critical) | "Workspace disk full. Writes are failing. Delete files to recover." | No — user must delete files |
| No LLM providers connected | `statusz.Connected = [] && ProvidersConfigured > 0` | `WorkspaceConditionAgentHealthy = False` (reason: AgentDegraded) | "No LLM provider connected. Check credentials." | No — user must refresh credentials |
| Pod NotReady (kubelet) | Pod condition `Ready = False` while phase=Active | `WorkspaceConditionAgentHealthy = Unknown` | "Workspace temporarily unreachable. Retrying automatically." | Yes — usually resolves when load drops |

**Key difference from safe mode**: Degraded does NOT restart the pod. The pod is running and the user may still have partial functionality (terminal works, file access works, just LLM calls fail). Restarting would destroy the user's running session for no benefit.

**Controller behavior for Degraded workspaces**:
- Continue normal `handleActive` reconcile loop
- Continue `enrichAgentStatus` (to detect when degradation resolves)
- Do NOT trigger `checkAgentHealth` pod restarts for application-level degradation
- If the degradation resolves (disk freed, credentials refreshed, providers reconnect), clear the condition automatically on next `enrichAgentStatus` cycle

**Frontend behavior**:
- Yellow banner for Degraded conditions with actionable message
- Red banner for SafeMode with "Retry" button + "Download files" button
- Both banners include the specific reason and what the user can do

### Image Pinning

Runtime images resolved via RuntimeEnvironment CRD should use SHA digests, not tags:
- `RuntimeEnvironmentSpec.Image` should be `ghcr.io/lenaxia/workspace-python@sha256:abc123...`
- This prevents "tag was overwritten with broken image" failures
- The webhook already validates image format; add digest validation
- RuntimeEnvironment updates (new image version) are an explicit operator action

### Secret Self-Healing

`ensurePasswordSecret` currently only runs in `handlePending` and `handleResuming`. If the secret is deleted while the workspace is in Creating phase (during recovery backoff), the next pod creation fails because the volume mount can't be satisfied.

Fix: `handleCreating` must call `ensurePasswordSecret` before creating a pod. This makes secret creation idempotent and self-healing — if the secret is missing for any reason, it's recreated on the next attempt.

---

## User Stories

| Story | Title | Domain | Depends On | Key Acceptance Criteria (from critical review) |
|---|---|---|---|---|
| US-24.1 | Controller file split: extract phase handlers, health, recovery, pod builder, secrets, PVC into separate files | Architecture | None | All 83 existing tests pass; no import cycles. **F46**: `checkAgentHealth` decomposed into observation (health.go returns result) and action (phase_active.go decides/acts). No file both makes HTTP calls AND modifies workspace status. |
| US-24.2 | Interface extraction: HealthChecker only (PodObserver and RecoveryEngine are plain functions) | Architecture | US-24.1 | **F38**: Only `HealthChecker` interface (avoids real HTTP in tests). Classification and recovery are pure functions — no interface needed (YAGNI). |
| US-24.3 | Burstable QoS: separate request/limit fields, 4× default burst, webhook validation | Resource Mgmt | None | **F10**: empty MemoryLimit + set Memory → limit=4×request. Webhook rejects limit < request. **F48**: `MaxCPUMillicores` caps `cpuLimit`, `MaxMemoryMi` caps `memoryLimit`. Requests implicitly bounded by limit >= request. |
| US-24.4 | FailureClass enum + pod observation → classification logic | Failure Classification | US-24.1 | **F4**: explicit classification for PodFailed reasons: Evicted→Infra, OOMKilled→Resource, ImagePullBackOff→Config, Error+exitcode→Process, Unschedulable→Infra. **F30**: unknown reasons default to `FailureClassProcess`. Classifier never returns `FailureClassNone` for a failed pod. |
| US-24.5 | RecoveryPolicy per class with exponential backoff | Recovery | US-24.4 | **F1/F16**: `handleCreating` checks NextRetryAt in the "pod not found" branch (NOT at top — must first check if existing pod recovered). **FN3**: pod in K8s Pending >5min → classify as Infra, enter backoff — BUT only if pod is Unschedulable. If pod is scheduled and pulling image (ContainerCreating), do NOT timeout (C17 fix). **F19**: `handleCreating` checks `restartGeneration` — if bumped, clear all recovery state and create pod immediately (bypass backoff). **F34**: counter is class-agnostic; MaxAttempts cap uses CURRENT class (Infra=unlimited never triggers Degraded). **F39**: cap shift exponent at 30 to prevent time.Duration overflow. **F49**: when handleCreating observes Failed pod, DELETE it before setting recovery state (prevents counter increment loop). |
| US-24.6 | Remove `Failed` phase entirely; all failure paths lead to recovery or safe mode | Recovery | US-24.5, US-24.13 | `Failed` phase removed from state machine. `markFailed` helper removed. All former Failed paths now enter recovery loop → safe mode. API `EnsureSession`/`waitForActive` detect SafeMode condition and return structured response. `WorkspacePhaseFailed` const kept but deprecated (never set). |
| US-24.7 | Controller-initiated restarts (checkAgentHealth) don't increment failure counter | Recovery | US-24.5 | **F9**: `RestartCount` = total (all causes). `ControllerRestartCount` = health-check subset. Both increment on health restart. |
| US-24.8 | Stability reset at 2 minutes of healthy Active | Recovery | US-24.5 | `LastStableAt` set when workspace healthy for 2 continuous minutes; `ConsecutiveFailures` reset to 0. **F28**: handle `LastStableAt = nil` (fresh/post-suspend): start clock on first healthy reconcile; explicit nil check before time comparison. **F22**: `handleSuspending` clears all recovery state (ConsecutiveFailures, NextRetryAt, LastFailureClass, LastFailureAt, Degraded condition). **F43**: stability reset also removes `WorkspaceConditionDegraded` (workspace recovered naturally). |
| US-24.9 | Readiness gate: don't health-check pods that haven't passed kubelet readiness | Health | US-24.1 | Check `ContainerStatuses[].Ready` before running `checkAgentHealth` |
| US-24.10 | SSE tracker context timeout (5-min deadline, reconnect on expiry) | Health | None | Context with 5-min deadline; on expiry, cancel + reconnect with backoff |
| US-24.11 | Prometheus metrics: failure_class counter, recovery_attempts histogram, backoff_seconds gauge, degraded_workspaces gauge | Observability | US-24.5 | All metrics registered; labels match FailureClass enum values |
| US-24.12 | CRD schema update: new status fields, removed fields, deepcopy regeneration | All | US-24.4, US-24.5 | New fields present in CRD YAML + deepcopy; old fields removed; `kubectl apply` works. **F36**: remove `Spec.MaxRetries` (replaced by per-class policies). Remove `Failed` phase entirely from CRD. Add `SafeMode bool` to status. |
| US-24.13 | Safe Mode: fallback pod with minimal image when normal recovery exhausted | Recovery | US-24.5 | Safe-mode image is SHA-pinned constant in controller. `buildSafeModePod` produces: alpine + diagnostic server, PVC conditional mount (read-write if disk pressure, read-only if process crash, omitted if PVC never bound), no init containers, no opencode. Pod boots → phase=Active + SafeMode condition. `restartGeneration` bump exits safe mode. Terminal handler works as-is (phase=Active, pod has /bin/sh). Controller skips `checkAgentHealth`/`enrichAgentStatus` for safe-mode pods. Also triggered by `ControllerRestartCount > 5` without stability (persistent unreachability, A13). **Auto-suspend disabled when SafeMode=true** (no activity tracking in safe mode; auto-suspending would strand user's data). **TTL-after-suspend disabled if workspace was in safe mode before suspension** (user needs time to download files). |
| US-24.14 | Image pinning: RuntimeEnvironment images use SHA digests; controller records resolved digest at workspace creation | Resilience | US-24.1 | `resolveRuntimeImage` stores resolved digest in `status.resolvedImageDigest`. Subsequent pod creations use the pinned digest, not re-resolve. Operator updates RuntimeEnvironment → new workspaces get new digest; existing workspaces keep their pinned digest until `restartGeneration` bump. |
| US-24.15 | Secret self-healing: `handleCreating` calls `ensurePasswordSecret` before pod creation | Resilience | US-24.1 | If password secret is missing (deleted during recovery, operator error), it's recreated before pod creation. Idempotent. Test: delete secret while workspace is in backoff → next retry recreates it and succeeds. |
| US-24.16 | Safe Mode file download: API endpoint to tar and stream workspace directory | Recovery UX | US-24.13 | `GET /workspaces/:id/files/download?path=/workspace` → streams tar.gz of the requested path from the safe-mode pod via `kubectl exec tar -czf - <path>`. Frontend shows "Download files" button when SafeMode condition is True. Requires phase=Active + SafeMode condition (rejected otherwise). Auth: same ownership check as terminal handler. |
| US-24.17 | Degraded detection: application-level health signals surfaced as conditions | Observability | US-24.1 | `enrichAgentStatus` detects: disk >95% → DiskPressure condition; no providers connected → AgentDegraded condition; pod NotReady while Active → structured 503 from proxy. Conditions auto-clear when signal resolves. Frontend shows yellow banner with actionable message. Controller does NOT restart pods for Degraded conditions. |
| US-24.18 | Frontend: SafeMode and Degraded banners with actionable UX | Frontend | US-24.13, US-24.17 | Frontend reads `conditions` array from workspace status API. Red banner for SafeMode: "{reason}. Your files are accessible. [Retry Normal Mode] [Download Files]". Yellow banner for Degraded (DiskPressure, AgentDegraded): "{message}. [specific action]". Banners dismiss when condition clears. Requires adding `conditions` field to frontend `WorkspaceStatus` TypeScript type. |

### Dependency Graph

```
US-24.1 (file split) ──┬── US-24.2 (interfaces)
                        ├── US-24.4 (FailureClass) ── US-24.5 (RecoveryPolicy) ──┬── US-24.6 (remove Failed, safe mode integration)
                        │                                                         ├── US-24.7 (controller restart ≠ failure)
                        │                                                         ├── US-24.8 (stability reset)
                        │                                                         └── US-24.11 (metrics)
                        ├── US-24.9 (readiness gate)
                        ├── US-24.14 (image pinning)
                        └── US-24.15 (secret self-healing)

US-24.3 (Burstable QoS) ── independent, can ship first
US-24.10 (SSE timeout) ── independent
US-24.13 (Safe Mode) ── depends on US-24.5 (needs recovery policy to know when to trigger)
US-24.16 (File Download) ── depends on US-24.13 (only available in safe mode)
US-24.12 (CRD schema) ── depends on US-24.4, US-24.5, US-24.13
```

### Critical Path

```
US-24.3 → (immediate stability win, ship first)
US-24.1 → US-24.4 → US-24.5 → US-24.13 (safe mode) → US-24.6 (remove Failed)
US-24.15 (secret self-healing) can ship with US-24.1
US-24.14 (image pinning) can ship with US-24.1
US-24.12 (CRD schema) ships last (after all status field changes are finalized)
```

---

## Test Plan

### Unit Tests (per story)

| Story | Test | What It Proves |
|---|---|---|
| US-24.1 | Existing 83 controller tests pass after file split | No regression from refactoring |
| US-24.3 | `TestResourceRequirements_BurstableDefaults` | Request < Limit, correct 4× ratio |
| US-24.3 | `TestResourceRequirements_CustomLimits` | User-specified limits override defaults |
| US-24.3 | `TestResourceRequirements_LimitBelowRequest_Rejected` | Webhook rejects limit < request |
| US-24.4 | `TestClassifyFailure_PodNotFound` → Infrastructure | Pod vanished = infra |
| US-24.4 | `TestClassifyFailure_OOMKilled` → Resource | OOM = resource |
| US-24.4 | `TestClassifyFailure_CrashLoopBackOff` → Process | Crash = process |
| US-24.4 | `TestClassifyFailure_ImagePullBackOff` → Configuration | Bad image = config |
| US-24.4 | `TestClassifyFailure_Evicted` → Infrastructure | Eviction = infra |
| US-24.5 | `TestRecoveryPolicy_InfrastructureUnlimited` | Infra never reaches Failed |
| US-24.5 | `TestRecoveryPolicy_ProcessCapped` | Process reaches Degraded at attempt 12 |
| US-24.5 | `TestRecoveryPolicy_ConfigurationImmediate` | Config fails on first attempt |
| US-24.5 | `TestBackoffCalculation_ExponentialWithCap` | Backoff doubles, caps at max |
| US-24.5 | `TestBackoffCalculation_FirstAttempt` | First retry uses BackoffBase |
| US-24.5 | `TestBackoffCalculation_HighFailureCount_NoOverflow` | ConsecutiveFailures=100 → BackoffMax, no negative duration (F39) |
| US-24.5 | `TestRecoveryPolicy_InfraAtHighCount_NeverDegraded` | ConsecutiveFailures=50 + Infrastructure → no Degraded (F34) |
| US-24.6 | `TestTransientPodLoss_NeverReachesFailed` | 100 consecutive infra failures → still Creating, never Failed |
| US-24.6 | `TestProcessFailure_ReachesDegraded_NotFailed` | 12 process failures → Degraded condition, phase still Creating |
| US-24.6 | `TestConfigFailure_ReachesFailed` | PodBuildFailed → immediate Failed |
| US-24.7 | `TestControllerRestart_DoesNotIncrementFailureCount` | Health-check pod delete → TransientFailureCount unchanged |
| US-24.7 | `TestControllerRestart_IncrementsRestartCount` | Health-check pod delete → RestartCount++ |
| US-24.8 | `TestStabilityReset_After2Minutes` | 2 min healthy → ConsecutiveFailures = 0 |
| US-24.8 | `TestStabilityReset_NotBefore2Minutes` | 1 min healthy → ConsecutiveFailures preserved |
| US-24.9 | `TestReadinessGate_NoHealthCheckBeforeReady` | Pod Running but not Ready → no health check |
| US-24.9 | `TestReadinessGate_HealthCheckAfterReady` | Pod Running + Ready → health check runs |
| US-24.10 | `TestSSETracker_ReconnectsOnTimeout` | 5-min context expires → reconnection attempt |
| US-24.11 | `TestMetrics_FailureClassIncrement` | Each failure class increments its counter |
| US-24.11 | `TestMetrics_RecoveryAttemptHistogram` | Recovery attempts recorded with class label |
| US-24.4 | `TestClassifyFailure_UnknownReason_DefaultsToProcess` | Unknown pod failure reason → Process class (F30) |
| US-24.8 | `TestStabilityReset_NilLastStableAt_StartsClockOnHealthy` | Fresh workspace, first healthy reconcile starts 2-min clock (F28) |
| US-24.8 | `TestSuspend_ClearsAllRecoveryState` | Suspend clears ConsecutiveFailures, NextRetryAt, LastFailureClass, Degraded (F22) |
| US-24.5 | `TestHandleCreating_RestartGeneration_BypassesBackoff` | restartGeneration bump during backoff → immediate pod creation (F19) |
| US-24.13 | `TestSafeMode_TriggeredAfterNFailures` | 6 Process failures → safe mode pod created |
| US-24.13 | `TestSafeMode_PodBootsSuccessfully` | Safe-mode pod reaches Running → phase=Active + SafeMode condition |
| US-24.13 | `TestSafeMode_RestartGeneration_ExitsSafeMode` | restartGeneration bump → SafeMode cleared, normal pod attempted |
| US-24.13 | `TestSafeMode_ReadOnlyPVC` | Safe-mode pod mounts PVC as read-only |
| US-24.13 | `TestSafeMode_NoInitContainers` | Safe-mode pod has zero init containers |
| US-24.13 | `TestSafeMode_ConfigFailure_FastEntry` | 3 Configuration failures → safe mode (faster than Process) |
| US-24.14 | `TestImagePinning_DigestStoredOnFirstResolve` | First pod creation stores resolved digest in status |
| US-24.14 | `TestImagePinning_SubsequentCreationsUseStoredDigest` | Second pod creation uses stored digest, not re-resolves |
| US-24.14 | `TestImagePinning_RestartGeneration_ReResolvesImage` | restartGeneration bump clears stored digest, re-resolves |
| US-24.15 | `TestSecretSelfHealing_MissingSecret_RecreatedBeforePodCreation` | Delete password secret during backoff → next retry recreates it |
| US-24.15 | `TestSecretSelfHealing_ExistingSecret_NoOp` | Secret exists → ensurePasswordSecret is no-op |
| US-24.16 | `TestFileDownload_SafeMode_StreamsTarGz` | GET /files/download in safe mode → 200 + tar.gz stream |
| US-24.16 | `TestFileDownload_NormalMode_Rejected` | GET /files/download when not in safe mode → 409 Conflict |
| US-24.16 | `TestFileDownload_WrongOwner_Forbidden` | GET /files/download by non-owner → 403 |
| US-24.16 | `TestFileDownload_PathTraversal_Rejected` | path=../../etc/passwd → 400 Bad Request |

### Integration Tests (envtest)

| # | Test | What It Proves |
|---|---|---|
| I1 | Create workspace → delete pod externally → workspace self-heals to Active | End-to-end infrastructure recovery |
| I2 | Create workspace → simulate 15 consecutive pod losses → workspace stays in Creating (never Failed) | Unlimited infra retries work |
| I3 | Create workspace → pod enters CrashLoopBackOff 12× → Degraded condition set, phase still Creating | Process cap + Degraded behavior |
| I4 | Create workspace → buildPod fails (bad image) → immediate Failed with FailureReason=PodBuildFailed | Config failure is terminal |
| I5 | Workspace in Degraded → bump restartGeneration → immediate recovery (backoff cleared) | Manual override works |
| I6 | Workspace Active for 3 min → pod lost → ConsecutiveFailures = 1 (not carried from prior) | Stability reset works |
| I7 | checkAgentHealth deletes pod → next reconcile → TransientFailureCount unchanged, RestartCount++ | Controller restart ≠ failure |
| I8 | Pod Running but readiness probe failing → no controller health check fires | Readiness gate works |
| I9 | Burstable pod with 512Mi request, 2Gi limit → pod spec correct | QoS change applied |
| I10 | Workspace Degraded → backoff elapses → pod succeeds → Active → 2 min stability → Degraded cleared, ConsecutiveFailures = 0 | Natural recovery from Degraded (F43/F47) |
| I11 | Workspace with broken runtime image → 3 failures → enters safe mode → phase=Active + SafeMode condition | Safe mode entry on config failure |
| I12 | Workspace in safe mode → user bumps restartGeneration → safe mode cleared → normal pod attempted | Safe mode exit |
| I13 | Delete password secret while workspace in backoff → next retry recreates secret and succeeds | Secret self-healing |
| I14 | Workspace with CrashLoopBackOff 6× → enters safe mode → user can access diagnostic endpoint | Safe mode accessibility |

### E2E Tests (real cluster or kind)

| # | Test | What It Proves |
|---|---|---|
| E1 | Deploy workspace → `kubectl delete pod` → workspace recovers within 60s | Self-healing in real K8s |
| E2 | Deploy workspace → drain node → workspace reschedules and recovers | Infrastructure resilience |
| E3 | Deploy workspace → OOM-kill container (stress-ng) → pod restarts via kubelet, workspace stays Active | Burstable QoS absorbs spike; kubelet restart doesn't trigger controller failure path |
| E4 | Deploy workspace → network partition (iptables DROP on agentd port) for 2 min → partition heals → workspace recovers | Health check backoff + self-heal |
| E5 | Deploy workspace → 10 rapid pod deletions → workspace eventually stabilizes, never reaches Failed | Stress test for recovery loop |
| E6 | Prometheus scrape during E5 → verify `llmsafespace_workspace_recovery_attempts_total` increments correctly | Observability under stress |
| E7 | Deploy workspace with intentionally broken runtime image → workspace enters safe mode within 5 min → user can `kubectl exec` into safe-mode pod and access /workspace files | Safe mode E2E |
| E8 | Deploy workspace → corrupt a critical file on PVC → workspace CrashLoops → enters safe mode → PVC accessible read-only | Safe mode with corrupt data |

---

## Observability: New Metrics

```go
// Counters (no per-workspace labels — use CRD status for per-workspace detail)
llmsafespace_workspace_failures_total{failure_class="Infrastructure|Resource|Process|Configuration"}
llmsafespace_workspace_recovery_attempts_total{failure_class, outcome="success|backoff|degraded"}
llmsafespace_workspace_controller_restarts_total  // health-check initiated restarts (not failures)

// Gauges (aggregate — safe at any scale)
llmsafespace_workspaces_in_recovery_total         // count of workspaces currently in backoff (phase=Creating + NextRetryAt set)
llmsafespace_workspaces_degraded_total            // count of workspaces with Degraded condition

// Histograms (no per-workspace labels)
llmsafespace_workspace_recovery_duration_seconds{failure_class}  // time from failure to Active
llmsafespace_workspace_time_to_ready_seconds                     // pod start to readiness probe pass
llmsafespace_workspace_backoff_duration_seconds{failure_class}   // distribution of backoff durations applied
```

**Note (F18):** Per-workspace gauges (`consecutive_failures{workspace}`, `backoff_seconds{workspace}`) were removed from this design to avoid Prometheus cardinality explosion at scale. Per-workspace recovery state is available via the CRD status fields (`ConsecutiveFailures`, `NextRetryAt`, `LastFailureClass`) queryable through the API or `kubectl get ws -o json`.

---

## Rollout Plan

### Phase 1: Immediate Stability (US-24.3 + US-24.10)
- Ship Burstable QoS (eliminates OOM kills from normal usage)
- Ship SSE tracker timeout (eliminates goroutine leak)
- **Expected impact**: 50-70% reduction in pod restarts

### Phase 2: Architecture (US-24.1 + US-24.2)
- File split + interface extraction
- All existing tests must pass
- **Expected impact**: None (refactoring only), but enables Phase 3 testing

### Phase 3: Core Recovery Redesign (US-24.4 → US-24.8 + US-24.11 + US-24.12)
- Failure classification + recovery policies + remove terminal Failed + metrics
- **Expected impact**: Workspaces stop dying permanently from transient causes

### Phase 4: Health Refinement (US-24.9)
- Readiness gate
- **Expected impact**: Eliminates false-positive health failures during startup

---

## Risks

| Risk | Mitigation |
|---|---|
| Unlimited infra retries could mask a genuinely broken cluster | Degraded condition + metrics alert when any workspace exceeds 10 consecutive failures |
| Burstable QoS allows memory overcommit → node pressure | Webhook caps still enforce maximum limit; node-level monitoring is orthogonal |
| File split introduces import cycles | Design interfaces in a `types.go` within the workspace package; implementations in separate files |
| CRD field removal breaks existing workspaces | A10: no production users. Migration is a non-concern. |
| Degraded workspaces create ~144 pods/day at max backoff | Operators should alert on `llmsafespace_workspaces_degraded_total > 0` and triage within 24 hours |
| API `waitForActive` spins on Degraded workspaces | US-24.6 must update API to detect Degraded condition and return structured error with recovery ETA |
| Multi-writer conflict (API writes Suspend/Resume, controller writes status) | Known risk, deferred to Epic 25 pending conflict-rate metrics from Epic 23 Story 1 |

---

## Critical Review Findings (2026-06-01)

Findings from adversarial review of this design. Each is assessed as Real Gap, False Positive, or Accepted Trade-off.

### Real Gaps (must fix before implementation)

| # | Finding | Impact | Resolution |
|---|---|---|---|
| F1 | `NextRetryAt` enforcement unspecified — where in reconcile loop is it checked? | Without enforcement, backoff is ignored; controller retries every 5s | US-24.5 acceptance criteria: `handleCreating` MUST check `NextRetryAt` at top. If `time.Now() < NextRetryAt`, return `RequeueAfter` without pod creation. |
| F2 | API `waitForActive` will timeout-spin on Degraded workspaces | User sees "timed out" with no indication workspace is recovering | Add to US-24.6: API must detect `WorkspaceConditionDegraded = True` and return structured error with `NextRetryAt` ETA |
| F4 | Classification of `PodFailed` in `handleCreating` is ambiguous | Misclassification → wrong recovery policy | US-24.4 must include explicit classification table for pod Failed reasons (Evicted→Infra, OOMKilled→Resource, ImagePull→Config, Error+exitcode→Process) |
| F9 | `ControllerRestartCount` vs `RestartCount` semantics unclear | Operator confusion about what each counter means | Define: `RestartCount` = total pod restarts (all causes, backwards-compatible). `ControllerRestartCount` = subset from health-check initiated restarts. |
| F10 | Defaulting logic for empty `MemoryLimit` unspecified | Ambiguous pod spec generation | US-24.3: if `MemoryLimit` empty and `Memory` set → limit = 4×request. If both empty → defaults. Webhook validates limit >= request when both set. |
| FN3 | Pod stuck in K8s Pending (unschedulable) has no timeout in `handleCreating` | Workspace shows "Creating" forever with no backoff or Degraded signal | US-24.5: if pod in K8s Pending for >5 minutes, classify as Infrastructure and enter backoff loop |

### False Positives (no change needed)

| # | Finding | Why it's not a problem |
|---|---|---|
| F5 | Race between kubelet liveness kill and controller recovery | Controller sets phase=Creating before kubelet acts; `handleCreating` doesn't call `recoverFromTransientPodLoss`; no double-counting |
| F6 | `handleActive` pod-NotFound after controller restart loses in-memory state | `LastStableAt` is persisted in CRD, not in-memory. Controller restart doesn't lose recovery state. |
| F8 | 2-minute stability reset cliff effect | Inherent to threshold-based resets. Gradual decay adds complexity for marginal benefit. 2 minutes is generous. |
| FN1 | Kubelet liveness probe kills CPU-starved pods | Post-Epic-22, `/v1/healthz` is a single JSON encode (<1ms). CPU throttling at 2000m limit won't affect it. |

### Accepted Trade-offs (documented, no change)

| # | Finding | Why we accept it |
|---|---|---|
| F3 | Failure class escalation missing (wrong classification retries forever) | Degraded condition + metrics alert. Operator triages. Adding auto-escalation reintroduces terminal-from-transient. |
| F7 | Multi-writer conflict on Status (API vs controller) | Deferred to Epic 25 pending conflict-rate metrics. Known risk with known mitigation path. |
| F11 | Degraded workspace consuming resources indefinitely | 144 pods/day is negligible. PVC cost is inherent. Operator alerts on Degraded > 0. |
| F12 | No TTL on Degraded condition | Adding TTL reintroduces terminal-from-transient. `restartGeneration` is the manual escape hatch. |
| FN2 | PVC stuck after node failure classified as Configuration (terminal) | Controller can't distinguish "storage misconfigured" from "volume lost." 5-min timeout is reasonable for both. Human intervention needed regardless. |

---

## Critical Review — Second Pass (2026-06-01)

### Additional Real Gaps Found

| # | Finding | Impact | Resolution |
|---|---|---|---|
| F13 | `FailureClass` vs `FailureReason` relationship unspecified — are they redundant? | Operator confusion; unclear which field to use for alerting | Add note: `FailureReason` = trigger event (only set when Phase=Failed). `FailureClass` = current recovery classification (set during recovery, cleared on stability). Complementary, not redundant. |
| F14 | `EnsureSession` enters wait loop on Degraded workspaces (amplification of F2) | User clicks workspace → 30s timeout → generic error. No indication of Degraded state or recovery ETA. | US-24.6 acceptance criteria must explicitly cover `EnsureSession`: check Degraded condition BEFORE entering wait loop. Return structured response with `nextRetryAt`, `consecutiveFailures`, and human-readable message. |
| F16 | `NextRetryAt` check at TOP of `handleCreating` would miss a pod that recovered on its own | Workspace stays in backoff even though its pod is now Running | F1 resolution refined: `NextRetryAt` check goes in the "pod doesn't exist" branch. Flow: (1) check pod exists, (2) if Running → Active, (3) if terminating → wait, (4) if Failed → classify, (5) if not exists → check NextRetryAt → create or requeue. |
| F18 | Per-workspace Prometheus gauge labels cause cardinality explosion at scale | 1000 workspaces = 2000 unbounded time series; Prometheus OOM risk | Remove per-workspace labels from gauges. Use aggregate gauges (`workspaces_in_backoff`, `workspaces_degraded_total`) + histograms for durations. Per-workspace detail available via CRD status / API. |
| F19 | `restartGeneration` bump not checked in `handleCreating` — Degraded workspace can't be manually recovered | User bumps restartGeneration on Degraded workspace → nothing happens (stuck in Creating backoff) | US-24.5: `handleCreating` must check `spec.restartGeneration > status.observedRestartGeneration`. If bumped, clear all recovery state (ConsecutiveFailures, NextRetryAt, Degraded condition) and immediately create pod. |

### Additional False Positives

| # | Finding | Why it's not a problem |
|---|---|---|
| F15 | Mixed failure classes across consecutive failures (OOM → eviction → crash) — which policy applies? | Single counter with last-class-wins is correct for "what's blocking recovery NOW?" Stability reset prevents historical accumulation. |
| F17 | `Spec.Timeout` clock restarts on every recovery — workspace never times out | Correct behavior. Timeout is "max continuous runtime." Recovering workspace is actively used, should not be auto-suspended. |
| F20 | `RecoveryPolicy` struct + map is over-engineered | No. Struct earns existence through testability (backoff calculation tested independently), readability (policy declared not buried in control flow), and extensibility (new class = one map entry). |

### Minor Items (no story needed, handle during implementation)

| # | Finding | Resolution |
|---|---|---|
| F21 | `FailureReasonTransientPodLoss` and `FailureReasonTooManyFailures` become dead code | Mark as deprecated in comments. Keep in enum for tooling compatibility. |
| F13-note | `FailureClass` and `FailureReason` relationship | Add clarifying comment to CRD types during US-24.12. |

---

## Critical Review — Third Pass (2026-06-01)

### Additional Assumptions Validated

| # | Assumption | Verification |
|---|---|---|
| A21 | `handleSuspending` clears `TransientFailureCount` to 0 | Verified: `controller.go:427` |
| A22 | API can only set phase to Suspending (from Active) or Resuming (from Suspended) | Verified: `workspace_service.go:394,402,434,442` |
| A23 | Default controller-runtime rate limiter applies (exponential backoff on errors) | Verified: no `RateLimiter` override in `SetupWithManager` |
| A24 | `Status().Update` failures cause reconcile requeue via returned error | Verified: all callsites return the error |
| A25 | Controller watches Pods via `Owns(&corev1.Pod{})` — pod changes trigger workspace reconcile | Verified: `controller.go:1132` |

### Additional Real Gaps Found

| # | Finding | Impact | Resolution |
|---|---|---|---|
| F22 | `handleSuspending` doesn't clear new recovery state fields | Workspace resumes with stale Degraded condition / ConsecutiveFailures from before suspend | US-24.5/US-24.6: `handleSuspending` must clear `ConsecutiveFailures`, `NextRetryAt`, `LastFailureClass`, `LastFailureAt`, and remove Degraded condition. Suspend is a clean break. |
| F28 | `LastStableAt = nil` after suspend/fresh-create — stability reset logic must handle nil | Panic on `time.Since(nil)` or incorrect stability calculation | US-24.8: handle nil `LastStableAt` as "clock not started." Start 2-minute clock on first healthy reconcile. Explicit nil check before time comparison. |
| F30 | `classifyFailure` has no default case for unknown pod failure reasons | Unknown failure → no policy → undefined behavior (panic or no-op) | US-24.4: unknown reasons default to `FailureClassProcess` (conservative non-terminal). Classifier must never return `FailureClassNone` for a failed pod. Test: `TestClassifyFailure_UnknownReason_DefaultsToProcess`. |

### Additional False Positives

| # | Finding | Why it's not a problem |
|---|---|---|
| F24 | Clock skew between controller and etcd causes premature retry | `NextRetryAt` is computed and compared on the same controller. NTP keeps nodes within ~100ms. Negligible on 10s+ backoffs. |
| F25 | Stale pod from previous attempt during backoff + workspace deletion | Pod naming is deterministic per workspace UID. `handleTerminating` finds correct pod or no-ops. |
| F26 | Infinite reconcile loop if etcd is down | controller-runtime's default rate limiter applies exponential backoff on errors. Standard behavior, no custom handling needed. |
| F31 | E3 test assumption (kubelet restart doesn't trigger controller) | Single OOM kill → container restarts → pod stays Running → controller doesn't notice. Correct for Burstable QoS spike scenario. |

### Additional Accepted Trade-offs

| # | Finding | Why we accept it |
|---|---|---|
| F23 | `Status().Update` conflict loses recovery state write (amplification of F7) | Worst case: one backoff cycle skipped (controller retries immediately). Liveness issue, not safety issue. System remains self-healing. Deferred to Epic 25 with retry-on-conflict. |
| F27 | `restartGeneration` not checked in `handlePending` — 5-min delay to apply spec fix | User can delete/recreate for immediate effect. Pending timeout is the correct escape hatch for wrong specs. Minor UX issue, not robustness gap. |

---

## Critical Review — Fourth Pass (2026-06-01, fresh perspective)

### Additional Real Gaps Found

| # | Finding | Impact | Resolution |
|---|---|---|---|
| F34 | `ConsecutiveFailures` semantics when failure class changes are unspecified | Ambiguous cap behavior: does Infrastructure failure at count=12 trigger Degraded? | Explicitly state: counter is class-agnostic (always increments). MaxAttempts cap check uses the CURRENT failure's class. Infrastructure (MaxAttempts=0) never triggers Degraded regardless of counter value. |
| F36 | `Spec.MaxRetries` field still exists on CRD but design doesn't mention it | Field exists but does nothing after Epic 24 — confusing for operators | Remove `Spec.MaxRetries` from CRD (A10 allows breaking changes). Per-class policies replace it. Add to US-24.12. |
| F39 | Backoff calculation `BackoffBase * 2^(failures-1)` overflows time.Duration at failures≥63 | Negative duration → immediate retry (no backoff) for long-running Infrastructure recovery | US-24.5: cap shift exponent at safe value (e.g., 30) before computing. Test: `TestBackoffCalculation_HighFailureCount_NoOverflow` with ConsecutiveFailures=100. |

### Over-Engineering Concern

| # | Finding | Assessment |
|---|---|---|
| F38 | US-24.2 proposes `HealthChecker`, `PodObserver`, `RecoveryEngine` interfaces but only one implementation exists for each | `PodObserver` and `RecoveryEngine` should be plain functions, not interfaces (YAGNI). Only `HealthChecker` genuinely benefits from an interface (avoids real HTTP in tests). Reduce US-24.2 scope or merge into US-24.1. |

### Additional False Positives

| # | Finding | Why it's not a problem |
|---|---|---|
| F32 | `handleActive` returns `RequeueAfter: 15s` after checkAgentHealth sets phase=Creating | Watch event from Status().Update immediately requeues; controller-runtime deduplicates. 15s is superseded. |
| F33 | ActivityTracker writes during backoff trigger wasted reconciles | Cheap (cache read + NextRetryAt check). ~3 extra reconciles/sec at 100 workspaces in backoff. Negligible. Defer optimization. |
| F35 | Pod NotFound → can't inspect container status for classification | Pod NotFound implies external deletion (node loss, preemption). OOM is observed via CrashLoopBackOff or PodFailed, not NotFound. Classification as Infrastructure is correct. |
| F37 | `StartTime` not cleared when checkAgentHealth deletes pod | Phase transitions to Creating; handleCreating doesn't run health checks; StartTime is overwritten when new pod reaches Active. No window for incorrect grace period. |
| F40 | Secret watch events trigger reconciles during backoff | Harmless — handleCreating is idempotent. Reconcile checks NextRetryAt and returns early. |

### Additional Accepted Trade-offs

| # | Finding | Why we accept it |
|---|---|---|
| F33-scale | At 10,000+ workspaces, wasted reconciles from activity writes could matter | Standard fix (predicate filter on watch) is well-known. Defer until metrics show it's needed. Not a correctness issue. |

---

## Critical Review — Fifth Pass (2026-06-01, under-examined areas)

Focus: Areas reviewed fewer than 3 times — file split risks, readiness gate edge cases, SSE timeout interactions, test plan completeness, rollout risks, Degraded lifecycle, frontend implications, webhook interactions.

### Additional Real Gaps Found

| # | Finding | Impact | Resolution |
|---|---|---|---|
| F43 | Degraded condition is never cleared on natural recovery (stability reset clears ConsecutiveFailures but not the Condition) | Workspace shows "Degraded" forever after recovering | US-24.8: stability reset must also remove `WorkspaceConditionDegraded` (set to False). Add test I10. |
| F44 | Frontend `WorkspaceStatus` type has no `conditions` field — Degraded won't be visible to users | Design promises "user sees workspace recovering" but frontend can't display it | Document as follow-up (frontend story). Epic 24 delivers the backend capability; frontend consumption is a separate story. |
| F46 | `checkAgentHealth` violates SRP — makes HTTP calls, modifies status, deletes pods, changes phase | File split (US-24.1) will be messy if this isn't decomposed | US-24.1: decompose into observation (health.go returns result) and action (phase_active.go decides/acts). No file both makes HTTP calls AND modifies workspace status. |
| F48 | Webhook `MaxCPUMillicores`/`MaxMemoryMi` flags — unclear if they cap request or limit fields | Operator confusion; possible bypass (set high limit, low request) | US-24.3: `MaxCPUMillicores` caps `cpuLimit`, `MaxMemoryMi` caps `memoryLimit`. Requests implicitly bounded by limit >= request. |
| F49 | `handleCreating` with Failed pod doesn't delete it before setting NextRetryAt — controller loops on same Failed pod | ConsecutiveFailures increments every 5s until kubelet GCs the pod | US-24.5: when handleCreating observes Failed pod, it must DELETE the pod before setting recovery state. Ensures next reconcile hits "pod not found" branch and respects backoff. |

### Test Plan Gap

| # | Finding | Resolution |
|---|---|---|
| F47 | No test for natural Degraded recovery (Degraded → retry succeeds → Active → 2min → Degraded cleared) | Add I10: "Workspace Degraded → next retry succeeds → Active → 2 min stability → Degraded cleared, ConsecutiveFailures = 0" |

### Additional False Positives

| # | Finding | Why it's not a problem |
|---|---|---|
| F41 | Readiness gate is redundant with grace period in happy path | Gate protects slow-starting pods (>30s) where grace period is insufficient. Correct feature, rationale needs refinement only. |
| F42 | Phase 1 (Burstable QoS) ships with old 3-strike rule | Partial fix is acceptable during rollout. 2Gi limit covers 95%+ of spikes. Full fix follows in Phase 3. |
| F45 | SSE tracker map stale during reconnect gap | Current behavior (keep map) is correct. Brief staleness is better than "all idle" flash. SSE delivers correct state within seconds of reconnect. |

---

## Supersession Notes

### Epic 21 (Workspace Recovery State Machine)

| Epic 21 Story | Disposition | Rationale |
|---|---|---|
| US-21.1 (add status fields) | **Superseded by US-24.12** | Different field set (FailureClass replaces per-reason backoff fields) |
| US-21.2 (implement backoff) | **Superseded by US-24.5** | Epic 24 uses per-class policies instead of a single 8-step schedule |
| US-21.3 (honor NextRetryAt) | **Superseded by US-24.5** | Same mechanism, different policy structure |
| US-21.4 (stability reset) | **Superseded by US-24.8** | 2-minute window instead of 5-minute |
| US-21.5 (restartGeneration during backoff) | **Superseded by US-24.5** | Same behavior, different implementation |
| US-21.6 (cap at attempt 9) | **Superseded by US-24.6** | Epic 24 caps at 12 for process/resource, unlimited for infra, and enters Degraded instead of Failed |
| US-21.7 (FailureReason enum) | **Already shipped** (worklog 0105). Epic 24 extends it with FailureClass. |
| US-21.8 (markFailed helper) | **Already shipped** (worklog 0105). Epic 24 modifies callsites. |
| US-21.9 (backoff policy per reason) | **Superseded by US-24.5** | Per-class instead of per-reason |
| US-21.10 (surface in API) | **Moved to Epic 24 follow-up** | Still useful, lower priority |

### Epic 22 (agentd Health-Endpoint Redesign)

**Fully shipped** (worklog 0105). No remaining stories. Epic 24 builds on top of Epic 22's work.

### Epic 23 (Controller Race Hardening)

| Epic 23 Story | Disposition | Rationale |
|---|---|---|
| Story 1 (DeletionTimestamp guard) | **Already shipped** (worklog 0105) |
| Story 2 (retry-on-conflict) | **Deferred** pending conflict-rate metrics from Story 1 |
| Story 3 (single-writer migration) | **Deferred** pending conflict-rate metrics from Story 1 |
| Story 4 (pwCache + header sanitization) | **Already shipped** (worklog 0105) |
