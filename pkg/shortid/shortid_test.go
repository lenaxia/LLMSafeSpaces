// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package shortid

import (
	"regexp"
	"strings"
	"testing"
)

// mintedShape pins ruling 2's format: registry prefix + exactly 8
// chars over [0-9a-z], lowercase, no other separator.
var mintedShape = regexp.MustCompile(`^(oc|cc|cx|pi)-[0-9a-z]{8}$`)

func TestMint_FormatPins(t *testing.T) {
	for _, harness := range []string{"opencode", "claude-code", "codex", "pi", ""} {
		id, err := Mint(harness)
		if err != nil {
			t.Fatalf("Mint(%q): %v", harness, err)
		}
		if !mintedShape.MatchString(id) {
			t.Errorf("Mint(%q) = %q: not prefix+8 chars over [0-9a-z]", harness, id)
		}
		wantPrefix, _ := PrefixForHarness(harness)
		if wantPrefix == "" {
			wantPrefix = "oc-"
		}
		if !strings.HasPrefix(id, wantPrefix) {
			t.Errorf("Mint(%q) = %q: want prefix %q", harness, id, wantPrefix)
		}
	}
}

func TestMint_UnknownHarnessDefaultsToOpencode(t *testing.T) {
	// Design 0066 §4.2: today every workspace is opencode; an unknown
	// or empty harness type must mint under oc-, not fail.
	p, ok := PrefixForHarness("")
	if p != "oc-" || ok {
		t.Fatalf("PrefixForHarness(\"\") = %q,%v; want \"oc-\",false", p, ok)
	}
	p, ok = PrefixForHarness("some-future-harness")
	if p != "oc-" || ok {
		t.Fatalf("unknown harness = %q,%v; want \"oc-\",false", p, ok)
	}
}

func TestMint_EntropyNotDeterministic(t *testing.T) {
	// Ruling 6: no deterministic derivation — two mints must differ
	// (overwhelmingly; a collision here would mean the RNG is broken).
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := Mint("opencode")
		if err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatalf("duplicate mint %q within 100 draws", id)
		}
		seen[id] = true
	}
}

func TestMint_NoHarnessIDDerivation(t *testing.T) {
	// Ruling 6's spirit: the mint takes NO session input at all — the
	// signature is (harnessType) only. This is a compile-time property;
	// the pin documents it (a hash of ses_… would preserve the coupling).
	id1, _ := Mint("opencode")
	id2, _ := Mint("opencode")
	if id1 == id2 {
		t.Errorf("mint derived identical ids; mint must be random")
	}
}

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"OC-AB12CD34":   "oc-ab12cd34",
		" oc-ab12cd34 ": "oc-ab12cd34",
		"OC-ab12cd34":   "oc-ab12cd34",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestIsShortIDForm(t *testing.T) {
	yes := []string{
		"oc-ab12cd34", "OC-AB12CD34", "cc-00000000", "pi-zz999999",
		"oc-ab12cd3", // 7 chars: truncated but id-shaped → scoring, not validation
	}
	no := []string{
		"oc-ab",       // body < 4
		"xx-ab12cd34", // unknown prefix
		"sunny-wolf",  // alias-shaped (adjective-noun name, not a registry prefix)
		"ses_ed9183a46ffeRiNw2PddpL5dAA",
		"worker: #1663 follow-up",
		"", "oc-", "oc-ab12cd34567890",
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

func TestIsLegacyIDForm(t *testing.T) {
	if !IsLegacyIDForm("ses_ed9183a46ffeRiNw2PddpL5dA") { // #1663's truncated id
		t.Error("truncated ses_ id must classify as legacy-id form")
	}
	if IsLegacyIDForm("oc-ab12cd34") || IsLegacyIDForm("sunny-wolf") {
		t.Error("non-ses_ refs are not legacy-id form")
	}
}

func TestIsIDForm(t *testing.T) {
	if !IsIDForm("oc-ab12cd34") || !IsIDForm("ses_abc") {
		t.Error("both id families must be id-form")
	}
	if IsIDForm("swift heron") || IsIDForm("sunny-wolf") {
		t.Error("alias-shaped refs must not be id-form")
	}
}
