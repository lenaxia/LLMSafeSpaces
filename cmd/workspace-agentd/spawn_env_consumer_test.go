// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// spawn_env_consumer_test.go — design 0051 US-4a: the US-0.2(a) IPC
// handoff, consumer side, end-to-end.
//
// US-2 shipped the supervisor's storage (SetSpawnEnv → next spawn) and
// the socket verbs; what did NOT exist is anyone DRIVING it:
//
//   - the SIDECAR never pushed anything at boot (the supervisor's first
//     spawn had no secrets env in sidecar mode once the file relocates);
//   - the reload path never pushed the fresh delta nor restarted (deps.proc
//     is nil in sidecar mode — files applied, opencode never restarted);
//   - the supervisor REPLACED the child env wholesale on SetSpawnEnv,
//     which cannot work when the sidecar sends only the secrets delta
//     (A.4 forbids env OUT of the supervisor — the sidecar cannot learn
//     the supervisor's parent env to compose it; the merge happens on
//     the supervisor side: parent + delta, platform vars win, mirroring
//     buildEnvFrom's file-delta semantics).

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// writeSecretsEnv writes a bash-sourceable env file (the materializer's
// applyEnvSecret format: KEY='shell-quoted value' lines).
func writeSecretsEnv(t *testing.T, dir, content string) string {
	t.Helper()
	p := filepath.Join(dir, "secrets-env")
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	return p
}

// TestParseSecretsEnvDelta_QuotingAndNoise: the parser must return the
// file-introduced variables ONLY — shell noise (SHLVL bumps, PWD, _) and
// pre-existing parent keys are excluded; shell-quoted values round-trip.
func TestParseSecretsEnvDelta_QuotingAndNoise(t *testing.T) {
	dir := t.TempDir()
	p := writeSecretsEnv(t, dir, ""+
		"export USER_KEY_SIMPLE='abc123'\n"+
		"export USER_KEY_SPACES='hello world'\n"+
		// shellquote.Bash renders it's quoted as 'it'\''s quoted
		"export USER_KEY_QUOTE='it'\\''s quoted'\n"+
		"export USER_KEY_MULTILINE='line1\nline2'\n")

	delta, err := parseSecretsEnvDelta(p)
	require.NoError(t, err)
	require.Equal(t, "abc123", delta["USER_KEY_SIMPLE"])
	require.Equal(t, "hello world", delta["USER_KEY_SPACES"])
	require.Equal(t, "it's quoted", delta["USER_KEY_QUOTE"])
	require.Contains(t, delta["USER_KEY_MULTILINE"], "line1")

	for _, noise := range []string{"SHLVL", "PWD", "OLDPWD", "_"} {
		require.NotContains(t, delta, noise, "shell noise key %q must not leak into the delta", noise)
	}
}

// TestParseSecretsEnvDelta_AbsentFileIsEmpty: no file → empty delta, no
// error (pods without env-secrets boot normally).
func TestParseSecretsEnvDelta_AbsentFileIsEmpty(t *testing.T) {
	delta, err := parseSecretsEnvDelta(filepath.Join(t.TempDir(), "nope"))
	require.NoError(t, err)
	require.Empty(t, delta)
}

// TestPushInitialSpawnEnv_BootHandoff: the sidecar's boot push lands the
// file's delta in the supervisor's stored spawn env over the REAL socket.
func TestPushInitialSpawnEnv_BootHandoff(t *testing.T) {
	proc := &fakeRestartProc{}
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", proc)
	go srv.serve()

	p := writeSecretsEnv(t, t.TempDir(), "export BOOT_SECRET='boot-value'\n")
	require.NoError(t, pushInitialSpawnEnv(newControlClient(srv.addr()), p))

	env := *proc.lastEnv.Load()
	require.Equal(t, "boot-value", env["BOOT_SECRET"])
}

// TestPushInitialSpawnEnv_EmptyFileNoPush: an empty/absent delta issues
// NO SpawnEnv call (nothing to hand off; supervisor keeps defaults).
func TestPushInitialSpawnEnv_EmptyFileNoPush(t *testing.T) {
	proc := &fakeRestartProc{}
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", proc)
	go srv.serve()

	require.NoError(t, pushInitialSpawnEnv(newControlClient(srv.addr()), filepath.Join(t.TempDir(), "nope")))
	require.Nil(t, proc.lastEnv.Load(), "no spawn_env call when the delta is empty")
}

// --- supervisor-side merge semantics -----------------------------------------

// TestManagedProcAdapter_SpawnEnvMergesWithParent: the composition is
// parent + delta (platform vars win, mirroring buildEnvFrom), NOT
// wholesale replacement — the delta carries only the secrets because
// A.4 forbids reading the supervisor's env back out.
func TestManagedProcAdapter_SpawnEnvMergesWithParent(t *testing.T) {
	p := &managedProcess{}
	base := func() (env []string) { return []string{"PLATFORM_VAR=platform", "SHARED=parent-wins"} }
	adapter := &managedProcAdapter{p: p, baseCmdFactory: mkFactoryEnv(base)}
	adapter.SetSpawnEnv(map[string]string{
		"USER_SECRET": "delta-value",
		"SHARED":      "delta-loses",
	})
	cmd := adapter.composeChild()
	require.Contains(t, cmd.Env, "PLATFORM_VAR=platform", "parent platform vars retained")
	require.Contains(t, cmd.Env, "USER_SECRET=delta-value", "delta applied")
	require.Contains(t, cmd.Env, "SHARED=parent-wins", "parent wins on conflict (buildEnvFrom parity)")
	require.NotContains(t, cmd.Env, "SHARED=delta-loses")
}

