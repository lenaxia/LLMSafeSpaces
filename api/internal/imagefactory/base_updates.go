// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package imagefactory

// Base-update computation (issue #928, design/0046 ruling #29): the
// read-side signal behind the base-update pill. Pure over (config,
// catalog bases) so the rules are unit-testable and callers batch one
// ListBases per request regardless of config count.
//
// Semantics (ruling #29 makes base migration the sanctioned
// version-migration path for apt-track packages):
//
//   - base_migration: the platform default base is a different NAME
//     than the config's. Headline signal; outranks a same-name bump.
//     The same-name latest version rides along for the diff preview.
//   - version_bump: same name, newer published version.
//   - nil: fresh (on latest of the default base), or the config's base
//     name is absent from the catalog (retired — nothing to suggest).
//
// Version comparison is numeric-semantic ("0.10.0" > "0.9.0"); a
// catalog max older than the config's pin never yields a signal
// (no downgrade suggestions).

import "strconv"

// BaseUpdateKind discriminates BaseUpdates.
type BaseUpdateKind string

const (
	BaseUpdateVersionBump   BaseUpdateKind = "version_bump"
	BaseUpdateBaseMigration BaseUpdateKind = "base_migration"
)

// BaseUpdates is the update pill payload on a config (omitempty on the
// wire: absent = fresh).
type BaseUpdates struct {
	Kind               BaseUpdateKind `json:"kind"`
	CurrentBaseName    string         `json:"currentBaseName"`
	CurrentBaseVersion string         `json:"currentBaseVersion"`
	// LatestBaseVersion is the newest published version of the SAME
	// base name (set for both kinds when one exists).
	LatestBaseVersion string `json:"latestBaseVersion,omitempty"`
	// DefaultBaseName/Version are set for base_migration only.
	DefaultBaseName    string `json:"defaultBaseName,omitempty"`
	DefaultBaseVersion string `json:"defaultBaseVersion,omitempty"`
}

// ComputeBaseUpdates returns the available base movement for c given
// the full catalog base list, or nil when the config is fresh / its
// base is retired.
func ComputeBaseUpdates(c Config, bases []Base) *BaseUpdates {
	var latestSame string
	var def *Base
	known := false
	for i := range bases {
		b := &bases[i]
		if b.Name == c.BaseName {
			known = true
			if CompareVersions(b.Version, latestSame) > 0 {
				latestSame = b.Version
			}
		}
		if b.IsDefault {
			def = b
		}
	}
	if !known {
		return nil
	}

	u := &BaseUpdates{
		CurrentBaseName:    c.BaseName,
		CurrentBaseVersion: c.BaseVersion,
		LatestBaseVersion:  latestSame,
	}

	if def != nil && def.Name != c.BaseName {
		u.Kind = BaseUpdateBaseMigration
		u.DefaultBaseName = def.Name
		u.DefaultBaseVersion = def.Version
		return u
	}
	if CompareVersions(latestSame, c.BaseVersion) > 0 {
		u.Kind = BaseUpdateVersionBump
		return u
	}
	return nil
}

// CompareVersions orders dot-separated numeric version strings
// ("0.10.0" > "0.9.0"). Non-numeric segments compare lexically; a
// longer prefix of equal segments is greater. Tolerates arbitrary
// segment counts.
func CompareVersions(a, b string) int {
	as, bs := splitDots(a), splitDots(b)
	n := len(as)
	if len(bs) < n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		ai, aErr := strconv.Atoi(as[i])
		bi, bErr := strconv.Atoi(bs[i])
		if aErr != nil || bErr != nil {
			if as[i] != bs[i] {
				if as[i] < bs[i] {
					return -1
				}
				return 1
			}
			continue
		}
		if ai != bi {
			if ai < bi {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(as) > len(bs):
		return 1
	case len(as) < len(bs):
		return -1
	default:
		return 0
	}
}

func splitDots(s string) []string {
	if s == "" {
		return nil
	}
	out := []string{}
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '.' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return out
}
