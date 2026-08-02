// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package imagefactory

import (
	"strings"
	"testing"
)

func TestValidateSelection_HappyPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		sel  Selection
	}{
		{"single id", Selection{"ffmpeg"}},
		{"several ids", Selection{"ffmpeg", "python313", "libgl1"}},
		{"id with dots/dashes/underscores", Selection{"python@3.13", "go_1.22", "node-lts"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateSelection(tc.sel); err != nil {
				t.Fatalf("expected nil, got %v", err)
			}
		})
	}
}

func TestValidateSelection_UnhappyPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		sel     Selection
		wantSub string
	}{
		{"empty slice", Selection{}, "empty"},
		{"empty string id", Selection{"ffmpeg", ""}, "empty"},
		{"whitespace id", Selection{"ffmpeg bar"}, "charset"},
		{"shell metachar", Selection{"ffmpeg;rm"}, "charset"},
		{"uppercase rejected", Selection{"FFmpeg"}, "charset"},
		{"path separator", Selection{"a/b"}, "charset"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateSelection(tc.sel)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("expected error containing %q, got %q", tc.wantSub, err.Error())
			}
		})
	}
}

func TestHashSelection_DeterministicRegardlessOfOrder(t *testing.T) {
	t.Parallel()
	base := "bookworm"
	a := Selection{"ffmpeg", "python313", "libgl1"}
	b := Selection{"libgl1", "ffmpeg", "python313"}
	c := Selection{"python313", "libgl1", "ffmpeg"}

	ha, err := HashSelection(a, base)
	if err != nil {
		t.Fatalf("HashSelection a: %v", err)
	}
	hb, _ := HashSelection(b, base)
	hc, _ := HashSelection(c, base)
	if ha != hb || hb != hc {
		t.Fatalf("order-independent: a=%s b=%s c=%s", ha, hb, hc)
	}
}

func TestHashSelection_Stable(t *testing.T) {
	t.Parallel()
	sel := Selection{"ffmpeg", "python313"}
	h1, _ := HashSelection(sel, "bookworm")
	h2, _ := HashSelection(sel, "bookworm")
	if h1 != h2 {
		t.Fatalf("hash not stable: %s vs %s", h1, h2)
	}
}

func TestHashSelection_HasSPrefix(t *testing.T) {
	t.Parallel()
	h, _ := HashSelection(Selection{"ffmpeg"}, "bookworm")
	if !strings.HasPrefix(h, "s-") {
		t.Fatalf("hash must start with 's-', got %q", h)
	}
	// 16 hex chars after the prefix.
	hex := strings.TrimPrefix(h, "s-")
	if len(hex) != 16 {
		t.Fatalf("hash suffix must be 16 chars, got %d (%q)", len(hex), hex)
	}
}

func TestHashSelection_BaseNameInPreimage(t *testing.T) {
	t.Parallel()
	sel := Selection{"ffmpeg"}
	h1, _ := HashSelection(sel, "bookworm")
	h2, _ := HashSelection(sel, "trixie")
	if h1 == h2 {
		t.Fatal("different base names must produce different hashes")
	}
}

func TestHashSelection_ContentChangeChangesHash(t *testing.T) {
	t.Parallel()
	base := "bookworm"
	h1, _ := HashSelection(Selection{"ffmpeg"}, base)
	h2, _ := HashSelection(Selection{"ffmpeg", "libgl1"}, base)
	if h1 == h2 {
		t.Fatal("different selections must hash differently")
	}
}

func TestHashSelection_DedupEquivalent(t *testing.T) {
	t.Parallel()
	base := "bookworm"
	h1, _ := HashSelection(Selection{"ffmpeg", "ffmpeg"}, base)
	h2, _ := HashSelection(Selection{"ffmpeg"}, base)
	if h1 != h2 {
		t.Fatal("duplicate ids must canonicalize to the same hash")
	}
}

func TestHashSelection_LargeSelection(t *testing.T) {
	t.Parallel()
	sel := make(Selection, 100)
	for i := range sel {
		sel[i] = "ext" + string(rune('a'+i%26)) + string(rune('0'+i/26))
	}
	if _, err := HashSelection(sel, "bookworm"); err != nil {
		t.Fatalf("large selection: %v", err)
	}
}

func TestHashSelection_RejectsInvalidBeforeHashing(t *testing.T) {
	t.Parallel()
	if _, err := HashSelection(Selection{"bad;rm"}, "bookworm"); err == nil {
		t.Fatal("expected validation error before hashing")
	}
}

func TestHashSelection_RejectsEmpty(t *testing.T) {
	t.Parallel()
	if _, err := HashSelection(Selection{}, "bookworm"); err == nil {
		t.Fatal("expected error for empty selection")
	}
}

func TestHashSelection_RejectsWhitespaceInBase(t *testing.T) {
	t.Parallel()
	if _, err := HashSelection(Selection{"ffmpeg"}, "book worm"); err == nil {
		t.Fatal("expected error for whitespace in base name")
	}
}
