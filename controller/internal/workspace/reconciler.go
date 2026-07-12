package workspace

import (
	"context"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type WorkspaceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// HostResolver is used by the per-workspace NetworkPolicy generator
	// (network_policy.go) to resolve declared FQDNs to /32 ipBlocks at
	// reconcile time. Tests inject a stub; production uses
	// defaultHostResolver (net.DefaultResolver) when nil.
	HostResolver HostResolver

	// InferenceRelayURL is the self-hosted relay URL for free-tier inference
	// (Epic 42 InferenceRelay fleet). When set, workspace pods route opencode
	// free-tier provider requests through this URL for IP distribution. When
	// empty (the chart default), opencode uses its default gateway
	// (opencode.ai/zen/v1) directly with the built-in `public` key.
	InferenceRelayURL string

	// OrgStatusClient, when non-nil AND the workspace belongs to an org
	// (Spec.Owner.OrgID != ""), is consulted on every Active reconcile to drive
	// D20 org-level suspension: if the org is suspended, the workspace
	// transitions Active → Suspending (pod killed, PVC retained). The client is
	// nil when --api-service-url is unset, disabling the feature. Lookups are
	// cached (30s TTL); a lookup failure fails open (workspace keeps running).
	OrgStatusClient OrgStatusClient

	// DefaultRuntimeClass is the container runtime class applied to all
	// workspace pods (Epic 51 S51.1). Typically "gvisor" for production
	// multi-tenant deployments to provide kernel-level isolation against
	// container escape. Empty means use the default (runc). Set via
	// --default-runtime-class controller flag, sourced from Helm
	// .Values.gvisor.defaultRuntimeClass (only set when .Values.gvisor.enabled
	// is true). Individual workspaces can override via spec.runtimeClass for
	// compatibility opt-out (admin-gated).
	DefaultRuntimeClass string

	// APIServiceURL is the in-cluster URL of the API service, used by the
	// workspace init container's bootstrap subcommand (Epic 35 US-35.4) to
	// fetch decrypted credentials via POST /internal/v1/pod-bootstrap. Same
	// value as --api-service-url (also used for OrgStatusClient). When empty,
	// the bootstrap subcommand degrades gracefully (empty secrets, pod boots
	// without credentials; live /v1/reload-secrets push handles delivery).
	APIServiceURL string

	// lastDeepStatus tracks the last time enrichAgentStatus was called per
	// workspace. In-memory only — lost on controller restart (acceptable;
	// the next reconcile will just call it immediately).
	lastDeepStatus   map[string]time.Time
	lastDeepStatusMu sync.Mutex
}

func (r *WorkspaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	start := time.Now()
	logger := log.FromContext(ctx).WithValues("workspace", req.NamespacedName)

	workspace := &v1.Workspace{}
	if err := r.Get(ctx, req.NamespacedName, workspace); err != nil {
		if errors.IsNotFound(err) {
			observeReconcileDuration("Workspace", "ok", time.Since(start))
			return ctrl.Result{}, nil
		}
		countReconcileError("Workspace", "get_failed")
		observeReconcileDuration("Workspace", "error", time.Since(start))
		return ctrl.Result{}, err
	}

	var result ctrl.Result
	var err error

	if !workspace.DeletionTimestamp.IsZero() {
		result, err = r.handleDeletion(ctx, workspace)
	} else {
		switch workspace.Status.Phase {
		case "", v1.WorkspacePhasePending:
			result, err = r.handlePending(ctx, workspace)
		case v1.WorkspacePhaseCreating:
			result, err = r.handleCreating(ctx, workspace)
		case v1.WorkspacePhaseActive:
			result, err = r.handleActive(ctx, workspace)
		case v1.WorkspacePhaseSuspending:
			result, err = r.handleSuspending(ctx, workspace)
		case v1.WorkspacePhaseSuspended:
			result, err = r.handleSuspended(ctx, workspace)
		case v1.WorkspacePhaseResuming:
			result, err = r.handleResuming(ctx, workspace)
		case v1.WorkspacePhaseTerminating:
			result, err = r.handleTerminating(ctx, workspace)
		case v1.WorkspacePhaseFailed:
			result, err = r.handleFailed(ctx, workspace)
		default:
			logger.Info("Unknown workspace phase", "phase", workspace.Status.Phase)
			observeReconcileDuration("Workspace", "ok", time.Since(start))
			return ctrl.Result{}, nil
		}
	}

	if err != nil {
		countReconcileError("Workspace", "phase_handler")
		observeReconcileDuration("Workspace", "error", time.Since(start))
	} else {
		observeReconcileDuration("Workspace", "ok", time.Since(start))
	}
	return result, err
}

