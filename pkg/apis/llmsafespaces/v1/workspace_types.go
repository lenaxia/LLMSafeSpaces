// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package v1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WorkspaceOwner identifies the user who owns a Workspace.
type WorkspaceOwner struct {
	UserID string `json:"userID"`
	OrgID  string `json:"orgID,omitempty"`
}

// Annotation keys written by the API server and read by the controller.
const (
	// AnnotationRequestedAt is set by the API at POST /workspaces time
	// (RFC3339Nano). The controller uses it as the start anchor for
	// WorkspaceCreateDurationSeconds so the measurement covers the full
	// user-perceived latency, not just the controller reconcile latency.
	AnnotationRequestedAt = "llmsafespaces.dev/requested-at"

	// AnnotationLastActivityAt stores the last user-activity timestamp
	// (RFC3339). Written by the API service (activity tracker + activate
	// flow); read by the controller for idle auto-suspend. Lives in
	// metadata.annotations so it uses the main-resource optimistic-
	// concurrency lane and never conflicts with Status().Update
	// (US-23.3 single-writer migration).
	AnnotationLastActivityAt = "llmsafespaces.dev/last-activity-at"
)

// WorkspaceStorageConfig defines PVC configuration for a Workspace.
type WorkspaceStorageConfig struct {
	// +kubebuilder:validation:Pattern=^[1-9][0-9]*(Gi|Mi)$
	Size             string `json:"size"`
	StorageClassName string `json:"storageClassName,omitempty"`
	// +kubebuilder:validation:Enum=ReadWriteOnce;ReadWriteMany
	// +kubebuilder:default=ReadWriteOnce
	AccessMode string `json:"accessMode,omitempty"`
}

// WorkspacePackageSet defines runtime-specific packages installed on every pod start.
type WorkspacePackageSet struct {
	Runtime      string   `json:"runtime"`
	Requirements []string `json:"requirements"`
}

// WorkspaceNetworkAccess defines network access rules for workspace pods.
type WorkspaceNetworkAccess struct {
	Egress []WorkspaceEgressRule `json:"egress,omitempty"`
	// +kubebuilder:default=false
	Ingress bool `json:"ingress,omitempty"`
}

// WorkspaceEgressRule defines an egress domain rule.
type WorkspaceEgressRule struct {
	Domain string `json:"domain"`
}

// WorkspaceAutoSuspend configures automatic workspace suspension after idle.
type WorkspaceAutoSuspend struct {
	// +kubebuilder:default=true
	Enabled bool `json:"enabled,omitempty"`
	// +kubebuilder:default=86400
	// +kubebuilder:validation:Minimum=1
	IdleTimeoutSeconds int64 `json:"idleTimeoutSeconds,omitempty"`
}

// WorkspaceCredentialRef refers to a Kubernetes Secret holding agent credentials.
type WorkspaceCredentialRef struct {
	SecretName string `json:"secretName"`
}

// PodSecurityContext defines security context for the workspace pod.
type PodSecurityContext struct {
	RunAsUser  int64 `json:"runAsUser,omitempty"`
	RunAsGroup int64 `json:"runAsGroup,omitempty"`
	// SeccompProfile is DEPRECATED in v1 (F1.2.8 / G24, Epic 17):
	// the controller unconditionally sets RuntimeDefault on the
	// generated PodSecurityContext to enforce the same hardening
	// baseline across every workspace. Setting this field has no
	// effect; it remains for API compatibility.
	SeccompProfile string `json:"seccompProfile,omitempty"`
}

