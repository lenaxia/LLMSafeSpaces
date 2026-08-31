// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// spawn_env_pull_exec_test.go — US-70.1 (design 0057 R2) exec-level
// integration: the REAL `supervise-opencode` subcommand performing the
// spawn-time pull against a REAL HTTP mux serving the production
// spawnEnvHandler. This is the in-process analog of the story's
// acceptance criteria:
//
//   - AC-1 (first-spawn env): the FIRST child's /proc/<pid>/environ
//     carries the pulled vars, and spawned_rev equals the served
//     revision — the #1087-class boot loss is gone by construction.
//   - bounded wait / never-block-spawn: a dead mux never prevents the
//     spawn; it degrades loudly (spawn_env_unavailable) and keeps the
//     last-good delta in memory.
//   - loud degrade + self-heal: after the mux returns, the next restart
//     converges and the degrade clears.
//   - fresh-pull supersession: a changed delta on the mux replaces the
//     previous one at the next spawn (revocation is absence, I12).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const execPullPassword = "exec-pull-pw"

// startPullMux serves the PRODUCTION spawnEnvHandler over the
// secrets-env file at path. Returns the host:port the supervisor should
// pull from.
func startPullMux(t *testing.T, secretsEnvPath string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/v1/spawn-env", http.HandlerFunc(spawnEnvHandler(execPullPassword, "", secretsEnvPath)))
	mux.Handle("/v1/spawn-files", http.HandlerFunc(spawnFilesHandler(execPullPassword, "", filepath.Join(t.TempDir(), "staged"))))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

func pullEnvFor(addr string) []string {
	dir, _ := os.MkdirTemp("", "spawn-pull-rt-*")
	return []string{
		"LLMSAFESPACES_SPAWN_ENV_PULL_ADDR=" + addr,
		"OPENCODE_SERVER_PASSWORD=" + execPullPassword,
		// Hermetic file-delivery roots/ledger: absent these, the files
		// pull would target /sandbox-runtime (absent on CI runners) and
		// degrade every spawn with spawn_files_unavailable.
		fileDeliveryRootsEnvOverride + "=" + dir,
		fileDeliveryLedgerEnvOverride + "=" + filepath.Join(dir, "led.json"),
	}
}

func childEnviron(t *testing.T, pid int) string {
	t.Helper()
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	require.NoError(t, err)
	return string(data)
}

// TestSupervisorSubprocess_SpawnPull_FirstSpawnEnvAndRev (AC-1 analog):
// cold boot with the mux already serving (the kubelet-gated sidecar is
// up before the workspace container starts) → the FIRST child's environ
// carries the pulled secret, the parent env rides along, and the
// reported spawned_rev is the revision of exactly what landed.
func TestSupervisorSubprocess_SpawnPull_FirstSpawnEnvAndRev(t *testing.T) {
	secretsEnv := writeSecretsEnv(t, t.TempDir(),
		"export PULL_PROBE='first-spawn-value'\n")
	muxAddr := startPullMux(t, secretsEnv)

	sp := startSupervisorSubprocessEnv(t, pullEnvFor(muxAddr)...)
	cc := newControlClient(sp.addr)

	var firstPID int
	require.Eventually(t, func() bool {
		firstPID = sp.childPIDOf(t, cc)
		return firstPID > 0
	}, 15*time.Second, 100*time.Millisecond, "first spawn must happen (pull must not block it)")

	environ := childEnviron(t, firstPID)
	require.Contains(t, environ, "PULL_PROBE=first-spawn-value\x00",
		"the FIRST child must spawn with the pulled delta — the #1087-class boot loss is gone")
	require.Contains(t, environ, "GO_TEST_SUPERVISOR=1",
		"merge semantics: the parent env rides along")

	st, err := cc.Status(context.Background())
	require.NoError(t, err)
	require.False(t, st.SpawnEnvDegraded)
	require.Empty(t, st.SpawnEnvReason)
	require.Equal(t, spawnDeltaRev(map[string]string{"PULL_PROBE": "first-spawn-value"}), st.SpawnedRev,
		"spawned_rev is the terminal revision of what the child actually spawned with")
}

// TestSupervisorSubprocess_SpawnPull_DeadMuxLoudNeverBlocks: first boot
// with the mux unreachable → the spawn still happens within the bound
// (never-block-spawn), the child is platform-env-only, and the degrade
// is loud and machine-readable in status.
func TestSupervisorSubprocess_SpawnPull_DeadMuxLoudNeverBlocks(t *testing.T) {
	boot := time.Now()
	sp := startSupervisorSubprocessEnv(t, pullEnvFor("127.0.0.1:1")...)
	cc := newControlClient(sp.addr)

	var firstPID int
	require.Eventually(t, func() bool {
		firstPID = sp.childPIDOf(t, cc)
		return firstPID > 0
	}, 15*time.Second, 100*time.Millisecond,
		"a dead sidecar mux must never block the spawn (bounded wait only)")

	// The bound is 2s; slack for process boot + probe loops.
	require.Less(t, time.Since(boot), 12*time.Second,
		"spawn must proceed within the bounded wait, not hang")

	environ := childEnviron(t, firstPID)
	require.Contains(t, environ, "GO_TEST_SUPERVISOR=1")
	require.False(t, strings.Contains(environ, "PULL_PROBE="),
		"platform-env-only is the degraded first-boot state")

	require.Eventually(t, func() bool {
		st, err := cc.Status(context.Background())
		return err == nil && st.SpawnEnvDegraded && st.SpawnEnvReason == spawnEnvReasonUnavailable
	}, 10*time.Second, 100*time.Millisecond,
		"first boot with a dead sidecar must surface degraded:spawn_env_unavailable — the silent loss class is gone")
}

// TestSupervisorSubprocess_SpawnPull_RecoveryAndLastGood: pull v1
// successfully, kill the mux (subsequent pulls fail), verify the
// last-good delta still reaches the next spawn + degrade appears; then
// bring a mux serving v2 up and verify convergence + degrade clearing.
func TestSupervisorSubprocess_SpawnPull_RecoveryAndLastGood(t *testing.T) {
	dir := t.TempDir()
	secretsEnv := writeSecretsEnv(t, dir, "export PULL_PROBE='v1'\n")
	muxAddr := startPullMux(t, secretsEnv)

	sp := startSupervisorSubprocessEnv(t, pullEnvFor(muxAddr)...)
	cc := newControlClient(sp.addr)

	var firstPID int
	require.Eventually(t, func() bool {
		firstPID = sp.childPIDOf(t, cc)
		return firstPID > 0
	}, 15*time.Second, 100*time.Millisecond)
	require.Contains(t, childEnviron(t, firstPID), "PULL_PROBE=v1\x00")

	// Corrupt the served delta source so pulls fail (handler 500s) — the
	// mux stays reachable, exercising the degraded-response class. Each
	// spawn now eats the full pull bound, so the restart's "next child
	// up" wait spans it; mirror the production reload caller's generous
	// control-client budget (socketReloadProc uses 60s).
	cc.timeout = 30 * time.Second
	require.NoError(t, os.WriteFile(secretsEnv, []byte("not the canonical encoder output\n"), 0o600))

	_, err := cc.Restart(context.Background(), "credential_reload", 5)
	require.NoError(t, err)

	var secondPID int
	require.Eventually(t, func() bool {
		pid := sp.childPIDOf(t, cc)
		if pid <= 0 || pid == firstPID {
			return false
		}
		secondPID = pid
		return true
	}, 15*time.Second, 100*time.Millisecond)
	require.Contains(t, childEnviron(t, secondPID), "PULL_PROBE=v1\x00",
		"the last-good delta from supervisor memory must survive a failed pull")
	require.Eventually(t, func() bool {
		st, err := cc.Status(context.Background())
		return err == nil && st.SpawnEnvDegraded && st.SpawnEnvReason != ""
	}, 10*time.Second, 100*time.Millisecond, "the failed pull must degrade loudly")

	// Recovery: serve v2 and restart — fresh pull wins, degrade clears.
	require.NoError(t, os.WriteFile(secretsEnv, []byte("export PULL_PROBE='v2'\n"), 0o600))
	_, err = cc.Restart(context.Background(), "credential_reload", 5)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		pid := sp.childPIDOf(t, cc)
		if pid <= 0 || pid == secondPID {
			return false
		}
		return strings.Contains(childEnvironRead(pid), "PULL_PROBE=v2\x00")
	}, 15*time.Second, 100*time.Millisecond, "the next spawn must pull the FRESH delta")

	require.Eventually(t, func() bool {
		st, err := cc.Status(context.Background())
		return err == nil && !st.SpawnEnvDegraded &&
			st.SpawnedRev == spawnDeltaRev(map[string]string{"PULL_PROBE": "v2"})
	}, 10*time.Second, 100*time.Millisecond, "recovery must clear the degrade and report the new terminal rev")
}

