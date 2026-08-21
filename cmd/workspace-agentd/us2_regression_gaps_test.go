// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// us2_regression_gaps_test.go — pins for the gaps found and fixed during
// US-2 (#980/#982) that had NO deterministic regression test at merge
// time. Each test names the failure it prevents:
//
//  1. Adapter factory injection (CI hang): the spawn-env wrapper silently
//     used defaultOpencodeCmdFactory — the production `opencode` argv.
//     Tests injected the base factory into managedProcess but the WRAPPER
//     ignored it; on machines with `opencode` on PATH everything passed,
//     and CI only failed as a 5-minute test timeout. Pin: the wrapper must
//     compose on the adapter's injected baseCmdFactory.
//  2. skipHealthProbe: the supervisor's post-restart probe would 401
//     against the sidecar's bearer-gated readyz forever (the supervisor
//     must never hold the admin token, D1). Pin: the flag suppresses the
//     probe AND the supervisor command's construction sets it.
//  3. Supervisor metrics wiring: the cgroup reader existed but was never
//     wired into the supervisor's control server (caught only as an
//     unused-func lint). Pin: the construction seam installs a live
//     metrics source.
//  4. Reload-path write modes: the three 0640 sites in secrets.go had no
//     mode tests (only the configwriter/materializer/marker sites did).
//     Pin: each writer leaves group-readable files (shared gid 1000).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
)

// --- Gap 1: adapter base-factory injection ---------------------------------

// TestManagedProcAdapter_WrapperUsesInjectedBaseFactory is the CI-hang
// regression: SetSpawnEnv's factory wrapper must build on the ADAPTER's
// baseCmdFactory, not on defaultOpencodeCmdFactory. The factory product
// is inspected structurally (no child spawned): Path must be the injected
// base's path and Env must be exactly the spawn env. A regression to the
// production factory shows up as Path=="opencode" — failing everywhere,
// not only on runners without opencode installed.
func TestManagedProcAdapter_WrapperUsesInjectedBaseFactory(t *testing.T) {
	withTestLogger(t)
	baseBinary := "/usr/bin/definitely-not-opencode-fake-base"
	p := &managedProcess{}

	adapter := &managedProcAdapter{p: p}
	adapter.baseCmdFactory = func() *exec.Cmd {
		cmd := exec.Command(baseBinary)
		cmd.Env = []string{"BASE_MARKER=1"}
		return cmd
	}
	adapter.SetSpawnEnv(map[string]string{"HANDED": "env"})

	factory := p.cmdFactory
	require.NotNil(t, factory, "SetSpawnEnv must install a factory wrapper")
	cmd := factory()
	require.Equal(t, baseBinary, cmd.Path,
		"the wrapper must compose on the injected baseCmdFactory — a Path of \"opencode\" is the CI-hang regression (#980 round 2)")
	// US-4a merge semantics: the delta rides ON TOP of the base env
	// (platform vars win — A.4 forbids the sidecar composing the parent).
	require.Contains(t, cmd.Env, "HANDED=env")
	require.Contains(t, cmd.Env, "BASE_MARKER=1", "base factory env retained (merge, not replace)")
}

// TestManagedProcAdapter_NilBaseFactoryFallsBackToDefault pins the lazy
// resolution behaviorally (func values are not comparable): nil
// baseCmdFactory must resolve to the production factory — the built cmd
// carries the production argv. If the fallback breaks, sidecar-mode
// reloads stop applying env entirely.
func TestManagedProcAdapter_NilBaseFactoryFallsBackToDefault(t *testing.T) {
	p := &managedProcess{}
	adapter := &managedProcAdapter{p: p}
	cmd := adapter.factory()()
	require.Equal(t, "opencode", filepath.Base(cmd.Path),
		"nil baseCmdFactory must resolve to defaultOpencodeCmdFactory (production argv)")
	require.Contains(t, cmd.Args, "serve")
}

// --- Gap 2: skipHealthProbe -------------------------------------------------

// TestManagedProcess_SkipHealthProbeSuppressesPostRestartProbe: with the
// flag set, restart() must NOT probe healthCheckURL. A stub server counts
// hits; zero hits within the probe's first-poll window is the pass
// condition. The positive control (flag clear → probe fires) runs first
// so a broken probe path cannot fake a pass.
func TestManagedProcess_SkipHealthProbeSuppressesPostRestartProbe(t *testing.T) {
	withTestLogger(t)

	// Positive control: WITHOUT the flag, the probe fires.
	hits := &atomic.Int64{}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(stub.Close)

	p := newTestManagedProcess(t, freeTCPPort(t), 0)
	p.healthCheckURL = stub.URL
	p.start()
	p.restart()
	require.Eventually(t, func() bool { return hits.Load() > 0 },
		4*time.Second, 100*time.Millisecond, "positive control: probe must fire when skipHealthProbe is clear")
	p.stop()

	// Regression: WITH the flag, no probe.
	hits2 := &atomic.Int64{}
	stub2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits2.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(stub2.Close)

	p2 := newTestManagedProcess(t, freeTCPPort(t), 0)
	p2.healthCheckURL = stub2.URL
	p2.skipHealthProbe = true
	p2.start()
	defer p2.stop()
	p2.restart()

	time.Sleep(1500 * time.Millisecond) // first probe poll fires at +1s when not skipped
	require.Zero(t, hits2.Load(),
		"skipHealthProbe must suppress the post-restart probe — in supervisor mode the URL targets the sidecar's bearer-gated readyz and every probe would 401 (D1: the supervisor never holds the token)")
}

