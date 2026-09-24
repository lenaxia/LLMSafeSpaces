// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"github.com/prometheus/client_golang/prometheus"

	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// fakeRelayWorkspaceGetter resolves a fixed Workspace (the
// k8sWorkspaceGetterAdapter shape without a live cluster).
type fakeRelayWorkspaceGetter struct{}

func (fakeRelayWorkspaceGetter) GetWorkspace(_ context.Context, id string) (*v1.Workspace, error) {
	return &v1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: "plat-ns"}}, nil
}

func handoffSecretData(t *testing.T, h *secrets.RelayHandoff) map[string][]byte {
	t.Helper()
	raw, err := json.Marshal(h)
	require.NoError(t, err)
	return map[string][]byte{secrets.RelayHandoffDataKey: raw}
}

// TestK8sRelayTokenSource_ReadsHandoffSecret: the controller-written
// Secret (workspace-relay-<wsName>, data key `handoff`, in the
// workspace's namespace) parses into the builder's handoff shape.
func TestK8sRelayTokenSource_ReadsHandoffSecret(t *testing.T) {
	const fakeToken = "lrt_fakeAAAAadapter"
	cs := k8sfake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-relay-ws-77", Namespace: "plat-ns"},
		Data:       handoffSecretData(t, &secrets.RelayHandoff{Revision: "rADAPT01", RouterURL: "http://router.example", Providers: []secrets.RelayHandoffProvider{{ProviderSlug: "openai", Token: fakeToken, RouterPath: "/w/ws-77/openai/v1"}}}),
	})
	src := newK8sRelayTokenSource(fakeRelayWorkspaceGetter{}, cs)

	h, err := src.RelayHandoff(context.Background(), "ws-77")
	require.NoError(t, err)
	require.NotNil(t, h)
	assert.Equal(t, "rADAPT01", h.Revision)
	assert.Equal(t, fakeToken, h.Providers[0].Token)
	assert.Equal(t, "http://router.example/w/ws-77/openai/v1", h.RouterBase()+h.Providers[0].RouterPath)
}

// TestK8sRelayTokenSource_AbsentIsNotReady: a missing Secret returns
// (nil, nil) — the builder's staging-not-ready signal, never an error
// and never a raw fallback.
func TestK8sRelayTokenSource_AbsentIsNotReady(t *testing.T) {
	src := newK8sRelayTokenSource(fakeRelayWorkspaceGetter{}, k8sfake.NewSimpleClientset())
	h, err := src.RelayHandoff(context.Background(), "ws-77")
	require.NoError(t, err)
	assert.Nil(t, h)
}

// TestK8sRelayTokenSource_EmptyDataIsNotReady: a Secret with an empty
// handoff key is the same not-ready class (an unusable handoff must not
// half-deliver).
func TestK8sRelayTokenSource_EmptyDataIsNotReady(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-relay-ws-77", Namespace: "plat-ns"},
		Data:       map[string][]byte{},
	})
	src := newK8sRelayTokenSource(fakeRelayWorkspaceGetter{}, cs)
	h, err := src.RelayHandoff(context.Background(), "ws-77")
	require.NoError(t, err)
	assert.Nil(t, h)
}

// TestK8sRelayTokenSource_GarbageDataErrors: an unparseable handoff is
// a loud error (staging wrote something the builder cannot trust).
func TestK8sRelayTokenSource_GarbageDataErrors(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-relay-ws-77", Namespace: "plat-ns"},
		Data:       map[string][]byte{secrets.RelayHandoffDataKey: []byte("{not-json")},
	})
	src := newK8sRelayTokenSource(fakeRelayWorkspaceGetter{}, cs)
	h, err := src.RelayHandoff(context.Background(), "ws-77")
	require.Error(t, err)
	assert.Nil(t, h)
}

