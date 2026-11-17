// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import "fmt"

// transcripthygiene.go — the #2030a transcript-hygiene contract: the
// replay-side complement of the accept-side identity strip.
//
// Mechanism (issue #2030, comment 'Live instance 2026-11-15'): the
// platform's MCP boundary stamps every platform tool call with the
// calling session's identity (lsp_injected_session; for opencode the
// first-party origin plugin inside the pinned harness, mcp_server.go's
// registry reads it back for attribution). Stamped args PERSIST into
// the harness's session history, and every later turn REPLAYS the
// stamp into model context — the model pattern-continues it (benign
// extra-field most times; value-replacement into a semantic param
// under the bleed condition, live-verified on board_import_plan's
// projectId).
//
// The hygiene rule (owner ruling 2026-11-15, harness-agnostic): the
// SERVED view of a transcript carries ZERO identity keys in tool-call
// input args; the STORED record keeps them byte-intact for
// attribution — the #1374/#1307 read-time-repair precedent (repair
// the model's VIEW, keep the record). Existing sessions heal forward
// on the next serve with no migration.
//
// The contract lives at the harness-provider surface (the S5
// registry-built Adapter seam): every harness adapter implements
// TranscriptHygiene over its OWN transcript wire shape, so a third
// harness complies by implementing the same adapter contract.
// NewAdapterForType enforces it at construction — an adapter without
// the contract fails loudly there (startup-fatal for a default
// harness, a per-call error for a lazily constructed one), never a
// silent per-harness gap.

// IdentityStampKeys are the identity keys stripped from tool-call
// input args in every SERVED transcript view: the platform-namespace
// stamp plus the two legacy spellings tools once accepted. This is
// the single source of truth for the key SET; the accept-side mirror
// (boardWireBody in cmd/workspace-agentd/board_manager_tools.go)
// strips the same three before platform wire submission, and
// boardIdentityInlineKeys lists them for the file-crud inline
// resolver. All three sites must stay in lockstep.
var IdentityStampKeys = []string{"lsp_injected_session", "sessionId", "session_id"}

// IsIdentityStampKey reports whether k is one of IdentityStampKeys.
func IsIdentityStampKey(k string) bool {
	for _, key := range IdentityStampKeys {
		if k == key {
			return true
		}
	}
	return false
}

// StripIdentityStampKeys deletes the identity keys from a decoded
// tool-args map in place and reports whether anything changed. Pure
// data operation, harness-agnostic; both in-tree implementations and
// their tests build on it so the key set has one enforcement point.
func StripIdentityStampKeys(args map[string]any) bool {
	if len(args) == 0 {
		return false
	}
	changed := false
	for _, key := range IdentityStampKeys {
		if _, ok := args[key]; ok {
			delete(args, key)
			changed = true
		}
	}
	return changed
}

// TranscriptHygiene is the per-harness transcript-serving contract
// (#2030a). StripIdentityStamps returns the SERVED view of one
// transcript payload in the harness's OWN wire shape with the
// IdentityStampKeys removed from tool-call input arguments.
//
// Contract rules (the #1307 strip's discipline):
//   - STORAGE IS NEVER TOUCHED — the input bytes are a read-side
//     copy; attribution consumers keep reading the stored record.
//   - Never break the payload: on any decode failure or when nothing
//     was stripped, the input bytes are returned unchanged.
//   - Idempotent and order-independent with the other serving
//     transforms (image stripping, phrase rewriting).
type TranscriptHygiene interface {
	StripIdentityStamps(served []byte) []byte
}

// TranscriptHygieneFor resolves an adapter's TranscriptHygiene
// contract, failing LOUDLY (naming the concrete type) when the
// adapter does not provide it — the no-silent-gaps rule. Construction
// seams call this; callers that already hold a TranscriptHygiene
// never re-assert.
func TranscriptHygieneFor(a Adapter) (TranscriptHygiene, error) {
	if a == nil {
		return nil, fmt.Errorf("transcript hygiene: nil adapter cannot provide the #2030a contract")
	}
	h, ok := a.(TranscriptHygiene)
	if !ok {
		return nil, fmt.Errorf("transcript hygiene: adapter %T does not implement TranscriptHygiene (#2030a: served transcripts must strip identity stamps from tool-call args — no silent per-harness gaps)", a)
	}
	return h, nil
}
