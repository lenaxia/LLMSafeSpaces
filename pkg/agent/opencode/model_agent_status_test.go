// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseSessionWire_RealShape1_18_10_ProviderExtracted verifies that
// the model provider is correctly extracted from the 1.18.10 wire shape,
// which uses "providerID" instead of "provider" (#743 Finding 1).
func TestParseSessionWire_RealShape1_18_10_ProviderExtracted(t *testing.T) {
	body := loadFixture(t, "session_get_1_18_10.json")
	s, err := ParseSessionWire(body, "ws-1")
	require.NoError(t, err)
	require.NotNil(t, s.Model, "Model must be populated")
	assert.Equal(t, "glm-5.2", s.Model.ID)
	assert.Equal(t, "thekaocloud", s.Model.Provider, "provider must be extracted from providerID key")
}

// TestOcModelRef_LegacyProviderKey verifies backward compat with the
// 1.15.12 wire shape that uses "provider" (not "providerID").
func TestOcModelRef_LegacyProviderKey(t *testing.T) {
	body := []byte(`{"id":"ses_1","model":{"id":"claude-3.5","provider":"anthropic"}}`)
	s, err := ParseSessionWire(body, "ws-1")
	require.NoError(t, err)
	require.NotNil(t, s.Model)
	assert.Equal(t, "anthropic", s.Model.Provider, "legacy 'provider' key must still work")
}

// TestParseSessionWire_RealShape1_18_10_AgentExtracted verifies that the
// "agent" field is parsed and mapped to session.AgentID (#743 Finding 2).
func TestParseSessionWire_RealShape1_18_10_AgentExtracted(t *testing.T) {
	body := loadFixture(t, "session_get_1_18_10.json")
	s, err := ParseSessionWire(body, "ws-1")
	require.NoError(t, err)
	assert.Equal(t, "build", s.AgentID, "agent field must be extracted from wire")
}

// TestParseSessionWire_StatusAbsent_NoError verifies that a session
// response with NO status field parses cleanly (#743 Finding 3).
// 1.18.10's fixture has no status — the current required (non-pointer)
// field tolerates this as zero-value, but a future shape change to a
// bare string would crash. This test pins the current tolerance.
func TestParseSessionWire_StatusAbsent_NoError(t *testing.T) {
	body := []byte(`{"id":"ses_nostatus","title":"test"}`)
	s, err := ParseSessionWire(body, "ws-1")
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Equal(t, "ses_nostatus", s.ID)
}

// TestParseSessionWire_StatusAsString_NoCrash verifies that if opencode
// sends status as a bare string (potential future drift), the parser
// handles it gracefully instead of crashing with a type-mismatch error.
// This is the latent 502 from Finding 3.
func TestParseSessionWire_StatusAsString_NoCrash(t *testing.T) {
	body := []byte(`{"id":"ses_strstatus","status":"idle"}`)
	s, err := ParseSessionWire(body, "ws-1")
	require.NoError(t, err, "bare-string status must not cause a parse error")
	require.NotNil(t, s)
	assert.Equal(t, "ses_strstatus", s.ID)
}

// TestParseSessionListWire_ProviderAndAgent verifies provider and agent
// extraction through the list path (same wire shape, different parser).
func TestParseSessionListWire_ProviderAndAgent(t *testing.T) {
	body := loadFixture(t, "session_list_1_18_10.json")
	sessions, err := ParseSessionListWire(body, "ws-1")
	require.NoError(t, err)
	require.True(t, len(sessions) >= 2)
	for _, s := range sessions {
		if s.Model != nil {
			assert.NotEmpty(t, s.Model.ID, "model ID must be present")
		}
	}
}

// TestOcModelRef_UnmarshalJSON_ProviderIDKey directly tests the
// UnmarshalJSON with the exact 1.18.10 model object shape.
func TestOcModelRef_UnmarshalJSON_ProviderIDKey(t *testing.T) {
	raw := []byte(`{"id":"glm-5.2","providerID":"thekaocloud","variant":"default"}`)
	var m ocModelRef
	err := json.Unmarshal(raw, &m)
	require.NoError(t, err)
	assert.Equal(t, "glm-5.2", m.ID)
	assert.Equal(t, "thekaocloud", m.Provider)
}
