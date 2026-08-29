// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package wire

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestIsPartUpdatedToleratesVersionSuffix(t *testing.T) {
	tests := []struct {
		eventType string
		want      bool
	}{
		{"message.part.updated", true},
		{"message.part.updated.1", true},
		{"message.part.updated.12", true},
		{"session.updated.1", false},
		{"session.updated", false},
		{"message.updated.1", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsPartUpdated(tt.eventType); got != tt.want {
			t.Errorf("IsPartUpdated(%q) = %v, want %v", tt.eventType, got, tt.want)
		}
	}
}

func TestIsStepEndedMatchesLegacyNameOnly(t *testing.T) {
	for _, legacy := range []string{"session.next.step.ended"} {
		if !IsStepEnded(legacy) {
			t.Errorf("IsStepEnded(%q) must be true for legacy events", legacy)
		}
	}
	for _, other := range []string{"session.next.step.ended.1", "message.part.updated", "step-finish"} {
		if IsStepEnded(other) {
			t.Errorf("IsStepEnded(%q) must be false — the legacy event is unversioned", other)
		}
	}
}

func TestParseStepUsageFromPartUpdate(t *testing.T) {
	raw := `{"id":"evt1","type":"message.part.updated.1","properties":{"sessionID":"ses_a","part":{"id":"prt1","type":"step-finish","reason":"tool-calls","cost":0,"tokens":{"total":3950,"input":2310,"output":285,"reasoning":75,"cache":{"read":1280,"write":0}}}}}`
	u, ok, err := ParseStepUsage("message.part.updated.1", raw)
	if err != nil || !ok {
		t.Fatalf("ParseStepUsage: ok=%v err=%v", ok, err)
	}
	if u.SessionID != "ses_a" {
		t.Fatalf("SessionID = %q, want ses_a", u.SessionID)
	}
	if got := u.Tokens.PromptTokens(); got != 2310+1280+0 {
		t.Fatalf("PromptTokens = %d, want 3590 (input + cache.read + cache.write)", got)
	}
	if u.Tokens.Output != 285 || u.Tokens.Reasoning != 75 || u.Tokens.Total != 3950 {
		t.Fatalf("tokens = %+v", u.Tokens)
	}
}

func TestParseStepUsageFromPartUpdateWithoutSuffix(t *testing.T) {
	raw := `{"id":"evt1","type":"message.part.updated","properties":{"sessionID":"ses_a","part":{"type":"step-finish","tokens":{"input":100,"cache":{"read":50,"write":25}}}}}`
	u, ok, err := ParseStepUsage("message.part.updated", raw)
	if err != nil || !ok {
		t.Fatalf("ParseStepUsage: ok=%v err=%v", ok, err)
	}
	if got := u.Tokens.PromptTokens(); got != 175 {
		t.Fatalf("PromptTokens = %d, want 175", got)
	}
}

func TestParseStepUsageFromLegacyStepEnded(t *testing.T) {
	raw := `{"type":"session.next.step.ended","properties":{"sessionID":"ses_legacy","tokens":{"input":800,"output":400,"reasoning":100,"cache":{"read":200,"write":50}}}}`
	u, ok, err := ParseStepUsage("session.next.step.ended", raw)
	if err != nil || !ok {
		t.Fatalf("ParseStepUsage: ok=%v err=%v", ok, err)
	}
	if u.SessionID != "ses_legacy" {
		t.Fatalf("SessionID = %q", u.SessionID)
	}
	if got := u.Tokens.PromptTokens(); got != 800+200+50 {
		t.Fatalf("PromptTokens = %d, want 1050", got)
	}
}

func TestParseStepUsageNonFinishPartIsNotUsage(t *testing.T) {
	raw := `{"id":"evt1","type":"message.part.updated.1","properties":{"sessionID":"ses_a","part":{"type":"text","text":"hello"}}}`
	_, ok, err := ParseStepUsage("message.part.updated.1", raw)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok {
		t.Fatalf("text parts carry no usage; ok must be false")
	}
}

func TestParseStepUsageMalformed(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
		raw       string
	}{
		{"broken json", "message.part.updated.1", `{"type":"message.part.updated.1","properties":`},
		{"missing sessionID", "message.part.updated.1", `{"properties":{"part":{"type":"step-finish","tokens":{"input":1}}}}`},
		{"missing tokens", "message.part.updated.1", `{"properties":{"sessionID":"ses_a","part":{"type":"step-finish"}}}`},
		{"wrong event type", "session.updated.1", `{"properties":{"sessionID":"ses_a"}}`},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, ok, err := ParseStepUsage(tt.eventType, tt.raw)
			if err == nil && ok {
				t.Fatalf("malformed input must not decode to usage")
			}
		})
	}
}

