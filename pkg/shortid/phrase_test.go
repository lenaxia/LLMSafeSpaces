// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package shortid

import (
	mathRand "math/rand"
	"regexp"
	"strings"
	"testing"
)

// The #1756 final format ruling: session short IDs are 3-word
// grammatical phrases (adj-noun-verb) from the frozen curated lists —
// NO hash suffix, NO prefix, NO checksum word; a deterministic counter
// suffix arbitrates mint collisions. Exact word count: partial IDs are
// categorically invalid.

var phraseShape = regexp.MustCompile(`^[a-z]+-[a-z]+-[a-z]+$`)

func TestMintPhrase_FormatPins(t *testing.T) {
	for i := 0; i < 200; i++ {
		id, err := MintPhrase()
		if err != nil {
			t.Fatalf("MintPhrase: %v", err)
		}
		if !phraseShape.MatchString(id) {
			t.Errorf("MintPhrase() = %q: not adj-noun-verb (3 lowercase words, hyphen-joined)", id)
		}
		words := strings.Split(id, "-")
		if len(words) != 3 {
			t.Fatalf("exact word count violated: %q", id)
		}
		if !Adjectives[words[0]] {
			t.Errorf("word 0 %q not in the frozen adjective list", words[0])
		}
		if !Nouns[words[1]] {
			t.Errorf("word 1 %q not in the frozen noun list", words[1])
		}
		if !Verbs[words[2]] {
			t.Errorf("word 2 %q not in the frozen verb list", words[2])
		}
	}
}

// TestMintPhrase_HighEntropyNotSequential — deterministic (#1794):
// the pre-fix design drew 500 UNSEEDED samples from the 160x192x64 =
// 1,966,080 phrase space, a ~6.2% birthday-collision probability per
// run (1 - exp(-500*499/(2*1,966,080))) — a 1-in-16 CI redden with no
// defect behind it. The test now fixes the mint PRNG's seed for the
// draw window (test-only reseed of the package var, re-armed with a
// fresh crypto seed on cleanup), which makes the window deterministic
// AND lets the entropy guard get STRONGER: per-slot distribution
// bounds catch a degraded generator (e.g. one drawing verbs from a
// narrowed slice) that the old duplicate-only check could not — under
// the pinned seed a narrowed generator rarely composes the same full
// phrase twice in 500 draws. The production path stays crypto-seeded
// (#1756: enumeration
// is a ruled non-issue; ids are references under authz).
func TestMintPhrase_HighEntropyNotSequential(t *testing.T) {
	mintMu.Lock()
	mintRand = mathRand.New(mathRand.NewSource(20261020))
	mintMu.Unlock()
	t.Cleanup(func() {
		mintMu.Lock()
		mintRand = mathRand.New(mathRand.NewSource(cryptoSeed()))
		mintMu.Unlock()
	})

	seen := map[string]bool{}
	adjs, nouns, verbs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i := 0; i < 500; i++ {
		id, _ := MintPhrase()
		if seen[id] {
			t.Fatalf("duplicate mint %q within the seeded 500-draw window — random selection broken", id)
		}
		seen[id] = true
		words := strings.Split(id, "-")
		adjs[words[0]] = true
		nouns[words[1]] = true
		verbs[words[2]] = true
	}
	// Entropy bounds for the seeded window: uniform draws over the
	// frozen lists populate most slots — the expected distinct count
	// for 500 uniform draws over N slots is N(1-(1-1/N)^500):
	// 153.0/160 adjectives, 177.9/192 nouns, 64.0/64 verbs (the
	// pinned window's actuals land on these: 153/174/64). The bounds
	// sit far below expectation so the fixed window can never graze
	// them, and they detect a generator drawing from under ~55% of a
	// list (per-list floors: verbs <=35/64 ≈ 55%, adjectives ≈ 62%,
	// nouns ≈ 63% — narrowings above the floor, e.g. 38 of 64 verbs,
	// pass); strictly stronger than the old duplicate-only check,
	// which caught only near-total collapse.
	if len(adjs) < 100 {
		t.Errorf("adjective entropy low: %d distinct in 500 draws (uniform expectation 153.0)", len(adjs))
	}
	if len(nouns) < 120 {
		t.Errorf("noun entropy low: %d distinct in 500 draws (uniform expectation 177.9)", len(nouns))
	}
	if len(verbs) < 36 {
		t.Errorf("verb entropy low: %d distinct in 500 draws (uniform expectation 64.0)", len(verbs))
	}
}

