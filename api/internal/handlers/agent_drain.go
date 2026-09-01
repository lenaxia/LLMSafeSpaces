// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// SessionStatusChecker returns the current status map for all sessions
// on a workspace. *opencode.Client satisfies this; the interface decouples
// agent_drain.go from the concrete opencode import.
type SessionStatusChecker interface {
	GetSessionStatuses(ctx context.Context) (map[string]string, error)
}

// ErrDrainTimeout is returned by WaitUntilIdle when the deadline elapses
// before all sessions become idle.
type ErrDrainTimeout struct {
	BusySessions []string
}

func (e *ErrDrainTimeout) Error() string {
	return fmt.Sprintf("drain timeout: sessions still busy: %v", e.BusySessions)
}

// WaitUntilIdle blocks until all sessions in the workspace are idle,
// the context is canceled, or the deadline fires. US-69.11: the retired
// SSE tracker's drain subscription is replaced by a bounded statusz
// poll — the agent's own status is the authority, and drain windows are
// multi-second, so polling is equivalent without a held stream.
func WaitUntilIdle(
	ctx context.Context,
	workspaceID string,
	statusChecker SessionStatusChecker,
	timeout time.Duration,
) error {
	drainCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(drainPollInterval)
	defer ticker.Stop()

	// busy reports the still-busy sessions; ok=false means the statusz
	// read failed — an unavailable authority is NOT drain completion
	// (a broken statusz must not yield a false "drained" verdict).
	busy := func() (remaining []string, ok bool) {
		statuses, err := statusChecker.GetSessionStatuses(drainCtx)
		if err != nil {
			return nil, false // transient: retry on the next tick
		}
		for id, typ := range statuses {
			if typ != "idle" {
				remaining = append(remaining, id)
			}
		}
		sort.Strings(remaining)
		return remaining, true
	}

	if remaining, ok := busy(); ok && len(remaining) == 0 {
		return nil
	}
	for {
		select {
		case <-drainCtx.Done():
			remaining, ok := busy()
			if ok && len(remaining) == 0 {
				return nil // idle on the final check
			}
			if drainCtx.Err() == context.DeadlineExceeded {
				if ok {
					return &ErrDrainTimeout{BusySessions: remaining}
				}
				return &ErrDrainTimeout{BusySessions: nil}
			}
			return drainCtx.Err()
		case <-ticker.C:
			if remaining, ok := busy(); ok && len(remaining) == 0 {
				return nil
			}
		}
	}
}

// drainPollInterval balances drain latency against statusz load (one
// request per active drain per tick).
const drainPollInterval = 500 * time.Millisecond
