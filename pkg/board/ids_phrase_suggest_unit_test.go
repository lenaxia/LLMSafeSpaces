// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"strings"
	"testing"
)

// --- #1831: did-you-mean over WORDS for phrase ids ---------------------------

// TestSuggestWordDistance is the #1831 acceptance case: a one-WORD
// drift (the class opaque ids made undetectable — vlfuvcil vs
// vlfuvciI) must suggest; character distance is out of bound there.
func TestSuggestWordDistance(t *testing.T) {
	candidates := []string{
		"tsk-meek-poncho-rides",
		"tsk-amber-falcon-glides",
		"tsk-crimson-falcon-glides",
	}
	got := suggestTop3("tsk-meek-pony-rides", candidates)
	if len(got) == 0 || got[0] != "tsk-meek-poncho-rides" {
		t.Errorf("one-word drift must suggest the phrase id (char distance 4 > bound 2), got %v", got)
	}
	// Counter-suffixed variant: the base phrase is one word-token away.
	got = suggestTop3("tsk-meek-poncho-ride", candidates)
	if len(got) == 0 || got[0] != "tsk-meek-poncho-rides" {
		t.Errorf("within-word typo must suggest (word substitution distance 1), got %v", got)
	}
	// A two-word drift stays inside the bound; three words out.
	if got := suggestTop3("tsk-amber-falcon-soars", candidates); len(got) == 0 {
		t.Error("two-word drift (bound 2) should still suggest")
	}
	if got := suggestTop3("tsk-ebony-lantern-soars", candidates); len(got) != 0 {
		t.Errorf("three-word drift must not suggest, got %v", got)
	}
}

// TestSuggestLegacyStillCharacterRanked: during the dual-accept window
// the legacy random ids rank by CHARACTER distance (word distance
// degenerates on one random token — every same-prefix id would be
// distance 1 and the suggestion set becomes noise).
func TestSuggestLegacyStillCharacterRanked(t *testing.T) {
	candidates := []string{
		"tsk-aaaa1111", "tsk-aaaa1112", "tsk-aaaa1113", "tsk-zzzzzzzz",
	}
	got := suggestTop3("tsk-aaaa1119", candidates)
	if len(got) != 3 {
		t.Fatalf("legacy near-miss keeps the character ranker (top-3), got %v", got)
	}
	for _, g := range got {
		if !strings.HasPrefix(g, "tsk-aaaa111") {
			t.Errorf("suggestion %q is not among the nearest", g)
		}
	}
	if got := suggestTop3("tsk-99999999", candidates); len(got) != 0 {
		t.Errorf("bounded at edit distance 2, got %v", got)
	}
	// A legacy needle against phrase candidates: character ranker only
	// (nothing meaningful to bridge the vocabularies) — no crash, no
	// noise suggestions.
	if got := suggestTop3("tsk-aaaa1119", []string{"tsk-amber-falcon-glides"}); len(got) != 0 {
		t.Errorf("cross-vocabulary suggestion noise: %v", got)
	}
}

// TestSuggestMixedFleet: one scope holding BOTH vocabularies — each
// needle suggests within its own vocabulary, never across.
func TestSuggestMixedFleet(t *testing.T) {
	candidates := []string{
		"tsk-meek-poncho-rides", // phrase fleet
		"tsk-aaaa1111",          // legacy fleet
		"tsk-aaaa1112",
	}
	got := suggestTop3("tsk-meek-pony-rides", candidates)
	if len(got) != 1 || got[0] != "tsk-meek-poncho-rides" {
		t.Errorf("phrase needle must suggest only the phrase candidate, got %v", got)
	}
	got = suggestTop3("tsk-aaaa1113", candidates)
	if len(got) != 2 {
		t.Fatalf("legacy needle must keep its character suggestions, got %v", got)
	}
}
