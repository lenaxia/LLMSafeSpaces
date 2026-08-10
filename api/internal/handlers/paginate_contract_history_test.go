// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

func TestPaginateContractHistory_FirstPage(t *testing.T) {
	msgs := makeTestMessages(10)
	page, nextCursor := paginateContractHistory(msgs, 5, "")

	assert.Len(t, page, 5)
	assert.Equal(t, "msg_5", page[0].ID, "first page = last 5 messages, oldest-first")
	assert.Equal(t, "msg_9", page[4].ID)
	assert.Equal(t, "msg_5", nextCursor, "cursor = oldest ID in page")
}

func TestPaginateContractHistory_SecondPage(t *testing.T) {
	msgs := makeTestMessages(10)
	page, nextCursor := paginateContractHistory(msgs, 5, "msg_5")

	assert.Len(t, page, 5)
	assert.Equal(t, "msg_0", page[0].ID)
	assert.Equal(t, "msg_4", page[4].ID)
	assert.Empty(t, nextCursor, "no older messages = no cursor")
}

func TestPaginateContractHistory_UnknownCursor_EmptyPage(t *testing.T) {
	msgs := makeTestMessages(10)
	page, nextCursor := paginateContractHistory(msgs, 5, "nonexistent")

	assert.Empty(t, page)
	assert.Empty(t, nextCursor)
}

func TestPaginateContractHistory_FewerThanLimit(t *testing.T) {
	msgs := makeTestMessages(3)
	page, nextCursor := paginateContractHistory(msgs, 50, "")

	assert.Len(t, page, 3)
	assert.Empty(t, nextCursor, "fewer messages than limit = no cursor")
}

func TestPaginateContractHistory_EmptyInput(t *testing.T) {
	page, nextCursor := paginateContractHistory(nil, 50, "")

	assert.Empty(t, page)
	assert.Empty(t, nextCursor)
}

func TestPaginateContractHistory_PartialLastPage(t *testing.T) {
	msgs := makeTestMessages(7)
	page, nextCursor := paginateContractHistory(msgs, 5, "msg_5")

	assert.Len(t, page, 5, "7 total, before=msg_5 -> messages 0-4")
	assert.Empty(t, nextCursor, "start=0 = no older messages")
}

func makeTestMessages(n int) []session.Message {
	out := make([]session.Message, n)
	for i := 0; i < n; i++ {
		out[i] = session.Message{
			ID:        "msg_" + string(rune('0'+i)),
			Type:      session.MessageUser,
			CreatedAt: time.Now(),
		}
	}
	return out
}

// Ensure the test compiles correctly with the session package.
var _ = session.MessageUser
