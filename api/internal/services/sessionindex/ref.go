// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionindex

import (
	"context"
	"strings"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/sessionref"
	"github.com/lenaxia/llmsafespaces/pkg/shortid"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// RefResolver is the read surface the design 0066 §4.4 resolution
// precedence runs against. Production wires the *database.Service
// (which implements it through the DatabaseService interface); nil
// means resolution is disabled and every ref passes through
// byte-identical — the backward-compatibility posture.
type RefResolver interface {
	FindSessionByShortID(ctx context.Context, workspaceID, shortID string) (string, error)
	FindSessionsByAlias(ctx context.Context, workspaceID, alias string) ([]types.SessionListItem, error)
	ListSessionIndex(ctx context.Context, workspaceID string) ([]types.SessionListItem, error)
	ListWorkspaceIDsByOwner(ctx context.Context, userID string) ([]string, error)
	FindRefCandidatesAcrossWorkspaces(ctx context.Context, workspaceIDs []string, ref string) ([]types.SessionListItem, error)
}

// RefMatch names how a ref resolved ("" = passthrough).
type RefMatch string

const (
	// MatchPassthrough: no positive resolution — use the ref as-is.
	// Every legacy harness id takes this path BYTE-IDENTICAL (design
	// 0066 §4.4 precedence 2 is exact-match acceptance, never a
	// rewrite); an unresolvable id-shaped ref also passes through so
	// the adapter remains the authority (index lag must never make a
	// live session unaddressable — the #1663 lesson).
	MatchPassthrough RefMatch = ""
	// MatchShortID: precedence 1 — case-insensitive short-id equality.
	MatchShortID RefMatch = "short"
	// MatchAlias: precedence 3 — exactly one current title equals the
	// ref under casefold+trim.
	MatchAlias RefMatch = "alias"
)

// RefResolution is the outcome of one ref resolution.
type RefResolution struct {
	// CanonicalID is the harness session id the caller must use. On
	// passthrough it equals the input ref byte-for-byte. Empty ONLY
	// on MatchPassthrough of an empty input, or on an unresolvable
	// ALIAS-form ref (UnresolvedAlias / AmbiguousAlias) — alias-form
	// refs never pass through: they can only ever have meant a name.
	CanonicalID string
	// MatchedBy is the precedence step that accepted the ref.
	MatchedBy RefMatch
	// ShortID is the session's short id when known (short-id match,
	// or alias match on a row that carries one).
	ShortID string
	// UnresolvedAlias: an alias-form ref that matched nothing.
	UnresolvedAlias bool
	// AmbiguousAlias: an alias-form ref matching >1 current title —
	// NEVER auto-resolved (ruling 8); AliasMatches carries the rows
	// for the did-you-mean payload.
	AmbiguousAlias bool
	AliasMatches   []types.SessionListItem
}

// ResolveSessionRef applies design 0066 §4.4's precedence to one ref
// within one workspace:
//
//  1. exact short id, case-insensitive;
//  2. exact legacy harness id — byte-equal passthrough (NEVER rewritten,
//     never DB-verified: an unindexed-but-live session must stay
//     addressable);
//  3. alias (current title), workspace-scoped, unambiguous only.
//
// Id-shaped refs that match nothing pass through unchanged (the
// adapter's 404 remains the verdict; suggestions attach there).
// Alias-shaped refs that match nothing — or match several — do NOT
// pass through: a name was meant, and the caller gets the structured
// not-found/did-you-mean instead of a silent ghost-row write.
func ResolveSessionRef(ctx context.Context, r RefResolver, workspaceID, ref string) RefResolution {
	if r == nil {
		return RefResolution{CanonicalID: ref, MatchedBy: MatchPassthrough}
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return RefResolution{CanonicalID: ref, MatchedBy: MatchPassthrough}
	}
	if shortid.IsShortIDForm(ref) {
		id, err := r.FindSessionByShortID(ctx, workspaceID, ref)
		if err == nil && id != "" {
			return RefResolution{CanonicalID: id, MatchedBy: MatchShortID, ShortID: shortid.Normalize(ref)}
		}
		// Miss (or transient error): id-shaped → pass through. The
		// adapter stays the authority; its 404-class verdict carries
		// the suggestions.
		return RefResolution{CanonicalID: ref, MatchedBy: MatchPassthrough}
	}
	if shortid.IsLegacyIDForm(ref) {
		// Precedence 2: byte-equal acceptance. A lookup here could
		// only "confirm" what the adapter will confirm anyway, and a
		// miss must NOT downgrade addressing (index lag ≠ absent).
		return RefResolution{CanonicalID: ref, MatchedBy: MatchPassthrough}
	}
	// Alias-form: must resolve or refuse.
	matches, err := r.FindSessionsByAlias(ctx, workspaceID, ref)
	if err != nil || len(matches) == 0 {
		return RefResolution{UnresolvedAlias: true}
	}
	if len(matches) > 1 {
		return RefResolution{AmbiguousAlias: true, AliasMatches: matches}
	}
	return RefResolution{
		CanonicalID: matches[0].ID,
		MatchedBy:   MatchAlias,
		ShortID:     matches[0].ShortID,
	}
}

// SuggestionsFor computes the §4.6 did-you-mean payload for a failed
// ref: workspace-scoped candidates first (bounded by the list
// surface's own LIMIT), then — only when nothing scores and the
// requester is known — an exact-match pass over the requester's OTHER
// authorized workspaces (one bounded query; suggestions never reveal
// a row the caller cannot already list). Best-effort by contract: any
// error path returns fewer or no suggestions, never fails the caller.
func SuggestionsFor(ctx context.Context, r RefResolver, userID, workspaceID, ref string) []sessionref.Suggestion {
	if r == nil || strings.TrimSpace(ref) == "" {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	var out []sessionref.Suggestion
	if rows, err := r.ListSessionIndex(cctx, workspaceID); err == nil {
		cands := make([]sessionref.Candidate, 0, len(rows))
		for _, row := range rows {
			cands = append(cands, sessionref.Candidate{
				SessionID:  row.ID,
				ShortID:    row.ShortID,
				Title:      row.Title,
				Workspace:  workspaceID,
				LastActive: lastActiveOf(row),
			})
		}
		out = sessionref.Rank(ref, cands)
	}
	if len(out) > 0 || userID == "" {
		return out
	}
	ids, err := r.ListWorkspaceIDsByOwner(cctx, userID)
	if err != nil || len(ids) == 0 {
		return out
	}
	others := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != workspaceID {
			others = append(others, id)
		}
	}
	if len(others) == 0 {
		return out
	}
	if rows, err := r.FindRefCandidatesAcrossWorkspaces(cctx, others, ref); err == nil && len(rows) > 0 {
		for _, row := range rows {
			s := sessionref.Suggestion{
				Title:     row.Title,
				ShortID:   row.ShortID,
				SessionID: row.ID,
				Workspace: row.Workspace,
			}
			if t := lastActiveOf(row); !t.IsZero() {
				utc := t.UTC()
				s.LastActive = &utc
			}
			out = append(out, s)
		}
	}
	return out
}

// lastActiveOf picks a row's freshest activity timestamp (never zero
// when either exists) for ranking + payload.
func lastActiveOf(row types.SessionListItem) time.Time {
	switch {
	case row.LastMessageAt != nil && row.LastSeenAt != nil:
		if row.LastMessageAt.After(*row.LastSeenAt) {
			return *row.LastMessageAt
		}
		return *row.LastSeenAt
	case row.LastMessageAt != nil:
		return *row.LastMessageAt
	case row.LastSeenAt != nil:
		return *row.LastSeenAt
	default:
		return time.Time{}
	}
}
