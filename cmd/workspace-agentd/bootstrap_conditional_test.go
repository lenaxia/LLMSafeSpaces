// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// bootstrap_conditional_test.go — US-70.2 Part 2: the bootstrap
// subcommand's conditional-pull client. Contract: the request always
// negotiates contractVersion 2 and presents the prior envelope's
// manifestHash when one exists on disk; 304 keeps the prior file
// untouched; 200 writes the payload verbatim (envelope or legacy);
// a failed pull keeps the last-good file when one exists (the
// resume-within-pod doctrine) and writes an empty batch otherwise.

import (
	"encoding/json"
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

const testEnvelope = `{"entries":[{"secretId":"sec-1","version":3,"type":"env-secret","name":"db","value":"v1","metadata":{"var_name":"DB"}}],"revision":{"seq":5,"manifestHash":"mh-5","batchHash":"bh-5"}}`

// conditionalServer records the last request body it served and replies
// with the configured status/payload.
type conditionalServer struct {
	status  int
	payload string
	etag    string
	lastReq atomic.Value // bootstrapRequest
}

func (s *conditionalServer) handler(t *testing.T, hits *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		var req bootstrapRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		s.lastReq.Store(req)
		if s.etag != "" {
			w.Header().Set("ETag", s.etag)
		}
		if s.payload != "" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(s.payload))
			return
		}
		w.WriteHeader(s.status)
	}
}

func lastBootstrapRequest(t *testing.T, srv *conditionalServer) bootstrapRequest {
	t.Helper()
	v, ok := srv.lastReq.Load().(bootstrapRequest)
	require.True(t, ok, "server must have observed a request")
	return v
}

// TestBootstrap_NegotiatesContractV2_AlwaysSent: every request carries
// contractVersion 2, even on first boot with no prior file.
func TestBootstrap_NegotiatesContractV2_AlwaysSent(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "secrets.json")
	token := writeBootstrapToken(t, dir)

	srv := &conditionalServer{payload: `{"secrets":[]}`}
	s := httptest.NewServer(srv.handler(t, nil))
	defer s.Close()

	require.Equal(t, 0, runBootstrap([]string{
		"--workspace-id", "ws", "--api-url", s.URL, "--token-file", token, "--out", out,
	}))

	req := lastBootstrapRequest(t, srv)
	assert.Equal(t, 2, req.ContractVersion)
	assert.Empty(t, req.ClientManifestHash, "no prior envelope ⇒ no client hash")
}

// TestBootstrap_PresentsPriorEnvelopeManifestHash: a prior envelope on
// disk is parsed and its revision's manifestHash presented.
func TestBootstrap_PresentsPriorEnvelopeManifestHash(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(out, []byte(testEnvelope), 0o600))
	token := writeBootstrapToken(t, dir)

	srv := &conditionalServer{payload: `{"secrets":[]}`}
	s := httptest.NewServer(srv.handler(t, nil))
	defer s.Close()

	require.Equal(t, 0, runBootstrap([]string{
		"--workspace-id", "ws", "--api-url", s.URL, "--token-file", token, "--out", out,
	}))

	req := lastBootstrapRequest(t, srv)
	assert.Equal(t, 2, req.ContractVersion)
	assert.Equal(t, "mh-5", req.ClientManifestHash)
}

// TestBootstrap_PriorLegacyFile_NoClientHash: a legacy bare-array prior
// file carries no revision, so nothing is presented (the server's 200
// upgrades the file to an envelope).
func TestBootstrap_PriorLegacyFile_NoClientHash(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(out, []byte(`[{"type":"env-secret","name":"e","plaintext":"p"}]`), 0o600))
	token := writeBootstrapToken(t, dir)

	srv := &conditionalServer{payload: `{"secrets":[]}`}
	s := httptest.NewServer(srv.handler(t, nil))
	defer s.Close()

	require.Equal(t, 0, runBootstrap([]string{
		"--workspace-id", "ws", "--api-url", s.URL, "--token-file", token, "--out", out,
	}))
	assert.Empty(t, lastBootstrapRequest(t, srv).ClientManifestHash)
}

// TestBootstrap_304_KeepsFileAndSkipsSideWrites: a 304 must leave the
// prior secrets file byte-untouched, write NOTHING else (no
// workspace-config, no admin-prompt, no allowed-dirs), and exit 0 with
// one log line.
func TestBootstrap_304_KeepsFileAndSkipsSideWrites(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "secrets.json")
	promptOut := filepath.Join(dir, "admin-prompt.md")
	dirsOut := filepath.Join(dir, "allowed-dirs.json")
	require.NoError(t, os.WriteFile(out, []byte(testEnvelope), 0o600))
	token := writeBootstrapToken(t, dir)

	srv := &conditionalServer{status: http.StatusNotModified, etag: `"5:mh-5"`}
	s := httptest.NewServer(srv.handler(t, nil))
	defer s.Close()

	var stderr testWriter
	require.Equal(t, 0, runBootstrapCommand([]string{
		"--workspace-id", "ws", "--api-url", s.URL, "--token-file", token, "--out", out,
		"--admin-prompt-out", promptOut, "--allowed-dirs-out", dirsOut,
	}, io.Discard, &stderr))

	got, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, testEnvelope, string(got), "304 keeps the prior envelope byte-for-byte")

	for _, p := range []string{filepath.Join(dir, "workspace-config.json"), promptOut, dirsOut} {
		_, err := os.Stat(p)
		assert.True(t, os.IsNotExist(err), "304 must not write %s", p)
	}
	assert.Contains(t, stderr.String(), "304", "the skip is logged")
}

