// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err, "failed to read fixture %s", name)
	return data
}

func TestParseSessionWire_RealShape1_18_10(t *testing.T) {
	body := loadFixture(t, "session_get_1_18_10.json")
	s, err := ParseSessionWire(body, "ws-1")
	require.NoError(t, err, "ParseSessionWire must not fail on the 1.18.10 wire shape")
	require.NotNil(t, s)
	assert.NotEmpty(t, s.ID)
	assert.Equal(t, "Test Session", s.Title)
}

func TestParseSessionWire_RealShape1_18_10_TokensExtracted(t *testing.T) {
	body := loadFixture(t, "session_get_1_18_10.json")
	s, err := ParseSessionWire(body, "ws-1")
	require.NoError(t, err)
	require.NotNil(t, s)
	require.NotNil(t, s.Cost, "session.Cost must be populated from the tokens object")
	assert.Equal(t, int64(4868893), s.Cost.InputTokens)
	assert.Equal(t, int64(330410), s.Cost.OutputTokens)
	assert.Equal(t, int64(35310), s.Cost.ReasoningTokens)
	assert.Equal(t, int64(761649152), s.Cost.CacheReadTokens)
}

func TestParseSessionWire_SummaryString_Legacy1_15_12(t *testing.T) {
	body := []byte(`{"id":"ses_old","title":"Old","status":{"type":"idle"},"summary":"legacy string"}`)
	s, err := ParseSessionWire(body, "ws-1")
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Equal(t, "ses_old", s.ID)
}

func TestParseSessionListWire_RealShape1_18_10(t *testing.T) {
	body := loadFixture(t, "session_list_1_18_10.json")
	sessions, err := ParseSessionListWire(body, "ws-1")
	require.NoError(t, err)
	require.True(t, len(sessions) >= 2)
}

func TestParseSessionWire_SummaryAbsent(t *testing.T) {
	body := []byte(`{"id":"ses_nosummary","status":{"type":"idle"}}`)
	s, err := ParseSessionWire(body, "ws-1")
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Equal(t, "ses_nosummary", s.ID)
}
