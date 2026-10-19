// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// session_ref.go — design 0066 §4.4 acceptance + §4.6 did-you-mean on
// the agentd MCP tools. The precedence, exactly as the design states
// it:
//
//  1. exact short id, case-insensitive — the replica (with an on-miss
//     pull from the platform's registry);
//  2. exact legacy harness id — byte-equal passthrough, NEVER rewritten
//     and never probed first (an unindexed-but-live session must stay
//     addressable — the #1663 lesson; the seam's own 404 remains the
//     verdict and carries the suggestion clause);
//  3. alias — the session's current title OR the harness slug
//     ('sunny-wolf'), workspace-scoped, UNAMBIGUOUS ONLY: a name two
//     sessions share is a did-you-mean, never a guess.
//
// Id-shaped refs that match nothing pass through untouched (the seam
// arbitrates downstream — byte-identical to today). Alias-shaped refs
// that match nothing cannot be ids at all: they fail HERE with the
// did-you-mean clause instead of a bare seam 404.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/lenaxia/llmsafespaces/pkg/sessionref"
	"github.com/lenaxia/llmsafespaces/pkg/shortid"
)

// shortIDPullTTL bounds how often an unresolved short id may trigger a
// registry pull (the on-miss path must not hammer the platform when a
// caller loops on a typo).
const shortIDPullTTL = 30 * time.Second

var (
	shortIDPullMu   sync.Mutex
	shortIDLastPull time.Time
)

// pullSessionShortIDs fetches the workspace's registry from the
// platform and replaces the replica. Best-effort: errors return false
// (resolution degrades; nothing fails).
func pullSessionShortIDs(ctx context.Context) bool {
	client, saToken, workspaceID, err := podPlatformClient()
	if err != nil {
		return false
	}
	pullCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	entries, err := client.SessionShortIDRegistry(pullCtx, saToken, workspaceID)
	if err != nil {
		return false
	}
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		m[e.ShortID] = e.SessionID
	}
	currentSessionShortIDs().putAll(m)
	return true
}

// pullSessionShortIDsRateLimited is the on-miss pull behind the TTL.
func pullSessionShortIDsRateLimited(ctx context.Context) bool {
	shortIDPullMu.Lock()
	if time.Since(shortIDLastPull) < shortIDPullTTL {
		shortIDPullMu.Unlock()
		return false
	}
	shortIDLastPull = time.Now()
	shortIDPullMu.Unlock()
	return pullSessionShortIDs(ctx)
}

// bootPullSessionShortIDs warms the replica at agentd start (§4.3
// population path (a)). Detached context, bounded, best-effort.
func bootPullSessionShortIDs(ctx context.Context) {
	pullCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	pullSessionShortIDs(pullCtx)
}

// seamIDPattern mirrors the seam's session-id charset
// (^([A-Za-z0-9_-]{1,128})$) — a ref matching it can be handed to the
// seam verbatim; anything else can only ever have been a name.
var seamIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// errSessionRef is the structured resolution failure returned to tool
// callers: the message carries the did-you-mean clause (the #1667
// style — suggest-only, never an auto-correct).
type errSessionRef struct {
	msg string
}

func (e *errSessionRef) Error() string { return e.msg }