func TestCounterSuffix_DeterministicCollisionForm(t *testing.T) {
	// "deterministic counter suffix on collision": the suffix is a
	// plain -N (N >= 2), never a re-rolled random phrase-fragment.
	if got := WithCounterSuffix("swift-heron-glides", 2); got != "swift-heron-glides-2" {
		t.Fatalf("WithCounterSuffix(base, 2) = %q", got)
	}
	if got := WithCounterSuffix("swift-heron-glides", 10); got != "swift-heron-glides-10" {
		t.Fatalf("WithCounterSuffix(base, 10) = %q", got)
	}
}

func TestIsShortIDForm_PhraseShapes(t *testing.T) {
	yes := []string{
		"swift-heron-glides",  // canonical
		"calm-river-drifts-2", // counter-suffixed collision form
		"Swift-Heron-Glides",  // case-insensitive resolution
		"bold-stone-hums-12",
	}
	no := []string{
		"swift-heron",                    // 2 words: partial ID, categorically invalid
		"swift",                          // 1 word: partial
		"swift-heron-glides-mist",        // 4 WORDS (non-counter): invalid
		"swift-heron-glides-2x",          // malformed counter
		"amber-river-7",                  // workspace-phrase shape (2 words + digit)
		"sunny-wolf",                     // harness slug shape (2 words, no digit)
		"oc-ab12cd34",                    // legacy #1745 short form — INPUT-only class, never minted
		"ses_ed9183a46ffeRiNw2PddpL5dAA", // full id
		"",
	}
	for _, s := range yes {
		if !IsShortIDForm(s) {
			t.Errorf("IsShortIDForm(%q) = false; want true", s)
		}
	}
	for _, s := range no {
		if IsShortIDForm(s) {
			t.Errorf("IsShortIDForm(%q) = true; want false", s)
		}
	}
}

func TestIsLegacyShortIDForm(t *testing.T) {
	// The #1745 hash format exists on clusters that ran main between
	// the two rulings: those rows must stay addressable as INPUT
	// (deprecated) — but the form is never minted again and is NOT a
	// session phrase.
	if !IsLegacyShortIDForm("oc-ab12cd34") || !IsLegacyShortIDForm("CC-00000000") {
		t.Error("legacy prefix+hash form must classify for input acceptance")
	}
	if IsLegacyShortIDForm("swift-heron-glides") || IsLegacyShortIDForm("amber-river-7") {
		t.Error("phrase shapes are not legacy short ids")
	}
}

// Namespace disjointness, PINNED (the in-lane ruling): session phrases
// (3 words, no digits), workspace phrases (2 words + 1 trailing digit,
// #1757), and the harness's adj-noun slugs (2 words, no digits) are
// pairwise disjoint BY SHAPE — plus the frozen lists exclude the
// harness's observed slug vocabulary so even word-level collisions
// with live slugs cannot compose into a session phrase.
func TestNamespaceDisjointness(t *testing.T) {
	harnessSlugWords := []string{
		// observed live slugs (adjectives): opencode's own generator
		"sunny", "mighty", "curious", "brave", "hidden", "lucky", "quiet",
		"crisp", "glowing", "jolly", "kind", "playful", "gentle", "nimble",
		"tidy", "stellar", "neon", "witty", "eager", "silent", "shiny",
		"cosmic", "proud", "clever", "swift", "bright", "calm", "bold",
		"keen", "warm", "cool", "sharp",
		// observed live slugs (nouns)
		"wolf", "heron", "harbor", "otter", "planet", "cabin", "moon",
		"star", "tiger", "canyon", "sailor", "orchid", "meadow", "island",
		"lagoon", "cactus", "comet", "knight", "falcon", "circuit",
		"river", "stone", "cloud", "wind", "flame", "leaf", "wave",
		"spark",
	}
	for _, w := range harnessSlugWords {
		if Adjectives[w] {
			t.Errorf("adjective list must exclude the harness slug word %q", w)
		}
		if Nouns[w] {
			t.Errorf("noun list must exclude the harness slug word %q", w)
		}
		if Verbs[w] {
			t.Errorf("verb list must exclude the harness slug word %q", w)
		}
	}
}

