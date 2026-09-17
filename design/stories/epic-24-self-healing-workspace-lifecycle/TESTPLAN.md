# Epic 24: Test Plan & Observability Specification

**Created:** 2026-06-01

---

## Test Strategy

Three layers, each catching different classes of bugs:

| Layer | What it catches | Speed | Scope |
|---|---|---|---|
| Unit tests | Logic errors in pure functions (classifier, backoff calc, policy lookup) | <1s per test | Single function |
| Integration tests (envtest) | State machine transitions, multi-reconcile sequences, CRD interactions | 1-5s per test | Controller + fake K8s API |
| E2E tests (kind cluster) | Real K8s behavior, kubelet interactions, actual pod lifecycle, network | 30-120s per test | Full system |

---

## Unit Tests

### US-24.3: Burstable QoS

```
TestResourceRequirements_BurstableDefaults
  - Input: spec.resources = nil
  - Assert: request=500m/512Mi, limit=2000m/2Gi

TestResourceRequirements_CustomRequestAndLimit
  - Input: cpu=1000m, memory=1Gi, cpuLimit=4000m, memoryLimit=4Gi
  - Assert: request=1000m/1Gi, limit=4000m/4Gi

TestResourceRequirements_CustomRequestEmptyLimit
  - Input: cpu=1000m, memory=1Gi, cpuLimit="", memoryLimit=""
  - Assert: request=1000m/1Gi, limit=4000m/4Gi (4× default)

TestResourceRequirements_LimitBelowRequest_Rejected
  - Input: memory=2Gi, memoryLimit=512Mi
  - Assert: webhook returns admission.Denied

TestResourceRequirements_LimitEqualsRequest_Allowed
  - Input: memory=1Gi, memoryLimit=1Gi
  - Assert: webhook allows (QoS=Guaranteed is valid, just not default)

TestResourceRequirements_MaxCapAppliesToLimit
  - Input: memoryLimit=128Gi, MaxMemoryMi=65536
  - Assert: webhook returns admission.Denied (128Gi > 64Gi cap)

TestResourceRequirements_EphemeralStorageUnchanged
  - Input: ephemeralStorage=2Gi
  - Assert: request=limit=2Gi (ephemeral storage stays Guaranteed — no burst)
```

### US-24.4: Failure Classification

```
--- Infrastructure class ---
TestClassifyFailure_PodNotFound → Infrastructure
TestClassifyFailure_Evicted → Infrastructure
TestClassifyFailure_Preempting → Infrastructure
TestClassifyFailure_NodeShutdown → Infrastructure
TestClassifyFailure_NodeAffinity → Infrastructure
TestClassifyFailure_Unschedulable → Infrastructure
TestClassifyFailure_Terminated → Infrastructure
TestClassifyFailure_DeadlineExceeded → Infrastructure
TestClassifyFailure_GracefulNodeShutdown → Infrastructure
TestClassifyFailure_DisruptionTarget → Infrastructure

--- Resource class ---
TestClassifyFailure_OOMKilled → Resource
TestClassifyFailure_ExitCode137_OOMReason → Resource
TestClassifyFailure_EphemeralStorageEviction → Resource
TestClassifyFailure_ContainerExceededMemoryLimit → Resource

--- Process class ---
TestClassifyFailure_CrashLoopBackOff → Process
TestClassifyFailure_ExitCode1 → Process
TestClassifyFailure_ExitCode137_ErrorReason → Process (SIGKILL without OOM)
TestClassifyFailure_ExitCode143 → Process (SIGTERM)
TestClassifyFailure_RunContainerError → Process
TestClassifyFailure_StartError → Process
TestClassifyFailure_ContainerCannotRun → Process
TestClassifyFailure_PostStartHookError → Process
TestClassifyFailure_BackOff → Process

--- Configuration class ---
TestClassifyFailure_ImagePullBackOff → Configuration
TestClassifyFailure_ErrImagePull → Configuration
TestClassifyFailure_InvalidImageName → Configuration
TestClassifyFailure_ErrImageNeverPull → Configuration
TestClassifyFailure_CreateContainerConfigError → Configuration
TestClassifyFailure_CreateContainerError → Configuration

--- Priority / ambiguity ---
TestClassifyFailure_OOMAlwaysWins
  - OOMKilled=true + CrashLoop=true + Reason="Evicted" → Resource (OOM takes priority)

TestClassifyFailure_ExitCode137_Disambiguation
  - ExitCode=137 + Reason="OOMKilled" → Resource
  - ExitCode=137 + Reason="Error" → Process
  - ExitCode=137 + Reason="" → Process (conservative default)

--- Default / unknown ---
TestClassifyFailure_UnknownReason → Process
TestClassifyFailure_EmptyReason → Process
TestClassifyFailure_NilContainerStatus → Process
TestClassifyFailure_FuzzNeverReturnsNone (1000 random strings → never None)
```

