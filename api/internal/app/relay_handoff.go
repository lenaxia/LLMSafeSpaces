// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// relay_handoff.go — US-72.4 (design 0058 §4.5): the API-side
// RelayTokenSource. Reads the controller-staged handoff Secret
// (workspace-relay-<wsName>, data key `handoff` — US-72.3 worklog D2)
// from the workspace namespace via the API's existing K8s client, so
// the one builder can swap raw provider keys for scoped relay tokens
// under the relayOnlyKeyDelivery deployment flag.
//
// Semantics the builder relies on:
//   - Secret absent            → (nil, nil): staging not ready (loud
//     degrade; never a raw fallback under flag-on).
//   - Empty/unparseable `handoff` data → (nil, nil)/(nil, err): the
//     same not-ready class — an unusable handoff must not half-deliver.
//   - No token or workspace material is ever logged.

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// relayWorkspaceGetter resolves the K8s Workspace (name + namespace of
// the handoff Secret). Satisfied by k8sWorkspaceGetterAdapter.
type relayWorkspaceGetter interface {
	GetWorkspace(ctx context.Context, id string) (*v1.Workspace, error)
}

// k8sRelayTokenSource is the production RelayTokenSource.
type k8sRelayTokenSource struct {
	workspaces relayWorkspaceGetter
	clientset  kubernetes.Interface
}

func newK8sRelayTokenSource(getter relayWorkspaceGetter, clientset kubernetes.Interface) *k8sRelayTokenSource {
	return &k8sRelayTokenSource{workspaces: getter, clientset: clientset}
}

// RelayHandoff implements secrets.RelayTokenSource.
func (s *k8sRelayTokenSource) RelayHandoff(ctx context.Context, workspaceID string) (*secrets.RelayHandoff, error) {
	ws, err := s.workspaces.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("relay handoff: resolve workspace: %w", err)
	}
	sec, err := s.clientset.CoreV1().Secrets(ws.Namespace).Get(
		ctx, secrets.RelayHandoffSecretName(ws.Name), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Staging has not completed a pass (or the Secret was removed
			// out-of-band): not-ready, never a raw fallback.
			return nil, nil
		}
		return nil, fmt.Errorf("relay handoff: read secret: %w", err)
	}
	data := sec.Data[secrets.RelayHandoffDataKey]
	if len(data) == 0 {
		return nil, nil
	}
	var h secrets.RelayHandoff
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, fmt.Errorf("relay handoff: parse: %w", err)
	}
	return &h, nil
}

// installRelayTokenSource is the app.New seam (US-72.4, design 0058
// §4.5): under the relayOnlyKeyDelivery deployment flag, install the
// k8s relay source on the one builder. Flag off (or a nil service)
// installs NOTHING — byte-identical legacy batches. Extracted from
// app.New so the wiring itself is testable (the #1529 review's missing
// test case 2: the config test covers the env half, the batch tests
// install the source manually — this pins the seam between them).
func installRelayTokenSource(svc *secrets.SecretService, enabled bool, getter relayWorkspaceGetter, clientset kubernetes.Interface) {
	if svc == nil || !enabled {
		return
	}
	svc.SetRelayTokenSource(newK8sRelayTokenSource(getter, clientset))
}

// Compile-time assertion: the source satisfies the builder seam.
var _ secrets.RelayTokenSource = (*k8sRelayTokenSource)(nil)