// resolveSessionRef applies the §4.4 precedence for one ref. Legacy
// ids and unresolved id-shaped refs cost ZERO extra seam round trips
// (the passthrough is byte-identical and immediate); short ids cost a
// replica lookup (plus one rate-limited pull on miss); aliases cost
// the one bounded GET /session the tools mostly already pay.
func resolveSessionRef(ctx context.Context, password, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("session_id is required")
	}

	// 1. short id, case-insensitive — replica, then on-miss pull.
	if shortid.IsShortIDForm(ref) {
		if id, ok := currentSessionShortIDs().resolveShortID(ref); ok {
			return id, nil
		}
		if pullSessionShortIDsRateLimited(ctx) {
			if id, ok := currentSessionShortIDs().resolveShortID(ref); ok {
				return id, nil
			}
		}
		// Unresolved short id: id-shaped → pass through. The seam's
		// 404 (with the generalized clause) stays the verdict.
		return ref, nil
	}

	// 2. legacy harness id — byte-equal passthrough, never rewritten.
	if shortid.IsLegacyIDForm(ref) {
		return ref, nil
	}

	// 3. alias — title or slug, workspace-scoped, unambiguous only.
	sessions, err := seamClientWithPassword(password).SessionList(ctx)
	if err != nil {
		// The list fetch failed (transport/5xx): NEVER a suggestion
		// surface — degrade to passthrough and let the seam answer.
		return ref, nil
	}
	folded := strings.ToLower(strings.TrimSpace(ref))
	var matches []opencode.SessionSummary
	for _, s := range sessions {
		if strings.ToLower(strings.TrimSpace(s.Title)) == folded ||
			(s.Slug != "" && strings.ToLower(strings.TrimSpace(s.Slug)) == folded) {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0].ID, nil
	case 0:
		// No title/slug match. Charset-valid refs pass through
		// byte-identical: the seam's 404-class verdict downstream
		// appends the generalized clause (titles + slugs + short ids +
		// harness ids), and ids outside the ses_ shape (the
		// "ses-two" class) keep working. Refs the seam could never
		// accept anyway (spaces, dots, …) fail HERE with the clause —
		// a name was meant, and the caller gets the did-you-mean
		// instead of a client-side charset error.
		if seamIDPattern.MatchString(ref) {
			return ref, nil
		}
		return "", &errSessionRef{msg: fmt.Sprintf(
			"session %q not found: no session in this workspace is titled (or slugged) %q%s",
			ref, ref, suggestRefClauseFrom(ref, sessions))}
	default:
		names := make([]string, 0, len(matches))
		for _, m := range matches {
			names = append(names, describeSuggestion(sessionref.Suggestion{
				SessionID: m.ID,
				ShortID:   currentSessionShortIDs().shortIDFor(m.ID),
				Title:     m.Title,
			}))
		}
		return "", &errSessionRef{msg: fmt.Sprintf(
			"session alias %q is ambiguous — %d sessions share it (%s); none was used (use the exact session id or short id)",
			ref, len(matches), strings.Join(names, ", "))}
	}
}

// candidateOf builds the §4.6 candidate for one live session: short
// id from the replica, title + slug live, last-active from the
// harness row.
func candidateOf(s opencode.SessionSummary) sessionref.Candidate {
	last := time.Time{}
	if s.Time.Updated > 0 {
		last = time.UnixMilli(s.Time.Updated).UTC()
	}
	return sessionref.Candidate{
		SessionID:  s.ID,
		ShortID:    currentSessionShortIDs().shortIDFor(s.ID),
		Title:      s.Title,
		Slug:       s.Slug,
		LastActive: last,
	}
}

// suggestRefClauseFrom ranks the live sessions against a failed ref
// and formats the did-you-mean clause (the #1667 text style,
// generalized: candidates carry short ids, titles, and slugs).
func suggestRefClauseFrom(requested string, sessions []opencode.SessionSummary) string {
	cands := make([]sessionref.Candidate, 0, len(sessions))
	for _, s := range sessions {
		cands = append(cands, candidateOf(s))
	}
	ranked := sessionref.Rank(requested, cands)
	switch len(ranked) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf(" — did you mean %s? re-copy the exact id from session_list / session_metadata",
			describeSuggestion(ranked[0]))
	default:
		names := make([]string, 0, len(ranked))
		for _, r := range ranked {
			names = append(names, describeSuggestion(r))
		}
		return fmt.Sprintf(" — %d sessions are near %q (%s); none was used — re-copy the exact id from session_list / session_metadata",
			len(ranked), requested, strings.Join(names, ", "))
	}
}

// describeSuggestion renders one candidate for the clause: the short
// id when known (the canonical address), the harness id always (the
// re-copy target), and the title/slug when present.
func describeSuggestion(s sessionref.Suggestion) string {
	label := s.SessionID
	if s.ShortID != "" {
		label = s.ShortID + " (" + s.SessionID + ")"
	}
	if s.Title != "" {
		label += " — " + s.Title
	} else if s.SessionID == "" && s.ShortID != "" {
		label = s.ShortID
	}
	return label
}
