// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionindex

// #1880 direction 3 — the listing serve-through eligibility rule, the
// pure twin of DefaultReapGuard's spawn-window grace: which rows are
// KNOWN-untrustworthy on count (drift class) and inside the overlay's
// scope. Lives beside the guard so the two age gates (reap grace,
// serve grace) share one window constant and one drift vocabulary.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

func TestCountServeEligible(t *testing.T) {
	aged := func() *time.Time { v := time.Now().Add(-2 * time.Hour); return &v }
	fresh := func() *time.Time { v := time.Now().Add(-5 * time.Minute); return &v }
	lastMsg := func() *time.Time { v := time.Now().Add(-time.Hour); return &v }

	cases := []struct {
		name string
		row  func(r types.SessionListItem) types.SessionListItem
		want bool
	}{
		{"never-counted aged row — the drift class", func(r types.SessionListItem) types.SessionListItem {
			r.MessageCount = 0
			r.CreatedAt = aged()
			return r
		}, true},
		{"never-counted row without creation info — hand-built/legacy, eligible", func(r types.SessionListItem) types.SessionListItem {
			r.MessageCount = 0
			r.CreatedAt = nil
			return r
		}, true},
		{"fresh spawn inside the grace — the #1881 belt owns it", func(r types.SessionListItem) types.SessionListItem {
			r.MessageCount = 0
			r.CreatedAt = fresh()
			return r
		}, false},
		{"persisted-once drift row (count>0, last_message_at still nil) — stays hot to keep converging", func(r types.SessionListItem) types.SessionListItem {
			r.MessageCount = 389
			r.LastMessageAt = nil
			r.CreatedAt = aged()
			return r
		}, true},
		{"converged proxy-traversed row — outside scope", func(r types.SessionListItem) types.SessionListItem {
			r.MessageCount = 5
			r.LastMessageAt = lastMsg()
			r.CreatedAt = aged()
			return r
		}, false},
		{"archived row — never spends walk budget", func(r types.SessionListItem) types.SessionListItem {
			r.MessageCount = 0
			r.CreatedAt = aged()
			r.Archived = true
			return r
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, CountServeEligible(tc.row(types.SessionListItem{})))
		})
	}
}
