// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionref

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The #1663 incident ids (live evidence): the caller's model re-typed
// the 30-char id one character off in two variants.
const (
	incidentLiveID   = "ses_ed9183a46ffeRiNw2PddpL5dAA"
	incidentVariant1 = "ses_ed9183a46ffeRiNw2PddpL5dA"  // dropped final 'A'
	incidentVariant2 = "ses_ed9183a46ffeRiNw2PddpL5daA" // 'A'→'a' substitution
	otherID          = "ses_eaef7aee4ffem2ma1jE6ox9Edq"
)

func ago(m time.Duration) time.Time { return time.Now().Add(-m) }

func TestEditDistanceBounded(t *testing.T) {
	cases := []struct {
		a, b        string
		bound, want int
	}{
		{"", "", 2, 0},
		{"abc", "abc", 2, 0},
		{incidentLiveID, incidentVariant1, 2, 1}, // drop
		{incidentLiveID, incidentVariant2, 2, 1}, // substitute
		{"abc", "bac", 2, 2},                     // single transposition scores 2
		{"ab", "ba", 2, 2},
		{"abcd", "badc", 2, 3}, // two displaced chars: distance 3 exceeds bound 2 → bound+1
		{"abcd", "badc", 4, 3}, // same pair, bound lifted: true distance 3
		{"abc", "x", 2, 3},     // exceeds bound → bound+1
		{"", "ab", 2, 2},
	}
	for _, c := range cases {
		if got := EditDistanceBounded(c.a, c.b, c.bound); got != c.want {
			t.Errorf("EditDistanceBounded(%q,%q,%d) = %d; want %d", c.a, c.b, c.bound, got, c.want)
		}
	}
}

func TestRank_LegacyIDEdits_SuggestsLiveID(t *testing.T) {
	// The #1663 regression class: a one-char-truncated id must produce
	// a suggestion naming the live id — never silence, never a wrong
	// resolution (Rank is suggest-only by construction).
	cands := []Candidate{
		{SessionID: incidentLiveID, ShortID: "oc-11111111", Title: "Worker: #1648 mobile viewport fix", LastActive: ago(time.Hour)},
		{SessionID: otherID, ShortID: "oc-22222222", Title: "Worker: #1735 archived-session filter", LastActive: ago(2 * time.Hour)},
	}
	got := Rank(incidentVariant1, cands)
	if len(got) == 0 {
		t.Fatal("truncated id produced no suggestions (the #1663 silence)")
	}
	if got[0].SessionID != incidentLiveID {
		t.Errorf("top suggestion = %q; want the near-miss live id %q", got[0].SessionID, incidentLiveID)
	}
}

func TestRank_ShortIDEdits(t *testing.T) {
	cands := []Candidate{
		{SessionID: "ses_a", ShortID: "amber-falcon-glides", LastActive: ago(time.Hour)},
		{SessionID: "ses_b", ShortID: "linen-orchard-chirps", LastActive: ago(time.Minute)},
	}
	got := Rank("Amber-Falcon-Glided", cands) // case-insensitive, 1 substitution
	if len(got) != 1 || got[0].SessionID != "ses_a" {
		t.Fatalf("Rank(Amber-Falcon-Glided) = %+v; want single ses_a", got)
	}
}

func TestRank_AliasPrefixAndToken(t *testing.T) {
	cands := []Candidate{
		{SessionID: "ses_1", Title: "Swift Heron — #1667 follow-up", LastActive: ago(time.Hour)},
		{SessionID: "ses_2", Title: "swift herons migration", LastActive: ago(30 * time.Minute)},
		{SessionID: "ses_3", Title: "Board reconciler", LastActive: ago(time.Second)},
	}
	// Prefix match outranks token-overlap even though ses_3 is newer.
	got := Rank("swift heron", cands)
	if len(got) < 2 {
		t.Fatalf("Rank(swift heron) = %+v; want both heron-titled sessions", got)
	}
	if got[0].SessionID != "ses_1" {
		t.Errorf("top = %s; want ses_1 (exact-prefix class beats token-overlap regardless of recency)", got[0].SessionID)
	}
}

func TestRank_SlugAlias(t *testing.T) {
	// The orchestrator's acceptance case: 'sunny-wolf' (a harness slug).
	cands := []Candidate{
		{SessionID: "ses_eb21f596affeBH60OiG9IhcM6L", Slug: "sunny-wolf", Title: "Worker: #1729 cross-workspace routine triggers", LastActive: ago(time.Hour)},
		{SessionID: "ses_x", Slug: "mighty-harbor", LastActive: ago(time.Minute)},
	}
	got := Rank("sunny-wolf", cands)
	if len(got) != 1 || got[0].SessionID != "ses_eb21f596affeBH60OiG9IhcM6L" {
		t.Fatalf("Rank(sunny-wolf) = %+v; want the sunny-wolf session", got)
	}
}

