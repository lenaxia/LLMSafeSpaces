// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// agedArgTestBody builds a chat-completion body with nAged old-shape
// llmsafespaces_* create calls (bulk-first arguments — acceptanceCriteria
// and a multi-KB description leading, discrete scalars last, the pre-#1929
// emission order), interleaved with platform bash calls and tool results,
// plus nRecent fresh llmsafespaces_* calls at the tail.
func agedArgTestBody(nAged, nRecent int) []byte {
	var msgs []string
	add := func(s string) { msgs = append(msgs, s) }
	bulk := strings.Repeat("charter prose for the aged bulk payload. ", 60)
	add(`{"role":"user","content":"begin"}`)
	for i := 0; i < nAged; i++ {
		add(fmt.Sprintf(`{"role":"assistant","content":"","tool_calls":[{"id":"call_old_%d","type":"function","function":{"name":"llmsafespaces_board_create_task","arguments":"{\"acceptanceCriteria\":[{\"text\":\"c1\"},{\"text\":\"c2\"}],\"description\":\"%s\",\"epicId\":\"epc-1\",\"tags\":[\"t\"],\"title\":\"old %d\"}"}}]}`, i, bulk, i))
		add(fmt.Sprintf(`{"role":"tool","tool_call_id":"call_old_%d","content":"{\"ok\":true,\"task\":{\"id\":\"tsk-%d\"}}"}`, i, i))
		add(`{"role":"assistant","content":"done","tool_calls":[{"id":"call_bash","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls -la /workspace && git status --short && echo done\"}"}}]}`)
		add(`{"role":"tool","tool_call_id":"call_bash","content":"total 0"}`)
	}
	for i := 0; i < nRecent; i++ {
		add(fmt.Sprintf(`{"role":"assistant","content":"","tool_calls":[{"id":"call_new_%d","type":"function","function":{"name":"llmsafespaces_board_update_task","arguments":"{\"taskId\":\"tsk-%d\",\"setCriteriaMet\":{\"index\":0,\"met\":true}}"}}]}`, i, i))
		add(fmt.Sprintf(`{"role":"tool","tool_call_id":"call_new_%d","content":"{\"ok\":true}"}`, i))
	}
	add(`{"role":"user","content":"continue"}`)
	return []byte(`{"model":"glm-5.3","stream":false,"messages":[` + strings.Join(msgs, ",") + `],"max_tokens":4096}`)
}

func parseMessages(t *testing.T, body []byte) []map[string]json.RawMessage {
	t.Helper()
	var top struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(body, &top))
	return top.Messages
}

func toolCallsOfMap(t *testing.T, m map[string]json.RawMessage) []map[string]any {
	t.Helper()
	raw, ok := m["tool_calls"]
	if !ok {
		return nil
	}
	var calls []map[string]any
	require.NoError(t, json.Unmarshal(raw, &calls))
	return calls
}

// TestAgedArgCompaction_StubsAgedLLMSafeSpacesArgs — the core cure: aged
// llmsafespaces_* arguments become the stub; call identity and tool name
// survive; tool RESULTS and bash calls are untouched; recent
// llmsafespaces_* calls are untouched.
func TestAgedArgCompaction_StubsAgedLLMSafeSpacesArgs(t *testing.T) {
	// Body tool-call sequence (aged block ×6 then fresh ×2):
	//   old_0, bash, old_1, bash, … old_5, bash, new_0, new_1
	// Window 4 protects the last 4 calls: old_5, its bash, new_0, new_1.
	c := newAgedArgCompactorWindow(4)
	body := agedArgTestBody(6, 2)
	out, stats := c.Compact(body)

	assert.True(t, stats.Changed)
	assert.Equal(t, 5, stats.CallsStubbed, "aged llmsafespaces_* calls beyond the window stubbed")
	assert.Less(t, stats.BytesAfter, stats.BytesBefore, "bulk payloads removed")

	msgs := parseMessages(t, out)
	var stubbedArgs, intactAged, intactRecent, bashIntact, boardResults int
	for _, m := range msgs {
		calls := toolCallsOfMap(t, m)
		for _, call := range calls {
			fn := call["function"].(map[string]any)
			name := fn["name"].(string)
			args := fn["arguments"].(string)
			id := call["id"].(string)
			switch {
			case id == "call_old_5":
				assert.Contains(t, args, "old 5", "the window-protected aged call keeps its args")
				intactAged++
			case strings.HasPrefix(id, "call_old_"):
				assert.Equal(t, "llmsafespaces_board_create_task", name, "tool name preserved")
				assert.Equal(t, string(agedArgStub()), args, "aged args replaced by the stub")
				stubbedArgs++
			case strings.HasPrefix(id, "call_new_"):
				assert.Contains(t, args, "tsk-", "recent llmsafespaces_* args untouched")
				intactRecent++
			case id == "call_bash":
				assert.Contains(t, args, "ls -la", "platform tool args untouched")
				bashIntact++
			}
		}
		if string(m["role"]) == `"tool"` {
			var content string
			require.NoError(t, json.Unmarshal(m["content"], &content))
			assert.NotContains(t, content, "aged-arg compaction", "results never rewritten")
			if strings.HasPrefix(content, "{\"ok\"") {
				boardResults++
			}
		}
	}
	assert.Equal(t, 5, stubbedArgs)
	assert.Equal(t, 1, intactAged)
	assert.Equal(t, 2, intactRecent)
	assert.Equal(t, 6, bashIntact)
	assert.Equal(t, 8, boardResults, "6 aged + 2 recent board results intact")
}

