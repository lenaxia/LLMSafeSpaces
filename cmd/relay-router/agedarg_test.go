// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// agedArgTestBody builds a body with nAged aged llmsafespaces calls
// (each followed by a bash call) and nRecent fresh llmsafespaces calls.
func agedArgTestBody(nAged, nRecent int) []byte {
	var msgs []string
	add := func(s string) { msgs = append(msgs, s) }
	bulk := strings.Repeat("charter prose for the aged bulk payload. ", 60)
	add(`{"role":"user","content":"begin"}`)
	for i := 0; i < nAged; i++ {
		add(`{"role":"assistant","content":"working","tool_calls":[{"id":"call_old_` + itoa(i) + `","type":"function","function":{"name":"llmsafespaces_board_create_task","arguments":"{\"acceptanceCriteria\":[{\"text\":\"c1\"},{\"text\":\"c2\"}],\"description\":\"` + bulk + `\",\"epicId\":\"epc-1\",\"tags\":[\"t\"],\"title\":\"old ` + itoa(i) + `\"}"}}]}`)
		add(`{"role":"tool","tool_call_id":"call_old_` + itoa(i) + `","content":"{\"ok\":true,\"task\":{\"id\":\"tsk-` + itoa(i) + `\"}}"}`)
		add(`{"role":"assistant","content":"done","tool_calls":[{"id":"call_bash","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls -la /workspace && git status --short && echo done\"}"}}]}`)
		add(`{"role":"tool","tool_call_id":"call_bash","content":"total 0"}`)
	}
	for i := 0; i < nRecent; i++ {
		add(`{"role":"assistant","content":"","tool_calls":[{"id":"call_new_` + itoa(i) + `","type":"function","function":{"name":"llmsafespaces_board_update_task","arguments":"{\"taskId\":\"tsk-` + itoa(i) + `\",\"setCriteriaMet\":{\"index\":0,\"met\":true}}"}}]}`)
		add(`{"role":"tool","tool_call_id":"call_new_` + itoa(i) + `","content":"{\"ok\":true}"}`)
	}
	add(`{"role":"user","content":"continue"}`)
	return []byte(`{"model":"glm-5.3","stream":false,"messages":[` + strings.Join(msgs, ",") + `],"max_tokens":4096}`)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := ""
	for i > 0 {
		digits = string(rune('0'+i%10)) + digits
		i /= 10
	}
	return digits
}

func parseMessages(t *testing.T, body []byte) []map[string]json.RawMessage {
	t.Helper()
	var top struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(body, &top))
	return top.Messages
}

// TestAgedArgCompaction_StubsAgedLLMSafeSpacesArgs — v3: aged tool usage
// is restructured into plain text (params + results as bullets); call
// identity, results, and platform calls all survive as content; the
// tool_calls STRUCTURE is gone from the restructured messages.
func TestAgedArgCompaction_StubsAgedLLMSafeSpacesArgs(t *testing.T) {
	c := newAgedArgCompactorWindow(0) // restructure EVERYTHING (validated setting)
	body := agedArgTestBody(6, 2)
	out, stats := c.Compact(body)

	assert.True(t, stats.Changed)
	assert.Equal(t, 14, stats.CallsStubbed, "all tool calls restructured: 6 create + 6 bash + 2 update")

	msgs := parseMessages(t, out)
	// NO tool_calls anywhere in the restructured body
	for _, m := range msgs {
		_, has := m["tool_calls"]
		assert.False(t, has, "no tool_calls survive restructuring")
		role, hasRole := m["role"]
		if hasRole {
			assert.NotEqual(t, `"tool"`, string(role), "no role:tool messages survive")
		}
	}
	// the prose keeps full information: params + results verbatim
	joined := string(out)
	assert.Contains(t, joined, "Called `llmsafespaces_board_create_task`", "call recorded as a bullet")
	assert.Contains(t, joined, `**epicId**: \"epc-1\"`, "parameters visible (wire-escaped form)")
	assert.Contains(t, joined, `Result: {\"ok\":true,\"task\":{\"id\":\"tsk-0\"}}`, "results verbatim (wire-escaped form)")
	assert.Contains(t, joined, "charter prose for the aged bulk payload", "bulk payload content PRESERVED (lossless)")
	assert.Contains(t, joined, "Called `bash`", "platform calls restructured too (validated config)")
	assert.Contains(t, joined, `**command**: \"ls -la`, "platform params visible")
}

