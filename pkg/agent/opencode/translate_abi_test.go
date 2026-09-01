// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"encoding/json"
	"testing"
	"time"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fixedClock() time.Time { return time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC) }

func tx() ABITranslator { return ABITranslator{Now: fixedClock} }

func parse(t *testing.T, raw string) (*abiv1.Event, bool) {
	t.Helper()
	evt, ok, err := tx().Parse([]byte(raw))
	require.NoError(t, err)
	return evt, ok
}

// TestTranslateABI_UnknownEventCustomValve: unknown event types pass
// through the contract's Custom valve with the original payload preserved —
// nothing silently dropped (issue #1137: the 14k-unknown lesson relocated).
func TestTranslateABI_UnknownEventCustomValve(t *testing.T) {
	raw := `{"id":"evt42","type":"session.next.tool.frobnicated","properties":{"sessionID":"ses1","weird":[1,2,{"a":"b"}]}}`
	evt, ok := parse(t, raw)
	require.True(t, ok, "unknown type must NOT be dropped")
	assert.Equal(t, abiv1.EventType_EVENT_TYPE_PART_START, evt.Type)
	require.NotNil(t, evt.Part)
	require.Equal(t, abiv1.PartType_PART_TYPE_CUSTOM, evt.Part.Type)
	custom := evt.Part.GetCustom()
	require.NotNil(t, custom)
	assert.Equal(t, "opencode.event.session.next.tool.frobnicated", custom.Kind)
	assert.Equal(t, "ses1", evt.SessionId)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(custom.Data, &payload))
	assert.Equal(t, "ses1", payload["sessionID"], "original payload preserved verbatim")
}

// TestTranslateABI_IDPreservation: store message/part/session IDs in ==
// contract IDs out (I12 stitch rule; issue #1137: id_preservation_stitch).
func TestTranslateABI_IDPreservation(t *testing.T) {
	evt, ok := parse(t, `{"id":"evt1","type":"message.part.updated","properties":{"sessionID":"sesS","part":{"id":"prtP","messageID":"msgM","sessionID":"sesS2","type":"text","text":"hi"}}}`)
	require.True(t, ok)
	assert.Equal(t, "sesS", evt.SessionId)
	require.NotNil(t, evt.Part)
	assert.Equal(t, "prtP", evt.Part.Id, "part ID preserved")
	assert.Equal(t, "msgM", evt.MessageId, "message ID preserved")

	evt2, ok := parse(t, `{"id":"evt2","type":"message.created","properties":{"sessionID":"sesS","info":{"id":"msgM2","role":"assistant","sessionID":"sesS","time":{"created":1787874778675}}}}`)
	require.True(t, ok)
	require.NotNil(t, evt2.Message)
	assert.Equal(t, "msgM2", evt2.Message.Id, "store message ID preserved through info.id")
	assert.Equal(t, "sesS", evt2.Message.SessionId)
}

// TestTranslateABI_BusyStepBoundaries: busy derives from step boundaries;
// a failed step clears busy (issue #1137: busy_step_boundaries).
func TestTranslateABI_BusyStepBoundaries(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantType abiv1.EventType
		busy     bool
	}{
		{"prompted", `{"id":"e1","type":"session.next.prompted","properties":{"sessionID":"s1","messageID":"m1"}}`, abiv1.EventType_EVENT_TYPE_SESSION_STATUS, true},
		{"step_started", `{"id":"e2","type":"session.next.step.started","properties":{"sessionID":"s1","assistantMessageID":"m2"}}`, abiv1.EventType_EVENT_TYPE_MESSAGE_START, true},
		{"status_busy", `{"id":"e3","type":"session.status","properties":{"sessionID":"s1","status":{"type":"busy"}}}`, abiv1.EventType_EVENT_TYPE_SESSION_STATUS, true},
		{"status_idle", `{"id":"e4","type":"session.status","properties":{"sessionID":"s1","status":{"type":"idle"}}}`, abiv1.EventType_EVENT_TYPE_SESSION_STATUS, false},
		{"session_idle", `{"id":"e5","type":"session.idle","properties":{"sessionID":"s1"}}`, abiv1.EventType_EVENT_TYPE_SESSION_STATUS, false},
		{"step_failed_clears_busy", `{"id":"e6","type":"session.next.step.failed","properties":{"sessionID":"s1","assistantMessageID":"m2","error":{"type":"unknown","message":"boom"}}}`, abiv1.EventType_EVENT_TYPE_ERROR, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evt, ok := parse(t, tc.raw)
			require.True(t, ok)
			assert.Equal(t, tc.wantType, evt.Type)
			if tc.wantType == abiv1.EventType_EVENT_TYPE_SESSION_STATUS {
				if tc.busy {
					assert.Equal(t, abiv1.SessionStatus_SESSION_STATUS_BUSY, evt.Status)
				} else {
					assert.Equal(t, abiv1.SessionStatus_SESSION_STATUS_IDLE, evt.Status)
				}
			}
		})
	}
}

