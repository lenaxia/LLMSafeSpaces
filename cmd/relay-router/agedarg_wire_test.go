// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgedArgCompactionFiresOnTheWirePath (#1949): a poisoned
// chat-completion body posted through the BYO resolve path reaches the
// upstream COMPACTED — aged llmsafespaces_* args stubbed, recent window
// and tool results intact — and the fire is loudly observable: the
// metrics counters move. The workspace's own store is never touched by
// the router (it never persists bodies), so observability lives in
// metrics + the metadata-only log line.
func TestAgedArgCompactionFiresOnTheWirePath(t *testing.T) {
	rig := newByoTestRig(t)
	rig.svc.agedArgs = newAgedArgCompactorWindow(1)
	token := rig.mint(t, nil)

	body := `{"model":"glm-4.7","messages":[` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"call_old_1","type":"function","function":{"name":"llmsafespaces_board_create_task","arguments":` + jsonString(t, `{"description":"`+strings.Repeat("poisoned bulk charter ", 40)+`","epicId":"epc-1","title":"t"}`) + `}}]},` +
		`{"role":"tool","tool_call_id":"call_old_1","content":"{\"ok\":true}"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"call_recent","type":"function","function":{"name":"llmsafespaces_board_read","arguments":` + jsonString(t, `{"taskId":"tsk-1"}`) + `}}]},` +
		`{"role":"tool","tool_call_id":"call_recent","content":"{\"task\":{\"id\":\"tsk-1\"}}"},` +
		`{"role":"user","content":"go"}]}`

	resp, err := rig.do(http.MethodPost, "/w/ws-1/zai/v1/chat/completions", token, body, nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	up := rig.upstream.last()
	var got struct {
		Messages []struct {
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal([]byte(up.body), &got))
	require.Len(t, got.Messages, 5)

	assert.Equal(t, string(agedArgStub()), got.Messages[0].ToolCalls[0].Function.Arguments,
		"the aged poisoned args arrive upstream stubbed")
	assert.Equal(t, "llmsafespaces_board_create_task", got.Messages[0].ToolCalls[0].Function.Name,
		"tool name survives")
	assert.Equal(t, "call_old_1", got.Messages[0].ToolCalls[0].ID, "call identity survives")
	assert.Contains(t, got.Messages[1].Content, "ok", "outcome semantics (results) survive")
	assert.Contains(t, got.Messages[2].ToolCalls[0].Function.Arguments, "tsk-1",
		"the recent-window call is untouched end-to-end")

	// Observability: the fire is counted on every metric surface.
	assert.Equal(t, 1.0, testutil.ToFloat64(rig.svc.metrics.agedArgRequests.WithLabelValues("ws-1", "zai")))
	assert.Equal(t, 1.0, testutil.ToFloat64(rig.svc.metrics.agedArgStubbed.WithLabelValues("ws-1", "zai")))
	assert.Greater(t, testutil.ToFloat64(rig.svc.metrics.agedArgBytesSaved.WithLabelValues("ws-1", "zai")), 100.0)
}

// TestAgedArgCompactionCleanBodyUntouchedOnTheWire — a body with no aged
// llmsafespaces_* calls forwards byte-identical (no compaction telemetry).
func TestAgedArgCompactionCleanBodyUntouchedOnTheWire(t *testing.T) {
	rig := newByoTestRig(t)
	rig.svc.agedArgs = newAgedArgCompactorWindow(2)
	token := rig.mint(t, nil)

	body := `{"model":"glm-4.7","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}}]}]}`
	resp, err := rig.do(http.MethodPost, "/w/ws-1/zai/v1/chat/completions", token, body, nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	assert.Equal(t, body, rig.upstream.last().body, "clean body forwarded byte-identical")
	assert.Equal(t, 0.0, testutil.ToFloat64(rig.svc.metrics.agedArgRequests.WithLabelValues("ws-1", "zai")))
}

// TestAgedArgTinyAgedArgsNoPanic (#1949 r3 regression): an aged
// llmsafespaces_* call whose arguments are SMALLER than the stub grows
// the rewritten body — the naive bytes_saved delta goes negative and
// Prometheus Counter.Add panics (reproduced on the r2 head's rig),
// killing the client's request with EOF. The request must complete,
// the stub must land upstream, and every counter must stay >= 0.
func TestAgedArgTinyAgedArgsNoPanic(t *testing.T) {
	rig := newByoTestRig(t)
	rig.svc.agedArgs = newAgedArgCompactorWindow(0)
	token := rig.mint(t, nil)

	// 17-byte args — far below the ~110-byte stub.
	body := `{"model":"glm-4.7","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"c_tiny","type":"function","function":{"name":"llmsafespaces_board_read","arguments":` + jsonString(t, `{"taskId":"t1"}`) + `}}]},{"role":"user","content":"go"}]}`
	resp, err := rig.do(http.MethodPost, "/w/ws-1/zai/v1/chat/completions", token, body, nil)
	require.NoError(t, err, "the request must complete — observability never kills the request")
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	up := rig.upstream.last()
	var got struct {
		Messages []struct {
			ToolCalls []struct {
				Function struct {
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal([]byte(up.body), &got))
	require.NotEmpty(t, got.Messages[0].ToolCalls)
	assert.Equal(t, string(agedArgStub()), got.Messages[0].ToolCalls[0].Function.Arguments,
		"the tiny aged args still land stubbed upstream")
	assert.NotContains(t, up.body, `"taskId":"t1"`, "the original tiny args are gone")

	assert.Equal(t, 1.0, testutil.ToFloat64(rig.svc.metrics.agedArgRequests.WithLabelValues("ws-1", "zai")))
	assert.Equal(t, 1.0, testutil.ToFloat64(rig.svc.metrics.agedArgStubbed.WithLabelValues("ws-1", "zai")))
	assert.GreaterOrEqual(t, testutil.ToFloat64(rig.svc.metrics.agedArgBytesSaved.WithLabelValues("ws-1", "zai")), 0.0,
		"bytes_saved clamped at zero, never negative")
}

// TestAgedArgWindowSplitsWithinOneMessage — the window boundary falls
// BETWEEN two tool calls of the SAME assistant message: per-call
// treatment inside one message (the (msg, idx) positional set).
func TestAgedArgWindowSplitsWithinOneMessage(t *testing.T) {
	msg := `{"role":"assistant","content":"","tool_calls":[` +
		`{"id":"a","type":"function","function":{"name":"llmsafespaces_session_list","arguments":` + jsonString(t, `{"allSessions":true}`) + `}},` +
		`{"id":"b","type":"function","function":{"name":"llmsafespaces_board_read","arguments":` + jsonString(t, `{"taskId":"t9"}`) + `}},` +
		`{"id":"c","type":"function","function":{"name":"bash","arguments":` + jsonString(t, `{"command":"ls"}`) + `}}]}`
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"},` + msg + `]}`)

	// Window 2 protects the LAST two calls (b, c): a is stubbed.
	out, stats := newAgedArgCompactorWindow(2).Compact(body)
	require.True(t, stats.Changed)
	require.Equal(t, 1, stats.CallsStubbed)
	var parsed struct {
		Messages []struct {
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(out, &parsed))
	calls := parsed.Messages[1].ToolCalls
	require.Len(t, calls, 3)
	assert.Equal(t, string(agedArgStub()), calls[0].Function.Arguments, "a (beyond window) stubbed")
	assert.Contains(t, calls[1].Function.Arguments, "t9", "b (in window) intact")
	assert.Contains(t, calls[2].Function.Arguments, "ls", "c (in window, platform tool) intact")
}

// TestAgedArgCompactionKillSwitchOnTheWire — a negative window disables
// the compaction on the wire path: the poisoned body forwards verbatim.
func TestAgedArgCompactionKillSwitchOnTheWire(t *testing.T) {
	rig := newByoTestRig(t)
	rig.svc.agedArgs = newAgedArgCompactorWindow(-1)
	token := rig.mint(t, nil)

	body := `{"model":"glm-4.7","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"llmsafespaces_session_list","arguments":"{\"includeArchived\":true}"}}]},{"role":"user","content":"go"}]}`
	resp, err := rig.do(http.MethodPost, "/w/ws-1/zai/v1/chat/completions", token, body, nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	assert.Contains(t, rig.upstream.last().body, "includeArchived", "disabled compaction forwards verbatim")
}

// TestAgedArgSyntheticReplayCure — the checked-in synthetic replay arm
// (Go twin of scripts/1949-aged-arg-replay.py): a deterministic poisoned
// transcript — the recorded production runaway shapes (duplicate-key
// boolean tiling and old-shape bulk-first charters) — goes through the
// compaction, and the OUTPUT passes the harness scorer's cleanliness
// gates: strictly-parseable arguments everywhere, no key repeated >=10x,
// no >=10x fragment tiling, aged bulk gone, recent window + results
// intact. This is the poisoned→compacted→clean discriminator, pinned
// where CI runs it every push.
func TestAgedArgSyntheticReplayCure(t *testing.T) {
	// The recorded 2026-10-25 production runaway, verbatim class:
	// {"epicId":"…","acknowledgeInactiveDeps":true,…×157} — 29.6KB of
	// duplicate-key tiling.
	runawayArgs := `{"epicId":"epc-wooden-carousel-reprints"` + strings.Repeat(`,"acknowledgeInactiveDeps":true`, 157) + `}`
	oldShape := `{"acceptanceCriteria":[{"text":"c1"},{"text":"c2"},{"text":"c3"},{"text":"c4"}],"description":"` +
		strings.Repeat("charter bulk ", 300) + `","dependsOn":[],"epicId":"epc-1","tags":["a"],"title":"poisoned"}`

	var msgs []string
	add := func(s string) { msgs = append(msgs, s) }
	add(`{"role":"user","content":"orchestrate"}`)
	// The poisoned history: one recorded runaway + three old-shape bulk
	// charters (+ their results), interleaved with platform bash calls —
	// the orchestrator-session shape.
	poisons := []struct{ id, name, args string }{
		{"call_run_1", "llmsafespaces_board_epic_activate", runawayArgs},
		{"call_old_1", "llmsafespaces_board_create_task", oldShape},
		{"call_old_2", "llmsafespaces_board_create_task", oldShape},
		{"call_old_3", "llmsafespaces_board_create_task", oldShape},
	}
	for i, p := range poisons {
		args, err := json.Marshal(p.args)
		require.NoError(t, err)
		add(`{"role":"assistant","content":"","tool_calls":[{"id":"` + p.id + `","type":"function","function":{"name":"` + p.name + `","arguments":` + string(args) + `}}]}`)
		add(`{"role":"tool","tool_call_id":"` + p.id + `","content":"{\"ok\":true,\"n\":` + itoa(i) + `}"}`)
		bashArgs, _ := json.Marshal(`{"command":"git status --short"}`)
		add(`{"role":"assistant","content":"","tool_calls":[{"id":"call_bash_` + itoa(i) + `","type":"function","function":{"name":"bash","arguments":` + string(bashArgs) + `}}]}`)
		add(`{"role":"tool","tool_call_id":"call_bash_` + itoa(i) + `","content":"clean"}`)
	}
	fresh, _ := json.Marshal(`{"taskId":"tsk-9","setCriteriaMet":{"index":0,"met":true}}`)
	add(`{"role":"assistant","content":"","tool_calls":[{"id":"call_recent","type":"function","function":{"name":"llmsafespaces_board_update_task","arguments":` + string(fresh) + `}}]}`)
	add(`{"role":"tool","tool_call_id":"call_recent","content":"{\"ok\":true}"}`)
	add(`{"role":"user","content":"continue the board work"}`)
	body := []byte(`{"model":"glm-5.3","messages":[` + strings.Join(msgs, ",") + `]}`)

	// The poisoned arm's OWN hygiene (scorer negative-control): the raw
	// poisoned args carry the runaway signatures the scorer detects.
	assert.Greater(t, strings.Count(runawayArgs, `"acknowledgeInactiveDeps":`), 10, "fixture carries the duplicate-key tiling")

	// The cure: compact (recent window 2 = the last llmsafespaces_ call
	// + its bash neighbor stay live).
	out, stats := newAgedArgCompactorWindow(2).Compact(body)
	require.True(t, stats.Changed)
	require.Equal(t, 4, stats.CallsStubbed, "runaway + 3 bulk charters stubbed")

	// Scorer cleanliness gates on the compacted body.
	var parsed struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(out, &parsed))
	s := string(out)
	stubs, recent, results := 0, 0, 0
	for _, m := range parsed.Messages {
		for _, c := range m.ToolCalls {
			// strict-parse every arguments payload (duplicate keys would
			// still parse in Go — count key repetitions textually, the
			// harness scorer's >=10x signature).
			for _, key := range []string{"acknowledgeInactiveDeps", "description", "epicId", "taskId"} {
				count := strings.Count(c.Function.Arguments, `"`+key+`":`)
				require.LessOrEqual(t, count, 1, "no key repetition >=2 in any emitted args (scorer: >=10x is runaway; the compacted body must carry NONE)")
			}
			require.True(t, json.Valid([]byte(c.Function.Arguments)), "arguments strictly parseable")
			if c.Function.Arguments == string(agedArgStub()) {
				stubs++
				assert.NotEmpty(t, c.ID, "stubbed calls keep identity")
				assert.True(t, strings.HasPrefix(c.Function.Name, "llmsafespaces_"), "stubbed calls keep the tool name")
			} else if strings.HasPrefix(c.Function.Name, "llmsafespaces_") {
				assert.Equal(t, "call_recent", c.ID, "the only intact llmsafespaces_* args is the recent-window call")
				recent++
			} else {
				assert.Equal(t, "bash", c.Function.Name)
			}
		}
		if m.Role == "tool" {
			results++
			assert.NotContains(t, m.Content, "aged-arg compaction", "results never rewritten")
		}
		// No >=10x fragment tiling anywhere the model would re-see.
		for _, frag := range []string{`"acknowledgeInactiveDeps":true,`, "charter bulk charter bulk"} {
			require.Less(t, strings.Count(s, frag), 10, "no tiling signature survives compaction")
		}
	}
	assert.Equal(t, 4, stubs)
	assert.Equal(t, 1, recent)
	assert.Equal(t, 9, results, "4 poison results + 4 bash results + 1 recent result intact")
	assert.NotContains(t, s, strings.Repeat("charter bulk ", 50), "the bulk-first charter payload is gone")
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

// jsonString serializes s as a JSON string literal (for embedding raw
// JSON fragments as arguments values inside hand-built bodies).
func jsonString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	require.NoError(t, err)
	return string(b)
}

// TestAgedArgRunConfigEnvKnob — the env knob threads through the run
// config (threshold-teachable per #1949; default 30, 0 = stub all,
// negative = kill switch).
func TestAgedArgRunConfigEnvKnob(t *testing.T) {
	t.Setenv("BYO_AGED_ARG_RECENT_TOOL_CALLS", "7")
	cfg := loadByoRunConfig()
	assert.Equal(t, 7, cfg.agedArgRecentToolCalls)
	// Default (unset env): the documented default window.
	t.Setenv("BYO_AGED_ARG_RECENT_TOOL_CALLS", "")
	got := getEnvInt("BYO_AGED_ARG_RECENT_TOOL_CALLS", agedArgDefaultWins)
	assert.Equal(t, agedArgDefaultWins, got)
}
