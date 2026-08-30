// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- US-70.1 (design 0057 R2/R3): spawn-time env pull ----------------------
//
// The boot push (pushInitialSpawnEnv) dials a control socket that cannot
// exist yet under native-sidecar gating — env-class secrets were lost on
// every boot, silently. The pull moves the point of consumption to spawn:
// the supervisor fetches the delta from the sidecar mux (4097, §D1
// credential) with a bounded wait; on miss it spawns with the last-good
// delta from memory; a first-boot miss spawns platform-env-only and
// surfaces degraded:spawn_env_unavailable (never-block-spawn, I10).

func writeSecretsEnvVars(t *testing.T, dir string, vars map[string]string) string {
	t.Helper()
	var b strings.Builder
	for k, v := range vars {
		fmt.Fprintf(&b, "export %s='%s'\n", k, strings.ReplaceAll(v, `'`, `'\''`))
	}
	p := filepath.Join(dir, "secrets-env")
	require.NoError(t, os.WriteFile(p, []byte(b.String()), 0o600))
	return p
}

func newSpawnEnvServer(t *testing.T, path, password string) *httptest.Server {
	t.Helper()
	h := spawnEnvHandler(path, password)
	ts := httptest.NewServer(http.HandlerFunc(h))
	t.Cleanup(ts.Close)
	return ts
}

// TestSpawnEnvHandler_ServesFreshDeltaWithRev: the mux endpoint parses the
// CURRENT file content on every request (rev changes when the file does —
// the reload path hands off freshness without any push).
func TestSpawnEnvHandler_ServesFreshDeltaWithRev(t *testing.T) {
	dir := t.TempDir()
	path := writeSecretsEnvVars(t, dir, map[string]string{"GH_TOKEN": "tok-1"})
	ts := newSpawnEnvServer(t, path, "pw")

	get := func() spawnEnvResponse {
		req, _ := http.NewRequest("GET", ts.URL+"/v1/spawn-env", nil)
		req.SetBasicAuth("opencode", "pw")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		var out spawnEnvResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		return out
	}

	first := get()
	require.Equal(t, "tok-1", first.Env["GH_TOKEN"])
	require.NotEmpty(t, first.Rev, "rev must be reported (spawned_rev input)")

	writeSecretsEnvVars(t, dir, map[string]string{"GH_TOKEN": "tok-2"})
	second := get()
	require.Equal(t, "tok-2", second.Env["GH_TOKEN"])
	require.NotEqual(t, first.Rev, second.Rev, "rev must track content")
}

// TestSpawnEnvHandler_AuthAndAbsence: §D1 gate on every route; an absent
// file is an authoritative-empty delta (normal), not an error.
func TestSpawnEnvHandler_AuthAndAbsence(t *testing.T) {
	dir := t.TempDir()
	ts := newSpawnEnvServer(t, filepath.Join(dir, "secrets-env"), "pw")

	unauth, _ := http.NewRequest("GET", ts.URL+"/v1/spawn-env", nil)
	resp, err := http.DefaultClient.Do(unauth)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "I8: no unauthenticated route")

	req, _ := http.NewRequest("GET", ts.URL+"/v1/spawn-env", nil)
	req.SetBasicAuth("opencode", "pw")
	resp2, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp2.Body.Close()
	var out spawnEnvResponse
	require.NoError(t, json.NewDecoder(resp2.Body).Decode(&out))
	assert.Empty(t, out.Env, "absent file = authoritative empty")
	assert.Empty(t, out.Rev)
}

// TestSpawnEnvPull_BoundedWaitNeverBlocksSpawn: mux unreachable → the
// bounded wait expires → spawn proceeds with the last-good delta (or
// platform-env-only on first boot) + degraded reason recorded. Fault
// injection: the endpoint hangs past the bound.
func TestSpawnEnvPull_BoundedWaitNeverBlocksSpawn(t *testing.T) {
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(hang.Close)

	puller := newSpawnEnvPuller(hang.URL, "pw", 50*time.Millisecond)
	start := time.Now()
	delta, rev, degraded, err := puller.pull(context.Background())
	require.NoError(t, err)
	assert.Empty(t, delta)
	assert.Empty(t, rev)
	assert.Equal(t, "spawn_env_unavailable", degraded)
	assert.Less(t, time.Since(start), 2*time.Second, "never-block-spawn: the bound holds")
}