// Two golden fixtures pin the CURRENT (1.18.10) event surfaces — see
// testdata/REFRESH.md for provenance and refresh procedure. The same
// logical event type carries different names on the two surfaces; that
// dual reality is why the decoder is suffix-tolerant:
//
//   - LIVE /event SSE stream: unsuffixed types (message.part.updated),
//     session.status present (never persisted), no session.next.step.ended
//   - persisted event store: version-suffixed types (message.part.updated.1)
//
// If either test fails on main without a seam change, opencode drifted —
// re-capture the fixture before touching the parser.
func TestGoldenFixtureTaxonomy_LiveWire(t *testing.T) {
	for _, fixture := range []string{"../testdata/sse_events_1_18_10_live.jsonl", "../testdata/sse_events_1_18_15_live.jsonl"} {
		t.Run(fixture, func(t *testing.T) {
			taxonomyLiveWireFixture(t, fixture)
		})
	}
}

func taxonomyLiveWireFixture(t *testing.T, fixture string) {
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var stepEnded, partUpdates, stepFinishParts, sessionUpdates, suffixed, sessionStatus int
	decoded := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var env Envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("fixture line is not a valid envelope: %v — %s", err, line[:80])
		}
		if env.Type == "" {
			t.Fatalf("fixture line missing type: %s", line[:80])
		}
		if IsStepEnded(env.Type) {
			stepEnded++
		}
		if strings.HasSuffix(env.Type, ".1") {
			suffixed++
		}
		switch {
		case IsPartUpdated(env.Type):
			partUpdates++
			var p struct {
				Part struct {
					Type   string  `json:"type"`
					Tokens *Tokens `json:"tokens"`
				} `json:"part"`
				SessionID string `json:"sessionID"`
			}
			if err := json.Unmarshal(env.Properties, &p); err != nil {
				t.Fatalf("part-update properties undecodable: %v", err)
			}
			if p.Part.Type == "step-finish" {
				stepFinishParts++
				if p.Part.Tokens == nil {
					t.Fatalf("step-finish part without tokens — wire shape drifted")
				}
				if p.SessionID == "" {
					t.Fatalf("step-finish part-update without sessionID")
				}
				if _, ok, err := ParseStepUsage(env.Type, line); err != nil || !ok {
					t.Fatalf("ParseStepUsage must decode every golden step-finish event: err=%v ok=%v", err, ok)
				}
				decoded++
			}
		case env.Type == "session.updated":
			sessionUpdates++
		case env.Type == "session.status":
			sessionStatus++
		}
	}
	if stepEnded != 0 {
		t.Fatalf("legacy session.next.step.ended must be absent; got %d", stepEnded)
	}
	if suffixed != 0 {
		t.Fatalf("live /event wire types are UNSUFFIXED (verified by verbatim capture); got %d suffixed — wrong fixture?", suffixed)
	}
	if partUpdates == 0 || stepFinishParts == 0 || sessionUpdates == 0 || sessionStatus == 0 {
		t.Fatalf("live fixture must contain part-updates (%d), step-finish parts (%d), session.updates (%d), session.status (%d)",
			partUpdates, stepFinishParts, sessionUpdates, sessionStatus)
	}
	t.Logf("live fixture: %d part-updates, %d step-finish (all decoded), %d session.updates, %d session.status",
		partUpdates, stepFinishParts, sessionUpdates, sessionStatus)
}

func TestGoldenFixtureTaxonomy_EventStore(t *testing.T) {
	for _, fixture := range []string{"../testdata/event_store_1_18_10.jsonl", "../testdata/event_store_1_18_15.jsonl"} {
		t.Run(fixture, func(t *testing.T) {
			taxonomyEventStoreFixture(t, fixture)
		})
	}
}

