// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// board_file_params.go — the file-backed board payload discipline.
// #1837 P1a established the read half (long STRING fields via file
// params, the loadImages discipline); the file-crud cutover  (design
// 0070) extends it to the whole structured payload: on the five
// payload-bearing manager tools (create/update task+epic,
// import_plan) the payload file (bodyFile/patchFile/planFile) is the
// ONLY payload path — inline payload arguments are refused at
// dispatch and removed from the tool schemas (the unsafe shape is
// unexpressible, not merely discouraged). The external MCP and REST
// surfaces stay file-free and inline (the #1837 ruling: only the
// in-workspace agent can resolve a workspace path — and their inline
// surface is programmatic, not model-emitted).
//
// The file-read discipline is loadImages' (PR #1364 review): regular
// files only (O_NONBLOCK open defeats the FIFO wedge), stat AND read
// caps (TOCTOU), plus UTF-8 validation — the wire is JSON and
// Postgres text; non-UTF-8 could not round-trip byte-identically, so
// it is refused loudly rather than silently replaced.
//
// Design 0070 §2.4a: file params anchor at the EXCHANGE root
// (/exchange — the dual-rw payload exchange dir), not the workspace
// root: the sidecar's /workspace mount is read-only (design 0055 M2),
// so a payload file there could never be auto-deleted (the live
// 2026-10-30 failure: every payload call cleanup_warning'd EROFS).
// Absolute /workspace paths stay readable for one release (compat
// arm) and warn with a pointer at the exchange dir when the delete
// fails.
//
// The #1837 single-field params (descriptionFile/outcomeFile and
// their resolver) are DELETED by the same cutover: a body/patch file
// already carries the long content they existed to carry.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// boardPayloadFileMaxBytes caps one payload file. 256 KiB total
// payload: the platform's largest single text cap is
// MaxDescriptionBytes (128 KiB); the doubling is documented headroom
// for a whole structured payload (title + tags + criteria +
// description) — each FIELD's own cap stays platform-authoritative.
const boardPayloadFileMaxBytes = 256 << 10

// boardFileExchangeRoot anchors RELATIVE file-param paths (design 0070
// §2.4a): the payload EXCHANGE dir — the one PVC subtree mounted RW in
// BOTH uid spaces (the runtime container's write tool creates payload
// files there; the agentd sidecar's cleanup hook unlinks consumed
// ones). The pre-§2.4a anchoring was the workspace root, which made
// the auto-delete contract structurally false in sidecar mode: the
// sidecar's /workspace mount is ReadOnly (design 0055 M2), so every
// payload call cleanup_warning'd EROFS. Absolute paths pass through
// (an absolute /workspace path is the one-release compat arm — read +
// pointed warning; see boardInjectCleanupWarning). A var (not the
// const) so tests can pin the anchoring without touching /exchange.
var boardFileExchangeRoot = agentd.ExchangePath

// boardFileWorkspaceRoot names the LEGACY payload location (the
// workspace root) for the compat predicate: an anchored path under it
// was written by a pre-§2.4a script, stays READABLE for one release,
// and cannot be auto-deleted in sidecar mode (the RO mount) — the
// cleanup warning points at the exchange dir instead of issuing a
// forever-manual rm chore. Also a var so tests can pin the predicate.
var boardFileWorkspaceRoot = agentd.WorkspacePath

// boardFileNotFoundHint is the missing-file error's second half: the
// auto-cleanup hook deletes consumed payload files after a successful
// CRUD call, so a RETRY of an already-landed call finds no file. The
// hint names the cause, both exits (re-write; confirm the landing
// before re-submitting), and WHERE payload files live (the exchange
// dir — a relative path that misses it is the #1 post-cutover
// confusion). "payloads are file-only" closes the door on the inline
// fallback the cutover removed.
const boardFileNotFoundHint = "file not found — files are deleted after a successful CRUD call; " +
	"if you already submitted it, re-write it; payloads are file-only " +
	"(if this was a retry, check board_list / board_read to confirm the create landed before re-submitting). " +
	"Payload files live under " + agentd.ExchangePath + " (the payload exchange dir); relative paths resolve against it"

