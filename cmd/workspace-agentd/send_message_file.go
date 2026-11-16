// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// send_message_file.go — #2030b: file-mediated send_message bodies.
// Issue #2030's owner adjudication (PR #2032 r2): the unparseable-storm
// subclass is MEDIUM+ bodies at 3rd+ call position — long-prose
// emission inside a JSON string is the fragile shape (the model dies
// resuming structural emission after an escaped-JSON long string). The
// mitigation is the #1837 / design-0070 payload-file pattern applied to
// the message body: the model emits a SHORT file-path argument
// (proven-safe across thousands of board bodyFile/planFile calls) and
// the platform reads the body from the workspace file.
//
// Conventions carried verbatim from the board precedent
// (board_file_params.go):
//
//   - /exchange is the canonical dir; RELATIVE paths anchor there
//     (never the process cwd), absolute paths pass through;
//   - the flat contract: message files live DIRECTLY under the
//     exchange root — a nested file could be read but never
//     auto-deleted (the write tool's subdirectories lack the cross-uid
//     group-write bit an unlink needs);
//   - the loadImages read discipline: O_NONBLOCK open (FIFO wedge),
//     regular-file gate, stat AND read caps (TOCTOU), UTF-8 validation
//     (byte-identical delivery is impossible for non-UTF-8, so it is
//     refused loudly);
//   - the 256 KiB cap (boardPayloadFileMaxBytes — the established
//     payload-file budget);
//   - delete-after-success: the consumed file is unlinked once the
//     send has been scheduled; a delete failure folds a
//     cleanup_warning into the SUCCEEDED result, never fails it.
//
// Harness-agnostic BY CONSTRUCTION (owner ruling 2026-11-15): the file
// mediation ends at the composed text — the same mcpSendMessage
// delivers it for every harness. This file (and the dispatch arm that
// calls it) contains ZERO harness predicates — no harness-selector
// reads, no per-harness branches — pinned by
// TestMCPSendMessage_HarnessParity (the composed artifact crosses BOTH
// in-tree harness adapters byte-identically, and a source scan proves
// the send path never branches on the harness).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// dispatchSendMessage is the send_message dispatch arm (extracted from
// callMCPTool: the #2030b body-spelling admission pushed the switch's
// cyclomatic budget over its cap — the arm's complexity lives here,
// with the file mediation it mostly is). Harness-agnostic: the file arm
// is pure workspace I/O + the shared send path — no harness branch.
func dispatchSendMessage(ctx context.Context, password string, args map[string]any) (string, error) {
	// destination is canonical; session_id is its one-release legacy
	// alias, gated STRICTLY on destination's ABSENCE (the rename is not
	// a case fold, so argStr's wrong-type case cannot reach the alias
	// here): an explicitly-present destination — empty string or
	// mistyped — is authoritative and refuses loudly, never falling
	// through (the silent-precedence class the round-1 review killed
	// for file params).
	sessionID := argStr(args, "destination")
	if _, present := args["destination"]; !present {
		sessionID, _ = args["session_id"].(string)
	}
	// #2030b: the body's two spellings — message (inline; short bodies
	// stay first-class) and messageFile (a workspace file path whose
	// contents become the body; the payload-file discipline above, the
	// RECOMMENDED shape for long or structurally complex bodies — the
	// emission-shape mitigation). EXACTLY ONE OF THE TWO, resolved
	// loudly: both present refuses (a null value included — a mention
	// is a mention), neither defers to mcpSendMessage's required error.
	// Precedence is DOCUMENTED in the schema: there is no silent
	// winner.
	messageVal, hasMessage := args["message"]
	fileVal, hasFile := args["messageFile"]
	if hasMessage && hasFile {
		return "", fmt.Errorf("message and messageFile are mutually exclusive — pass exactly one body (messageFile for long or structurally complex prose: write it DIRECTLY under /exchange, then pass the path). Nothing was delivered and the file was not consumed")
	}
	var message string
	consumed := ""
	if hasFile {
		path, ok := fileVal.(string)
		if !ok || strings.TrimSpace(path) == "" {
			return "", fmt.Errorf("messageFile must be a non-empty workspace file path — write the body with the write tool DIRECTLY under /exchange (no subdirectories), then pass its path")
		}
		body, consumedPath, rerr := resolveMessageFile(path)
		if rerr != nil {
			return "", rerr
		}
		message, consumed = body, consumedPath
	} else {
		message, _ = messageVal.(string)
	}
	// Origin (#1465/#1469, #1849 tightening): lsp_injected_session is
	// the platform plugin's harness-attested injection — the ONLY
	// source. The former from_session_id self-declared fallback is GONE
	// from the schema and the seam: the agent is never responsible for
	// supplying its own return address (it was also an
	// emission-corruption surface). Injection OVERWRITES any
	// model-supplied copy (unconditional, last-write-wins at the
	// plugin); a degraded pod (plugin absent) refuses delivery with a
	// report-the-platform error rather than accepting an unattested
	// origin.
	injected, _ := args["lsp_injected_session"].(string)
	out, serr := mcpSendMessage(ctx, password, sessionID, message, injected)
	if serr != nil {
		// The consumed file SURVIVES a failed send: every validation
		// refusal above left it on disk, and so does this one — the
		// caller retries the same path without rewriting the body
		// (pinned).
		return "", serr
	}
	if hasFile {
		out = foldSendMessageResultFields(out, map[string]string{"message_source": "messageFile"})
		out = consumeMessageFile(out, consumed)
	}
	return out, nil
}

