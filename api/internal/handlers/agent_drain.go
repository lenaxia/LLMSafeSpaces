// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/lenaxia/llmsafespaces/api/internal/services/sse"
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
// the context is canceled, or the deadline fires.
func WaitUntilIdle(
	ctx context.Context,
	workspaceID string,
	tracker *sse.Tracker,
	statusChecker SessionStatusChecker,
	timeout time.Duration,
) error {
	drainCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type event struct {
		sessionID string
		idle      bool
	}
	events := make(chan event, 64)

	// Subscribe BEFORE snapshot to avoid missing transitions.
	unsub := tracker.SubscribeDrain(workspaceID,
		func(_, sid string) {
			select {
			case events <- event{sid, true}:
			default:
			}
		},
		func(_, sid string) {
			select {
			case events <- event{sid, false}:
			default:
			}
		},
	)
	defer unsub()

	// Authoritative snapshot from the agent.
	statuses, err := statusChecker.GetSessionStatuses(drainCtx)
	if err != nil {
		return fmt.Errorf("WaitUntilIdle: snapshot: %w", err)
	}

	// Seed the busy set.
	busy := make(map[string]struct{})
	for id, typ := range statuses {
		if typ != "idle" {
			busy[id] = struct{}{}
		}
	}

	if len(busy) == 0 {
		return nil
	}

	// Event loop.
	for {
		select {
		case e := <-events:
			if e.idle {
				delete(busy, e.sessionID)
			} else {
				busy[e.sessionID] = struct{}{}
			}
			if len(busy) == 0 {
				return nil
			}
		case <-drainCtx.Done():
			remaining := make([]string, 0, len(busy))
			for id := range busy {
				remaining = append(remaining, id)
			}
			sort.Strings(remaining)
			if drainCtx.Err() == context.DeadlineExceeded {
				return &ErrDrainTimeout{BusySessions: remaining}
			}
			return drainCtx.Err()
		}
	}
}
