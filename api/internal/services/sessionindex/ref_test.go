// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionindex

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/sessionref"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// fakeRefResolver stubs the RefResolver surface with canned state.
type fakeRefResolver struct {
	shortIDs map[string]string                  // lower(short) -> session id
	aliases  map[string][]types.SessionListItem // lower(alias) -> rows
	list     []types.SessionListItem
	ownerWS  []string
	crossWS  []types.SessionListItem
}

func (f *fakeRefResolver) FindSessionByShortID(_ context.Context, _, shortID string) (string, error) {
	return f.shortIDs[lower(shortID)], nil
}

func (f *fakeRefResolver) FindSessionsByAlias(_ context.Context, _, alias string) ([]types.SessionListItem, error) {
	return f.aliases[lower(alias)], nil
}

func (f *fakeRefResolver) ListSessionIndex(_ context.Context, _ string) ([]types.SessionListItem, error) {
	return f.list, nil
}

func (f *fakeRefResolver) ListWorkspaceIDsByOwner(_ context.Context, _ string) ([]string, error) {
	return f.ownerWS, nil
}

func (f *fakeRefResolver) FindRefCandidatesAcrossWorkspaces(_ context.Context, _ []string, _ string) ([]types.SessionListItem, error) {
	return f.crossWS, nil
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func TestResolveSessionRef_NilResolverPassthrough(t *testing.T) {
	res := ResolveSessionRef(context.Background(), nil, "ws", "ses_abc")
	if res.CanonicalID != "ses_abc" || res.MatchedBy != MatchPassthrough {
		t.Fatalf("nil resolver must pass through byte-identical: %+v", res)
	}
}

func TestResolveSessionRef_ShortIDCaseInsensitive(t *testing.T) {
	r := &fakeRefResolver{shortIDs: map[string]string{"amber-falcon-glides": "ses_live"}}
	res := ResolveSessionRef(context.Background(), r, "ws", "Amber-Falcon-Glides")
	if res.CanonicalID != "ses_live" || res.MatchedBy != MatchShortID || res.ShortID != "amber-falcon-glides" || res.DeprecatedInput {
		t.Fatalf("short-id precedence broken: %+v", res)
	}
}

func TestResolveSessionRef_ShortIDMissPassesThrough(t *testing.T) {
	r := &fakeRefResolver{shortIDs: map[string]string{}}
	res := ResolveSessionRef(context.Background(), r, "ws", "opal-meadow-drifts")
	if res.CanonicalID != "opal-meadow-drifts" || res.MatchedBy != MatchPassthrough {
		t.Fatalf("id-shaped miss must pass through for the adapter to arbitrate: %+v", res)
	}
}

func TestResolveSessionRef_LegacyIDByteIdenticalNeverLookedUp(t *testing.T) {
	// #1663's core safety: a legacy id — live, truncated, or bogus —
	// is NEVER rewritten, never "corrected", never DB-gated. Even a
	// one-char-truncated id passes through byte-identical (the seam's
	// 404 + did-you-mean clause is the recovery, not silent repair).
	r := &fakeRefResolver{shortIDs: map[string]string{"amber-falcon-glides": "ses_live"}}
	for _, id := range []string{
		"ses_ed9183a46ffeRiNw2PddpL5dAA",
		"ses_ed9183a46ffeRiNw2PddpL5dA", // truncated
		"ses_totally_bogus",
	} {
		res := ResolveSessionRef(context.Background(), r, "ws", id)
		if res.CanonicalID != id || res.MatchedBy != MatchPassthrough {
			t.Errorf("legacy id %q resolved to %+v — must pass through byte-identical", id, res)
		}
	}
}

func TestResolveSessionRef_AliasUnambiguous(t *testing.T) {
	r := &fakeRefResolver{aliases: map[string][]types.SessionListItem{
		"worker: #1691": {{ID: "ses_a", ShortID: "amber-falcon-glides", Title: "Worker: #1691"}},
	}}
	res := ResolveSessionRef(context.Background(), r, "ws", "  Worker: #1691 ")
	if res.CanonicalID != "ses_a" || res.MatchedBy != MatchAlias || res.ShortID != "amber-falcon-glides" {
		t.Fatalf("alias happy path broken: %+v", res)
	}
}

func TestResolveSessionRef_AliasCollisionRefuses(t *testing.T) {
	// Two sessions share the title: NEVER auto-resolve — the caller
	// gets the ambiguity payload (design 0066 §4.4.3 / ruling 8).
	r := &fakeRefResolver{aliases: map[string][]types.SessionListItem{
		"mighty-harbor": {
			{ID: "ses_new", ShortID: "jade-harvest-murmurs"},
			{ID: "ses_old", ShortID: "linen-orchard-chirps"},
		},
	}}
	res := ResolveSessionRef(context.Background(), r, "ws", "mighty-harbor")
	if !res.AmbiguousAlias || res.CanonicalID != "" || len(res.AliasMatches) != 2 {
		t.Fatalf("ambiguity must refuse and carry both rows: %+v", res)
	}
}

func TestResolveSessionRef_AliasMissRefuses(t *testing.T) {
	// An unresolvable ALIAS-form ref must NOT pass through (it can
	// only ever have meant a name — passing it downstream would
	// silently ghost-row the title/seen upserts).
	r := &fakeRefResolver{}
	res := ResolveSessionRef(context.Background(), r, "ws", "sunny-wolf")
	if !res.UnresolvedAlias || res.CanonicalID != "" {
		t.Fatalf("alias miss must refuse: %+v", res)
	}
}

func TestSuggestionsFor_WorkspaceScopedFirst(t *testing.T) {
	when := time.Now().Add(-time.Hour)
	r := &fakeRefResolver{list: []types.SessionListItem{
		{ID: "ses_ed9183a46ffeRiNw2PddpL5dAA", ShortID: "amber-falcon-glides", Title: "Worker: #1648 mobile viewport fix", LastMessageAt: &when},
	}}
	got := SuggestionsFor(context.Background(), r, "user-1", "ws", "ses_ed9183a46ffeRiNw2PddpL5dA")
	if len(got) != 1 || got[0].SessionID != "ses_ed9183a46ffeRiNw2PddpL5dAA" {
		t.Fatalf("suggestions = %+v; want the near-miss live session", got)
	}
}

func TestSuggestionsFor_CrossWorkspaceOnlyWhenOwnEmpty(t *testing.T) {
	// Own workspace scores nothing → the requester's OTHER
	// authorized workspaces are consulted once (exact matches only).
	r := &fakeRefResolver{
		ownerWS: []string{"ws-own", "ws-other"},
		crossWS: []types.SessionListItem{{ID: "ses_x", ShortID: "opal-meadow-drifts", Workspace: "ws-other", Title: "Board lane"}},
	}
	got := SuggestionsFor(context.Background(), r, "user-1", "ws-own", "opal-meadow-drifts")
	if len(got) != 1 || got[0].Workspace != "ws-other" {
		t.Fatalf("cross-workspace suggestion missing: %+v", got)
	}

	// Own workspace scores → other workspaces are NOT consulted.
	r2 := &fakeRefResolver{
		list:    []types.SessionListItem{{ID: "ses_own", ShortID: "opal-meadow-drifts"}},
		ownerWS: []string{"ws-own", "ws-other"},
		crossWS: []types.SessionListItem{{ID: "ses_x", ShortID: "opal-meadow-drifts", Workspace: "ws-other"}},
	}
	got2 := SuggestionsFor(context.Background(), r2, "user-1", "ws-own", "opal-meadow-drift")
	if len(got2) != 1 || got2[0].Workspace != "ws-own" {
		t.Fatalf("own-workspace hit must not fall through to cross-workspace: %+v", got2)
	}
}

// Review r1 finding 4's pin: cross-workspace exact matches never
// exceed the top-3 cap, and their order is deterministic (last-active
// DESC, then workspace/session id) even when duplicates abound.
func TestSuggestionsFor_CrossWorkspaceTopThreeCap(t *testing.T) {
	now := time.Now()
	older := now.Add(-2 * time.Hour)
	oldest := now.Add(-3 * time.Hour)
	r := &fakeRefResolver{
		ownerWS: []string{"ws-own", "w1", "w2", "w3", "w4"},
		crossWS: []types.SessionListItem{
			{ID: "ses_new", ShortID: "amber-falcon-glides", Workspace: "w1", Title: "dup", LastMessageAt: &now},
			{ID: "ses_mid", ShortID: "oc-22222222", Workspace: "w2", Title: "dup", LastMessageAt: &older},
			{ID: "ses_old", ShortID: "oc-33333333", Workspace: "w3", Title: "dup", LastMessageAt: &oldest},
			{ID: "ses_anc", ShortID: "oc-44444444", Workspace: "w4", Title: "dup", LastMessageAt: nil},
		},
	}
	got := SuggestionsFor(context.Background(), r, "user-1", "ws-own", "opal-meadow-drifts")
	require.Len(t, got, sessionref.MaxSuggestions, "ruling 8: top-3, ALWAYS — including the cross-workspace tail")
	if got[0].SessionID != "ses_new" || got[1].SessionID != "ses_mid" || got[2].SessionID != "ses_old" {
		t.Fatalf("cross-workspace order must be last-active DESC: %s, %s, %s",
			got[0].SessionID, got[1].SessionID, got[2].SessionID)
	}
}

// Review r1 finding 5's pin: a DB error on the alias lookup degrades
// to passthrough — a 5xx must never masquerade as a miss and answer a
// 404-with-suggestions.
func TestResolveSessionRef_AliasLookupErrorDegradesToPassthrough(t *testing.T) {
	r := &erroringAliasResolver{}
	res := ResolveSessionRef(context.Background(), r, "ws", "some-title")
	if res.MatchedBy != MatchPassthrough || res.CanonicalID != "some-title" {
		t.Fatalf("alias lookup error must degrade to passthrough: %+v", res)
	}
	if !res.DegradedLookup {
		t.Fatal("the degrade must be flagged so write surfaces can guard (r2 finding B)")
	}
}

type erroringAliasResolver struct{}

func (e *erroringAliasResolver) FindSessionByShortID(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (e *erroringAliasResolver) FindSessionsByAlias(_ context.Context, _, _ string) ([]types.SessionListItem, error) {
	return nil, errors.New("db down")
}
func (e *erroringAliasResolver) ListSessionIndex(_ context.Context, _ string) ([]types.SessionListItem, error) {
	return nil, nil
}
func (e *erroringAliasResolver) ListWorkspaceIDsByOwner(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}
func (e *erroringAliasResolver) FindRefCandidatesAcrossWorkspaces(_ context.Context, _ []string, _ string) ([]types.SessionListItem, error) {
	return nil, nil
}

// Review r2 finding A's pin: the cross-workspace tail is ordered
// last-active DESC even when the cap does NOT engage (1–3 matches —
// the common case; the sort must be the source of determinism, not
// DB return order).
func TestSuggestionsFor_CrossWorkspaceOrderingWithoutCap(t *testing.T) {
	now := time.Now()
	stale := now.Add(-5 * 24 * time.Hour)
	r := &fakeRefResolver{
		ownerWS: []string{"ws-own", "w1", "w2"},
		crossWS: []types.SessionListItem{
			// Deliberately seeded STALE-first so DB order and contract
			// order differ.
			{ID: "ses_stale", ShortID: "oc-33333333", Workspace: "w1", Title: "deploy", LastMessageAt: &stale},
			{ID: "ses_fresh", ShortID: "amber-falcon-glides", Workspace: "w2", Title: "deploy", LastMessageAt: &now},
		},
	}
	got := SuggestionsFor(context.Background(), r, "user-1", "ws-own", "opal-meadow-drifts")
	require.Len(t, got, 2)
	if got[0].SessionID != "ses_fresh" || got[1].SessionID != "ses_stale" {
		t.Fatalf("≤3 cross-workspace matches must still list newest first: %s, %s",
			got[0].SessionID, got[1].SessionID)
	}
}

func TestSuggestionsFor_NoUserNoCrossWorkspace(t *testing.T) {
	r := &fakeRefResolver{ownerWS: []string{"ws-other"}, crossWS: []types.SessionListItem{{ID: "ses_x"}}}
	if got := SuggestionsFor(context.Background(), r, "", "ws-own", "oc-00000001"); len(got) != 0 {
		t.Fatalf("unauthenticated scope must never widen: %+v", got)
	}
}