### US-24.5: Recovery Policy & Backoff

```
--- Policy lookup ---
TestRecoveryPolicy_InfrastructureUnlimited
  - MaxAttempts=0, any ConsecutiveFailures → never triggers safe mode

TestRecoveryPolicy_ProcessCapped
  - ConsecutiveFailures=6 + Process → triggers safe mode

TestRecoveryPolicy_ConfigurationFast
  - ConsecutiveFailures=3 + Configuration → triggers safe mode

TestRecoveryPolicy_ResourceCapped
  - ConsecutiveFailures=6 + Resource → triggers safe mode

TestRecoveryPolicy_InfraAtHighCount_NeverSafeMode
  - ConsecutiveFailures=50 + Infrastructure → no safe mode (F34)

--- Backoff calculation ---
TestBackoff_FirstAttempt
  - ConsecutiveFailures=1, Infrastructure → 5s

TestBackoff_SecondAttempt
  - ConsecutiveFailures=2, Infrastructure → 10s

TestBackoff_CappedAtMax
  - ConsecutiveFailures=10, Infrastructure → 2min (not 2560s)

TestBackoff_ProcessBase
  - ConsecutiveFailures=1, Process → 10s

TestBackoff_ProcessCap
  - ConsecutiveFailures=10, Process → 5min (not 5120s)

TestBackoff_HighFailureCount_NoOverflow (F39)
  - ConsecutiveFailures=100 → BackoffMax (no negative duration)

TestBackoff_ZeroFailures_ZeroDuration
  - ConsecutiveFailures=0 → 0 (no backoff, immediate retry)

--- Counter semantics ---
TestCounter_ClassAgnostic_AlwaysIncrements
  - Failure(Infra) → 1, Failure(Process) → 2, Failure(Resource) → 3

TestCounter_SafeModeTrigger_UsesCurrentClass
  - ConsecutiveFailures=5, next failure is Process (SafeModeAfter=6) → triggers
  - ConsecutiveFailures=5, next failure is Infra (SafeModeAfter=0) → does NOT trigger

--- NextRetryAt enforcement ---
TestHandleCreating_NextRetryAt_NotElapsed_RequeuesWithoutPodCreation
TestHandleCreating_NextRetryAt_Elapsed_CreatesPod
TestHandleCreating_NextRetryAt_PodAlreadyRunning_TransitionsToActive (F16)
TestHandleCreating_RestartGeneration_BypassesBackoff (F19)
TestHandleCreating_FailedPod_DeletedBeforeSettingBackoff (F49)

--- Pending timeout ---
TestHandleCreating_PodPending_Unschedulable_5min_EntersBackoff (FN3)
TestHandleCreating_PodPending_PullingImage_NoTimeout (C17)
TestHandleCreating_PodPending_Scheduled_ContainerCreating_NoTimeout
```

### US-24.7: Controller-Initiated Restarts

```
TestControllerRestart_IncrementsRestartCount
TestControllerRestart_IncrementsControllerRestartCount
TestControllerRestart_DoesNotIncrementConsecutiveFailures
TestControllerRestart_5Consecutive_TriggersSafeMode (A13)
TestControllerRestart_StabilityBetween_ResetsControllerRestartCount
```

### US-24.8: Stability Reset

