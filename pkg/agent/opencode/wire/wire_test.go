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

// TestGoldenFixtureTaxonomy pins the CURRENT (1.18.10) event taxonomy from a
// live capture: no session.next.step.ended, usage in step-finish parts, and
// version-suffixed event type names. If opencode changes any of this, this
// test fails and the fixture must be re-captured (upgrade runbook).
func TestGoldenFixtureTaxonomy(t *testing.T) {
	data, err := os.ReadFile("../testdata/sse_events_1_18_10.jsonl")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var stepEnded, partUpdates, stepFinishParts, sessionUpdates int
	var suffixed int
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
		case env.Type == "session.updated.1":
			sessionUpdates++
		}
	}
	if stepEnded != 0 {
		t.Fatalf("legacy session.next.step.ended must be absent from the 1.18.10 fixture; got %d", stepEnded)
	}
	if partUpdates == 0 || stepFinishParts == 0 || sessionUpdates == 0 || suffixed == 0 {
		t.Fatalf("fixture must contain part-updates (%d), step-finish parts (%d), session.updates (%d), suffixed types (%d)",
			partUpdates, stepFinishParts, sessionUpdates, suffixed)
	}
	t.Logf("fixture: %d part-updates, %d step-finish (all decoded), %d session.updates",
		partUpdates, stepFinishParts, sessionUpdates)
}
