// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Tests for the sidecar-absorbed bootstrap+materialize boot phase
// (design 0051 sidecar migration, step 1).
//
// TDD: authored before the implementation. runSidecarCommand gains a
// boot phase that runs BEFORE the muxes serve (the #857
// stamp-before-read guarantee rides the startup probe): the sidecar
// performs the credential-setup init container's bootstrap + materialize
// in-process, against /sandbox-runtime/rt/secrets.json (the sidecar's
// /sandbox-cfg mount is ReadOnly — bootstrap output relocates to the
// pod-scoped tmpfs it already owns).
//
// Lifecycle semantics under test (native sidecar ≠ init container — it
// RESTARTS):
//
//   - Idempotency guard: on a mid-pod-lifetime restart, an existing
//     non-empty secrets.json means bootstrap already ran for THIS pod —
//     the API is never re-hit (it may be down; the projected SA token
//     is long expired). Materialize re-runs: it is idempotent by design
//     (reset() + reinstall).
//   - Fresh boot: bootstrap fetches and materialize applies; exit 0.
//   - API down on fresh boot: bootstrap degrades to an empty batch and
//     the pod still boots (reload-secrets path recovers later) — the
//     never-block-boot contract is preserved.
//   - Materialize structural failure: propagated non-zero — the sidecar
//     exits, CrashLoopBackOff makes the failure VISIBLE (the 2026-08-25
//     incident class surfaces as a restart-reason, not a zombie).
//
//   - Fail-fast: whatever materialize returns, the sidecar returns.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setSidecarMaterializeEnv points the materialize path set at a temp
// tree (same env-override seam the subcommand tests use).
func setSidecarMaterializeEnv(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Setenv("LLMSAFESPACES_SECRETS_BASE_DIR", filepath.Join(dir, "secrets"))
	t.Setenv("LLMSAFESPACES_SSH_DIR", filepath.Join(dir, "ssh"))
	t.Setenv("LLMSAFESPACES_AGENT_CONFIG_PATH", filepath.Join(dir, "agent-config.json"))
	t.Setenv("LLMSAFESPACES_SECRETS_ENV_PATH", filepath.Join(dir, "secrets-env"))
	t.Setenv("LLMSAFESPACES_GIT_CREDS_PATH", filepath.Join(dir, "git-credentials"))
	t.Setenv("LLMSAFESPACES_RELOAD_CACHE_PATH", filepath.Join(dir, "reload-cache.json"))
	require.NoError(t, os.MkdirAll(os.Getenv("HOME"), 0o755))
}