// TestNewSupervisorProcess_SupervisorModeFlags pins the supervisor
// command's process construction: probe skipped, no session-tracker hook.
func TestNewSupervisorProcess_SupervisorModeFlags(t *testing.T) {
	proc := newSupervisorProcess()
	require.True(t, proc.skipHealthProbe,
		"the supervisor must not run the post-restart health probe (bearer-gated sidecar readyz; D1 token boundary)")
	require.Nil(t, proc.onChildStarted,
		"no session tracker in supervisor mode — policy lives in the sidecar")
}

// --- Gap 3: supervisor metrics wiring ---------------------------------------

// TestNewSupervisorControlServer_WiresLiveMetricsSource is the
// unused-reader regression: the supervisor's control server must carry a
// non-nil metrics source. The source is read directly (values are
// environment-dependent; the contract is presence + no error path).
func TestNewSupervisorControlServer_WiresLiveMetricsSource(t *testing.T) {
	withTestLogger(t)
	p := newTestManagedProcess(t, freeTCPPort(t), 0)
	p.healthCheckURL = ""
	p.start()
	defer p.stop()

	srv, err := newSupervisorControlServer("127.0.0.1:0", &managedProcAdapter{p: p})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.close() })
	require.NotNil(t, srv.metricsSource,
		"the supervisor must serve workspace-cgroup metrics — an unwired reader is the #980 lint-catch regression")

	m := srv.metricsSource()
	require.NotNil(t, m)
	require.GreaterOrEqual(t, m.MemoryCurrentBytes, int64(0))
	require.GreaterOrEqual(t, m.CPUUsageUsec, int64(0))
}

// --- Gap 4: reload-path write modes -----------------------------------------

// TestReloadWritePaths_GroupReadable pins 0640 on the three reload-path
// writers in secrets.go. These files are read across the uid split
// (sidecar uid 2000 writes/stamps; uid-1000 opencode reads) via the
// pod's shared gid 1000 — a 0600 regression breaks sidecar-mode reload
// with a bare EACCES.
func TestReloadWritePaths_GroupReadable(t *testing.T) {
	dir := t.TempDir()
	agentCfg := filepath.Join(dir, "agent-config.json")
	// Base config so the read-modify-write paths have something to read.
	require.NoError(t, os.WriteFile(agentCfg, []byte(`{"$schema":"x"}`), 0o640))
	secretsPath := filepath.Join(dir, "secrets.json")
	// workspace-config.json beside secrets.json (the path convention the
	// writer derives).
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace-config.json"),
		[]byte(`{"defaultModel":"anthropic/claude"}`), 0o600))

	applyWorkspaceConfig(agentCfg, secretsPath)
	info, err := os.Stat(agentCfg)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o640), info.Mode().Perm(),
		"applyWorkspaceConfig must leave agent-config.json group-readable")

	mcpCfg := filepath.Join(dir, "agent-config-mcp.json")
	require.NoError(t, os.WriteFile(mcpCfg, []byte(`{}`), 0o640))
	applyMCPServersToConfig(mcpCfg, []secrets.StagedMCPServer{
		{Name: "llmsafespaces", Transport: "http", URL: "http://127.0.0.1:4097/v1/mcp"},
	})
	info, err = os.Stat(mcpCfg)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o640), info.Mode().Perm(),
		"applyMCPServersToConfig must leave agent-config.json group-readable")

	warnPath := filepath.Join(dir, "model-resolution-warning.json")
	writeModelResolutionWarning(warnPath, "anthropic/claude")
	info, err = os.Stat(warnPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o640), info.Mode().Perm(),
		"writeModelResolutionWarning must leave the marker group-readable (sidecar writes, uid-1000 reads)")
	data, err := os.ReadFile(warnPath)
	require.NoError(t, err)
	var payload struct {
		DefaultModel string `json:"defaultModel"`
	}
	require.NoError(t, json.Unmarshal(data, &payload))
	require.Equal(t, "anthropic/claude", payload.DefaultModel)
}

// --- bonus pin: production metrics path is real ------------------------------

// TestWorkspaceCgroupReader_ProductionPathsReadable documents the local
// environment dependency honestly: on a cgroup-v2 host (CI containers,
// live pods) the production paths must be readable and yield a positive
// memory figure; where cgroupfs is absent the reader must degrade to
// zeros, never error.
func TestWorkspaceCgroupReader_ProductionPathsReadable(t *testing.T) {
	m := newWorkspaceCgroupReader().read()
	require.NotNil(t, m)
	if data, err := os.ReadFile("/sys/fs/cgroup/memory.current"); err == nil && len(data) > 0 {
		require.Positive(t, m.MemoryCurrentBytes,
			"cgroup v2 present: memory.current must parse to a positive value")
	}
}
