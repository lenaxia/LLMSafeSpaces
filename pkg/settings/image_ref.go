// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package settings

import (
	"fmt"
	"strings"
)

// mutableTags are registry tags that are re-pushed over time. A workspace
// image selected by one of these tags resolves to a different digest on
// every pull — and per puller: registry mirrors (e.g. spegel) serve the
// digest cached on the node while the upstream registry has already moved
// the tag. Production incident 2026-08-14: the seeded default
// ghcr.io/lenaxia/llmsafespaces/base:latest served a 4-day-old v0.13.0
// digest through the cluster mirror while upstream latest was v0.15.5,
// so new workspaces silently launched with a pre-fix agentd.
//
// Scope decision (2026-08-15 review): this is a blocklist of the common
// mutable conventions, NOT a shape allow-list (semver/sha-/ts-). A shape
// rule would reject legitimate private conventions (e.g. "v2-final",
// "2026q3-pinned"). The blocklist is deliberately extended with the
// common alias family (stable/prod/current/release); an operator using a
// non-listed re-pushable tag owns that choice — the known-mutable
// conventions are what the platform has ever seeded or documented.
var mutableTags = map[string]struct{}{
	"latest":  {},
	"main":    {},
	"master":  {},
	"dev":     {},
	"edge":    {},
	"nightly": {},
	"stable":  {},
	"prod":    {},
	"current": {},
	"release": {},
}

const (
	maxTagLen    = 128
	maxDigestLen = 71 // "sha256:" + 64 hex
)

func isMutableTag(tag string) bool {
	_, ok := mutableTags[strings.ToLower(tag)]
	return ok
}

// ValidateImageRefPinned is the exported form of validateImageRefPinned for
// read paths that consume stored settings values (e.g. the workspace
// service's default-runtime resolution) and must not launch a floating-tag
// image even when the stored value predates write-side validation.
func ValidateImageRefPinned(value string) error {
	return validateImageRefPinned(value)
}

// validateImageRefPinned enforces that a value shaped like a container
// image reference is pinned to an immutable-ish selector: an explicit tag
// that is not a known-mutable tag, or a digest. Un-tagged image refs are
// rejected because runtimes implicitly resolve them to :latest. Values
// without '/' are RuntimeEnvironment names resolved against the chart —
// they pass here.
func validateImageRefPinned(value string) error {
	if value == "" {
		return nil
	}
	if strings.ContainsAny(value, " \t\n\r") {
		return fmt.Errorf("image reference %q must not contain whitespace", value)
	}
	lastSlash := strings.LastIndex(value, "/")
	if lastSlash < 0 {
		// No slash: either a RuntimeEnvironment name or a docker.io
		// library-image shorthand. Disambiguation is total — K8s object
		// names cannot contain ':', so a colon means image ref. Apply
		// the full tag checks to the post-colon segment (round-4 review:
		// "ubuntu:latest"/"nginx:stable" previously bypassed the pinning
		// check entirely via this branch).
		if colon := strings.LastIndex(value, ":"); colon >= 0 {
			tag := value[colon+1:]
			if tag == "" {
				return fmt.Errorf("image reference %q has an empty tag", value)
			}
			if len(tag) > maxTagLen {
				return fmt.Errorf("image reference %q tag exceeds %d characters", value, maxTagLen)
			}
			if !validTagChars(tag) {
				return fmt.Errorf("image reference %q tag %q contains invalid characters", value, tag)
			}
			if isMutableTag(tag) {
				return fmt.Errorf("image reference %q uses mutable tag %q; pin to an immutable tag or digest instead", value, tag)
			}
			return nil
		}
		if isMutableTag(value) {
			return fmt.Errorf("%q is a mutable tag, not a runtime environment name", value)
		}
		return nil
	}

	if at := strings.LastIndex(value, "@"); at > lastSlash {
		digest := value[at+1:]
		// sha256 digests are exactly "sha256:" + 64 hex chars. An
		// over-long hex tail (65+) would only fail later at pull time —
		// reject at the write boundary with a precise message instead.
		if len(digest) != maxDigestLen || !strings.HasPrefix(digest, "sha256:") || !isHex(digest[len("sha256:"):]) {
			return fmt.Errorf("image reference %q has a malformed digest", value)
		}
		return nil
	}

	repoAndTag := value[lastSlash+1:]
	colon := strings.LastIndex(repoAndTag, ":")
	if colon < 0 {
		return fmt.Errorf("image reference %q has no tag and would implicitly resolve to :latest", value)
	}
	tag := repoAndTag[colon+1:]
	if tag == "" {
		return fmt.Errorf("image reference %q has an empty tag", value)
	}
	if len(tag) > maxTagLen {
		return fmt.Errorf("image reference %q tag exceeds %d characters", value, maxTagLen)
	}
	if !validTagChars(tag) {
		return fmt.Errorf("image reference %q tag %q contains invalid characters", value, tag)
	}
	if isMutableTag(tag) {
		return fmt.Errorf("image reference %q uses mutable tag %q; pin to an immutable tag or digest instead", value, tag)
	}
	return nil
}

// validTagChars enforces the OCI tag grammar:
// [a-zA-Z0-9_][a-zA-Z0-9._-]{0,127} — the first character may be
// alphanumeric or an underscore; subsequent characters may also be
// '.' and '-'. (A leading '.' or '-' is invalid.)
func validTagChars(tag string) bool {
	if len(tag) == 0 {
		return false
	}
	c := tag[0]
	isFirst := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
	if !isFirst {
		return false
	}
	for i := 1; i < len(tag); i++ {
		c := tag[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
