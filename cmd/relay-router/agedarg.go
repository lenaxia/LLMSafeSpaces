// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
)

// Aged-arg compaction (#1949): the poisoned-context emission CURE.
//
// #1929's replay evidence split the defense: schema reordering is
// PREVENTION (fresh sessions emit the bulk-last shape), but a context
// whose history already carries old-shape bulk-first llmsafespaces_*
// tool-call arguments pattern-continues into duplicate-key/tiling
// runaways EVEN with the new schemas live (their last row: RUNAWAY 3/3;
// production confirmations 2026-10-25). Their row 4 proved the cure:
// stub the prior args (CLEAN 3/3). This transform implements that cure
// in the BYO resolve router's request path.
//
// Semantics (security-sensitive: this rewrites what the model sees of
// its own history):
//   - STUB the `arguments` of every llmsafespaces_* tool call OLDER than
//     the recent window. Call identity (id), tool name, and outcome
//     semantics (role:"tool" result messages) are preserved untouched.
//   - The recent window is the last N tool calls of ANY tool (positional,
//     flattened across messages): active-turn work keeps full fidelity —
//     no semantic change to recent calls.
//   - Never touches non-llmsafespaces_* tools (platform tools are the
//     proven-safe precedent), tool RESULTS, the tools array, or anything
//     outside messages[].tool_calls.
//   - Audit-preserving: the router never persists bodies (K7) — the
//     on-disk session store in the workspace keeps every original arg.
//   - Passthrough-safe: no llmsafespaces_* name in the body, no messages
//     array, or anything unexpected → the body is forwarded byte-identical.
//     Compaction never rejects or breaks a request.
//   - Idempotent: the stub replaces itself byte-identically on re-runs
//     (the client re-sends its stored history every turn; the router
//     re-compacts each time — output is stable).
//   - Loudly observable: callers log (metadata-only, K7) and count via
//     metrics whenever compaction fires — silent history rewriting is
//     the failure mode to avoid.
const (
	agedArgToolPrefix  = "llmsafespaces_"
	agedArgDefaultWins = 30

	// agedArgStubNote mirrors the checked-in replay harness's stub
	// (scripts/1949-aged-arg-replay.py) byte-for-byte — the parity pin
	// lives in TestAgedArgStubMatchesReplayHarness.
	agedArgStubNote = "arguments removed by relay aged-arg compaction (full text preserved in the session audit trail)"
)

// agedArgStub is the replacement arguments payload: one short note, the
// single-field bulk-last-safe shape (nothing for emission to resume
// mid-skeleton — the poison was the bulk-first array/string skeleton).
func agedArgStub() []byte {
	stub, _ := json.Marshal(map[string]string{"note": agedArgStubNote})
	return stub
}

// agedArgStats reports what one compaction pass did (log/metric inputs).
type agedArgStats struct {
	CallsStubbed int  // llmsafespaces_* tool-call arguments replaced
	BytesBefore  int  // request body bytes in
	BytesAfter   int  // request body bytes out
	Changed      bool // any stub applied
}

// agedArgCompactor stubs aged llmsafespaces_* tool-call arguments in
// chat-completion request bodies. RecentToolCalls < 0 disables the
// compaction entirely (kill switch); 0 stubs every llmsafespaces_* call.
type agedArgCompactor struct {
	ToolPrefix string
	// RecentToolCalls is the number of most-recent tool calls (any tool,
	// flattened across messages) left untouched.
	RecentToolCalls int
}

func newAgedArgCompactor() *agedArgCompactor {
	return &agedArgCompactor{ToolPrefix: agedArgToolPrefix, RecentToolCalls: agedArgDefaultWins}
}

func newAgedArgCompactorWindow(window int) *agedArgCompactor {
	return &agedArgCompactor{ToolPrefix: agedArgToolPrefix, RecentToolCalls: window}
}

// recentWindow reports the configured recent-window size (nil-safe —
// used in the compaction log line).
func (c *agedArgCompactor) recentWindow() int {
	if c == nil {
		return 0
	}
	return c.RecentToolCalls
}