// The frozen lists are FROZEN: lengths pinned so silent edits to the
// curated sets fail loudly (extending requires updating the pin —
// deliberate, reviewed, and never mid-release).
func TestWordlists_Frozen(t *testing.T) {
	if len(Adjectives) != 160 {
		t.Errorf("adjective list size %d; frozen pin says 160", len(Adjectives))
	}
	if len(Nouns) != 192 {
		t.Errorf("noun list size %d; frozen pin says 192", len(Nouns))
	}
	if len(Verbs) != 64 {
		t.Errorf("verb list size %d; frozen pin says 64", len(Verbs))
	}
	// No overlaps between the three lists (a word in two lists makes
	// phrase parsing ambiguous for humans).
	for w := range Adjectives {
		if Nouns[w] {
			t.Errorf("%q in both adjective and noun lists", w)
		}
		if Verbs[w] {
			t.Errorf("%q in both adjective and verb lists", w)
		}
	}
	for w := range Nouns {
		if Verbs[w] {
			t.Errorf("%q in both noun and verb lists", w)
		}
	}
	// Every word: lowercase ASCII letters only, <= 9 chars (LLM
	// transcription ergonomics — the whole point of the format).
	check := func(set map[string]bool, name string) {
		for w := range set {
			if len(w) == 0 || len(w) > 9 {
				t.Errorf("%s list word %q: length outside 1..9", name, w)
			}
			for _, c := range w {
				if c < 'a' || c > 'z' {
					t.Errorf("%s list word %q: non-lowercase-ASCII rune %q", name, w, c)
				}
			}
		}
	}
	check(Adjectives, "adjective")
	check(Nouns, "noun")
	check(Verbs, "verb")
}

// --- #1757 workspace phrase pins -------------------------------------------

func TestMintWorkspacePhrase_FormatPins(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 300; i++ {
		id := MintWorkspacePhrase()
		if !IsWorkspacePhraseForm(id) {
			t.Fatalf("MintWorkspacePhrase() = %q: not 2-words+1-digit", id)
		}
		words := strings.Split(id, "-")
		if !Adjectives[words[0]] || !Nouns[words[1]] {
			t.Fatalf("%q: slots must come from the frozen lists", id)
		}
		if len(words[2]) != 1 || words[2][0] < '0' || words[2][0] > '9' {
			t.Fatalf("%q: third token must be a single digit", id)
		}
		seen[id] = true
	}
	// The workspace space (160×192×10 ≈ 307k) admits birthday collisions
	// in a few hundred draws — uniqueness is the fleet-unique INDEX's
	// job (check-and-retry at the seam), not the mint's. The pin is
	// randomness: nearly all draws distinct.
	if len(seen) < 280 {
		t.Fatalf("only %d distinct phrases in 300 draws — selection not uniform", len(seen))
	}
}

// Namespace disjointness for the third ID kind: workspace phrases
// never classify as session phrases or harness slugs, and vice versa.
func TestWorkspacePhrase_NamespaceDisjointness(t *testing.T) {
	ws := []string{"amber-river-7", "linen-orchard-3"}
	sess := []string{"amber-falcon-glides", "calm-river-drifts-2"}
	slugs := []string{"sunny-wolf", "mighty-harbor"}
	for _, s := range ws {
		if !IsWorkspacePhraseForm(s) {
			t.Errorf("IsWorkspacePhraseForm(%q) = false", s)
		}
		if IsShortIDForm(s) {
			t.Errorf("workspace phrase %q must not classify as a session phrase", s)
		}
	}
	for _, s := range sess {
		if IsWorkspacePhraseForm(s) {
			t.Errorf("session phrase %q must not classify as a workspace phrase", s)
		}
	}
	for _, s := range slugs {
		if IsWorkspacePhraseForm(s) || IsShortIDForm(s) {
			t.Errorf("harness slug %q must classify as neither phrase kind", s)
		}
	}
}

// The lazy-backfill read contract: a NULL phrase_id mints on read.
// (Pinned at the sqlmock tier in the database package.)
