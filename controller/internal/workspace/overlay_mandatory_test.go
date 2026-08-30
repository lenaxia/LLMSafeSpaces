// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// Design 0053 S3: mandatory overlay pins, no baked fallback.
//
// Post-S3 the runtime base carries no platform artifacts: the main
// container execs the digest-pinned agentd binary directly, the platform
// init containers are the only boot path, the legacy bash init
// containers are gone, and the base image's ENV block (mise homes, PATH
// composition, git-credential env layer) is injected by the controller.
// These tests lock the mandatory shape: a reconciler without pins fails
// loud, and a pinned reconciler produces the overlay-only pod.

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// reconcilerPinned returns a reconciler with BOTH overlay pins set —
// the only supported configuration post-S3.
func reconcilerPinned(t *testing.T) *WorkspaceReconciler {
	t.Helper()
	r := reconcilerWithAgentd(t)
	r.OpencodeImage = testOpencodeImage
	r.OpencodeBinarySHA256AMD64 = testOpencodeSHAAMD64
	r.OpencodeBinarySHA256ARM64 = testOpencodeSHAARM64
	return r
}

func buildPodMandatory(t *testing.T, r *WorkspaceReconciler) *corev1.Pod {
	t.Helper()
	ws := newWorkspaceForSecurity(t)
	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)
	return pod
}

func mainContainerOf(t *testing.T, pod *corev1.Pod) *corev1.Container {
	t.Helper()
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "workspace" {
			return &pod.Spec.Containers[i]
		}
	}
	t.Fatal("main container not found")
	return nil
}

func initNameSet(pod *corev1.Pod) map[string]bool {
	names := map[string]bool{}
	for _, c := range pod.Spec.InitContainers {
		names[c.Name] = true
	}
	return names
}

// --- mandatory pins: fail loud ---------------------------------------------

func TestS3_MissingAgentdImage_FailsLoud(t *testing.T) {
	r := reconcilerPinned(t)
	r.AgentdImage = ""
	ws := newWorkspaceForSecurity(t)

	_, err := r.buildPod(context.Background(), ws)
	require.Error(t, err, "post-S3 a missing agentd pin must fail the build, not fall back to a baked binary")
	assert.Contains(t, err.Error(), "agentdDelivery")
}

func TestS3_MissingOpencodeImage_FailsLoud(t *testing.T) {
	r := reconcilerPinned(t)
	r.OpencodeImage = ""
	ws := newWorkspaceForSecurity(t)

	_, err := r.buildPod(context.Background(), ws)
	require.Error(t, err, "post-S3 a missing opencode pin must fail the build, not fall back to a baked binary")
	assert.Contains(t, err.Error(), "opencodeDelivery")
}

// --- main container: direct overlay exec -----------------------------------

func TestS3_MainContainerRunsOverlayAgentdDirectly(t *testing.T) {
	pod := buildPodMandatory(t, reconcilerPinned(t))
	main := mainContainerOf(t, pod)

	assert.Equal(t, []string{agentdMountPath + agentdBinaryRelPath}, main.Command,
		"the entrypoint is deleted; the pod execs the pinned agentd binary")
	assert.Equal(t, []string{"--supervise"}, main.Args,
		"single-container mode runs the supervisor")

	for _, blob := range []string{"entrypoint"} {
		for _, c := range pod.Spec.InitContainers {
			assert.NotContains(t, strings.Join(c.Command, " "), blob, "no init container may reference deleted entrypoints")
		}
	}
}

func TestS3_SidecarMode_OverridesToSuperviseOpencode(t *testing.T) {
	r := reconcilerPinned(t)
	r.AgentdSidecarEnabled = true
	ws := newWorkspaceForSecurity(t)
	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	main := mainContainerOf(t, pod)
	assert.Equal(t, []string{agentdMountPath + agentdBinaryRelPath}, main.Command)
	assert.Equal(t, []string{"supervise-opencode"}, main.Args)
}

// --- init containers: platform path unconditional ---------------------------

func TestS3_PlatformInitContainersUnconditional(t *testing.T) {
	pod := buildPodMandatory(t, reconcilerPinned(t))
	inits := initNameSet(pod)

	assert.True(t, inits["platform-init"], "platform-init (init-fs) is the only PVC-prep path post-S3")
	assert.True(t, inits["platform-bootstrap"], "single-container mode boots via the platform bootstrap init")
	assert.True(t, inits["platform-materialize"], "single-container mode materializes via the platform init")
	assert.False(t, inits["workspace-dirs"], "the bash workspace-dirs init is deleted with the entrypoints")
	assert.False(t, inits["credential-setup"], "the bash credential-setup heredoc is deleted with the entrypoints")

	for _, c := range pod.Spec.InitContainers {
		assert.Equal(t, agentdMountPath+agentdBinaryRelPath, c.Command[0],
			"platform inits exec the pinned agentd binary, not a PATH lookup (%s)", c.Name)
	}
}

