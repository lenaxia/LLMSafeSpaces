// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package utilities

import (
	"encoding/json"
	"testing"
)

// #1214: MaskSensitiveFieldsWithList was array-blind — the pod-bootstrap
// payload nests credentials at secrets.entries[].value (an array of
// objects), so "value" keys inside array elements were never reached.
// These tests execute the masker against the REAL payload shape observed
// in the INFO logs.

func mustUnmarshal(t *testing.T, raw string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func TestMaskArrayNestedPodBootstrapShape(t *testing.T) {
	raw := `{"workspace":"ws","secrets":{"entries":[{"name":"gh","value":"ghp_secret1234567890"},{"name":"ssh","value":"-----BEGIN OPENSSH PRIVATE KEY-----abcd"}]},"other":"visible"}`
	m := mustUnmarshal(t, raw)

	MaskSensitiveFieldsWithList(m, []string{"value"})

	out, _ := json.Marshal(m["secrets"])
	if s := string(out); containsPlain(s, "ghp_secret1234567890") || containsPlain(s, "OPENSSH PRIVATE KEY") {
		t.Fatalf("array-nested value NOT masked — the #1214 leak: %s", out)
	}
	if m["other"] != "visible" || m["workspace"] != "ws" {
		t.Fatal("non-sensitive fields must survive")
	}
}

func TestMaskArrayDirectValues(t *testing.T) {
	m := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{"value": "short"},
			map[string]interface{}{"value": "longer-secret-value-here"},
			"plain-string-in-array",
		},
	}
	MaskSensitiveFieldsWithList(m, []string{"value"})

	items := m["items"].([]interface{})
	e0 := items[0].(map[string]interface{})["value"].(string)
	e1 := items[1].(map[string]interface{})["value"].(string)
	if e0 != "********" {
		t.Errorf("short value in array not fully masked: %q", e0)
	}
	if containsPlain(e1, "longer-secret-value-here") {
		t.Errorf("long value in array leaked: %q", e1)
	}
	if items[2] != "plain-string-in-array" {
		t.Errorf("non-map array element mutated: %v", items[2])
	}
}

func TestMaskDeeplyNestedArraysInMapsInArrays(t *testing.T) {
	raw := `{"a":[{"b":[{"value":"deep-secret-abcdef"}]}]}`
	m := mustUnmarshal(t, raw)
	MaskSensitiveFieldsWithList(m, []string{"value"})
	out, _ := json.Marshal(m)
	if containsPlain(string(out), "deep-secret-abcdef") {
		t.Fatalf("deeply array-nested value leaked: %s", out)
	}
}

func TestMaskNilSafety(t *testing.T) {
	m := map[string]interface{}{"arr": []interface{}{nil, 42, map[string]interface{}{"value": "x-short"}}, "n": nil}
	MaskSensitiveFieldsWithList(m, []string{"value"}) // must not panic
}

func containsPlain(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) > 0 && stringContains(s, sub))
}

func stringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
