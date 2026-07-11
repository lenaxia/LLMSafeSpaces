# CRD Reference

The platform manages three custom resources in the `llmsafespaces.dev/v1` API group. The authoritative Go types live in [`pkg/apis/llmsafespaces/v1/`](https://github.com/lenaxia/LLMSafeSpaces/blob/main/pkg/apis/llmsafespaces/v1/); the rendered CRD YAMLs are installed from `helm/crds/`.

| Kind | Scope | Short | Status subresource | Purpose |
|---|---|---|---|---|
| [`Workspace`](#workspace) | Namespaced | `ws` | yes | PVC-backed persistent environment + pod running `opencode serve` |
| [`RuntimeEnvironment`](#runtimeenvironment) | Cluster | `rte` | yes | Mapping from runtime name → container image |
| [`InferenceRelay`](#inferencerelay) | Cluster | `irelay` | yes | Opt-in fleet of relay VMs (AWS/OCI/GCP) proxying free-tier inference |

V1 CRDs (`Sandbox`, `SandboxProfile`, `WarmPool`, `WarmPod`) have been removed. `Workspace` absorbs all sandbox and profile functionality.

## CRD type ownership

CRD types exist in two locations with strictly separate roles — they must not be merged:

| Location | Purpose |
|---|---|
| `pkg/apis/llmsafespaces/v1/` | **Authoritative** — kubebuilder-annotated CRD types, used by both controller and API |
| `pkg/types/` | **API transfer objects only** — REST request/response DTOs. Not CRD schemas. |

---

## Workspace

A `Workspace` is the unit of lifecycle management. It owns a PVC and (when active) a pod running `opencode serve`. Suspend deletes the pod and retains the PVC; activate recreates the pod on the existing PVC. See [lifecycle](../architecture/lifecycle.md).

**Scope:** Namespaced · **Short name:** `ws` · **Status subresource:** yes

### Spec

<!-- crd-gen:start:WorkspaceSpec -->

| Field | Type | Default | Description |
|---|---|---|---|
| `owner` | [WorkspaceOwner](#workspaceowner) | *required* | The user (and optionally org) who owns this workspace. |
| `runtime` | string | *required* | Runtime environment name (e.g. `python:3.11`) or an explicit image reference matching the webhook's `allowedImageRegistries`. |
| `architecture` | enum `amd64` \| `arm64` | `amd64` | CPU architecture. Sets a `nodeSelector`. Changing triggers pod recreation. |
| `securityLevel` | enum `standard` \| `high` | `standard` | Deprecated in favor of the composable `securityPolicy` (design 0027). V2.1. |
| `storage` | [WorkspaceStorageConfig](#workspacestorageconfig) | *required* | PVC configuration. |
| `networkAccess` | [WorkspaceNetworkAccess](#workspacenetworkaccess) | *nil* | Network access rules. `securityPolicy.network` (V2.1) supersedes. |
| `autoSuspend` | [WorkspaceAutoSuspend](#workspaceautosuspend) | *nil* | Auto-suspend after idle. |
| `ttlSecondsAfterSuspended` | int64 | `0` | Seconds before a Suspended workspace is auto-deleted. `0` = never. |
| `packages` | [][WorkspacePackageSet](#workspacepackageset) | *nil* | Runtime-specific packages installed by init container on every pod start. Idempotent. |
| `initScript` | string | *nil* | Shell script run by the `workspace-setup` init container before the main container starts. Runs on every pod start (including resume). |
| `maxActiveSessions` | int32 | `5` | Max concurrent active sessions (in-flight or open connections). Range 1–20. Enforced by the API proxy. |
| `credentials` | [WorkspaceCredentialRef](#workspacecredentialref) | *nil* | Reference to a K8s Secret holding agent credentials. |
| `timeout` | int | `0` | Max pod lifetime in seconds. `0` = no limit. Max 86400. |
| `resources` | [ResourceRequirements](#resourcerequirements) | *nil* | Compute resource requests/limits. |
| `restartGeneration` | int64 | `0` | Bump to trigger a pod restart (controller compares against `status.observedRestartGeneration`). |
| `podSecurityContext` | [PodSecurityContext](#podsecuritycontext) | *nil* | Pod security context. `seccompProfile` is deprecated — controller unconditionally sets `RuntimeDefault`. |
| `runtimeClass` | *string | *nil* | Override the container runtime (Epic 51 S51.1). Set to `"runc"` for gVisor opt-out. **Admin-gated**: requires annotation `llmsafespaces.dev/allow-runtime-class-override: "true"` (cluster-admin RBAC). `nil` = use controller default; `""` (empty string) = explicitly clear to kubelet default (runc). |
| `autoApprovePermissions` | bool | `false` | Auto-approve agent permission requests (replies "always" to all `permission.asked` events). |
| `suspend` | *bool | *nil* | Tri-state lifecycle request flag (US-23.3). `nil` = no request; `true` = API requests suspend; `false` = API requests resume. The controller clears to `nil` after acting. **API is the sole writer.** |

<!-- crd-gen:end:WorkspaceSpec -->

#### WorkspaceOwner

| Field | Type | Description |
|---|---|---|
| `userID` | string | *required* Owning user ID. |
| `orgID` | string | Optional owning org ID. When set, the tenant identity (`llmsafespaces.dev/tenant` label) is the org ID; otherwise the user ID. |

#### WorkspaceStorageConfig

| Field | Type | Default | Description |
|---|---|---|---|
| `size` | string | *required* | PVC size. Pattern `^[1-9][0-9]*(Gi|Mi)$`. Capped by webhook `maxWorkspaceStorageGi` (default 1024). |
| `storageClassName` | string | *nil* | StorageClass. Must match `webhooks.allowedStorageClassNames` if non-empty. |
| `accessMode` | enum `ReadWriteOnce` \| `ReadWriteMany` | `ReadWriteOnce` | RWO = one pod at a time (default). RWX requires a storage class that supports it (NFS, CephFS). |

#### WorkspaceNetworkAccess

| Field | Type | Default | Description |
|---|---|---|---|
| `egress` | [][WorkspaceEgressRule](#workspaceegressrule) | *nil* | Allowed egress domains. |
| `ingress` | bool | `false` | Allow ingress to the workspace pod. |

#### WorkspaceEgressRule

| Field | Type | Description |
|---|---|---|
| `domain` | string | *required* Allowed egress domain. |

#### WorkspaceAutoSuspend

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `true` | Enable auto-suspend. |
| `idleTimeoutSeconds` | int64 | `86400` | Idle threshold (seconds). Min 1. |

#### WorkspacePackageSet

| Field | Type | Description |
|---|---|---|
| `runtime` | string | *required* Runtime these packages apply to. |
| `requirements` | []string | *required* Package specifiers. |

#### WorkspaceCredentialRef

| Field | Type | Description |
|---|---|---|
| `secretName` | string | *required* K8s Secret holding agent credentials. |

#### ResourceRequirements

| Field | Type | Default | Description |
|---|---|---|---|
| `cpu` | string | `500m` | CPU request. Capped by webhook `maxWorkspaceCPUMillicores` (default 16000). |
| `memory` | string | `512Mi` | Memory request. Capped by webhook `maxWorkspaceMemoryMi` (default 65536). |
| `cpuPinning` | bool | `false` | CPU pinning. |
| `cpuLimit` | string | *nil* | CPU limit. |
| `memoryLimit` | string | *nil* | Memory limit. |

#### PodSecurityContext

| Field | Type | Description |
|---|---|---|
| `runAsUser` | int64 | UID. |
| `runAsGroup` | int64 | GID. |
| `seccompProfile` | string | **Deprecated** (F1.2.8 / G24). Controller unconditionally sets `RuntimeDefault`. Setting has no effect; retained for API compatibility. |

### Status

<!-- crd-gen:start:WorkspaceStatus -->

Controller-owned except where noted. Each field has exactly one writer (US-23.3 single-writer principle).

| Field | Type | Owner | Description |
|---|---|---|---|
| `phase` | [WorkspacePhase](#workspacephase) | controller | Lifecycle phase. |
| `pvcName` | string | controller | PVC name. |
| `activeSessions` | int32 | controller | Count of active sessions. |
| `suspendedAt` | *metav1.Time | controller | When the workspace entered Suspended. |
| `conditions` | [][WorkspaceCondition](#workspacecondition) | controller | Conditions. |
| `message` | string | controller | Human-readable status message. |
| `observedGeneration` | int64 | controller | Last reconciled generation. |
| `failureReason` | [FailureReason](#failurereason) | controller | Typed enum when `phase == Failed`. |
| `podName` | string | controller | Pod name. |
| `podNamespace` | string | controller | Pod namespace. |
| `podIP` | string | controller | Pod IP. Used by the API proxy. |
| `imageTag` | string | controller | Image tag in use. |
| `endpoint` | string | controller | Internal endpoint. |
| `startTime` | *metav1.Time | controller | Pod start time. |
| `restartCount` | int32 | controller | Pod restart count. |
| `consecutiveFailures` | int32 | controller | Recovery failure counter. |
| `lastFailureClass` | string | controller | Last failure classification. |
| `lastFailureAt` | *metav1.Time | controller | |
| `nextRetryAt` | *metav1.Time | controller | Recovery backoff expiry. |
| `lastStableAt` | *metav1.Time | controller | Stability window anchor. |
| `controllerRestartCount` | int32 | controller | Health-check-driven restarts. |
| `safeMode` | bool | controller | Recovery-exhausted flag. |
| `observedRestartGeneration` | int64 | controller | Last acted-upon `restartGeneration`. |
| `credentialSecretHash` | string | controller | Hash of the credential Secret for drift detection. |
| `lastHealthCheckAt` | *metav1.Time | controller | |
| `consecutiveHealthFailures` | int32 | controller | Health-check failure streak. |
| `lastActivityAt` | *metav1.Time | **deprecated** | Authoritative value is now the `llmsafespaces.dev/last-activity-at` annotation. Retained for migration; read via `GetLastActivityAt()`. |
| `sessions` | [][AgentSessionStatus](#agentsessionstatus) | controller | Agent-reported sessions (from agentd scrape). |
| `diskUsedBytes` / `diskTotalBytes` | int64 | controller | Disk usage (agentd scrape). |
| `memoryUsedBytes` / `memoryTotalBytes` | int64 | controller | Memory usage (cgroup v2). |
| `cpuUsageMicros` | int64 | controller | Cumulative CPU microseconds (cgroup v2 `cpu.stat`). |
| `cpuLimitMicrosPerSec` | int64 | controller | CPU limit. |
| `contextUsed` / `contextTotal` | int64 | controller | Agent context window usage. |
| `userCredsPresent` | *bool | controller | Tri-state: whether agentd's reload-replay cache indicates user-DEK content is materialized. `nil` = unknown, `true` = present, `false` = absent (API may auto-push). |
| `pendingAt` | *metav1.Time | controller | Startup-latency anchor (prefers the `requested-at` annotation). |
| `resumedAt` | *metav1.Time | controller | Resume-latency anchor. |

<!-- crd-gen:end:WorkspaceStatus -->

#### WorkspacePhase

`Pending` · `Creating` · `Active` · `Suspending` · `Suspended` · `Resuming` · `Terminating` · `Terminated` · `Failed`

#### FailureReason

`""` · `TransientPodLoss` · `PodFailedDuringCreation` · `PodBuildFailed` · `PVCBindTimeout` · `PendingTimeout` · `TooManyFailures`

#### WorkspaceConditionType

`Ready` · `PVCReady` · `PodRunning` · `Suspended` · `CredentialsAvailable` · `AgentHealthy` · `ProviderReady` · `DiskPressure` · `MemoryPressure`

#### WorkspaceCondition

| Field | Type | Description |
|---|---|---|
| `type` | [WorkspaceConditionType](#workspaceconditiontype) | |
| `status` | enum `True` \| `False` \| `Unknown` | |
| `lastTransitionTime` | metav1.Time | |
| `reason` | string | |
| `message` | string | |

#### AgentSessionStatus

| Field | Type | Description |
|---|---|---|
| `id` | string | Session ID. |
| `title` | string | Session title. |
| `status` | string | `idle` or `busy`. |
| `contextUsed` | int64 | Context window tokens used. |

### Annotations

| Annotation | Writer | Purpose |
|---|---|---|
| `llmsafespaces.dev/requested-at` | API | RFC3339Nano timestamp set at `POST /workspaces` time. The controller uses it as the start anchor for `WorkspaceCreateDurationSeconds`. |
| `llmsafespaces.dev/last-activity-at` | API | RFC3339 timestamp of last user activity. Authoritative (supersedes `status.lastActivityAt`). Lives in `metadata.annotations` to use the main-resource optimistic-concurrency lane and avoid conflicts with `Status().Update`. |

### Example

```yaml
apiVersion: llmsafespaces.dev/v1
kind: Workspace
metadata:
  name: ws-550e8400-e29b-41d4-a716-446655440000
  namespace: llmsafespaces
  annotations:
    llmsafespaces.dev/requested-at: "2026-07-11T14:30:00Z"
    llmsafespaces.dev/last-activity-at: "2026-07-11T14:35:22Z"
spec:
  owner:
    userID: "user-abc123"
    orgID: "org-acme"
  runtime: "python:3.11"
  architecture: amd64
  storage:
    size: "15Gi"
    accessMode: ReadWriteOnce
  autoSuspend:
    enabled: true
    idleTimeoutSeconds: 86400
  maxActiveSessions: 5
  packages:
    - runtime: python
      requirements: ["requests", "numpy"]
  resources:
    cpu: "500m"
    memory: "512Mi"
status:
  phase: Active
  pvcName: workspace-ws-550e8400
  podName: ws-550e8400-a1b2c3d4
  podIP: 10.42.1.23
  conditions:
    - type: Ready
      status: "True"
      reason: AgentHealthy
```

---

## RuntimeEnvironment

A `RuntimeEnvironment` maps a runtime name (referenced by `Workspace.spec.runtime`) to a container image and metadata. Cluster-scoped so all namespaces share the same runtime catalog.

**Scope:** Cluster · **Short name:** `rte` · **Status subresource:** yes

### Spec

<!-- crd-gen:start:RuntimeEnvironmentSpec -->

| Field | Type | Description |
|---|---|---|
| `image` | string | *required* Container image. |
| `language` | string | *required* Programming language (e.g. `python`, `nodejs`). |
| `version` | string | Language runtime version. |
| `tags` | []string | Categorization tags. |
| `preInstalledPackages` | []string | Packages pre-installed in the image. |
| `packageManager` | string | Default package manager (e.g. `pip`, `npm`). |
| `securityFeatures` | []string | Supported security features. |
| `resourceRequirements` | [RuntimeResourceRequirements](#runtimeresourcerequirements) | Recommended resource requirements. |
| `requiresCredentials` | bool | When true, workspace creation rejects requests where the workspace has no credential Secret. |

<!-- crd-gen:end:RuntimeEnvironmentSpec -->

#### RuntimeResourceRequirements

| Field | Type | Description |
|---|---|---|
| `minCpu` | string | Minimum CPU. |
| `minMemory` | string | Minimum memory. |
| `recommendedCpu` | string | Recommended CPU. |
| `recommendedMemory` | string | Recommended memory. |

### Status

<!-- crd-gen:start:RuntimeEnvironmentStatus -->

| Field | Type | Description |
|---|---|---|
| `available` | bool | Whether this runtime is available for use. |
| `lastValidated` | *metav1.Time | Last validation time. |

<!-- crd-gen:end:RuntimeEnvironmentStatus -->

### Example

```yaml
apiVersion: llmsafespaces.dev/v1
kind: RuntimeEnvironment
metadata:
  name: python-3.11
spec:
  image: ghcr.io/lenaxia/llmsafespaces/base:latest
  language: python
  version: "3.11"
  packageManager: pip
  tags: ["python", "data-science"]
  requiresCredentials: true
  resourceRequirements:
    minCpu: "250m"
    minMemory: "256Mi"
    recommendedCpu: "500m"
    recommendedMemory: "512Mi"
status:
  available: true
```

---

## InferenceRelay

An `InferenceRelay` describes a managed fleet of relay VMs (AWS/OCI/GCP) that proxy free-tier inference so workspace pods never hold the upstream secret. **Opt-in** — disabled unless `controller.inferenceRelay.enabled: true` and requires `rbac.scope=cluster` (cluster-scoped CRD).

See the [inference relay fleet README](https://github.com/lenaxia/LLMSafeSpaces/blob/main/design/stories/epic-42-multi-cloud-inference-relay/README.md) for the full design.

**Scope:** Cluster · **Short name:** `irelay` · **Status subresource:** yes

### Spec

<!-- crd-gen:start:InferenceRelaySpec -->

| Field | Type | Default | Description |
|---|---|---|---|
| `upstreamURL` | string | `https://opencode.ai/zen/v1` | LLM provider endpoint the relays proxy to. Default uses the anonymous `public` key for free Zen models. |
| `providers` | [][RelayProviderSpec](#relayproviderspec) | *required (min 1)* | Relay VM configurations. Default fleet: 1 AWS (paid primary) + 1 OCI (free secondary); GCP optional. |
| `healthCheck` | [HealthCheckConfig](#healthcheckconfig) | see defaults | Active health-checking of relay VMs. |
| `rotation` | [RotationConfig](#rotationconfig) | see defaults | Destroy-and-recreate on 429 detection. |
| `fallback` | [FallbackConfig](#fallbackconfig) | see defaults | Direct-to-upstream routing when all relays are down. |

<!-- crd-gen:end:InferenceRelaySpec -->

#### RelayProviderSpec

| Field | Type | Default | Description |
|---|---|---|---|
| `provider` | enum `aws` \| `oci` \| `gcp` | *required* | Cloud provider. |
| `region` | string | *required* | Provider region. AWS: any. OCI: must be tenancy home region for Always Free. GCP: `us-west1`/`us-central1`/`us-east1` for Always Free. |
| `credentialsRef` | [corev1.LocalObjectReference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#localobjectreference-v1-core) | *required* | K8s Secret in the controller's namespace. Required keys: aws = `accessKeyId, secretAccessKey, region`; oci = `tenancy, user, fingerprint, key, region`; gcp = `service-account-json`. The validating webhook checks existence. |
| `shape` | string | provider default | VM shape. AWS default `t4g.micro` (2 vCPU Graviton2, 1GB, Arm64); OCI `VM.Standard.A1.Flex` (2 OCPU, 12GB, Arm); GCP `e2-micro`. |

#### HealthCheckConfig

| Field | Type | Default | Description |
|---|---|---|---|
| `interval` | metav1.Duration | `15s` | Interval between health checks per VM. |
| `timeout` | metav1.Duration | `5s` | Health-check request timeout. |
| `unhealthyThreshold` | int | `3` | Consecutive failures before marking unhealthy. |
| `replacementTimeout` | metav1.Duration | `15m` | Time to stay unhealthy before destroy + reprovision. |

#### RotationConfig

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `true` | Destroy-and-recreate on 429 storms. |
| `max429Rate` | float64 | `0.5` | 429 fraction (of total responses) that triggers rotation. |
| `detectionWindow` | metav1.Duration | `5m` | Rolling window for counting 429s. |
| `cooldown` | metav1.Duration | `30m` | Minimum time between rotations on the same provider slot. |

#### FallbackConfig

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `true` | Enable direct fallback when all relays are down. `false` = router returns 502 to all requests. |
| `rate` | float64 | `0.5` | Max request rate to upstream in fallback mode (req/s, global). Rate-limited to avoid worsening IP throttling. |
| `maxConcurrent` | int | `1` | Max in-flight requests to upstream in fallback mode. |

### Status

<!-- crd-gen:start:InferenceRelayStatus -->

| Field | Type | Description |
|---|---|---|
| `instances` | [][RelayInstanceStatus](#relayinstancestatus) | Observed state of all managed relay VMs. |
| `healthyReplicas` | int | Count of instances currently passing health checks. |
| `conditions` | []metav1.Condition | Overall fleet health. Types: `Ready`, `Degraded`, `ProvisioningFailed`, `Rotating`, `FallbackActive`. |
| `lastRotation` | *metav1.Time | Time of the most recent destroy-and-recreate. |

<!-- crd-gen:end:InferenceRelayStatus -->

#### RelayInstanceStatus

| Field | Type | Description |
|---|---|---|
| `id` | string | VM ID. |
| `provider` | string | Cloud provider. |
| `region` | string | Region. |
| `publicIP` | string | Public IP. |
| `state` | string | Lifecycle state (see below). |
| `healthy` | bool | Currently passing health checks. |
| `lastCheck` | *metav1.Time | Last health check. |
| `429Count` | int | 429 responses observed. |
| `totalRequests` | int | Total requests observed. |
| `egressBytes` | int64 | Egress bytes. |
| `provisioningAttempts` | int | Provisioning attempts. |
| `lastProvisionError` | string | Last provisioning error. |

**RelayInstanceState:** `provisioning` · `healthy` · `draining` · `unhealthy` · `quota-exhausted` · `terminated` · `provisioning-failed`

### Example

```yaml
apiVersion: llmsafespaces.dev/v1
kind: InferenceRelay
metadata:
  name: default-relay
spec:
  upstreamURL: "https://opencode.ai/zen/v1"
  providers:
    - provider: aws
      region: us-east-1
      credentialsRef:
        name: relay-aws-creds
      shape: t4g.micro
    - provider: oci
      region: us-ashburn-1
      credentialsRef:
        name: relay-oci-creds
  healthCheck:
    interval: 15s
    timeout: 5s
    unhealthyThreshold: 3
    replacementTimeout: 15m
  rotation:
    enabled: true
    max429Rate: 0.5
    detectionWindow: 5m
    cooldown: 30m
  fallback:
    enabled: true
    rate: 0.5
    maxConcurrent: 1
status:
  healthyReplicas: 2
  instances:
    - id: i-0abc123def456
      provider: aws
      region: us-east-1
      state: healthy
      healthy: true
```