// TestTranslateABI_BillingFieldsCarried: step.ended tokens and
// message-level cost map to contract Cost (Epic 33 shape).
func TestTranslateABI_BillingFieldsCarried(t *testing.T) {
	evt, ok := parse(t, `{"id":"e1","type":"session.next.step.ended","properties":{"sessionID":"s1","assistantMessageID":"m2","tokens":{"input":100,"output":50,"reasoning":10,"cache":{"read":5,"write":0}}}}`)
	require.True(t, ok)
	assert.Equal(t, abiv1.EventType_EVENT_TYPE_MESSAGE_END, evt.Type)
	require.NotNil(t, evt.Message)
	require.NotNil(t, evt.Message.Cost)
	assert.Equal(t, int64(100), evt.Message.Cost.InputTokens)
	assert.Equal(t, int64(50), evt.Message.Cost.OutputTokens)
	assert.Equal(t, int64(10), evt.Message.Cost.ReasoningTokens)
	assert.Equal(t, int64(5), evt.Message.Cost.CacheReadTokens)

	evt2, ok := parse(t, `{"id":"e2","type":"message.updated","properties":{"sessionID":"s1","info":{"id":"m3","role":"assistant","cost":{"total":0.012},"tokens":{"input":10,"output":5,"reasoning":0,"cache":{"read":0,"write":0}}}}}`)
	require.True(t, ok)
	require.NotNil(t, evt2.Message)
	require.NotNil(t, evt2.Message.Cost)
	assert.InDelta(t, 0.012, evt2.Message.Cost.CostUsd, 1e-9)
}

