// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package version

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// withVars temporarily replaces the ldflags-injectable vars and restores
// them after the test. resolveOnce may already have fired in another test —
// that is fine: resolve() never overwrites non-"unknown" values, so the
// values set here are exactly what String()/Commit() observe.
func withVars(t *testing.T, v, sha, bt string) {
	t.Helper()
	ov, osha, obt := Version, CommitSHA, BuildTime
	Version, CommitSHA, BuildTime = v, sha, bt
	t.Cleanup(func() { Version, CommitSHA, BuildTime = ov, osha, obt })
}

func TestString_FullIdentity(t *testing.T) {
	withVars(t, "v0.15.7", "d93e52ea", "2026-08-15T20:43:36Z")
	assert.Equal(t, "v0.15.7+gd93e52ea", String())
	assert.Equal(t, "v0.15.7+gd93e52ea (built 2026-08-15T20:43:36Z)", Full())
}

func TestString_OmitsUnknownCommit(t *testing.T) {
	// Incident 2026-08-15 class of build: VERSION stamped (release value),
	// commit never injected. String() must NOT pretend to be a clean
	// release — omitting the commit makes the deficiency visible.
	withVars(t, "0.15.7", "unknown", "unknown")
	assert.Equal(t, "0.15.7", String())
	assert.Equal(t, "0.15.7", Full(), "unknown build time must be omitted too")
}

func TestString_AllUnknown(t *testing.T) {
	withVars(t, "unknown", "unknown", "unknown")
	assert.Equal(t, "unknown", String())
}

func TestString_LongSHAIsNotTruncatedByString(t *testing.T) {
	// ldflags may inject the full 40-char sha; String passes it through.
	// (Only the buildinfo FALLBACK truncates to 8 chars.)
	withVars(t, "v1.0.0", "d93e52ea11112222333344445555666677778888", "unknown")
	assert.Equal(t, "v1.0.0+gd93e52ea11112222333344445555666677778888", String())
}

func TestCommit_AccessorResolves(t *testing.T) {
	// Commit() must return whatever is currently injected (post-resolve).
	withVars(t, "v1.0.0", "abcd1234", "unknown")
	assert.Equal(t, "abcd1234", Commit())
}