// TestAgedArgCompaction_DeterministicRendering — the v3 determinism
// contract: the same body restructures to byte-identical output on
// every pass (prefix-cache stability depends on it).
func TestAgedArgCompaction_DeterministicRendering(t *testing.T) {
	body := agedArgTestBody(4, 2)
	out1, _ := newAgedArgCompactorWindow(0).Compact(body)
	out2, _ := newAgedArgCompactorWindow(0).Compact(body)
	assert.Equal(t, string(out1), string(out2), "restructure is a pure function: byte-identical across passes")
}

// TestAgedArgCompaction_WindowProtectsRecentVerbatim — window > 0 keeps
// the last N tool calls in verbatim tool-call form (untested-in-prod
// tuning knob; the default is 0 = restructure everything).
func TestAgedArgCompaction_WindowProtectsRecentVerbatim(t *testing.T) {
	body := agedArgTestBody(6, 2)
	c := newAgedArgCompactorWindow(4)
	out, stats := c.Compact(body)
	require.True(t, stats.Changed)
	msgs := parseMessages(t, out)
	// the 4 most recent calls (2 bash+... positional: new_0, bash_tail?, new_1)
	// stay verbatim: their tool_calls survive
	verbatim := 0
	for _, m := range msgs {
		if _, ok := m["tool_calls"]; ok {
			verbatim++
		}
	}
	assert.Equal(t, 4, verbatim, "the window-protected calls (old_5, its bash, new_0, new_1) stay in 4 verbatim messages")

}

// TestAgedArgCompaction_IdempotentAcrossTurns — re-applying the
// transform to an already-restructured body changes nothing (there are
// no tool_calls left to restructure).
func TestAgedArgCompaction_IdempotentAcrossTurns(t *testing.T) {
	body := agedArgTestBody(3, 2)
	once, _ := newAgedArgCompactorWindow(0).Compact(body)
	twice, stats2 := newAgedArgCompactorWindow(0).Compact(once)
	assert.Equal(t, string(once), string(twice), "byte-stable: the restructured form re-renders identically")
	assert.False(t, stats2.Changed, "second pass is a no-op")
}

// TestAgedArgCompaction_PassthroughSafety — bodies without tool calls,
// without messages, or malformed are forwarded unchanged.
func TestAgedArgCompaction_PassthroughSafety(t *testing.T) {
	for name, body := range map[string][]byte{
		"no tool calls":  []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`),
		"no messages":    []byte(`{"model":"m"}`),
		"malformed body": []byte(`{"model":`),
	} {
		out, stats := newAgedArgCompactorWindow(0).Compact(body)
		assert.Equal(t, string(body), string(out), name+": passthrough")
		assert.False(t, stats.Changed, name+": unchanged")
	}
}

// TestAgedArgCompaction_UntouchedFieldsBytePreserved — unknown
// top-level fields, results, ids, and names survive.
func TestAgedArgCompaction_UntouchedFieldsBytePreserved(t *testing.T) {
	body := []byte(`{"future_field":{"nested":[1,2,{"deep":true}]},"max_tokens":10,"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"","tool_calls":[{"id":"call_old_1","type":"function","function":{"name":"llmsafespaces_board_epic_activate","arguments":"{\"acknowledgeInactiveDeps\":true,\"epicId\":\"epc-x\"}"}}]},{"role":"tool","tool_call_id":"call_old_1","content":"{\"ok\":true}"}]}`)
	out, stats := newAgedArgCompactorWindow(0).Compact(body)
	require.True(t, stats.Changed)
	s := string(out)
	assert.Contains(t, s, `"future_field":{"nested":[1,2,{"deep":true}]}`, "unknown top-level fields preserved")
	assert.Contains(t, s, "Called `llmsafespaces_board_epic_activate`", "call bullet present")
	assert.Contains(t, s, "**acknowledgeInactiveDeps**: true", "boolean param visible")
	assert.Contains(t, s, `**epicId**: \"epc-x\"`, "id param visible")
	assert.Contains(t, s, `Result: {\"ok\":true}`, "result visible")
}

