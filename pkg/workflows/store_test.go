// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestNullableJSON(t *testing.T) {
	tests := []struct {
		name string
		in   json.RawMessage
		want any
	}{
		{"empty → nil", json.RawMessage{}, nil},
		{"nil → nil", nil, nil},
		{"non-empty → bytes", json.RawMessage(`{"x":1}`), []byte(`{"x":1}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nullableJSON(tt.in)
			if tt.want == nil {
				if got != nil {
					t.Errorf("expected nil, got %v", got)
				}
			} else {
				if string(got.([]byte)) != string(tt.want.([]byte)) {
					t.Errorf("expected %v, got %v", tt.want, got)
				}
			}
		})
	}
}

func TestNullableStrPtr(t *testing.T) {
	s := "hello"
	got := nullableStrPtr(&s)
	if got != "hello" {
		t.Errorf("expected 'hello', got %v", got)
	}

	got = nullableStrPtr(nil)
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestToNullableStringArray(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want any
	}{
		{"empty → nil", []string{}, nil},
		{"nil → nil", nil, nil},
		{"non-empty → []string", []string{"a", "b"}, []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toNullableStringArray(tt.in)
			if tt.want == nil {
				if got != nil {
					t.Errorf("expected nil, got %v", got)
				}
			} else {
				sl, ok := got.([]string)
				if !ok {
					t.Fatalf("expected []string, got %T", got)
				}
				if len(sl) != len(tt.want.([]string)) {
					t.Errorf("length mismatch: got %d, want %d", len(sl), len(tt.want.([]string)))
				}
			}
		})
	}
}

// fakePGError implements the SQLState() string method that isUniqueViolation checks.
type fakePGError struct{ state string }

func (e *fakePGError) Error() string    { return "fake pg error" }
func (e *fakePGError) SQLState() string { return e.state }

func TestIsUniqueViolation(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unique violation (23505)", &fakePGError{state: "23505"}, true},
		{"foreign key violation (23503)", &fakePGError{state: "23503"}, false},
		{"generic error", errors.New("something"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isUniqueViolation(tt.err); got != tt.want {
				t.Errorf("isUniqueViolation() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTargetWorkspaceUpdateFlag(t *testing.T) {
	tests := []struct {
		name string
		s    *string
		want any // nil, false, or true
	}{
		{"nil → keep (nil)", nil, nil},
		{"empty → clear (false)", strPtr(""), false},
		{"uuid → set (true)", strPtr("550e8400-e29b-41d4-a716-446655440000"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := targetWorkspaceUpdateFlag(tt.s)
			if tt.want == nil {
				if got != nil {
					t.Errorf("expected nil, got %v", got)
				}
			} else {
				b, ok := got.(bool)
				if !ok {
					t.Fatalf("expected bool, got %T", got)
				}
				if b != tt.want.(bool) {
					t.Errorf("expected %v, got %v", tt.want, got)
				}
			}
		})
	}
}

func TestTargetWorkspaceUpdateValue(t *testing.T) {
	uuidVal := "550e8400-e29b-41d4-a716-446655440000"
	tests := []struct {
		name string
		s    *string
		want any
	}{
		{"nil → nil", nil, nil},
		{"empty → nil", strPtr(""), nil},
		{"uuid → uuid", &uuidVal, uuidVal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := targetWorkspaceUpdateValue(tt.s)
			if tt.want == nil {
				if got != nil {
					t.Errorf("expected nil, got %v", got)
				}
			} else {
				if got != tt.want {
					t.Errorf("expected %v, got %v", tt.want, got)
				}
			}
		})
	}
}

func strPtr(s string) *string { return &s }
