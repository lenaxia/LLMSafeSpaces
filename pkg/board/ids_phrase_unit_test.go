// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"strings"
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/shortid"
)

// --- #1831: word-phrase board ids (session-ID playbook) ----------------------
//
// The vocabulary flip: prj-/epc-/tsk- keep their prefixes (the closed
// kind map), the random 8-char suffix retires, and the mint emits the
// #1756 3-word phrase (adjective-noun-verb, frozen lists) —
// tsk-amber-falcon-glides. Legacy random rows stay addressable for one
// release (dual-accept window), marked deprecated on read.

func TestMintPhraseIDShape(t *testing.T) {
	for _, prefix := range []string{PrefixProject, PrefixEpic, PrefixTask} {
		id, err := mintBoardID(prefix)
		if err != nil {
			t.Fatalf("mint %s: %v", prefix, err)
		}
		if !IsPhraseIDForm(id) {
			t.Errorf("mint %q produced out-of-shape id %q", prefix, id)
		}
		if !strings.HasPrefix(id, prefix) {
			t.Errorf("mint %q lost its prefix: %q", prefix, id)
		}
		// The words come from the frozen session lists (the shared
		// generator — same vocabulary, not a board fork).
		words := strings.Split(strings.TrimPrefix(id, prefix), "-")
		if !shortid.Adjectives[words[0]] {
			t.Errorf("slot 1 word %q not in the frozen adjective list", words[0])
		}
		if !shortid.Nouns[words[1]] {
			t.Errorf("slot 2 word %q not in the frozen noun list", words[1])
		}
		if !shortid.Verbs[words[2]] {
			t.Errorf("slot 3 word %q not in the frozen verb list", words[2])
		}
	}
	if _, err := mintBoardID("sty-"); err == nil {
		t.Error("the closed prefix map rejects unknown prefixes (stories do not exist, ruling R1)")
	}
}

func TestMintPhraseIDNeverRandom(t *testing.T) {
	// The random 8-char form is never minted again (#1831 emission
	// flip): across a draw, every id carries the phrase shape.
	for i := 0; i < 200; i++ {
		id, err := mintBoardID(PrefixTask)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if IsLegacyBoardIDForm(id) {
			t.Fatalf("mint emitted the retired random form %q", id)
		}
	}
}

func TestPhraseIDFormClassification(t *testing.T) {
	yes := []string{
		"tsk-amber-falcon-glides",
		"prj-meek-pony-rides",
		"epc-crimson-falcon-glides",
		"TSK-Amber-Falcon-Glides",   // case-insensitive on input
		"tsk-amber-falcon-glides-2", // counter suffix (mint collision)
		"tsk-amber-falcon-glides-10",
	}
	for _, s := range yes {
		if !IsPhraseIDForm(s) {
			t.Errorf("IsPhraseIDForm(%q) = false, want true", s)
		}
	}
	no := []string{
		"tsk-vlfuvcil",              // legacy random form
		"tsk-amber-falcon",          // partial phrase — categorically invalid
		"tsk-amber-falcon-glides-x", // non-counter 5th token
		"amber-falcon-glides",       // bare session phrase — the session namespace
		"amber-river-7",             // workspace phrase — the workspace namespace
		"tsk-",                      // bare prefix
		"",
	}
	for _, s := range no {
		if IsPhraseIDForm(s) {
			t.Errorf("IsPhraseIDForm(%q) = true, want false", s)
		}
	}
}

// TestPhraseNamespacesDisjoint pins the shape separation against the
// session/workspace classifiers: a board phrase id never classifies as
// a session phrase (or workspace phrase) and vice versa, so mixed text
// (charters, transcripts) stays unambiguous.
func TestPhraseNamespacesDisjoint(t *testing.T) {
	boardIDs := []string{
		"tsk-amber-falcon-glides", "prj-meek-pony-rides-2", "epc-crimson-falcon-glides",
	}
	for _, id := range boardIDs {
		if shortid.IsShortIDForm(id) {
			t.Errorf("board id %q classifies as a session phrase — namespaces overlap", id)
		}
		if shortid.IsWorkspacePhraseForm(id) {
			t.Errorf("board id %q classifies as a workspace phrase — namespaces overlap", id)
		}
	}
	// Counter-suffixed board ids vs the session counter form: a session
	// phrase with counter is 4 bare tokens; the board form carries the
	// kind prefix and is never bare.
	if IsPhraseIDForm("amber-falcon-glides-2") {
		t.Error("bare session phrase (with counter) must not classify as a board id")
	}
}

func TestLegacyBoardIDFormClassification(t *testing.T) {
	yes := []string{"tsk-vlfuvcil", "prj-1byh85dn", "epc-mojaj89f", "TSK-VLFUVCIL"}
	for _, s := range yes {
		if !IsLegacyBoardIDForm(s) {
			t.Errorf("IsLegacyBoardIDForm(%q) = false, want true", s)
		}
		if !IsDeprecatedBoardID(s) {
			t.Errorf("IsDeprecatedBoardID(%q) = false, want true", s)
		}
	}
	no := []string{"tsk-amber-falcon-glides", "amber-falcon-glides", "tsk-short", "tsk-aaaaaaaaaaa"}
	for _, s := range no {
		if IsLegacyBoardIDForm(s) {
			t.Errorf("IsLegacyBoardIDForm(%q) = true, want false", s)
		}
		if IsDeprecatedBoardID(s) {
			t.Errorf("IsDeprecatedBoardID(%q) = true, want false", s)
		}
	}
	// A phrase id — minted or counter-suffixed — is never deprecated.
	if IsDeprecatedBoardID("tsk-amber-falcon-glides-2") {
		t.Error("counter-suffixed phrase classified deprecated")
	}
}
