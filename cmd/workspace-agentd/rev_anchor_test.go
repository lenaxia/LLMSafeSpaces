// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// rev_anchor_test.go — US-70.2 Part 2: revision anchoring on the pod.
//
// The materialize path persists the applied batch's revision where the
// spawn seams read state:
//   - <secrets-env>.rev — {"rev":"<seq>:<manifestHash>","appliedSeq":N}
//     (written when a revision is known, removed when not)
//   - the staged manifest's additive "rev" field (tested in
//     pkg/agentd/secrets/staging_rev_test.go)
//
// The appliedSeq member is the W2 apply-guard: a PULLED batch whose seq
// is ≤ appliedSeq is skipped (idempotent-equal no-op, stale skipped);
// legacy/push batches bypass the guard and invalidate the anchor.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
)

// runMaterializeWithEnvelope materializes a --from file in a temp tree
// and returns the materialize exit code.
func runMaterializeWithEnvelope(t *testing.T, batchFileBody string) (int, string) {
	t.Helper()
	dir := t.TempDir()
	setSidecarMaterializeEnv(t, dir)
	from := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(from, []byte(batchFileBody), 0o600))
	code := runMaterializeCommand([]string{"--from", from}, nil, io.Discard)
	return code, dir
}

func TestMaterialize_Envelope_WritesRevAnchor(t *testing.T) {
	code, dir := runMaterializeWithEnvelope(t, `{"entries":[{"secretId":"s1","version":1,"type":"env-secret","name":"db","value":"v1","metadata":{"var_name":"DB"}}],"revision":{"seq":4,"manifestHash":"mh-4","batchHash":"bh"}}`)
	require.Equal(t, 0, code)

	anchor, err := readRevAnchor(revAnchorPath(filepath.Join(dir, "secrets-env")))
	require.NoError(t, err)
	assert.Equal(t, "4:mh-4", anchor.Rev)
	assert.EqualValues(t, 4, anchor.AppliedSeq, "a successful apply records its seq")
}

func TestMaterialize_LegacyBatch_NoAnchorFile(t *testing.T) {
	code, dir := runMaterializeWithEnvelope(t, `[{"type":"env-secret","name":"db","metadata":{"var_name":"DB"},"plaintext":"v1"}]`)
	require.Equal(t, 0, code)

	_, err := os.Stat(revAnchorPath(filepath.Join(dir, "secrets-env")))
	assert.True(t, os.IsNotExist(err), "legacy batches write no anchor (byte-compat)")
}

func TestMaterialize_LegacyBatch_RemovesStaleAnchor(t *testing.T) {
	dir := t.TempDir()
	setSidecarMaterializeEnv(t, dir)
	from := filepath.Join(dir, "secrets.json")
	envPath := filepath.Join(dir, "secrets-env")
	require.NoError(t, os.WriteFile(from, []byte(`[{"type":"env-secret","name":"db","metadata":{"var_name":"DB"},"plaintext":"v1"}]`), 0o600))
	require.NoError(t, writeRevAnchor(revAnchorPath(envPath), revAnchor{Rev: "9:mh", AppliedSeq: 9}))

	require.Equal(t, 0, runMaterializeCommand([]string{"--from", from}, nil, io.Discard))

	_, err := os.Stat(revAnchorPath(envPath))
	assert.True(t, os.IsNotExist(err), "a legacy/push apply invalidates the marker back to unknown")
}

// TestMaterialize_ApplyGuard_EqualSeq_NoOp: re-materializing the SAME
// pulled revision is a logged no-op — the tmpfs state from the prior
// apply is already the exact intended set.
func TestMaterialize_ApplyGuard_EqualSeq_NoOp(t *testing.T) {
	dir := t.TempDir()
	setSidecarMaterializeEnv(t, dir)
	from := filepath.Join(dir, "secrets.json")
	envPath := filepath.Join(dir, "secrets-env")
	envelope := `{"entries":[{"secretId":"s1","version":1,"type":"env-secret","name":"db","value":"v1","metadata":{"var_name":"DB"}}],"revision":{"seq":4,"manifestHash":"mh-4","batchHash":"bh"}}`
	require.NoError(t, os.WriteFile(from, []byte(envelope), 0o600))

	require.Equal(t, 0, runMaterializeCommand([]string{"--from", from}, nil, io.Discard))
	_, err := os.ReadFile(envPath)
	require.NoError(t, err, "the first apply writes the env file")

	// Sabotage the materialized state: a guard-less re-run would rewrite it.
	require.NoError(t, os.Remove(envPath))

	require.Equal(t, 0, runMaterializeCommand([]string{"--from", from}, nil, io.Discard))
	_, err = os.Stat(envPath)
	assert.True(t, os.IsNotExist(err), "equal seq is a no-op — the sabotaged file is NOT rewritten")
}