// TestInstallRelayTokenSource_FlagGate (the app.New seam, #1529 review
// item 4): flag ON installs the k8s source on the one builder — and the
// INSTALLED source actually resolves a staged handoff; flag OFF
// installs nothing (nil source = byte-identical legacy batches).
func TestInstallRelayTokenSource_FlagGate(t *testing.T) {
	newSvc := func() *secrets.SecretService { return secrets.NewSecretService(nil, nil) }

	// Flag off: dependencies provided, flag false — nothing installed.
	off := newSvc()
	installRelayTokenSource(off, false, "", fakeRelayWorkspaceGetter{}, k8sfake.NewSimpleClientset())
	assert.Nil(t, off.RelayTokensForTest(), "flag off must not install a source")

	// Flag on: the k8s source is installed and WORKS through the seam.
	const fakeToken = "lrt_fakeInstall0123456789"
	cs := k8sfake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-relay-ws-99", Namespace: "plat-ns"},
		Data: handoffSecretData(t, &secrets.RelayHandoff{
			Revision: "rINSTALL1", RouterURL: "http://router.example",
			Providers: []secrets.RelayHandoffProvider{{ProviderSlug: "openai", Token: fakeToken}},
		}),
	})
	on := newSvc()
	installRelayTokenSource(on, true, "", fakeRelayWorkspaceGetter{}, cs)
	require.NotNil(t, on.RelayTokensForTest(), "flag on must install the relay source on SecretService")

	h, err := on.RelayTokensForTest().RelayHandoff(context.Background(), "ws-99")
	require.NoError(t, err)
	require.NotNil(t, h)
	assert.Equal(t, "rINSTALL1", h.Revision)
	assert.Equal(t, fakeToken, h.Providers[0].Token)

	// Nil service must not panic (defensive seam contract).
	installRelayTokenSource(nil, true, "", fakeRelayWorkspaceGetter{}, cs)
}

// M2 (design 0061 §4): the fallback mode rides the relay install —
// migration unless explicitly strict (the builder's zero value is
// STRICT, so this wiring is what arms migration in deployment; the
// behavioral table lives in pkg/secrets/relay_fallback_test.go).
func TestInstallRelay_FallbackModeThreads(t *testing.T) {
	cs := k8sfake.NewSimpleClientset()

	for _, tc := range []struct {
		mode     string
		expected bool
	}{
		{"", true},          // unset → migration (the default)
		{"migration", true}, // explicit migration
		{"strict", false},   // explicit strict
	} {
		svc := secrets.NewSecretService(nil, nil)
		installRelayTokenSource(svc, true, tc.mode, fakeRelayWorkspaceGetter{}, cs)
		if svc.RelayFallbackForTest() != tc.expected {
			t.Errorf("mode %q: fallback = %v, want %v", tc.mode, svc.RelayFallbackForTest(), tc.expected)
		}
	}

	// Flag off: no wiring at all (the source AND the mode stay unset).
	off := secrets.NewSecretService(nil, nil)
	installRelayTokenSource(off, false, "migration", fakeRelayWorkspaceGetter{}, cs)
	if off.RelayTokensForTest() != nil || off.RelayFallbackForTest() {
		t.Error("flag off must not install the source or arm the fallback")
	}
}

// Counter-registration observability (r1 finding 2): deleting the
// registerRelayMetricsOnce block must fail a test — the alerts can only
// fire if the collectors are GATHERABLE. The pkg/secrets tests read the
// unexported collectors directly; THIS asserts the default registry
// exposure after the install seam.
func TestInstallRelay_CountersRegistered(t *testing.T) {
	installRelayTokenSource(secrets.NewSecretService(nil, nil), true, "migration", fakeRelayWorkspaceGetter{}, k8sfake.NewSimpleClientset())
	// A CounterVec emits NO family until a child exists — probe both so
	// gatherability is observable (the production increments are the
	// children; the probe uses throwaway label values).
	fb, deg := secrets.RelayFallbackMetrics()
	fb.WithLabelValues("probe", "probe").Inc()
	deg.WithLabelValues("probe", "probe").Inc()
	mfs, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	saw := map[string]bool{}
	for _, mf := range mfs {
		name := mf.GetName()
		if name == "relay_fallback_deliveries_total" || name == "relay_degraded_batches_total" {
			saw[name] = true
		}
	}
	assert.True(t, saw["relay_fallback_deliveries_total"], "the fallback counter must be gatherable after relay install (else the alert is permanently dark on a green tree)")
	assert.True(t, saw["relay_degraded_batches_total"], "the degraded counter must be gatherable after relay install")
}
