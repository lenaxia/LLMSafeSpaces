// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// US-69.11: the onRawEvent → adapter → ContextUsageFromEvent e2e tests
// were deleted with the tracker's raw-event pipeline — context usage now
// arrives as a structured ABI MESSAGE_END cost and flows through
// usageBridge.ContextUsed (pinned in opencode_upgrade_test.go and the
// usagestream consumer tests). What remains here is the workspace
// status wire contract the frontend reads.

// TestContextUsed_JSONWireShape: the context-usage fields must survive
// a JSON round-trip on the workspace status result — the per-session
// contextUsed the UI renders its occupancy bars from.
func TestContextUsed_JSONWireShape(t *testing.T) {
	statusResult := &types.WorkspaceStatusResult{
		Phase:        "Active",
		ContextUsed:  45000,
		ContextTotal: 200000,
		Sessions: []types.SessionStatusItem{
			{ID: "ses_rt", Status: "idle", ContextUsed: 45000},
		},
	}

	raw, err := json.Marshal(statusResult)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"contextUsed":45000`, "contextUsed must survive JSON round-trip")
	assert.Contains(t, string(raw), `"contextTotal":200000`, "contextTotal must survive JSON round-trip")

	var decoded types.WorkspaceStatusResult
	require.NoError(t, json.Unmarshal(raw, &decoded))
	assert.Equal(t, int64(45000), decoded.ContextUsed)
	assert.Equal(t, int64(200000), decoded.ContextTotal)
	require.Len(t, decoded.Sessions, 1)
	assert.Equal(t, int64(45000), decoded.Sessions[0].ContextUsed)
}
