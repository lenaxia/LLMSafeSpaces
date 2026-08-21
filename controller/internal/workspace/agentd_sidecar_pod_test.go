// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// agentd_sidecar_pod_test.go — US-2 (design 0051 §D1/§4a): the native
// sidecar split at the pod-spec level.
//
// Invariants these tests lock in:
//
//   - Disabled (default): pod is byte-for-byte the single-container shape —
//     no agentd init container, no AGENTD_SIDECAR_MODE on the main
//     container, liveness probe still HTTP healthz (regression pin).
//   - Enabled: an `agentd` native sidecar (init container with
//     restartPolicy Always) runs the SAME digest-pinned agentd image with
//     `--sidecar`, at uid 2000 / gid 1000, credentials via ENV from the
//     password Secret (uid-2000 space — the 0600 uid-1000 files in
//     /sandbox-cfg are unreadable cross-uid), mounted sandbox-cfg RO +
//     sandbox-runtime RW, with a startup probe that gates the main
//     container on the #857 stamp-before-opencode-reads guarantee.
//   - Ordering: the sidecar is the LAST init container (after
//     credential-setup), so materialize's base agent-config.json exists
//     before the sidecar stamps platform blocks onto it.
//   - Main container in sidecar mode: AGENTD_SIDECAR_MODE=1 and liveness
//     switches to a kernel-level TCP probe on opencode's port (a dead
//     PID-1 supervisor means a dead container; an alive-but-wedged one is
//     an accepted residual — HTTP liveness targeting the sidecar's mux
//     would restart the WORKSPACE container on a sidecar wedge).
//   - Validation: enabling the sidecar without agentdDelivery is a
//     configuration error caught at controller startup.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/stretchr/testify/require"
)

func reconcilerWithAgentdSidecar(t *testing.T) *WorkspaceReconciler {
	t.Helper()
	r := reconcilerWithAgentd(t)
	r.AgentdSidecarEnabled = true
	return r
}

func sidecarInitContainer(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == name {
			return &pod.Spec.InitContainers[i]
		}
	}
	return nil
}

func sidecarEnvVar(c *corev1.Container, name string) *corev1.EnvVar {
	for i := range c.Env {
		if c.Env[i].Name == name {
			return &c.Env[i]
		}
	}
	return nil
}

func sidecarVolumeMount(c *corev1.Container, name string) *corev1.VolumeMount {
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == name {
			return &c.VolumeMounts[i]
		}
	}
	return nil
}

// --- disabled (default) regression pins -----------------------------------

func TestAgentdSidecar_DisabledByDefault_NoSidecarContainer(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerWithAgentd(t) // overlay delivery on, sidecar OFF

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	require.Nil(t, sidecarInitContainer(pod, "agentd"),
		"no agentd sidecar unless agentdSidecar is enabled")
	main := &pod.Spec.Containers[0]
	require.Nil(t, sidecarEnvVar(main, "AGENTD_SIDECAR_MODE"),
		"main container must not carry sidecar-mode env when disabled")
	require.NotNil(t, main.LivenessProbe.HTTPGet,
		"single-container liveness stays HTTP /v1/healthz on the admin port")
	require.Equal(t, int(agentd.AgentdAdminPort), main.LivenessProbe.HTTPGet.Port.IntValue())
}

// --- enabled: sidecar container shape --------------------------------------