// TestAgedArgCompaction_RecentWindowCountsAllTools — the window is the
// last N tool calls of ANY tool (positional, flattened): bash calls in
// the tail push older llmsafespaces_* calls out of the window, and mixed
// messages get per-call treatment.
func TestAgedArgCompaction_RecentWindowCountsAllTools(t *testing.T) {
	// 6 aged llmsafespaces calls each followed by a bash call, 2 fresh
	// llmsafespaces calls: total tool calls = 6 + 6 + 2 = 14. A window
	// of 14 protects everything (0 stubs); a window of 4 protects the
	// last 4 (old_5, bash, new_0, new_1) → the other 5 aged stubbed.
	body := agedArgTestBody(6, 2)

	all, stats := newAgedArgCompactorWindow(14).Compact(body)
	assert.False(t, stats.Changed, "window covers every tool call → untouched")
	assert.Equal(t, len(body), len(all))

	_, stats4 := newAgedArgCompactorWindow(4).Compact(body)
	assert.Equal(t, 5, stats4.CallsStubbed)
}

// TestAgedArgCompaction_IdempotentAcrossTurns — the client re-sends its
// stored history every turn; re-compacting the compacted body (with aged
// stubs now part of the client's copy) must be a byte-identical no-op.
func TestAgedArgCompaction_IdempotentAcrossTurns(t *testing.T) {
	c := newAgedArgCompactorWindow(4)
	one, stats1 := c.Compact(agedArgTestBody(6, 2))
	require.True(t, stats1.Changed)
	require.Equal(t, 5, stats1.CallsStubbed)
	two, stats2 := c.Compact(one)
	assert.Equal(t, one, two, "second pass byte-identical")
	assert.Equal(t, 0, stats2.CallsStubbed)
	assert.False(t, stats2.Changed)
}

// TestAgedArgCompaction_PassthroughSafety — nothing to do must mean
// byte-identical output: no prefixed tool names, no messages array,
// non-object bodies, and missing/disabled window guards.
func TestAgedArgCompaction_PassthroughSafety(t *testing.T) {
	c := newAgedArgCompactor()
	cases := map[string][]byte{
		"no llmsafespaces names": []byte(`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}}]}]}`),
		"no messages":            []byte(`{"model":"m","stream":true}`),
		"not an object":          []byte(`[1,2,3]`),
		"garbage":                []byte(`not json at all`),
		"empty":                  nil,
	}
	for name, body := range cases {
		out, stats := c.Compact(body)
		assert.Equal(t, body, out, name)
		assert.False(t, stats.Changed, name)
	}

	// Kill switch: negative window disables even a poisoned body.
	disabled := &agedArgCompactor{ToolPrefix: agedArgToolPrefix, RecentToolCalls: -1}
	poisoned := agedArgTestBody(6, 2)
	out, stats := disabled.Compact(poisoned)
	assert.Equal(t, poisoned, out)
	assert.False(t, stats.Changed)
}