type testWriter struct{ data []byte }

func (w *testWriter) Write(p []byte) (int, error) {
	w.data = append(w.data, p...)
	return len(p), nil
}
func (w *testWriter) String() string { return string(w.data) }

// TestBootstrap_EnvelopeWrittenVerbatim: a 200 envelope is persisted
// byte-for-byte (it is self-describing — the revision rides inside).
func TestBootstrap_EnvelopeWrittenVerbatim(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "secrets.json")
	token := writeBootstrapToken(t, dir)

	resp := `{"secrets":` + testEnvelope + `}`
	srv := &conditionalServer{payload: resp}
	s := httptest.NewServer(srv.handler(t, nil))
	defer s.Close()

	require.Equal(t, 0, runBootstrap([]string{
		"--workspace-id", "ws", "--api-url", s.URL, "--token-file", token, "--out", out,
	}))

	got, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, testEnvelope, string(got), "the envelope bytes are written verbatim")
}

// TestBootstrap_LegacyArrayStillWritten: a legacy bare-array response
// (mixed fleet / non-negotiating server) is written as today.
func TestBootstrap_LegacyArrayStillWritten(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "secrets.json")
	token := writeBootstrapToken(t, dir)

	legacy := `[{"type":"env-secret","name":"e","metadata":{"var_name":"V"},"plaintext":"p"}]`
	srv := &conditionalServer{payload: `{"secrets":` + legacy + `}`}
	s := httptest.NewServer(srv.handler(t, nil))
	defer s.Close()

	require.Equal(t, 0, runBootstrap([]string{
		"--workspace-id", "ws", "--api-url", s.URL, "--token-file", token, "--out", out,
	}))

	got, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, legacy, string(got))
}

// TestBootstrap_FailedPull_WithPriorFile_KeepsLastGood: the
// resume-within-pod doctrine — an unreachable API with a prior batch on
// disk must NOT overwrite it with an empty array.
func TestBootstrap_FailedPull_WithPriorFile_KeepsLastGood(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(out, []byte(testEnvelope), 0o600))
	token := writeBootstrapToken(t, dir)

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()

	var stderr testWriter
	require.Equal(t, 0, runBootstrapCommand([]string{
		"--workspace-id", "ws", "--api-url", closed.URL, "--token-file", token, "--out", out,
	}, io.Discard, &stderr))

	got, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, testEnvelope, string(got), "the prior batch is the last-good state")
	assert.Contains(t, stderr.String(), "last-good", "the keep is logged")
}

// TestBootstrap_FailedPull_WithLegacyPriorFile_KeepsLastGood: same
// doctrine for a legacy-shaped prior file.
func TestBootstrap_FailedPull_WithLegacyPriorFile_KeepsLastGood(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "secrets.json")
	legacy := `[{"type":"env-secret","name":"e","plaintext":"p"}]`
	require.NoError(t, os.WriteFile(out, []byte(legacy), 0o600))
	token := writeBootstrapToken(t, dir)

	srv := &conditionalServer{status: http.StatusInternalServerError}
	s := httptest.NewServer(srv.handler(t, nil))
	defer s.Close()

	require.Equal(t, 0, runBootstrap([]string{
		"--workspace-id", "ws", "--api-url", s.URL, "--token-file", token, "--out", out,
	}))

	got, _ := os.ReadFile(out)
	assert.Equal(t, legacy, string(got))
}

// TestBootstrap_EnvelopeWithEmptyEntries_IsEnvelope: an empty envelope
// (`{"entries":[],"revision":{...}}`) must be detected as an envelope
// and written verbatim — NOT treated as a malformed legacy payload.
func TestBootstrap_EnvelopeWithEmptyEntries_IsEnvelope(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "secrets.json")
	token := writeBootstrapToken(t, dir)

	emptyEnvelope := `{"entries":[],"revision":{"seq":2,"manifestHash":"mh","batchHash":"bh"}}`
	srv := &conditionalServer{payload: `{"secrets":` + emptyEnvelope + `}`}
	s := httptest.NewServer(srv.handler(t, nil))
	defer s.Close()

	require.Equal(t, 0, runBootstrap([]string{
		"--workspace-id", "ws", "--api-url", s.URL, "--token-file", token, "--out", out,
	}))

	got, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, emptyEnvelope, string(got))
}
