// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// spawn_files_pull_test.go — R2b (#1165) unit coverage for the
// /v1/spawn-files endpoint and its supervisor-side puller: the §D1 gate,
// the quiet-empty vs loud-corrupt manifest doctrine, rev determinism, and
// the bounded-wait reason-code family.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
	"github.com/stretchr/testify/require"
)

func writeStaging(t *testing.T, stagingDir string, entries []secrets.StagedEntry, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(stagingDir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o700))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}
	if entries != nil {
		data, err := json.Marshal(entries)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(stagingDir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(stagingDir, secrets.ManifestName), data, 0o600))
	}
}

func TestSpawnFilesHandler_ServesManifestWithRev(t *testing.T) {
	staging := t.TempDir()
	writeStaging(t, staging,
		[]secrets.StagedEntry{
			{Target: "/rt/ssh/id_ed25519_x", Mode: 0o600, File: "ssh/id_ed25519_x"},
			{Target: "/rt/git-credentials", Mode: 0o600, File: "git-credentials"},
		},
		map[string]string{"ssh/id_ed25519_x": "KEY", "git-credentials": "https://oauth2:t@github.com\n"})

	srv := httptest.NewServer(spawnFilesHandler("pw", "", staging))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/spawn-files", nil)
	req.SetBasicAuth("opencode", "pw")
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var got spawnFilesResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Len(t, got.Files, 2)
	byPath := map[string]spawnFileEntry{}
	for _, f := range got.Files {
		byPath[f.Path] = f
	}
	require.Equal(t, "KEY", string(byPath["/rt/ssh/id_ed25519_x"].Content))
	require.Equal(t, 0o600, byPath["/rt/ssh/id_ed25519_x"].Mode)
	require.Equal(t, spawnFilesRev(got.Files), got.Rev,
		"the advertised rev is the deterministic digest of the served set")
}

func TestSpawnFilesHandler_AuthAndMethod(t *testing.T) {
	srv := httptest.NewServer(spawnFilesHandler("pw", "", t.TempDir()))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/spawn-files", nil)
	req.SetBasicAuth("opencode", "wrong")
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "wrong credential is a 401")

	req2, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/spawn-files", nil)
	req2.SetBasicAuth("opencode", "pw")
	resp2, err := srv.Client().Do(req2)
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	require.Equal(t, http.StatusMethodNotAllowed, resp2.StatusCode)
}

func TestSpawnFilesHandler_AbsentStagingIsQuietEmpty(t *testing.T) {
	srv := httptest.NewServer(spawnFilesHandler("pw", "", filepath.Join(t.TempDir(), "absent")))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/spawn-files", nil)
	req.SetBasicAuth("opencode", "pw")
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var got spawnFilesResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Empty(t, got.Files, "absent staging is the quiet 'no file-class secrets bound' state")
	require.Equal(t, spawnFilesRev(nil), got.Rev)
}

func TestSpawnFilesHandler_CorruptStagingIsLoud(t *testing.T) {
	for name, tc := range map[string]func(t *testing.T, staging string){
		"manifest not json": func(t *testing.T, staging string) {
			require.NoError(t, os.WriteFile(filepath.Join(staging, secrets.ManifestName), []byte("not-json"), 0o600))
		},
		"staged file missing": func(t *testing.T, staging string) {
			writeStaging(t, staging, []secrets.StagedEntry{{Target: "/rt/x", Mode: 0o600, File: "x"}}, nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			staging := t.TempDir()
			require.NoError(t, os.MkdirAll(staging, 0o700))
			tc(t, staging)
			srv := httptest.NewServer(spawnFilesHandler("pw", "", staging))
			t.Cleanup(srv.Close)

			req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/spawn-files", nil)
			req.SetBasicAuth("opencode", "pw")
			resp, err := srv.Client().Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			require.Equal(t, http.StatusInternalServerError, resp.StatusCode,
				"single-writer corruption is surfaced, never served partially")
		})
	}
}

func TestSpawnFilesRev_DeterministicRegardlessOfOrder(t *testing.T) {
	a := []spawnFileEntry{{Path: "/b", Mode: 0o600, Content: []byte("2")}, {Path: "/a", Mode: 0o600, Content: []byte("1")}}
	b := []spawnFileEntry{{Path: "/a", Mode: 0o600, Content: []byte("1")}, {Path: "/b", Mode: 0o600, Content: []byte("2")}}
	require.Equal(t, spawnFilesRev(a), spawnFilesRev(b))
	require.NotEqual(t, spawnFilesRev(a), spawnFilesRev([]spawnFileEntry{{Path: "/a", Mode: 0o640, Content: []byte("1")}, {Path: "/b", Mode: 0o600, Content: []byte("2")}}),
		"the mode is part of the contract and of the rev")
}

func TestSpawnFilesPuller_ReasonCodes(t *testing.T) {
	t.Run("no credential", func(t *testing.T) {
		p := newSpawnFilesPuller("127.0.0.1:1", "")
		_, reason, err := p.pullFilesBounded(context.Background())
		require.Error(t, err)
		require.Equal(t, spawnFilesReasonNoCredential, reason)
	})

	t.Run("unauthorized is immediate", func(t *testing.T) {
		srv := httptest.NewServer(spawnFilesHandler("pw", "", t.TempDir()))
		t.Cleanup(srv.Close)
		p := newSpawnFilesPuller(stripHTTP(srv.URL), "wrong")
		start := time.Now()
		_, reason, err := p.pullFilesBounded(context.Background())
		require.Error(t, err)
		require.Equal(t, spawnFilesReasonUnauthorized, reason)
		require.Less(t, time.Since(start), time.Second, "401 must not burn the bound retrying")

	})

	t.Run("malformed body is permanent", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("not-json"))
		}))
		t.Cleanup(srv.Close)
		p := newSpawnFilesPuller(stripHTTP(srv.URL), "pw")
		_, reason, err := p.pullFilesBounded(context.Background())
		require.Error(t, err)
		require.Equal(t, spawnFilesReasonBadResponse, reason)
	})

	t.Run("unreachable expires bounded", func(t *testing.T) {
		p := newSpawnFilesPuller("127.0.0.1:1", "pw")
		p.bound = 300 * time.Millisecond
		start := time.Now()
		_, reason, err := p.pullFilesBounded(context.Background())
		require.Error(t, err)
		require.Equal(t, spawnFilesReasonUnavailable, reason)
		require.GreaterOrEqual(t, time.Since(start), 250*time.Millisecond)
		require.Less(t, time.Since(start), 3*time.Second)
	})
}

// stripHTTP converts an httptest server URL (http://host:port) to the
// host:port form the pullers take.
func stripHTTP(url string) string {
	const prefix = "http://"
	if len(url) > len(prefix) && url[:len(prefix)] == prefix {
		return url[len(prefix):]
	}
	return url
}
