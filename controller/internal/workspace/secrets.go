package workspace

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/lenaxia/llmsafespaces/controller/internal/common"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func (r *WorkspaceReconciler) deletePodByName(ctx context.Context, name, namespace string) {
	pod := &corev1.Pod{}
	pod.Name = name
	pod.Namespace = namespace
	_ = r.Delete(ctx, pod)
}

// cleanupFailedWorkspaceSecrets deletes the per-workspace K8s Secrets
// (`workspace-creds-*`, `workspace-pw-*`) when a workspace has entered the
// Failed phase. The Secrets are useless once the workspace is non-recoverable;
// leaving them behind is the symptom of Bug 12 in worklog 0085 (Secrets
// persisting 45+ hours after pod disappeared). Each Get is best-effort:
// missing-Secret is the desired end state.
//
// Epic 35: workspace-secrets-* is no longer created (secretless injection),
// so it is no longer in the cleanup list.
func (r *WorkspaceReconciler) cleanupFailedWorkspaceSecrets(ctx context.Context, workspace *v1.Workspace) {
	logger := log.FromContext(ctx)
	for _, secretName := range []string{
		fmt.Sprintf("workspace-creds-%s", workspace.Name),
		fmt.Sprintf("workspace-pw-%s", workspace.Name),
	} {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: workspace.Namespace}, secret); err != nil {
			continue // already gone or not found
		}
		if err := r.Delete(ctx, secret); err != nil {
			logger.V(1).Info("Failed to delete Secret for Failed workspace",
				"name", secretName, "error", err.Error())
		}
	}
}

func (r *WorkspaceReconciler) ensurePasswordSecret(ctx context.Context, workspace *v1.Workspace) error {
	name := passwordSecretName(workspace.Name)
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: workspace.Namespace}, secret); err == nil {
		// #887 D5.1 / design 0051 US-3 (Q3): converge legacy Secrets onto
		// the DISTINCT admin-token and agentdPassword keys. Generated
		// once, never rotated in place — running pods hold the accepted
		// values in agentd memory while rebuilt probe specs / API-server
		// credential reads diverge; in-place rotation desyncs them.
		changed := false
		if secret.Data == nil {
			// A Secret created without any data carries a nil map;
			// assignment would panic (F4, review round 1).
			secret.Data = map[string][]byte{}
		}
		if _, ok := secret.Data["admin-token"]; !ok {
			secret.Data["admin-token"] = []byte(common.GenerateRandomString(32))
			changed = true
		}
		// US-3: the control-plane Basic secret (§D1 per-endpoint table).
		// Same upsert-once rule. Its absence in a sidecar-enabled build
		// would leave the pod failing on a missing Secret key, so
		// convergence happens here — in handlePending, BEFORE buildPod.
		if _, ok := secret.Data["agentdPassword"]; !ok {
			secret.Data["agentdPassword"] = []byte(common.GenerateRandomString(32))
			changed = true
		}
		if !changed {
			return nil
		}
		return r.Update(ctx, secret)
	}
	password := common.GenerateRandomString(32)
	newSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: workspace.Namespace,
		},
		Data: map[string][]byte{
			"password":       []byte(password),
			"admin-token":    []byte(common.GenerateRandomString(32)),
			"agentdPassword": []byte(common.GenerateRandomString(32)),
		},
	}
	if err := controllerutil.SetControllerReference(workspace, newSecret, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, newSecret)
}

// ensureWorkspaceServiceAccount creates the per-workspace ServiceAccount used
// for secretless credential injection (Epic 35 US-35.1). The SA holds no
// secrets itself — its only purpose is to be the identity behind the
// projected token volume mounted into the init container. The init container
// presents that token to the API's /internal/v1/pod-bootstrap endpoint, which
// validates it via TokenReview and verifies the SA name matches the claimed
// workspaceID.
//
// AutomountServiceAccountToken is explicitly false: only the explicit projected
// token volume (added in US-35.4) is used, never the default automounted token.
// This preserves G17 — the main container never sees any SA token.
//
// OwnerReference ensures the SA is garbage-collected when the Workspace CRD is
// deleted. The SA survives suspend (which only deletes the pod) and is GC'd
// only on terminate (CRD deletion).
//
// Idempotent: if the SA already exists, returns nil immediately — same pattern
// as ensurePasswordSecret. Called from handlePending (first creation) and
// handleCreating (resume coverage, per adversarial finding F4).
func (r *WorkspaceReconciler) ensureWorkspaceServiceAccount(ctx context.Context, workspace *v1.Workspace) error {
	name := bootstrapSAName(workspace.Name)
	sa := &corev1.ServiceAccount{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: workspace.Namespace}, sa); err == nil {
		return nil
	}
	falseVal := false
	newSA := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: workspace.Namespace,
		},
		AutomountServiceAccountToken: &falseVal,
	}
	if err := controllerutil.SetControllerReference(workspace, newSA, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, newSA)
}

// --- PVC helpers ---
