// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionindex

import (
	"context"
	"testing"
	"time"

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
	r := &fakeRefResolver{shortIDs: map[string]string{"oc-ab12cd34": "ses_live"}}
	res := ResolveSessionRef(context.Background(), r, "ws", "OC-AB12CD34")
	if res.CanonicalID != "ses_live" || res.MatchedBy != MatchShortID || res.ShortID != "oc-ab12cd34" {
		t.Fatalf("short-id precedence broken: %+v", res)
	}
}

func TestResolveSessionRef_ShortIDMissPassesThrough(t *testing.T) {
	r := &fakeRefResolver{shortIDs: map[string]string{}}
	res := ResolveSessionRef(context.Background(), r, "ws", "oc-00000000")
	if res.CanonicalID != "oc-00000000" || res.MatchedBy != MatchPassthrough {
		t.Fatalf("id-shaped miss must pass through for the adapter to arbitrate: %+v", res)
	}
}

func TestResolveSessionRef_LegacyIDByteIdenticalNeverLookedUp(t *testing.T) {
	// #1663's core safety: a legacy id — live, truncated, or bogus —
	// is NEVER rewritten, never "corrected", never DB-gated. Even a
	// one-char-truncated id passes through byte-identical (the seam's
	// 404 + did-you-mean clause is the recovery, not silent repair).
	r := &fakeRefResolver{shortIDs: map[string]string{"oc-ab12cd34": "ses_live"}}
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
		"worker: #1691": {{ID: "ses_a", ShortID: "oc-11111111", Title: "Worker: #1691"}},
	}}
	res := ResolveSessionRef(context.Background(), r, "ws", "  Worker: #1691 ")
	if res.CanonicalID != "ses_a" || res.MatchedBy != MatchAlias || res.ShortID != "oc-11111111" {
		t.Fatalf("alias happy path broken: %+v", res)
	}
}

func TestResolveSessionRef_AliasCollisionRefuses(t *testing.T) {
	// Two sessions share the title: NEVER auto-resolve — the caller
	// gets the ambiguity payload (design 0066 §4.4.3 / ruling 8).
	r := &fakeRefResolver{aliases: map[string][]types.SessionListItem{
		"mighty-harbor": {
			{ID: "ses_new", ShortID: "oc-aaaaaaaa"},
			{ID: "ses_old", ShortID: "oc-bbbbbbbb"},
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
		{ID: "ses_ed9183a46ffeRiNw2PddpL5dAA", ShortID: "oc-ab12cd34", Title: "Worker: #1648 mobile viewport fix", LastMessageAt: &when},
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
		crossWS: []types.SessionListItem{{ID: "ses_x", ShortID: "oc-99999999", Workspace: "ws-other", Title: "Board lane"}},
	}
	got := SuggestionsFor(context.Background(), r, "user-1", "ws-own", "oc-99999999")
	if len(got) != 1 || got[0].Workspace != "ws-other" {
		t.Fatalf("cross-workspace suggestion missing: %+v", got)
	}

	// Own workspace scores → other workspaces are NOT consulted.
	r2 := &fakeRefResolver{
		list:    []types.SessionListItem{{ID: "ses_own", ShortID: "oc-99999999"}},
		ownerWS: []string{"ws-own", "ws-other"},
		crossWS: []types.SessionListItem{{ID: "ses_x", ShortID: "oc-99999999", Workspace: "ws-other"}},
	}
	got2 := SuggestionsFor(context.Background(), r2, "user-1", "ws-own", "oc-99999998")
	if len(got2) != 1 || got2[0].Workspace != "ws-own" {
		t.Fatalf("own-workspace hit must not fall through to cross-workspace: %+v", got2)
	}
}

func TestSuggestionsFor_NoUserNoCrossWorkspace(t *testing.T) {
	r := &fakeRefResolver{ownerWS: []string{"ws-other"}, crossWS: []types.SessionListItem{{ID: "ses_x"}}}
	if got := SuggestionsFor(context.Background(), r, "", "ws-own", "oc-00000001"); len(got) != 0 {
		t.Fatalf("unauthenticated scope must never widen: %+v", got)
	}
}