func TestRank_AliasCollisionSuggestsAllTies(t *testing.T) {
	// Two live sessions share the 'mighty-harbor' slug (this fleet's
	// own session list). Ambiguity NEVER auto-resolves — the caller
	// gets both, deterministically.
	cands := []Candidate{
		{SessionID: "ses_new", Slug: "mighty-harbor", LastActive: ago(time.Minute)},
		{SessionID: "ses_old", Slug: "mighty-harbor", LastActive: ago(48 * time.Hour)},
		{SessionID: "ses_other", Slug: "quiet-tiger", LastActive: ago(time.Second)},
	}
	got := Rank("mighty-harbor", cands)
	if len(got) != 2 {
		t.Fatalf("Rank(mighty-harbor) returned %d suggestions; want both colliding sessions", len(got))
	}
	if got[0].SessionID != "ses_new" || got[1].SessionID != "ses_old" {
		t.Errorf("collision order = %s,%s; want recency DESC (ses_new, ses_old)", got[0].SessionID, got[1].SessionID)
	}
}

func TestRank_TopThreeCap(t *testing.T) {
	bases := []string{
		"amber-falcon-glided", "amber-falcon-glider", "amber-falcon-gliders",
		"amber-falcon-gliding", "amber-falcon-glide",
	}
	var cands []Candidate
	for i := 0; i < 5; i++ {
		cands = append(cands, Candidate{
			SessionID:  fmt.Sprintf("ses_%d", i),
			ShortID:    bases[i],
			LastActive: ago(time.Duration(i) * time.Hour),
		})
	}
	got := Rank("amber-falcon-glides", cands) // distance 1 to all five
	if len(got) != MaxSuggestions {
		t.Fatalf("Rank returned %d; want cap %d", len(got), MaxSuggestions)
	}
	// Tiebreak: last-active DESC among equal-distance candidates —
	// ses_0 (age 0) is the newest of the five.
	if got[0].SessionID != "ses_0" {
		t.Errorf("top = %s; want ses_0 (newest of the distance-1 ties)", got[0].SessionID)
	}
}

func TestRank_Deterministic(t *testing.T) {
	cands := []Candidate{
		{SessionID: "ses_1", Title: "alpha beta", LastActive: ago(time.Hour)},
		{SessionID: "ses_2", Title: "alpha gamma", LastActive: ago(time.Hour)},
		{SessionID: "ses_3", Title: "alpha delta", LastActive: ago(time.Hour)},
	}
	first := Rank("alpha", cands)
	for i := 0; i < 20; i++ {
		again := Rank("alpha", cands)
		if len(first) != len(again) {
			t.Fatalf("nondeterministic length: %d vs %d", len(first), len(again))
		}
		for j := range first {
			if first[j].SessionID != again[j].SessionID {
				t.Fatalf("nondeterministic order at %d: %s vs %s", j, first[j].SessionID, again[j].SessionID)
			}
		}
	}
}

func TestRank_NothingScoresSilence(t *testing.T) {
	cands := []Candidate{{SessionID: "ses_1", Title: "totally unrelated", LastActive: ago(time.Hour)}}
	if got := Rank("zzzzzzzzzz", cands); len(got) != 0 {
		t.Errorf("Rank(zzzzzzzzzz) = %+v; want silence (≥3 edits is noise)", got)
	}
	if got := Rank("oc-qqq99999", cands); len(got) != 0 {
		t.Errorf("Rank(short-id far away) = %+v; want silence", got)
	}
}

func TestRank_SuggestionShape(t *testing.T) {
	when := ago(90 * time.Minute)
	got := Rank("ses_ed9183a46ffeRiNw2PddpL5dA", []Candidate{
		{SessionID: incidentLiveID, ShortID: "oc-ab12cd34", Title: "Worker: #1648 mobile viewport fix", Workspace: "ws-1", LastActive: when},
	})
	if len(got) != 1 {
		t.Fatalf("want 1 suggestion, got %d", len(got))
	}
	s := got[0]
	if s.Title != "Worker: #1648 mobile viewport fix" || s.ShortID != "oc-ab12cd34" || s.Workspace != "ws-1" {
		t.Errorf("payload fields wrong: %+v", s)
	}
	if s.LastActive == nil || !s.LastActive.Equal(when.Truncate(0)) && !s.LastActive.Equal(when) {
		t.Errorf("lastActive not carried: %+v", s.LastActive)
	}
	if s.SessionID != incidentLiveID {
		t.Errorf("sessionId missing: %+v", s)
	}
}

func TestRank_TitleOmittedWhenEmpty(t *testing.T) {
	got := Rank("ses_ed9183a46ffeRiNw2PddpL5dA", []Candidate{
		{SessionID: incidentLiveID, ShortID: "oc-ab12cd34"},
	})
	if got[0].Title != "" {
		t.Errorf("title must stay empty (omitempty in JSON)")
	}
}

func TestFold(t *testing.T) {
	if fold("  Swift Heron ") != "swift heron" {
		t.Error("fold = casefold+trim")
	}
	if strings.Contains(fold("Sunny-Wolf"), "-") == false {
		t.Error("fold must not strip separators (slug aliases match whole)")
	}
}