func taxonomyEventStoreFixture(t *testing.T, fixture string) {
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var stepEnded, partUpdates, stepFinishParts, sessionUpdates, suffixed int
	decoded := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var env Envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("fixture line is not a valid envelope: %v — %s", err, line[:80])
		}
		if IsStepEnded(env.Type) {
			stepEnded++
		}
		if strings.HasSuffix(env.Type, ".1") {
			suffixed++
		}
		switch {
		case IsPartUpdated(env.Type):
			partUpdates++
			var p struct {
				Part struct {
					Type   string  `json:"type"`
					Tokens *Tokens `json:"tokens"`
				} `json:"part"`
				SessionID string `json:"sessionID"`
			}
			if err := json.Unmarshal(env.Properties, &p); err != nil {
				t.Fatalf("part-update properties undecodable: %v", err)
			}
			if p.Part.Type == "step-finish" {
				stepFinishParts++
				if p.Part.Tokens == nil || p.SessionID == "" {
					t.Fatalf("step-finish store row without tokens/sessionID — shape drifted")
				}
				if _, ok, err := ParseStepUsage(env.Type, line); err != nil || !ok {
					t.Fatalf("ParseStepUsage must decode suffixed store events too: err=%v ok=%v", err, ok)
				}
				decoded++
			}
		case env.Type == "session.updated.1":
			sessionUpdates++
		}
	}
	if stepEnded != 0 {
		t.Fatalf("legacy session.next.step.ended must be absent from the 1.18.10 store; got %d", stepEnded)
	}
	if partUpdates == 0 || stepFinishParts == 0 || sessionUpdates == 0 || suffixed == 0 {
		t.Fatalf("store fixture must contain part-updates (%d), step-finish parts (%d), session.updates (%d), suffixed types (%d)",
			partUpdates, stepFinishParts, sessionUpdates, suffixed)
	}
	t.Logf("store fixture: %d part-updates, %d step-finish (all decoded), %d session.updates, %d suffixed",
		partUpdates, stepFinishParts, sessionUpdates, suffixed)
}

// --- session.updated (cumulative usage / metering attribution) ---

func TestIsSessionUpdatedToleratesVersionSuffix(t *testing.T) {
	for _, tt := range []struct {
		eventType string
		want      bool
	}{
		{"session.updated", true},
		{"session.updated.1", true},
		{"session.status", false},
		{"message.part.updated", false},
		{"", false},
	} {
		if got := IsSessionUpdated(tt.eventType); got != tt.want {
			t.Errorf("IsSessionUpdated(%q) = %v, want %v", tt.eventType, got, tt.want)
		}
	}
}

func TestParseSessionUpdated(t *testing.T) {
	// Verbatim shape from the tracker_test pin (1.15.12 live capture):
	// tokens and model under properties.info; cost a bare number.
	raw15 := `{"id":"evt_t","type":"session.updated","properties":{"sessionID":"ses_abc","info":{"id":"ses_abc","cost":0.042,"tokens":{"input":509911,"output":20861,"reasoning":41,"cache":{"read":9229154,"write":0}},"model":{"id":"glm-5.1","providerID":"thekao cloud","variant":"default"}}}}`
	u, ok, err := ParseSessionUpdated("session.updated", raw15)
	if err != nil || !ok {
		t.Fatalf("ParseSessionUpdated: ok=%v err=%v", ok, err)
	}
	if u.SessionID != "ses_abc" || u.ModelID != "glm-5.1" || u.ProviderID != "thekao cloud" {
		t.Fatalf("attribution = %+v", u)
	}
	if u.InputTokens != 509911 || u.OutputTokens != 20861 {
		t.Fatalf("tokens = %+v", u)
	}
	if u.CostUSD != 0.042 {
		t.Fatalf("CostUSD = %v", u.CostUSD)
	}
}

