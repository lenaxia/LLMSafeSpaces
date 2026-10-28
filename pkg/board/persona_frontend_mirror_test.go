// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// persona_frontend_mirror_test.go — the frontend's
// DEFAULT_REVIEWER_PERSONA mirror (frontend/src/lib/boardRules.ts — the
// persona editor's one-click import) is pinned BYTE-IDENTICAL to the
// Go DefaultReviewerPersona. The mirror exists because the UI's
// suppress-warning import must import EXACTLY what the engine would
// have spawned; drift would render a different persona pre- and
// post-import. Same pattern as the guard-codes fixture parity
// (api/internal/handlers/board_guard_codes_parity_test.go): the Go
// source is authoritative, the frontend file is read as a fixture.

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultReviewerPersona_FrontendMirror(t *testing.T) {
	raw, err := os.ReadFile("../../frontend/src/lib/boardRules.ts")
	require.NoError(t, err, "the mirror lives at frontend/src/lib/boardRules.ts")
	src := string(raw)

	nameRe := regexp.MustCompile(`DEFAULT_REVIEWER_PERSONA[^=]*=\s*\{\s*name:\s*"([^"]+)"`)
	m := nameRe.FindStringSubmatch(src)
	require.NotNil(t, m, "DEFAULT_REVIEWER_PERSONA's name literal not found")
	assert.Equal(t, DefaultReviewerPersona.Name, m[1], "the mirror's name must match the Go persona")

	// The instructions render as adjacent string concatenation
	// ("..." + "..." + …); the parity is on the JOINED value. Between
	// literals ONLY '+' and whitespace may appear (the concat is
	// strictly left-associative literals — anything else refuses).
	lit := `"(?:[^"\\]|\\.)*"`
	blockRe := regexp.MustCompile(`instructions:\s*(` + lit + `(?:\s*\+\s*` + lit + `)*)`)
	b := blockRe.FindStringSubmatch(src)
	require.NotNil(t, b, "DEFAULT_REVIEWER_PERSONA's instructions block not found")
	joined := stringConcatLiteral(t, b[1])
	assert.Equal(t, DefaultReviewerPersona.Instructions, joined,
		"the mirror's instructions must be byte-identical to the Go persona — the one-click import must import exactly what the engine would have spawned")
}

// stringConcatLiteral joins a TS string-concatenation expression's
// parts ("a" + "b" → ab), refusing anything but string literals and
// + separators (a template or identifier in the mirror would break
// byte-identity and must fail this test loudly).
func stringConcatLiteral(t *testing.T, expr string) string {
	t.Helper()
	litRe := regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
	parts := litRe.FindAllStringSubmatch(expr, -1)
	require.NotEmpty(t, parts, "no string literals found in the instructions block")
	joined := make([]string, 0, len(parts))
	for _, p := range parts {
		joined = append(joined, unescapeTSLiteral(p[1]))
	}
	out := strings.Join(joined, "")
	// Every non-literal rune outside quotes must be '+' or whitespace.
	stripped := litRe.ReplaceAllString(expr, "")
	for _, r := range stripped {
		if r != '+' && r != ' ' && r != '\t' && r != '\n' && r != '\r' {
			t.Fatalf("unexpected %q outside string literals in the mirror — only concatenation is parseable", string(r))
		}
	}
	return out
}

// unescapeTSLiteral decodes the escape sequences the mirror may carry
// (\" and \\ are the ones the persona text uses; anything else refuses
// rather than silently diverging from the Go literal).
func unescapeTSLiteral(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		i++
		if i >= len(s) {
			b.WriteByte('\\')
			break
		}
		switch s[i] {
		case '"', '\\':
			b.WriteByte(s[i])
		default:
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
