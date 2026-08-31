// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// spawn_env_pull.go — US-70.1 (design 0057 R2): the spawn-time env pull,
// both ends of the wire.
//
// The push design this replaces (US-4a pushInitialSpawnEnv) was
// structurally broken under native-sidecar startup gating: the sidecar
// pushed to the supervisor's control socket before its own startup probe
// could pass — and the workspace container only starts after that probe
// passes — so the dial could only ever fail ("connection refused", every
// boot; 2026-08-30 fleet audit: 3/6 pods, suspend/resume re-breaks
// deterministically). Here the direction flips: at every spawn the
// SUPERVISOR pulls the current delta from the sidecar's user mux
// (4097) with a bounded wait. Never-block-boot extends to
// never-block-spawn: a secrets problem never becomes a
// spawn-availability problem.
//
// Security shape (design 0051 §D1): the supervisor is uid-1000 space
// and must never hold agentdPassword or the admin bearer. It presents
// the workspace password (OPENCODE_SERVER_PASSWORD, controller-wired
// into the main container env) — the same §D1 carve-out credential
// class as /v1/mcp and dev-preview. The served delta is exactly the
// materialized secrets-env content, i.e. the values destined for the
// child's environ by design; the control socket's write-only
// spawn_env store (A.4 invariant 1) is untouched.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// Machine-readable degrade reason codes (design 0057 I10/I13): plumbed
// supervisor → control socket → sidecar healthz → CRD status. A silent
// degrade is a review-failing defect.
const (
	spawnEnvReasonUnavailable  = "spawn_env_unavailable"
	spawnEnvReasonUnauthorized = "spawn_env_unauthorized"
	spawnEnvReasonNoCredential = "spawn_env_no_credential"
	spawnEnvReasonBadResponse  = "spawn_env_bad_response"
)

// Bounded-wait defaults: the sidecar's healthy response time is single-
// digit milliseconds; the bound only absorbs sidecar restarts, the
// mux-bind race at first boot, and momentary unavailability. The worst
// case per spawn is bound+attempt (2.5s: a final attempt may start just
// inside the deadline and run its full attempt timeout) — only when the
// sidecar is down.
const (
	spawnEnvPullBound       = 2 * time.Second
	spawnEnvPullAttempt     = 500 * time.Millisecond
	spawnEnvPullRetryGap    = 150 * time.Millisecond
	spawnEnvPullURLPath     = "/v1/spawn-env"
	spawnEnvPullAddrEnvVar  = "LLMSAFESPACES_SPAWN_ENV_PULL_ADDR"
	supervisorCredentialEnv = "OPENCODE_SERVER_PASSWORD"
)

// spawnEnvResponse is the /v1/spawn-env wire shape: the current
// secrets-env delta plus the sidecar's revision over it. The supervisor
// derives spawned_rev from the delta IT applied — never from this Rev
// field (terminal verification, design 0057 I4).
type spawnEnvResponse struct {
	Env map[string]string `json:"env"`
	Rev string            `json:"rev"`
}