// TestMaterialize_ApplyGuard_LowerSeq_SkippedAsStale: an out-of-order
// older batch (stale replica race, W2) must not regress applied state.
func TestMaterialize_ApplyGuard_LowerSeq_SkippedAsStale(t *testing.T) {
	dir := t.TempDir()
	setSidecarMaterializeEnv(t, dir)
	from := filepath.Join(dir, "secrets.json")
	envPath := filepath.Join(dir, "secrets-env")
	require.NoError(t, writeRevAnchor(revAnchorPath(envPath), revAnchor{Rev: "7:mh-7", AppliedSeq: 7}))

	stale := `{"entries":[{"secretId":"s1","version":1,"type":"env-secret","name":"db","value":"old","metadata":{"var_name":"DB"}}],"revision":{"seq":6,"manifestHash":"mh-6","batchHash":"bh"}}`
	require.NoError(t, os.WriteFile(from, []byte(stale), 0o600))

	require.Equal(t, 0, runMaterializeCommand([]string{"--from", from}, nil, io.Discard))
	_, err := os.Stat(envPath)
	assert.True(t, os.IsNotExist(err), "a lower seq is skipped as stale — no materialization happens")
}

// TestMaterialize_ApplyGuard_HigherSeq_Applies: a newer pulled revision
// passes the guard and updates the anchor.
func TestMaterialize_ApplyGuard_HigherSeq_Applies(t *testing.T) {
	dir := t.TempDir()
	setSidecarMaterializeEnv(t, dir)
	from := filepath.Join(dir, "secrets.json")
	envPath := filepath.Join(dir, "secrets-env")
	require.NoError(t, writeRevAnchor(revAnchorPath(envPath), revAnchor{Rev: "7:mh-7", AppliedSeq: 7}))

	newer := `{"entries":[{"secretId":"s1","version":2,"type":"env-secret","name":"db","value":"new","metadata":{"var_name":"DB"}}],"revision":{"seq":8,"manifestHash":"mh-8","batchHash":"bh"}}`
	require.NoError(t, os.WriteFile(from, []byte(newer), 0o600))

	require.Equal(t, 0, runMaterializeCommand([]string{"--from", from}, nil, io.Discard))
	env, err := os.ReadFile(envPath)
	require.NoError(t, err)
	assert.Contains(t, string(env), "DB")

	anchor, err := readRevAnchor(revAnchorPath(envPath))
	require.NoError(t, err)
	assert.Equal(t, "8:mh-8", anchor.Rev)
	assert.EqualValues(t, 8, anchor.AppliedSeq)
}

// --- spawn seams: served rev composition ----------------------------------

