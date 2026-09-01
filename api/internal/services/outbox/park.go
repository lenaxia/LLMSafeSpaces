// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package outbox

import (
	"context"
	"encoding/json"
	"time"
)

// StatusParked marks an entry parked by an operator action (the flip
// gate's mode_transition). Parked entries are inert to the delivery
// loop — no auto-retry, no backoff — and re-arm only through
// UnparkWorkspace (the rollback drain) or explicit dismissal.
const StatusParked = "parked"

// parkReasonPrefix namespaces the flip gate's park reason; the unpark
// sweep matches exactly this prefix so genuine error entries are never
// re-armed by a rollback.
const parkReasonPrefix = "mode_transition: "

// ParkWorkspace moves every in-flight (pending/delivering/verifying)
// outbox entry of the workspace to StatusParked carrying the explicit
// reason — design 0055 M4's drain-before-flip: entries that would race
// a mode change are held visible with why, never auto-re-sent. Returns
// the number parked.
func (s *Service) ParkWorkspace(ctx context.Context, workspaceID, reason string) (int, error) {
	return s.parkSweep(ctx, workspaceID, parkReasonPrefix+reason, true)
}

// UnparkWorkspace re-arms exactly the mode_transition parks of the
// workspace back to pending (the rollback's ledger back-drain via the
// 0052 path). Returns the number re-armed.
func (s *Service) UnparkWorkspace(ctx context.Context, workspaceID string) (int, error) {
	pairs := s.sessions(ctx)
	now := time.Now().UTC()
	unparked := 0
	for _, p := range pairs {
		ws, ses := p[0], p[1]
		if ws != workspaceID {
			continue
		}
		qk := qKey(ws, ses)
		vals, err := s.client.LRange(ctx, qk, 0, -1).Result()
		if err != nil {
			continue
		}
		for i, v := range vals {
			var e Entry
			if json.Unmarshal([]byte(v), &e) != nil {
				continue
			}
			if e.Status != StatusParked || !hasParkReason(e.LastError) {
				continue
			}
			e.Status = StatusPending
			e.NextAttemptAt = now
			raw, err := json.Marshal(e)
			if err != nil {
				continue
			}
			if err := s.client.LSet(ctx, qk, int64(i), string(raw)).Err(); err != nil {
				continue
			}
			unparked++
		}
	}
	return unparked, nil
}

func (s *Service) parkSweep(ctx context.Context, workspaceID, reason string, inFlightOnly bool) (int, error) {
	pairs := s.sessions(ctx)
	parked := 0
	for _, p := range pairs {
		ws, ses := p[0], p[1]
		if ws != workspaceID {
			continue
		}
		qk := qKey(ws, ses)
		vals, err := s.client.LRange(ctx, qk, 0, -1).Result()
		if err != nil {
			continue
		}
		for i, v := range vals {
			var e Entry
			if json.Unmarshal([]byte(v), &e) != nil {
				continue
			}
			if !parkable(e.Status) {
				continue
			}
			e.Status = StatusParked
			e.LastError = reason
			e.NextAttemptAt = time.Time{}
			raw, err := json.Marshal(e)
			if err != nil {
				continue
			}
			if err := s.client.LSet(ctx, qk, int64(i), string(raw)).Err(); err != nil {
				continue
			}
			parked++
		}
	}
	_ = inFlightOnly // parkable() already scopes to in-flight statuses
	return parked, nil
}

func parkable(status string) bool {
	switch status {
	case StatusPending, StatusDelivering, StatusVerifying:
		return true
	}
	return false
}

func hasParkReason(lastErr string) bool {
	return len(lastErr) >= len(parkReasonPrefix) && lastErr[:len(parkReasonPrefix)] == parkReasonPrefix
}

// SeedEntryForTest pushes a raw queue entry (test harnesses only).
func (s *Service) SeedEntryForTest(ctx context.Context, ws, ses, rawEntry string) error {
	return s.client.RPush(ctx, qKey(ws, ses), rawEntry).Err()
}