// TestAgedArgRenderMatchesReplayHarness — the python mirror
// (scripts/1949-aged-arg-replay.py) renders the same merged markdown
// as the Go transform for a fixed sample (semantic parity on a parsed
// representation — the cure bytes must match across the pin).
func TestAgedArgRenderMatchesReplayHarness(t *testing.T) {
	script := filepath.Join("..", "..", "scripts", "1949-aged-arg-replay.py")
	src, err := os.ReadFile(script)
	require.NoError(t, err, "the replay harness must ship beside this package")
	srcS := string(src)
	assert.Contains(t, srcS, "- Called `", "the harness renders the same bullet form")
	assert.Contains(t, srcS, "  - Result: ", "the harness renders the same result line")
	assert.NotContains(t, srcS, "arguments removed by relay",
		"the failed v1 prose stub must not survive in the harness")
	assert.Contains(t, srcS, "RESTRUCTURE_TOOL_CALLS = True",
		"the harness mirrors the v3 restructure (not stub/dot)")

	// Executed leg: the harness --print-stub prints the restructured
	// form of a FIXED sample body; the Go transform of the identical
	// body must render byte-identically (deterministic rendering parity
	// across the pin).
	var (
		foundInterp bool
		lastPath    string
		lastErr     error
		lastOut     []byte
	)
	// The sample carries < and & — the r2 finding 5 divergence (Go's
	// default Marshal HTML-escapes them; both sides now emit raw bytes).
	// KNOWN, DOCUMENTED divergence NOT covered here: non-integer number
	// FORMATS (Go json.Number preserves "1.50"; python re-serializes
	// "1.5") — semantic parity only for number formatting.
	sampleBody := []byte(`{"model":"glm-5.3","messages":[` +
		`{"role":"user","content":"go < & >"},` +
		`{"role":"assistant","content":"working","tool_calls":[{"id":"call_1","type":"function","function":{"name":"llmsafespaces_board_create_task","arguments":"{\"epicId\":\"epc-1 < & >\",\"title\":\"t\"}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"{\"ok\":true}"}]}`)
	goOut, _ := newAgedArgCompactorWindow(0).Compact(sampleBody)
	// RAW-BYTES leg (r3 finding 3): parsed comparison cannot see HTML
	// escaping — \u003c parses back to < — so assert the literal bytes.
	assert.Contains(t, string(goOut), `epc-1 < & >`,
		"Go wire bytes carry < & > LITERALLY (no \u003c escaping — marshalTop and the merge marshal are escape-free)")
	var goParsed struct {
		Messages []struct{ Content string } `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(goOut, &goParsed))
	require.Len(t, goParsed.Messages, 2)
	goRendered := goParsed.Messages[1].Content

	for _, interp := range []string{"python3", "python"} {
		path, lerr := exec.LookPath(interp)
		if lerr != nil {
			continue
		}
		foundInterp, lastPath = true, path
		captured, xerr := exec.Command(path, script, "--print-stub").CombinedOutput()
		if xerr == nil {
			pyRaw := strings.TrimSpace(string(captured))
			assert.Contains(t, pyRaw, `epc-1 < & >`,
				"python wire bytes carry < & > literally (ensure_ascii=False)")
			var pyParsed struct {
				Messages []struct{ Content string } `json:"messages"`
			}
			require.NoError(t, json.Unmarshal([]byte(pyRaw), &pyParsed))
			require.Len(t, pyParsed.Messages, 2)
			assert.Equal(t, goRendered, pyParsed.Messages[1].Content,
				"python mirror renders byte-identical markdown to the Go transform")
			return
		}
		lastErr, lastOut = xerr, captured
	}
	if foundInterp {
		t.Fatalf("replay harness failed to run under %s: %v; output: %q", lastPath, lastErr, lastOut)
	}
	t.Skip("no python interpreter on PATH — source-pin leg above still enforced")
}

// TestAgedArgCompaction_MalformedEntriesLeftUntouched — a message whose
// tool_calls entries are malformed is forwarded as-is; the transform
// never breaks a request over garbage it cannot parse.
func TestAgedArgCompaction_MalformedEntriesLeftUntouched(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"assistant","tool_calls":["garbage", 42]},{"role":"assistant","tool_calls":[{"id":"x","type":"function","function":{"name":"llmsafespaces_session_list","arguments":"{\"includeArchived\":true,\"sessionId\":\"ses-1\"}"}}]},{"role":"tool","tool_call_id":"x","content":"{\"ok\":true}"}]}`)
	out, stats := newAgedArgCompactorWindow(0).Compact(body)
	assert.True(t, stats.Changed, "the well-formed call (with its paired result) is still restructured")
	assert.Contains(t, string(out), `"garbage"`, "malformed entry preserved verbatim")
	assert.Contains(t, string(out), `**sessionId**: \"ses-1\"`, "params visible in the bullet")
}

