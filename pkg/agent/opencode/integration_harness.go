// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// Package-shared integration boot harness: the REAL pinned opencode
// binary behind the seam, booted the one proven way (env discipline,
// detach, log-to-file, readiness wait). Lives in a tagged non-test
// file so OUT-OF-PACKAGE integration tests (the cmd/workspace-agentd
// origin e2e, #1469) can boot the identical harness via
// StartIntegrationServer — a bespoke boot there drifted once and
// wedged; drift is now structurally impossible.
package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const opencodeVersion = "1.18.10"

func findOrDownloadBinary(t *testing.T) string {
	t.Helper()

	envBinary := os.Getenv("OPENCODE_BINARY")
	if envBinary != "" {
		if _, err := os.Stat(envBinary); err == nil { //nolint:gosec // G703 operator-supplied path, tag-gated harness
			return envBinary
		}
	}

	cacheDir := filepath.Join(os.TempDir(), "opencode-test-binaries")
	binPath := filepath.Join(cacheDir, fmt.Sprintf("opencode-%s", opencodeVersion))

	if _, err := os.Stat(binPath); err == nil {
		return binPath
	}

	t.Logf("Downloading opencode v%s ...", opencodeVersion)
	require.NoError(t, os.MkdirAll(cacheDir, 0o755)) //nolint:gosec // G301 test-harness pattern (tag-gated)

	arch := "x64"
	url := fmt.Sprintf(
		"https://github.com/anomalyco/opencode/releases/download/v%s/opencode-linux-%s.tar.gz",
		opencodeVersion, arch,
	)

	tmp := filepath.Join(cacheDir, "opencode.tar.gz")
	out, err := os.Create(tmp)
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil) //nolint:gosec // G107 pinned release URL, tag-gated test download
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G107 pinned release URL, tag-gated test download
	if err != nil {
		_ = out.Close()
		require.NoError(t, err)
	}
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode, "download failed: %s", url)

	_, err = io.Copy(out, resp.Body)
	_ = out.Close()
	require.NoError(t, err)

	require.NoError(t, exec.CommandContext(context.Background(), "tar", "-xzf", tmp, "-C", cacheDir, "opencode").Run()) //nolint:gosec // G204 fixed argv, tag-gated test extraction

	extracted := filepath.Join(cacheDir, "opencode")
	require.NoError(t, os.Rename(extracted, binPath)) //nolint:gosec // G703 trusted cache path, tag-gated
	require.NoError(t, os.Chmod(binPath, 0o755))      //nolint:gosec // G302 binary must be executable
	require.NoError(t, os.Remove(tmp))

	return binPath
}

type opencodeServer struct {
	baseURL string
	ctx     context.Context
	cancel  context.CancelFunc
	t       *testing.T
}

func startOpencodeServer(t *testing.T, port int) *opencodeServer {
	t.Helper()
	return startOpencodeServerWithConfig(t, port, legacyIntegrationConfig)
}

// legacyIntegrationConfig is the historical integration-provider config
// (opencode.ai zen) the provider/config endpoint tests run against.
const legacyIntegrationConfig = `{
	"$schema": "https://opencode.ai/config.json",
	"provider": {
		"opencode": {
			"options": {
				"apiKey": "public",
				"baseURL": "https://opencode.ai/zen/v1"
			}
		}
	}
}`