```
TestStabilityReset_After2Minutes_ClearsConsecutiveFailures
TestStabilityReset_After2Minutes_ClearsDegradedCondition (F43)
TestStabilityReset_After2Minutes_ClearsSafeModeIfNormalPodRunning
TestStabilityReset_Before2Minutes_PreservesState
TestStabilityReset_NilLastStableAt_StartsClockOnHealthy (F28)
TestSuspend_ClearsAllRecoveryState (F22)
TestSuspend_ClearsSafeModeFlag
TestSuspend_ClearsDegradedCondition
```

### US-24.9: Readiness Gate

```
TestReadinessGate_PodRunning_NotReady_NoHealthCheck
TestReadinessGate_PodRunning_Ready_HealthCheckRuns
TestReadinessGate_NoContainerStatuses_NoHealthCheck
TestReadinessGate_SafeModePod_SkipsHealthCheck
```

### US-24.10: SSE Tracker Timeout

```
TestSSETracker_ReconnectsOnContextTimeout
TestSSETracker_PreservesMapAcrossReconnect
TestSSETracker_BackoffOnReconnectFailure
TestSSETracker_5MinDeadline_Fires
```

### US-24.13: Safe Mode

```
TestSafeMode_TriggeredAfterNProcessFailures
TestSafeMode_TriggeredAfterNConfigFailures
TestSafeMode_NotTriggeredForInfrastructure
TestSafeMode_PodSpec_SHAPinnedImage
TestSafeMode_PodSpec_NoInitContainers
TestSafeMode_PodSpec_PVCReadOnly_WhenProcessCrash
TestSafeMode_PodSpec_PVCReadWrite_WhenDiskPressure
TestSafeMode_PodSpec_NoPVC_WhenNeverBound
TestSafeMode_PodSpec_MinimalResources
TestSafeMode_PodSpec_SecurityContext_Maintained
TestSafeMode_TransitionsToActive_WithSafeModeCondition
TestSafeMode_RestartGeneration_ExitsSafeMode
TestSafeMode_ControllerSkipsHealthCheck
TestSafeMode_ControllerSkipsEnrichStatus
TestSafeMode_AutoSuspendDisabled
TestSafeMode_TTLDisabledAfterSuspend
TestSafeMode_TriggeredByControllerRestartCount (A13)
```

### US-24.14: Image Pinning

```
TestImagePinning_FirstResolve_StoresDigest
TestImagePinning_SubsequentCreations_UseStoredDigest
TestImagePinning_RestartGeneration_ClearsDigest_ReResolves
TestImagePinning_ExplicitImageRef_StoredAsIs
TestImagePinning_RuntimeEnvUpdate_DoesNotAffectExisting
```

### US-24.15: Secret Self-Healing

```
TestSecretSelfHealing_MissingSecret_RecreatedInHandleCreating
TestSecretSelfHealing_ExistingSecret_NoOp
TestSecretSelfHealing_SecretDeletedDuringBackoff_RecreatedOnRetry
```

### US-24.16: File Download

```
TestFileDownload_SafeMode_StreamsTarGz
TestFileDownload_NormalMode_Rejected (409)
TestFileDownload_WrongOwner_Forbidden (403)
TestFileDownload_PathTraversal_Rejected (400)
TestFileDownload_EmptyPath_DefaultsToWorkspace
TestFileDownload_LargeDirectory_StreamsWithoutBuffering
```

### US-24.17: Degraded Detection

```
TestDegraded_DiskPressure_95Percent_SetsCondition
TestDegraded_DiskPressure_Below95_ClearsCondition
TestDegraded_NoProviders_SetsAgentDegraded
TestDegraded_ProvidersReconnect_ClearsCondition
TestDegraded_PodNotReady_StructuredProxy503
TestDegraded_DoesNotRestartPod
TestDegraded_ConditionAutoClearsOnRecovery
```

### US-24.11: Metrics

