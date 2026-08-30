// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// spawn_env_pull_test.go — US-70.1 (design 0057 R2) unit tests: the
// canonical delta revision, the /v1/spawn-env pull endpoint (auth,
// response shape, corruption posture), and the supervisor's bounded-wait
// puller (reason-code classification, retry-within-bound, permanent
// failures returning without burning the bound).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/stretchr/testify/require"
)

func TestSpawnDeltaRev_DeterministicAndOrderIndependent(t *testing.T) {
	a := map[string]string{"B": "2", "A": "1"}
	b := map[string]string{"A": "1", "B": "2"}
	require.Equal(t, spawnDeltaRev(a), spawnDeltaRev(b), "map iteration order must not affect the revision")
}

func TestSpawnDeltaRev_ValueSensitivity(t *testing.T) {
	require.NotEqual(t, spawnDeltaRev(map[string]string{"K": "1"}), spawnDeltaRev(map[string]string{"K": "2"}))
	require.NotEqual(t, spawnDeltaRev(map[string]string{"K": "1"}), spawnDeltaRev(map[string]string{"J": "1"}))
}

func TestSpawnDeltaRev_EmptyIsStable(t *testing.T) {
	require.Equal(t, spawnDeltaRev(map[string]string{}), spawnDeltaRev(nil))
	require.NotEmpty(t, spawnDeltaRev(map[string]string{}))
}

func writeSpawnEnvFile(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "secrets-env")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestSpawnEnvHandler_AuthMatrix(t *testing.T) {
	dir := t.TempDir()
	path := writeSpawnEnvFile(t, dir, "export API_TOKEN='sekret'\n")
	h := spawnEnvHandler("workspace-pw", "control-pw", path)

	cases := []struct {
		name string
		user string
		pass string
		code int
	}{
		{"no auth", "", "", http.StatusUnauthorized},
		{"wrong password", "opencode", "nope", http.StatusUnauthorized},
		{"workspace password (the supervisor carve-out credential)", "opencode", "workspace-pw", http.StatusOK},
		{"control-plane password", "opencode", "control-pw", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/spawn-env", nil)
			if tc.pass != "" {
				req.SetBasicAuth(tc.user, tc.pass)
			}
			rec := httptest.NewRecorder()
			h(rec, req)
			require.Equal(t, tc.code, rec.Code)
		})
	}
}

func TestSpawnEnvHandler_ResponseShape(t *testing.T) {
	dir := t.TempDir()
	path := writeSpawnEnvFile(t, dir, "export API_TOKEN='sekret'\nexport MULTI='line1\nline2'\n")
	h := spawnEnvHandler("pw", "", path)

	req := httptest.NewRequest(http.MethodGet, "/v1/spawn-env", nil)
	req.SetBasicAuth("opencode", "pw")
	rec := httptest.NewRecorder()
	h(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got spawnEnvResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, map[string]string{"API_TOKEN": "sekret", "MULTI": "line1\nline2"}, got.Env)
	require.Equal(t, spawnDeltaRev(got.Env), got.Rev, "the endpoint's rev must equal the canonical delta rev")
}

func TestSpawnEnvHandler_AbsentFileIsQuietEmptyDelta(t *testing.T) {
	h := spawnEnvHandler("pw", "", filepath.Join(t.TempDir(), "absent"))

	req := httptest.NewRequest(http.MethodGet, "/v1/spawn-env", nil)
	req.SetBasicAuth("opencode", "pw")
	rec := httptest.NewRecorder()
	h(rec, req)

	// "Owner has no env secrets bound" is the ONLY quiet outcome (law 5).
	require.Equal(t, http.StatusOK, rec.Code)
	var got spawnEnvResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Empty(t, got.Env)
	require.Equal(t, spawnDeltaRev(map[string]string{}), got.Rev)
}

