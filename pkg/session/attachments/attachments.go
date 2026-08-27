// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package attachments implements the v1 attachment manifest contract
// (Epic 67, design decisions D7/D8/D15): prompt text carrying file
// references the agent reads with its own tools.
//
// Manifest format v1 — one line per file, appended after the user text
// separated by exactly one blank line, each line:
//
//	[llmsafespaces:attachment path="<path>" name="<name>"]
//
// The format is a stable API contract locked by the golden fixtures in
// testdata/ (authoritative). Changes are additive-only; any format
// change requires a new version marker while this parser keeps
// supporting v1. Attribute values backslash-escape quotes (") and
// backslashes (\), and control characters are stripped from names, so a
// value can never break line structure. Compose is idempotent (D15):
// any pre-existing trailing v1 block is stripped before the new block is
// appended, so compose(compose(t, f), f) == compose(t, f). Parse
// consumes only the trailing block and treats unknown-version or
// unknown-attribute lines as plain text (forward compatibility). v1
// carries path and name only: send-time validation is shape-only (D8 —
// the server cannot stat workspace files), so the bytes field sketched
// in the epic design doc is superseded and intentionally absent.
package attachments

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

const (
	uploadsPrefix    = "/workspace/uploads/"
	uploadsPrefixLen = len(uploadsPrefix) + 36 + 1
	maxFiles         = 10
)

var (
	ErrTooManyFiles  = errors.New("too many files")
	ErrDuplicatePath = errors.New("duplicate file path")
	ErrInvalidPath   = errors.New("invalid attachment path")
)

var uploadPathPattern = regexp.MustCompile(
	`^/workspace/uploads/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}-`)

var attachmentLinePattern = regexp.MustCompile(
	`^\[llmsafespaces:attachment path="((?:\\\\|\\"|[^"\\])*)" name="((?:\\\\|\\"|[^"\\])*)"\]$`)

type Attachment struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

func ValidatePaths(files []string) error {
	if len(files) > maxFiles {
		return fmt.Errorf("%w: %d exceeds the maximum of %d", ErrTooManyFiles, len(files), maxFiles)
	}
	seen := make(map[string]struct{}, len(files))
	for i, p := range files {
		if p == "" || strings.TrimSpace(p) != p {
			return fmt.Errorf("%w at index %d: empty or whitespace-padded entry", ErrInvalidPath, i)
		}
		if !uploadPathPattern.MatchString(p) {
			return fmt.Errorf("%w at index %d", ErrInvalidPath, i)
		}
		for _, seg := range strings.Split(p, "/") {
			if seg == ".." {
				return fmt.Errorf("%w at index %d: '..' segment", ErrInvalidPath, i)
			}
		}
		if _, dup := seen[p]; dup {
			return fmt.Errorf("%w at index %d", ErrDuplicatePath, i)
		}
		seen[p] = struct{}{}
	}
	return nil
}

func Compose(text string, files []string) (string, error) {
	if len(files) == 0 {
		return text, nil
	}
	if err := ValidatePaths(files); err != nil {
		return "", err
	}
	var b strings.Builder
	if base := stripTrailingBlock(text); base != "" {
		b.WriteString(base)
		b.WriteString("\n\n")
	}
	for _, p := range files {
		name := sanitizeName(p[uploadsPrefixLen:])
		b.WriteString(`[llmsafespaces:attachment path="`)
		b.WriteString(escapeAttribute(p[:uploadsPrefixLen] + name))
		b.WriteString(`" name="`)
		b.WriteString(escapeAttribute(name))
		b.WriteString(`"]`)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func Parse(text string) ([]Attachment, string) {
	core := strings.TrimRight(text, "\n")
	if core == "" {
		return nil, text
	}
	lines := strings.Split(core, "\n")
	start := trailingBlockStart(lines)
	if start == len(lines) {
		return nil, text
	}
	end := start
	if end > 0 && lines[end-1] == "" {
		end--
	}
	atts := make([]Attachment, 0, len(lines)-start)
	for _, line := range lines[start:] {
		m := attachmentLinePattern.FindStringSubmatch(line)
		atts = append(atts, Attachment{
			Path: unescapeAttribute(m[1]),
			Name: unescapeAttribute(m[2]),
		})
	}
	return atts, strings.Join(lines[:end], "\n")
}

func stripTrailingBlock(text string) string {
	core := strings.TrimRight(text, "\n")
	if core == "" {
		return ""
	}
	lines := strings.Split(core, "\n")
	start := trailingBlockStart(lines)
	if start == len(lines) {
		return core
	}
	if start > 0 && lines[start-1] == "" {
		start--
	}
	return strings.Join(lines[:start], "\n")
}

func trailingBlockStart(lines []string) int {
	i := len(lines)
	for i > 0 && attachmentLinePattern.MatchString(lines[i-1]) {
		i--
	}
	return i
}

func sanitizeName(name string) string {
	clean := make([]rune, 0, len(name))
	for _, r := range name {
		if unicode.IsControl(r) {
			continue
		}
		clean = append(clean, r)
	}
	return string(clean)
}

func escapeAttribute(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v)
}

func unescapeAttribute(v string) string {
	return strings.NewReplacer(`\\`, `\`, `\"`, `"`).Replace(v)
}
