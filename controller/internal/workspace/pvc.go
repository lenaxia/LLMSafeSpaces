package workspace

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func (r *WorkspaceReconciler) isPVCStale(pvc *corev1.PersistentVolumeClaim, workspace *v1.Workspace) bool {
	if !pvc.DeletionTimestamp.IsZero() {
		return true
	}
	if len(pvc.OwnerReferences) > 0 {
		for _, owner := range pvc.OwnerReferences {
			if owner.UID == workspace.UID {
				return false
			}
		}
		return true
	}
	return false
}

func (r *WorkspaceReconciler) pendingTimedOut(workspace *v1.Workspace) bool {
	return !workspace.CreationTimestamp.IsZero() && time.Since(workspace.CreationTimestamp.Time) > pendingPhaseTimeout
}

func (r *WorkspaceReconciler) pvcUsesWaitForFirstConsumer(ctx context.Context, pvc *corev1.PersistentVolumeClaim) bool {
	scName := ""
	if pvc.Spec.StorageClassName != nil {
		scName = *pvc.Spec.StorageClassName
	}
	if scName == "" {
		return false
	}
	sc := &storagev1.StorageClass{}
	if err := r.directReader().Get(ctx, types.NamespacedName{Name: scName}, sc); err != nil {
		return false
	}
	if sc.VolumeBindingMode == nil {
		return false
	}
	return *sc.VolumeBindingMode == storagev1.VolumeBindingWaitForFirstConsumer
}

func (r *WorkspaceReconciler) buildPVC(workspace *v1.Workspace, pvcName string) *corev1.PersistentVolumeClaim {
	storageSize := resource.MustParse(workspace.Spec.Storage.Size)
	accessMode := corev1.ReadWriteOnce
	if workspace.Spec.Storage.AccessMode == "ReadWriteMany" {
		accessMode = corev1.ReadWriteMany
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: workspace.Namespace,
			Labels: map[string]string{
				LabelApp:       AppName,
				LabelComponent: ComponentWorkspace,
				LabelWorkspace: workspace.Name,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{accessMode},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: storageSize},
			},
		},
	}
	if workspace.Spec.Storage.StorageClassName != "" {
		pvc.Spec.StorageClassName = &workspace.Spec.Storage.StorageClassName
	}
	return pvc
}

// --- Pod building ---
