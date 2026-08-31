// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// spawn_files_pull.go — design 0057 R2b (#1165): the file-class spawn-time
// pull, both sides of the wire (endpoint + supervisor client).
//
// The materializer STAGES file-class credentials (ssh keys + config,
// git-credentials, secret-files) as canonical bytes + a typed manifest in
// a staging tree it owns; the endpoint serves them over the same §D1 seam
// as /v1/spawn-env. The uid-1000 supervisor pulls the manifest at every
// spawn and writes the delivered files itself — ownership by
// construction: OpenSSH's ownership check on ~/.ssh/config (and every
// other consumer's secure-file semantics) is satisfied because the
// consuming uid is the writing uid.
//
// Absent staging = the quiet empty manifest ("no file-class secrets
// bound"); corrupt staging = 500 (single-writer contract — surface, never
// serve a partial manifest).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
)

// Machine-readable file-delivery degrade reason codes (design 0057 I10):
// plumbed supervisor → control socket → healthz → CRD alongside the env
// family. A silent file-delivery degrade is a review-failing defect.
const (
	spawnFilesReasonUnavailable  = "spawn_files_unavailable"
	spawnFilesReasonUnauthorized = "spawn_files_unauthorized"
	spawnFilesReasonNoCredential = "spawn_files_no_credential"
	spawnFilesReasonBadResponse  = "spawn_files_bad_response"
	spawnFilesReasonBadPath      = "spawn_files_bad_path"
)

const (
	spawnFilesURLPath     = "/v1/spawn-files"
	stagingDirEnvOverride = "LLMSAFESPACES_STAGED_FILES_DIR"
)

// spawnFileEntry is one delivered file in the /v1/spawn-files wire shape:
// the absolute target (validated against the delivery roots by the
// supervisor), the consumer-contract mode, and the canonical bytes.
type spawnFileEntry struct {
	Path    string `json:"path"`
	Mode    int    `json:"mode"`
	Content []byte `json:"content"`
}

// spawnFilesResponse carries the manifest plus the staging-side revision.
// The supervisor derives files_rev from the files IT applied — never from
// this Rev field (terminal verification, design 0057 I4, same doctrine as
// spawned_rev).
type spawnFilesResponse struct {
	Files []spawnFileEntry `json:"files"`
	Rev   string           `json:"rev"`
}

