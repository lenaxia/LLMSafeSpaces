// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/config"
	"github.com/lenaxia/llmsafespaces/api/internal/services/canary"
	agentd "github.com/lenaxia/llmsafespaces/pkg/agentd"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	k8s "github.com/lenaxia/llmsafespaces/pkg/kubernetes"
)

// canary_adapters.go — the epic-71 / 0c canary's production target
// seams (design recorded in the 0c reserved comment): a class picker
// over the Workspace CRD list (v1: class == spec.runtime selector) and
// a pod resolver twin of the proxy's agentdEndpoint (CRD PodIP + the
// workspace password Secret, port agentd.AgentdPort). Tests drive both
// through the same narrow interfaces the secretsreconcile adapters use.

// canarySecretGetter is the minimal secret-read seam (K8s Secret fetch
// for the workspace password).
type canarySecretGetter interface {
	GetSecret(ctx context.Context, name string) (*corev1.Secret, error)
}

// k8sSecretGetterAdapter is the production canarySecretGetter.
type k8sSecretGetterAdapter struct {
	client    *k8s.Client
	namespace string
}

func (a *k8sSecretGetterAdapter) GetSecret(ctx context.Context, name string) (*corev1.Secret, error) {
	return a.client.Clientset().CoreV1().Secrets(a.namespace).Get(ctx, name, metav1.GetOptions{})
}

// k8sCanaryTargetPicker implements canary.PickTarget: Active-phase
// workspaces whose spec.runtime equals the class, deterministically the
// oldest (ties by name). Deterministic keeps the probe signal stable
// tick-to-tick; oldest biases to the longest-lived pod (least resume
// churn). Phase is not field-selectable on status, so pages are listed
// and filtered in memory, following the k8sActiveWorkspaceLister
// pagination discipline.
type k8sCanaryTargetPicker struct {
	list workspaceCRDListerClient
}

func (p *k8sCanaryTargetPicker) Pick(ctx context.Context, class string) (string, error) {
	var candidates []v1.Workspace
	opts := metav1.ListOptions{Limit: activeWorkspaceListPageSize}
	for {
		list, err := p.list.List(ctx, opts)
		if err != nil {
			return "", fmt.Errorf("list workspaces for canary class %q: %w", class, err)
		}
		for i := range list.Items {
			ws := &list.Items[i]
			if ws.Status.Phase != v1.WorkspacePhaseActive || ws.Spec.Runtime != class {
				continue
			}
			candidates = append(candidates, *ws)
		}
		if list.Continue == "" {
			break
		}
		opts.Continue = list.Continue
	}
	if len(candidates) == 0 {
		return "", nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		ni, nj := candidates[i].CreationTimestamp, candidates[j].CreationTimestamp
		if ni.Time.Equal(nj.Time) {
			return candidates[i].Name < candidates[j].Name
		}
		return ni.Time.Before(nj.Time)
	})
	return candidates[0].Name, nil
}

// k8sCanaryPodResolver implements canary.Resolve: the pod's ABI base
// URL + password (the agentdEndpoint twin — CRD PodIP + §1 secret,
// without the proxy's state-store cache; the canary's 60s cadence does
// not need it).
type k8sCanaryPodResolver struct {
	ws      workspaceCRDGetter
	secrets canarySecretGetter
}

func (r *k8sCanaryPodResolver) Resolve(ctx context.Context, workspaceID string) (string, string, error) {
	ws, err := r.ws.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return "", "", fmt.Errorf("canary: get workspace %s: %w", workspaceID, err)
	}
	if ws.Status.PodIP == "" {
		return "", "", fmt.Errorf("canary: no pod IP for %s (phase %s)", workspaceID, ws.Status.Phase)
	}
	secret, err := r.secrets.GetSecret(ctx, "workspace-pw-"+workspaceID)
	if err != nil {
		return "", "", fmt.Errorf("canary: read password secret for %s: %w", workspaceID, err)
	}
	pw := string(secret.Data["password"])
	if pw == "" {
		return "", "", fmt.Errorf("canary: password secret for %s has empty password key", workspaceID)
	}
	return fmt.Sprintf("http://%s:%d", ws.Status.PodIP, agentd.AgentdPort), pw, nil
}

// newCanaryService builds the canary from config with the production
// seams; nil (idle) unless the knob is on. A construction error fails
// boot — validateCanary already refuses the one operator-reachable
// partial state, so an error here is a wiring defect, and silently
// disabling an explicitly-enabled canary is the false coverage that
// gate exists to prevent (review r1: no fail-open degrade).
func newCanaryService(cfg *config.Config, k8sClient *k8s.Client, log canary.Logger) (*canary.Service, error) {
	return newCanaryServiceWith(cfg,
		(&k8sCanaryTargetPicker{
			list: &k8sWorkspaceListerAdapter{client: k8sClient, namespace: cfg.Kubernetes.Namespace},
		}).Pick,
		(&k8sCanaryPodResolver{
			ws:      &k8sWorkspaceGetterAdapter{client: k8sClient, namespace: cfg.Kubernetes.Namespace},
			secrets: &k8sSecretGetterAdapter{client: k8sClient, namespace: cfg.Kubernetes.Namespace},
		}).Resolve,
		canary.NewAbiClient,
		log,
	)
}

// newCanaryServiceWith is newCanaryService's test seam: identical
// contract, injectable seams (the boot-fail path is only reachable
// through a defective seam set, which production wiring cannot produce).
func newCanaryServiceWith(cfg *config.Config, pick canary.PickTarget, resolve canary.Resolve, newClient canary.NewClient, log canary.Logger) (*canary.Service, error) {
	if cfg == nil || !cfg.Canary.Enabled {
		return nil, nil
	}
	return canary.New(canary.Config{
		Interval:  cfg.Canary.Interval,
		Timeout:   cfg.Canary.Timeout,
		Classes:   cfg.Canary.Classes,
		Pick:      pick,
		Resolve:   resolve,
		NewClient: newClient,
		Logger:    log,
	})
}