// Compact returns the rewritten body. On any structural surprise the
// input is returned unchanged (Compaction is best-effort; inference
// must never fail because of it).
func (c *agedArgCompactor) Compact(body []byte) ([]byte, agedArgStats) {
	stats := agedArgStats{BytesBefore: len(body), BytesAfter: len(body)}
	if c == nil || c.RecentToolCalls < 0 || c.ToolPrefix == "" {
		return body, stats
	}
	// Fast pre-filter: nothing to do unless a prefixed tool name appears.
	if !bytes.Contains(body, []byte(`"`+c.ToolPrefix)) {
		return body, stats
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body, stats
	}
	rawMsgs, ok := top["messages"]
	if !ok {
		return body, stats
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(rawMsgs, &msgs); err != nil {
		return body, stats
	}

	// Pass 1 (backward): flatten tool-call positions to decide the
	// protected recent window. Positions are (message, call-index).
	// Window <= 0 protects nothing (0 = stub every llmsafespaces_* call).
	type callPos struct{ msg, idx int }
	var flat []callPos
	if c.RecentToolCalls > 0 {
		for mi := len(msgs) - 1; mi >= 0; mi-- {
			calls, ok := toolCallsOf(msgs[mi])
			if !ok {
				continue
			}
			for ci := len(calls) - 1; ci >= 0; ci-- {
				flat = append(flat, callPos{mi, ci})
				if len(flat) == c.RecentToolCalls {
					break
				}
			}
			if len(flat) == c.RecentToolCalls {
				break
			}
		}
	}
	protected := make(map[callPos]bool, len(flat))
	for _, p := range flat {
		protected[p] = true
	}

	// Pass 2 (forward): rewrite aged llmsafespaces_* calls.
	changed := false
	for mi, raw := range msgs {
		calls, ok := toolCallsOf(raw)
		if !ok {
			continue
		}
		needStub := false
		for ci, call := range calls {
			if protected[callPos{mi, ci}] {
				continue
			}
			name, ok := toolCallName(call)
			if ok && bytes.HasPrefix(name, []byte(c.ToolPrefix)) {
				needStub = true
			}
		}
		if !needStub {
			continue
		}
		rewritten, n, err := stubAgedCalls(raw, c.ToolPrefix, func(mi2, ci int) bool {
			return protected[callPos{mi2, ci}]
		}, mi)
		if err != nil {
			continue // malformed entry: leave the whole message untouched
		}
		msgs[mi] = rewritten
		stats.CallsStubbed += n
		changed = changed || n > 0
	}
	if !changed {
		return body, stats
	}
	out, err := marshalTop(top, msgs)
	if err != nil {
		return body, stats
	}
	stats.BytesAfter = len(out)
	stats.Changed = true
	return out, stats
}

// toolCallsOf reports the raw tool_calls array of a message, if any.
func toolCallsOf(msg json.RawMessage) ([]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(msg, &m); err != nil {
		return nil, false
	}
	raw, ok := m["tool_calls"]
	if !ok {
		return nil, false
	}
	var calls []json.RawMessage
	if err := json.Unmarshal(raw, &calls); err != nil {
		return nil, false
	}
	return calls, true
}

// toolCallName extracts function.name from one tool-call entry (unquoted).
func toolCallName(call json.RawMessage) ([]byte, bool) {
	var c map[string]json.RawMessage
	if err := json.Unmarshal(call, &c); err != nil {
		return nil, false
	}
	fn, ok := c["function"]
	if !ok {
		return nil, false
	}
	var f map[string]json.RawMessage
	if err := json.Unmarshal(fn, &f); err != nil {
		return nil, false
	}
	name, ok := f["name"]
	if !ok {
		return nil, false
	}
	var nameStr string
	if err := json.Unmarshal(name, &nameStr); err != nil {
		return nil, false
	}
	return []byte(nameStr), true
}

// stubAgedCalls rewrites one message: every non-protected tool call whose
// function.name carries the prefix gets arguments = the stub. All other
// bytes of the message (content, ids, names, other fields, non-matching
// calls) are re-emitted verbatim.
func stubAgedCalls(msg json.RawMessage, prefix string, isProtected func(msgIdx, callIdx int) bool, msgIdx int) (json.RawMessage, int, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(msg, &m); err != nil {
		return nil, 0, err
	}
	var calls []json.RawMessage
	if err := json.Unmarshal(m["tool_calls"], &calls); err != nil {
		return nil, 0, err
	}
	stubbed := 0
	for ci, call := range calls {
		if isProtected(msgIdx, ci) {
			continue
		}
		var c map[string]json.RawMessage
		if err := json.Unmarshal(call, &c); err != nil {
			return nil, 0, err
		}
		fn, ok := c["function"]
		if !ok {
			continue
		}
		var f map[string]json.RawMessage
		if err := json.Unmarshal(fn, &f); err != nil {
			continue
		}
		name, ok := f["name"]
		if !ok {
			continue
		}
		var nameStr string
		if err := json.Unmarshal(name, &nameStr); err != nil || !bytes.HasPrefix([]byte(nameStr), []byte(prefix)) {
			continue
		}
		// Replace arguments (whatever it was — string, object, or absent)
		// with the stub JSON string, preserving every other field.
		wireStub, err := json.Marshal(string(agedArgStub()))
		if err != nil {
			return nil, 0, err
		}
		if bytes.Equal(bytes.TrimSpace(f["arguments"]), wireStub) {
			continue // already stubbed (idempotent across re-sent turns)
		}
		f["arguments"] = json.RawMessage(wireStub)
		newFn, err := marshalOrderedMap(f, []string{"name", "arguments"})
		if err != nil {
			return nil, 0, err
		}
		c["function"] = newFn
		newCall, err := marshalOrderedMap(c, []string{"id", "type", "function"})
		if err != nil {
			return nil, 0, err
		}
		calls[ci] = newCall
		stubbed++
	}
	newCalls, err := json.Marshal(calls)
	if err != nil {
		return nil, 0, err
	}
	m["tool_calls"] = newCalls
	// Message shape on the wire: role/content first, tool_calls last.
	out, err := marshalOrderedMap(m, []string{"role", "content", "tool_calls"})
	if err != nil {
		return nil, 0, err
	}
	return out, stubbed, nil
}

// agedArgStubJSONString is the stub object serialized as a JSON string
// literal (arguments is a string on the OpenAI wire).
func agedArgStubJSONString() string {
	wire, _ := json.Marshal(string(agedArgStub()))
	return string(wire)
}

// marshalOrderedMap marshals m with the named keys first (in the given
// order) and any remaining keys after, alphabetically — deterministic
// output (Go maps otherwise randomize; determinism is what makes the
// transform idempotent across turns).
func marshalOrderedMap(m map[string]json.RawMessage, first []string) (json.RawMessage, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	used := make(map[string]bool, len(m))
	i := 0
	for _, k := range first {
		v, ok := m[k]
		if !ok {
			continue
		}
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(v)
		used[k] = true
		i++
	}
	rest := make([]string, 0, len(m))
	for k := range m {
		if !used[k] {
			rest = append(rest, k)
		}
	}
	// insertion order for the rest: sort for determinism
	for _, k := range sortedStringKeys(rest) {
		v := m[k]
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(v)
		i++
	}
	buf.WriteByte('}')
	return json.RawMessage(buf.Bytes()), nil
}

func sortedStringKeys(keys []string) []string {
	out := append([]string(nil), keys...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// marshalTop re-serializes the top-level body with messages replaced.
// Field order: messages LAST (bulk-last on the wire — the write/bash
// precedent; never re-introduce a bulk-first seam at the top level).
func marshalTop(top map[string]json.RawMessage, msgs []json.RawMessage) ([]byte, error) {
	m := top
	if msgs != nil {
		m = make(map[string]json.RawMessage, len(top))
		for k, v := range top {
			m[k] = v
		}
		newMsgs, err := json.Marshal(msgs)
		if err != nil {
			return nil, err
		}
		m["messages"] = newMsgs
	}
	first := make([]string, 0, len(m))
	for k := range m {
		if k != "messages" {
			first = append(first, k)
		}
	}
	out, err := marshalOrderedMap(m, sortedStringKeys(first))
	if err != nil {
		return nil, err
	}
	return out, nil
}
