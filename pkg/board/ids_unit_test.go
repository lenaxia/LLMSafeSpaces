// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestMintShortIDShape(t *testing.T) {
	for _, prefix := range []string{PrefixProject, PrefixEpic, PrefixTask} {
		id, err := mintShortID(prefix)
		if err != nil {
			t.Fatalf("mint %s: %v", prefix, err)
		}
		if !regexp.MustCompile(`^` + regexp.QuoteMeta(prefix) + `[0-9a-z]{8}$`).MatchString(id) {
			t.Errorf("mint %q produced out-of-shape %q", prefix, id)
		}
	}
	if _, err := mintShortID("sty-"); err == nil {
		t.Error("the closed prefix map rejects unknown prefixes (stories do not exist, ruling R1)")
	}
}

func TestMintShortIDUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		id, err := mintShortID(PrefixTask)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if seen[id] {
			t.Fatalf("collision at %d draws — alphabet/entropy broken", i)
		}
		seen[id] = true
	}
}

func TestSuggestTop3(t *testing.T) {
	candidates := []string{
		"tsk-aaaa1111", "tsk-aaaa1112", "tsk-aaaa1113", "tsk-aaaa1114",
		"tsk-zzzzzzzz", "epc-bbbb2222",
	}
	// One edit away: best-first, capped at 3.
	got := suggestTop3("tsk-aaaa1119", candidates)
	if len(got) != 3 {
		t.Fatalf("expected 3 suggestions, got %v", got)
	}
	for _, g := range got {
		if !strings.HasPrefix(g, "tsk-aaaa111") {
			t.Errorf("suggestion %q is not among the nearest", g)
		}
	}
	// Out of bound (distance > 2): no suggestions.
	if got := suggestTop3("tsk-99999999", candidates); len(got) != 0 {
		t.Errorf("bounded at Levenshtein <= 2, got %v", got)
	}
	// Exact match distance 0.
	if got := suggestTop3("tsk-zzzzzzzz", candidates); len(got) == 0 || got[0] != "tsk-zzzzzzzz" {
		t.Errorf("exact candidate should surface first, got %v", got)
	}
}

func TestEditDistanceBounded(t *testing.T) {
	cases := []struct {
		a, b  string
		bound int
		want  int
	}{
		{"abc", "abc", 2, 0},
		{"abc", "abd", 2, 1},
		{"abc", "cbc", 2, 1},
		{"abc", "abcde", 2, 2},
		{"abc", "abcdef", 2, 3}, // exceeds bound → bound+1
		{"", "ab", 1, 2},
		{"ab", "", 1, 2},
		{"kitten", "sitting", 3, 3},
		{"kitten", "sitting", 2, 3},
	}
	for _, c := range cases {
		if got := editDistanceBounded(c.a, c.b, c.bound); got != c.want {
			t.Errorf("editDistanceBounded(%q,%q,%d) = %d, want %d", c.a, c.b, c.bound, got, c.want)
		}
	}
}

func TestDefaultEngineConfigR6(t *testing.T) {
	cfg := DefaultEngineConfig()
	if cfg.AttemptsCap != 3 {
		t.Errorf("ruling R6: attempts-cap default is 3, got %d", cfg.AttemptsCap)
	}
	if cfg.LeaseDuration != 15*time.Minute {
		t.Errorf("lease default 15m, got %v", cfg.LeaseDuration)
	}
	if cfg.DRRQuantum != 1 {
		t.Errorf("DRR quantum default 1, got %d", cfg.DRRQuantum)
	}
}