// TestAgedArgCompaction_UntouchedFieldsBytePreserved — messages without
// aged llmsafespaces_* calls keep their exact bytes inside the rebuilt
// body (RawMessage fidelity), and the top level keeps unknown fields.
func TestAgedArgCompaction_UntouchedFieldsBytePreserved(t *testing.T) {
	body := []byte(`{"model":"m","future_field":{"nested":[1,2,{"deep":true}]},"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"","tool_calls":[{"id":"call_old_1","type":"function","function":{"name":"llmsafespaces_board_epic_activate","arguments":"{\"acknowledgeInactiveDeps\":true,\"epicId\":\"epc-1\"}"}}]},{"role":"tool","tool_call_id":"call_old_1","content":"{\"ok\":true}"}],"max_tokens":10}`)
	out, stats := newAgedArgCompactorWindow(0).Compact(body)
	require.True(t, stats.Changed)
	s := string(out)
	assert.Contains(t, s, `"role":"user","content":"hi"`, "untouched message bytes preserved")
	assert.Contains(t, s, `"future_field":{"nested":[1,2,{"deep":true}]}`, "unknown top-level fields preserved")
	assert.Contains(t, s, `"content":"{\"ok\":true}"`, "tool result bytes preserved (wire-escaped form)")
	assert.Contains(t, s, `"id":"call_old_1"`, "call id preserved")
	assert.Contains(t, s, `"name":"llmsafespaces_board_epic_activate"`, "tool name preserved")
	assert.NotContains(t, s, "acknowledgeInactiveDeps", "poisoned boolean skeleton gone")
}

// TestAgedArgStubMatchesReplayHarness — the stub is byte-identical to
// the checked-in python mirror (scripts/1949-aged-arg-replay.py
// --print-stub): the live replay arm and the Go in-path transform emit
// the SAME cure bytes. The pin EXECUTES the script (not a hardcoded
// copy), so drift in either direction fails here.
func TestAgedArgStubMatchesReplayHarness(t *testing.T) {
	expected := `{"note":"arguments removed by relay aged-arg compaction (full text preserved in the session audit trail)"}`
	assert.Equal(t, expected, string(agedArgStub()), "Go stub literal")
	wireExpected, err := json.Marshal(expected)
	require.NoError(t, err)
	assert.Equal(t, string(wireExpected), agedArgStubJSONString(), "wire form: the stub object JSON-encoded as a string")

	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available — script-parity leg skipped (literal legs above still pinned)")
	}
	script := filepath.Join("..", "..", "scripts", "1949-aged-arg-replay.py")
	if _, err := os.Stat(script); err != nil {
		t.Skipf("replay harness not found at %s", script)
	}
	out, err := exec.Command(python, script, "--print-stub").Output()
	require.NoError(t, err, "running the harness --print-stub")
	assert.Equal(t, expected, strings.TrimSpace(string(out)),
		"the python mirror's stub payload is byte-identical to the Go stub")
}

// TestAgedArgCompaction_MalformedEntriesLeftUntouched — a message whose
// tool_calls entries are malformed (not objects) is forwarded as-is;
// compaction never breaks a request over garbage it cannot parse.
func TestAgedArgCompaction_MalformedEntriesLeftUntouched(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"assistant","tool_calls":["garbage", 42]},{"role":"assistant","tool_calls":[{"id":"x","type":"function","function":{"name":"llmsafespaces_session_list","arguments":"{}"}}]}]}`)
	out, stats := newAgedArgCompactorWindow(0).Compact(body)
	assert.True(t, stats.Changed, "the well-formed aged call is still stubbed")
	assert.Contains(t, string(out), `"garbage"`, "malformed entry preserved verbatim")
}

// TestAgedArgCompactCompactorDefaults — the default window matches the
// documented constant (threshold-teachable via env; default 30).
func TestAgedArgCompactorDefaults(t *testing.T) {
	c := newAgedArgCompactor()
	assert.Equal(t, agedArgToolPrefix, c.ToolPrefix)
	assert.Equal(t, 30, c.RecentToolCalls)
	var nilC *agedArgCompactor
	out, stats := nilC.Compact([]byte(`{"x":1}`))
	assert.Equal(t, []byte(`{"x":1}`), out, "nil compactor is a passthrough")
	assert.False(t, stats.Changed)
}
