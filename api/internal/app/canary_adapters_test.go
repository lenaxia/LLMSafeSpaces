// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/config"
	"github.com/lenaxia/llmsafespaces/api/internal/services/canary"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// --- picker -----------------------------------------------------------------

type fakeCRDLister struct {
	pages []*v1.WorkspaceList
	err   error
	calls []metav1.ListOptions
}

func (f *fakeCRDLister) List(ctx context.Context, opts metav1.ListOptions) (*v1.WorkspaceList, error) {
	f.calls = append(f.calls, opts)
	if f.err != nil {
		return nil, f.err
	}
	if len(f.pages) == 0 {
		return &v1.WorkspaceList{}, nil
	}
	page := f.pages[0]
	f.pages = f.pages[1:]
	return page, nil
}

func wsItem(name, runtime, phase, podIP string, created time.Time) v1.Workspace {
	ws := v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(created)},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhase(phase)},
	}
	ws.Spec.Runtime = runtime
	ws.Status.PodIP = podIP
	return ws
}

func TestCanaryPicker_SelectsOldestActiveOfClass(t *testing.T) {
	now := time.Now()
	lister := &fakeCRDLister{pages: []*v1.WorkspaceList{{Items: []v1.Workspace{
		wsItem("ws-young", "python:3.11", string(v1.WorkspacePhaseActive), "10.0.0.2", now),
		wsItem("ws-old", "python:3.11", string(v1.WorkspacePhaseActive), "10.0.0.3", now.Add(-time.Hour)),
		wsItem("ws-other-class", "node:22", string(v1.WorkspacePhaseActive), "10.0.0.4", now.Add(-2*time.Hour)),
		wsItem("ws-suspended", "python:3.11", string(v1.WorkspacePhaseSuspended), "10.0.0.5", now.Add(-3*time.Hour)),
	}}}}
	picker := &k8sCanaryTargetPicker{list: lister}

	got, err := picker.Pick(context.Background(), "python:3.11")
	require.NoError(t, err)
	assert.Equal(t, "ws-old", got, "oldest ACTIVE workspace of the class wins — suspended/other-class ignored")
}

func TestCanaryPicker_TieBreaksByName(t *testing.T) {
	ts := time.Now()
	lister := &fakeCRDLister{pages: []*v1.WorkspaceList{{Items: []v1.Workspace{
		wsItem("ws-b", "base", string(v1.WorkspacePhaseActive), "10.0.0.2", ts),
		wsItem("ws-a", "base", string(v1.WorkspacePhaseActive), "10.0.0.3", ts),
	}}}}
	picker := &k8sCanaryTargetPicker{list: lister}

	got, err := picker.Pick(context.Background(), "base")
	require.NoError(t, err)
	assert.Equal(t, "ws-a", got, "equal creation timestamps resolve deterministically by name")
}

func TestCanaryPicker_NoTargetIsNotAnError(t *testing.T) {
	lister := &fakeCRDLister{pages: []*v1.WorkspaceList{{Items: []v1.Workspace{
		wsItem("ws-only", "node:22", string(v1.WorkspacePhaseActive), "10.0.0.2", time.Now()),
	}}}}
	picker := &k8sCanaryTargetPicker{list: lister}

	got, err := picker.Pick(context.Background(), "python:3.11")
	require.NoError(t, err)
	assert.Empty(t, got, "an absent class is a fleet shape (canary no_target), never an error")
}

func TestCanaryPicker_ListErrorPropagates(t *testing.T) {
	picker := &k8sCanaryTargetPicker{list: &fakeCRDLister{err: errors.New("apiserver down")}}
	_, err := picker.Pick(context.Background(), "base")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "apiserver down")
}

func TestCanaryPicker_PaginatesUntilExhausted(t *testing.T) {
	now := time.Now()
	lister := &fakeCRDLister{pages: []*v1.WorkspaceList{
		{ListMeta: metav1.ListMeta{Continue: "tok-2"}, Items: []v1.Workspace{
			wsItem("ws-page1", "base", string(v1.WorkspacePhaseActive), "10.0.0.2", now),
		}},
		{Items: []v1.Workspace{
			wsItem("ws-page2-older", "base", string(v1.WorkspacePhaseActive), "10.0.0.3", now.Add(-time.Minute)),
		}},
	}}
	picker := &k8sCanaryTargetPicker{list: lister}

	got, err := picker.Pick(context.Background(), "base")
	require.NoError(t, err)
	assert.Equal(t, "ws-page2-older", got, "the winner may live on any page — pagination must walk the fleet")
	require.Len(t, lister.calls, 2)
	assert.Equal(t, "tok-2", lister.calls[1].Continue)
}

// --- resolver -----------------------------------------------------------------

type fakeWorkspaceGetter struct {
	ws  *v1.Workspace
	err error
}

func (f *fakeWorkspaceGetter) GetWorkspace(ctx context.Context, id string) (*v1.Workspace, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.ws, nil
}