// TestTranslateABI_PartVariants: opencode part shapes → contract part
// types; unknown part shape → Custom valve.
func TestTranslateABI_PartVariants(t *testing.T) {
	cases := []struct {
		name     string
		part     string
		wantType abiv1.PartType
		check    func(t *testing.T, p *abiv1.Part)
	}{
		{"text", `{"id":"p1","type":"text","text":"hello","messageID":"m1","sessionID":"s1"}`, abiv1.PartType_PART_TYPE_TEXT,
			func(t *testing.T, p *abiv1.Part) { assert.Equal(t, "hello", p.GetText()) }},
		{"reasoning", `{"id":"p2","type":"reasoning","text":"thinking...","messageID":"m1","sessionID":"s1"}`, abiv1.PartType_PART_TYPE_REASONING,
			func(t *testing.T, p *abiv1.Part) { assert.Equal(t, "thinking...", p.GetReasoning()) }},
		{"tool", `{"id":"p3","type":"tool","callID":"call1","tool":"bash","state":{"status":"running"},"input":{"command":"ls"},"output":"ok","messageID":"m1","sessionID":"s1"}`, abiv1.PartType_PART_TYPE_TOOL,
			func(t *testing.T, p *abiv1.Part) {
				tool := p.GetTool()
				require.NotNil(t, tool)
				assert.Equal(t, "call1", tool.CallId)
				assert.Equal(t, "bash", tool.Name)
				assert.Equal(t, abiv1.ToolStatus_TOOL_STATUS_RUNNING, tool.State.Status)
				assert.NotNil(t, tool.Input)
				assert.JSONEq(t, `"ok"`, string(tool.Output), "output preserved as raw JSON")
			}},
		{"tool flat-shape time anchors the elapsed badge (#892)", `{"id":"p5","type":"tool","callID":"call2","tool":"bash","state":{"status":"running","time":{"start":1756684800123,"end":0}},"messageID":"m1","sessionID":"s1"}`, abiv1.PartType_PART_TYPE_TOOL,
			func(t *testing.T, p *abiv1.Part) {
				tool := p.GetTool()
				require.NotNil(t, tool)
				require.NotNil(t, tool.State)
				require.NotNil(t, tool.State.StartedAt, "state.time.start (epoch millis) must populate started_at — the live elapsed badge anchors on it")
				assert.Equal(t, int64(1756684800), tool.State.StartedAt.AsTime().Unix())
				assert.Equal(t, 123000000, tool.State.StartedAt.AsTime().Nanosecond())
			}},
		{"tool legacy nested shape still translates", `{"id":"p6","type":"tool","tool":{"name":"edit","callID":"call3","state":{"status":"completed","startedAt":"2026-08-31T12:00:00Z"}},"messageID":"m1","sessionID":"s1"}`, abiv1.PartType_PART_TYPE_TOOL,
			func(t *testing.T, p *abiv1.Part) {
				tool := p.GetTool()
				require.NotNil(t, tool)
				assert.Equal(t, "edit", tool.Name)
				assert.Equal(t, "call3", tool.CallId)
				require.NotNil(t, tool.State)
				assert.Equal(t, abiv1.ToolStatus_TOOL_STATUS_COMPLETED, tool.State.Status)
				assert.Equal(t, int64(1788177600), tool.State.StartedAt.AsTime().Unix())
			}},
		{"unknown_part_custom_valve", `{"id":"p4","type":"sonic-boom","weird":true,"messageID":"m1","sessionID":"s1"}`, abiv1.PartType_PART_TYPE_CUSTOM,
			func(t *testing.T, p *abiv1.Part) {
				c := p.GetCustom()
				require.NotNil(t, c)
				assert.Equal(t, "opencode.part.sonic-boom", c.Kind)
				assert.Contains(t, string(c.Data), "sonic-boom")
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"id":"e1","type":"message.part.updated","properties":{"sessionID":"s1","part":` + tc.part + `}}`
			evt, ok := parse(t, raw)
			require.True(t, ok)
			assert.Equal(t, abiv1.EventType_EVENT_TYPE_PART_END, evt.Type)
			require.NotNil(t, evt.Part)
			assert.Equal(t, tc.wantType, evt.Part.Type)
			if tc.check != nil {
				tc.check(t, evt.Part)
			}
		})
	}
}

// TestTranslateABI_QuestionPermissionLifecycle: question/permission events
// carry the full unified InputRequest shape in / resolved out.
func TestTranslateABI_QuestionPermissionLifecycle(t *testing.T) {
	asked, ok := parse(t, `{"id":"e1","type":"question.asked","properties":{"id":"q1","sessionID":"s1","questions":[{"question":"Proceed?","options":[{"label":"Yes","description":"proceed"},{"label":"No"}],"multiple":false}]}}`)
	require.True(t, ok)
	assert.Equal(t, abiv1.EventType_EVENT_TYPE_INPUT_REQUEST, asked.Type)
	require.NotNil(t, asked.Input)
	assert.Equal(t, abiv1.InputKind_INPUT_KIND_QUESTION, asked.Input.Kind)
	assert.Equal(t, "Proceed?", asked.Input.Question)
	require.Len(t, asked.Input.Options, 2)
	assert.Equal(t, "Yes", asked.Input.Options[0].Label)

	resolved, ok := parse(t, `{"id":"e2","type":"question.replied","properties":{"id":"q1","sessionID":"s1"}}`)
	require.True(t, ok)
	assert.Equal(t, abiv1.EventType_EVENT_TYPE_INPUT_RESOLVED, resolved.Type)
	assert.Equal(t, "q1", resolved.Input.Id)

	perm, ok := parse(t, `{"id":"e3","type":"permission.asked","properties":{"id":"pm1","sessionID":"s1","permission":"bash","patterns":["rm -rf /"],"always":["bash"]}}`)
	require.True(t, ok)
	assert.Equal(t, abiv1.EventType_EVENT_TYPE_INPUT_REQUEST, perm.Type)
	require.NotNil(t, perm.Input)
	assert.Equal(t, abiv1.InputKind_INPUT_KIND_PERMISSION, perm.Input.Kind)
	assert.Equal(t, "bash", perm.Input.Permission)
}

// TestTranslateABI_KnownButNonSession: server/plugin/catalog/file events
// are deliberately ignored (ok=false, counted separately — not the
// unknown-valve path).
func TestTranslateABI_KnownButNonSession(t *testing.T) {
	for _, raw := range []string{
		`{"id":"e1","type":"server.heartbeat","properties":{}}`,
		`{"id":"e2","type":"plugin.added","properties":{"plugin":"x"}}`,
		`{"id":"e3","type":"file.edited","properties":{"file":"/x"}}`,
		`{"id":"e4","type":"catalog.updated","properties":{}}`,
	} {
		_, ok := parse(t, raw)
		assert.False(t, ok, "non-session event should report ok=false: %s", raw)
	}
}

// TestTranslateABI_SessionSetLifecycle: created/updated carry the Session
// payload so the projection can enumerate the session set.
func TestTranslateABI_SessionSetLifecycle(t *testing.T) {
	created, ok := parse(t, `{"id":"e1","type":"session.created","properties":{"sessionID":"ses1","info":{"id":"ses1","title":"T","version":"1.18.15","directory":"/tmp/x","cost":{"total":0},"tokens":{"input":1,"output":2,"reasoning":0,"cache":{"read":0,"write":0}},"time":{"created":1787874778616,"updated":1787874778616}}}}`)
	require.True(t, ok)
	assert.Equal(t, abiv1.EventType_EVENT_TYPE_SESSION_UPDATED, created.Type)
	require.NotNil(t, created.Session)
	assert.Equal(t, "ses1", created.Session.Id)
	assert.Equal(t, "T", created.Session.Title)

	delta, ok := parse(t, `{"id":"e2","type":"message.part.delta","properties":{"sessionID":"s","messageID":"m","partID":"p","delta":"chunk"}}`)
	require.True(t, ok)
	assert.Equal(t, abiv1.EventType_EVENT_TYPE_PART_DELTA, delta.Type)
	assert.Equal(t, "chunk", delta.Delta)
	assert.Equal(t, "p", delta.PartId)
}
