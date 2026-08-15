// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"time"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

const WorkspaceFinalizer = "workspace.llmsafespaces.dev/finalizer"

// bootstrapAudience is the TokenReview audience for the projected SA token
// (Epic 35). Must match the API handler's TokenReview spec and the agentd
// bootstrap subcommand.
const bootstrapAudience = "llmsafespace-api"

// Pod naming: {workspaceName}-{uid[:8]}
const podNameSuffix = 8

// Requeue intervals.
//
// requeueCreating is the safety-net poll cadence during the Creating phase.
// Pod state changes are already delivered via the Owns(Pod) watch
// (SetupWithManager), and PVC Bound transitions are delivered via
// Owns(PersistentVolumeClaim), so this value only governs the worst-case
// gap between watch events. Was 5s historically; lowered to 2s after the
// 2026-06-23 cold-start audit found it was the dominant source of
// pre-PodScheduled latency. Lowering further is unlikely to help — at
// that point we'd be polling faster than kubelet/longhorn report
// progress.
//
// requeueActive is the cadence at which a healthy Active workspace is
// re-reconciled to refresh egress NetworkPolicies, accumulate billing
// metrics, and run health checks. Independent of cold-start latency.
const (
	requeueCreating = 2 * time.Second
	requeueActive   = 15 * time.Second
)

// pendingPhaseTimeout is how long a workspace can stay in Pending before
// entering recovery backoff.
const pendingPhaseTimeout = 5 * time.Minute

// agentdVerifyFailureRequeue (#863): cadence for re-reconciling a workspace
// whose agentd entrypoint verification failed (exit 81/82). Slow on purpose —
// the failure requires operator action (correct the pin or investigate
// tampering); reconciling faster only re-reads the same terminal state.
const agentdVerifyFailureRequeue = 5 * time.Minute

// stuckScheduledPendingTimeout is how long a pod can remain Pending
// despite being scheduled (PodScheduled=True) before the controller
// declares it stuck and enters recovery. This catches node-level
// volume-mount deadlocks (stale CSI mounts, dead CSI plugins, kubelet
// volume queue saturation) that block container creation without the
// pod ever becoming Unschedulable. The 10-minute window is generous:
// measured workspace startup is ~22s; init containers rarely exceed
// 2 min. A pod that is Pending with zero containers after 10 min has
// a broken node, not a slow startup.
const stuckScheduledPendingTimeout = 10 * time.Minute

// Labels applied to workspace pods.
const (
	LabelApp       = "app"
	LabelComponent = "component"
	LabelWorkspace = "llmsafespaces.dev/workspace"
	LabelRuntime   = "runtime"
	LabelTenant    = "llmsafespaces.dev/tenant"

	AppName            = "llmsafespaces"
	ComponentWorkspace = "workspace"
)

// Password secret naming.
func passwordSecretName(workspaceName string) string {
	return "workspace-pw-" + workspaceName
}

// bootstrapSAName returns the per-workspace ServiceAccount name used for
// secretless credential injection (Epic 35). The SA holds a projected token
// that the init container presents to the API's /internal/v1/pod-bootstrap
// endpoint. Named "workspace-<workspaceName>" so the API can extract the
// workspaceID via strings.TrimPrefix (workspaceIDs are UUIDs — the embedded
// hyphens are safe under TrimPrefix but must never be split on "-").
func bootstrapSAName(workspaceName string) string {
	return "workspace-" + workspaceName
}

// Pod name from workspace name and UID.
func podName(workspaceName string, uid string) string {
	suffix := uid
	if len(suffix) > podNameSuffix {
		suffix = suffix[:podNameSuffix]
	}
	return workspaceName + "-" + suffix
}

// tenantID resolves the tenant identity for a workspace (Epic 51 S51.3).
// Per Design 0031 D4, org members' workspaces are org-attributed.
// tenant_id = Owner.OrgID if set, else Owner.UserID.
func tenantID(owner v1.WorkspaceOwner) string {
	if owner.OrgID != "" {
		return owner.OrgID
	}
	return owner.UserID
}
