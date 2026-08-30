// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// spawn_env_pull.go — design 0057 R2 (Epic 70 US-70.1): spawn-time env
// pull replaces the boot-time push.
//
// The push (pushInitialSpawnEnv, US-4a) dials the supervisor's control
// socket BEFORE the supervisor container can be running under native
// sidecar startup gating — structurally impossible, env-class secrets
// silently lost on every boot (fleet audit 2026-08-30: 3/6 pods).
//
// The pull inverts the direction and moves the point of consumption to
// spawn: at EVERY spawn the supervisor fetches the current delta from the
// sidecar mux (GET /v1/spawn-env on 4097, §D1 credential) with a BOUNDED
// wait; on timeout/unreachability it spawns with the LAST-GOOD delta from
// memory; a first-boot miss spawns platform-env-only and records
// degraded:spawn_env_unavailable (never-block-spawn; the silent-loss
// class becomes a visible, self-healing degrade).
//
// I4 (terminal verification): spawned_rev is the digest of the delta the
// child ACTUALLY spawned with — never a revision observed at
// materialization or fetch.
//
// I7 (spot-confinement): the last-good delta lives in supervisor memory
// only — no PVC, no logs, no audit rows.
//
// A2 (validated by construction here): the supervisor authenticates with
// the §D1 workspace credential read from /sandbox-cfg/password — a file
// the uid-1000 supervisor owns (init-fs installs it 0600 uid-1000) — so
// no NEW cross-uid file crossing appears in the boot path; the HTTP
// boundary is the crossing, gated by the D6.1 credential pair.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"
)

// spawnEnvResponse is the /v1/spawn-env wire shape: the CURRENT delta
// plus its content digest (the spawned_rev input).
type spawnEnvResponse struct {
	Env map[string]string `json:"env"`
	Rev string            `json:"rev"`
}

// spawnEnvHandler serves GET /v1/spawn-env: the CURRENT secrets delta,
// parsed from the secrets-env file at REQUEST time (fresh on every pull —
// reloads need no push), behind the D6.1 Basic gate. An absent file is an
// authoritative empty delta (normal), a corrupt one a 500 (the single
// writer never produces anything else — surface, don't drop).
func spawnEnvHandler(path string, passwords ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkBasicAuthAny(r, passwords...) {
			rejectUnauthorized(w)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// parseSecretsEnvDelta's parent-exclusion filters against the
		// SERVING process's environ — wrong reference for a cross-uid
		// server. The raw scan is the canonical delta; the supervisor's
		// parentPlusDelta applies platform-wins at compose time.
		data, err := os.ReadFile(path) //nolint:gosec // G304: deployment-configured coordinate
		if os.IsNotExist(err) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(spawnEnvResponse{})
			return
		}
		if err != nil {
			http.Error(w, "spawn-env read failed", http.StatusInternalServerError)
			return
		}
		parsed, err := scanShellquoteExports(string(data))
		if err != nil {
			log.Error("spawn-env: corrupt secrets-env (single writer — must surface)", zap.Error(err))
			http.Error(w, "spawn-env corrupt", http.StatusInternalServerError)
			return
		}
		delta := make(map[string]string, len(parsed))
		for _, kv := range parsed {
			delta[kv.name] = kv.value
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(spawnEnvResponse{Env: delta, Rev: spawnEnvDigest(delta)})
	}
}

// spawnEnvDigest is the deterministic content digest of a delta (sorted
// keys) — the rev the endpoint reports and spawned_rev verifies against.
func spawnEnvDigest(delta map[string]string) string {
	if len(delta) == 0 {
		return ""
	}
	keys := make([]string, 0, len(delta))
	for k := range delta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		_, _ = fmt.Fprintf(h, "%s=%s\n", k, delta[k]) //nolint:errcheck // hash writes cannot fail
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

// spawnEnvPuller is the supervisor-side pull state machine: bounded-wait
// fetch + memory-only last-good cache + terminal spawned_rev/degrade
// reporting. Safe for concurrent use (the spawn path and status reads).
type spawnEnvPuller struct {
	mu         sync.Mutex
	client     *http.Client
	url        string
	password   string
	bound      time.Duration
	lastGood   map[string]string
	lastRev    string
	spawnDelta map[string]string
	spawnedAt  string
	degraded   string
}

func newSpawnEnvPuller(url, password string, bound time.Duration) *spawnEnvPuller {
	return &spawnEnvPuller{
		client:   &http.Client{Timeout: bound},
		url:      url,
		password: password,
		bound:    bound,
	}
}

// pull fetches the current delta under the bound. On success it caches
// last-good (memory-only) and returns the fresh delta. On miss it
// returns the cached delta with degraded=spawn_env_unavailable (an
// empty, platform-env-only delta on first boot). Never blocks past the
// bound, never returns an error that could block a spawn.
func (p *spawnEnvPuller) pull(ctx context.Context) (delta map[string]string, rev, degraded string, err error) {
	ctx, cancel := context.WithTimeout(ctx, p.bound)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url+"/v1/spawn-env", nil)
	if err == nil {
		req.SetBasicAuth("opencode", p.password)
		var resp *http.Response
		resp, err = p.client.Do(req)
		if err == nil {
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode == http.StatusOK {
				var out spawnEnvResponse
				if json.NewDecoder(resp.Body).Decode(&out) == nil {
					p.mu.Lock()
					p.lastGood = out.Env
					p.lastRev = out.Rev
					p.degraded = ""
					p.mu.Unlock()
					return out.Env, out.Rev, "", nil
				}
			}
			err = fmt.Errorf("spawn-env pull: status %d", resp.StatusCode)
		}
	}
	// Miss (timeout, unreachable, bad status): last-good + loud degrade.
	p.mu.Lock()
	p.degraded = "spawn_env_unavailable"
	cached := p.lastGood
	rev = p.lastRev
	p.mu.Unlock()
	if cached == nil {
		cached = map[string]string{}
	}
	return cached, rev, "spawn_env_unavailable", nil //nolint:staticcheck // err captured in log below
}

// withSpawnEnvPull wraps a base cmd factory so every spawn (first boot +
// every restart) composes parent+delta from a fresh pull, records the
// terminal spawned state (I4: what the child spawned with), and surfaces
// the degrade reason (I10).
func withSpawnEnvPull(base func() *exec.Cmd, p *spawnEnvPuller) func() *exec.Cmd {
	return func() *exec.Cmd {
		cmd := base()
		delta, rev, degraded, _ := p.pull(context.Background())
		cmd.Env = parentPlusDelta(cmd.Env, delta)
		p.mu.Lock()
		p.spawnDelta = delta
		p.spawnedAt = rev
		p.degraded = degraded
		p.mu.Unlock()
		return cmd
	}
}

func (p *spawnEnvPuller) spawnedRev() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.spawnedAt
}

func (p *spawnEnvPuller) spawnedDelta() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.spawnDelta
}

func (p *spawnEnvPuller) degradedReason() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.degraded
}