// --- r1-review adversarial passthrough pins: the lossless-by-
// construction contract. Each anomaly span forwards VERBATIM — we never
// drop or reshape what we cannot perfectly pair (findings 1,2,3,5,7).

func TestAgedArgCompaction_OrphanResultPassthrough(t *testing.T) {
	// A tool message whose id matches no rendered call → the span is
	// forwarded verbatim (r1 finding 1: silently deleted in the v3 draft).
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"llmsafespaces_session_list","arguments":"{\"includeArchived\":true}"}}]},` +
		`{"role":"tool","tool_call_id":"call_UNKNOWN","content":"{\"ok\":true}"},` +
		`{"role":"user","content":"go"}]}`)
	out, stats := newAgedArgCompactorWindow(0).Compact(body)
	assert.False(t, stats.Changed, "unpairable result → span passthrough")
	assert.Equal(t, string(body), string(out), "byte-verbatim")
}

func TestAgedArgCompaction_DuplicateResultIDPassthrough(t *testing.T) {
	// Two tool messages sharing one tool_call_id → passthrough (r1
	// finding 2: last-wins dropped the first result).
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"llmsafespaces_session_list","arguments":"{\"includeArchived\":true}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"{\"first\":true}"},` +
		`{"role":"tool","tool_call_id":"call_1","content":"{\"second\":true}"},` +
		`{"role":"user","content":"go"}]}`)
	out, stats := newAgedArgCompactorWindow(0).Compact(body)
	assert.False(t, stats.Changed, "duplicate result ids → span passthrough")
	assert.Equal(t, string(body), string(out), "byte-verbatim")
}

func TestAgedArgCompaction_NonStringResultContentPassthrough(t *testing.T) {
	// Array-shaped (parts) result content → passthrough (r1 finding 3:
	// content was dropped while the message was consumed).
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"llmsafespaces_session_list","arguments":"{\"includeArchived\":true}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":[{"type":"text","text":"parts-shaped"}]},` +
		`{"role":"user","content":"go"}]}`)
	out, stats := newAgedArgCompactorWindow(0).Compact(body)
	assert.False(t, stats.Changed, "non-string result content → span passthrough")
	assert.Equal(t, string(body), string(out), "byte-verbatim")
}

func TestAgedArgCompaction_ArrayAssistantContentPassthrough(t *testing.T) {
	// Array-shaped assistant content → passthrough (r1 finding 4: the
	// content was silently dropped while bullets still rendered).
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"assistant","content":[{"type":"text","text":"parts-shaped thinking"}],"tool_calls":[{"id":"call_1","type":"function","function":{"name":"llmsafespaces_session_list","arguments":"{\"includeArchived\":true}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"{\"ok\":true}"},` +
		`{"role":"user","content":"go"}]}`)
	out, stats := newAgedArgCompactorWindow(0).Compact(body)
	assert.False(t, stats.Changed, "array assistant content → span passthrough")
	assert.Equal(t, string(body), string(out), "byte-verbatim")
}

