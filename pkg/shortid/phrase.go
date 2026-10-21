// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package shortid

import (
	cryptoRand "crypto/rand"
	"encoding/binary"
	"fmt"
	mathRand "math/rand"
	"regexp"
	"strings"
	"sync"
)

// mintRand selects words uniformly, guarded by mintMu: a *rand.Rand
// is NOT goroutine-safe, and the registration seam races legitimately
// (two replicas registering concurrently — CI's -race integration
// tier caught the unguarded version). The phrase space is not a
// secret (#1756: enumeration is ruled a non-issue — ids are
// references under authz, never capabilities); crypto/rand seeds the
// process PRNG once so the hot registration path pays no crypto
// overhead per mint.
//
//nolint:gosec // G404: the phrase space is not a secret (#1756: enumeration ruled a non-issue; ids are references under authz); crypto/rand seeds once so the hot path pays no crypto cost.
var (
	mintMu   sync.Mutex
	mintRand = mathRand.New(mathRand.NewSource(cryptoSeed()))
)

// pickFrom selects one word from a frozen slice under the mint lock.
func pickFrom(words []string) string {
	mintMu.Lock()
	defer mintMu.Unlock()
	return words[mintRand.Intn(len(words))]
}

func cryptoSeed() int64 {
	var b [8]byte
	if _, err := cryptoRand.Read(b[:]); err != nil {
		// Unreachable in practice; a fixed seed would be a mint
		// determinism bug, so panic loudly rather than degrade.
		panic("shortid: crypto/rand unavailable: " + err.Error())
	}
	//nolint:gosec // G115: any nonzero seed suffices; the high-bit
	// truncation only reduces the seed space, never correctness.
	return int64(binary.LittleEndian.Uint64(b[:]) >> 1)
}

// Slice mirrors of the frozen sets for uniform random selection.
var (
	adjectiveWords = sortedSlice(Adjectives)
	nounWords      = sortedSlice(Nouns)
	verbWords      = sortedSlice(Verbs)
)

// MintPhrase returns a fresh 3-word adjective-noun-verb phrase from
// the frozen lists — the #1756 canonical session short id. Collisions
// are NOT retried here; the registration seam's check-and-retry loop
// applies WithCounterSuffix deterministically. The error is always
// nil (crypto/rand failure is treated as a process-level fault at the
// seed source); the error slot is kept for signature stability only.
func MintPhrase() (string, error) {
	return pickFrom(adjectiveWords) + "-" +
		pickFrom(nounWords) + "-" +
		pickFrom(verbWords), nil
}

// WithCounterSuffix renders a collision's deterministic
// disambiguator: base-2, base-3, … (the unsuffixed form is the first
// mint; N < 2 clamps to 2).
func WithCounterSuffix(base string, n int) string {
	if n < 2 {
		n = 2
	}
	return fmt.Sprintf("%s-%d", base, n)
}

// legacyShortIDForm recognizes the #1745 hash-format short id
// (registry prefix + 4..12 alphanumerics). Clusters that ran main
// between the two rulings hold such rows: the form stays addressable
// as INPUT (with the deprecation notice) and is never minted again.
var legacyShortIDForm = regexp.MustCompile(`^(oc|cc|cx|pi)-[0-9a-zA-Z]{4,12}$`)

// IsLegacyShortIDForm classifies the #1745 hash short-id form.
func IsLegacyShortIDForm(s string) bool {
	return legacyShortIDForm.MatchString(strings.ToLower(strings.TrimSpace(s)))
}

// counterSuffix matches the deterministic collision suffix (-2, -10, …).
var counterSuffix = regexp.MustCompile(`^(?:[2-9]|[1-9][0-9]+)$`)
var lowerAlphaWord = regexp.MustCompile(`^[a-z]+$`)

// --- #1757: workspace phrase IDs -------------------------------------------
//
// Ruling: exactly 2 words + 1 digit (e.g. "amber-river-7"),
// FLEET-unique via check-and-retry. The mint shares the session
// wordlists' adjective+noun slots (already frozen, pairwise disjoint,
// and harness-slug-excluded) — the SHARED-LIST rule, pinned here and
// in phrase_test.go: the digit slot alone separates workspace phrases
// from the harness's 2-word no-digit slugs, and the 2-vs-3 word count
// separates them from session phrases, so all three namespaces are
// pairwise disjoint by shape in mixed text.
//
// There is no counter-suffix slot (the digit IS the 10-way
// disambiguator): a fleet collision re-mints a fresh draw, bounded by
// the same retry-loop discipline as the session mint.

// workspaceDigitAlphabet is the mint's digit slot (0-9).
const workspaceDigitAlphabet = "0123456789"

// MintWorkspacePhrase returns a fresh 2-word + 1-digit phrase under
// the mint lock (see mintRand — same thread-safety discipline). The
// draw cannot fail (r1: the dead error return was removed); the
// bounded check-and-retry that CAN fail lives in the persistence
// seam (EnsureWorkspacePhraseRegistered).
func MintWorkspacePhrase() string {
	mintMu.Lock()
	defer mintMu.Unlock()
	return adjectiveWords[mintRand.Intn(len(adjectiveWords))] + "-" +
		nounWords[mintRand.Intn(len(nounWords))] + "-" +
		string(workspaceDigitAlphabet[mintRand.Intn(len(workspaceDigitAlphabet))])
}

// workspacePhraseForm is the #1757 shape: exactly two lowercase word
// tokens + one trailing digit, case-insensitive on input.
var workspacePhraseForm = regexp.MustCompile(`^[a-z]+-[a-z]+-[0-9]$`)

// IsWorkspacePhraseForm reports whether s carries the #1757
// workspace-phrase shape. Disjoint by construction from session
// phrases (IsShortIDForm: 3 words, no digit slot) and harness slugs
// (2 words, no digit).
func IsWorkspacePhraseForm(s string) bool {
	return workspacePhraseForm.MatchString(Normalize(s))
}