// ResourceRequirements defines compute resource requirements for the workspace pod.
type ResourceRequirements struct {
	// +kubebuilder:validation:Pattern=^([1-9][0-9]*m|[1-9][0-9]*\.[0-9]+|0\.[0-9]*[1-9][0-9]*)$
	// +kubebuilder:default="500m"
	CPU string `json:"cpu,omitempty"`
	// +kubebuilder:validation:Pattern=^[1-9][0-9]*(Ki|Mi|Gi)$
	// +kubebuilder:default="512Mi"
	Memory     string `json:"memory,omitempty"`
	CPUPinning bool   `json:"cpuPinning,omitempty"`
	// +kubebuilder:validation:Pattern=^([1-9][0-9]*m|[1-9][0-9]*\.[0-9]+|0\.[0-9]*[1-9][0-9]*)$
	CPULimit string `json:"cpuLimit,omitempty"`
	// +kubebuilder:validation:Pattern=^[1-9][0-9]*(Ki|Mi|Gi)$
	MemoryLimit string `json:"memoryLimit,omitempty"`
}

// WorkspaceSpec defines the desired state of a Workspace.
type WorkspaceSpec struct {
	Owner WorkspaceOwner `json:"owner"`

	// Runtime is the runtime environment (e.g. "python:3.11").
	Runtime string `json:"runtime"`

	// Architecture is the CPU architecture for the workspace pod.
	// The controller sets a nodeSelector to schedule on matching nodes.
	// Changing this field triggers a pod recreation.
	// +kubebuilder:validation:Enum=amd64;arm64
	// +kubebuilder:default=amd64
	Architecture string `json:"architecture,omitempty"`

	// +kubebuilder:validation:Enum=standard;high
	// +kubebuilder:default=standard
	SecurityLevel string `json:"securityLevel,omitempty"`

	Storage       WorkspaceStorageConfig  `json:"storage"`
	NetworkAccess *WorkspaceNetworkAccess `json:"networkAccess,omitempty"`
	AutoSuspend   *WorkspaceAutoSuspend   `json:"autoSuspend,omitempty"`

	// +kubebuilder:default=0
	TTLSecondsAfterSuspended int64 `json:"ttlSecondsAfterSuspended,omitempty"`

	Packages   []WorkspacePackageSet `json:"packages,omitempty"`
	InitScript string                `json:"initScript,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=20
	// +kubebuilder:default=5
	MaxActiveSessions int32 `json:"maxActiveSessions,omitempty"`

	Credentials *WorkspaceCredentialRef `json:"credentials,omitempty"`

	// Pod lifecycle fields (absorbed from Sandbox):

	// Timeout is the max pod lifetime in seconds. 0 = no limit.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=86400
	Timeout int `json:"timeout,omitempty"`

	Resources         *ResourceRequirements `json:"resources,omitempty"`
	RestartGeneration int64                 `json:"restartGeneration,omitempty"`

	PodSecurityContext *PodSecurityContext `json:"podSecurityContext,omitempty"`

	// RuntimeClass overrides the container runtime for this workspace
	// (Epic 51 S51.1). Used for per-workspace gVisor opt-out: set to "runc"
	// to disable the default gVisor sandbox for workloads incompatible with
	// gVisor (ptrace debuggers, certain seccomp filters). Empty means use
	// the controller's DefaultRuntimeClass (typically "gvisor" in production,
	// empty/runc in dev).
	//
	// Enforcement: webhook validation to prevent tenants from setting this
	// field via direct kubectl is deferred to S51.2. Today the API's
	// CreateWorkspaceRequest does not expose the field (mitigating the API
	// path), but direct kubectl users can set it.
	RuntimeClass *string `json:"runtimeClass,omitempty"`

	// AutoApprovePermissions controls whether permission requests from the agent
	// are automatically approved without user interaction. When true, the backend
	// replies "always" to all permission.asked events. Default: false.
	// +kubebuilder:default=false
	AutoApprovePermissions bool `json:"autoApprovePermissions,omitempty"`

	// Suspend is a tri-state request flag for workspace lifecycle control
	// (US-23.3). It uses a pointer so that "field absent" (nil) is
	// distinguishable from "explicitly set to false":
	//
	//   - nil   : request acknowledged or never set. The controller does NOT
	//             use this to make resume decisions. This is the state of
	//             workspaces created before the migration, and the state after
	//             the controller has consumed a suspend/resume request.
	//   - true  : API requests suspension. handleActive transitions to Suspending,
	//             then clears the flag.
	//   - false : API requests resume from Suspended. handleSuspended transitions
	//             to Resuming, then clears the flag.
	//
	// The controller MUST clear this field (set to nil) after acting on it,
	// otherwise a stale &false would cause handleSuspended to immediately
	// resume after any controller-initiated suspend (idle/timeout/TTL),
	// creating an infinite suspend/resume loop.
	//
	// The API service is the sole writer of this field (when non-nil); the
	// controller is the sole writer of Status.Phase and the sole writer of
	// the nil-clear. This eliminates the cross-writer race that was the root
	// cause of multiple incidents (see Epic 23 Story 3 design).
	// +kubebuilder:validation:Optional
	// +nullable
	Suspend *bool `json:"suspend,omitempty"`
}

// WorkspacePhase represents the lifecycle phase of a Workspace.
type WorkspacePhase string

const (
	WorkspacePhasePending     WorkspacePhase = "Pending"
	WorkspacePhaseCreating    WorkspacePhase = "Creating"
	WorkspacePhaseActive      WorkspacePhase = "Active"
	WorkspacePhaseSuspending  WorkspacePhase = "Suspending"
	WorkspacePhaseSuspended   WorkspacePhase = "Suspended"
	WorkspacePhaseResuming    WorkspacePhase = "Resuming"
	WorkspacePhaseTerminating WorkspacePhase = "Terminating"
	WorkspacePhaseTerminated  WorkspacePhase = "Terminated"
	WorkspacePhaseFailed      WorkspacePhase = "Failed"
)

// FailureReason is a typed enum identifying why a workspace entered Failed.
// Operators and frontend can switch on this for per-cause UX and alerting.
type FailureReason string

const (
	FailureReasonNone                    FailureReason = ""
	FailureReasonTransientPodLoss        FailureReason = "TransientPodLoss"
	FailureReasonPodFailedDuringCreation FailureReason = "PodFailedDuringCreation"
	FailureReasonPodBuildFailed          FailureReason = "PodBuildFailed"
	FailureReasonPVCBindTimeout          FailureReason = "PVCBindTimeout"
	FailureReasonPendingTimeout          FailureReason = "PendingTimeout"
	FailureReasonTooManyFailures         FailureReason = "TooManyFailures"
)

type PVCState string

const (
	PVCStateNone    PVCState = ""        // no PVC yet
	PVCStateCluster PVCState = "cluster" // PVC exists on cluster
	PVCStateS3      PVCState = "s3"      // PVC offloaded to S3
)

// WorkspaceConditionType identifies a condition on a Workspace.
type WorkspaceConditionType string

const (
	WorkspaceConditionReady                WorkspaceConditionType = "Ready"
	WorkspaceConditionPVCReady             WorkspaceConditionType = "PVCReady"
	WorkspaceConditionPodRunning           WorkspaceConditionType = "PodRunning"
	WorkspaceConditionSuspended            WorkspaceConditionType = "Suspended"
	WorkspaceConditionCredentialsAvailable WorkspaceConditionType = "CredentialsAvailable"
	WorkspaceConditionAgentHealthy         WorkspaceConditionType = "AgentHealthy"
	WorkspaceConditionProviderReady        WorkspaceConditionType = "ProviderReady"
	WorkspaceConditionDiskPressure         WorkspaceConditionType = "DiskPressure"
	WorkspaceConditionMemoryPressure       WorkspaceConditionType = "MemoryPressure"
)

const (
	ReasonCredentialsValid          = "CredentialsValid"
	ReasonCredentialSecretNotFound  = "CredentialSecretNotFound"
	ReasonCredentialEmpty           = "CredentialEmpty"
	ReasonCredentialInvalid         = "CredentialInvalid"
	ReasonCredentialCheckError      = "CredentialCheckError"
	ReasonCredentialValidationError = "CredentialValidationError"
)

const (
	ReasonAgentHealthy          = "AgentHealthy"
	ReasonAgentUnhealthy        = "AgentUnhealthy"
	ReasonAgentDegraded         = "AgentDegraded"
	ReasonHealthCheckFailed     = "HealthCheckFailed"
	ReasonProvidersReady        = "ProvidersReady"
	ReasonProvidersNotConnected = "ProvidersNotConnected"
	ReasonDiskPressure          = "DiskPressure"
	ReasonMemoryPressure        = "MemoryPressure"
)

// WorkspaceCondition describes a condition of a Workspace.
type WorkspaceCondition struct {
	Type WorkspaceConditionType `json:"type"`
	// +kubebuilder:validation:Enum=True;False;Unknown
	Status             string      `json:"status"`
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
	Reason             string      `json:"reason,omitempty"`
	Message            string      `json:"message,omitempty"`
}

// AgentSessionStatus describes a session reported by the workspace agent.
type AgentSessionStatus struct {
	ID          string `json:"id"`
	Title       string `json:"title,omitempty"`
	Status      string `json:"status"` // "idle" | "busy"
	ContextUsed int64  `json:"contextUsed"`
}

// WorkspaceStatus defines the observed state of a Workspace.
// Ownership is documented per-field to enforce the single-writer principle
// (US-23.3): each field has exactly one owner, eliminating cross-owner
// optimistic-concurrency conflicts.
type WorkspaceStatus struct {
	// Controller-owned: written by the controller's reconcile loop only.
	Phase              WorkspacePhase       `json:"phase,omitempty"`
	PVCName            string               `json:"pvcName,omitempty"`
	ActiveSessions     int32                `json:"activeSessions,omitempty"`
	SuspendedAt        *metav1.Time         `json:"suspendedAt,omitempty"`
	Conditions         []WorkspaceCondition `json:"conditions,omitempty"`
	Message            string               `json:"message,omitempty"`
	ObservedGeneration int64                `json:"observedGeneration,omitempty"`

	// FailureReason provides a typed enum for programmatic consumers when
	// Phase == Failed. Operators and frontend can switch on this without
	// parsing free-form Message strings. Empty when not in Failed phase.
	FailureReason FailureReason `json:"failureReason,omitempty"`

	// Pod status fields (absorbed from Sandbox) — controller-owned:
	PodName                   string       `json:"podName,omitempty"`
	PodNamespace              string       `json:"podNamespace,omitempty"`
	PodIP                     string       `json:"podIP,omitempty"`
	ImageTag                  string       `json:"imageTag,omitempty"`
	Endpoint                  string       `json:"endpoint,omitempty"`
	StartTime                 *metav1.Time `json:"startTime,omitempty"`
	RestartCount              int32        `json:"restartCount,omitempty"`
	ConsecutiveFailures       int32        `json:"consecutiveFailures,omitempty"`
	LastFailureClass          string       `json:"lastFailureClass,omitempty"`
	LastFailureAt             *metav1.Time `json:"lastFailureAt,omitempty"`
	NextRetryAt               *metav1.Time `json:"nextRetryAt,omitempty"`
	LastStableAt              *metav1.Time `json:"lastStableAt,omitempty"`
	ControllerRestartCount    int32        `json:"controllerRestartCount,omitempty"`
	SafeMode                  bool         `json:"safeMode,omitempty"`
	ObservedRestartGeneration int64        `json:"observedRestartGeneration,omitempty"`
	CredentialSecretHash      string       `json:"credentialSecretHash,omitempty"`
	LastHealthCheckAt         *metav1.Time `json:"lastHealthCheckAt,omitempty"`
	ConsecutiveHealthFailures int32        `json:"consecutiveHealthFailures,omitempty"`

	// LastActivityAt is DEPRECATED (US-23.3). The authoritative value is
	// now the metadata annotation llmsafespaces.dev/last-activity-at,
	// written by the API service. This field is retained for backward
	// compatibility during the migration window but is no longer written
	// to by any code path. New readers MUST use GetLastActivityAt().
	LastActivityAt *metav1.Time `json:"lastActivityAt,omitempty"`

	// Agent-reported fields (populated from agentd /v1/statusz scrape) — controller-owned:
	Sessions         []AgentSessionStatus `json:"sessions,omitempty"`
	DiskUsedBytes    int64                `json:"diskUsedBytes,omitempty"`
	DiskTotalBytes   int64                `json:"diskTotalBytes,omitempty"`
	MemoryUsedBytes  int64                `json:"memoryUsedBytes,omitempty"`
	MemoryTotalBytes int64                `json:"memoryTotalBytes,omitempty"`
	// CpuUsageMicros is cumulative CPU microseconds from cgroup v2 cpu.stat.
	// Stored so enrichAgentStatus can compute delta on the next poll.
	CpuUsageMicros       int64 `json:"cpuUsageMicros,omitempty"`
	CpuLimitMicrosPerSec int64 `json:"cpuLimitMicrosPerSec,omitempty"`
	ContextUsed          int64 `json:"contextUsed"`
	ContextTotal         int64 `json:"contextTotal"`

	// UserCredsPresent (worklog 0591) reports whether agentd's
	// last-reload-secrets.json cache indicates that user-DEK content
	// has been materialized on the pod. Populated by the controller
	// on every health scrape from agentd's /v1/healthz. A pointer for
	// tri-state:
	//
	//   nil   : the controller has not scraped agentd yet, or the
	//           pod isn't reachable. The API's watcher-driven auto-push
	//           MUST treat nil as "unknown" and skip firing — a phase
	//           transition alone is not enough signal.
	//   true  : agentd reports at least one user-DEK entry materialized.
	//           No push needed.
	//   false : agentd reports no user-DEK content. The API's watcher
	//           fires a background auto-push if the workspace has any
	//           user_secret_bindings.
	//
	// Cleared to nil when the pod becomes unreachable so a stale "true"
	// from a previous pod doesn't suppress the push after recreation.
	UserCredsPresent *bool `json:"userCredsPresent,omitempty"`

	// ---- Startup latency measurement anchors (S18.10) ----
	//
	// PendingAt is set by the controller on the first Pending-phase reconcile.
	// The controller prefers the llmsafespaces.dev/requested-at annotation
	// written by the API at POST /workspaces time (so the measurement starts
	// from the user request, not the controller reconcile). Cleared by the
	// controller after WorkspaceCreateDurationSeconds is recorded.
	//
	// Stale-anchor protection: if more than maxStartupAnchorAge elapses
	// between PendingAt and the Active transition (e.g. after a controller
	// restart), the observation is dropped and the field cleared to avoid
	// inflating the histogram with multi-hour values.
	PendingAt *metav1.Time `json:"pendingAt,omitempty"`

	// ResumedAt is set by the controller when the workspace enters Resuming
	// phase. Cleared after WorkspaceResumeDurationSeconds is recorded.
	// Same stale-anchor protection as PendingAt.
	ResumedAt *metav1.Time `json:"resumedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ws
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Runtime",type="string",JSONPath=".spec.runtime"
// +kubebuilder:printcolumn:name="Storage",type="string",JSONPath=".spec.storage.size"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Workspace is the Schema for the workspaces API.
type Workspace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkspaceSpec   `json:"spec,omitempty"`
	Status WorkspaceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WorkspaceList contains a list of Workspace.
type WorkspaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Workspace `json:"items"`
}

// GetLastActivityAt reads the last-activity timestamp from the
// metadata annotation (authoritative) with a fallback to the
// deprecated Status.LastActivityAt field for workspaces created
// before the migration (US-23.3).
func GetLastActivityAt(ws *Workspace) *metav1.Time {
	if ws.Annotations != nil {
		if v, ok := ws.Annotations[AnnotationLastActivityAt]; ok && v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				mt := metav1.NewTime(t)
				return &mt
			}
		}
	}
	return ws.Status.LastActivityAt
}

// SetLastActivityAtAnnotation writes the last-activity timestamp into
// the metadata annotation. The caller must ensure the map is initialized
// (use EnsureAnnotations).
func SetLastActivityAtAnnotation(annotations map[string]string, t metav1.Time) {
	annotations[AnnotationLastActivityAt] = t.Format(time.RFC3339)
}
