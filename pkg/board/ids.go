// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lenaxia/llmsafespaces/pkg/shortid"
)

// ids.go — phrase-ID mint + did-you-mean resolution (design §4.1
// "Identifiers", #1831 vocabulary flip). The #1831 CURRENT format is
// the session playbook's (#1756/#1790): the prj-/epc-/tsk- kind prefix
// (closed set) + a 3-word adjective-noun-verb phrase from the SAME
// frozen curated lists (e.g. "tsk-amber-lantern-ambles"), with a
// deterministic counter suffix ("-2", "-3", …) arbitrating mint
// collisions. Words are high-probability LLM tokens, transcription
// drift is visible, and the did-you-mean ranker recovers near-misses on
// WORDS far better than it ever could on random fragments.
//
// The PREVIOUS random form ("tsk-ab12cd34", design 0066's 8-char
// [0-9a-z] suffix) exists on clusters that ran main before #1831: those
// rows stay addressable as INPUT through the one-release dual-accept
// window (classified by IsLegacyBoardIDForm, marked
// short_id_deprecated on read via IsDeprecatedBoardID), never minted
// again.
//
// SEAM NOTE: 0066's shared library (issue #1691's implementation lane)
// never landed; the session phrase generator DID (pkg/shortid) — the
// board mint reuses it directly (same vocabulary, not a fork).

// Short-ID prefixes (closed set; no sty- — stories do not exist, ruling R1).
const (
	PrefixProject = "prj-"
	PrefixEpic    = "epc-"
	PrefixTask    = "tsk-"
)

// boardPrefixes is the closed kind map (mint admission + shape checks).
var boardPrefixes = []string{PrefixProject, PrefixEpic, PrefixTask}

// phraseIDForm is the #1831 shape: kind prefix + exactly three
// lowercase word tokens + optional deterministic counter suffix
// ("-2", "-10", …), case-insensitive on input. Shape-only, like the
// session classifier (shortid.IsShortIDForm): list MEMBERSHIP is the
// mint's guarantee, not the classifier's job — a transcribed drift
// ("lantrn") still classifies phrase-shaped and flows to the word
// ranker instead of a bare 404.
var phraseIDForm = regexp.MustCompile(`^(prj|epc|tsk)-[a-z]+-[a-z]+-[a-z]+(-([2-9]|[1-9][0-9]+))?$`)

// legacyBoardIDForm is the pre-#1831 random form: prefix + exactly 8
// chars over [0-9a-z] (design 0066's mint, constrained by the
// 000036/000042 CHECK). Addressable as INPUT through the dual-accept
// window, never minted again.
var legacyBoardIDForm = regexp.MustCompile(`^(prj|epc|tsk)-[0-9a-z]{8}$`)

// IsPhraseIDForm reports whether s carries the #1831 board-phrase
// shape (see phraseIDForm). Disjoint by construction from bare session
// phrases (no kind prefix), workspace phrases (2 words + digit), and
// the legacy random board form.
func IsPhraseIDForm(s string) bool {
	return phraseIDForm.MatchString(shortid.Normalize(s))
}

// IsLegacyBoardIDForm classifies the pre-#1831 random short-id form.
func IsLegacyBoardIDForm(s string) bool {
	return legacyBoardIDForm.MatchString(shortid.Normalize(s))
}

// IsDeprecatedBoardID reports whether the id belongs to an INPUT-only
// class whose acceptance is deprecated by #1831: the random form rows
// minted before the flip. Such rows keep resolving (dual-accept
// window) and render short_id_deprecated; the mint never emits the
// form again.
func IsDeprecatedBoardID(s string) bool {
	return IsLegacyBoardIDForm(s)
}

// mintBoardID mints a fresh #1831 phrase id: the kind prefix + a
// 3-word draw from the session generator's frozen lists (thread-safe
// mintRand under the shared lock). Uniqueness is NOT checked here —
// mintUnique's check-and-ladder owns it.
func mintBoardID(prefix string) (string, error) {
	if !slicesContains(boardPrefixes, prefix) {
		return "", fmt.Errorf("board: unknown short-id prefix %q", prefix)
	}
	phrase, err := shortid.MintPhrase()
	if err != nil {
		return "", fmt.Errorf("board: phrase mint: %w", err)
	}
	return prefix + phrase, nil
}

func slicesContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// suggestMaxEdits is the did-you-mean bound (0066: Levenshtein <= 2 —
// #1831: the bound now applies to WORD tokens for phrase candidates,
// characters for legacy candidates).
const suggestMaxEdits = 2

// suggestCandidateScore ranks one candidate against the needle. The
// vocabulary dispatch (#1831): a PHRASE-form candidate scores the
// minimum of word-token distance and character distance (word distance
// catches the one-word drift class — "tsk-amber-lantern-ambles" vs
// "tsk-amber-gondola-ambles" is word distance 1, character distance
// 6, out of bound; character still catches within-word typos); a
// LEGACY random candidate keeps the character ranker — word distance
// degenerates on the single random token (every same-prefix id would
// be distance 1 and the suggestion set becomes noise). Titles and
// other free text are never phrase-form (no kind prefix) and keep the
// character ranker.
func suggestCandidateScore(needle, candidate string) int {
	if IsPhraseIDForm(candidate) {
		wd := wordEditDistanceBounded(needle, candidate, suggestMaxEdits)
		if wd == 0 {
			return 0
		}
		cd := editDistanceBounded(strings.ToLower(needle), strings.ToLower(candidate), suggestMaxEdits)
		return min2(wd, cd)
	}
	return editDistanceBounded(strings.ToLower(needle), strings.ToLower(candidate), suggestMaxEdits)
}

// suggestTop3 returns up to 3 candidates within the edit-distance
// bound, best-first. Candidates are in-scope identifiers/titles (the caller
// scopes them — did-you-mean never suggests across authorization
// boundaries).
func suggestTop3(requested string, candidates []string) []string {
	type scored struct {
		id    string
		score int
	}
	var hits []scored
	for _, c := range candidates {
		d := suggestCandidateScore(requested, c)
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

// wordEditDistanceBounded is Levenshtein over the "-"-split WORD
// tokens (#1831's ranker unit), case-insensitive, with the same early
// exit as the character ranker: once the distance exceeds bound it
// returns bound+1.
func wordEditDistanceBounded(a, b string, bound int) int {
	return editDistanceTokensBounded(strings.Split(shortid.Normalize(a), "-"),
		strings.Split(shortid.Normalize(b), "-"), bound)
}

func editDistanceTokensBounded(a, b []string, bound int) int {
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

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
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

// WorkerAborter aborts a worker session whose claim was revoked (delete
// of an in-flight claim, the S8 reclaim deposal — every reconciler pass
// that definitively ends a claim aborts the deposed holder post-commit —
// and superseded-spawn cleanup). Rides the existing interrupt seam in
// production (the V1 session abort — no bus dependency); no-op by
// default.
type WorkerAborter func(ctx context.Context, taskID, assigneeSession string) error

// WorkerMessenger delivers a pause/resume DIRECTIVE to an in-flight
// worker's session (#1747 pause-must-pause): it lands at the current
// turn's boundary (finish the thought, then stop) — NOT an abort. Rides
// the orchestrator↔worker message machinery in production; no-op by
// default.
type WorkerMessenger func(ctx context.Context, sessionID, directive, taskShortID string) error

// BounceRedispatcher delivers the #1959 bounce re-dispatch turn to the
// ORIGINAL implementer's EXISTING session (the owner ruling's
// bounce-target semantics: the session that did the work holds the
// context). The payload text is engine-rendered (fresh bounce claim
// token + embedded verdict chain); delivery rides the same
// session-message machinery as WorkerMessenger (existing-session
// delivery — never a session:new spawn). A delivery error means the
// original is gone/unresponsive: the engine falls back to a
// fresh-implementer spawn.
type BounceRedispatcher func(ctx context.Context, sessionID, text, taskShortID string) error

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
