// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package settings

import (
	"regexp"
	"strings"
)

// Normalize canonicalizes a value before validation. The motivation
// is the "8gi" production failure: an admin typed "8gi" (lowercase
// unit) in the admin settings UI; the value passed validation (no
// pattern), reached the database, and broke every workspace
// creation when the validating webhook rejected the lowercase suffix.
//
// Two-stage policy:
//
//  1. Normalize() rewrites unambiguous near-misses to canonical form
//     ("8gi" → "8Gi", "8GB" → "8Gi", "  500m  " → "500m"). The
//     canonical form is what the Kubernetes apiserver and our
//     validating webhook accept.
//
//  2. The caller then runs Validate() against the normalized value.
//     Inputs that the normalizer can't safely correct ("banana",
//     "8gigabyte", "8 G") pass through unchanged so Validate's
//     pattern check rejects them with a precise error.
//
// Only string-typed settings with a known shape are normalized. Bool,
// int, enum, []string, and string settings without a registered
// normalizer pass through untouched.
//
// Normalize cannot fail — rejection is Validate's job. Returning
// (value) without an error keeps the call sites simple.
func Normalize(def SettingDef, value any) any {
	if def.Type != TypeString {
		return value
	}
	s, ok := value.(string)
	if !ok {
		return value
	}
	switch def.Key {
	case "workspace.defaultResources.memory":
		return normalizeMemory(s)
	case "workspace.defaultStorageSize":
		// Storage uses Gi/Mi only (no Ki) per the schema pattern, but
		// the input shapes we want to fix up are the same.
		return normalizeMemory(s)
	case "workspace.defaultResources.cpu":
		return normalizeCPU(s)
	case "workspace.defaultImage":
		return strings.TrimSpace(s)
	}
	return s
}

// memoryNormalizePattern matches "<digits>[whitespace]<unit>" where
// unit is any case variant of Ki/Mi/Gi or the colloquial KB/MB/GB.
// Anchored at start; trailing whitespace stripped before matching.
//
// Group 1: digits.
// Group 2: unit token (case-insensitive). We canonicalize via
// memoryUnitMap below; anything not in the map is rejected by
// returning the original input.
var memoryNormalizePattern = regexp.MustCompile(`(?i)^([0-9]+)[\s]*([a-z]+)$`)

// memoryUnitMap maps lowercase variants to canonical Kubernetes
// suffixes. KB/MB/GB → Ki/Mi/Gi reflects what users almost always
// mean: most workloads are sized in powers of 2 (memory), and the
// difference between e.g. 1 GB (10^9 bytes) and 1 GiB (2^30 bytes)
// is below the noise floor of how operators size workspace pods.
// Choosing the binary unit silently is the safer default — the user
// gets at least the GB they asked for.
//
// Bare single-letter units ("8K", "8M", "8G") are NOT in the map.
// In Kubernetes Quantity grammar bare K/M/G mean decimal multiples
// (10^3, 10^6, 10^9), distinct from Ki/Mi/Gi (binary). A user who
// types "8G" might reasonably mean either, so we pass these through
// to Validate's pattern check rather than guess. The two-stage
// design (Normalize then Validate) means ambiguous inputs get a
// clean rejection, not a wrong silent fix.
var memoryUnitMap = map[string]string{
	"ki": "Ki",
	"mi": "Mi",
	"gi": "Gi",
	"kb": "Ki",
	"mb": "Mi",
	"gb": "Gi",
}

func normalizeMemory(s string) string {
	trimmed := strings.TrimSpace(s)
	m := memoryNormalizePattern.FindStringSubmatch(trimmed)
	if m == nil {
		return s // pass through; Validate will reject
	}
	digits, unitRaw := m[1], strings.ToLower(m[2])
	canonical, ok := memoryUnitMap[unitRaw]
	if !ok {
		return s // unrecognized unit token; pass through
	}
	return digits + canonical
}

// cpuNormalizePattern matches the millicore form "<digits>[whitespace]m"
// (case-insensitive) — the only shape we auto-correct. The other
// valid CPU form ("1.0", "0.5") doesn't have any common typo to
// correct, so it passes through untouched.
var cpuNormalizePattern = regexp.MustCompile(`(?i)^([0-9]+)[\s]*m$`)

func normalizeCPU(s string) string {
	trimmed := strings.TrimSpace(s)
	m := cpuNormalizePattern.FindStringSubmatch(trimmed)
	if m == nil {
		return s
	}
	return m[1] + "m"
}