// childEnvironRead is the non-failing variant for Eventually loops.
func childEnvironRead(pid int) string {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return ""
	}
	return string(data)
}

// TestSupervisorSubprocess_SpawnPull_RevocationIsAbsence: a served EMPTY
// delta (owner unbound the env secret) must supersede a previously
// non-empty delta — absence is the delete (I12), not a stuck last-good.
func TestSupervisorSubprocess_SpawnPull_RevocationIsAbsence(t *testing.T) {
	dir := t.TempDir()
	secretsEnv := writeSecretsEnv(t, dir, "export REVOKED_PROBE='present'\n")
	muxAddr := startPullMux(t, secretsEnv)

	sp := startSupervisorSubprocessEnv(t, pullEnvFor(muxAddr)...)
	cc := newControlClient(sp.addr)

	var firstPID int
	require.Eventually(t, func() bool {
		firstPID = sp.childPIDOf(t, cc)
		return firstPID > 0
	}, 15*time.Second, 100*time.Millisecond)
	require.Contains(t, childEnviron(t, firstPID), "REVOKED_PROBE=present\x00")

	// Unbind: the materializer's reset() removes the file entirely —
	// the handler then serves the quiet empty delta.
	require.NoError(t, os.Remove(secretsEnv))
	_, err := cc.Restart(context.Background(), "credential_reload", 5)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		pid := sp.childPIDOf(t, cc)
		if pid <= 0 || pid == firstPID {
			return false
		}
		env := childEnvironRead(pid)
		return strings.Contains(env, "GO_TEST_SUPERVISOR=1") && !strings.Contains(env, "REVOKED_PROBE=")
	}, 15*time.Second, 100*time.Millisecond, "a successful empty pull must remove the revoked var from the next spawn")

	require.Eventually(t, func() bool {
		st, err := cc.Status(context.Background())
		return err == nil && !st.SpawnEnvDegraded &&
			st.SpawnedRev == spawnDeltaRev(map[string]string{})
	}, 10*time.Second, 100*time.Millisecond,
		"empty-but-healthy is the quiet 'no secrets bound' state — no degrade, empty-delta rev")
}