// mkFactoryEnv builds a base factory whose cmd carries the given env
// (structural inspection only — nothing is spawned; a zero exec.Cmd is
// safe to construct and read).
func mkFactoryEnv(env func() []string) func() *exec.Cmd {
	return func() *exec.Cmd {
		return &exec.Cmd{Env: env()}
	}
}

// --- reload path: restart-only (US-70.1 removed the pre-restart push —
// the restarted child's spawn PULLS the fresh delta from the user mux) --

// TestSocketReloadProc_RestartsWithClosedEnum: the sidecar's reload
// restarter requests the credential_reload restart over the real socket
// and pushes NOTHING — delta delivery is the supervisor's spawn-time
// pull (pull-only correctness, design 0057 I3).
func TestSocketReloadProc_RestartsWithClosedEnum(t *testing.T) {
	withTestLogger(t)
	proc := &fakeRestartProc{}
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", proc)
	go srv.serve()

	rp := newSocketReloadProc(newControlClient(srv.addr()))
	rp.restart()

	require.Nil(t, proc.lastEnv.Load(), "no spawn_env push on the reload path — the pull owns delivery")
	require.Equal(t, "credential_reload", *proc.lastReason.Load(), "restart carries the closed reason enum")
	require.Equal(t, int64(1), proc.restarts.Load())
}

// TestSidecarDeps_WireReloadProc: buildSidecarDeps must hand the reload
// path a socket-backed restartableProcess (deps.proc is nil in sidecar
// mode — without this, reload applies files and NEVER restarts, the
// documented US-2 gap).
func TestSidecarDeps_WireReloadProc(t *testing.T) {
	withTestLogger(t)
	t.Setenv("AGENTD_CONTROL_PLANE_PASSWORD", "cp")
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", &fakeRestartProc{})
	go srv.serve()

	deps := buildSidecarDeps(sidecarConfig{
		password:    "pw",
		adminToken:  "tok",
		controlAddr: srv.addr(),
	})
	require.NotNil(t, deps.reloadProc,
		"the sidecar's reload path needs a socket-backed restarter — nil means files apply but opencode never restarts")
	require.Nil(t, deps.proc, "the sidecar must NOT own a managedProcess")
}