func TestAgedArgCompaction_UnnameableCallPassthrough(t *testing.T) {
	// A call missing its function.name → the whole span passes through
	// (r1 finding 5: the call was skipped while its result was consumed).
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"arguments":"{\"a\":1}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"{\"ok\":true}"},` +
		`{"role":"user","content":"go"}]}`)
	out, stats := newAgedArgCompactorWindow(0).Compact(body)
	assert.False(t, stats.Changed, "unnameable call → span passthrough")
	assert.Equal(t, string(body), string(out), "byte-verbatim")
}

func TestAgedArgCompaction_UnconsumedResultPassthrough(t *testing.T) {
	// r2 finding 1: an ORPHAN riding alongside a COMPLETE pair — the
	// one-directional v3 draft consumed and deleted it. Pairing must be
	// exactly bijective.
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"llmsafespaces_session_list","arguments":"{\"includeArchived\":true}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"{\"ok\":true}"},` +
		`{"role":"tool","tool_call_id":"call_UNKNOWN","content":"{\"orphan\":true}"},` +
		`{"role":"user","content":"go"}]}`)
	out, stats := newAgedArgCompactorWindow(0).Compact(body)
	assert.False(t, stats.Changed, "orphan result alongside a complete pair → span passthrough")
	assert.Equal(t, string(body), string(out), "byte-verbatim")
}

func TestAgedArgCompaction_NullResultContentPassthrough(t *testing.T) {
	// r2 finding 3 / r4 review: null RESULT content — JSON null into
	// *string is a silent no-op in Go; the v3 draft rendered an empty
	// Result line while consuming the message. Passthrough.
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"llmsafespaces_session_list","arguments":"{\"includeArchived\":true}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":null},` +
		`{"role":"user","content":"go"}]}`)
	out, stats := newAgedArgCompactorWindow(0).Compact(body)
	assert.False(t, stats.Changed, "null result content → span passthrough")
	assert.Equal(t, string(body), string(out), "byte-verbatim")
}

func TestAgedArgCompaction_NullToolCallIDPassthrough(t *testing.T) {
	// r2 finding 2: null tool_call_id — null-into-string is a silent
	// no-op in Go; the v3 draft landed it in resultByID[""] (deleted).
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"llmsafespaces_session_list","arguments":"{\"includeArchived\":true}"}}]},` +
		`{"role":"tool","tool_call_id":null,"content":"{\"vanishing\":true}"},` +
		`{"role":"user","content":"go"}]}`)
	out, stats := newAgedArgCompactorWindow(0).Compact(body)
	assert.False(t, stats.Changed, "null tool_call_id → span passthrough")
	assert.Equal(t, string(body), string(out), "byte-verbatim")
}

