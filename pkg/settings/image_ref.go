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
var mutableTags = map[string]struct{}{
	"latest":  {},
	"main":    {},
	"master":  {},
	"dev":     {},
	"edge":    {},
	"nightly": {},
}

const (
	maxTagLen   = 128
	maxDigestLn = 71
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
	if strings.ContainsAny(value, " \t") {
		return fmt.Errorf("image reference %q must not contain whitespace", value)
	}
	lastSlash := strings.LastIndex(value, "/")
	if lastSlash < 0 {
		if isMutableTag(value) {
			return fmt.Errorf("%q is a mutable tag, not a runtime environment name", value)
		}
		return nil
	}

	if at := strings.LastIndex(value, "@"); at > lastSlash {
		digest := value[at+1:]
		if len(digest) < maxDigestLn || !strings.HasPrefix(digest, "sha256:") || !isHex(digest[len("sha256:"):]) {
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

func validTagChars(tag string) bool {
	for i := 0; i < len(tag); i++ {
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
