// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package imagefactory

// Tests for ComputeBaseUpdates — the read-side signal behind the
// base-update pill (issue #928, design/0046 ruling #29).
//
// The computation is pure over (config, catalog bases) so every rule is
// unit-testable without SQL:
//
//   - current (base,version) is the latest published version of the
//     SAME base name and matches the platform default → nil (fresh)
//   - same name, newer version published → kind=version_bump
//   - platform default base is a DIFFERENT name → kind=base_migration
//     (the sanctioned path per ruling #29; reported even if the old
//     base also has a newer version — migration outranks bump)
//   - config pinned to a base name absent from the catalog → nil with
//     no error (base retired; nothing to suggest — refresh would be a
//     re-save against the default, which the migration pill on other
//     configs already covers; silently suggesting a rebuild for a base
//     we no longer publish would be wrong)
//   - version comparison is numeric-semantic ("0.10.0" > "0.9.0"),
//     not lexicographic

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func cfg(base, version string) Config {
	return Config{BaseName: base, BaseVersion: version}
}

func TestComputeBaseUpdates_Fresh_Nil(t *testing.T) {
	bases := []Base{
		{Name: "bookworm", Version: "0.6.0"},
		{Name: "bookworm", Version: "0.9.0", IsDefault: true},
	}
	require.Nil(t, ComputeBaseUpdates(cfg("bookworm", "0.9.0"), bases))
}

func TestComputeBaseUpdates_VersionBump(t *testing.T) {
	bases := []Base{
		{Name: "bookworm", Version: "0.6.0"},
		{Name: "bookworm", Version: "0.9.0", IsDefault: true},
		{Name: "trixie", Version: "0.1.0"},
	}
	got := ComputeBaseUpdates(cfg("bookworm", "0.6.0"), bases)
	require.NotNil(t, got)
	require.Equal(t, BaseUpdateVersionBump, got.Kind)
	require.Equal(t, "0.9.0", got.LatestBaseVersion)
	require.Equal(t, "bookworm", got.CurrentBaseName)
	require.Equal(t, "0.6.0", got.CurrentBaseVersion)
	require.Empty(t, got.DefaultBaseName, "no migration when config is on the default base")
}

func TestComputeBaseUpdates_BaseMigration(t *testing.T) {
	// Default moved to trixie; config still on bookworm — and bookworm
	// ALSO has a newer version. Migration is the headline (sanctioned
	// path), the bump rides along for the diff preview.
	bases := []Base{
		{Name: "bookworm", Version: "0.6.0"},
		{Name: "bookworm", Version: "0.9.0"},
		{Name: "trixie", Version: "0.1.0", IsDefault: true},
	}
	got := ComputeBaseUpdates(cfg("bookworm", "0.6.0"), bases)
	require.NotNil(t, got)
	require.Equal(t, BaseUpdateBaseMigration, got.Kind)
	require.Equal(t, "trixie", got.DefaultBaseName)
	require.Equal(t, "0.1.0", got.DefaultBaseVersion)
	require.Equal(t, "0.9.0", got.LatestBaseVersion, "same-name bump included for the diff preview")
}

func TestComputeBaseUpdates_OnLatestOfRetiredDefault_Nil(t *testing.T) {
	// Default moved to trixie; config on bookworm@latest. Migration
	// must still be reported — that IS the update.
	bases := []Base{
		{Name: "bookworm", Version: "0.9.0"},
		{Name: "trixie", Version: "0.1.0", IsDefault: true},
	}
	got := ComputeBaseUpdates(cfg("bookworm", "0.9.0"), bases)
	require.NotNil(t, got)
	require.Equal(t, BaseUpdateBaseMigration, got.Kind)
}

func TestComputeBaseUpdates_UnknownBaseName_Nil(t *testing.T) {
	bases := []Base{{Name: "trixie", Version: "0.1.0", IsDefault: true}}
	require.Nil(t, ComputeBaseUpdates(cfg("bullseye", "1.0.0"), bases),
		"base absent from catalog: nothing to suggest; a pill would imply a refresh target that doesn't exist")
}

func TestComputeBaseUpdates_SemverNotLexicographic(t *testing.T) {
	// "0.10.0" > "0.9.0" numerically; lexicographic says otherwise.
	bases := []Base{
		{Name: "bookworm", Version: "0.9.0"},
		{Name: "bookworm", Version: "0.10.0", IsDefault: true},
	}
	got := ComputeBaseUpdates(cfg("bookworm", "0.9.0"), bases)
	require.NotNil(t, got, "0.10.0 is newer than 0.9.0")
	require.Equal(t, "0.10.0", got.LatestBaseVersion)

	// Reverse: config on 0.10.0, catalog max is 0.9.0 (shouldn't happen;
	// must not produce an "update" downgrading).
	require.Nil(t, ComputeBaseUpdates(cfg("bookworm", "0.10.0"), []Base{
		{Name: "bookworm", Version: "0.9.0", IsDefault: true},
	}), "never suggest a downgrade")
}

func TestComputeBaseUpdates_NoDefaultBase_NilForCurrent(t *testing.T) {
	// Degenerate catalog with no is_default row (operator error):
	// version-bump logic still works off same-name max.
	bases := []Base{
		{Name: "bookworm", Version: "0.6.0"},
		{Name: "bookworm", Version: "0.8.0"},
	}
	got := ComputeBaseUpdates(cfg("bookworm", "0.6.0"), bases)
	require.NotNil(t, got)
	require.Equal(t, BaseUpdateVersionBump, got.Kind)
	require.Nil(t, ComputeBaseUpdates(cfg("bookworm", "0.8.0"), bases))
}

// TestCompareVersions_PinnedEdges pins the comparator's documented
// behavior at its edges (review round 1: the comparator advertises
// arbitrary segment counts; these tests make the semantics explicit
// rather than accidental). Catalog discipline today is 3-segment
// dot-numeric; if rc-suffixes ever enter the catalog, revisit — a
// pre-release currently sorts ABOVE its final release via the lexical
// fallback, which would suggest "downgrading" rc→final as an update.
func TestCompareVersions_PinnedEdges(t *testing.T) {
	// Fewer segments = less, even when the shared prefix ties: "0.9" <
	// "0.9.0". A config pinned "0.9" against catalog "0.9.0" therefore
	// gets a version_bump pill — same version in spirit, harmless
	// (re-save reconciles), pinned here so a change is deliberate.
	require.Negative(t, CompareVersions("0.9", "0.9.0"))
	require.Positive(t, CompareVersions("0.9.0", "0.9"))

	// Non-numeric suffix compares lexically: "-rc1" > "" so rc sorts
	// above final. Pinned; see the doc comment above.
	require.Positive(t, CompareVersions("0.9.0-rc1", "0.9.0"))
	require.Negative(t, CompareVersions("0.9.0-rc1", "0.9.0-rc2"), "rc1 < rc2 lexically")

	// Identical inputs.
	require.Zero(t, CompareVersions("1.2.3", "1.2.3"))
	require.Zero(t, CompareVersions("", ""))
}