// TestReloadHandler_SidecarEndToEnd: the REAL reload handler against the
// socket-backed restarter — env-secret batch → materialized file, marker
// written to the env-overridden (shared-tmpfs) path, credential_reload
// restart requested over the real socket (US-70.1: no push — the
// restarted child's spawn pulls the fresh delta from the user mux).
func TestReloadHandler_SidecarEndToEnd(t *testing.T) {
	withTestLogger(t)
	dir := t.TempDir()
	secretsEnv := filepath.Join(dir, "secrets-env")
	marker := filepath.Join(dir, "marker.json")
	t.Setenv("LLMSAFESPACES_SECRETS_ENV_PATH", secretsEnv)
	t.Setenv("LLMSAFESPACES_RESTART_MARKER_PATH", marker)
	t.Setenv("LLMSAFESPACES_SECRETS_BASE_DIR", filepath.Join(dir, "rt", "secrets"))
	t.Setenv("LLMSAFESPACES_SSH_DIR", filepath.Join(dir, "rt", "ssh"))
	t.Setenv("LLMSAFESPACES_AGENT_CONFIG_PATH", filepath.Join(dir, "agent-config.json"))
	t.Setenv("LLMSAFESPACES_GIT_CREDS_PATH", filepath.Join(dir, "rt", "git-credentials"))
	t.Setenv("LLMSAFESPACES_RELOAD_CACHE_PATH", filepath.Join(dir, "last-reload.json"))

	proc := &fakeRestartProc{}
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", proc)
	go srv.serve()
	rp := newSocketReloadProc(newControlClient(srv.addr()))

	deps := reloadSecretsDeps{
		OpencodePassword: "pw",
		Proc:             rp,
	}
	h := reloadSecretsHandler(loadMaterializeConfig(), deps)

	// env-secret carries the variable name in metadata.var_name (the
	// materializer's contract), not in the secret name.
	body := bytes.NewBufferString(
		`[{"type":"env-secret","name":"my-provider-key","metadata":{"var_name":"MY_PROVIDER_KEY"},"plaintext":"sk-live-123"}]`)
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", body)
	req.SetBasicAuth("opencode", "pw")
	rec := httptest.NewRecorder()
	h(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// The env-secret materialized to the (overridden) secrets-env path —
	// this is what the supervisor's spawn-time pull will fetch.
	require.FileExists(t, secretsEnv, "env-secret must materialize to the configured path")

	// The restart fired with the enum; NOTHING was pushed — delivery is
	// the pull at the next spawn (I3).
	require.Nil(t, proc.lastEnv.Load(), "no spawn_env push on the reload path")
	require.Equal(t, "credential_reload", *proc.lastReason.Load())
	require.Equal(t, int64(1), proc.restarts.Load())

	// The marker landed on the env-overridden (shared, sidecar-writable)
	// path — NOT the PVC const the sidecar sees read-only. Its reason is
	// the FINE-GRAINED batch classification (env_secrets_changed); the
	// coarse closed enum (credential_reload) is the SOCKET reason, asserted
	// above via the supervisor-side proc — two different layers by design.
	data, err := os.ReadFile(marker)
	require.NoError(t, err, "reload must persist the restart-reason marker")
	require.Contains(t, string(data), "env_secrets_changed")
	require.Contains(t, string(data), "MY_PROVIDER_KEY")
}

// TestReloadHandler_SidecarEndToEnd_CrossUIDModes: with the sidecar's
// LLMSAFESPACES_CROSS_UID_FILES armed, the SAME handler's materialization
// must produce group-readable rt/* stores (US-4b: uid-2000 writes,
// uid-1000 tools read via gid 1000).
func TestReloadHandler_SidecarEndToEnd_CrossUIDModes(t *testing.T) {
	withTestLogger(t)
	dir := t.TempDir()
	secretsDir := filepath.Join(dir, "rt", "secrets")
	t.Setenv("LLMSAFESPACES_SECRETS_ENV_PATH", filepath.Join(dir, "secrets-env"))
	t.Setenv("LLMSAFESPACES_RESTART_MARKER_PATH", filepath.Join(dir, "marker.json"))
	t.Setenv("LLMSAFESPACES_SECRETS_BASE_DIR", secretsDir)
	t.Setenv("LLMSAFESPACES_SSH_DIR", filepath.Join(dir, "rt", "ssh"))
	t.Setenv("LLMSAFESPACES_AGENT_CONFIG_PATH", filepath.Join(dir, "agent-config.json"))
	t.Setenv("LLMSAFESPACES_GIT_CREDS_PATH", filepath.Join(dir, "rt", "git-credentials"))
	t.Setenv("LLMSAFESPACES_RELOAD_CACHE_PATH", filepath.Join(dir, "last-reload.json"))
	t.Setenv("LLMSAFESPACES_CROSS_UID_FILES", "1")

	proc := &fakeRestartProc{}
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", proc)
	go srv.serve()
	rp := newSocketReloadProc(newControlClient(srv.addr()))

	h := reloadSecretsHandler(loadMaterializeConfig(), reloadSecretsDeps{
		OpencodePassword: "pw",
		Proc:             rp,
	})
	body := bytes.NewBufferString(
		`[{"type":"secret-file","name":"app-cfg","metadata":{"mount_path":"app/config.env"},"plaintext":"tool-bytes"}]`)
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", body)
	req.SetBasicAuth("opencode", "pw")
	rec := httptest.NewRecorder()
	h(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	fileInfo, err := os.Stat(filepath.Join(secretsDir, "app", "config.env"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o640), fileInfo.Mode().Perm(),
		"cross-uid reload: secret-file must be gid-1000 readable")
	dirInfo, err := os.Stat(secretsDir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o770), dirInfo.Mode().Perm(),
		"cross-uid reload: secret-file root must be gid-1000 traversable")
}

// --- review round 1: boot ordering + degradation ------------------------------

// TestPushInitialSpawnEnv_DeadSocketFailsFast: an unreachable supervisor
// surfaces as an error WITHIN the 5s budget (never a hang), so the boot
// sequence's log-and-continue branch is reachable and bounded.
func TestPushInitialSpawnEnv_DeadSocketFailsFast(t *testing.T) {
	deadPort := freeTCPPort(t)
	cc := newControlClient(fmt.Sprintf("127.0.0.1:%d", deadPort))
	cc.timeout = 5 * time.Second

	p := writeSecretsEnv(t, t.TempDir(), "export BOOT_SECRET='x'\n")
	begin := time.Now()
	err := pushInitialSpawnEnv(cc, p)
	require.Error(t, err, "dead socket must surface an error, not silence")
	require.Less(t, time.Since(begin), 6*time.Second,
		"the boot push must be bounded by its timeout — took %v", time.Since(begin))
}

// TestPushInitialSpawnEnv_EmptyDeltaSkipsDeadSocket: with no delta, no
// socket call is made at all — a supervisor that is not yet up cannot
// even delay the boot when there is nothing to hand off.
func TestPushInitialSpawnEnv_EmptyDeltaSkipsDeadSocket(t *testing.T) {
	deadPort := freeTCPPort(t)
	cc := newControlClient(fmt.Sprintf("127.0.0.1:%d", deadPort))
	cc.timeout = 100 * time.Millisecond // would fail fast IF dialed

	begin := time.Now()
	require.NoError(t, pushInitialSpawnEnv(cc, filepath.Join(t.TempDir(), "nope")))
	require.Less(t, time.Since(begin), time.Second,
		"empty delta must short-circuit before any dial")
}