// anchorBoardFilePath resolves one file-param path against the
// exchange root: absolute paths pass through, relative paths join the
// payload exchange dir (never the process cwd, never the workspace
// root — the sidecar cannot delete from there).
func anchorBoardFilePath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(boardFileExchangeRoot, path)
}

// boardPathIsLegacyWorkspace reports whether an anchored (absolute)
// payload path sits under the LEGACY workspace root — the compat arm:
// readable for one release, undeletable in sidecar mode, and the
// cleanup warning points at the exchange dir when the delete fails.
func boardPathIsLegacyWorkspace(anchored string) bool {
	if anchored == boardFileWorkspaceRoot {
		return false // the root itself is not a file param
	}
	rel, err := filepath.Rel(boardFileWorkspaceRoot, anchored)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// boardExchangeFlatContractFmt is the exchange dir's flatness rule:
// the runtime's write tool creates intermediate directories WITHOUT
// the cross-uid group-write bit (umask 022 → 0755), and an unlink's
// permission comes from the parent directory's write bit — so a
// payload file nested under /exchange subdirectories could be READ
// but never auto-DELETED (the sidecar's unlink gets EACCES even on
// the dual-rw mount). Review r1 finding 2: the motivating live
// failure itself shipped a nested path (reports/charters/p1.md); the
// anchoring must refuse that shape loudly instead of degrading back
// to the warning status quo. Formatted with the LIVE exchange root
// (the var) so overridden-root tests and production agree.
const boardExchangeFlatContractFmt = "payload files must live DIRECTLY under %s (no subdirectories): " +
	"the write tool creates subdirectories without the cross-uid group-write bit the platform's auto-delete needs — " +
	"a nested payload file could be read but never auto-deleted. Write the file flat, directly under the exchange root"

// refuseNestedExchangePath returns the flat-contract error message
// when an anchored payload path sits NESTED under the exchange root
// ("" when the path is fine: flat inside the exchange tree, outside
// it entirely — legacy workspace compat or absolute passthrough).
func refuseNestedExchangePath(anchored string) string {
	if anchored == boardFileExchangeRoot {
		return "" // the root itself is not a file param
	}
	rel, err := filepath.Rel(boardFileExchangeRoot, anchored)
	if err != nil {
		return ""
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "" // outside the exchange tree — not this rule's business
	}
	if strings.ContainsRune(rel, filepath.Separator) {
		return fmt.Sprintf(boardExchangeFlatContractFmt, boardFileExchangeRoot)
	}
	return ""
}

// isBoardFileParamKey reports whether an argument key is a
// file-param spelling (camelCase "…File" or legacy snake "…_file")
// — the stray-key guard's predicate. A file spelling on a tool that
// does not admit it is a caller mistake, refused loudly; never
// silently dropped onto the wire (a null value included: "I mentioned
// the param" is still a mention).
func isBoardFileParamKey(k string) bool {
	return strings.HasSuffix(k, "File") || strings.HasSuffix(k, "_file")
}

// boardReadFileField reads one workspace file under the loadImages
// discipline: O_NONBLOCK open (FIFO wedge), regular-file gate, stat
// AND read caps (TOCTOU), UTF-8 validation. key names the file param
// in error prose; the return carries the CONSUMED (anchored) path for
// the cleanup hook — what was read is what gets deleted.
func boardReadFileField(key, path string, maxBytes int64) (string, string, error) {
	anchored := anchorBoardFilePath(path)
	// The flat-contract gate (design 0070 §2.4a, review r1 finding 2):
	// a NESTED exchange path can never auto-delete — refuse at read
	// time, the same choke point every payload consumer (CRUD
	// resolver + board_validate_file) passes through.
	if msg := refuseNestedExchangePath(anchored); msg != "" {
		return "", "", fmt.Errorf("%s: %s", key, msg)
	}
	f, err := os.OpenFile(anchored, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // agent-supplied workspace path, gated below
	if err != nil {
		return "", "", boardFileReadError(key, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", "", fmt.Errorf("%s: unreadable: %w", key, err)
	}
	if !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("%s: %s is not a regular file (FIFOs, devices, and sockets are refused)", key, filepath.Base(anchored))
	}
	if info.Size() > maxBytes {
		return "", "", fmt.Errorf("%s: exceeds the %d KiB cap", key, maxBytes>>10)
	}
	// Cap the READ, not just the stat: stat-then-read is a TOCTOU.
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return "", "", fmt.Errorf("%s: unreadable: %w", key, err)
	}
	if int64(len(data)) > maxBytes {
		return "", "", fmt.Errorf("%s: exceeds the %d KiB cap", key, maxBytes>>10)
	}
	if !utf8.Valid(data) {
		return "", "", fmt.Errorf("%s: content is not valid UTF-8 — byte-identical storage is impossible (binary files are refused)", key)
	}
	return string(data), anchored, nil
}

// boardFileReadError renders an open failure, special-casing the
// not-found class with the cleanup hint (a retry after a successful
// call finds the file deleted — the message must say so instead of a
// bare ENOENT).
func boardFileReadError(key string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: %s", key, boardFileNotFoundHint)
	}
	return fmt.Errorf("%s: unreadable: %w", key, err)
}