```
TestMetrics_FailureClassifications_Increments
TestMetrics_UnknownClassification_IncrementsUnknownCounter
TestMetrics_RecoveryAttempts_RecordsOutcome
TestMetrics_WorkspacesInRecovery_GaugeAccurate
TestMetrics_WorkspacesDegraded_GaugeAccurate
TestMetrics_RecoveryDuration_RecordsHistogram
TestMetrics_ControllerRestarts_Increments
TestMetrics_SafeModeEntries_Increments
```

> #760 (2026-09-16): the SafeMode entries/active/exits metrics and their
> tests above were deleted with the SafeMode machinery — superseded by the
> `WorkspaceRecoveryExhaustedTotal` counter tests in
> `controller/internal/workspace/recovery_exhaustion_test.go`.

---

## Integration Tests (envtest)

Full reconcile-loop tests with fake K8s API server. Each test creates a workspace and drives it through multiple reconcile cycles.

```
I1:  Create workspace → delete pod externally → workspace self-heals to Active
I2:  Create workspace → simulate 15 consecutive pod losses → stays Creating (never Failed)
I3:  Create workspace → pod CrashLoopBackOff 6× → enters safe mode → phase=Active + SafeMode
I4:  Create workspace → buildPod fails (bad image) → 3 retries → enters safe mode
I5:  Workspace in safe mode → bump restartGeneration → safe mode cleared → normal pod attempted
I6:  Workspace Active 3 min → pod lost → ConsecutiveFailures=1 (stability reset worked)
I7:  checkAgentHealth deletes pod → ConsecutiveFailures unchanged, RestartCount++
I8:  Pod Running but not Ready → no controller health check fires
I9:  Burstable pod spec: request=512Mi, limit=2Gi
I10: Workspace Degraded → retry succeeds → Active → 2 min → Degraded cleared
I11: Workspace with broken image → 3 failures → safe mode → phase=Active + SafeMode
I12: Workspace in safe mode → bump restartGeneration → normal pod attempted
I13: Delete password secret during backoff → next retry recreates and succeeds
I14: Workspace CrashLoop 6× → safe mode → terminal accessible (exec works)
I15: Workspace Active → disk 96% → DiskPressure condition set → disk freed → condition cleared
I16: Workspace Active → providers disconnect → AgentDegraded set → reconnect → cleared
I17: Workspace in safe mode → auto-suspend does NOT fire (disabled)
I18: Workspace in safe mode → suspend manually → resume → tries normal mode (recovery state cleared)
I19: Pod Pending (Unschedulable) 5 min → enters backoff → node added → pod schedules → Active
I20: Pod Pending (pulling large image) 7 min → does NOT enter backoff (C17 fix)
I21: Controller restart (new reconciler instance) → picks up recovery state from CRD → continues backoff
I22: restartGeneration bumped during backoff → immediate pod creation (bypasses NextRetryAt)
I23: ControllerRestartCount > 5 without stability → enters safe mode (A13)
I24: Safe mode pod with no PVC (PVC never bound) → boots successfully without volume
I25: Image pinning: first pod stores digest → RuntimeEnv updated → second pod uses OLD digest
```

---

## E2E Tests (kind cluster)

Real K8s, real kubelet, real pod lifecycle. Slower but catches real-world behavior.

```
E1:  Deploy workspace → kubectl delete pod → workspace recovers within 60s
E2:  Deploy workspace → drain node → workspace reschedules and recovers
E3:  Deploy workspace → stress-ng memory spike to 1.5Gi → pod stays Running (Burstable absorbs)
E4:  Deploy workspace → iptables DROP on agentd port 2 min → partition heals → recovers
E5:  Deploy workspace → 10 rapid pod deletions → stabilizes, never enters safe mode
E6:  Prometheus scrape during E5 → recovery_attempts_total increments correctly
E7:  Deploy workspace with broken image → enters safe mode → kubectl exec works
E8:  Deploy workspace → fill PVC to 100% → DiskPressure condition set → delete files → cleared
E9:  Deploy workspace → safe mode → GET /files/download → receives valid tar.gz
E10: Deploy workspace → verify safe-mode image is pulled by digest (not tag)
```

---

## Observability Specification

