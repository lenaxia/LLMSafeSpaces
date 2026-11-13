// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Aged-arg HISTORY RESTRUCTURE (v3, 2026-11-13): the verified cure for
// the glm-5.3 tool-call emission tiling (#1949).
//
// Root behavior (60/60 events across three workspaces): the model
// pattern-continues its OWN prior tool-call structures in history.
// When it emits a new call of the same family, the generation latches
// into a duplicate-key/fragment-tiling loop that burns to the token
// cap (unparseable arguments, 16k+ tokens, ~100s per event).
//
// v3 renders prior tool usage as PLAIN MARKUP TEXT inside the assistant
// message that made the calls:
//
//	<original assistant content>
//	- Called `tool_name`(**param**: value, ...)
//	  - Result: <result content verbatim>
//
// replacing the assistant tool_calls entry and its paired role:"tool"
// messages. The model re-plans with ZERO tool_call JSON anywhere in its
// history — the emission template is gone — while parameters AND
// outputs stay fully visible (lossless, unlike v1 stubbing / v2
// dotting).
//
// Verified (exact-context replays via litellm, glm-5.3, 2026-11-13):
//
//	myriad (live fleet-cascade context):   baseline 1/1 RUNAWAY -> 3/3 CLEAN
//	quick-panda (send_message, 3/4 base):  3/3 CLEAN, correct calls emitted
//
// FAILURE POST-MORTEMS (preserved — do not regress):
//
//	v1 prose-note stub ({"note":"arguments removed by relay …"}) FAILED
//	in production: word-bearing values are the tiling attractor itself
//	(controlled pair on the live Nov-12 context: v1 stub → cap-burn,
//	{"note":"..."} → clean), and the v1 sentence leaked into 4 delivered
//	agent messages. v2 value-dotting ("..." over string values) was
//	cross-context clean (3/3 electric, 3/3 myriad) but LOSSY. v3
//	supersedes both at zero information cost.
//
// DETERMINISM CONTRACT (cache-critical): the rendering is a pure
// function of the message bytes — fixed bullet format, parameter order
// taken from the arguments JSON's OWN key order (decoded with a token
// decoder; never Go maps), numbers byte-preserved via json.Number,
// results verbatim, no timestamps, no counters. A restructured message
// renders identically on every request: prefix caching pays ONE bust
// per restructured message (a full bust at enablement) and is
// byte-stable thereafter.
//
// Scope: ALL tool calls are restructured — platform tools too. That is
// the validated configuration (the harness arms merged every family),
// and their parameters/outputs remain fully visible in the bullets.
//
// Config: BYO_AGED_ARG_RECENT_TOOL_CALLS < 0 disables (kill switch);
// 0 (default) restructures every tool call; > 0 keeps the last N tool
// calls (any tool, flattened across messages) in verbatim tool-call
// form (untested tuning knob — the validated setting is 0).
const (
	agedArgToolPrefix  = "llmsafespaces_"
	agedArgDefaultWins = 0

	agedArgBulletPrefix = "- Called `"
	agedArgResultPrefix = "  - Result: "
)

// agedArgStats reports what one restructure pass did (log/metric inputs).
type agedArgStats struct {
	CallsStubbed int  // tool calls restructured into prose
	BytesBefore  int  // request body bytes in
	BytesAfter   int  // request body bytes out
	Changed      bool // any restructure applied
}

// agedArgCompactor restructures prior tool usage into plain text in
// chat-completion request bodies. RecentToolCalls < 0 disables the
// transform entirely (kill switch); 0 restructures every tool call.
type agedArgCompactor struct {
	ToolPrefix string
	// RecentToolCalls is the number of most-recent tool calls (any tool,
	// flattened across messages) left in verbatim tool-call form.
	RecentToolCalls int
}

func newAgedArgCompactor() *agedArgCompactor {
	return &agedArgCompactor{ToolPrefix: agedArgToolPrefix, RecentToolCalls: agedArgDefaultWins}
}

func newAgedArgCompactorWindow(window int) *agedArgCompactor {
	return &agedArgCompactor{ToolPrefix: agedArgToolPrefix, RecentToolCalls: window}
}

// recentWindow reports the configured recent-window size (nil-safe —
// used in the restructure log line).
func (c *agedArgCompactor) recentWindow() int {
	if c == nil {
		return 0
	}
	return c.RecentToolCalls
}

