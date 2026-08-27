// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sse

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTokenSeenStore is an in-memory TokenSeenStore for tracker tests.
type fakeTokenSeenStore struct {
	mu       sync.Mutex
	state    map[string][2]float64 // key -> {output, cost}
	getErr   error
	setErr   error
	getCalls int
}

func newFakeTokenSeenStore() *fakeTokenSeenStore {
	return &fakeTokenSeenStore{state: map[string][2]float64{}}
}

func (s *fakeTokenSeenStore) GetSessionUsage(_ context.Context, workspaceID, sessionID string) (int64, float64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getCalls++
	if s.getErr != nil {
		return 0, 0, false, s.getErr
	}
	v, ok := s.state[workspaceID+":"+sessionID]
	return int64(v[0]), v[1], ok, nil
}

func (s *fakeTokenSeenStore) SetSessionUsage(_ context.Context, workspaceID, sessionID string, output int64, cost float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setErr != nil {
		return s.setErr
	}
	s.state[workspaceID+":"+sessionID] = [2]float64{float64(output), cost}
	return nil
}

// inferenceCapture records every onInference firing.
type inferenceCapture struct {
	mu     sync.Mutex
	firing []inferenceRecord
}

type inferenceRecord struct {
	WorkspaceID  string
	ModelID      string
	ProviderID   string
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
}

func (c *inferenceCapture) attach(t *Tracker) {
	t.SetOnInference(func(ws, model, provider string, in, out int64, cost float64) {
		c.mu.Lock()
		c.firing = append(c.firing, inferenceRecord{ws, model, provider, in, out, cost})
		c.mu.Unlock()
	})
}

func (c *inferenceCapture) all() []inferenceRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]inferenceRecord, len(c.firing))
	copy(cp, c.firing)
	return cp
}

// TestSSETracker_Restart_DoesNotRebillInput is the #759 core scenario:
// two tracker instances (simulating an API pod restart mid-session)
// sharing the persistent store. The second tracker must NOT re-bill the
// cumulative input that the first already billed.
func TestSSETracker_Restart_DoesNotRebillInput(t *testing.T) {
	store := newFakeTokenSeenStore()

	// Tracker instance #1 (pre-restart).
	cap1 := &inferenceCapture{}
	tr1 := newTestSSETracker(func(_, _ string) {})
	tr1.SetTokenSeenStore(store)
	cap1.attach(tr1)
	tr1.processEvent("ws-1", makeSessionUpdatedEvent("ses_restart", map[string]interface{}{
		"id":     "ses_restart",
		"cost":   0.10,
		"tokens": map[string]interface{}{"input": 100000, "output": 10},
		"model":  map[string]interface{}{"id": "glm-5.3", "providerID": "thekaocloud"},
	}))

	fired := cap1.all()
	require.Len(t, fired, 1, "first event must bill")
	assert.Equal(t, int64(100000), fired[0].InputTokens, "first event bills full input")
	assert.Equal(t, int64(10), fired[0].OutputTokens)

	// Tracker instance #2 (post-restart): fresh in-memory maps, same store.
	cap2 := &inferenceCapture{}
	tr2 := newTestSSETracker(func(_, _ string) {})
	tr2.SetTokenSeenStore(store)
	cap2.attach(tr2)
	tr2.processEvent("ws-1", makeSessionUpdatedEvent("ses_restart", map[string]interface{}{
		"id":     "ses_restart",
		"cost":   0.30,
		"tokens": map[string]interface{}{"input": 100000, "output": 30},
		"model":  map[string]interface{}{"id": "glm-5.3", "providerID": "thekaocloud"},
	}))

	fired2 := cap2.all()
	require.Len(t, fired2, 1, "post-restart event must still bill (the delta)")
	assert.Equal(t, int64(0), fired2[0].InputTokens,
		"cumulative input must NOT be re-billed after a tracker restart (#759)")
	assert.Equal(t, int64(20), fired2[0].OutputTokens,
		"output delta is cumulative-diff: 30-10")
	assert.InDelta(t, 0.20, fired2[0].CostUSD, 1e-9)
}

// TestSSETracker_FirstEvent_BillsInput: with a store configured but no
// prior state, the first event of a session bills its input exactly once.
func TestSSETracker_FirstEvent_BillsInput(t *testing.T) {
	store := newFakeTokenSeenStore()
	cap := &inferenceCapture{}
	tr := newTestSSETracker(func(_, _ string) {})
	tr.SetTokenSeenStore(store)
	cap.attach(tr)

	tr.processEvent("ws-1", makeSessionUpdatedEvent("ses_first", map[string]interface{}{
		"id":     "ses_first",
		"cost":   0.05,
		"tokens": map[string]interface{}{"input": 5000, "output": 7},
		"model":  map[string]interface{}{"id": "glm-5.3", "providerID": "thekaocloud"},
	}))

	fired := cap.all()
	require.Len(t, fired, 1)
	assert.Equal(t, int64(5000), fired[0].InputTokens)
	assert.Equal(t, int64(7), fired[0].OutputTokens)
}

// TestSSETracker_StoreGetFails_FallsBackToInMemory: a store read error
// degrades to today's in-memory behavior (bill input on first sight) —
// never a panic, never a silent event drop.
func TestSSETracker_StoreGetFails_FallsBackToInMemory(t *testing.T) {
	store := newFakeTokenSeenStore()
	store.getErr = errors.New("redis unavailable")
	cap := &inferenceCapture{}
	tr := newTestSSETracker(func(_, _ string) {})
	tr.SetTokenSeenStore(store)
	cap.attach(tr)

	tr.processEvent("ws-1", makeSessionUpdatedEvent("ses_getfail", map[string]interface{}{
		"id":     "ses_getfail",
		"cost":   0.01,
		"tokens": map[string]interface{}{"input": 800, "output": 3},
		"model":  map[string]interface{}{"id": "glm-5.3", "providerID": "thekaocloud"},
	}))

	fired := cap.all()
	require.Len(t, fired, 1, "billing must continue when the store errors")
	assert.Equal(t, int64(800), fired[0].InputTokens)
}