### Metrics (Prometheus)

#### Counters

```go
// Failure classification — every classification decision is counted
llmsafespace_workspace_failure_classifications_total{
    class,           // "Infrastructure" | "Resource" | "Process" | "Configuration"
    pod_reason,      // raw K8s pod.Status.Reason (bounded: ~15 known values)
    container_reason // raw container state reason (bounded: ~20 known values)
}

// Unknown classification — fires when classifier hits default path
llmsafespace_workspace_failure_classification_unknown_total{
    pod_reason,
    container_reason
}

// Recovery attempts — outcome of each recovery cycle
llmsafespace_workspace_recovery_attempts_total{
    failure_class,
    outcome          // "retry" | "safe_mode" | "recovered"
}

// Controller-initiated restarts (health check driven, NOT counted as failures)
llmsafespace_workspace_controller_restarts_total

// Safe mode entries
llmsafespace_workspace_safe_mode_entries_total{
    trigger          // "process_failures" | "resource_failures" | "config_failures" | "controller_restart_loop"
}

// Safe mode exits
llmsafespace_workspace_safe_mode_exits_total{
    method           // "restart_generation" | "suspend_resume"
}
```

#### Gauges

```go
// Current state (aggregate — no per-workspace labels)
llmsafespace_workspaces_in_recovery_total      // phase=Creating + NextRetryAt set
llmsafespace_workspaces_in_safe_mode_total     // SafeMode=true
llmsafespace_workspaces_degraded_total         // any Degraded condition set
llmsafespace_workspaces_active_healthy_total   // Active, no conditions, healthy
```

#### Histograms

```go
// Time from failure detection to Active recovery
llmsafespace_workspace_recovery_duration_seconds{failure_class}
  Buckets: 5, 10, 30, 60, 120, 300, 600, 1800, 3600

// Time from pod creation to readiness probe pass
llmsafespace_workspace_time_to_ready_seconds
  Buckets: 5, 10, 15, 30, 60, 120, 300

// Backoff durations applied
llmsafespace_workspace_backoff_applied_seconds{failure_class}
  Buckets: 5, 10, 20, 40, 60, 120, 300, 600
```

### Structured Logging

Every operationally significant decision is logged at Info level with structured fields:

```go
// Failure classification
logger.Info("failure classified",
    "workspace", ws.Name,
    "class", class,
    "consecutiveFailures", ws.Status.ConsecutiveFailures,
    "podPhase", obs.Phase,
    "podReason", obs.Reason,
    "containerReason", obs.ContainerReason,
    "containerExitCode", obs.ContainerExitCode,
    "oomKilled", obs.ContainerOOMKilled,
)

// Recovery decision
logger.Info("recovery action",
    "workspace", ws.Name,
    "action", "backoff|safe_mode|retry_immediate",
    "class", class,
    "attempt", ws.Status.ConsecutiveFailures,
    "nextRetryAt", nextRetryAt,
    "backoffDuration", backoff,
    "safeModeAfter", policy.SafeModeAfter,
)

// Safe mode entry
logger.Info("entering safe mode",
    "workspace", ws.Name,
    "trigger", trigger,
    "consecutiveFailures", ws.Status.ConsecutiveFailures,
    "lastFailureClass", ws.Status.LastFailureClass,
    "lastError", ws.Status.Message,
)

// Safe mode exit
logger.Info("exiting safe mode",
    "workspace", ws.Name,
    "method", "restart_generation|suspend_resume",
    "restartGeneration", ws.Spec.RestartGeneration,
)

// Stability reset
logger.Info("stability reset",
    "workspace", ws.Name,
    "previousFailures", previousCount,
    "stableSince", ws.Status.LastStableAt,
)

// Degraded condition change
logger.Info("degraded condition changed",
    "workspace", ws.Name,
    "condition", conditionType,
    "status", "True|False",
    "reason", reason,
    "message", message,
)

// Controller-initiated restart
logger.Info("controller-initiated pod restart",
    "workspace", ws.Name,
    "reason", "health_check_threshold",
    "consecutiveHealthFailures", ws.Status.ConsecutiveHealthFailures,
    "controllerRestartCount", ws.Status.ControllerRestartCount,
)
```