// startOpencodeServerWithConfig boots the real opencode binary with an
// arbitrary agent config (the loopback L2 tests point it at a local mock
// provider so sends complete offline).
func startOpencodeServerWithConfig(t *testing.T, port int, configJSON string) *opencodeServer {
	t.Helper()

	binary := findOrDownloadBinary(t)

	configDir := t.TempDir()
	dataDir := filepath.Join(configDir, "data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755)) //nolint:gosec // G301 tempdir, tag-gated //nolint:gosec // G301 test-harness pattern (tag-gated)

	agentConfigPath := filepath.Join(configDir, "agent-config.json")
	require.NoError(t, os.WriteFile(agentConfigPath, []byte(configJSON), 0o644)) //nolint:gosec // G302 test-harness pattern (tag-gated)
	xdgConfigDir := filepath.Join(configDir, "opencode")
	require.NoError(t, os.MkdirAll(xdgConfigDir, 0o755))                                                       //nolint:gosec // G301 tempdir, tag-gated
	require.NoError(t, os.WriteFile(filepath.Join(xdgConfigDir, "opencode.jsonc"), []byte(configJSON), 0o644)) //nolint:gosec // G302 test-harness pattern (tag-gated)

	ctx, cancel := context.WithCancel(context.Background()) //nolint:gosec // G118 test-harness pattern (tag-gated)

	cmd := exec.CommandContext(ctx, binary, "serve", //nolint:gosec // G204 test-harness pattern (tag-gated)
		"--hostname", "127.0.0.1",
		"--port", fmt.Sprintf("%d", port),
	)
	// EXPLICIT env, not os.Environ()+overrides: duplicated vars resolve
	// first-match in the child's libc, so an inherited HOME/XDG_* from the
	// runner (a workspace pod carries its own opencode config + auth
	// there) silently overrides the test's configDirs and the instance
	// answers with the POD's providers instead of the test's — the exact
	// failure mode live-debugged 2026-09-13 (send hangs, mock never
	// dialed, "ServeError" in stderr). Path/TMPDIR are the only
	// pass-throughs the binary needs.
	cmd.Env = append(os.Environ(),
		"OPENCODE_CONFIG="+agentConfigPath,
		"XDG_DATA_HOME="+dataDir,
		"XDG_CONFIG_HOME="+configDir,
		"HOME="+configDir,
		"OPENCODE_SERVER_PASSWORD=test-password",
	)
	cmd.Dir = configDir
	// Session-detached, mirroring how agentd's supervisor actually
	// spawns opencode and how every live-validated manual boot ran: the
	// pinned binary's outbound fetches misbehave when it remains in the
	// test runner's process group (live-debugged 2026-09-13 — same
	// binary/config/mock answers perfectly when detached, "Cannot
	// connect to API" for every provider call when not).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	// stderr/stdout to a FILE, not a pipe: a pipe consumer that stalls
	// or dies can block the child's stderr writer, and opencode places
	// writes on the request path under load (live-debugged 2026-09-13:
	// harness boots served health but hung every message send; the
	// shell-booted identical binary+config served the same sends in
	// ~1s). The file also survives for post-mortem; the tail is dumped
	// into the test log at cleanup.
	logPath := filepath.Join(configDir, "oc.stderr.log")
	logFile, err := os.Create(logPath)
	require.NoError(t, err)
	cmd.Stderr = logFile
	cmd.Stdout = logFile

	require.NoError(t, cmd.Start())

	t.Cleanup(func() {
		_ = logFile.Close()
		if data, rerr := os.ReadFile(logPath); rerr == nil {
			lines := strings.Split(string(data), "\n")
			if len(lines) > 40 {
				lines = lines[len(lines)-40:]
			}
			for _, l := range lines {
				t.Logf("[opencode] %s", l)
			}
		}
		// The structured file log carries the boot stages stderr never
		// shows (config/MCP/plugin subsystems) — the difference between
		// a wedged boot and a working one (live-debugged #1469 r2).
		if data, rerr := os.ReadFile(filepath.Join(dataDir, "opencode", "log", "opencode.log")); rerr == nil {
			lines := strings.Split(string(data), "\n")
			if len(lines) > 40 {
				lines = lines[len(lines)-40:]
			}
			for _, l := range lines {
				if strings.TrimSpace(l) != "" {
					t.Logf("[opencode-file] %s", l)
				}
			}
		}
	})

	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(os.Interrupt)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
			}
		}
	})

	srv := &opencodeServer{
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", port),
		ctx:     ctx,
		cancel:  cancel,
		t:       t,
	}

	srv.waitForHealthy(30 * time.Second)
	return srv
}

func (s *opencodeServer) waitForHealthy(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	// Bounded probe client: an unbounded Do() hangs forever when the
	// child boots into a stalled state (its catalog fetch can blackhole
	// on egress-filtered runners — the kernel accepts the SYN, the app
	// never answers). Bounded probes turn that into a retry, then a
	// loud timeout, instead of an infinite hang (live-debugged
	// 2026-09-13).
	probeClient := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, s.baseURL+"/global/health", nil) //nolint:gosec // G107 harness baseURL //nolint:gosec // G107 test-harness pattern (tag-gated)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		req.SetBasicAuth("opencode", "test-password")
		resp, err := probeClient.Do(req)
		if err == nil {
			var body map[string]interface{}
			_ = json.NewDecoder(resp.Body).Decode(&body)
			_ = resp.Body.Close()
			if healthy, _ := body["healthy"].(bool); healthy {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.t.Fatal("opencode did not become healthy within " + timeout.String())
}

// StartIntegrationServer is the tag-gated export for out-of-package
// integration tests needing the real binary booted the proven way.
func StartIntegrationServer(t *testing.T, port int, configJSON string) *opencodeServer {
	t.Helper()
	return startOpencodeServerWithConfig(t, port, configJSON)
}

// IntegrationBaseURL returns the booted server's loopback base URL.
func (s *opencodeServer) IntegrationBaseURL() string { return s.baseURL }
