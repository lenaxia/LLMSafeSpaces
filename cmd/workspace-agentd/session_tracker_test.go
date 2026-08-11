// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSessionStatusTracker_TreatsAllNonIdleAsBusy verifies that the
// agentd session tracker maps retry/error/compacting to "busy" (#753).
// The old code only stored "busy" and "idle", causing the secrets
// rotation restart decision to fire mid-turn for sessions in retry/
// error/compacting states.
func TestSessionStatusTracker_TreatsAllNonIdleAsBusy(t *testing.T) {
	tracker := newSessionStatusTracker()

	cases := []struct {
		status string
		want   string
	}{
		{"busy", "busy"},
		{"retry", "busy"},
		{"error", "busy"},
		{"compacting", "busy"},
		{"idle", "idle"},
	}

	for _, tc := range cases {
		props := json.RawMessage(`{"sessionID":"ses_1","status":{"type":"` + tc.status + `"}}`)
		tracker.handleSessionStatus(props)

		got, ok := tracker.statuses["ses_1"]
		assert.True(t, ok && got == tc.want,
			"status %q should map to %q, got %q (ok=%v)", tc.status, tc.want, got, ok)
		delete(tracker.statuses, "ses_1")
	}
}