func TestAgedArgCompaction_UnknownAssistantFieldPassthrough(t *testing.T) {
	// r2 finding 3: a real glm wire field (reasoning_content) must not be
	// silently dropped by the {role, content} merge marshal.
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"assistant","content":"working","reasoning_content":"hidden chain","tool_calls":[{"id":"call_1","type":"function","function":{"name":"llmsafespaces_session_list","arguments":"{\"includeArchived\":true}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"{\"ok\":true}"},` +
		`{"role":"user","content":"go"}]}`)
	out, stats := newAgedArgCompactorWindow(0).Compact(body)
	assert.False(t, stats.Changed, "unknown assistant fields → span passthrough (lossless)")
	assert.Equal(t, string(body), string(out), "byte-verbatim")
}

func TestAgedArgCompaction_RoleFabricationPassthrough(t *testing.T) {
	// r3 finding 1: a span whose role is absent or not "assistant" is
	// never reshaped into an assistant turn (the merge hardcodes
	// role:assistant — fabrication).
	for _, tc := range []struct{ name, roleJSON string }{
		{"absent role", ""},
		{"user role", `"role":"user",`},
		{"null role", `"role":null,`},
	} {
		_ = tc
		var roleJSON string
		if tc.roleJSON != "" {
			roleJSON = tc.roleJSON
		}
		body := []byte(`{"model":"m","messages":[` +
			`{"role":"user","content":"hi"},` +
			`{` + roleJSON + `"content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"llmsafespaces_session_list","arguments":"{\"includeArchived\":true}"}}]},` +
			`{"role":"tool","tool_call_id":"call_1","content":"{\"ok\":true}"},` +
			`{"role":"user","content":"go"}]}`)
		out, stats := newAgedArgCompactorWindow(0).Compact(body)
		assert.False(t, stats.Changed, tc.name+" → span passthrough (no role fabrication)")
		assert.Equal(t, string(body), string(out), "byte-verbatim")
	}
}

func TestAgedArgCompaction_DuplicateCallIDPassthrough(t *testing.T) {
	// r2 finding 7: duplicate CALL ids — one result would render on both
	// bullets (inconsistent with the any-anomaly-passthrough contract).
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"assistant","content":"","tool_calls":[` +
		`{"id":"call_1","type":"function","function":{"name":"llmsafespaces_session_list","arguments":"{\"a\":1}"}},` +
		`{"id":"call_1","type":"function","function":{"name":"llmsafespaces_session_list","arguments":"{\"a\":2}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"{\"ok\":true}"},` +
		`{"role":"user","content":"go"}]}`)
	out, stats := newAgedArgCompactorWindow(0).Compact(body)
	assert.False(t, stats.Changed, "duplicate call ids → span passthrough")
	assert.Equal(t, string(body), string(out), "byte-verbatim")
}

func TestAgedArgRenderArguments_TrailingBracketVerbatim(t *testing.T) {
	// r2 finding 4: More() is FALSE for a trailing `]` and Token()
	// ERRORS on it — the v3 draft fell through to a partial bullet.
	out := renderArguments(`{"a":1} ]`)
	assert.Contains(t, out, `"a":1} ]`, "the trailing ] rides verbatim (raw render, total function)")
}

func TestAgedArgRenderArguments_TrailingTokenVerbatim(t *testing.T) {
	// a trailing VALID token must also render raw (not silently dropped)
	out := renderArguments(`{"a":1} 42`)
	assert.Contains(t, out, `"a":1} 42`, "trailing valid token rides verbatim")
}

// TestAgedArgRenderArguments_MalformedTrailingContentVerbatim — a
// payload that parses as an object but carries trailing garbage renders
// the RAW inner text verbatim (total function; r1 finding 6: the draft
// rendered a partial bullet).
func TestAgedArgRenderArguments_MalformedTrailingContentVerbatim(t *testing.T) {
	out := renderArguments(`{"epicId":"epc-1"} GARBAGE`)
	assert.Contains(t, out, `"epicId":"epc-1"} GARBAGE`, "the raw payload rides verbatim inside the bullet (total function)")
}

func TestAgedArgRenderArguments_DuplicateKeysCollapseLastWins(t *testing.T) {
	// r1 finding 8: Go and python must agree on duplicate keys — last
	// value wins, first position kept, ONE bullet (the per-occurrence
	// rendering re-created the duplicate-key attractor).
	out := renderArguments(`{"epicId":"epc-first","epicId":"epc-second","force":true}`)
	assert.Equal(t, `**epicId**: "epc-second", **force**: true`, out)
}

// TestAgedArgCompactorDefaults — the default window matches the
// documented constant (0 = restructure every tool call; <0 disables).
func TestAgedArgCompactorDefaults(t *testing.T) {
	c := newAgedArgCompactor()
	assert.Equal(t, 0, c.RecentToolCalls,
		"default restructures every tool call — the validated configuration")
	var nilC *agedArgCompactor
	out, stats := nilC.Compact([]byte(`{"x":1}`))
	assert.Equal(t, []byte(`{"x":1}`), out, "nil compactor is a passthrough")
	assert.False(t, stats.Changed)
}