func TestAgentdSidecar_Enabled_NativeSidecarContainer(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerWithAgentdSidecar(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	sc := sidecarInitContainer(pod, "agentd")
	require.NotNil(t, sc, "agentd native sidecar must exist when enabled")
	require.NotNil(t, sc.RestartPolicy, "native sidecar requires an explicit restartPolicy")
	require.Equal(t, corev1.ContainerRestartPolicyAlways, *sc.RestartPolicy,
		"restartPolicy Always is what makes an init container a native sidecar (KEP-753)")
	require.Equal(t, testAgentdImage, sc.Image,
		"sidecar runs the SAME digest-pinned artifact — single-artifact provenance (#863 D1)")
	require.Equal(t, []string{"/agentd" + "/usr/local/bin/workspace-agentd", "--sidecar"}, sc.Command)
}

func TestAgentdSidecar_Enabled_UID2000GID1000(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerWithAgentdSidecar(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	sc := sidecarInitContainer(pod, "agentd")
	require.NotNil(t, sc)
	sec := sc.SecurityContext
	require.NotNil(t, sec)
	require.NotNil(t, sec.RunAsUser)
	require.Equal(t, int64(2000), *sec.RunAsUser, "sidecar uid 2000 (design 0051 §4a)")
	require.NotNil(t, sec.RunAsGroup)
	require.Equal(t, int64(1000), *sec.RunAsGroup, "sidecar gid 1000 = workspace group: shared-file reads stay possible")
	require.NotNil(t, sec.RunAsNonRoot)
	require.True(t, *sec.RunAsNonRoot)
	require.False(t, *sec.AllowPrivilegeEscalation)
	require.NotNil(t, sec.Capabilities)
	require.Equal(t, []corev1.Capability{"ALL"}, sec.Capabilities.Drop,
		"no capability grants — §6 rejects CAP_SETUID/CAP_KILL buy-backs")
	require.NotNil(t, sec.ReadOnlyRootFilesystem)
	require.True(t, *sec.ReadOnlyRootFilesystem)
}

func TestAgentdSidecar_Enabled_CredentialsViaEnvOnly(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerWithAgentdSidecar(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	sc := sidecarInitContainer(pod, "agentd")
	require.NotNil(t, sc)

	pw := sidecarEnvVar(sc, "AGENTD_SIDECAR_PASSWORD")
	require.NotNil(t, pw, "sidecar password arrives via env")
	require.NotNil(t, pw.ValueFrom.SecretKeyRef, "…from the password Secret")
	require.Equal(t, passwordSecretName(ws.Name), pw.ValueFrom.SecretKeyRef.Name)
	require.Equal(t, "password", pw.ValueFrom.SecretKeyRef.Key,
		"US-2 keeps the workspace password; the distinct agentdPassword key is US-3")

	tok := sidecarEnvVar(sc, "AGENTD_ADMIN_TOKEN")
	require.NotNil(t, tok, "sidecar admin token arrives via env")
	require.NotNil(t, tok.ValueFrom.SecretKeyRef)
	require.Equal(t, "admin-token", tok.ValueFrom.SecretKeyRef.Key,
		"the #933 distinct admin-token key; env delivery is safe in the sidecar's own container env (uid-2000 space, no child inherits it)")

	// The sidecar must NOT be wired with the token FILE env: the file in
	// /sandbox-cfg is uid-1000-owned 0400 — unreadable at uid 2000.
	require.Nil(t, sidecarEnvVar(sc, "AGENTD_ADMIN_TOKEN_FILE"))
}

func TestAgentdSidecar_Enabled_Mounts(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerWithAgentdSidecar(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	sc := sidecarInitContainer(pod, "agentd")
	require.NotNil(t, sc)

	cfg := sidecarVolumeMount(sc, "sandbox-cfg")
	require.NotNil(t, cfg)
	require.True(t, cfg.ReadOnly, "sandbox-cfg RO: the sidecar reads, never writes, uid-1000-owned bootstrap artifacts")

	rt := sidecarVolumeMount(sc, "sandbox-runtime")
	require.NotNil(t, rt)
	require.False(t, rt.ReadOnly, "sandbox-runtime RW: the sidecar stamps agent-config.json (#857) and owns the reload cache")

	wsMnt := sidecarVolumeMount(sc, "workspace")
	require.NotNil(t, wsMnt, "workspace subPath mounted for statfs disk stats in statusz")
	require.True(t, wsMnt.ReadOnly, "the user's workspace is read-only to the sidecar")
	require.Equal(t, "workspace", wsMnt.SubPath)
}

func TestAgentdSidecar_Enabled_StartupProbeGatesStampBeforeRead(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerWithAgentdSidecar(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	sc := sidecarInitContainer(pod, "agentd")
	require.NotNil(t, sc)
	sp := sc.StartupProbe
	require.NotNil(t, sp,
		"native sidecar MUST carry a startup probe: the main container (opencode) must not start until the sidecar's mux serves — which happens only AFTER ensureBootAgentConfig stamped agent-config.json (#857 stamp-before-read)")
	require.NotNil(t, sp.HTTPGet)
	require.Equal(t, "/v1/healthz", sp.HTTPGet.Path)
	require.Equal(t, int(agentd.AgentdAdminPort), sp.HTTPGet.Port.IntValue())
	require.Empty(t, sp.HTTPGet.HTTPHeaders,
		"startup probe targets the deliberately-open /v1/healthz — no auth header needed (kubelet needs no Secret plumbing)")

	require.NotNil(t, sc.LivenessProbe, "sidecar liveness restarts a wedged sidecar without touching the workspace container")
	require.NotNil(t, sc.LivenessProbe.HTTPGet)
}

// TestAgentdSidecar_ReadinessProbeCarriesBearerToken is the L3-found
// regression (kind run 32421411899): /v1/readyz is bearer-gated once the
// admin token exists (#933), so the SIDECAR's readiness probe must carry
// the same Authorization header the MAIN container's probes do — without
// it the probe 401s forever and the pod never goes Ready even though the
// sidecar itself is serving (its own readyz_first_200 gate fired).
func TestAgentdSidecar_ReadinessProbeCarriesBearerToken(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerWithAgentdSidecar(t)

	// The Secret with the distinct admin-token key must exist for
	// buildPod's adminToken resolution (ensurePasswordSecret shape).
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      passwordSecretName(ws.Name),
			Namespace: ws.Namespace,
		},
		Data: map[string][]byte{
			"password":    []byte("pw"),
			"admin-token": []byte("the-admin-token"),
		},
	}
	require.NoError(t, r.Create(context.Background(), sec))

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	sc := sidecarInitContainer(pod, "agentd")
	require.NotNil(t, sc)
	rp := sc.ReadinessProbe
	require.NotNil(t, rp)
	require.NotNil(t, rp.HTTPGet)
	require.Equal(t, "/v1/readyz", rp.HTTPGet.Path)
	require.Len(t, rp.HTTPGet.HTTPHeaders, 1, "readiness probe MUST carry the bearer header (bearer-gated endpoint)")
	require.Equal(t, "Authorization", rp.HTTPGet.HTTPHeaders[0].Name)
	require.Equal(t, "Bearer the-admin-token", rp.HTTPGet.HTTPHeaders[0].Value)
}

func TestAgentdSidecar_Enabled_OrderingAfterCredentialSetup(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerWithAgentdSidecar(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	credIdx, sidecarIdx := -1, -1
	for i, c := range pod.Spec.InitContainers {
		switch c.Name {
		case "credential-setup":
			credIdx = i
		case "agentd":
			sidecarIdx = i
		}
	}
	require.GreaterOrEqual(t, credIdx, 0, "credential-setup init must exist")
	require.Greater(t, sidecarIdx, credIdx,
		"sidecar must start AFTER credential-setup: the base agent-config.json materialize writes must exist before the sidecar stamps platform blocks")
	require.Equal(t, len(pod.Spec.InitContainers)-1, sidecarIdx,
		"sidecar is the LAST init container")
}

func TestAgentdSidecar_Enabled_MainContainerSwitchesToSupervisorMode(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerWithAgentdSidecar(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	main := &pod.Spec.Containers[0]
	mode := sidecarEnvVar(main, "AGENTD_SIDECAR_MODE")
	require.NotNil(t, mode)
	require.Equal(t, "1", mode.Value,
		"entrypoint-opencode.sh branches on this to exec `workspace-agentd supervise-opencode`")

	// Liveness: kernel-level TCP on opencode's port. HTTP /v1/healthz would
	// be served by the SIDECAR in this mode — a wedged sidecar must restart
	// the SIDECAR, not the workspace container.
	lv := main.LivenessProbe
	require.NotNil(t, lv.TCPSocket, "sidecar-mode liveness must be a TCP probe")
	require.Equal(t, int(agentd.AgentPort), lv.TCPSocket.Port.IntValue(),
		"TCP on opencode's port: refused = opencode gone = restart the workspace container (supervisor included)")

	// Readiness/startup keep targeting the sidecar-served readyz (shared
	// netns): they gate traffic and boot on opencode's listener.
	require.NotNil(t, main.ReadinessProbe.HTTPGet)
	require.Equal(t, "/v1/readyz", main.ReadinessProbe.HTTPGet.Path)
	require.NotNil(t, main.StartupProbe.HTTPGet)
	require.Equal(t, "/v1/readyz", main.StartupProbe.HTTPGet.Path)
}

func TestAgentdSidecar_Enabled_WorkspaceIDAndRelayEnv(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerWithAgentdSidecar(t)
	r.InferenceRelayURL = "https://relay.example/"

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	sc := sidecarInitContainer(pod, "agentd")
	require.NotNil(t, sc)
	id := sidecarEnvVar(sc, "WORKSPACE_ID")
	require.NotNil(t, id, "ops-metrics labels and bootstrap identity need WORKSPACE_ID")
	require.Equal(t, ws.Name, id.Value)
	require.NotNil(t, sidecarEnvVar(sc, "INFERENCE_RELAY_BASEURL"),
		"the relay injector lives in the sidecar now")
	require.NotNil(t, sidecarEnvVar(sc, "XDG_DATA_HOME"),
		"auth.json discovery must match the main container's XDG_DATA_HOME")
}

// --- validation -------------------------------------------------------------

func TestAgentdSidecar_Validation(t *testing.T) {
	err := validateAgentdSidecarConfig(true, "")
	require.Error(t, err, "sidecar without a delivery image has nothing to run")
	require.Contains(t, err.Error(), "--agentd-image")

	require.NoError(t, validateAgentdSidecarConfig(true, testAgentdImage))
	require.NoError(t, validateAgentdSidecarConfig(false, ""))
	require.NoError(t, validateAgentdSidecarConfig(false, testAgentdImage))
}

// --- POSIX guards -----------------------------------------------------------

// TestAgentdSidecar_EntrypointBranchExecLevel runs the REAL entrypoint
// under bash with AGENTD_SIDECAR_MODE=1 and a stubbed AGENTD_BIN, and
// asserts the branch execs `workspace-agentd supervise-opencode`. This is
// the exec-level guard the dash-bashism lesson (round 2 of #933) mandates
// for every new entrypoint line.
func TestAgentdSidecar_EntrypointBranchExecLevel(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping entrypoint regression")
	}
	script := "../../../runtimes/base/tools/entrypoints/entrypoint-opencode.sh"
	dir := t.TempDir()

	// Stub every external the script touches: mise, cat, and the agentd
	// binary itself (records argv — the assertion target).
	stubDir := filepath.Join(dir, "bin")
	require.NoError(t, os.MkdirAll(stubDir, 0o755))
	for _, name := range []string{"mise", "cat"} {
		p := filepath.Join(stubDir, name)
		require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	}
	agentdStub := filepath.Join(stubDir, "workspace-agentd")
	require.NoError(t, os.WriteFile(agentdStub, []byte("#!/bin/sh\necho \"argv=$*\"\nexit 0\n"), 0o755))

	// entrypoint-opencode.sh sources entrypoint-common.sh at its image
	// path (/usr/local/bin/...), which exists in the runtime image and
	// on live workspace pods but NOT on CI runners. Copy the script and
	// point the source line at the repo's real entrypoint-common.sh —
	// everything else runs verbatim.
	src, err := os.ReadFile(script)
	require.NoError(t, err)
	patched := strings.Replace(string(src),
		"source /usr/local/bin/entrypoint-common.sh",
		"source \""+entrypointScriptPath(t)+"\"", 1)
	require.NotContains(t, patched, "/usr/local/bin/entrypoint-common.sh",
		"the source line moved — update the patch to match the script")
	scriptCopy := filepath.Join(dir, "entrypoint-opencode-test.sh")
	require.NoError(t, os.WriteFile(scriptCopy, []byte(patched), 0o755))

	// entrypoint-opencode.sh execs "${AGENTD_BIN}" --supervise (legacy
	// tail); in sidecar mode it must exec supervise-opencode instead.
	// AGENTD_IMAGE_VOLUME is explicitly blanked: this sandbox is itself a
	// live workspace pod carrying overlay env (AGENTD_IMAGE_VOLUME=1 +
	// real pins) that would bypass the stub with the production binary.
	cmd := exec.Command("bash", "-c",
		`set -euo pipefail; export PATH="`+stubDir+`:$PATH"; `+
			`export AGENTD_IMAGE_VOLUME=; `+
			`export AGENTD_BIN="`+agentdStub+`"; `+
			`export AGENTD_SIDECAR_MODE=1; `+
			`bash "`+scriptCopy+`"`)
	cmd.Env = append(os.Environ(),
		"OPENCODE_CONFIG="+filepath.Join(dir, "agent-config.json"),
		"XDG_DATA_HOME="+filepath.Join(dir, "xdg"),
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "output: %s", out)
	require.Contains(t, string(out), "argv=supervise-opencode",
		"sidecar-mode entrypoint must exec the supervisor subcommand; output: %s", out)
}

// entrypointScriptPath resolves the repo's entrypoint-common.sh relative
// to this test file.
func entrypointScriptPath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(entrypointScript)
	require.NoError(t, err)
	_, err = os.Stat(abs)
	require.NoError(t, err, "entrypoint-common.sh not found at %s", abs)
	return abs
}

// --- US-3: control-plane credential wiring -----------------------------------

// TestAgentdSidecar_ControlPlanePasswordEnvAndIsolation: the sidecar gets
// AGENTD_CONTROL_PLANE_PASSWORD from the NEW agentdPassword Secret key
// (§D1), and — the whole point of the split — the MAIN container must
// carry NO reference to it (the secret must never exist in uid-1000
// space).
func TestAgentdSidecar_ControlPlanePasswordEnvAndIsolation(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      passwordSecretName(ws.Name),
			Namespace: ws.Namespace,
		},
		Data: map[string][]byte{
			"password":       []byte("pw"),
			"admin-token":    []byte("tok"),
			"agentdPassword": []byte("the-agentd-pw"),
		},
	}
	r := reconcilerWithAgentdSidecar(t)
	require.NoError(t, r.Create(context.Background(), sec))

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	sc := sidecarInitContainer(pod, "agentd")
	require.NotNil(t, sc)
	cp := sidecarEnvVar(sc, "AGENTD_CONTROL_PLANE_PASSWORD")
	require.NotNil(t, cp, "sidecar must receive the control-plane credential")
	require.NotNil(t, cp.ValueFrom.SecretKeyRef)
	require.Equal(t, "agentdPassword", cp.ValueFrom.SecretKeyRef.Key,
		"the NEW §D1 Secret key")
	require.Equal(t, passwordSecretName(ws.Name), cp.ValueFrom.SecretKeyRef.Name)

	// Isolation: the workspace container (uid-1000 space) must have NO
	// env var sourced from the agentdPassword key — by name OR by key.
	main := &pod.Spec.Containers[0]
	require.Nil(t, sidecarEnvVar(main, "AGENTD_CONTROL_PLANE_PASSWORD"),
		"the control-plane credential must never be wired into uid-1000 space")
	for _, e := range main.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			require.NotEqual(t, "agentdPassword", e.ValueFrom.SecretKeyRef.Key,
				"main container env %s references the agentdPassword key — uid-1000 leak", e.Name)
		}
	}
}
