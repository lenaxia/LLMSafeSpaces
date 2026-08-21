// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TestCredentialScript_ExecutesUnderRealSh is the F1 regression guard:
// the credential-setup init script runs under /bin/sh (dash on Debian),
// where [[ ]] is a silent no-op — the admin-token install skipped itself
// while every string-assertion test stayed green. This test EXECUTES the
// generated script in a sandbox dir with the projected Secret layout and
// asserts the file actually lands mode 0400.
func TestCredentialScript_ExecutesUnderRealSh(t *testing.T) {
	if _, err := exec.LookPath("install"); err != nil {
		t.Skip("install(1) not available on this runner")
	}

	ws := makeWorkspace("ws-shexec", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "pvc-" + ws.Name
	ws.Spec.Runtime = "base"
	r := reconcilerFor(t, makeRuntimeEnv("base"))
	require.NoError(t, r.ensurePasswordSecret(context.Background(), ws))
	// ensurePasswordSecret created the Secret WITH the admin-token key
	// (file mode). Fetch it so the projection layout mirrors production.
	sec := &corev1.Secret{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: passwordSecretName(ws.Name), Namespace: "default"}, sec))

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)
	script := adminTokenBlock(t, credSetupScript(t, pod))

	// Sandbox the projected-Secret + destination layout.
	dir := t.TempDir()
	pwDir := filepath.Join(dir, "mnt", "secrets", "password")
	cfgDir := filepath.Join(dir, "sandbox-cfg")
	home := filepath.Join(dir, "home")
	workspace := filepath.Join(dir, "workspace")
	runtimeDir := filepath.Join(dir, "sandbox-runtime")
	for _, d := range []string{pwDir, cfgDir, home, workspace, runtimeDir + "/rt"} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	for _, b := range []byte("placeholder") {
		_ = b
	}
	require.NoError(t, os.WriteFile(filepath.Join(pwDir, "password"), sec.Data["password"], 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(pwDir, "admin-token"), sec.Data["admin-token"], 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "free-models.json"), []byte("{}"), 0o644))

	// Hermetic rewrite: the generated block's absolute paths (/mnt/...,
	// /sandbox-cfg) point at read-only container mounts in test envs;
	// substitute the sandbox dir while keeping the guard + install logic
	// byte-identical in structure. F1 was a control-flow bug (dash
	// no-op'ing [[ ]]); that is exactly what this execution exercises.
	rewritten := strings.ReplaceAll(strings.ReplaceAll(script,
		"/mnt/secrets/password", pwDir),
		"/sandbox-cfg", cfgDir)

	cmd := exec.Command("/bin/sh", "-c", rewritten)
	cmd.Env = []string{
		"PATH=/usr/bin:/bin",
		"WORKSPACE_ID=" + ws.Name,
		"LLMSAFESPACE_API_URL=http://test:8080",
		"HOME=" + home,
		"XDG_DATA_HOME=" + workspace + "/.local",
	}
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "script must run clean under /bin/sh; output:\n%s", out)

	// THE assertion F1 broke: the token file must actually exist.
	info, statErr := os.Stat(filepath.Join(cfgDir, "admin-token"))
	require.NoError(t, statErr,
		"admin-token file must be INSTALLED (F1: the [[ ]] bashism silently skipped this under dash); output:\n%s", out)
	assert.Equal(t, os.FileMode(0o400), info.Mode().Perm(), "installed mode must be 0400")
}

// TestCredentialScript_LegacySecretSkipsInstallUnderRealSh: the legacy
// guard must ALSO work under dash — absent key → no file, no error.
func TestCredentialScript_LegacySecretSkipsInstallUnderRealSh(t *testing.T) {
	if _, err := exec.LookPath("install"); err != nil {
		t.Skip("install(1) not available on this runner")
	}

	sec := makePasswordSecret("ws-shlegacy", "default") // no admin-token key
	ws := makeWorkspace("ws-shlegacy", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "pvc-" + ws.Name
	ws.Spec.Runtime = "base"
	r := reconcilerFor(t, sec, makeRuntimeEnv("base"))
	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)
	script := adminTokenBlock(t, credSetupScript(t, pod))

	dir := t.TempDir()
	pwDir := filepath.Join(dir, "mnt", "secrets", "password")
	cfgDir := filepath.Join(dir, "sandbox-cfg")
	home := filepath.Join(dir, "home")
	workspace := filepath.Join(dir, "workspace")
	for _, d := range []string{pwDir, cfgDir, home, workspace} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(pwDir, "password"), sec.Data["password"], 0o600))

	rewritten := strings.ReplaceAll(strings.ReplaceAll(script,
		"/mnt/secrets/password", pwDir),
		"/sandbox-cfg", cfgDir)

	cmd := exec.Command("/bin/sh", "-c", rewritten)
	cmd.Env = []string{
		"PATH=/usr/bin:/bin",
		"WORKSPACE_ID=" + ws.Name,
		"LLMSAFESPACE_API_URL=http://test:8080",
		"HOME=" + home,
		"XDG_DATA_HOME=" + workspace + "/.local",
	}
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "legacy script must run clean; output:\n%s", out)

	_, statErr := os.Stat(filepath.Join(cfgDir, "admin-token"))
	assert.True(t, os.IsNotExist(statErr), "no admin-token key → no file (legacy env mode)")
}

// TestCredentialScript_NoBashisms: mechanical guard — the generated init
// scripts must never contain [[ (works even where /bin/sh IS bash and
// execution tests can't catch the portability break).
func TestCredentialScript_NoBashisms(t *testing.T) {
	ws := makeWorkspace("ws-bashism", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "pvc-" + ws.Name
	ws.Spec.Runtime = "base"
	r := reconcilerFor(t, makeRuntimeEnv("base"))
	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	for _, ic := range pod.Spec.InitContainers {
		require.Len(t, ic.Command, 3, "init container %s must be sh -c <script>", ic.Name)
		require.Equal(t, "/bin/sh", ic.Command[0])
		for _, line := range strings.Split(ic.Command[2], "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") {
				continue // comments may name the banned construct
			}
			assert.NotContains(t, line, "[[",
				"init script %s must be POSIX sh (no [[ ]] — dash silently no-ops it)", ic.Name)
		}
	}
}

// adminTokenBlock extracts the generated admin-token install block (the
// guard, comment, and install line) from the credential-setup script.
// Full-script execution is unsafe in test containers (absolute /workspace,
// /home/sandbox, /sandbox-runtime paths); executing the extracted block
// still runs the exact generated bytes of the F1-affected logic under a
// real /bin/sh. The end anchor is the US-4b sidecar-branch comment: the
// bootstrap invocation text also appears INSIDE that branch, so anchoring
// on it would truncate the extracted script mid-if (US-4b).
func adminTokenBlock(t *testing.T, script string) string {
	t.Helper()
	start := strings.Index(script, "# #887 D5.1")
	end := strings.Index(script, "# Design 0051 US-4b")
	if end < 0 {
		end = strings.Index(script, "workspace-agentd bootstrap")
	}
	require.GreaterOrEqual(t, start, 0, "admin-token block must exist in the script")
	require.Greater(t, end, start, "a block boundary must follow the admin-token block")
	return script[start:end]
}