func TestSpawnEnvHandler_CorruptFileIsServerError(t *testing.T) {
	dir := t.TempDir()
	path := writeSpawnEnvFile(t, dir, "this is not the canonical encoder output\n")
	h := spawnEnvHandler("pw", "", path)

	req := httptest.NewRequest(http.MethodGet, "/v1/spawn-env", nil)
	req.SetBasicAuth("opencode", "pw")
	rec := httptest.NewRecorder()
	h(rec, req)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestSpawnEnvHandler_MethodAndAuthOrdering(t *testing.T) {
	h := spawnEnvHandler("pw", "", filepath.Join(t.TempDir(), "absent"))

	req := httptest.NewRequest(http.MethodPost, "/v1/spawn-env", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code, "auth is checked before method — no unauthenticated probing")

	req.SetBasicAuth("opencode", "pw")
	rec = httptest.NewRecorder()
	h(rec, req)
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// --- bounded-wait puller ------------------------------------------------------

func fastPuller(addr, password string) *spawnEnvPuller {
	p := newSpawnEnvPuller(addr, password)
	p.bound = 500 * time.Millisecond
	p.attempt = 100 * time.Millisecond
	p.retryGap = 20 * time.Millisecond
	return p
}

func serveSpawnEnv(t *testing.T, password string, delta map[string]string) *spawnEnvTestServer {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/spawn-env", func(w http.ResponseWriter, r *http.Request) {
		if !checkBasicAuthAny(r, "control-pw", password) {
			rejectUnauthorized(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(spawnEnvResponse{Env: delta, Rev: spawnDeltaRev(delta)})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// serveSkewedSpawnEnv serves a delta whose advertised Rev is
// deliberately wrong — the I4 negative: spawned_rev must derive from the
// applied delta, never from what the server claims.
func serveSkewedSpawnEnv(t *testing.T, password string, delta map[string]string, wrongRev string) *spawnEnvTestServer {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/spawn-env", func(w http.ResponseWriter, r *http.Request) {
		if !checkBasicAuthAny(r, "control-pw", password) {
			rejectUnauthorized(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(spawnEnvResponse{Env: delta, Rev: wrongRev})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestSpawnEnvPuller_Success(t *testing.T) {
	srv := serveSpawnEnv(t, "pw", map[string]string{"A": "1"})
	p := fastPuller(hostOf(t, srv), "pw")

	res, reason, err := p.pullBounded(context.Background())
	require.NoError(t, err)
	require.Empty(t, reason)
	require.Equal(t, map[string]string{"A": "1"}, res.Env)
	require.Equal(t, spawnDeltaRev(map[string]string{"A": "1"}), res.Rev)
}

func TestSpawnEnvPuller_UnauthorizedIsPermanent(t *testing.T) {
	srv := serveSpawnEnv(t, "pw", map[string]string{"A": "1"})
	p := fastPuller(hostOf(t, srv), "wrong")

	start := time.Now()
	_, reason, err := p.pullBounded(context.Background())
	require.Error(t, err)
	require.Equal(t, spawnEnvReasonUnauthorized, reason)
	require.Less(t, time.Since(start), p.bound, "a 401 must not burn the bounded-wait budget")
}

func TestSpawnEnvPuller_NoCredentialNeverDials(t *testing.T) {
	// A port with nothing on it: if the puller dialed, it would burn the
	// bound; the no-credential failure must be immediate.
	p := fastPuller("127.0.0.1:1", "")

	start := time.Now()
	_, reason, err := p.pullBounded(context.Background())
	require.Error(t, err)
	require.Equal(t, spawnEnvReasonNoCredential, reason)
	require.Less(t, time.Since(start), 50*time.Millisecond)
}

func TestSpawnEnvPuller_UnreachableExpiresBounded(t *testing.T) {
	p := fastPuller("127.0.0.1:1", "pw")

	start := time.Now()
	_, reason, err := p.pullBounded(context.Background())
	require.Error(t, err)
	require.Equal(t, spawnEnvReasonUnavailable, reason)
	took := time.Since(start)
	require.GreaterOrEqual(t, took, p.bound, "the pull must keep retrying until the bound")
	require.Less(t, took, p.bound+600*time.Millisecond, "the pull must never exceed the bound materially")
}

func TestSpawnEnvPuller_TransientFailureThenSuccess(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		delta := map[string]string{"LATE": "arrived"}
		_ = json.NewEncoder(w).Encode(spawnEnvResponse{Env: delta, Rev: spawnDeltaRev(delta)})
	}))
	t.Cleanup(srv.Close)

	p := fastPuller(hostOf(t, srv), "pw")
	res, reason, err := p.pullBounded(context.Background())
	require.NoError(t, err)
	require.Empty(t, reason)
	require.Equal(t, map[string]string{"LATE": "arrived"}, res.Env)
	require.Equal(t, 2, calls, "the first 503 must have been retried, not accepted")
}

func TestSpawnEnvPuller_BadResponseBodyIsClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "not-json{")
	}))
	t.Cleanup(srv.Close)

	p := fastPuller(hostOf(t, srv), "pw")
	_, reason, err := p.pullBounded(context.Background())
	require.Error(t, err)
	require.Equal(t, spawnEnvReasonBadResponse, reason)
}

func TestSpawnEnvPuller_OversizedBodyIsBounded(t *testing.T) {
	// The 1MiB body cap turns an oversized response into a decode
	// failure (bad_response), never an unbounded read.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, 2<<20))
	}))
	t.Cleanup(srv.Close)

	p := fastPuller(hostOf(t, srv), "pw")
	_, reason, err := p.pullBounded(context.Background())
	require.Error(t, err)
	require.Equal(t, spawnEnvReasonBadResponse, reason,
		"a truncated-by-cap body must not decode as a valid delta")
}

func TestAdapterConcurrentPushPullCompose(t *testing.T) {
	// Concurrent legacy push (socket goroutine) vs preSpawn/composeChild
	// (supervise goroutine) vs SpawnEnvState (status reader) — the
	// adapter's pullMu is the only shared state; this hammers it under
	// -race.
	a, srv := mkAdapterWithPuller(t, map[string]string{"SEED": "1"}, "pw")
	defer srv.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			a.SetSpawnEnv(map[string]string{"K": "pushed"})
			a.preSpawn()
			_ = a.composeChild()
			_ = a.SpawnEnvState()
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent adapter access deadlocked")
	}
}

func TestSpawnEnvPuller_ContextCancelAbortsBoundedWait(t *testing.T) {
	p := fastPuller("127.0.0.1:1", "pw")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(60 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, _, err := p.pullBounded(ctx)
	require.Error(t, err)
	require.Less(t, time.Since(start), p.bound, "shutdown must abort the bounded wait immediately")
}

func TestSpawnEnvPullAddr_DefaultAndOverride(t *testing.T) {
	require.Equal(t, fmt.Sprintf("127.0.0.1:%d", agentd.AgentdPort), spawnEnvPullAddr())
	t.Setenv("LLMSAFESPACES_SPAWN_ENV_PULL_ADDR", "127.0.0.1:14097")
	require.Equal(t, "127.0.0.1:14097", spawnEnvPullAddr())
}

func hostOf(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	require.NotNil(t, srv)
	return srv.Listener.Addr().String()
}
