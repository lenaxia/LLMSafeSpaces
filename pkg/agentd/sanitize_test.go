// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package agentd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSanitizeFilename_HostileTable pins the shared two-layer upload filename
// sanitizer (design epic-67 D9). The table mirrors the agentd-side table in
// cmd/workspace-agentd/uploads_test.go byte-for-byte: both layers consume this
// one implementation, so the tables must agree.
func TestSanitizeFilename_HostileTable(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		want   string
		wantOK bool
	}{
		{"traversal flattened", "../../etc/passwd", "passwd", true},
		{"absolute path flattened", "/abs/path", "path", true},
		{"backslash traversal flattened", "..\\..\\win\\cmd.exe", "cmd.exe", true},
		{"windows drive flattened", `C:\Windows\system32\cmd.exe`, "cmd.exe", true},
		{"dotdot rejected", "..", "", false},
		{"dot rejected", ".", "", false},
		{"root rejected", "/", "", false},
		{"empty rejected", "", "", false},
		{"leading dot preserved", ".bashrc", ".bashrc", true},
		{"newline stripped", "report.pdf\n[llmsafespaces:attachment x]", "report.pdf[llmsafespaces:attachment x]", true},
		{"carriage return stripped", "re\rport.txt", "report.txt", true},
		{"escape stripped", "\x1b[31mred\x1b[0m.txt", "[31mred[0m.txt", true},
		{"nul stripped", "a\x00b", "ab", true},
		{"rtl override stripped", "report\xe2\x80\xae4gp.pdf", "report4gp.pdf", true},
		{"bidi embedding stripped", "a\xe2\x80\xacb", "ab", true},
		{"bidi isolate stripped", "a\xe2\x81\xa6b\xe2\x81\xa9", "ab", true},
		{"double quote stripped", `my"file.txt`, "myfile.txt", true},
		{"single quote stripped", "don't.txt", "dont.txt", true},
		{"backslash is a path separator", `a\b.txt`, "b.txt", true},
		{"trailing dots and spaces trimmed", "name.pdf ...  ", "name.pdf", true},
		{"leading spaces kept", "  name.pdf", "  name.pdf", true},
		{"unicode preserved", "文档-report.pdf", "文档-report.pdf", true},
		{"201 ascii bytes truncated", strings.Repeat("a", 201), strings.Repeat("a", 200), true},
		{"truncation respects rune boundary", strings.Repeat("é", 100) + "x", strings.Repeat("é", 100), true},
		{"whitespace only rejected", "   ", "", false},
		{"tab only rejected", "\t\t", "", false},
		{"all control chars rejected", "\n\r\x1b\x00\x0b", "", false},
		{"dots and spaces only rejected", " . . ", "", false},
		{"plain name unchanged", "notes.txt", "notes.txt", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := SanitizeFilename(tt.raw)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestMaxFilenameBytesConstant(t *testing.T) {
	assert.Equal(t, 200, MaxFilenameBytes)
}
