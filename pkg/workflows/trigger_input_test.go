// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

import (
	"encoding/json"
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

const testWebhookEnvelope = `{"source":{"type":"webhook","id":"t1"},"received_at":"2026-09-17T00:00:00Z","headers":{"X-Hub-Signature-256":"sha256=x"},"body":{"topic":"hello","nested":{"a":1}}}`

const testCronEnvelope = `{"source":{"type":"cron","id":"t1"},"received_at":"2026-09-17T00:00:00Z"}`

// TestResolveTriggerInput pins the design 0059 §3.4 resolution matrix:
// modes × static-present × body-kind × nil-static — including the
// byte-identical envelope default (legacy triggers) and overlay-wins
// shallow merge.
func TestResolveTriggerInput(t *testing.T) {
	cases := []struct {
		name      string
		inputFrom string
		static    string // "" = absent; "null" = JSON null literal = absent
		envelope  string
		want      string
		wantBytes bool // want must match BYTES (not just structurally)
	}{
		// --- envelope mode (legacy default) ---
		{"envelope no static returns envelope bytes verbatim", "envelope", "", testCronEnvelope, testCronEnvelope, true},
		{"empty inputFrom is envelope", "", "", testCronEnvelope, testCronEnvelope, true},
		{"envelope + JSON-null static is still verbatim", "envelope", "null", testCronEnvelope, testCronEnvelope, true},
		{"envelope + static overlays, static wins", "envelope", `{"topic":"nightly","extra":true}`, testCronEnvelope,
			`{"received_at":"2026-09-17T00:00:00Z","source":{"id":"t1","type":"cron"},"topic":"nightly","extra":true}`, false},
		{"envelope + empty-object static keeps envelope keys", "envelope", `{}`, testCronEnvelope,
			`{"received_at":"2026-09-17T00:00:00Z","source":{"id":"t1","type":"cron"}}`, false},

		// --- body mode (webhook only) ---
		{"body no static returns the payload document verbatim", "body", "", testWebhookEnvelope, `{"topic":"hello","nested":{"a":1}}`, true},
		{"body + static overlays onto the payload object", "body", `{"topic":"override"}`, testWebhookEnvelope,
			`{"nested":{"a":1},"topic":"override"}`, false},
		{"body non-object (array) no static returns the array", "body", "", envelopeWithBody(`["a","b"]`), `["a","b"]`, true},
		{"body non-object (string) no static returns the string", "body", "", envelopeWithBody(`"ping"`), `"ping"`, true},
		{"body non-object (raw fallback doc) returns the fallback doc", "body", "", envelopeWithBody(`{"raw":"a=1","content_type":"application/x-www-form-urlencoded"}`), `{"content_type":"application/x-www-form-urlencoded","raw":"a=1"}`, false},
		{"body non-object + static: static IS the input (overlay undefined)", "body", `{"topic":"x"}`, envelopeWithBody(`["a","b"]`), `{"topic":"x"}`, true},
		{"body absent key no static: nil input", "body", "", `{"source":{"type":"webhook","id":"t"},"received_at":"z"}`, `null`, false},

		// --- mapped mode ---
		{"mapped no static: nil input", "mapped", "", testWebhookEnvelope, `null`, false},
		{"mapped + static: static verbatim, no envelope keys", "mapped", `{"topic":"nightly"}`, testWebhookEnvelope, `{"topic":"nightly"}`, true},
		{"mapped + static passes through byte-exact", "mapped", `{"b":2,"a":1}`, testWebhookEnvelope, `{"b":2,"a":1}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var static json.RawMessage
			if tc.static != "" {
				static = json.RawMessage(tc.static)
			}
			got, err := ResolveTriggerInput(TriggerInputSpec{InputFrom: tc.inputFrom, Input: static}, json.RawMessage(tc.envelope))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantBytes {
				if string(got) != tc.want {
					t.Fatalf("expected byte-exact %s, got %s", tc.want, got)
				}
				return
			}
			var wantV, gotV any
			if err := json.Unmarshal([]byte(tc.want), &wantV); err != nil {
				t.Fatalf("want not JSON: %v", err)
			}
			if got == nil {
				got = json.RawMessage("null")
			}
			if err := json.Unmarshal(got, &gotV); err != nil {
				t.Fatalf("got not JSON (%s): %v", got, err)
			}
			assertJSONEqValue(t, wantV, gotV, tc.want, string(got))
		})
	}
}

func envelopeWithBody(body string) string {
	return `{"source":{"type":"webhook","id":"t1"},"received_at":"2026-09-17T00:00:00Z","body":` + body + `}`
}

func assertJSONEqValue(t *testing.T, want, got any, wantRaw, gotRaw string) {
	t.Helper()
	wb, _ := json.Marshal(want)
	gb, _ := json.Marshal(got)
	if string(wb) != string(gb) {
		t.Fatalf("structural mismatch:\n want %s\n got  %s", wantRaw, gotRaw)
	}
}

// TestResolveTriggerInput_OverlayIsShallow pins that the static overlay is
// a TOP-LEVEL merge only: nested objects are replaced wholesale, never
// deep-merged.
func TestResolveTriggerInput_OverlayIsShallow(t *testing.T) {
	got, err := ResolveTriggerInput(
		TriggerInputSpec{InputFrom: "body", Input: json.RawMessage(`{"nested":{"b":2}}`)},
		json.RawMessage(envelopeWithBody(`{"nested":{"a":1}}`)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(got, &doc); err != nil {
		t.Fatalf("got not an object: %s", got)
	}
	if string(doc["nested"]) != `{"b":2}` {
		t.Fatalf("nested object must be replaced by the static value wholesale, got %s", doc["nested"])
	}
}

// TestTriggerOptedIn pins D3's scope: only triggers carrying an input
// mapping get fire-time validation.
func TestTriggerOptedIn(t *testing.T) {
	cases := []struct {
		name      string
		inputFrom string
		input     string
		want      bool
	}{
		{"legacy row (empty mode, no static)", "", "", false},
		{"explicit envelope, no static", "envelope", "", false},
		{"JSON-null static is not an opt-in", "envelope", "null", false},
		{"static input opts in", "envelope", `{"topic":"x"}`, true},
		{"body mode opts in", "body", "", true},
		{"mapped mode opts in", "mapped", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var in json.RawMessage
			if tc.input != "" {
				in = json.RawMessage(tc.input)
			}
			if got := TriggerOptedIn(tc.inputFrom, in); got != tc.want {
				t.Fatalf("TriggerOptedIn(%q, %s) = %v, want %v", tc.inputFrom, tc.input, got, tc.want)
			}
		})
	}
}

// TestRequiredNonEnvelopeProperties pins the V6 predicate: top-level
// `required` entries the source's envelope key set provides no value for.
func TestRequiredNonEnvelopeProperties(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","required":["topic","source","received_at"],"properties":{"topic":{"type":"string"}}}`)
	cases := []struct {
		name       string
		schema     string
		sourceType string
		want       string // JSON array; "null" = none
	}{
		{"cron schema requiring topic trips", `{"required":["topic"]}`, types.TriggerSourceCron, `["topic"]`},
		{"cron schema requiring only envelope keys passes", `{"required":["source","received_at"]}`, types.TriggerSourceCron, `null`},
		{"webhook schema requiring body passes", `{"required":["body","headers"]}`, types.TriggerSourceWebhook, `null`},
		{"webhook schema requiring topic+body names topic", `{"required":["topic","body"]}`, types.TriggerSourceWebhook, `["topic"]`},
		{"absent schema requires nothing", ``, types.TriggerSourceCron, `null`},
		{"null schema requires nothing", `null`, types.TriggerSourceCron, `null`},
		{"unparseable schema requires nothing (guard skips)", `{"required":`, types.TriggerSourceCron, `null`},
		{"no required key requires nothing", `{"type":"object"}`, types.TriggerSourceCron, `null`},
		{"full example", string(schema), types.TriggerSourceCron, `["topic"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RequiredNonEnvelopeProperties(json.RawMessage(tc.schema), tc.sourceType)
			gb, _ := json.Marshal(got)
			if string(gb) != tc.want {
				t.Fatalf("got %s, want %s", gb, tc.want)
			}
		})
	}
}

// TestNormalizeTriggerInputFrom: unset means the legacy envelope default.
func TestNormalizeTriggerInputFrom(t *testing.T) {
	if got := NormalizeTriggerInputFrom(""); got != types.TriggerInputFromEnvelope {
		t.Fatalf("empty must normalize to envelope, got %q", got)
	}
	if got := NormalizeTriggerInputFrom("body"); got != "body" {
		t.Fatalf("body must pass through, got %q", got)
	}
}
