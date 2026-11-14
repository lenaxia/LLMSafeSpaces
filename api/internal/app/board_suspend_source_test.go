// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"testing"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// TestBoardSuspendSource_PhaseMapping pins the adapter's honesty map:
// Suspending/Suspended ⇒ suspended; every other phase — and, critically,
// an ABSENT workspace (watcher cache empty/lagging, watcher not yet
// started) ⇒ NOT suspended (today's lease semantics; the freeze never
// fires on the strength of a blind window).
func TestBoardSuspendSource_PhaseMapping(t *testing.T) {
	cases := []struct {
		name   string
		phases map[string]string
		want   map[string]bool
	}{
		{
			name:   "suspended and suspending freeze",
			phases: map[string]string{"ws-a": string(v1.WorkspacePhaseSuspended), "ws-b": string(v1.WorkspacePhaseSuspending)},
			want:   map[string]bool{"ws-a": true, "ws-b": true},
		},
		{
			name: "active, resuming, terminating never freeze",
			phases: map[string]string{
				"ws-a": string(v1.WorkspacePhaseActive),
				"ws-r": string(v1.WorkspacePhaseResuming),
				"ws-t": string(v1.WorkspacePhaseTerminating),
				"ws-f": string(v1.WorkspacePhaseFailed),
			},
			want: nil,
		},
		{name: "empty cache (watcher down) is honest not-suspended", phases: nil, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := newBoardSuspendSource(func() map[string]string { return tc.phases })
			got := src.SuspendedWorkspaces(context.Background())
			if tc.want == nil {
				if got != nil {
					t.Fatalf("expected nil set, got %v", got)
				}
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("set mismatch: want %v got %v", tc.want, got)
			}
			for id := range tc.want {
				if !got[id] {
					t.Fatalf("expected %s suspended, got %v", id, got)
				}
			}
		})
	}
}

// TestBoardSuspendSource_NilPhaseFn pins the nil-safety contract: a nil
// phase function (the seam's zero value — never wired) answers nothing
// suspended, never panics.
func TestBoardSuspendSource_NilPhaseFn(t *testing.T) {
	src := newBoardSuspendSource(nil)
	if got := src.SuspendedWorkspaces(context.Background()); got != nil {
		t.Fatalf("nil phase fn must answer nil, got %v", got)
	}
}
