// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
)

// --- #1663: near-miss session-id suggestions --------------------------------
//
// Root cause of the incident (evidence in the PR body): the caller's
// model re-typed a live 30-char session id with ONE character dropped,
// in two different variants across attempts. Every platform surface
// carried the full id byte-exactly; the bare "not found" error was the
// only reply — and it gave the caller no signal that a near-identical
// LIVE session existed. An hour was lost diagnosing a "zombie" session
// that was addressable the whole time.
//
// This layer converts that failure into an immediate self-correction:
// when a session id fails by-ID resolution, the workspace's live id
// inventory is consulted ONCE and a did-you-mean clause is appended to
// the error. SUGGEST-ONLY by design — nothing is ever auto-corrected,
// re-addressed, or delivered on a guess (misdelivery risk; destructive
// verbs must never act on a near-match). The clause is advisory: any
// failure to compute it degrades to the legacy bare error.

// suggestMaxEdits bounds how close a live id must be to the requested
// (unresolvable) id to be offered: distance 1 covers the observed
// single-character drops/inserts/substitutions; distance 2 also covers
// an adjacent transposition or a double drop (plain Levenshtein — a
// transposition scores 2, not 1). Three or more edits between 30-char
// mixed-case ids is noise, not a typo.
const suggestMaxEdits = 2

// suggestSessionID returns a did-you-mean clause for a session id that
// failed by-ID resolution, computed against the workspace's live id
// inventory; "" when there is nothing useful to say (no near candidate,
// the id actually resolves, or the inventory is empty). Deterministic:
// a unique closest candidate is named; a tie names all tied candidates
// and never picks one. The inventory is list-based (#1452 caveat): a
// near-match hidden from GET /session cannot be suggested — best-effort
// by design, never a substitute for the exact id.
func suggestSessionID(requested string, ids []string) string {
	if requested == "" {
		return ""
	}
	minDist := suggestMaxEdits + 1
	var atMin []string
	for _, id := range ids {
		if id == "" || id == requested {
			continue
		}
		d := editDistanceBounded(requested, id, suggestMaxEdits)
		if d > suggestMaxEdits {
			continue
		}
		switch {
		case d < minDist:
			minDist, atMin = d, []string{id}
		case d == minDist:
			atMin = append(atMin, id)
		}
	}
	switch len(atMin) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf(" — did you mean %s? (your id differs by %d character%s; re-copy the exact id from session_list / session_metadata)",
			atMin[0], minDist, pluralEdits(minDist))
	default:
		sort.Strings(atMin)
		return fmt.Sprintf(" — %d workspace sessions are within %d edits of that id (%s); none was used — re-copy the exact id from session_list / session_metadata",
			len(atMin), suggestMaxEdits, strings.Join(atMin, ", "))
	}
}

func pluralEdits(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// sessionSuggestClause is suggestSessionID plus the live-inventory
// fetch: one bounded GET /session on the failure path. Best-effort —
// any error returns "" (the suggestion must never fail, delay beyond
// the one fetch, or mask the original error).
func sessionSuggestClause(ctx context.Context, client *opencode.Client, requestedID string) string {
	sessions, err := client.SessionList(ctx)
	if err != nil {
		return ""
	}
	ids := make([]string, 0, len(sessions))
	for _, s := range sessions {
		ids = append(ids, s.ID)
	}
	return suggestSessionID(requestedID, ids)
}

// isSessionNotFoundErr recognizes the seam's 404 error shapes —
// statusError's "status 404:" (GET/POST routes, the automation surface)
// and Abort's "returned 404:". The suggestion clause is appended ONLY
// on not-found: transport failures and 5xx keep their original text
// (an unreachable server is not a typo, and a suggestion there would
// train callers to ignore real errors).
func isSessionNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "status 404:") || strings.Contains(msg, "returned 404:")
}

// editDistanceBounded is Levenshtein distance with an early exit: once
// every cell of the current row exceeds bound, the distance cannot come
// back under it, so bound+1 is returned immediately. Ids are capped at
// 128 chars by the seam's validation pattern; the bounded loop keeps
// the failure path O(len·len) worst case and usually far less.
func editDistanceBounded(a, b string, bound int) int {
	la, lb := len(a), len(b)
	if la == 0 {
		if lb <= bound {
			return lb
		}
		return bound + 1
	}
	if lb == 0 {
		if la <= bound {
			return la
		}
		return bound + 1
	}
	if abs(la-lb) > bound {
		return bound + 1
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		rowMin := curr[0]
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min3(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
			if curr[j] < rowMin {
				rowMin = curr[j]
			}
		}
		if rowMin > bound {
			return bound + 1
		}
		prev, curr = curr, prev
	}
	return prev[lb]
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

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