// --- base ENV block: controller-injected ------------------------------------

func TestS3_BaseEnvInjectedOnMainContainer(t *testing.T) {
	pod := buildPodMandatory(t, reconcilerPinned(t))
	main := mainContainerOf(t, pod)

	want := map[string]string{
		"MISE_DATA_DIR":      "/workspace/.local/share/mise",
		"CARGO_HOME":         "/workspace/.local/share/cargo",
		"GEM_HOME":           "/workspace/.local/share/gem",
		"GOPATH":             "/workspace/.local/share/go",
		"NPM_CONFIG_PREFIX":  "/workspace/.local",
		"PYTHONUSERBASE":     "/workspace/.local",
		"GIT_CONFIG_COUNT":   "1",
		"GIT_CONFIG_KEY_0":   "credential.helper",
		"GIT_CONFIG_VALUE_0": "store --file=/home/sandbox/.git-credentials",
	}
	for name, value := range want {
		v, ok := envVar(main, name)
		require.True(t, ok, "ENV %s must move from the base image to pod env (design 0053 §4.3)", name)
		assert.Equal(t, value, v.Value, name)
	}

	v, ok := envVar(main, "PATH")
	require.True(t, ok, "PATH composition is platform contract post-S3")
	for _, dir := range []string{
		"/workspace/.local/bin",
		"/workspace/.local/share/mise/shims",
		"/workspace/.local/share/go/bin",
		"/workspace/.local/share/cargo/bin",
		"/usr/local/share/mise/shims",
	} {
		assert.Contains(t, v.Value, dir, "PATH must keep the shims/homes composition: %s", dir)
	}
}

func TestS3_OpencodeEnvStaysBehindAgentSeam(t *testing.T) {
	// Containment (#942, design 0051 D1): opencode env-var names are
	// runtime knowledge. The controller must not know them — the
	// supervisor's spawn seam (opencodeServeCmd) owns the exports.
	pod := buildPodMandatory(t, reconcilerPinned(t))
	main := mainContainerOf(t, pod)

	for _, name := range []string{
		"OPENCODE_CONFIG",
		"OPENCODE_EXPERIMENTAL_EVENT_SYSTEM",
		"OPENCODE_SERVER_PASSWORD",
	} {
		_, ok := envVar(main, name)
		assert.False(t, ok, "%s must be set by the supervisor (agent seam), not the pod spec", name)
	}
}

// --- overlay pin env: always on ----------------------------------------------

func TestS3_OverlayPinEnvAlwaysSet(t *testing.T) {
	pod := buildPodMandatory(t, reconcilerPinned(t))
	main := mainContainerOf(t, pod)

	assertEnvValue(t, main, "AGENTD_IMAGE_VOLUME", "1")
	assertEnvValue(t, main, "LLMSAFESPACES_AGENTD_BINARY", agentdMountPath+agentdBinaryRelPath)
	assertEnvValue(t, main, "LLMSAFESPACES_AGENTD_SHA256_AMD64", testAgentdSHAAMD64)
	assertEnvValue(t, main, "LLMSAFESPACES_AGENTD_SHA256_ARM64", testAgentdSHAARM64)

	assertEnvValue(t, main, string(opencodeOverlayEnvKey), "1")
	assertEnvValue(t, main, "LLMSAFESPACES_OPENCODE_BINARY", opencodeMountPath+opencodeBinaryRelPath)
	assertEnvValue(t, main, "LLMSAFESPACES_OPENCODE_SHA256_AMD64", testOpencodeSHAAMD64)
	assertEnvValue(t, main, "LLMSAFESPACES_OPENCODE_SHA256_ARM64", testOpencodeSHAARM64)
}

func assertEnvValue(t *testing.T, c *corev1.Container, name, want string) {
	t.Helper()
	v, ok := envVar(c, name)
	require.True(t, ok, "env %s missing", name)
	assert.Equal(t, want, v.Value, name)
}

// compile-time: the workspace type stays referenced for future slices.
var _ = v1.WorkspacePhaseCreating