func TestParseSessionUpdatedSuffixedAndObjectCost(t *testing.T) {
	// Store-surface variant: suffixed type; cost may arrive as an object
	// whose "cost" field is the dollar amount (1.18.x shape).
	raw18 := `{"id":"evt_t","type":"session.updated.1","properties":{"sessionID":"ses_x","info":{"id":"ses_x","cost":{"cost":0.5,"total":12345},"tokens":{"input":100,"output":50},"model":{"id":"m","provider":"p"}}}}`
	u, ok, err := ParseSessionUpdated("session.updated.1", raw18)
	if err != nil || !ok {
		t.Fatalf("ParseSessionUpdated: ok=%v err=%v", ok, err)
	}
	if u.CostUSD != 0.5 {
		t.Fatalf("object cost must yield the dollar field, got %v (total=12345 is a token count, not cost)", u.CostUSD)
	}
	if u.ProviderID != "p" {
		t.Fatalf("provider fallback (provider when providerID absent) failed: %+v", u)
	}
}

func TestParseSessionUpdatedNotUsageOrIncomplete(t *testing.T) {
	// Different event type → ok=false, no error.
	if _, ok, err := ParseSessionUpdated("session.status", `{"properties":{}}`); err != nil || ok {
		t.Fatalf("wrong type: ok=%v err=%v", ok, err)
	}
	// Usage-typed but undecodable properties → drift error.
	if _, _, err := ParseSessionUpdated("session.updated", `{"type":"session.updated","properties":[1]}`); err == nil {
		t.Fatalf("undecodable properties must be a drift error")
	}
	// Missing info → not usage (ok=false, no error): session.updated
	// events legitimately fire without info (e.g. early lifecycle).
	if _, ok, err := ParseSessionUpdated("session.updated", `{"properties":{"sessionID":"s"}}`); err != nil || ok {
		t.Fatalf("info-less event: ok=%v err=%v", ok, err)
	}
}

// The live fixtures' session.updated events must all decode through
// the metering path — billing verified against verbatim wire, not just
// hand-written shapes. Runs against every pinned live capture.
func TestGoldenFixture_SessionUpdatedAllDecode(t *testing.T) {
	for _, fixture := range []string{"../testdata/sse_events_1_18_10_live.jsonl", "../testdata/sse_events_1_18_15_live.jsonl"} {
		t.Run(fixture, func(t *testing.T) {
			sessionUpdatedAllDecode(t, fixture)
		})
	}
}

func sessionUpdatedAllDecode(t *testing.T, fixture string) {
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	decoded := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var env Envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			continue
		}
		if !IsSessionUpdated(env.Type) {
			continue
		}
		u, ok, err := ParseSessionUpdated(env.Type, line)
		if err != nil {
			t.Fatalf("golden session.updated must not drift-error: %v — %s", err, line[:100])
		}
		if ok {
			if u.SessionID == "" || u.ModelID == "" {
				t.Fatalf("golden session.updated missing attribution: %+v — %s", u, line[:100])
			}
			decoded++
		}
	}
	if decoded == 0 {
		t.Fatalf("live fixture must contain decodable session.updated events")
	}
	t.Logf("%d golden session.updated events decoded with attribution", decoded)
}

func TestParseSessionUpdatedIdentityIsInfoID(t *testing.T) {
	// Pin the metering identity contract at the seam layer: session
	// identity comes from info.id, NOT properties.sessionID. An empty
	// info.id must decode ok with an empty SessionID so the billing
	// path can warn-and-skip (pinned end-to-end by
	// TestSSETracker_Inference_EmptyID_LogsWarn).
	raw := `{"type":"session.updated","properties":{"sessionID":"ses_props","info":{"id":"","cost":0,"tokens":{"input":10,"output":5},"model":{"id":"m"}}}}`
	u, ok, err := ParseSessionUpdated("session.updated", raw)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if u.SessionID != "" {
		t.Fatalf("identity must be info.id (empty here), not properties.sessionID; got %q", u.SessionID)
	}
}

func TestParseSessionUpdatedMalformedCostFlags(t *testing.T) {
	raw := `{"type":"session.updated","properties":{"sessionID":"s","info":{"id":"s","cost":"not-a-number","tokens":{"input":10,"output":5},"model":{"id":"m"}}}}`
	u, ok, err := ParseSessionUpdated("session.updated", raw)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if !u.CostMalformed || u.CostUSD != 0 {
		t.Fatalf("malformed cost must flag CostMalformed and decode as 0; got %+v", u)
	}
}

