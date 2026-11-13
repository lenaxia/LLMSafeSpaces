// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
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
	// RecentToolCalls is the number of most-recent tool calls (any tool,
	// flattened across messages) left in verbatim tool-call form.
	RecentToolCalls int
}

func newAgedArgCompactor() *agedArgCompactor {
	return &agedArgCompactor{RecentToolCalls: agedArgDefaultWins}
}

func newAgedArgCompactorWindow(window int) *agedArgCompactor {
	return &agedArgCompactor{RecentToolCalls: window}
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
	// message — but ONLY when the whole span validates clean (see
	// tryRestructureSpan). Any anomaly → the span is forwarded
	// VERBATIM: lossless-by-construction beats cleverness (r1 review:
	// the v3 draft silently dropped orphaned/duplicate/non-string
	// results and array-shaped assistant content).
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
		// Find the span: the message + its directly following tool msgs.
		j := i + 1
		for j < len(msgs) {
			role, ok := messageRole(msgs[j])
			if !ok || role != "tool" {
				break
			}
			j++
		}
		merged, n, ok := tryRestructureSpan(raw, msgs[i:j], calls, protected, i)
		if !ok {
			// Anomaly (or fully protected): forward the span verbatim.
			out = append(out, msgs[i:j]...)
			i = j
			continue
		}
		out = append(out, merged)
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

// callPos identifies one tool call position (message index, call index)
// within the flattened history — the window bookkeeping key.
type callPos struct{ msg, idx int }

// tryRestructureSpan validates and renders one span (an assistant
// tool-call message plus its following tool messages) as a single
// plain-text assistant message. ok=false means the span carries ANY
// anomaly — protected calls, non-string assistant content, unpairable
// ids, duplicate ids, non-string ids, non-string result content, or a
// call missing its name — and must be forwarded verbatim. Lossless by
// construction: we never drop or reshape what we cannot perfectly
// pair (r1 review findings 1-5,7).
func tryRestructureSpan(msg json.RawMessage, span []json.RawMessage, calls []json.RawMessage, protected map[callPos]bool, msgIdx int) (json.RawMessage, int, bool) {
	// Window: a span holding ANY protected call stays verbatim (per-
	// message granularity — r1-reviewed; the validated production
	// setting is window 0 = protect nothing).
	for ci := range calls {
		if protected[callPos{msgIdx, ci}] {
			return nil, 0, false
		}
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(msg, &m); err != nil {
		return nil, 0, false
	}
	// Assistant content must be a string (or absent). Array/parts-shaped
	// content → passthrough (r1 finding 4).
	//
	// r2 finding 3: the assistant message must carry NO fields beyond
	// role/content/tool_calls — a real glm wire field (reasoning_content)
	// would be silently dropped by the {role, content} merge marshal.
	// Lossless means lossless: unknown fields → passthrough span.
	for k := range m {
		switch k {
		case "role", "content", "tool_calls":
		default:
			return nil, 0, false
		}
	}
	assistantPrefix := ""
	if contentRaw, ok := m["content"]; ok {
		var cs string
		if err := json.Unmarshal(contentRaw, &cs); err != nil {
			return nil, 0, false
		}
		if strings.TrimSpace(cs) != "" {
			assistantPrefix = strings.TrimSpace(cs)
		}
	}

	// Parse every call: name (string), arguments (string), id (string).
	type parsedCall struct {
		id, name, argsBullet string
	}
	parsed := make([]parsedCall, 0, len(calls))
	for _, call := range calls {
		var c map[string]json.RawMessage
		if err := json.Unmarshal(call, &c); err != nil {
			return nil, 0, false
		}
		fn, ok := c["function"]
		if !ok {
			return nil, 0, false
		}
		var f map[string]json.RawMessage
		if err := json.Unmarshal(fn, &f); err != nil {
			return nil, 0, false
		}
		var name string
		nameRaw, hasName := f["name"]
		if !hasName || json.Unmarshal(nameRaw, &name) != nil || name == "" {
			return nil, 0, false // r1 finding 5: unnameable call → passthrough span
		}
		var id string
		idRaw, hasID := c["id"]
		if !hasID || json.Unmarshal(idRaw, &id) != nil || id == "" {
			return nil, 0, false // r1 finding 5: non-string/absent id → passthrough span
		}
		argsRaw, hasArgs := f["arguments"]
		if !hasArgs {
			return nil, 0, false
		}
		var argsInner string
		if err := json.Unmarshal(argsRaw, &argsInner); err != nil {
			return nil, 0, false
		}
		parsed = append(parsed, parsedCall{id: id, name: name, argsBullet: renderArguments(argsInner)})
	}

	// Pair results: every tool message in the span must carry a string
	// tool_call_id matching exactly one parsed call, exactly once, with
	// string content. Anything else → passthrough span (r1 findings
	// 1,2,3,7).
	resultByID := make(map[string]string, len(span))
	seenIDs := make(map[string]bool, len(span))
	for _, t := range span[1:] {
		var tm map[string]json.RawMessage
		if err := json.Unmarshal(t, &tm); err != nil {
			return nil, 0, false
		}
		idRaw, ok := tm["tool_call_id"]
		if !ok {
			return nil, 0, false // r1 finding 7: id-less tool message → passthrough
		}
		// r2 finding 2: JSON null into *string unmarshals as a no-op —
		// a null id is PRESENT-but-invalid: reject explicitly (the v3
		// draft landed it in resultByID[""], unreachable → deleted).
		var idPtr *string
		if err := json.Unmarshal(idRaw, &idPtr); err != nil || idPtr == nil {
			return nil, 0, false
		}
		id := *idPtr
		if id == "" {
			return nil, 0, false
		}
		if seenIDs[id] {
			return nil, 0, false // r1 finding 2: duplicate result id → passthrough
		}
		seenIDs[id] = true
		contentRaw, ok := tm["content"]
		if !ok {
			return nil, 0, false // r1 finding 3: absent content → passthrough
		}
		// r2 finding 7: null content is PRESENT-but-invalid — same
		// null-into-string no-op class as the id. Passthrough.
		var contentPtr *string
		if err := json.Unmarshal(contentRaw, &contentPtr); err != nil || contentPtr == nil {
			return nil, 0, false
		}
		resultByID[id] = *contentPtr
	}
	// r2 finding 1: the pairing must be EXACTLY bijective — every result
	// consumed (this direction was missing; an orphan riding alongside a
	// complete pair was silently deleted) and every call paired.
	if len(resultByID) != len(parsed) {
		return nil, 0, false
	}
	for _, pc := range parsed {
		if _, ok := resultByID[pc.id]; !ok {
			return nil, 0, false
		}
	}
	// r2 finding 7: duplicate CALL ids — one result would render on both
	// bullets. Any duplicate call id → passthrough.
	seenCalls := make(map[string]bool, len(parsed))
	for _, pc := range parsed {
		if seenCalls[pc.id] {
			return nil, 0, false
		}
		seenCalls[pc.id] = true
	}

	// Render.
	var b strings.Builder
	if assistantPrefix != "" {
		b.WriteString(assistantPrefix)
		b.WriteString("\n")
	}
	rendered := 0
	for _, pc := range parsed {
		b.WriteString(agedArgBulletPrefix)
		b.WriteString(pc.name)
		b.WriteString("`(")
		b.WriteString(pc.argsBullet)
		b.WriteString(")\n")
		b.WriteString(agedArgResultPrefix)
		b.WriteString(resultByID[pc.id])
		b.WriteString("\n")
		rendered++
	}
	// The span was validated to carry ONLY role/content/tool_calls, so
	// {role, content} is lossless here (r2 finding 3 handled by the
	// unknown-field gate above, not by a wider marshal). Marshal with
	// HTML escaping DISABLED — matching the determinism contract and the
	// python mirror's ensure_ascii=False (r2 finding 5: <, &, > ride the
	// wire raw, not as \u003c escapes).
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]string{
		"role":    "assistant",
		"content": b.String(),
	}); err != nil {
		return nil, 0, false
	}
	out := json.RawMessage(strings.TrimRight(buf.String(), "\n"))
	return out, rendered, true
}

