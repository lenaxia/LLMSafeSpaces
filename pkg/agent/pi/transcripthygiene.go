// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package pi

import (
	"encoding/json"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
)

// transcripthygiene.go — the pi implementation of the #2030a
// TranscriptHygiene contract (pkg/agent/transcripthygiene.go). The
// pi driver owns what pi's sessions serve: the adapter composes the
// platform view from pi's persisted session entries
// (/workspace/pi-sessions files, read via get_entries) — the SERVED
// view strips identity stamps from tool-call arguments; the persisted
// files keep them byte-intact for attribution. Driver-side replay
// hygiene: every platform serve of those files (history reads, pages,
// the raw contract below) is stamp-free, so the pattern the model
// could pattern-continue never enters a served view. Existing
// sessions heal forward on the next serve; no migration, no rewrite.

// stripIdentityStampsJSON returns the hygiene-clean view of one
// tool-call arguments JSON document (agent.IdentityStampKeys deleted
// from a decoded top-level object). Byte-stable passthrough when the
// document is not an object, fails to decode, or carries no identity
// key — clean args never re-marshal.
func stripIdentityStampsJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return raw // never break the payload: non-object carriers pass through
	}
	if !agent.StripIdentityStampKeys(args) {
		return raw
	}
	out, err := json.Marshal(args)
	if err != nil {
		return raw
	}
	return out
}

// StripIdentityStamps is the pi TranscriptHygiene contract method
// over one SERVED transcript payload in pi's own wire shape — the
// get_entries entries array (entry objects whose message.content
// blocks of type "toolCall" carry the arguments). Identity keys are
// deleted from those argument objects; every other byte passes
// through untouched, and a payload carrying no stamp is returned
// byte-identical. Storage (the pi-sessions files) is never touched —
// attribution keeps reading the persisted record.
func (a *Adapter) StripIdentityStamps(served []byte) []byte {
	if len(served) == 0 || !json.Valid(served) {
		return served
	}
	var entries []map[string]any
	if err := json.Unmarshal(served, &entries); err != nil {
		return served
	}
	changed := false
	for _, entry := range entries {
		msg, ok := entry["message"].(map[string]any)
		if !ok {
			continue
		}
		blocks, ok := msg["content"].([]any)
		if !ok {
			continue // bare-string content carries no tool args
		}
		for _, b := range blocks {
			blk, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := blk["type"].(string); t != "toolCall" {
				continue
			}
			if args, ok := blk["arguments"].(map[string]any); ok {
				changed = agent.StripIdentityStampKeys(args) || changed
			}
		}
	}
	if !changed {
		return served
	}
	out, err := json.Marshal(entries)
	if err != nil {
		return served
	}
	return out
}
