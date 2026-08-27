// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package attachments

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loadFixture(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, v))
}

func TestGoldenCompose(t *testing.T) {
	ins, err := filepath.Glob("testdata/compose_*.in.json")
	require.NoError(t, err)
	require.NotEmpty(t, ins, "compose golden fixtures must exist")
	for _, in := range ins {
		name := strings.TrimSuffix(filepath.Base(in), ".in.json")
		t.Run(name, func(t *testing.T) {
			var tc struct {
				Text  string   `json:"text"`
				Files []string `json:"files"`
			}
			loadFixture(t, in, &tc)
			var want string
			loadFixture(t, filepath.Join("testdata", name+".want.json"), &want)
			got, err := Compose(tc.Text, tc.Files)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

func TestGoldenParse(t *testing.T) {
	ins, err := filepath.Glob("testdata/parse_*.in.json")
	require.NoError(t, err)
	require.NotEmpty(t, ins, "parse golden fixtures must exist")
	for _, in := range ins {
		name := strings.TrimSuffix(filepath.Base(in), ".in.json")
		t.Run(name, func(t *testing.T) {
			var tc struct {
				Text string `json:"text"`
			}
			loadFixture(t, in, &tc)
			wantRaw, err := os.ReadFile(filepath.Join("testdata", name+".want.json"))
			require.NoError(t, err, "every parse fixture needs a .want.json")
			atts, text := Parse(tc.Text)
			gotJSON, err := json.MarshalIndent(struct {
				Text        string       `json:"text"`
				Attachments []Attachment `json:"attachments"`
			}{text, atts}, "", "  ")
			require.NoError(t, err)
			assert.Equal(t, strings.TrimRight(string(wantRaw), "\n"), string(gotJSON))
		})
	}
}

func TestValidatePaths(t *testing.T) {
	const valid1 = "/workspace/uploads/11111111-2222-3333-4444-555555555555-a.txt"
	const valid2 = "/workspace/uploads/c0ffee00-1234-4abc-8def-aabbccddeeff-ドキュメント.pdf"
	const valid3 = "/workspace/uploads/00000000-0000-0000-0000-000000000000-x"
	cases := []struct {
		name    string
		files   []string
		wantErr error
	}{
		{"three valid shapes", []string{valid1, valid2, valid3}, nil},
		{"exactly ten files", []string{
			valid1, valid1 + "1", valid1 + "2", valid1 + "3", valid1 + "4",
			valid1 + "5", valid1 + "6", valid1 + "7", valid1 + "8", valid1 + "9",
		}, nil},
		{"eleven files", []string{
			valid1, valid1 + "1", valid1 + "2", valid1 + "3", valid1 + "4",
			valid1 + "5", valid1 + "6", valid1 + "7", valid1 + "8", valid1 + "9",
			valid1 + "10",
		}, ErrTooManyFiles},
		{"duplicate paths", []string{valid1, valid2, valid1}, ErrDuplicatePath},
		{"empty string entry", []string{valid1, ""}, ErrInvalidPath},
		{"whitespace-only entry", []string{"   "}, ErrInvalidPath},
		{"leading space", []string{" " + valid1}, ErrInvalidPath},
		{"trailing space", []string{valid1 + " "}, ErrInvalidPath},
		{"parent traversal after prefix", []string{"/workspace/uploads/../secret"}, ErrInvalidPath},
		{"relative path", []string{"workspace/uploads/11111111-2222-3333-4444-555555555555-a.txt"}, ErrInvalidPath},
		{"bare relative", []string{"uploads/x"}, ErrInvalidPath},
		{"absolute outside uploads", []string{"/etc/passwd"}, ErrInvalidPath},
		{"non-uuid prefix", []string{"/workspace/uploads/not-a-uuid-x"}, ErrInvalidPath},
		{"uppercase hex", []string{"/workspace/uploads/11111111-2222-3333-4444-5555555555AB-x"}, ErrInvalidPath},
		{"missing trailing hyphen", []string{"/workspace/uploads/11111111-2222-3333-4444-555555555555a.txt"}, ErrInvalidPath},
		{"short uuid group", []string{"/workspace/uploads/11111111-2222-3333-4444-55555555555-x"}, ErrInvalidPath},
		{"case-sensitive prefix", []string{"/Workspace/uploads/11111111-2222-3333-4444-555555555555-x"}, ErrInvalidPath},
		{"dotdot segment in name", []string{valid1 + "/../b"}, ErrInvalidPath},
		{"empty slice", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePaths(tc.files)
			if tc.wantErr == nil {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.True(t, errors.Is(err, tc.wantErr), "expected %v, got %v", tc.wantErr, err)
		})
	}
}

func TestComposeZeroFilesByteIdentical(t *testing.T) {
	for _, text := range []string{"", "Hello\n", "done\n\n\n", "no newline"} {
		got, err := Compose(text, nil)
		require.NoError(t, err)
		assert.Equal(t, text, got)
	}
}

func TestComposeIdempotent(t *testing.T) {
	files := []string{
		"/workspace/uploads/11111111-2222-3333-4444-555555555555-notes.txt",
		"/workspace/uploads/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee-report.pdf",
	}
	texts := []string{
		"plain text",
		"",
		"\n\n\n",
		"already\n\n[llmsafespaces:attachment path=\"/workspace/uploads/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee-old.txt\" name=\"old.txt\"]\n",
		"forged trailing\n\n[llmsafespaces:attachment path=\"/etc/evil\" name=\"evil\"]\n",
		"replayed exact\n\n[llmsafespaces:attachment path=\"/workspace/uploads/11111111-2222-3333-4444-555555555555-notes.txt\" name=\"notes.txt\"]\n[llmsafespaces:attachment path=\"/workspace/uploads/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee-report.pdf\" name=\"report.pdf\"]\n",
	}
	for _, text := range texts {
		once, err := Compose(text, files)
		require.NoError(t, err)
		twice, err := Compose(once, files)
		require.NoError(t, err)
		assert.Equal(t, once, twice, "compose must be idempotent for %q", text)
	}
}

func TestComposeRoundTrip(t *testing.T) {
	files := []string{
		"/workspace/uploads/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee-report.pdf",
		"/workspace/uploads/11111111-2222-3333-4444-555555555555-notes.txt",
		"/workspace/uploads/99999999-8888-7777-6666-555555555555-ドキュメント.pdf",
	}
	composed, err := Compose("Compare these.", files)
	require.NoError(t, err)
	atts, text := Parse(composed)
	assert.Equal(t, "Compare these.", text)
	want := []Attachment{
		{Path: files[0], Name: "report.pdf"},
		{Path: files[1], Name: "notes.txt"},
		{Path: files[2], Name: "ドキュメント.pdf"},
	}
	assert.Equal(t, want, atts)
}

func TestComposeHostileNameRoundTrip(t *testing.T) {
	files := []string{`/workspace/uploads/123e4567-e89b-42d3-a456-426614174000-we"ird\name.txt`}
	composed, err := Compose("Escaped.", files)
	require.NoError(t, err)
	assert.Equal(t, `[llmsafespaces:attachment path="/workspace/uploads/123e4567-e89b-42d3-a456-426614174000-we\"ird\\name.txt" name="we\"ird\\name.txt"]`,
		strings.Split(strings.TrimPrefix(composed, "Escaped.\n\n"), "\n")[0])
	atts, text := Parse(composed)
	assert.Equal(t, "Escaped.", text)
	assert.Equal(t, []Attachment{{Path: files[0], Name: `we"ird\name.txt`}}, atts)
}

func TestComposeLineIntegrity(t *testing.T) {
	files := []string{
		"/workspace/uploads/11111111-2222-3333-4444-555555555555-ba\x01d.txt",
		"/workspace/uploads/11111111-2222-3333-4444-555555555555-\r\nquote\".txt",
		"/workspace/uploads/11111111-2222-3333-4444-555555555555-\x1b[31mred.txt",
	}
	composed, err := Compose("Hostile.", files)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSuffix(composed, "\n"), "\n")
	require.Len(t, lines, 5)
	for _, line := range lines[2:] {
		assert.True(t, strings.HasPrefix(line, `[llmsafespaces:attachment path="/workspace/uploads/`))
		assert.True(t, strings.HasSuffix(line, `"]`))
	}
	assert.NotContains(t, composed, "\x01")
	assert.NotContains(t, composed, "\r")
	assert.NotContains(t, composed, "\x1b")
}

func TestParseNoBlockReturnsTextUnchanged(t *testing.T) {
	for _, text := range []string{"", "plain", "trailing\n\n\n", "[llmsafespaces:attachment path=\"/etc/x\" name=\"x\"] mid-text"} {
		atts, out := Parse(text)
		assert.Nil(t, atts)
		assert.Equal(t, text, out)
	}
}

func TestParseConsumesOnlyTrailingBlock(t *testing.T) {
	block := "[llmsafespaces:attachment path=\"/workspace/uploads/11111111-2222-3333-4444-555555555555-a.txt\" name=\"a.txt\"]\n[llmsafespaces:attachment path=\"/workspace/uploads/22222222-3333-4444-5555-666666666666-b.txt\" name=\"b.txt\"]"
	text := "head\n" + block + "\ntail\n\n" + block + "\n\n"
	atts, out := Parse(text)
	require.Len(t, atts, 2)
	assert.Equal(t, "a.txt", atts[0].Name)
	assert.Equal(t, "b.txt", atts[1].Name)
	assert.Equal(t, "head\n"+block+"\ntail", out)
}

func TestParseBlockOnlyText(t *testing.T) {
	block := "[llmsafespaces:attachment path=\"/workspace/uploads/11111111-2222-3333-4444-555555555555-a.txt\" name=\"a.txt\"]\n"
	atts, out := Parse(block)
	require.Len(t, atts, 1)
	assert.Empty(t, out)
}
