// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"encoding/json"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
)

// transcripthygiene.go — the opencode implementation of the #2030a
// TranscriptHygiene contract (pkg/agent/transcripthygiene.go): the
// SERVED view of opencode transcripts carries zero identity stamps in
// tool-call input args; the stored record (opencode's session DB)
// keeps them byte-intact for attribution. The replay-side complement
// of the accept-side strip (boardWireBody), and the #1374/#1307
// read-time-repair discipline applied to identity keys: repair the
// model's VIEW, keep the record — existing sessions heal forward on
// the next serve with no migration.
//
// Two serve tiers, one key set (agent.IdentityStampKeys):
//
//   - ToolPart translation (AcpToolCallFromNative / translateV2Tool):
//     every tool part the platform contract serves — history reads,
//     pages, SSE — strips the stamp from its input args JSON.
//   - Raw wire serves (StripIdentityStamps): one V1 message-list body
//     (the loopback seam's SessionMessagesRaw shape) with the stamps
//     removed from tool parts — the session_read surface's model-facing
//     emission, applied beside the #1307 image strip and the #1756
//     phrase rewrite (same never-break-the-payload rule).

// stripIdentityStampsJSON returns the hygiene-clean view of one
// tool-call input-args JSON document: the agent.IdentityStampKeys are
// deleted from a decoded top-level object. Byte-stable passthrough —
// the ORIGINAL bytes — when the document is not an object, fails to
// decode, or carries no identity key (clean args and non-JSON carriers
// never re-marshal, so cache-stable serves stay cache-stable).
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

// StripIdentityStamps is the opencode TranscriptHygiene contract
// over one SERVED transcript payload in the harness's own wire shape
// — the V1 message list the loopback seam returns (an array of
// message objects, each carrying parts; tool parts hold their input
// args in state.input (flat 1.18.x shape) or tool.input (legacy
// nested shape)). Identity keys are deleted from those input objects;
// every other byte passes through untouched, and a payload that
// carries no stamp is returned byte-identical. Storage is never
// touched — attribution keeps reading the stored record. The Adapter
// method (below) satisfies pkg/agent's TranscriptHygiene; this
// package-level form is the serving seam's direct entry (the
// StripImageDataURLs calling convention).
func StripIdentityStamps(served []byte) []byte {
	return stripIdentityStampsFromBody(served)
}

// StripIdentityStamps satisfies the registry-required TranscriptHygiene
// contract (pkg/agent/transcripthygiene.go) for the opencode adapter.
func (a *Adapter) StripIdentityStamps(served []byte) []byte {
	return stripIdentityStampsFromBody(served)
}

// stripIdentityStampsFromBody walks one V1 message-list body and
// strips identity keys from every tool part's input args. Shape-aware
// (only a tool part's state.input / tool.input are touched — never an
// arbitrary "input" key); never-break-the-payload on any decode error
// or non-array body (original bytes returned).
func stripIdentityStampsFromBody(body []byte) []byte {
	if len(body) == 0 || !json.Valid(body) {
		return body
	}
	var msgs []map[string]any
	if err := json.Unmarshal(body, &msgs); err != nil {
		return body
	}
	changed := false
	for _, msg := range msgs {
		parts, ok := msg["parts"].([]any)
		if !ok {
			continue
		}
		for _, p := range parts {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := part["type"].(string); t != "tool" {
				continue
			}
			// Flat 1.18.x shape: {"type":"tool","tool":"bash",
			// "state":{"status":...,"input":{...}}}.
			if state, ok := part["state"].(map[string]any); ok {
				if input, ok := state["input"].(map[string]any); ok {
					changed = agent.StripIdentityStampKeys(input) || changed
				}
			}
			// Legacy nested shape (≤1.15.x): {"type":"tool",
			// "tool":{"name":...,"input":{...},...}}.
			if tool, ok := part["tool"].(map[string]any); ok {
				if input, ok := tool["input"].(map[string]any); ok {
					changed = agent.StripIdentityStampKeys(input) || changed
				}
			}
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(msgs)
	if err != nil {
		return body
	}
	return out
}