type fakeSecretGetter struct {
	secret *corev1.Secret
	err    error
}

func (f *fakeSecretGetter) GetSecret(ctx context.Context, name string) (*corev1.Secret, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.secret, nil
}

func TestCanaryResolver_HappyPath(t *testing.T) {
	ws := wsItem("ws-1", "base", string(v1.WorkspacePhaseActive), "10.1.2.3", time.Now())
	secret := &corev1.Secret{Data: map[string][]byte{"password": []byte("pw-1")}}
	resolver := &k8sCanaryPodResolver{
		ws:      &fakeWorkspaceGetter{ws: &ws},
		secrets: &fakeSecretGetter{secret: secret},
	}

	baseURL, password, err := resolver.Resolve(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.Equal(t, "http://10.1.2.3:4097", baseURL, "the ABI surface — same endpoint discipline as agentdEndpoint")
	assert.Equal(t, "pw-1", password)
}

func TestCanaryResolver_MissingPodIPFailsLoud(t *testing.T) {
	ws := wsItem("ws-1", "base", string(v1.WorkspacePhaseCreating), "", time.Now())
	resolver := &k8sCanaryPodResolver{
		ws:      &fakeWorkspaceGetter{ws: &ws},
		secrets: &fakeSecretGetter{secret: &corev1.Secret{Data: map[string][]byte{"password": []byte("pw")}}},
	}
	_, _, err := resolver.Resolve(context.Background(), "ws-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no pod IP")
}

func TestCanaryResolver_SecretFailuresSurface(t *testing.T) {
	ws := wsItem("ws-1", "base", string(v1.WorkspacePhaseActive), "10.1.2.3", time.Now())

	t.Run("secret read error", func(t *testing.T) {
		resolver := &k8sCanaryPodResolver{
			ws:      &fakeWorkspaceGetter{ws: &ws},
			secrets: &fakeSecretGetter{err: errors.New("rbac denied")},
		}
		_, _, err := resolver.Resolve(context.Background(), "ws-1")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "rbac denied")
	})

	t.Run("empty password", func(t *testing.T) {
		resolver := &k8sCanaryPodResolver{
			ws:      &fakeWorkspaceGetter{ws: &ws},
			secrets: &fakeSecretGetter{secret: &corev1.Secret{Data: map[string][]byte{}}},
		}
		_, _, err := resolver.Resolve(context.Background(), "ws-1")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty password")
	})
}

// --- the enable-knob wiring (review r1: the app-layer contract) ---------------

func okPick(ctx context.Context, class string) (string, error)         { return "ws", nil }
func okResolve(ctx context.Context, ws string) (string, string, error) { return "http://x", "pw", nil }
func okClient(baseURL, password string) canary.Client                  { return nil }

// TestNewCanaryService_DisabledIsNil: the knob off (or nil config) →
// nil service, no error — Run() starts nothing.
func TestNewCanaryService_DisabledIsNil(t *testing.T) {
	empty := &config.Config{}
	empty.Canary.Classes = []string{"base"} // classes without the knob still means OFF
	for _, cfg := range []*config.Config{nil, empty} {
		svc, err := newCanaryServiceWith(cfg, okPick, okResolve, okClient, nil)
		require.NoError(t, err)
		assert.Nil(t, svc)
	}
}

// TestNewCanaryService_ProductionWiring: enabled config → non-nil
// service, nil error, through the REAL production seam set (nil k8s
// client is fine — the adapters hold it unkeferenced until probe time).
func TestNewCanaryService_ProductionWiring(t *testing.T) {
	cfg := &config.Config{}
	cfg.Canary.Enabled = true
	cfg.Canary.Classes = []string{"python:3.11"}
	cfg.Kubernetes.Namespace = "llmsafespaces"

	svc, err := newCanaryService(cfg, nil, nil)
	require.NoError(t, err, "enabled + validated classes + statically-wired seams cannot fail construction")
	require.NotNil(t, svc, "an explicitly-enabled canary must start — silent non-start is false coverage")
}

// TestNewCanaryService_DefectiveSeamsFailBoot: the fail-closed contract.
// Production wiring cannot produce a defective seam set (pinned above),
// but if one ever lands, boot must fail rather than degrade to
// no-canary — validateCanary's discipline, extended to wiring defects.
func TestNewCanaryService_DefectiveSeamsFailBoot(t *testing.T) {
	cfg := &config.Config{}
	cfg.Canary.Enabled = true
	cfg.Canary.Classes = []string{"base"}

	_, err := newCanaryServiceWith(cfg, nil, okResolve, okClient, nil)
	require.Error(t, err, "a nil Pick seam must fail construction, not silently disable the canary")
	_, err = newCanaryServiceWith(cfg, okPick, nil, okClient, nil)
	require.Error(t, err)
	_, err = newCanaryServiceWith(cfg, okPick, okResolve, nil, nil)
	require.Error(t, err)
}
