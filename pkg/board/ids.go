// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ids.go — short-ID mint + did-you-mean resolution (design §4.1
// "Identifiers"). Shaped on design 0066's machinery (PR #1693): prefix +
// 8 chars [0-9a-z] from crypto/rand, minted in Go never SQL,
// check-and-retry on uniqueness violation, case-insensitive resolution,
// 404 + top-3 suggestions with bounded Levenshtein <= 2.
//
// SEAM NOTE: 0066's shared library (issue #1691's implementation lane) has
// not landed in main yet; this file is the board's local registration of
// the same algorithm so Lane 1 is not blocked. When 0066's mint/suggest
// package lands, this file collapses into an import (the closed prefix
// map gains prj-/epc-/tsk-). The shapes are deliberately identical.

// Short-ID prefixes (closed set; no sty- — stories do not exist, ruling R1).
const (
	PrefixProject = "prj-"
	PrefixEpic    = "epc-"
	PrefixTask    = "tsk-"
)

const shortIDSuffixLen = 8

// shortIDAlphabet is [0-9a-z] (0066's closed alphabet).
const shortIDAlphabet = "0123456789abcdefghijklmnopqrstuvwxyz"

// mintShortIDSuffix returns 8 random chars over [0-9a-z] (crypto/rand).
func mintShortIDSuffix() (string, error) {
	buf := make([]byte, shortIDSuffixLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("board: mint entropy: %w", err)
	}
	out := make([]byte, shortIDSuffixLen)
	for i, b := range buf {
		out[i] = shortIDAlphabet[int(b)%len(shortIDAlphabet)]
	}
	return string(out), nil
}

// mintShortID mints prefix+suffix and persists it via the caller-supplied
// insert (check-and-retry: the insert's uniqueness violation retriggers the
// mint, bounded attempts). The prefix is normalized (lowercase).
func mintShortID(prefix string) (string, error) {
	if !strings.HasPrefix(prefix, "prj-") && !strings.HasPrefix(prefix, "epc-") && !strings.HasPrefix(prefix, "tsk-") {
		return "", fmt.Errorf("board: unknown short-id prefix %q", prefix)
	}
	suffix, err := mintShortIDSuffix()
	if err != nil {
		return "", err
	}
	return prefix + suffix, nil
}

// suggestMaxEdits is the did-you-mean bound (0066: Levenshtein <= 2).
const suggestMaxEdits = 2

// suggestTop3 returns up to 3 candidates within the edit-distance bound,
// best-first. Candidates are in-scope identifiers/titles (the caller
// scopes them — did-you-mean never suggests across authorization
// boundaries).
func suggestTop3(requested string, candidates []string) []string {
	type scored struct {
		id    string
		score int
	}
	var hits []scored
	needle := strings.ToLower(requested)
	for _, c := range candidates {
		if len(hits) >= 3 && len(c) > len(needle)+suggestMaxEdits {
			continue
		}
		d := editDistanceBounded(needle, strings.ToLower(c), suggestMaxEdits)
		if d <= suggestMaxEdits {
			hits = append(hits, scored{c, d})
		}
	}
	// Stable best-first (fewer edits first; insertion order breaks ties).
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && hits[j].score < hits[j-1].score; j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
	if len(hits) > 3 {
		hits = hits[:3]
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.id)
	}
	return out
}

// editDistanceBounded is Levenshtein with an early exit: once the distance
// exceeds bound it returns bound+1 (0066/session_suggest.go shape).
func editDistanceBounded(a, b string, bound int) int {
	if a == b {
		return 0
	}
	la, lb := len(a), len(b)
	if la > lb {
		a, b = b, a
		la, lb = lb, la
	}
	if lb-la > bound {
		return bound + 1
	}
	prev := make([]int, la+1)
	curr := make([]int, la+1)
	for i := 0; i <= la; i++ {
		prev[i] = i
	}
	for j := 1; j <= lb; j++ {
		curr[0] = j
		rowMin := curr[0]
		for i := 1; i <= la; i++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[i] = min3(prev[i]+1, curr[i-1]+1, prev[i-1]+cost)
			if curr[i] < rowMin {
				rowMin = curr[i]
			}
		}
		if rowMin > bound {
			return bound + 1
		}
		prev, curr = curr, prev
	}
	if prev[la] > bound {
		return bound + 1
	}
	return prev[la]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}

// Store is the Postgres-backed data-access layer (pkg/workflows shape:
// constructed once at boot, safe for concurrent use).
type Store struct {
	pool *pgxpool.Pool
}

// NewStore constructs a Store backed by the given pgxpool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Pool exposes the pool for transactional callers (the service layer).
func (st *Store) Pool() *pgxpool.Pool { return st.pool }

// Clock is the engine's time source (seam for tests; production UTC).
type Clock func() time.Time

// Failpoint is the fault seam (kill-tests): the engine/reconciler invoke it
// at named points; a non-nil error simulates a crash mid-operation (the
// surrounding transaction aborts). Named points are pinned by the kill-test
// harness.
type Failpoint func(where string) error

// WorkerSpawner spawns a fresh-charter worker session for a granted claim
// (ruling R9: claim-then-spawn; one fresh session per claimed task). The
// spawn failure NEVER revokes the claim — the lease lapse path heals it
// (§4.4). Lane 3 wires the production session-creation seam; Lane 1 runs
// the engine with an injected spawner or the synthetic default.
type WorkerSpawner func(ctx context.Context, claim *Claim) (sessionID string, err error)

// WorkerAborter aborts a worker session whose claim was revoked (cancel).
// Rides the existing interrupt seam in production; no-op by default.
type WorkerAborter func(ctx context.Context, taskID, assigneeSession string) error

// PRState is the external-truth drift source's answer for one PR anchor.
type PRState struct {
	Merged bool
	Closed bool
	URL    string
}

// PRDriftSource resolves pr:N anchors for RC2 (v1: the platform's git
// host). Nil source disables RC2 drift checks (dark-run).
type PRDriftSource interface {
	GetPullRequest(ctx context.Context, prNumber int) (PRState, error)
}

// PendingInputState is the sessionstate pending-input projection's answer
// for one worker session (unanswered question/permission).
type PendingInputState struct {
	HasPendingInput bool
}

// PendingInputSource reads the pending-input projection for worker sessions
// (worker-origin auto-stage + resolution resume, §4.4). Nil disables the
// coupling (dark-run until Lane 3 wires the internal seam).
type PendingInputSource interface {
	PendingInput(ctx context.Context, assigneeSession string) (PendingInputState, error)
}
