// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// validateSalvagePointer's guard matrix (review r1's missing-test
// finding 5): the free-text pointer is capped and corruption-scanned
// like every other manager-supplied text field (the textguard
// discipline) — pinned at the UNIT tier so the default test lane sees
// it, not only the integration suites.

func TestValidateSalvagePointer_EmptyRefused(t *testing.T) {
	err := validateSalvagePointer(SalvagePointer{})
	require.Error(t, err)
	g, ok := err.(*GuardError)
	require.True(t, ok)
	require.Equal(t, GuardInput, g.Code)
}

func TestValidateSalvagePointer_Caps(t *testing.T) {
	require.NoError(t, validateSalvagePointer(SalvagePointer{State: "pushed, CI green"}))

	oversized := strings.Repeat("a", MaxSalvageStateBytes+1)
	for field, ptr := range map[string]SalvagePointer{
		"state": {State: oversized},
		"note":  {Note: oversized},
	} {
		err := validateSalvagePointer(ptr)
		require.Error(t, err, "%s over the cap must refuse", field)
		g, ok := err.(*GuardError)
		require.True(t, ok)
		require.Equal(t, GuardInput, g.Code)
		require.Contains(t, g.Detail, "salvage."+field)
	}
	refOversized := strings.Repeat("b", MaxSalvageRefBytes+1)
	for field, ptr := range map[string]SalvagePointer{
		"branch": {Branch: refOversized},
		"pr":     {PR: refOversized},
		"head":   {Head: refOversized},
	} {
		err := validateSalvagePointer(ptr)
		require.Error(t, err, "%s over the cap must refuse", field)
		g, ok := err.(*GuardError)
		require.True(t, ok)
		require.Equal(t, GuardInput, g.Code)
		require.Contains(t, g.Detail, "salvage."+field)
	}
}

func TestValidateSalvagePointer_CorruptionScan(t *testing.T) {
	// The #1832 stored-artifact shape: serialized JSON structural splice
	// bytes leaking INTO a string value.
	corrupted := SalvagePointer{State: `done","title":"PLACEHOLDER"}`}
	err := validateSalvagePointer(corrupted)
	require.Error(t, err)
	g, ok := err.(*GuardError)
	require.True(t, ok)
	require.Equal(t, GuardTextRejected, g.Code)

	// The degenerate-repetition shape: one unit tiled past the floor.
	tiled := SalvagePointer{Note: strings.Repeat("PLACEHOLDER ", 60)}
	err = validateSalvagePointer(tiled)
	require.Error(t, err)
	g, ok = err.(*GuardError)
	require.True(t, ok)
	require.Equal(t, GuardTextRejected, g.Code)
}

func TestRenderSalvage_NilAndEmptyRenderNothing(t *testing.T) {
	require.Empty(t, RenderSalvage(nil))
	require.Empty(t, RenderSalvage(&SalvagePointer{}))
	require.NotEmpty(t, RenderSalvage(&SalvagePointer{PR: "1997"}))
}