// TestSpawnEnvHandler_AnchoredRevWhenAnchorPresent: with a
// <secrets-env>.rev anchor the served rev is
// "<seq>:<manifestHash>:<contentHash>"; without it, today's bare
// contentHash.
func TestSpawnEnvHandler_AnchoredRevWhenAnchorPresent(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "secrets-env")
	require.NoError(t, os.WriteFile(envPath, []byte("export DB='v'\n"), 0o600))

	handler := spawnEnvHandler("pw", "", envPath)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/spawn-env", nil)
	req.SetBasicAuth("opencode", "pw")
	handler(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	var plain struct {
		Env map[string]string `json:"env"`
		Rev string            `json:"rev"`
	}
	require.NoError(t, jsonDecode(rr.Body.Bytes(), &plain))
	assert.NotContains(t, plain.Rev, ":", "no anchor ⇒ bare content hash")

	require.NoError(t, writeRevAnchor(revAnchorPath(envPath), revAnchor{Rev: "4:mh-4", AppliedSeq: 4}))

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/spawn-env", nil)
	req.SetBasicAuth("opencode", "pw")
	handler(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	var anchored struct {
		Env map[string]string `json:"env"`
		Rev string            `json:"rev"`
	}
	require.NoError(t, jsonDecode(rr.Body.Bytes(), &anchored))
	assert.Equal(t, "4:mh-4:"+plain.Rev, anchored.Rev,
		"anchored rev = <seq>:<manifestHash>:<self-computed contentHash>")
	assert.Equal(t, plain.Env, anchored.Env, "the delta itself is unchanged by anchoring")
}

// TestSpawnFilesHandler_AnchoredRevWhenManifestCarriesRev: the staged
// manifest's rev field anchors the files rev the same way.
func TestSpawnFilesHandler_AnchoredRevWhenManifestCarriesRev(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, "staged")
	require.NoError(t, os.MkdirAll(staging, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(staging, secrets.ManifestName),
		[]byte(`{"rev":"6:mh-6","entries":[{"target":"/t/k","mode":384,"file":"k"}]}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "k"), []byte("bytes"), 0o600))

	handler := spawnFilesHandler("pw", "", staging)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/spawn-files", nil)
	req.SetBasicAuth("opencode", "pw")
	handler(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	var resp spawnFilesResponse
	require.NoError(t, jsonDecode(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Files, 1)
	assert.Equal(t, "6:mh-6:"+spawnFilesRev(resp.Files), resp.Rev)
}

func jsonDecode(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// --- supervisor: spawned_rev / files_rev carry the anchored form ----------

// serveAnchoredSpawnEnv serves a delta with an anchored served rev whose
// content-hash component is deliberately WRONG — the I4 pin: the
// supervisor keeps the anchored prefix but re-derives the content hash
// from the delta IT applied.
func serveAnchoredSpawnEnv(t *testing.T, password string, delta map[string]string, anchoredRev string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/spawn-env", func(w http.ResponseWriter, r *http.Request) {
		if !checkBasicAuthAny(r, "", password) {
			rejectUnauthorized(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(spawnEnvResponse{Env: delta, Rev: anchoredRev})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestSpawnedRev_AnchoredWhenServedRevAnchored(t *testing.T) {
	delta := map[string]string{"PULLED": "fresh"}
	srv := serveAnchoredSpawnEnv(t, "pw", delta, "4:mh-4:deadbeef")
	p := newSpawnEnvPuller(hostOf(t, srv), "pw")
	p.bound, p.attempt, p.retryGap = fastTiming()
	a := &managedProcAdapter{
		baseCmdFactory: mkFactoryEnv(staticEnv("PATH=/bin")),
		puller:         p,
		pullCtx:        context.Background(),
	}

	a.preSpawn()
	_ = a.composeChild()

	st := a.SpawnEnvState()
	want := "4:mh-4:" + spawnDeltaRev(delta)
	assert.Equal(t, want, st.SpawnedRev,
		"anchored prefix from the served rev + SELF-computed content hash (I4: the served content hash is never trusted)")
}

func TestSpawnedRev_BareWhenServedRevLegacy(t *testing.T) {
	delta := map[string]string{"PULLED": "fresh"}
	srv := serveAnchoredSpawnEnv(t, "pw", delta, spawnDeltaRev(delta))
	p := newSpawnEnvPuller(hostOf(t, srv), "pw")
	p.bound, p.attempt, p.retryGap = fastTiming()
	a := &managedProcAdapter{
		baseCmdFactory: mkFactoryEnv(staticEnv("PATH=/bin")),
		puller:         p,
		pullCtx:        context.Background(),
	}

	a.preSpawn()
	_ = a.composeChild()

	st := a.SpawnEnvState()
	assert.Equal(t, spawnDeltaRev(delta), st.SpawnedRev,
		"a legacy (content-hash-only) served rev passes through unchanged")
}

// TestFilesRev_AnchoredWhenServedRevAnchored: same composition on the
// file-class side — the pull's served rev anchors, the local apply's
// terminal rev terminates.
func TestFilesRev_AnchoredWhenServedRevAnchored(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "k")
	files := []spawnFileEntry{{Path: target, Mode: 0o600, Content: []byte("bytes")}}

	servedRev := "6:mh-6:deadbeef"
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/spawn-files", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(spawnFilesResponse{Files: files, Rev: servedRev})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	p := newSpawnFilesPuller(hostOf(t, srv), "pw")
	p.bound, p.attempt, p.retryGap = fastTiming()
	a := &managedProcAdapter{
		filesPuller: p,
		pullCtx:     context.Background(),
		delivery:    fileDelivery{roots: []string{dir}, ledgerPath: filepath.Join(dir, "ledger.json"), sshDir: dir},
	}

	a.preSpawn()

	st := a.SpawnEnvState()
	want := "6:mh-6:" + spawnFilesRev(files)
	assert.Equal(t, want, st.FilesRev,
		"anchored prefix + the terminal rev over the files THIS uid applied")
}