// spawnFilesRev derives the terminal revision over a delivered file set:
// hex SHA-256 over the canonical serialization (sorted by path:
// `path\x00mode\x00content` joined by `\n`). Order, timestamps, and
// replica identity never affect it.
func spawnFilesRev(files []spawnFileEntry) string {
	sorted := make([]spawnFileEntry, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	for i, f := range sorted {
		if i > 0 {
			h.Write([]byte("\n"))
		}
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00", f.Path, f.Mode)
		h.Write(f.Content)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// stagingDirPath resolves the staging tree the endpoint serves. Production
// keeps the fixed /sandbox-runtime/staged-secret-files; the env override is
// the exec-level test seam.
func stagingDirPath() string {
	if d := os.Getenv(stagingDirEnvOverride); d != "" {
		return d
	}
	return "/sandbox-runtime/staged-secret-files"
}

// spawnFilesHandler serves GET /v1/spawn-files on the user mux: the
// current staging manifest with contents inlined. Auth is the §D1 Basic
// pair — identical gate to /v1/spawn-env and reload-secrets.
func spawnFilesHandler(password, controlPlanePassword, stagingDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkBasicAuthAny(r, controlPlanePassword, password) {
			rejectUnauthorized(w)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		resp, ok := loadStagedFiles(stagingDir)
		if !ok {
			// Single writer: an unreadable manifest or a staged file the
			// manifest references but cannot be read is corruption —
			// loud 500, never a partial manifest (I10).
			http.Error(w, "staged files unreadable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// loadStagedFiles reads the staging tree fresh at request time. Absent
// tree/manifest is the quiet empty manifest (law 5); any manifest row
// whose staged bytes cannot be read is corruption (false). The served
// rev is revision-anchored ("<seq>:<manifestHash>:<contentHash>") when
// the published manifest carries the US-70.2 rev anchor, else today's
// bare content hash.
func loadStagedFiles(stagingDir string) (spawnFilesResponse, bool) {
	empty := spawnFilesResponse{Files: []spawnFileEntry{}, Rev: spawnFilesRev(nil)}
	manifestPath := filepath.Join(stagingDir, secrets.ManifestName)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return empty, true
		}
		return spawnFilesResponse{}, false
	}
	staged, revAnchor, err := secrets.ReadStagingManifest(data)
	if err != nil {
		return spawnFilesResponse{}, false
	}
	if staged == nil {
		return empty, true
	}
	files := make([]spawnFileEntry, 0, len(staged))
	for _, e := range staged {
		content, err := os.ReadFile(filepath.Join(stagingDir, filepath.FromSlash(e.File)))
		if err != nil {
			return spawnFilesResponse{}, false
		}
		files = append(files, spawnFileEntry{Path: e.Target, Mode: e.Mode, Content: content})
	}
	return spawnFilesResponse{Files: files, Rev: anchoredSpawnRev(revAnchor, spawnFilesRev(files))}, true
}

// errPullUnauthorized is the shared transport-level sentinel: 401 is
// deterministic per use-site reason code and never retried.
var errPullUnauthorized = errors.New("pull: 401 unauthorized")

// newSpawnFilesPuller builds the supervisor-side client for the file
// manifest: same bounded-wait machinery as the env puller, its own path
// and reason-code family.
func newSpawnFilesPuller(addr, password string) *spawnEnvPuller {
	return &spawnEnvPuller{
		url:      "http://" + addr + spawnFilesURLPath,
		username: agentd.AuthUsername,
		password: password,
		client:   &http.Client{},
		bound:    spawnEnvPullBound,
		attempt:  spawnEnvPullAttempt,
		retryGap: spawnEnvPullRetryGap,
	}
}

// pullFilesBounded fetches the file manifest within the bounded wait,
// reporting the file-family reason codes. A fully-read malformed body is
// permanent (spawn_files_bad_response); 401/missing credential are
// immediate; transport faults retry until the bound.
func (p *spawnEnvPuller) pullFilesBounded(ctx context.Context) (spawnFilesResponse, string, error) {
	if p.password == "" {
		return spawnFilesResponse{}, spawnFilesReasonNoCredential,
			fmt.Errorf("spawn-files pull: %s env unset in the supervisor", supervisorCredentialEnv)
	}
	deadline := time.Now().Add(p.bound)
	for {
		res, reason, err := p.attemptFilesOnce(ctx)
		if err == nil {
			return res, "", nil
		}
		if reason != "" {
			return spawnFilesResponse{}, reason, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return spawnFilesResponse{}, spawnFilesReasonUnavailable, err
		}
		gap := p.retryGap
		if gap > remaining {
			gap = remaining
		}
		select {
		case <-ctx.Done():
			return spawnFilesResponse{}, spawnFilesReasonUnavailable,
				fmt.Errorf("spawn-files pull aborted: %w", ctx.Err())
		case <-time.After(gap):
		}
	}
}

// attemptFilesOnce performs one bounded attempt; reason != "" marks a
// permanent failure (no retry).
func (p *spawnEnvPuller) attemptFilesOnce(ctx context.Context) (spawnFilesResponse, string, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, p.attempt)
	defer cancel()
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, p.url, nil)
	if err != nil {
		return spawnFilesResponse{}, "", err
	}
	req.SetBasicAuth(p.username, p.password)
	resp, err := p.client.Do(req)
	if err != nil {
		return spawnFilesResponse{}, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		body, err := io.ReadAll(io.LimitReader(resp.Body, agentd.StagedFilesMaxBytes))
		if err != nil {
			return spawnFilesResponse{}, "", err
		}
		var out spawnFilesResponse
		if err := json.Unmarshal(body, &out); err != nil {
			return spawnFilesResponse{}, spawnFilesReasonBadResponse,
				fmt.Errorf("spawn-files pull decode: %w", err)
		}
		if out.Files == nil {
			out.Files = []spawnFileEntry{}
		}
		return out, "", nil
	case http.StatusUnauthorized:
		return spawnFilesResponse{}, spawnFilesReasonUnauthorized,
			fmt.Errorf("spawn-files pull %s: %w", p.url, errPullUnauthorized)
	default:
		return spawnFilesResponse{}, "",
			fmt.Errorf("spawn-files pull %s: status %d", p.url, resp.StatusCode)
	}
}
