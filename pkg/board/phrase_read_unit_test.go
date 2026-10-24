// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// phrase_read_unit_test.go — #1922 unit tier (the ids_phrase_unit_test
// convention): the translation helpers are pure or seam-injected, so
// their branch table lives here — malformed/session-less/already-phrase
// passthroughs, the degraded shapes, the failing-consult nil contract,
// and the resolve-failure flag path.

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
)

// failingPhraseQueryer always errors (the consult-failure contract).
type failingPhraseQueryer struct{}

func (failingPhraseQueryer) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errUnitTestQuery //nolint:staticcheck // deliberately opaque
}

var errUnitTestQuery = &queryErr{}

type queryErr struct{}

func (*queryErr) Error() string { return "unit-test consult failure" }

func TestLookupSessionPhrases(t *testing.T) {
	if got := LookupSessionPhrases(context.Background(), failingPhraseQueryer{}, "ws", []string{"ses_a"}); got != nil {
		t.Errorf("failing consult must return a nil map (degrade), got %v", got)
	}
	if got := LookupSessionPhrases(context.Background(), failingPhraseQueryer{}, "", []string{"ses_a"}); got != nil {
		t.Errorf("empty workspace must short-circuit nil, got %v", got)
	}
	if got := LookupSessionPhrases(context.Background(), failingPhraseQueryer{}, "ws", nil); got != nil {
		t.Errorf("empty session set must short-circuit nil, got %v", got)
	}
}

func TestFlagUntranslated(t *testing.T) {
	byProject := map[string][]*Task{
		"p1": {{AssigneeSession: "ses_a"}, {AssigneeSession: "ses_b"}},
	}
	flagUntranslated(byProject)
	for _, ts := range byProject {
		for _, task := range ts {
			if !task.AssigneeSessionDeprecated {
				t.Errorf("resolve failure must flag %q deprecated (raw stays)", task.AssigneeSession)
			}
		}
	}
}

func TestTranslateActorSessionPhrase(t *testing.T) {
	phrases := map[string]string{"ses_known": "amber-falcon-glides"}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty passthrough", ``, ``},
		{"malformed JSON passthrough", `{not-json`, `{not-json`},
		{"session-less actor passthrough", `{"kind":"user","surface":"rest"}`, `{"kind":"user","surface":"rest"}`},
		{"already-phrase guard (double-translation)", `{"kind":"user","session_id":"amber-falcon-glides"}`, `{"kind":"user","session_id":"amber-falcon-glides"}`},
		{"warm translate", `{"kind":"user","session_id":"ses_known"}`, `{"kind":"user","session_id":"amber-falcon-glides"}`},
		{"cold degrade carries the #1821 flag", `{"kind":"user","session_id":"ses_cold"}`, `{"kind":"user","session_id":"ses_cold","session_id_deprecated":"true"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(TranslateActorSessionPhrase([]byte(tc.in), phrases))
			if got != tc.want {
				t.Errorf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestPayloadAssigneeIDs(t *testing.T) {
	payload := []byte(`{"snapshot":{"assignee_session":"ses_a","stage":"in_progress"},
		"other":{"nested":[{"assignee_session":"ses_b"},{"assignee_session":"amber-falcon-glides"}]}}`)
	got := PayloadAssigneeIDs(payload)
	slices.Sort(got) // set semantics — the map-walk's visit order is not contractual (Go map iteration is randomized; only the collected set is)
	want := []string{"ses_a", "ses_b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v (phrase-form values are skipped)", got, want)
	}
	if PayloadAssigneeIDs([]byte(`{"none":true}`)) != nil {
		t.Errorf("no keys → nil")
	}
	if PayloadAssigneeIDs([]byte(`not-json`)) != nil {
		t.Errorf("malformed → nil")
	}
}

func TestTranslatePayloadAssignees(t *testing.T) {
	phrases := map[string]string{"ses_known": "amber-falcon-glides"}

	noIDs := []byte(`{"id":"t","snapshot":{"stage":"backlog"}}`)
	if got := string(TranslatePayloadAssignees(noIDs, phrases)); got != string(noIDs) {
		t.Errorf("payload without assignee keys must return byte-identical, got %s", got)
	}
	malformed := []byte(`not-json`)
	if got := string(TranslatePayloadAssignees(malformed, phrases)); got != string(malformed) {
		t.Errorf("malformed payload must return byte-identical, got %s", got)
	}

	warm := []byte(`{"snapshot":{"assignee_session":"ses_known"}}`)
	var m map[string]any
	if err := json.Unmarshal(TranslatePayloadAssignees(warm, phrases), &m); err != nil {
		t.Fatalf("warm output must be valid JSON: %v", err)
	}
	snap := m["snapshot"].(map[string]any)
	if snap["assignee_session"] != "amber-falcon-glides" {
		t.Errorf("warm snapshot must carry the phrase, got %v", snap["assignee_session"])
	}
	if _, flagged := snap["assignee_session_deprecated"]; flagged {
		t.Errorf("warm snapshot must not carry the deprecation flag")
	}

	cold := []byte(`{"snapshot":{"assignee_session":"ses_cold"}}`)
	if err := json.Unmarshal(TranslatePayloadAssignees(cold, phrases), &m); err != nil {
		t.Fatalf("cold output must be valid JSON: %v", err)
	}
	snap = m["snapshot"].(map[string]any)
	if snap["assignee_session"] != "ses_cold" {
		t.Errorf("cold snapshot must keep the raw id (never worse), got %v", snap["assignee_session"])
	}
	if snap["assignee_session_deprecated"] != true {
		t.Errorf("cold snapshot must carry assignee_session_deprecated=true, got %v", snap["assignee_session_deprecated"])
	}
}