### Alert Rules

```yaml
groups:
- name: llmsafespace-workspace-health
  rules:

  # Unknown failure classification — new K8s reason string discovered
  - alert: WorkspaceClassifierUnknownReason
    expr: rate(llmsafespace_workspace_failure_classification_unknown_total[1h]) > 0
    for: 5m
    labels:
      severity: info
    annotations:
      summary: "Classifier hit unknown reason string"
      runbook: "Check logs for raw reason. Add to classifier. Workspace defaulted to Process class (safe mode after 6)."

  # Workspaces stuck in recovery for >30 min
  - alert: WorkspacesInRecoveryTooLong
    expr: llmsafespace_workspaces_in_recovery_total > 0
    for: 30m
    labels:
      severity: warning
    annotations:
      summary: "{{ $value }} workspace(s) in recovery loop >30 min"
      runbook: "Check failure class. Infrastructure: cluster issue (nodes, storage). Process: runtime bug. Configuration: bad spec."

  # Any workspace in safe mode
  - alert: WorkspaceInSafeMode
    expr: llmsafespace_workspaces_in_safe_mode_total > 0
    for: 5m
    labels:
      severity: warning
    annotations:
      summary: "{{ $value }} workspace(s) in safe mode"
      runbook: "User has shell access. Check diagnostic endpoint. Common causes: broken image, init failure, persistent crash."

  # Degraded workspaces (disk pressure, credential issues)
  - alert: WorkspacesDegraded
    expr: llmsafespace_workspaces_degraded_total > 0
    for: 10m
    labels:
      severity: info
    annotations:
      summary: "{{ $value }} workspace(s) degraded"
      runbook: "Check conditions on workspace CRD. DiskPressure: user must free space. AgentDegraded: credentials may need refresh."

  # Recovery taking too long (p95 > 5 min)
  - alert: WorkspaceRecoverySlowP95
    expr: histogram_quantile(0.95, rate(llmsafespace_workspace_recovery_duration_seconds_bucket[1h])) > 300
    for: 15m
    labels:
      severity: warning
    annotations:
      summary: "p95 workspace recovery time >5 min"
      runbook: "Check if backoff values are too aggressive. Check if image pulls are slow. Check node scheduling capacity."

  # Controller restart loop (health checks failing repeatedly)
  - alert: WorkspaceControllerRestartLoop
    expr: rate(llmsafespace_workspace_controller_restarts_total[10m]) > 0.5
    for: 10m
    labels:
      severity: warning
    annotations:
      summary: "Controller restarting workspace pods at >3/min rate"
      runbook: "Likely network issue between controller and workspace pods. Check NetworkPolicy, CNI health, pod IP reachability."

  # Safe mode image pull failures (registry issue)
  - alert: SafeModeImagePullFailing
    expr: rate(llmsafespace_workspace_safe_mode_entries_total[5m]) > 0 and llmsafespace_workspaces_in_safe_mode_total == 0
    for: 10m
    labels:
      severity: critical
    annotations:
      summary: "Safe mode entries happening but no workspaces in safe mode — image pull likely failing"
      runbook: "Check registry connectivity. Safe-mode image is SHA-pinned; verify digest exists in registry."
```

### Dashboard Panels (Grafana)

```
Row 1: Overview
  - Gauge: Active Healthy / In Recovery / Safe Mode / Degraded (4 single-stat panels)
  - Graph: workspace state distribution over time (stacked area)

Row 2: Failures
  - Graph: failure_classifications_total by class (rate, stacked bar)
  - Graph: unknown_classification_total (rate — should be 0)
  - Table: top 5 pod_reason + container_reason combinations (last 1h)

Row 3: Recovery
  - Graph: recovery_attempts_total by outcome (rate)
  - Histogram: recovery_duration_seconds by class (heatmap)
  - Graph: backoff_applied_seconds by class (p50, p95, p99)

Row 4: Safe Mode
  - Graph: safe_mode_entries_total by trigger (rate)
  - Graph: safe_mode_exits_total by method (rate)
  - Stat: current workspaces in safe mode

Row 5: Health
  - Graph: controller_restarts_total (rate)
  - Graph: time_to_ready_seconds (p50, p95)
  - Graph: workspaces_degraded_total by condition type
```

