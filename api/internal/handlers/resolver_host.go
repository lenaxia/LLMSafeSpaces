// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"fmt"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/services/wsstate"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

// ResolverHost owns the connection-resolution infrastructure the Agent
// Adapter consumes: workspace pod-IP resolution and workspace password
// resolution, backed by the SHARED per-workspace state store. It exists
// so the adapter can be constructed BEFORE the ProxyHandler (the
// handler's ctor takes the adapter as a required parameter — #828 final
// batch) while keeping the password cache and its suspend/restart
// invalidation in ONE place: app.go builds the host first, builds the
// adapter over it, passes the adapter to the handler ctor, and adopts
// the host via SetResolverHost. A late SetStateStore (Redis swap)
// forwards into the host, so the adapter's resolver sees the same
// store the handler does.
//
// Pre-Start mutation only (SetStateStore): request goroutines read the
// store without synchronization — same invariant the handler has always
// had.
type ResolverHost struct {
	k8sClient  pkginterfaces.KubernetesClient
	namespace  string
	stateStore wsstate.Store
	// stateMu guards stateStore's lazy init (#1641 r2: State()'s
	// unsynchronized nil-write raced under concurrent GetPassword —
	// the frame the r1 reviewer reproduced twice on the first fix's
	// head). Same double-checked pattern as ProxyHandler.host().
	stateMu sync.RWMutex
}

// NewResolverHost constructs a standalone host with an in-memory state
// store.
func NewResolverHost(k8sClient pkginterfaces.KubernetesClient, namespace string) *ResolverHost {
	return &ResolverHost{k8sClient: k8sClient, namespace: namespace}
}

// State returns the state store, initializing it lazily under the lock
// (struct-literal constructions bypass the normal wiring; production
// always sets one — but the read must be race-free either way).
func (r *ResolverHost) State() wsstate.Store {
	r.stateMu.RLock()
	s := r.stateStore
	r.stateMu.RUnlock()
	if s != nil {
		return s
	}
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if r.stateStore == nil {
		r.stateStore = wsstate.NewInMemoryStore()
	}
	return r.stateStore
}

// SetStateStore swaps the backing store. Pre-Start by convention; the
// write lock makes that invariant enforced rather than assumed (#1641
// r2 discipline: no unguarded writes to shared resolver state remain).
func (r *ResolverHost) SetStateStore(store wsstate.Store) {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	r.stateStore = store
}

// GetPassword resolves the workspace password: cache-first against the
// state store, K8s Secret fetch on miss, caching the result.
func (r *ResolverHost) GetPassword(ctx context.Context, workspaceID string) (string, error) {
	// Cache-only lookup against the state store; the K8s Secret fetch
	// fallback stays local so the store remains pure-state with no I/O
	// dependencies (US-45.4's Redis-swap property).
	if pw, ok := r.State().GetCachedPassword(ctx, workspaceID); ok {
		return pw, nil
	}

	secretName := fmt.Sprintf("workspace-pw-%s", workspaceID)
	secret, err := r.k8sClient.Clientset().CoreV1().Secrets(r.namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading password secret %s: %w", secretName, err)
	}

	pw := string(secret.Data["password"])
	if pw == "" {
		return "", fmt.Errorf("password secret %s has empty password key", secretName)
	}

	r.State().SetCachedPassword(ctx, workspaceID, pw)
	return pw, nil
}

// GetWorkspacePodIP resolves the workspace's pod IP from the K8s CRD
// status. Satisfies WorkspacePodIPResolver (the userID parameter is
// accepted per the Adapter's interface contract but not used — the
// lookup is namespace-scoped, not user-scoped).
func (r *ResolverHost) GetWorkspacePodIP(ctx context.Context, _, workspaceID string) (string, error) {
	v1Client, err := r.k8sClient.LlmsafespacesV1()
	if err != nil {
		return "", fmt.Errorf("get K8s client: %w", err)
	}
	ws, err := v1Client.Workspaces(r.namespace).Get(ctx, workspaceID, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get workspace %s: %w", workspaceID, err)
	}
	if ws.Status.Phase != phaseActive || ws.Status.PodIP == "" {
		return "", nil
	}
	return ws.Status.PodIP, nil
}