// renderArguments renders one tool-call arguments payload (a JSON string
// on the wire) as a deterministic parameter list: "**k**: v"
// comma-separated, in the payload's OWN key order (token-decoded —
// never Go maps), duplicate keys COLLAPSED last-wins-first-position
// (matching the python mirror's dict semantics — and removing the
// duplicate-key attractor rather than re-rendering it, r1 finding 8),
// values as compact JSON with numbers byte-preserved (json.Number) and
// HTML escaping disabled. TOTAL function: ANY parse deviation — a
// non-object payload, a mid-payload break, or trailing content after
// the object (r1 finding 6's `{"…"} GARBAGE` splice shape) — renders
// the RAW inner text verbatim, never a partial bullet and never an
// error upward. Lossless and deterministic on every input.
func renderArguments(inner string) string {
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
	var order []string
	values := make(map[string]json.RawMessage)
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return inner
		}
		key, ok := kt.(string)
		if !ok {
			return inner
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return inner
		}
		if _, seen := values[key]; !seen {
			order = append(order, key)
		}
		values[key] = val // last value wins, first position kept
	}
	if _, err := dec.Token(); err != nil { // closing brace
		return inner
	}
	// Anything after the closing brace — a trailing token, or garbage
	// the decoder would ERROR on (e.g. `]`: More() is false and Token()
	// errors, so neither decoder probe catches it — r1 finding 4) —
	// means a non-object payload: raw verbatim. Byte-offset peek, total.
	if rest := strings.TrimLeft(inner[dec.InputOffset():], " \t\r\n"); rest != "" {
		return inner
	}
	parts := make([]string, 0, len(order))
	for _, k := range order {
		pv, err := compactJSON(values[k])
		if err != nil {
			return inner
		}
		parts = append(parts, "**"+k+"**: "+pv)
	}
	return strings.Join(parts, ", ")
}

// compactJSON renders one value as compact JSON with HTML escaping
// disabled and numbers byte-preserved (json.Number) — deterministic.
func compactJSON(raw json.RawMessage) (string, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return "", err
	}
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(raw); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
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