func (r *WorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.Workspace{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.ServiceAccount{}).
		// Owning the workspace's PVC means the reconciler is woken
		// immediately on PVC events (Bound, Lost, Pending → Bound).
		// Without this, the Pending phase relies on the reconcile-loop
		// poll interval (requeueCreating) to notice the PVC has bound,
		// adding up to ~5s of dead time on every cold start. With it,
		// the Bound transition triggers a reconcile within milliseconds.
		// Workspace owns its PVC via SetControllerReference at PVC
		// creation in handlePending, so the watch is exact (no
		// cross-workspace fan-out).
		Owns(&corev1.PersistentVolumeClaim{}).
		Complete(r)
}

// sanitizeLabelValue maps a runtime image reference to a valid k8s
// label value. K8s label values must match
// `(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?` (max 63 chars, no
// `/`, `:`, `@`, etc.).
//
// Pre-fix this only replaced `:`. After image-pull-style runtimes
// became common (workspaces with `Spec.Runtime: ghcr.io/.../base:latest`
// — which the G2 webhook now requires), the slashes still in the
// value caused pod-creation kube-apiserver rejection:
//
//	metadata.labels: Invalid value: "ghcr.io/.../base_latest"
//
// We now also replace `/` and `@`, then truncate to 63 chars (k8s
// label-value max) preserving leading + trailing alphanumerics.
func sanitizeLabelValue(s string) string {
	r := strings.NewReplacer(":", "_", "/", "_", "@", "_")
	out := r.Replace(s)
	if len(out) > 63 {
		out = out[len(out)-63:]
	}
	for len(out) > 0 && !isLabelChar(out[0]) {
		out = out[1:]
	}
	for len(out) > 0 && !isLabelChar(out[len(out)-1]) {
		out = out[:len(out)-1]
	}
	if out == "" {
		out = "unspecified"
	}
	return out
}

func isLabelChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// imageTagFromPod extracts the human-readable image tag for the first
// container of a running pod. It prefers the resolved ImageID from
// ContainerStatuses over the requested image in Spec, because the spec
// carries the tag at scheduling time (which may be stale during a rolling
// upgrade), while ImageID reflects what was actually pulled.
//
// ImageID format varies by container runtime:
//
//	docker tag+digest  : "ghcr.io/org/img:ts-123@sha256:<hex>"  → "ts-123"
//	docker tag only    : "ghcr.io/org/img:ts-123"               → "ts-123"
//	containerd digest  : "ghcr.io/org/img@sha256:<hex>"         → no tag; fallback to spec
//	bare digest        : "sha256:<hex>"                         → no tag; fallback to spec
//	empty ImageID      : ""                                     → fallback to spec
//
// In all cases where a tag cannot be determined from ImageID the function
// falls back to parsing Spec.Containers[0].Image, which is always present
// for a schedulable pod.
func imageTagFromPod(pod *corev1.Pod) string {
	if pod == nil || len(pod.Spec.Containers) == 0 {
		return ""
	}

	// Attempt to extract tag from the resolved ImageID.
	if len(pod.Status.ContainerStatuses) > 0 {
		if tag := tagFromImageID(pod.Status.ContainerStatuses[0].ImageID); tag != "" {
			return tag
		}
	}

	// Fallback: parse the spec image (requested tag, not necessarily pulled tag).
	return tagFromSpecImage(pod.Spec.Containers[0].Image)
}

// tagFromImageID extracts the tag portion from a container runtime ImageID.
// Returns "" if the ImageID carries no tag (digest-only or empty).
func tagFromImageID(imageID string) string {
	if imageID == "" {
		return ""
	}
	// Strip digest suffix (everything from "@" onward) so that
	// "ghcr.io/org/img:ts-123@sha256:abc" becomes "ghcr.io/org/img:ts-123".
	// Using "@" rather than "@sha256:" handles any digest algorithm (sha512,
	// etc.) even though sha256 is the only algorithm used by current runtimes.
	ref := imageID
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	// If nothing remains after digest strip, or the result is itself a bare
	// digest (containerd records "sha256:<hex>" with no registry prefix),
	// there is no tag.
	if ref == "" || strings.Contains(ref, ":") && !strings.Contains(ref, "/") {
		// bare digest: looks like "sha256:abc" — colon present but no slash
		return ""
	}
	// Extract tag after the last colon, but only if the colon is not part of
	// a port number in a registry host (e.g. "registry.local:5000/img" has no
	// tag). A tag colon must appear after the last "/" in the path.
	lastSlash := strings.LastIndex(ref, "/")
	lastColon := strings.LastIndex(ref, ":")
	if lastColon > lastSlash {
		return ref[lastColon+1:]
	}
	return ""
}

// tagFromSpecImage extracts the tag from a spec image reference, returning
// the full reference if no tag separator is found (untagged image).
func tagFromSpecImage(image string) string {
	if image == "" {
		return ""
	}
	lastSlash := strings.LastIndex(image, "/")
	lastColon := strings.LastIndex(image, ":")
	if lastColon > lastSlash {
		return image[lastColon+1:]
	}
	return image
}

// --- Operations Metrics ---

var workspacePhaseTransitions = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "llmsafespaces_workspace_phase_transitions_total",
	Help: "Workspace phase transitions observed by the controller.",
}, []string{"from_phase", "to_phase"})