// decodeBoardPayloadJSON strict-decodes one payload file: a single
// JSON document (no trailing bytes), duplicate keys refused at EVERY
// object depth (the emission-corruption class #1832 dogfooded is
// duplicated argument fragments — silently taking the last duplicate
// would store a payload the caller never reviewed), and the top level
// must be a JSON object. Numbers decode as json.Number so merged
// fields re-marshal byte-identically onto the wire.
func decodeBoardPayloadJSON(data []byte) (map[string]any, error) {
	if err := refuseDuplicateJSONKeys(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var fields map[string]any
	if err := dec.Decode(&fields); err != nil {
		// Only top-level shape errors reach here (syntax and duplicate
		// keys already died in the walk): a non-object document.
		return nil, fmt.Errorf("the top level must be a JSON object: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("trailing bytes after JSON value: %w", err)
		}
		return nil, errors.New("trailing bytes after JSON value")
	}
	if fields == nil {
		// JSON null (or an empty document) decodes to a nil map — a
		// payload file must BE an object.
		return nil, errors.New("the top level must be a JSON object")
	}
	return fields, nil
}

// refuseDuplicateJSONKeys token-walks one JSON document and refuses
// duplicate object keys at every depth. encoding/json silently keeps
// the LAST duplicate; a payload the caller cannot see whole is
// exactly the silent-corruption class this file exists to kill.
func refuseDuplicateJSONKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil // scalar
		}
		switch delim {
		case '{':
			seen := make(map[string]bool)
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return err
				}
				key, _ := keyTok.(string)
				if seen[key] {
					return fmt.Errorf("duplicate key %q — the JSON object carries two spellings of one field; emit each field exactly once", key)
				}
				seen[key] = true
				if err := walk(); err != nil {
					return err
				}
			}
			_, err := dec.Token() // consume the closing brace
			return err
		case '[':
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err := dec.Token() // consume the closing bracket
			return err
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	// Exactly one document: the walk must land at EOF.
	if _, err := dec.Token(); err != io.EOF {
		if err != nil {
			return fmt.Errorf("trailing bytes after JSON value: %w", err)
		}
		return errors.New("trailing bytes after JSON value")
	}
	return nil
}

// resolveBoardPayloadFile reads and strict-decodes one payload file
// (key names the file param in error prose). It does NOT touch args —
// the dispatcher owns the admission/required/inline guards and the
// merge; this returns the parsed fields plus the consumed (anchored)
// path for the cleanup hook.
func resolveBoardPayloadFile(key, path string) (map[string]any, string, error) {
	content, consumed, err := boardReadFileField(key, path, boardPayloadFileMaxBytes)
	if err != nil {
		return nil, "", err
	}
	fields, err := decodeBoardPayloadJSON([]byte(content))
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", key, err)
	}
	return fields, consumed, nil
}