// TestSSETracker_StoreSetFails_BillingStillFires: a store write error is
// tolerated — the in-memory state still advances and billing fires.
func TestSSETracker_StoreSetFails_BillingStillFires(t *testing.T) {
	store := newFakeTokenSeenStore()
	store.setErr = errors.New("redis read-only")
	cap := &inferenceCapture{}
	tr := newTestSSETracker(func(_, _ string) {})
	tr.SetTokenSeenStore(store)
	cap.attach(tr)

	tr.processEvent("ws-1", makeSessionUpdatedEvent("ses_setfail", map[string]interface{}{
		"id":     "ses_setfail",
		"cost":   0.02,
		"tokens": map[string]interface{}{"input": 900, "output": 4},
		"model":  map[string]interface{}{"id": "glm-5.3", "providerID": "thekaocloud"},
	}))
	tr.processEvent("ws-1", makeSessionUpdatedEvent("ses_setfail", map[string]interface{}{
		"id":     "ses_setfail",
		"cost":   0.03,
		"tokens": map[string]interface{}{"input": 900, "output": 6},
		"model":  map[string]interface{}{"id": "glm-5.3", "providerID": "thekaocloud"},
	}))

	fired := cap.all()
	require.Len(t, fired, 2)
	assert.Equal(t, int64(900), fired[0].InputTokens)
	assert.Equal(t, int64(0), fired[1].InputTokens,
		"in-memory dedup must still work when the store write fails")
	assert.Equal(t, int64(2), fired[1].OutputTokens)
}

// TestSSETracker_StopWatching_DoesNotRebillFromStore: StopWatching wipes
// the in-memory maps (bounded memory) but the persistent store carries
// the dedup truth — the next cumulative event for the same session must
// not re-bill input. This also covers the suspend/resume variant of
// #759: session history survives, cumulative tokens keep arriving.
func TestSSETracker_StopWatching_DoesNotRebillFromStore(t *testing.T) {
	store := newFakeTokenSeenStore()
	cap := &inferenceCapture{}
	tr := newTestSSETracker(func(_, _ string) {})
	tr.SetTokenSeenStore(store)
	cap.attach(tr)

	emit := func(output int) {
		tr.processEvent("ws-1", makeSessionUpdatedEvent("ses_resume", map[string]interface{}{
			"id":     "ses_resume",
			"cost":   float64(output) / 100.0,
			"tokens": map[string]interface{}{"input": 40000, "output": output},
			"model":  map[string]interface{}{"id": "glm-5.3", "providerID": "thekaocloud"},
		}))
	}

	emit(10)
	tr.StopWatching("ws-1") // in-memory wiped, store retained
	emit(25)

	fired := cap.all()
	require.Len(t, fired, 2)
	assert.Equal(t, int64(40000), fired[0].InputTokens, "first event bills input")
	assert.Equal(t, int64(0), fired[1].InputTokens,
		"post-rewatch event must load prior usage from the store, not re-bill input")
	assert.Equal(t, int64(15), fired[1].OutputTokens)
}

// TestSSETracker_NoStore_LegacyBehavior: without a store configured the
// tracker behaves exactly as before (in-memory only) — deployments
// without Redis must see no change.
func TestSSETracker_NoStore_LegacyBehavior(t *testing.T) {
	cap := &inferenceCapture{}
	tr := newTestSSETracker(func(_, _ string) {})
	cap.attach(tr)

	tr.processEvent("ws-1", makeSessionUpdatedEvent("ses_legacy", map[string]interface{}{
		"id":     "ses_legacy",
		"cost":   0.01,
		"tokens": map[string]interface{}{"input": 1000, "output": 5},
		"model":  map[string]interface{}{"id": "glm-5.3", "providerID": "thekaocloud"},
	}))

	fired := cap.all()
	require.Len(t, fired, 1)
	assert.Equal(t, int64(1000), fired[0].InputTokens)
}

// TestSSETracker_ConcurrentUsage_SameSession: parallel cumulative events
// for one session fire inference exactly as many times as there are
// strict increases, with no race and no double input billing (store +
// in-memory cache must be race-free).
func TestSSETracker_ConcurrentUsage_SameSession(t *testing.T) {
	store := newFakeTokenSeenStore()
	cap := &inferenceCapture{}
	tr := newTestSSETracker(func(_, _ string) {})
	tr.SetTokenSeenStore(store)
	cap.attach(tr)

	const n = 32
	var wg sync.WaitGroup
	for i := 1; i <= n; i++ {
		wg.Add(1)
		go func(out int) {
			defer wg.Done()
			tr.processEvent("ws-1", makeSessionUpdatedEvent("ses_race", map[string]interface{}{
				"id":     "ses_race",
				"cost":   float64(out) / 1000.0,
				"tokens": map[string]interface{}{"input": 7000, "output": out},
				"model":  map[string]interface{}{"id": "glm-5.3", "providerID": "thekaocloud"},
			}))
		}(i)
	}
	wg.Wait()

	fired := cap.all()
	inputBilled := int64(0)
	for _, f := range fired {
		inputBilled += f.InputTokens
	}
	assert.Equal(t, int64(7000), inputBilled,
		"input must be billed exactly once no matter the interleaving (fired %d times)", len(fired))
	assert.NotEmpty(t, fired)
}
