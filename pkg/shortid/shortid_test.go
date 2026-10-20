// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package shortid

import (
	"strings"
	"testing"
)

// mintedShape pins ruling 2's format: registry prefix + exactly 8
// chars over [0-9a-z], lowercase, no other separator.
func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"Amber-Falcon-Glides":   "amber-falcon-glides",
		" amber-falcon-glides ": "amber-falcon-glides",
		"Swift-Heron-Glides-2":  "swift-heron-glides-2",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q; want %q", in, got, want)
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

func TestIsSeamIDForm_Table(t *testing.T) {
	// The degraded-lookup write guard's charset contract (review r2:
	// a tightening on '_' or the 128-byte boundary would otherwise
	// pass every suite while silently shifting which refs 503).
	yes := []string{
		"ses-two", "ses_ed9183a46ffeRiNw2PddpL5dAA", "oc-ab12cd34",
		"a", strings.Repeat("a", 128), "a-b_c9",
	}
	no := []string{
		"", "has space", "swift heron follow-ups", "dot.id", "col:on",
		"slash/id", "trailing ", " leading", strings.Repeat("a", 129),
	}
	for _, s := range yes {
		if !IsSeamIDForm(s) {
			t.Errorf("IsSeamIDForm(%q) = false; want true", s)
		}
	}
	for _, s := range no {
		if IsSeamIDForm(s) {
			t.Errorf("IsSeamIDForm(%q) = true; want false", s)
		}
	}
}