// Store-surface session.updated rows (suffixed) must decode through
// the metering path — the two-projection (wire vs store) drift concern
// is exactly what #939 exists to close; counting them in the taxonomy
// test is not enough. Exact counts are pinned per fixture: composition
// changes must be conscious (refresh per REFRESH.md), not silent drift.
func TestGoldenFixture_EventStore_SessionUpdatedAllDecode(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		pinned  int
	}{
		{"../testdata/event_store_1_18_10.jsonl", 81},
		{"../testdata/event_store_1_18_15.jsonl", 5},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			eventStoreSessionUpdatedAllDecode(t, tc.fixture, tc.pinned)
		})
	}
}

func eventStoreSessionUpdatedAllDecode(t *testing.T, fixture string, pinned int) {
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	decoded := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var env Envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			continue
		}
		if !IsSessionUpdated(env.Type) {
			continue
		}
		u, ok, err := ParseSessionUpdated(env.Type, line)
		if err != nil {
			t.Fatalf("golden store session.updated must not drift-error: %v — %s", err, line[:100])
		}
		if ok {
			if u.SessionID == "" || u.ModelID == "" {
				t.Fatalf("golden store row missing attribution: %+v — %s", u, line[:100])
			}
			decoded++
		}
	}
	// Exact count pinned: fixture composition changes must be conscious
	// (refresh per REFRESH.md), not silent drift.
	if decoded != pinned {
		t.Fatalf("store fixture session.updated decode count changed: got %d, pinned %d — fixture refreshed?", decoded, pinned)
	}
	t.Logf("%d golden store session.updated rows decoded with attribution", decoded)
}

// --- IsKnownEventType (drift observability taxonomy) ---

func TestIsKnownEventType(t *testing.T) {
	known := []string{
		// Live /event taxonomy (captured, sse_events_1_18_10_live.jsonl)
		"session.status", "session.updated", "session.created",
		"message.part.updated", "message.part.delta", "message.updated",
		"session.idle", "session.diff", "server.connected", "server.heartbeat",
		"plugin.added", "catalog.updated", "reference.updated",
		"integration.updated", "file.edited", "file.watcher.updated",
		// Legacy event system (mixed fleet)
		"session.next.step.ended", "session.next.step.started",
		"session.next.step.failed", "session.next.prompt.admitted",
		"session.next.prompted", "session.next.text.started",
		"session.next.text.delta", "session.next.text.ended",
		"message.created", "session.error",
	}
	for _, tt := range known {
		if !IsKnownEventType(tt) {
			t.Errorf("IsKnownEventType(%q) = false, want true", tt)
		}
	}
	// Version-suffixed store variants are the same logical types.
	if !IsKnownEventType("message.part.updated.1") || !IsKnownEventType("session.updated.12") {
		t.Error("suffixed variants of known types must be known")
	}
	unknown := []string{"", "totally.new.event", "session.updatedx", "message.part.updated.x", "SESsion.status"}
	for _, tt := range unknown {
		if IsKnownEventType(tt) {
			t.Errorf("IsKnownEventType(%q) = true, want false", tt)
		}
	}
}

// Every event type in BOTH golden fixtures must classify as known — the
// fixtures are the pinned truth; a fixture refresh that introduces a new
// type must extend the taxonomy in the same change (this test forces it).
func TestIsKnownEventType_CoversBothFixtures(t *testing.T) {
	for _, fixture := range []string{
		"../testdata/sse_events_1_18_10_live.jsonl",
		"../testdata/event_store_1_18_10.jsonl",
		"../testdata/sse_events_1_18_15_live.jsonl",
		"../testdata/event_store_1_18_15.jsonl",
	} {
		data, err := os.ReadFile(fixture)
		if err != nil {
			t.Fatalf("read fixture: %v", err)
		}
		seen := map[string]bool{}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var env Envelope
			if json.Unmarshal([]byte(line), &env) != nil || env.Type == "" {
				continue
			}
			if !seen[env.Type] {
				seen[env.Type] = true
				if !IsKnownEventType(env.Type) {
					t.Errorf("%s: fixture type %q not in taxonomy — extend IsKnownEventType with the fixture refresh", fixture, env.Type)
				}
			}
		}
	}
}