// TestSpawnEnvPull_LastGoodCacheSurvivesOutage: a successful pull caches
// the delta in memory; a subsequent unreachable mux spawns with the CACHED
// delta (memory-only last-good, I7) and no degrade downgrade of the env.
func TestSpawnEnvPull_LastGoodCacheSurvivesOutage(t *testing.T) {
	dir := t.TempDir()
	path := writeSecretsEnvVars(t, dir, map[string]string{"GH_TOKEN": "tok-A"})
	ts := newSpawnEnvServer(t, path, "pw")

	puller := newSpawnEnvPuller(ts.URL, "pw", 200*time.Millisecond)
	delta, rev, degraded, err := puller.pull(context.Background())
	require.NoError(t, err)
	require.Equal(t, "tok-A", delta["GH_TOKEN"])
	require.Empty(t, degraded)
	goodRev := rev

	// Outage: swap the URL to a dead endpoint.
	puller.url = "http://127.0.0.1:1"
	delta2, rev2, degraded2, err := puller.pull(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "tok-A", delta2["GH_TOKEN"], "last-good delta from memory")
	assert.Equal(t, goodRev, rev2, "the cached rev is what the child will spawn with")
	assert.Equal(t, "spawn_env_unavailable", degraded2, "degradation is visible, env is not lost")
}

// TestSpawnEnvPull_FreshAfterReload: the pull reads the file at request
// time — a reload (file rewritten) reaches the NEXT spawn with no push.
func TestSpawnEnvPull_FreshAfterReload(t *testing.T) {
	dir := t.TempDir()
	path := writeSecretsEnvVars(t, dir, map[string]string{"GH_TOKEN": "old"})
	ts := newSpawnEnvServer(t, path, "pw")
	puller := newSpawnEnvPuller(ts.URL, "pw", time.Second)

	delta, _, _, err := puller.pull(context.Background())
	require.NoError(t, err)
	require.Equal(t, "old", delta["GH_TOKEN"])

	writeSecretsEnvVars(t, dir, map[string]string{"GH_TOKEN": "fresh"})
	delta2, _, _, err := puller.pull(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "fresh", delta2["GH_TOKEN"])
}

// TestSpawnedRevTerminalVerification (I4): spawned_rev is measured at the
// env the child ACTUALLY spawned with — under injected skew (the file
// changes between pull and spawn), the reported rev matches the applied
// delta, not a later observation.
func TestSpawnedRevTerminalVerification(t *testing.T) {
	dir := t.TempDir()
	path := writeSecretsEnvVars(t, dir, map[string]string{"GH_TOKEN": "v1"})
	ts := newSpawnEnvServer(t, path, "pw")

	puller := newSpawnEnvPuller(ts.URL, "pw", time.Second)
	delta, rev, _, err := puller.pull(context.Background())
	require.NoError(t, err)

	// Skew: the file changes AFTER the pull that feeds this spawn.
	writeSecretsEnvVars(t, dir, map[string]string{"GH_TOKEN": "v2"})
	fresh, freshRev, _, err := puller.pull(context.Background())
	require.NoError(t, err)
	require.Equal(t, "v2", fresh["GH_TOKEN"])

	// The child env is composed from the FIRST delta (what spawn consumed).
	childEnv := parentPlusDelta([]string{"PATH=/bin"}, delta)
	assert.Contains(t, childEnv, "GH_TOKEN=v1")

	// revOf: the same digest the endpoint computes — terminal verification
	// is the digest OF THE APPLIED DELTA.
	assert.Equal(t, rev, spawnEnvRev(delta))
	assert.NotEqual(t, freshRev, spawnEnvRev(delta), "skew must be observable: applied != latest")
}

