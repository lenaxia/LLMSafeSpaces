// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// persona_frontend_mirror_test.go — the frontend's
// DEFAULT_REVIEWER_PERSONA mirror (frontend/src/lib/boardRules.ts — the
// persona editor's one-click import) is pinned to the Go
// DefaultReviewerPersona's role reference. Role-only (owner ruling
// 2026-10-29, round 2): the mirror is the built-in reviewer role's
// SLUG — the import writes exactly the reference the engine's default
// composition resolves; drift would render a different reviewer pre-
// and post-import. Same pattern as the guard-codes fixture parity
// (api/internal/handlers/board_guard_codes_parity_test.go): the Go
// source is authoritative, the frontend file is read as a fixture.

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultReviewerPersona_FrontendMirror(t *testing.T) {
	raw, err := os.ReadFile("../../frontend/src/lib/boardRules.ts")
	require.NoError(t, err, "the mirror lives at frontend/src/lib/boardRules.ts")

	re := regexp.MustCompile(`DEFAULT_REVIEWER_PERSONA[^=]*=\s*"([^"]+)"`)
	m := re.FindStringSubmatch(string(raw))
	require.NotNil(t, m, "DEFAULT_REVIEWER_PERSONA's string literal not found")
	assert.Equal(t, DefaultReviewerPersona.RoleID, m[1],
		"the mirror must be the Go DefaultReviewerPersona's role reference (the built-in reviewer role's slug)")
	assert.Equal(t, DefaultReviewerRoleSlug, m[1], "and the slug is the seeded built-in role's")
}
