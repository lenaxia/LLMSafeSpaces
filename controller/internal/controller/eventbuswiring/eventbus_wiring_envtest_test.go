//go:build envtest

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package eventbuswiring

// eventbus_wiring_envtest_test.go — the design-0072 ARMED posture pin:
// SetupControllers with a live bus address + password secret ref must
// drive the full production plumbing — flags → reconciler fields →
// buildPod — so the created pod's MAIN container (single-container
// topology, agentdSidecarEnabled=false: the process that runs the
// notifier + bus consumers) carries AGENTD_EVENTBUS_ADDR as the plain
// credentialess value and AGENTD_EVENTBUS_PASSWORD as a secretKeyRef.
// Red on: the signature drift that the envtest tag compiles out of
// plain `go test` (review round 1, finding 1) and any wiring that
// lands the env on a container whose process never reads it (round 1,
// finding 2). The dormant default (no eventbus env anywhere) is pinned
// by the unit suite + the helm render pins + the kind posture gate.
//
// The boilerplate mirrors setup_wiring_envtest_test.go (same envtest
// start, same synthetic delivery pins) — the helpers there are
// _test.go symbols of another package and cannot be imported.
//
// Run: KUBEBUILDER_ASSETS=… go test ./controller/internal/controller/eventbuswiring/ -tags envtest -run TestEnvtestSetupWiring_EventbusArmsPodEnv

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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/lenaxia/llmsafespaces/controller/internal/controller"
	"github.com/lenaxia/llmsafespaces/controller/internal/workspace"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

const (
	wiringAgentdImage  = "ghcr.io/lenaxia/llmsafespaces/agentd@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	wiringAgentdSHAAMD = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	wiringAgentdSHAARM = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	wiringOpencodeImg  = "ghcr.io/lenaxia/llmsafespaces/opencode@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	wiringOpencodeAMD  = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	wiringOpencodeARM  = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	wiringRTEImage     = "registry.example.com/runtimes/python:3.11-wffc"
)

func TestEnvtestSetupWiring_EventbusArmsPodEnv(t *testing.T) {
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "..", "helm", "crds")},
	}
	cfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.Stop() })

	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, storagev1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	require.NoError(t, err)

	const busAddr = "llmsafespaces-valkey.llmsafespaces.svc.cluster.local:6379"
	require.NoError(t, controller.SetupControllers(mgr,
		"", "", "", "", "", "",
		busAddr, "llmsafespaces-credentials", "redis-password",
		workspace.UploadStagingConfig{},
		controller.AgentdDelivery{Image: wiringAgentdImage, BinarySHA256AMD64: wiringAgentdSHAAMD, BinarySHA256ARM64: wiringAgentdSHAARM},
		controller.OpencodeDelivery{Image: wiringOpencodeImg, BinarySHA256AMD64: wiringOpencodeAMD, BinarySHA256ARM64: wiringOpencodeARM},
		false, 1, nil, 0),
		"SetupControllers must carry the armed bus wiring into the Workspace reconciler")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()
	require.True(t, mgr.GetCache().WaitForCacheSync(ctx), "manager cache must sync before seeding")

	dyn, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)

	wffcMode := storagev1.VolumeBindingWaitForFirstConsumer
	require.NoError(t, dyn.Create(ctx, &storagev1.StorageClass{
		ObjectMeta:        metav1.ObjectMeta{Name: "wiring-wffc-evbus"},
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
		ObjectMeta: metav1.ObjectMeta{Name: "ws-wiring-evbus", Namespace: "default"},
		Spec: v1.WorkspaceSpec{
			Owner:   v1.WorkspaceOwner{UserID: "user-wiring"},
			Runtime: "python",
			Storage: v1.WorkspaceStorageConfig{Size: "1Gi", StorageClassName: "wiring-wffc-evbus", AccessMode: "ReadWriteOnce"},
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
		"the pod must appear under the armed controller — its absence means the bus wiring broke the reconcile path itself")

	var addr *corev1.EnvVar
	var password *corev1.EnvVar
	for i := range pod.Spec.Containers[0].Env {
		switch pod.Spec.Containers[0].Env[i].Name {
		case "AGENTD_EVENTBUS_ADDR":
			addr = &pod.Spec.Containers[0].Env[i]
		case "AGENTD_EVENTBUS_PASSWORD":
			password = &pod.Spec.Containers[0].Env[i]
		}
	}
	require.NotNil(t, addr, "the MAIN container (the agentd server process in single-container mode) must carry AGENTD_EVENTBUS_ADDR")
	assert.Equal(t, busAddr, addr.Value,
		"the address is the plain credentialess value — credentials NEVER ride it")
	require.NotNil(t, password, "the MAIN container must carry AGENTD_EVENTBUS_PASSWORD")
	require.NotNil(t, password.ValueFrom, "the password must be a secretKeyRef, never a plain value")
	require.NotNil(t, password.ValueFrom.SecretKeyRef)
	assert.Equal(t, "llmsafespaces-credentials", password.ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "redis-password", password.ValueFrom.SecretKeyRef.Key)
}