// spawnDeltaRev derives the terminal content revision of a spawn delta:
// hex SHA-256 over the canonical serialization (sorted `k=v` lines joined
// by `\n`). Map iteration order, timestamps, and replica identity never
// affect it (design 0057 I6). US-70.2: when the materialized batch
// carried a delivery revision this hash becomes the final component of
// the anchored rev ("<seq>:<manifestHash>:<contentHash>") — the
// self-computed terminal-verification layer over the server's
// revision identity.
func spawnDeltaRev(delta map[string]string) string {
	keys := make([]string, 0, len(delta))
	for k := range delta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(delta[k])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// spawnEnvPullAddr is the sidecar user-mux address the supervisor pulls
// from. Production keeps the fixed in-pod 4097; the env override is the
// exec-level test seam (same pattern as LLMSAFESPACES_CONTROL_SOCKET_ADDR).
func spawnEnvPullAddr() string {
	if a := os.Getenv(spawnEnvPullAddrEnvVar); a != "" {
		return a
	}
	return fmt.Sprintf("127.0.0.1:%d", agentd.AgentdPort)
}

// supervisorSpawnCredential resolves the pull credential from the
// supervisor's own container env. Empty is not fatal — it degrades
// loudly with spawn_env_no_credential (the controller guarantees the
// wiring; absence is a bug state that must surface, not block spawn).
func supervisorSpawnCredential() string {
	return os.Getenv(supervisorCredentialEnv)
}

// spawnEnvHandler serves GET /v1/spawn-env on the user mux (4097): the
// current materialized secrets-env delta and its revision. Auth is the
// §D1 Basic pair (either credential) — identical gate to reload-secrets.
//
// The rev is revision-anchored ("<seq>:<manifestHash>:<contentHash>",
// US-70.2) when the materialized batch carried a delivery revision
// (the <secrets-env>.rev sibling anchor); the content hash stays
// self-computed over the served delta (I4).
func spawnEnvHandler(password, controlPlanePassword, secretsEnvPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkBasicAuthAny(r, controlPlanePassword, password) {
			rejectUnauthorized(w)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		delta, err := parseSecretsEnvDelta(secretsEnvPath)
		if err != nil {
			// Single writer: a malformed file is corruption — surface it
			// as a server error rather than silently serving a partial
			// delta (I10: complete or loudly degraded).
			http.Error(w, "secrets-env unreadable", http.StatusInternalServerError)
			return
		}
		anchor := servedEnvRevAnchor(secretsEnvPath)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(spawnEnvResponse{Env: delta, Rev: anchoredSpawnRev(anchor, spawnDeltaRev(delta))})
	}
}

// servedEnvRevAnchor reads the <secrets-env>.rev sibling (written by the
// materialize path when the applied batch carried a revision). Absent
// or unreadable is the unanchored (legacy) rev — never a failed serve.
func servedEnvRevAnchor(secretsEnvPath string) string {
	anchor, err := readRevAnchor(revAnchorPath(secretsEnvPath))
	if err != nil {
		return ""
	}
	return anchor.Rev
}

// spawnEnvPuller is the supervisor-side client. All fields are mutable
// within the package so tests can shrink the bound; production uses
// newSpawnEnvPuller's defaults.
type spawnEnvPuller struct {
	url      string
	username string
	password string
	client   *http.Client
	bound    time.Duration
	attempt  time.Duration
	retryGap time.Duration
}

func newSpawnEnvPuller(addr, password string) *spawnEnvPuller {
	return &spawnEnvPuller{
		url:      "http://" + addr + spawnEnvPullURLPath,
		username: agentd.AuthUsername,
		password: password,
		client:   &http.Client{},
		bound:    spawnEnvPullBound,
		attempt:  spawnEnvPullAttempt,
		retryGap: spawnEnvPullRetryGap,
	}
}

// pullBounded fetches the delta within the bounded wait. Success returns
// the response; failure returns a machine-readable reason code.
// Permanent failures (unauthorized, no credential) return immediately —
// retrying them only burns spawn latency. Transient failures (network,
// 5xx, malformed body) retry until the bound expires.
func (p *spawnEnvPuller) pullBounded(ctx context.Context) (spawnEnvResponse, string, error) {
	if p.password == "" {
		return spawnEnvResponse{}, spawnEnvReasonNoCredential,
			fmt.Errorf("spawn-env pull: %s env unset in the supervisor", supervisorCredentialEnv)
	}
	deadline := time.Now().Add(p.bound)
	for {
		res, reason, err := p.attemptOnce(ctx)
		if err == nil {
			return res, "", nil
		}
		// Unauthorized and a fully-read malformed body are deterministic
		// server-side states — retrying identical bytes only burns spawn
		// latency. Network/5xx outcomes (and truncated bodies, which are
		// transport faults) are retried until the bound.
		if reason == spawnEnvReasonUnauthorized || reason == spawnEnvReasonBadResponse {
			return spawnEnvResponse{}, reason, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return spawnEnvResponse{}, spawnEnvReasonUnavailable, err
		}
		gap := p.retryGap
		if gap > remaining {
			gap = remaining
		}
		select {
		case <-ctx.Done():
			return spawnEnvResponse{}, spawnEnvReasonUnavailable,
				fmt.Errorf("spawn-env pull aborted: %w", ctx.Err())
		case <-time.After(gap):
		}
	}
}

func (p *spawnEnvPuller) attemptOnce(ctx context.Context) (spawnEnvResponse, string, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, p.attempt)
	defer cancel()
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, p.url, nil)
	if err != nil {
		return spawnEnvResponse{}, spawnEnvReasonUnavailable, err
	}
	req.SetBasicAuth(p.username, p.password)
	resp, err := p.client.Do(req)
	if err != nil {
		return spawnEnvResponse{}, spawnEnvReasonUnavailable,
			fmt.Errorf("spawn-env pull %s: %w", p.url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		var out spawnEnvResponse
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			// Transport-level truncation — transient, retried.
			return spawnEnvResponse{}, spawnEnvReasonUnavailable,
				fmt.Errorf("spawn-env pull body: %w", err)
		}
		if err := json.Unmarshal(body, &out); err != nil {
			// Fully-read but malformed — deterministic, permanent.
			return spawnEnvResponse{}, spawnEnvReasonBadResponse,
				fmt.Errorf("spawn-env pull decode: %w", err)
		}
		if out.Env == nil {
			out.Env = map[string]string{}
		}
		return out, "", nil
	case http.StatusUnauthorized:
		return spawnEnvResponse{}, spawnEnvReasonUnauthorized,
			fmt.Errorf("spawn-env pull %s: 401 unauthorized", p.url)
	default:
		return spawnEnvResponse{}, spawnEnvReasonUnavailable,
			fmt.Errorf("spawn-env pull %s: status %d", p.url, resp.StatusCode)
	}
}
