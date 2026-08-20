//go:build envtest

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// Envtest integration for design 0051 US-2 (integration level L2, see
// docs/testing/0051-us2-integration-test-plan.md): the sidecar-mode POD
// SPEC against a real Kubernetes API server.
//
// What the API server adds over the unit suite's structural asserts:
// real validation of native-sidecar semantics (init-container
// restartPolicy admission, KEP-753), real SecurityContext validation
// (RunAsNonRoot vs uid 2000), and real EnvFrom/SecretKeyRef shape
// checking. A spec the API server rejects is a pod that CrashLoops or
// fails admission at deploy time — exactly the class of bug the unit
// suite's pure struct asserts cannot see.
//
// Run: go test ./controller/internal/workspace/ -tags envtest -run TestEnvtestAgentdSidecar
// Requires KUBEBUILDER_ASSETS (see .github/workflows/envtest.yml).

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// TestEnvtestAgentdSidecar_PodSpecAdmitted: buildPod's sidecar-mode
// output must be admitted by a real API server. This pins the whole
// container spec — any validation error (bad probe port, malformed
// securityContext, unsupported restartPolicy placement) fails here,
// not at deploy time.
func TestEnvtestAgentdSidecar_PodSpecAdmitted(t *testing.T) {
	cfg := startEnvtest(t)
	ws := newWorkspaceForSecurity(t)
	ws.UID = types.UID("us2-integration-uid")
	r := reconcilerWithAgentdSidecar(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	// buildPod targets the workspace's namespace; envtest only has
	// default. Retarget before create.
	pod.Namespace = "default"

	dyn, err := client.New(cfg, client.Options{})
	require.NoError(t, err)
	require.NoError(t, dyn.Create(context.Background(), pod))

	// Re-read the ADMIRED spec back from the server (defaulting may
	// fill fields; the contract is that OUR fields survived).
	got := &corev1.Pod{}
	require.NoError(t, dyn.Get(context.Background(), client.ObjectKeyFromObject(pod), got))

	var sc *corev1.Container
	for i := range got.Spec.InitContainers {
		if got.Spec.InitContainers[i].Name == "agentd" {
			sc = &got.Spec.InitContainers[i]
		}
	}
	require.NotNil(t, sc, "agentd native sidecar must survive admission")

	// KEP-753: the restartPolicy must survive on the INIT container.
	require.NotNil(t, sc.RestartPolicy)
	require.Equal(t, corev1.ContainerRestartPolicyAlways, *sc.RestartPolicy)

	// The uid split survives: 2000/1000 non-root.
	sec := sc.SecurityContext
	require.NotNil(t, sec)
	require.NotNil(t, sec.RunAsUser)
	require.Equal(t, int64(2000), *sec.RunAsUser)
	require.NotNil(t, sec.RunAsGroup)
	require.Equal(t, int64(1000), *sec.RunAsGroup)

	// The startup probe still targets the sidecar's admin mux — the
	// #857 gate must remain an HTTP probe on 4098.
	require.NotNil(t, sc.StartupProbe)
	require.NotNil(t, sc.StartupProbe.HTTPGet)
	require.Equal(t, "/v1/healthz", sc.StartupProbe.HTTPGet.Path)
	require.Equal(t, agentd.AgentdAdminPort, sc.StartupProbe.HTTPGet.Port.IntValue())

	// Ordering: credential-setup precedes agentd, agentd is last.
	credIdx, scIdx := -1, -1
	for i, c := range got.Spec.InitContainers {
		switch c.Name {
		case "credential-setup":
			credIdx = i
		case "agentd":
			scIdx = i
		}
	}
	require.Greater(t, scIdx, credIdx, "materialize must land before the sidecar stamps")
	require.Equal(t, len(got.Spec.InitContainers)-1, scIdx)
}

// TestEnvtestAgentdSidecar_DisabledPodUnchanged: the chart gate OFF
// must produce a pod the API server admits with NO sidecar — the
// single-container regression pin at the admission level.
func TestEnvtestAgentdSidecar_DisabledPodUnchanged(t *testing.T) {
	cfg := startEnvtest(t)
	ws := newWorkspaceForSecurity(t)
	ws.UID = types.UID("us2-integration-uid")
	r := reconcilerWithAgentd(t) // overlay delivery on, sidecar OFF

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)
	pod.Namespace = "default"

	dyn, err := client.New(cfg, client.Options{})
	require.NoError(t, err)
	require.NoError(t, dyn.Create(context.Background(), pod))

	got := &corev1.Pod{}
	require.NoError(t, dyn.Get(context.Background(), client.ObjectKeyFromObject(pod), got))
	for _, c := range got.Spec.InitContainers {
		require.NotEqual(t, "agentd", c.Name, "no sidecar when the gate is off")
	}
	require.Len(t, got.Spec.Containers, 1)
}

// TestEnvtestAgentdSidecar_SecretBackedEnv: the sidecar's credential
// env must reference a REAL creatable Secret shape — create the
// password Secret the way handlePending does and verify the pod's
// envFrom/secretKeyRef resolves against it (missing-key admission is
// delayed to kubelet, but key-reference SHAPE errors fail here).
func TestEnvtestAgentdSidecar_SecretBackedEnv(t *testing.T) {
	cfg := startEnvtest(t)
	dyn, err := client.New(cfg, client.Options{})
	require.NoError(t, err)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	_ = dyn.Get(ctx, client.ObjectKeyFromObject(ns), ns)

	ws := newWorkspaceForSecurity(t)
	ws.UID = types.UID("us2-integration-uid")
	r := reconcilerWithAgentdSidecar(t)

	// The Secret buildPod reads (password + admin-token keys).
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      passwordSecretName(ws.Name),
			Namespace: "default",
		},
		Data: map[string][]byte{
			"password":    []byte("integration-pw"),
			"admin-token": []byte("integration-tok"),
		},
	}
	require.NoError(t, dyn.Create(ctx, sec))

	pod, err := r.buildPod(ctx, ws)
	require.NoError(t, err)
	pod.Namespace = "default"
	require.NoError(t, dyn.Create(ctx, pod))

	got := &corev1.Pod{}
	require.NoError(t, dyn.Get(ctx, client.ObjectKeyFromObject(pod), got))
	var sc *corev1.Container
	for i := range got.Spec.InitContainers {
		if got.Spec.InitContainers[i].Name == "agentd" {
			sc = &got.Spec.InitContainers[i]
		}
	}
	require.NotNil(t, sc)

	keys := map[string]string{}
	for _, e := range sc.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			keys[e.Name] = e.ValueFrom.SecretKeyRef.Key
		}
	}
	require.Equal(t, "password", keys["AGENTD_SIDECAR_PASSWORD"])
	require.Equal(t, "admin-token", keys["AGENTD_ADMIN_TOKEN"])

	// Every referenced Secret key must exist in the created Secret —
	// the exact failure kubelet would report at container start.
	for _, e := range sc.Env {
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			continue
		}
		ref := e.ValueFrom.SecretKeyRef
		fetched := &corev1.Secret{}
		require.NoError(t, dyn.Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: "default"}, fetched),
			"sidecar env references Secret %s which must exist", ref.Name)
		require.Contains(t, fetched.Data, ref.Key,
			"sidecar env %s references key %q missing from Secret %s", e.Name, ref.Key, ref.Name)
	}
}
