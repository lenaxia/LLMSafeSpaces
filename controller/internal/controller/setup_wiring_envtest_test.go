//go:build envtest

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

// setup_wiring_envtest_test.go — #1587 wiring pin: the ONLY production
// construction path (SetupControllers → WorkspaceReconciler →
// SetupWithManager → buildPod) must serve RuntimeEnvironment resolution
// through the manager's DIRECT reader. The unit pins prove buildPod uses
// r.directReader(); this test proves the reader is actually WIRED — a
// forgotten `APIReader: mgr.GetAPIReader()` line makes SetupWithManager
// refuse (boot-loud), so this test goes red on exactly that wiring bug.
//
// The WFFC StorageClass makes Pending→Creating independent of PVC
// binding (envtest has no provisioner; the WFFC leg itself is served by
// the direct reader post-#1587) — so the pod's EXISTENCE proves BOTH
// cluster-scoped reads succeeded through the wired direct reader.
//
// Run: KUBEBUILDER_ASSETS=… go test ./controller/internal/controller/ -tags envtest -run TestEnvtestSetupWiring
// Requires KUBEBUILDER_ASSETS (see .github/workflows/envtest.yml).

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/lenaxia/llmsafespaces/controller/internal/workspace"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

const (
	wiringAgentdImage   = "ghcr.io/lenaxia/llmsafespaces/agentd@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	wiringAgentdSHAAMD  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	wiringAgentdSHAARM  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	wiringOpencodeImage = "ghcr.io/lenaxia/llmsafespaces/opencode@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	wiringOpencodeAMD   = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	wiringOpencodeARM   = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	wiringRTEImage      = "registry.example.com/runtimes/python:3.11-wffc"
)

func startControllerEnvtest(t *testing.T) *rest.Config {
	t.Helper()
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "helm", "crds")},
	}
	cfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.Stop() })
	return cfg
}

// TestEnvtestSetupWiring_DirectReaderServesRuntimeResolution drives the
// full production wiring against a real apiserver: SetupControllers
// registers the Workspace controller on a REAL manager, a Workspace with
// a NAMED runtime (python) resolves through the wired direct reader, and
// the pod appears with the RuntimeEnvironment's image. Red on: missing
// APIReader wiring (SetupWithManager refuses), a cached-client revert in
// buildPod (this suite's admin cache would serve it — that mutation is
// pinned red by the unit never-syncing test, not here), and any WFFC
// regression on the Pending→Creating transition.
func TestEnvtestSetupWiring_DirectReaderServesRuntimeResolution(t *testing.T) {
	cfg := startControllerEnvtest(t)

	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, storagev1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	require.NoError(t, err)

	require.NoError(t, SetupControllers(mgr,
		"", "", "", "", "", "",
		workspace.UploadStagingConfig{},
		AgentdDelivery{Image: wiringAgentdImage, BinarySHA256AMD64: wiringAgentdSHAAMD, BinarySHA256ARM64: wiringAgentdSHAARM},
		OpencodeDelivery{Image: wiringOpencodeImage, BinarySHA256AMD64: wiringOpencodeAMD, BinarySHA256ARM64: wiringOpencodeARM},
		false, 1, nil, 0),
		"SetupControllers must register the Workspace controller with a wired direct reader (#1587)")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()
	require.True(t, mgr.GetCache().WaitForCacheSync(ctx), "manager cache must sync before seeding")

	dyn, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)

	wffcMode := storagev1.VolumeBindingWaitForFirstConsumer
	require.NoError(t, dyn.Create(ctx, &storagev1.StorageClass{
		ObjectMeta:        metav1.ObjectMeta{Name: "wiring-wffc"},
		Provisioner:       "kubernetes.io/no-provisioner",
		VolumeBindingMode: &wffcMode,
	}))
	require.NoError(t, dyn.Create(ctx, &v1.RuntimeEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "python"},
		Spec: v1.RuntimeEnvironmentSpec{
			Image:    wiringRTEImage,
			Language: "python",
			Version:  "3.11",
		},
	}))

	ws := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-wiring", Namespace: "default"},
		Spec: v1.WorkspaceSpec{
			Owner:   v1.WorkspaceOwner{UserID: "user-wiring"},
			Runtime: "python",
			Storage: v1.WorkspaceStorageConfig{Size: "1Gi", StorageClassName: "wiring-wffc", AccessMode: "ReadWriteOnce"},
		},
	}
	v1.SetDefaults_Workspace(ws)
	require.NoError(t, dyn.Create(ctx, ws))

	var pod *corev1.Pod
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		list := &corev1.PodList{}
		require.NoError(t, dyn.List(ctx, list, client.InNamespace("default"),
			client.MatchingLabels{workspace.LabelWorkspace: ws.Name}))
		if len(list.Items) == 1 {
			pod = &list.Items[0]
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.NotNil(t, pod,
		"the pod must appear: creation is downstream of runtime resolution — its absence means the RTE read (direct reader) or the WFFC leg (direct reader) never completed (#1587 wiring)")
	require.Len(t, pod.Spec.Containers, 1)
	assert.Equal(t, wiringRTEImage, pod.Spec.Containers[0].Image,
		"the pod image must come from the RuntimeEnvironment served via the wired direct reader")
	assert.Equal(t, "python", pod.Annotations["llmsafespaces.dev/runtime-env"])
}
