// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package agentd

import (
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxFilenameBytes is the sanitized-filename byte cap applied by both upload
// layers (agentd PUT /v1/files authoritative, API defense-in-depth — design
// epic-68 D9). Truncation lands on a rune boundary.
const MaxFilenameBytes = 200

// SanitizeFilename flattens an untrusted filename to a safe base name. It is
// the single source shared by agentd's PUT /v1/files handler (authoritative)
// and the API's upload proxy (defense-in-depth), so the two layers cannot
// drift:
//
//  1. basename — both path separators (/ and \) are split so Windows-shaped
//     traversal cannot survive
//  2. strip control characters (C0/C1 incl. \n \r \x1b), bidi/RTL overrides
//     (U+202A-U+202E, U+2066-U+2069), quotes, backslashes, and any residual
//     slash
//  3. trim trailing dots and spaces (Windows-hostile suffixes)
//  4. truncate to MaxFilenameBytes on a rune boundary, re-trim
//  5. reject empty or whitespace-only results (ok=false)
func SanitizeFilename(raw string) (string, bool) {
	base := filepath.Base(strings.ReplaceAll(raw, "\\", "/"))

	var b strings.Builder
	b.Grow(len(base))
	for _, r := range base {
		if IsForbiddenFilenameRune(r) {
			continue
		}
		b.WriteRune(r)
	}

	name := strings.TrimRight(b.String(), ". ")
	if len(name) > MaxFilenameBytes {
		for len(name) > MaxFilenameBytes {
			_, size := utf8.DecodeLastRuneInString(name)
			name = name[:len(name)-size]
		}
		name = strings.TrimRight(name, ". ")
	}
	if strings.TrimSpace(name) == "" {
		return "", false
	}
	return name, true
}

// IsForbiddenFilenameRune reports whether r is stripped during filename
// sanitization: quote characters, path separators (backslash arrives here
// only when residual after the separator split), bidi/RTL overrides, and
// control characters.
func IsForbiddenFilenameRune(r rune) bool {
	switch r {
	case '"', '\'', '\\', '/':
		return true
	}
	if r >= 0x202A && r <= 0x202E {
		return true
	}
	if r >= 0x2066 && r <= 0x2069 {
		return true
	}
	return unicode.IsControl(r)
}