// Compact returns the rewritten body. On any structural surprise the
// input is returned unchanged (the transform is best-effort; inference
// must never fail because of it).
func (c *agedArgCompactor) Compact(body []byte) ([]byte, agedArgStats) {
	stats := agedArgStats{BytesBefore: len(body), BytesAfter: len(body)}
	if c == nil || c.RecentToolCalls < 0 {
		return body, stats
	}
	// Fast pre-filter: nothing to do without any tool call in the body.
	if !bytes.Contains(body, []byte(`"tool_calls"`)) {
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
	// protected verbatim window. Window <= 0 protects nothing (0 =
	// restructure every tool call — the validated setting).
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

	// Pass 2 (forward): merge each assistant tool-call message and its
	// following role:"tool" results into one plain-text assistant
	// message. Unmatched trailing tool messages (no home) are left
	// untouched rather than dropped — never destroy what we cannot
	// pair; the wire tolerates a trailing tool message after an
	// assistant text message no worse than before.
	out := make([]json.RawMessage, 0, len(msgs))
	i := 0
	for i < len(msgs) {
		raw := msgs[i]
		calls, ok := toolCallsOf(raw)
		if !ok || len(calls) == 0 {
			out = append(out, raw)
			i++
			continue
		}
		// Any protected call in this message → keep the message verbatim
		// (and its results, which follow it untouched).
		anyProtected := false
		for ci := range calls {
			if protected[callPos{i, ci}] {
				anyProtected = true
				break
			}
		}
		if anyProtected {
			out = append(out, raw)
			i++
			// keep the directly following tool-result messages verbatim
			for i < len(msgs) {
				if role, ok := messageRole(msgs[i]); ok && role == "tool" {
					out = append(out, msgs[i])
					i++
					continue
				}
				break
			}
			continue
		}
		// Collect the paired results (immediately following tool msgs).
		j := i + 1
		results := make(map[string]json.RawMessage, len(calls))
		var order []string
		for j < len(msgs) {
			role, ok := messageRole(msgs[j])
			if !ok || role != "tool" {
				break
			}
			var tm map[string]json.RawMessage
			if err := json.Unmarshal(msgs[j], &tm); err != nil {
				break
			}
			id, ok := tm["tool_call_id"]
			if !ok {
				break
			}
			var idStr string
			if err := json.Unmarshal(id, &idStr); err != nil {
				break
			}
			results[idStr] = msgs[j]
			order = append(order, idStr)
			j++
		}
		rendered, n, err := renderCallsAsText(raw, calls, results, order)
		if err != nil || n == 0 {
			// Nothing renderable (or a surprise): leave this message and
			// its results exactly as they were.
			out = append(out, raw)
			i++
			continue
		}
		out = append(out, rendered)
		stats.CallsStubbed += n
		stats.Changed = true
		i = j
	}
	if !stats.Changed {
		return body, stats
	}
	outBody, err := marshalTop(top, out)
	if err != nil {
		return body, stats
	}
	stats.BytesAfter = len(outBody)
	return outBody, stats
}

// messageRole extracts the role of a message (unquoted).
func messageRole(msg json.RawMessage) (string, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(msg, &m); err != nil {
		return "", false
	}
	role, ok := m["role"]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(role, &s); err != nil {
		return "", false
	}
	return s, true
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

// renderCallsAsText builds the merged plain-text assistant message for
// one assistant tool-call message plus its paired results. n = number
// of calls actually rendered.
func renderCallsAsText(msg json.RawMessage, calls []json.RawMessage, results map[string]json.RawMessage, order []string) (json.RawMessage, int, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(msg, &m); err != nil {
		return nil, 0, err
	}
	var b strings.Builder
	if content, ok := m["content"]; ok {
		var cs string
		if err := json.Unmarshal(content, &cs); err == nil && strings.TrimSpace(cs) != "" {
			b.WriteString(strings.TrimSpace(cs))
			b.WriteString("\n")
		}
	}
	rendered := 0
	for ci, call := range calls {
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
		nameRaw, ok := f["name"]
		if !ok {
			continue
		}
		var name string
		if err := json.Unmarshal(nameRaw, &name); err != nil {
			continue
		}
		b.WriteString(agedArgBulletPrefix)
		b.WriteString(name)
		b.WriteString("`(")
		b.WriteString(renderArguments(f["arguments"]))
		b.WriteString(")\n")
		rendered++
		// result line: prefer the paired result in call order
		var idStr string
		if idRaw, ok := c["id"]; ok {
			_ = json.Unmarshal(idRaw, &idStr)
		}
		if res, ok := results[idStr]; ok {
			var tm map[string]json.RawMessage
			if err := json.Unmarshal(res, &tm); err == nil {
				if contentRaw, ok := tm["content"]; ok {
					var content string
					if err := json.Unmarshal(contentRaw, &content); err == nil {
						b.WriteString(agedArgResultPrefix)
						b.WriteString(content)
						b.WriteString("\n")
					}
				}
			}
		}
		_ = ci
		_ = order
	}
	if rendered == 0 {
		return nil, 0, fmt.Errorf("no renderable calls")
	}
	out, err := json.Marshal(map[string]string{
		"role":    "assistant",
		"content": b.String(),
	})
	if err != nil {
		return nil, 0, err
	}
	return out, rendered, nil
}

// renderArguments renders one tool-call arguments payload (a JSON
// string on the wire) as a deterministic parameter list: "**k**: v"
// comma-separated, in the payload's OWN key order (token-decoded —
// never Go maps), values as compact JSON with numbers byte-preserved
// via json.Number and HTML escaping disabled. Unparseable payloads
// render verbatim.
func renderArguments(raw json.RawMessage) string {
	var inner string
	if err := json.Unmarshal(raw, &inner); err != nil {
		// arguments absent or not a string: render the raw bytes
		return string(raw)
	}
	dec := json.NewDecoder(strings.NewReader(inner))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return inner
	}
	d, ok := tok.(json.Delim)
	if !ok || d != '{' {
		return inner
	}
	var parts []string
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			break
		}
		key, ok := kt.(string)
		if !ok {
			break
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			break
		}
		parts = append(parts, "**"+key+"**: "+compactJSON(val))
	}
	// consume the closing brace
	_, _ = dec.Token()
	return strings.Join(parts, ", ")
}

// compactJSON renders one value as compact JSON with HTML escaping
// disabled and numbers byte-preserved (json.Number) — deterministic.
func compactJSON(raw json.RawMessage) string {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	dec.Decode(&raw)
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(raw)
	return strings.TrimRight(buf.String(), "\n")
}

// marshalOrderedMap marshals m with the named keys first (in the given
// order) and any remaining keys after, alphabetically — deterministic
// output (Go maps otherwise randomize; determinism is what makes the
// transform stable across turns).
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
	slices.Sort(rest)
	for _, k := range rest {
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
	slices.Sort(out)
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