// TestSupervisorSubprocess_SpawnPull_BadCredentialDegrades: a wrong
// credential is a permanent failure — the child still spawns (never
// blocks) and the degrade names spawn_env_unauthorized, distinguishing
// the wiring bug from a dead mux.
func TestSupervisorSubprocess_SpawnPull_BadCredentialDegrades(t *testing.T) {
	secretsEnv := writeSecretsEnv(t, t.TempDir(), "export PULL_PROBE='never'\n")
	muxAddr := startPullMux(t, secretsEnv)

	sp := startSupervisorSubprocessEnv(t,
		"LLMSAFESPACES_SPAWN_ENV_PULL_ADDR="+muxAddr,
		"OPENCODE_SERVER_PASSWORD=wrong-credential")
	cc := newControlClient(sp.addr)

	var firstPID int
	require.Eventually(t, func() bool {
		firstPID = sp.childPIDOf(t, cc)
		return firstPID > 0
	}, 15*time.Second, 100*time.Millisecond, "an auth failure must never block the spawn")

	require.False(t, strings.Contains(childEnviron(t, firstPID), "PULL_PROBE="))
	require.Eventually(t, func() bool {
		st, err := cc.Status(context.Background())
		return err == nil && st.SpawnEnvDegraded && st.SpawnEnvReason == spawnEnvReasonUnauthorized
	}, 10*time.Second, 100*time.Millisecond)
}

// TestBuildUserMux_RegistersSpawnEnvEndpoint: the endpoint is wired into
// the user mux exactly once, at the §D1 gate (E2E wiring — an unwired
// handler would make every supervisor pull 404).
func TestBuildUserMux_RegistersSpawnEnvEndpoint(t *testing.T) {
	withTestLogger(t)
	t.Setenv("LLMSAFESPACES_SECRETS_ENV_PATH", writeSecretsEnv(t, t.TempDir(),
		"export WIRED_PROBE='yes'\n"))

	mux := buildUserMux(context.Background(), &sync.WaitGroup{}, serverDeps{
		password:             "pw",
		controlPlanePassword: "cp",
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/spawn-env", nil)
	req.SetBasicAuth("opencode", "pw")
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var got spawnEnvResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, map[string]string{"WIRED_PROBE": "yes"}, got.Env)
}
