// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The salvage-prefer terminal's UNIT tier (the seam's pure halves; the
// policy pins live at the integration tier,
// salvage_terminal_integration_test.go):
//
//   - DifferFrom is the loop bound — identical {Branch, Head, OpenPR}
//     to the recorded pointer ⇒ NOT new salvage; any differing fact or
//     a first sighting ⇒ salvage.
//   - Pointer composes the S5 record the depose writes (facts + the
//     exhaustion-salvage State prose, Uncommitted riding State).
//   - WithSalvageSource installs; absent = the inert contract (the
//     exhaustion sites never probe).

func TestSalvageSignals_DifferFrom(t *testing.T) {
	ptr := &SalvagePointer{Branch: "board/wait-salvage", Head: "deadbeef", PR: "2064", State: "prior round"}
	same := &SalvageSignals{Branch: "board/wait-salvage", Head: "deadbeef", OpenPR: "2064", Uncommitted: true}

	// First sighting: nil or empty pointer ⇒ anything salvageable differs.
	require.True(t, same.DifferFrom(nil), "first sighting (nil pointer) is new salvage")
	require.True(t, same.DifferFrom(&SalvagePointer{}), "first sighting (empty pointer) is new salvage")
	// The loop bound: identical triple ⇒ NOT new salvage — even with
	// Uncommitted true (Uncommitted is a presence fact, not identity).
	require.False(t, same.DifferFrom(ptr), "identical {Branch, Head, OpenPR} ⇒ no NEW salvage (the loop bound)")

	for name, sig := range map[string]*SalvageSignals{
		"new head":   {Branch: "board/wait-salvage", Head: "cafebabe", OpenPR: "2064"},
		"new pr":     {Branch: "board/wait-salvage", Head: "deadbeef", OpenPR: "2100"},
		"new branch": {Branch: "board/other-lane", Head: "deadbeef", OpenPR: "2064"},
	} {
		require.True(t, sig.DifferFrom(ptr), "%s ⇒ new salvage", name)
	}

	// The pointer's Note/State prose never participates in identity.
	require.False(t, same.DifferFrom(&SalvagePointer{
		Branch: "board/wait-salvage", Head: "deadbeef", PR: "2064",
		State: "a completely different handoff note", Note: "and note",
	}), "identity is the triple {Branch, Head, PR} — State/Note never arbitrate")
}

func TestSalvageSignals_Pointer(t *testing.T) {
	s := &SalvageSignals{Branch: "board/wait-salvage", Head: "deadbeef", OpenPR: "2064"}
	p := s.Pointer(3)
	require.Equal(t, "board/wait-salvage", p.Branch)
	require.Equal(t, "deadbeef", p.Head)
	require.Equal(t, "2064", p.PR)
	require.NotEmpty(t, p.State, "the exhaustion-salvage State prose rides the pointer")
	require.Contains(t, p.State, "exhaustion-salvage")
	require.Contains(t, p.State, "3", "the attempts-at-cap count rides the State prose")
	require.NotContains(t, p.State, "uncommitted", "Uncommitted=false leaves the prose silent on it")
	require.NoError(t, validateSalvagePointer(p), "the composed pointer passes the depose validation")

	// Uncommitted rides State (the pointer has no field of its own).
	p2 := (&SalvageSignals{Uncommitted: true}).Pointer(3)
	require.Contains(t, p2.State, "uncommitted", "Uncommitted=true states itself in the prose")
	require.NoError(t, validateSalvagePointer(p2))

	// The all-empty degradation: a source that answers non-nil with no
	// recordable facts reads as EMPTY signals — salvageDeposeAtCap
	// degrades that to the ordinary terminal (the composed pointer
	// itself always carries State prose, so the guard keys on the
	// signals, never the pointer).
	require.True(t, (&SalvageSignals{}).Empty())
	require.False(t, (&SalvageSignals{Uncommitted: true}).Empty(), "Uncommitted alone is a fact")
}

func TestWithSalvageSource_Installs(t *testing.T) {
	src := stubSalvageSource{}
	s := NewService(nil, EngineConfig{}, WithSalvageSource(src))
	require.NotNil(t, s.salvage, "the option installs the seam")
	// The inert contract: absent the option the field stays nil and
	// salvageDeposeAtCap's first branch is the byte-identical terminal.
	s2 := NewService(nil, EngineConfig{})
	require.Nil(t, s2.salvage, "no option ⇒ inert (today's behavior)")
}

// stubSalvageSource is the unit-tier stub (the integration pins script
// theirs per scenario).
type stubSalvageSource struct{}

func (stubSalvageSource) SalvageFor(context.Context, *Task) *SalvageSignals { return nil }

// The seam's contract renders in RenderSalvage: an exhaustion-deposed
// pointer is the SAME handoff block the manager depose records (the
// inheritor cannot tell the two apart — inherit, don't restart).
func TestRenderSalvage_CarriesExhaustionPointer(t *testing.T) {
	p := (&SalvageSignals{Branch: "b/x", Head: "aa", OpenPR: "9"}).Pointer(3)
	out := RenderSalvage(&p)
	require.Contains(t, out, "SALVAGE HANDOFF")
	require.Contains(t, out, "b/x")
	require.Contains(t, out, "exhaustion-salvage")
	require.True(t, strings.HasPrefix(out, "SALVAGE HANDOFF"), "the block stays the S5 shape")
}
