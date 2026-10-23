// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// board_file_params.go — #1837 P1a: file-backed long fields on the
// agent manager surface. The emission-corruption class (#1832 dogfood)
// lives in >2KB INLINE string arguments; a workspace file path is a
// short argument whose CONTENT rides to the platform verbatim. The
// external MCP and REST surfaces deliberately do NOT get *_file params
// (the #1837 review ruling: only the in-workspace agent can resolve a
// workspace path — the loadImages precedent).
//
// The file-read discipline is loadImages' (PR #1364 review): regular
// files only (O_NONBLOCK open defeats the FIFO wedge), stat AND read
// caps (TOCTOU), plus UTF-8 validation — the wire is JSON and Postgres
// text; non-UTF-8 could not round-trip byte-identically, so it is
// refused loudly rather than silently replaced.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"
)

// boardFileFieldMaxBytes caps one file-backed field's content. It
// matches the service's largest text cap (MaxDescriptionBytes, 128
// KiB): the platform-side cap stays the single authority.
const boardFileFieldMaxBytes = 128 << 10

// resolveBoardFileArgs resolves the <field>_file param into the inline
// <field> string on args, consuming the file spelling (no *_file
// spelling ever reaches the wire — the platform API has no such key).
// targetField is the ONE field this tool admits (description_file on
// task create/update; outcome_file on epic create/update); an empty
// targetField admits NO file param at all. Any *_file key for another
// field — or on a no-file tool — is a caller mistake, refused loudly;
// never silently dropped onto the wire.
func resolveBoardFileArgs(args map[string]any, targetField string) error {
	admitted := targetField + "_file"
	if targetField == "" {
		admitted = "_" // no admitted spelling: every *_file key is foreign
	}
	for k := range args {
		// A *_file key for another field is a caller mistake, refused
		// loudly — never silently dropped onto the wire (a null value
		// included: "I mentioned the param" is still a mention).
		if strings.HasSuffix(k, "_file") && k != admitted {
			return fmt.Errorf("%s is not admitted here (this tool accepts %s) — refusing rather than silently dropping it", k, admitted)
		}
	}
	raw, present := args[admitted]
	if !present {
		return nil
	}
	if raw == nil {
		// A null mention is still a mention of the param — consume it
		// so no *_file spelling ever reaches the wire.
		delete(args, admitted)
		return nil
	}
	path, ok := raw.(string)
	if !ok || strings.TrimSpace(path) == "" {
		return fmt.Errorf("%s must be a non-empty workspace file path", admitted)
	}
	// Ambiguity is refused: an inline field alongside its file param
	// would silently resolve to whichever wins — exactly the silent-
	// corruption class this param exists to kill.
	if inline, ok := args[targetField]; ok {
		switch v := inline.(type) {
		case string:
			if v != "" {
				return fmt.Errorf("%s and %s are mutually exclusive — send one or the other", targetField, admitted)
			}
		case nil:
			// absent/JSON-null: the file wins
		default:
			return fmt.Errorf("%s and %s are mutually exclusive (the inline %s is neither string nor null)", targetField, admitted, targetField)
		}
	}
	content, err := readBoardFileField(path)
	if err != nil {
		return fmt.Errorf("%s: %w", admitted, err)
	}
	args[targetField] = content
	delete(args, admitted)
	return nil
}

// readBoardFileField reads one workspace file under the loadImages
// discipline: O_NONBLOCK open (FIFO wedge), regular-file gate, stat
// AND read caps, UTF-8 validation.
func readBoardFileField(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // agent-supplied workspace path, gated below
	if err != nil {
		return "", fmt.Errorf("unreadable: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("unreadable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file (FIFOs, devices, and sockets are refused)", filepath.Base(path))
	}
	if info.Size() > boardFileFieldMaxBytes {
		return "", fmt.Errorf("exceeds the %d KiB cap", boardFileFieldMaxBytes>>10)
	}
	// Cap the READ, not just the stat: stat-then-read is a TOCTOU.
	data, err := io.ReadAll(io.LimitReader(f, boardFileFieldMaxBytes+1))
	if err != nil {
		return "", fmt.Errorf("unreadable: %w", err)
	}
	if len(data) > boardFileFieldMaxBytes {
		return "", fmt.Errorf("exceeds the %d KiB cap", boardFileFieldMaxBytes>>10)
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("content is not valid UTF-8 — byte-identical storage is impossible (binary files are refused)")
	}
	return string(data), nil
}