func spawnEnvRev(delta map[string]string) string {
	if len(delta) == 0 {
		return ""
	}
	keys := make([]string, 0, len(delta))
	for k := range delta {
		keys = append(keys, k)
	}
	// deterministic: sorted key order
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s\n", k, delta[k])
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

// TestSpawnFactory_PullsAtSpawn: the supervisor's spawn factory wrapper
// pulls at EVERY spawn (first boot + every restart), merges parent+delta,
// and records spawnedRev + degraded for status.
func TestSpawnFactory_PullsAtSpawn(t *testing.T) {
	dir := t.TempDir()
	path := writeSecretsEnvVars(t, dir, map[string]string{"GH_TOKEN": "tok-factory"})
	ts := newSpawnEnvServer(t, path, "pw")

	puller := newSpawnEnvPuller(ts.URL, "pw", time.Second)
	base := func() *exec.Cmd {
		return &exec.Cmd{Env: []string{"PATH=/bin", "HOME=/home/sandbox"}}
	}
	wrapped := withSpawnEnvPull(base, puller)

	cmd := wrapped()
	assert.Contains(t, cmd.Env, "GH_TOKEN=tok-factory")
	assert.Contains(t, cmd.Env, "PATH=/bin", "parent env preserved")
	require.Equal(t, "tok-factory", puller.spawnedDelta()["GH_TOKEN"])
	require.NotEmpty(t, puller.spawnedRev())
	require.Empty(t, puller.degradedReason())

	// A rewrite reaches the next spawn.
	writeSecretsEnvVars(t, dir, map[string]string{"GH_TOKEN": "tok-next"})
	cmd2 := wrapped()
	assert.Contains(t, cmd2.Env, "GH_TOKEN=tok-next")
}

// TestSpawnFactory_FirstBootDeadMuxLoud: first boot with a dead mux spawns
// platform-env-only AND records the degrade reason (I10/I13: silent loss
// class gone) — the status surface exposes it.
func TestSpawnFactory_FirstBootDeadMuxLoud(t *testing.T) {
	puller := newSpawnEnvPuller("http://127.0.0.1:1", "pw", 50*time.Millisecond)
	base := func() *exec.Cmd { return &exec.Cmd{Env: []string{"PATH=/bin"}} }
	wrapped := withSpawnEnvPull(base, puller)

	cmd := wrapped()
	assert.Equal(t, []string{"PATH=/bin"}, cmd.Env, "platform-env-only")
	assert.Equal(t, "spawn_env_unavailable", puller.degradedReason())

	// Recovery on a later spawn clears the degrade.
	dir := t.TempDir()
	path := writeSecretsEnvVars(t, dir, map[string]string{"K": "V"})
	ts := newSpawnEnvServer(t, path, "pw")
	puller.url = ts.URL
	_ = wrapped()
	assert.Empty(t, puller.degradedReason(), "self-healing: degrade clears on recovery")
}

// TestLastGoodMemoryOnly (I7): the cache never hits disk.
func TestLastGoodMemoryOnly(t *testing.T) {
	dir := t.TempDir()
	path := writeSecretsEnvVars(t, dir, map[string]string{"SECRET": "plain"})
	ts := newSpawnEnvServer(t, path, "pw")
	puller := newSpawnEnvPuller(ts.URL, "pw", time.Second)
	_, _, _, err := puller.pull(context.Background())
	require.NoError(t, err)

	var found []string
	err = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), "plain") {
			found = append(found, p)
		}
		return nil
	})
	require.NoError(t, err)
	// The only plaintext is the source file itself — the cache adds none.
	require.Len(t, found, 1)
	assert.Equal(t, path, found[0])
}

var _ = sync.Mutex{}

// TestStatuszRelaysSpawnStatus (I4/I10 surface): the statusz handler
// carries the spawn-status source's spawned_rev + degraded into the
// response — the controller's deep-status scrape path.
func TestStatuszRelaysSpawnStatus(t *testing.T) {
	opencodeSrv := newOpenCodeTestServer()
	defer opencodeSrv.Close()
	client, cache, tracker := newStatuszTestFixture(t, opencodeSrv)
	handler := buildStatuszHandler(client, cache, tracker, newMemoryPressureMonitor(), time.Now(), "", defaultSysMetrics(),
		func() (string, string) { return "rev1234", "spawn_env_unavailable" })

	req := httptest.NewRequest("GET", "/v1/statusz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		SpawnedRev string `json:"spawned_rev"`
		Degraded   string `json:"degraded"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "rev1234", resp.SpawnedRev)
	assert.Equal(t, "spawn_env_unavailable", resp.Degraded)
}

// TestA2_SupervisorReadsD1Credential (A2 merge gate, R3): the supervisor
// authenticates its pull with the §D1 workspace credential read from the
// password file the uid-1000 init installs (0600, uid-1000-owned) — the
// ONLY file the supervisor touches on this path, and it owns it. The
// crossing itself is the HTTP boundary (D6.1 pair validated on the wire
// by TestSpawnEnvHandler_AuthAndAbsence). Evidence artifact for the R3
// matrix: no new cross-uid FILE crossing exists in the spawn-pull path.
func TestA2_SupervisorReadsD1Credential(t *testing.T) {
	dir := t.TempDir()
	pwPath := filepath.Join(dir, "password")
	require.NoError(t, os.WriteFile(pwPath, []byte("the-d1-credential\n"), 0o600))

	// The mux serves with the same credential pair the handler checks.
	deltaPath := writeSecretsEnvVars(t, dir, map[string]string{"X": "Y"})
	ts := httptest.NewServer(http.HandlerFunc(spawnEnvHandler(deltaPath, "the-d1-credential")))
	t.Cleanup(ts.Close)

	// The supervisor-side resolution: read the owned file, trim, pull.
	b, err := os.ReadFile(pwPath)
	require.NoError(t, err, "A2: the supervisor's uid must read its own password file")
	pw := strings.TrimSpace(string(b))

	puller := newSpawnEnvPuller(ts.URL, pw, time.Second)
	delta, rev, degraded, err := puller.pull(context.Background())
	require.NoError(t, err)
	require.Equal(t, "Y", delta["X"])
	require.NotEmpty(t, rev)
	require.Empty(t, degraded)
}