// bootstrapAPIServer serves a pod-bootstrap payload with one env-secret.
func bootstrapAPIServer(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		if r.URL.Path != "/internal/v1/pod-bootstrap" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"secrets":[{"type":"env-secret","name":"e","metadata":{"var_name":"MY_VAR"},"plaintext":"my_value"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSidecarBootSecrets_FreshBoot_FetchesAndMaterializes: the full boot
// phase against a live bootstrap API — secrets fetched to the tmpfs
// path, env-secret materialized, exit 0.
func TestSidecarBootSecrets_FreshBoot_FetchesAndMaterializes(t *testing.T) {
	dir := t.TempDir()
	setSidecarMaterializeEnv(t, dir)

	var hits atomic.Int32
	srv := bootstrapAPIServer(t, &hits)

	tokenFile := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("projected-sa-token"), 0o600))

	secretsOut := filepath.Join(dir, "rt", "secrets.json")

	exit := runSidecarBootSecrets(sidecarBootOpts{
		WorkspaceID: "ws-test",
		APIURL:      srv.URL,
		TokenFile:   tokenFile,
		SecretsOut:  secretsOut,
		Stderr:      io.Discard,
	})
	require.Equal(t, 0, exit)
	require.Equal(t, int32(1), hits.Load(), "fresh boot hits the bootstrap API exactly once")

	got, err := os.ReadFile(secretsOut)
	require.NoError(t, err)
	assert.Contains(t, string(got), "MY_VAR", "bootstrap payload persisted to tmpfs secrets.json")

	env, err := os.ReadFile(filepath.Join(dir, "secrets-env"))
	require.NoError(t, err, "materialize ran against the fetched batch")
	assert.Contains(t, string(env), "export MY_VAR=")
}

// TestSidecarBootSecrets_RestartGuard_SkipsRefetch: mid-pod-lifetime
// restart — secrets.json already on the tmpfs (emptyDir survives
// container restart), the projected SA token is long expired, the API
// may be down. The guard must skip the fetch entirely; materialize
// re-runs idempotently; exit 0.
func TestSidecarBootSecrets_RestartGuard_SkipsRefetch(t *testing.T) {
	dir := t.TempDir()
	setSidecarMaterializeEnv(t, dir)

	var hits atomic.Int32
	srv := bootstrapAPIServer(t, &hits)

	secretsOut := filepath.Join(dir, "rt", "secrets.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(secretsOut), 0o755))
	require.NoError(t, os.WriteFile(secretsOut, []byte(`[{"type":"env-secret","name":"e","metadata":{"var_name":"MY_VAR"},"plaintext":"restart_value"}]`), 0o600))

	// No token file at all: expired projection.
	exit := runSidecarBootSecrets(sidecarBootOpts{
		WorkspaceID: "ws-test",
		APIURL:      srv.URL,
		TokenFile:   filepath.Join(dir, "no-such-token"),
		SecretsOut:  secretsOut,
		Stderr:      io.Discard,
	})
	require.Equal(t, 0, exit, "restart with existing secrets must boot clean")
	require.Zero(t, hits.Load(), "the guard must not hit the bootstrap API on restart")

	env, err := os.ReadFile(filepath.Join(dir, "secrets-env"))
	require.NoError(t, err)
	assert.Contains(t, string(env), "restart_value", "materialize replayed the persisted batch")
}

// TestSidecarBootSecrets_APIDown_FreshBoot_StillBoots: the never-block-
// boot contract survives the absorption — an unreachable API degrades
// to an empty batch, exit 0 (the reload-secrets path recovers later).
func TestSidecarBootSecrets_APIDown_FreshBoot_StillBoots(t *testing.T) {
	dir := t.TempDir()
	setSidecarMaterializeEnv(t, dir)

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close() // unreachable port

	exit := runSidecarBootSecrets(sidecarBootOpts{
		WorkspaceID: "ws-test",
		APIURL:      closed.URL,
		TokenFile:   filepath.Join(dir, "token"),
		SecretsOut:  filepath.Join(dir, "rt", "secrets.json"),
		Stderr:      io.Discard,
	})
	require.Equal(t, 0, exit, "API-down must not block sidecar boot")

	got, err := os.ReadFile(filepath.Join(dir, "rt", "secrets.json"))
	require.NoError(t, err)
	assert.Equal(t, "[]", string(got), "degraded to empty batch")
}

// TestSidecarBootSecrets_CrossUIDWriteProfile: with
// LLMSAFESPACES_CROSS_UID_FILES=1 (armed on the sidecar since US-4b),
// materialized credential outputs carry 0640 — the supervisor (uid 1000)
// reads secrets-env via the shared gid 1000 bridge. Without the flag,
// 0600 (legacy byte-identical). A mode regression here breaks sidecar
// boot with a SILENT env-secrets degradation (buildEnvFrom EACCES-
// degrades), which is why it is pinned at this layer too.
func TestSidecarBootSecrets_CrossUIDWriteProfile(t *testing.T) {
	for _, tc := range []struct {
		name     string
		flag     string
		wantMode os.FileMode
	}{
		{"cross-uid on", "1", 0o640},
		{"cross-uid off (legacy)", "", 0o600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			setSidecarMaterializeEnv(t, dir)
			if tc.flag != "" {
				t.Setenv("LLMSAFESPACES_CROSS_UID_FILES", tc.flag)
			}

			secretsOut := filepath.Join(dir, "rt", "secrets.json")
			require.NoError(t, os.MkdirAll(filepath.Dir(secretsOut), 0o755))
			require.NoError(t, os.WriteFile(secretsOut, []byte(`[{"type":"env-secret","name":"e","metadata":{"var_name":"MY_VAR"},"plaintext":"v"}]`), 0o600))

			exit := runSidecarBootSecrets(sidecarBootOpts{
				WorkspaceID: "ws-test",
				APIURL:      "http://127.0.0.1:1", // unreachable — guard skips (batch exists)
				TokenFile:   filepath.Join(dir, "token"),
				SecretsOut:  secretsOut,
				Stderr:      io.Discard,
			})
			require.Equal(t, 0, exit)

			info, err := os.Stat(filepath.Join(dir, "secrets-env"))
			require.NoError(t, err, "secrets-env materialized")
			assert.Equal(t, tc.wantMode, info.Mode().Perm(), "cross_uid=%q", tc.flag)
		})
	}
}

// TestSidecarBootSecrets_MaterializeFailure_Propagates: a structurally
// malformed batch (API-server bug class) makes materialize exit 2 —
// the sidecar must propagate it so kubelet surfaces CrashLoopBackOff
// with a reason instead of a never-Ready zombie.
func TestSidecarBootSecrets_MaterializeFailure_Propagates(t *testing.T) {
	dir := t.TempDir()
	setSidecarMaterializeEnv(t, dir)

	var hits atomic.Int32
	srv := bootstrapAPIServer(t, &hits) // must NOT be hit — guard skips

	secretsOut := filepath.Join(dir, "rt", "secrets.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(secretsOut), 0o755))
	require.NoError(t, os.WriteFile(secretsOut, []byte(`[42]`), 0o600)) // non-object element

	exit := runSidecarBootSecrets(sidecarBootOpts{
		WorkspaceID: "ws-test",
		APIURL:      srv.URL,
		TokenFile:   filepath.Join(dir, "token"),
		SecretsOut:  secretsOut,
		Stderr:      io.Discard,
	})
	assert.Equal(t, 2, exit, "materialize's structural-failure exit code must propagate (fail fast)")
	require.Zero(t, hits.Load(), "existing batch means no refetch even on the failure path")
}