// sendMessageFileNotFoundHint is the missing-file error's second half:
// message files are CONSUMED (deleted) after a successful send, so a
// retry that re-references one may be re-sending an already-delivered
// message — the hint names the check (session_read the target) and
// WHERE message files live (the exchange dir — a relative path that
// misses it is the #1 confusion, same as the board twin).
const sendMessageFileNotFoundHint = "file not found — message files are deleted after a successful send; " +
	"if this was a retry, the message already went (session_read the target before re-sending rather than duplicating it). " +
	"Message files live DIRECTLY under " + agentd.ExchangePath + " (the payload exchange dir; no subdirectories); relative paths resolve against it"

// resolveMessageFile reads one message body file under the shared
// payload-file discipline and returns (body, consumedPath, err). The
// body is NOT trimmed here: mcpSendMessage applies the SAME trim to
// both sources, so a file-mediated body and a direct message of the
// same text deliver byte-identically (the round-trip pin).
func resolveMessageFile(path string) (string, string, error) {
	body, consumed, err := boardReadFileField("messageFile", path, boardPayloadFileMaxBytes, sendMessageFileNotFoundHint)
	if err != nil {
		return "", "", err
	}
	return body, consumed, nil
}

// consumeMessageFile deletes the consumed body file after a successful
// send — the board §2.4 cleanup semantics (what was read is what gets
// deleted; a delete failure WARNS via the result, it never fails the
// succeeded send). A validation failure upstream never reaches this
// hook: the file survives for the caller's retry (pinned).
func consumeMessageFile(result, consumed string) string {
	if consumed == "" {
		return result
	}
	if rmErr := os.Remove(consumed); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		return foldSendMessageResultFields(result, map[string]string{
			"cleanup_warning": fmt.Sprintf(
				"the message was sent but its body file could not be deleted: %v — remove %s yourself; the delivery has landed",
				rmErr, consumed),
		})
	}
	return result
}

// foldSendMessageResultFields merges string fields into mcpSendMessage's
// JSON result (message_source, cleanup_warning) — the board's
// boardInjectCleanupWarning fold, on the send_message result shape.
// A non-object result is wrapped, never dropped.
func foldSendMessageResultFields(result string, fields map[string]string) string {
	var obj map[string]any
	if err := json.Unmarshal([]byte(result), &obj); err == nil && obj != nil {
		for k, v := range fields {
			obj[k] = v
		}
		if b, mErr := json.Marshal(obj); mErr == nil {
			return string(b)
		}
	}
	wrapped := map[string]any{"status": "delivering", "body": result}
	for k, v := range fields {
		wrapped[k] = v
	}
	out, _ := json.Marshal(wrapped)
	return string(out)
}
