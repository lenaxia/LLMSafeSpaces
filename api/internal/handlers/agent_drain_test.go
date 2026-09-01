// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// checkerStep is one scripted GetSessionStatuses response for the
// poll-based WaitUntilIdle (US-69.11).
type checkerStep struct {
	statuses map[string]string
	err      error
}

func idleStep(ids ...string) checkerStep {
	m := make(map[string]string, len(ids))
	for _, id := range ids {
		m[id] = "idle"
	}
	return checkerStep{statuses: m}
}

func busyStep(ids ...string) checkerStep {
	m := make(map[string]string, len(ids))
	for _, id := range ids {
		m[id] = "busy"
	}
	return checkerStep{statuses: m}
}

// fakeStatusChecker scripts GetSessionStatuses responses: each call
// consumes one scripted step; when the script is exhausted the last
// step repeats.
type fakeStatusChecker struct {
	mu    sync.Mutex
	steps []checkerStep
	calls int
}

func (f *fakeStatusChecker) GetSessionStatuses(context.Context) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.steps) == 0 {
		return map[string]string{}, nil
	}
	step := f.steps[len(f.steps)-1]
	if f.calls <= len(f.steps) {
		step = f.steps[f.calls-1]
	}
	if step.err != nil {
		return nil, step.err
	}
	cp := make(map[string]string, len(step.statuses))
	for k, v := range step.statuses {
		cp[k] = v
	}
	return cp, nil
}

func (f *fakeStatusChecker) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestWaitUntilIdle_AlreadyIdle_ReturnsImmediately(t *testing.T) {
	checker := &fakeStatusChecker{steps: []checkerStep{idleStep("sess-1", "sess-2")}}

	err := WaitUntilIdle(context.Background(), "ws-1", checker, 5*time.Second)
	assert.NoError(t, err)
	assert.Equal(t, 1, checker.callCount(), "idle snapshot must return on the first poll")
}

func TestWaitUntilIdle_EmptySessions_ReturnsImmediately(t *testing.T) {
	checker := &fakeStatusChecker{}

	err := WaitUntilIdle(context.Background(), "ws-1", checker, 5*time.Second)
	assert.NoError(t, err)
}

func TestWaitUntilIdle_BusyThenIdle_ReturnsAfterPoll(t *testing.T) {
	checker := &fakeStatusChecker{steps: []checkerStep{
		busyStep("sess-1"),
		idleStep("sess-1"),
	}}

	err := WaitUntilIdle(context.Background(), "ws-1", checker, 5*time.Second)
	assert.NoError(t, err)
	assert.GreaterOrEqual(t, checker.callCount(), 2, "the busy first poll must not satisfy the drain")
}

func TestWaitUntilIdle_NeverIdle_TimeoutReturnsDrainError(t *testing.T) {
	checker := &fakeStatusChecker{steps: []checkerStep{busyStep("sess-1", "sess-2")}}

	err := WaitUntilIdle(context.Background(), "ws-1", checker, 100*time.Millisecond)
	require.Error(t, err)

	var drainErr *ErrDrainTimeout
	require.True(t, errors.As(err, &drainErr))
	assert.Len(t, drainErr.BusySessions, 2)
	assert.Contains(t, drainErr.BusySessions, "sess-1")
	assert.Contains(t, drainErr.BusySessions, "sess-2")
}

func TestWaitUntilIdle_ContextCancelled_ReturnsErr(t *testing.T) {
	checker := &fakeStatusChecker{steps: []checkerStep{busyStep("sess-1")}}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := WaitUntilIdle(ctx, "ws-1", checker, 5*time.Second)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestWaitUntilIdle_NewBusyDuringWait_HoldsTillIdle(t *testing.T) {
	// sess-1 goes idle but sess-2 becomes busy, then everything drains.
	checker := &fakeStatusChecker{steps: []checkerStep{
		busyStep("sess-1"),
		{statuses: map[string]string{"sess-1": "idle", "sess-2": "busy"}},
		idleStep("sess-1", "sess-2"),
	}}

	err := WaitUntilIdle(context.Background(), "ws-1", checker, 5*time.Second)
	assert.NoError(t, err)
}

func TestWaitUntilIdle_RetryStatusTreatedAsBusy(t *testing.T) {
	checker := &fakeStatusChecker{steps: []checkerStep{
		{statuses: map[string]string{"sess-1": "retry"}},
	}}

	err := WaitUntilIdle(context.Background(), "ws-1", checker, 100*time.Millisecond)
	require.Error(t, err)

	var drainErr *ErrDrainTimeout
	require.True(t, errors.As(err, &drainErr))
	assert.Equal(t, []string{"sess-1"}, drainErr.BusySessions)
}
