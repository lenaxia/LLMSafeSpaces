// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// shortIDSeamIndex is a SessionIndexService fake for the registration
// seam's wiring pins: Register RECORDS the call (the wiring is the
// assertion) and returns a stable minted-looking id per session.
type shortIDSeamIndex struct {
	mu         sync.Mutex
	registered []string
	minted     map[string]string
	counts     map[string]int
}

func newShortIDSeamIndex() *shortIDSeamIndex {
	return &shortIDSeamIndex{minted: map[string]string{}, counts: map[string]int{}}
}

func (s *shortIDSeamIndex) Register(_ context.Context, _, sessionID, _ string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registered = append(s.registered, sessionID)
	if id, ok := s.minted[sessionID]; ok {
		return id
	}
	id := "oc-" + ("00000000" + sessionID)[len(sessionID):]
	if len(id) > 11 {
		id = id[:11]
	}
	s.minted[sessionID] = id
	return id
}

func (s *shortIDSeamIndex) registerCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.registered...)
}

func (s *shortIDSeamIndex) RecordMessage(_, _, _ string, _ time.Time) {}
func (s *shortIDSeamIndex) ListByWorkspace(_ context.Context, _ string) ([]types.SessionListItem, error) {
	return nil, nil
}
func (s *shortIDSeamIndex) DeleteByWorkspace(_ context.Context, _ string) error { return nil }
func (s *shortIDSeamIndex) DeleteSession(_ context.Context, _, _ string) error  { return nil }
func (s *shortIDSeamIndex) UpsertTitle(_ context.Context, _, _, _ string) error { return nil }
func (s *shortIDSeamIndex) RebuildMessageCount(_ context.Context, _, sessionID string, count int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts[sessionID] = count
	return nil
}
func (s *shortIDSeamIndex) UpsertParent(_ context.Context, _, _, _ string) error { return nil }
func (s *shortIDSeamIndex) UpsertContextUsed(_ context.Context, _, _ string, _ int64) error {
	return nil
}
func (s *shortIDSeamIndex) UpdateLastSeen(_ context.Context, _, _ string) error      { return nil }
func (s *shortIDSeamIndex) SetArchived(_ context.Context, _, _ string, _ bool) error { return nil }
func (s *shortIDSeamIndex) IsArchived(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}
func (s *shortIDSeamIndex) Start() error { return nil }
func (s *shortIDSeamIndex) Stop() error  { return nil }

// TestBackfillShortIDs_MintsNullRowsWithinBudget pins the convergence
// sweep's short-id half: NULL rows are registered, already-registered
// rows are untouched, and the per-pass budget bounds the mint count
// (design 0066 §4.2; review r1 missing test 2).
func TestBackfillShortIDs_MintsNullRowsWithinBudget(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	si := newShortIDSeamIndex()
	h.sessionIndex = si

	rows := make([]types.SessionListItem, 0, shortIDBackfillBudget+5)
	for i := 0; i < shortIDBackfillBudget+5; i++ {
		rows = append(rows, types.SessionListItem{ID: "ses_null_" + string(rune('a'+i))})
	}
	// Two rows already carry a short id — must not be re-registered.
	rows[0].ShortID = "oc-aaaaaaaa"
	rows[1].ShortID = "oc-bbbbbbbb"

	h.backfillShortIDs(context.Background(), "ws-1", rows)

	calls := si.registerCalls()
	require.Len(t, calls, shortIDBackfillBudget, "the per-pass budget must bound the mint count")
	assert.NotContains(t, calls, rows[0].ID, "already-short-id'd rows are skipped")
}

// TestRebuildMessageCounts_RegistersFirst is the §6 per-path wiring
// pin for the MESSAGE-COUNT upsert shape: a drifted count routes
// through the registration seam before the write (review r1 finding 1
// / missing test 3).
func TestRebuildMessageCounts_RegistersFirst(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	si := newShortIDSeamIndex()
	h.sessionIndex = si
	h.adapter = &mockAdapter{
		countMessagesFn: func(_ context.Context, _, _, _ string) (int, error) {
			return 7, nil // drifted from the indexed 1
		},
	}

	rows := []types.SessionListItem{{ID: "ses_cnt_1", InternalID: "ses_cnt_1", MessageCount: 1}}
	h.rebuildMessageCounts(context.Background(), "ws-1", rows, map[string]bool{"ses_cnt_1": true})

	calls := si.registerCalls()
	require.NotEmpty(t, calls, "message-count rebuilds must route through the registration seam")
	assert.Equal(t, "ses_cnt_1", calls[0])
	assert.Equal(t, 7, si.counts["ses_cnt_1"], "the count rebuild itself still ran")
}