---

## Test Coverage Matrix

| Component | Unit | Integration | E2E | Observability |
|---|---|---|---|---|
| Failure classifier | 31 tests | via I2-I4, I11 | via E7 | classifications_total + unknown_total + structured log |
| Backoff calculation | 8 tests | via I2, I19 | via E5 | backoff_applied_seconds histogram |
| Recovery state machine | 12 tests | I1-I7, I21-I22 | E1-E5 | recovery_attempts_total + recovery_duration_seconds |
| Safe mode entry/exit | 17 tests | I3-I5, I11-I12, I14, I17-I18, I23-I24 | E7, E9-E10 | safe_mode_entries/exits_total |
| Stability reset | 8 tests | I6, I10 | — | structured log on reset |
| Readiness gate | 4 tests | I8 | — | — |
| Burstable QoS | 7 tests | I9 | E3 | — |
| Image pinning | 5 tests | I25 | E10 | — |
| Secret self-healing | 3 tests | I13 | — | — |
| Degraded detection | 7 tests | I15-I16 | E8 | workspaces_degraded_total |
| File download | 5 tests | — | E9 | — |
| SSE timeout | 4 tests | — | — | — |
| Controller restart tracking | 5 tests | I7, I23 | — | controller_restarts_total |
| Metrics emission | 8 tests | — | E6 | all metrics verified |
| **Total** | **124 tests** | **25 tests** | **10 tests** | **6 alerts + 5 dashboard rows** |

---

## Failure Mode → Test Coverage Mapping

Every failure mode from the enumeration mapped to its test coverage:

| Failure Mode | Unit Test | Integration Test | E2E Test | Alert |
|---|---|---|---|---|
| P1 (PVC API error) | — | — | — | ReconciliationErrorsTotal (existing) |
| P3 (wrong StorageClass) | TestClassify_Config | I4, I11 | — | WorkspacesInRecoveryTooLong |
| P4 (cluster out of storage) | TestClassify_Infra | I19 | — | WorkspacesInRecoveryTooLong |
| C1 (RTE not found) | TestClassify_Config | I4, I11 | E7 | WorkspaceInSafeMode |
| C7 (webhook rejects pod) | TestClassify_Config | I4 | — | WorkspaceInSafeMode |
| C9 (ImagePullBackOff) | TestClassify_Config | I11 | E7 | WorkspaceInSafeMode |
| C11 (Unschedulable) | TestClassify_Infra | I19 | — | WorkspacesInRecoveryTooLong |
| C12 (init container fails) | TestClassify_Process | I3 | — | WorkspaceInSafeMode |
| C17 (slow pull timeout) | TestPending_Pulling_NoTimeout | I20 | — | — |
| A1 (pod vanishes) | TestClassify_Infra | I1, I2 | E1, E2 | — (self-heals) |
| A3 (CrashLoopBackOff) | TestClassify_Process | I3, I14 | — | WorkspaceInSafeMode |
| A4 (health check fails) | TestControllerRestart_* | I7 | E4 | WorkspaceControllerRestartLoop |
| A11 (repeated OOM) | TestClassify_Resource | — | — | WorkspaceInSafeMode |
| A13 (persistent NetPol) | TestControllerRestart_5_SafeMode | I23 | — | WorkspaceControllerRestartLoop |
| A14 (PVC full) | TestDegraded_DiskPressure | I15 | E8 | WorkspacesDegraded |
| A19 (credentials expired) | TestDegraded_NoProviders | I16 | — | WorkspacesDegraded |
| SM1 (safe-mode image pull fail) | — | — | — | SafeModeImagePullFailing |
| SM4 (RWO PVC held) | TestSafeMode_NoPVC_WhenNeverBound | I24 | — | WorkspacesInRecoveryTooLong |
