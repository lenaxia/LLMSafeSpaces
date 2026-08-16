// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// Design 0050 D7 (#892): cap build-tool parallelism to the workspace's
// effective CPU limit — the incident's starvation was oversubscription
// (machine-sized pools inside a 2-CPU quota). GOMAXPROCS is the single
// lever: it caps the go toolchain directly and the esbuild CLI
// transitively (a Go binary). ESBUILD_WORKER_THREADS is deliberately
// absent — review round 1 on #897 verified against esbuild's shipped
// source that it has no numeric semantics (a "0"-disable flag for one
// sync-API worker thread) and would be a placebo.

func envValue(envs []corev1.EnvVar, name string) (string, bool) {
	for _, e := range envs {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

func TestPodBuilder_ToolParallelismCappedToCPULimit(t *testing.T) {
	ws := newWorkspaceForPodBuilder(t)
	ws.Spec.Resources = &v1.ResourceRequirements{CPU: "2"}
	r := reconcilerFor(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	env := pod.Spec.Containers[0].Env
	gomax, ok := envValue(env, "GOMAXPROCS")
	require.True(t, ok, "GOMAXPROCS must be present on the workspace container")
	assert.Equal(t, "8", gomax, "2 CPU request → 4× burst limit → 8-core cap")

	if v, present := envValue(env, "ESBUILD_WORKER_THREADS"); present {
		t.Fatalf("ESBUILD_WORKER_THREADS must not be set (placebo, no numeric semantics); got %q", v)
	}
}

// TestPodBuilder_ToolParallelism_Default: the default shape (no
// Resources → 500m request → 2-core burst limit) caps at 2. Lookup is
// by name with presence assertions — a loop that happens to find
// nothing must fail, not pass vacuously (review round 1: the old
// default-path test could not fail).
func TestPodBuilder_ToolParallelism_Default(t *testing.T) {
	ws := newWorkspaceForPodBuilder(t)
	r := reconcilerFor(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	gomax, ok := envValue(pod.Spec.Containers[0].Env, "GOMAXPROCS")
	require.True(t, ok, "GOMAXPROCS must be present even on default-shaped workspaces")
	assert.Equal(t, "2", gomax, "500m request bursts to a 2-core limit")
}

// TestToolParallelismEnv_GuardPaths: ceil-conversion boundaries and the
// missing/zero-limit fallback.
func TestToolParallelismEnv_GuardPaths(t *testing.T) {
	cases := []struct {
		name     string
		cpuLimit string
		want     string
	}{
		{"500m ceilings to 1", "500m", "1"},
		{"1000m is exactly 1", "1000m", "1"},
		{"1500m ceilings to 2", "1500m", "2"},
		{"2000m is exactly 2", "2000m", "2"},
		{"2500m ceilings to 3", "2500m", "3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := toolParallelismEnv(corev1.ResourceRequirements{Limits: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse(tc.cpuLimit),
			}})
			require.Len(t, env, 1, "GOMAXPROCS is the single lever")
			assert.Equal(t, "GOMAXPROCS", env[0].Name)
			assert.Equal(t, tc.want, env[0].Value)
		})
	}

	t.Run("missing CPU limit falls back to 1", func(t *testing.T) {
		env := toolParallelismEnv(corev1.ResourceRequirements{})
		require.Len(t, env, 1)
		assert.Equal(t, "1", env[0].Value)
	})
	t.Run("zero CPU limit falls back to 1", func(t *testing.T) {
		env := toolParallelismEnv(corev1.ResourceRequirements{Limits: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("0"),
		}})
		require.Len(t, env, 1)
		assert.Equal(t, "1", env[0].Value)
	})
}

// TestE2E_Reconcile_PodSpec_ToolParallelismCapped: the caps must survive
// a real reconcile into the persisted Pod (repo e2e convention —
// buildPod-level tests alone would not catch a wiring drop between
// building and persisting). Default shape and custom shape.
func TestE2E_Reconcile_PodSpec_ToolParallelismCapped(t *testing.T) {
	ws := makeWorkspace("ws-d7-caps", "default", v1.WorkspacePhasePending)
	ws.Spec.Resources = &v1.ResourceRequirements{CPU: "2"}
	const apiURL = "http://test-api.e2e:8080"
	_, pod := reconcileToCreatingPod(t, ws, apiURL)

	gomax, ok := envValue(pod.Spec.Containers[0].Env, "GOMAXPROCS")
	require.True(t, ok, "persisted pod must carry GOMAXPROCS (2-CPU workspace)")
	assert.Equal(t, "8", gomax)
	_, present := envValue(pod.Spec.Containers[0].Env, "ESBUILD_WORKER_THREADS")
	assert.False(t, present, "persisted pod must not carry the placebo esbuild var")
}

func TestE2E_Reconcile_PodSpec_ToolParallelismDefaultShape(t *testing.T) {
	ws := makeWorkspace("ws-d7-default", "default", v1.WorkspacePhasePending)
	const apiURL = "http://test-api.e2e:8080"
	_, pod := reconcileToCreatingPod(t, ws, apiURL)

	gomax, ok := envValue(pod.Spec.Containers[0].Env, "GOMAXPROCS")
	require.True(t, ok, "persisted default-shape pod must carry GOMAXPROCS")
	assert.Equal(t, "2", gomax, "500m default request bursts to a 2-core limit")
}
