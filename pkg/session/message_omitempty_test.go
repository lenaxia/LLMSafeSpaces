// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMessage_ZeroCreatedAt_OmittedFromJSON(t *testing.T) {
	m := Message{ID: "msg_1", Type: MessageUser}
	out, err := json.Marshal(m)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(out, &raw))
	_, hasCreatedAt := raw["createdAt"]
	assert.False(t, hasCreatedAt,
		"nil CreatedAt must be omitted from JSON wire (fixes Dec 31 bug)")
}

func TestMessage_NonZeroCreatedAt_IncludedInJSON(t *testing.T) {
	now := time.Now().UTC()
	m := Message{ID: "msg_1", Type: MessageUser, CreatedAt: &now}
	out, err := json.Marshal(m)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(out, &raw))
	val, hasCreatedAt := raw["createdAt"]
	assert.True(t, hasCreatedAt, "non-nil CreatedAt must appear on wire")
	assert.NotNil(t, val)
}

func TestMessage_CreatedAt_RoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	m := Message{ID: "msg_1", Type: MessageUser, CreatedAt: &now}
	out, err := json.Marshal(m)
	require.NoError(t, err)
	var decoded Message
	require.NoError(t, json.Unmarshal(out, &decoded))
	require.NotNil(t, decoded.CreatedAt)
	assert.True(t, decoded.CreatedAt.Equal(now),
		"createdAt must survive JSON round-trip")
}
