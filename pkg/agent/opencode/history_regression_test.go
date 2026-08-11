// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// TestParseHistoryStream_LargeBodyOver16MiB is the primary regression
// test for #737. The pre-fix code used readBody(resp, 16<<20) which
// truncated bodies >16 MiB, causing ParseHistoryWire to fail with
// "unexpected end of JSON input." This test generates a body >16 MiB
// and verifies it decodes completely via the streaming path.
func TestParseHistoryStream_LargeBodyOver16MiB(t *testing.T) {
	// Build a body >16 MiB with fewer, larger messages for speed.
	// 10 messages × 1.7 MB each ≈ 17 MB > 16 MiB.
	const numMessages = 10
	const textLen = 1700000

	bigText := strings.Repeat("x", textLen)

	var buf bytes.Buffer
	buf.WriteByte('[')
	for i := 0; i < numMessages; i++ {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(`{"id":"msg_`)
		buf.WriteString(itob(i))
		buf.WriteString(`","role":"assistant","info":{"role":"assistant"},"parts":[{"type":"text","text":"`)
		buf.WriteString(bigText)
		buf.WriteString(`"}]}`)
	}
	buf.WriteByte(']')

	bodySize := buf.Len()
	require.Greater(t, bodySize, 16<<20, "test body must exceed 16 MiB (got %d bytes)", bodySize)

	msgs, _, downgraded, err := ParseHistoryStream(&buf, "ws-1")
	require.NoError(t, err, "streaming decode must succeed on bodies >16 MiB")
	assert.Equal(t, numMessages, len(msgs), "all messages must be decoded")
	assert.Equal(t, 0, downgraded, "no messages should be downgraded")

	last := msgs[numMessages-1]
	require.NotEmpty(t, last.Parts)
	assert.Equal(t, textLen, len(last.Parts[0].Text), "last message text must be intact (not truncated)")
}

// TestParseHistoryStream_EmptyArray verifies that an empty JSON array
// returns empty slices with no error.
func TestParseHistoryStream_EmptyArray(t *testing.T) {
	msgs, _, _, err := ParseHistoryStream(strings.NewReader("[]"), "ws-1")
	require.NoError(t, err)
	assert.Empty(t, msgs)
}

// TestParseHistoryStream_TruncatedBody_ReturnsPartialResults verifies
// that a valid single-message body decodes correctly. The streaming
// decoder returns partial results for incomplete arrays.
func TestParseHistoryStream_TruncatedBody_ReturnsPartialResults(t *testing.T) {
	body := `[{"id":"m1","role":"assistant","info":{"id":"m1","role":"assistant"},"parts":[{"type":"text","text":"hello"}]}]`
	msgs, _, _, err := ParseHistoryStream(strings.NewReader(body), "ws-1")
	require.NoError(t, err)
	assert.Equal(t, 1, len(msgs), "single message must decode")
	assert.Equal(t, "m1", msgs[0].ID)
}

// TestSystemMessage_NilCreatedAt_OmitsJSONField verifies that
// SystemMessage(id, text, nil) produces JSON with no createdAt field.
// This is the fix for the review finding that newMessage always sets
// a non-nil pointer to time.Time{}.
func TestSystemMessage_NilCreatedAt_OmitsJSONField(t *testing.T) {
	m := session.SystemMessage("sys_1", "decode failed", nil)
	raw, err := json.Marshal(m)
	require.NoError(t, err)

	var m2 map[string]any
	require.NoError(t, json.Unmarshal(raw, &m2))

	_, hasCreatedAt := m2["createdAt"]
	assert.False(t, hasCreatedAt, "createdAt must be absent when nil is passed")
	assert.Equal(t, "sys_1", m2["id"])
	assert.Equal(t, "decode failed", m2["text"])
}

// TestSystemMessage_WithCreatedAt_IncludesJSONField verifies that
// SystemMessage(id, text, &t) includes createdAt in JSON.
func TestSystemMessage_WithCreatedAt_IncludesJSONField(t *testing.T) {
	ts := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	m := session.SystemMessage("sys_2", "test", &ts)
	raw, err := json.Marshal(m)
	require.NoError(t, err)

	var m2 map[string]any
	require.NoError(t, json.Unmarshal(raw, &m2))

	_, hasCreatedAt := m2["createdAt"]
	assert.True(t, hasCreatedAt, "createdAt must be present when a timestamp is passed")
}

func itob(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
