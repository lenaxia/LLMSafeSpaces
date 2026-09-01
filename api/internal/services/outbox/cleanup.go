// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package outbox

import (
	"context"
)

// CleanupWorkspace deletes every outbox key of the workspace — queues,
// staging, dedupe markers, locks (#1119 follow-up from the #1211 F2
// triage: deleting a Workspace CR left orphaned Valkey residue with no
// agent to verify against, no queue UI to dismiss from, and no expiry;
// the rows inflated the outbox gauges forever and were re-scanned every
// worker tick). Returns the number of keys removed. Idempotent: a missed
// sweep leaves the keys for the next transition.
func (s *Service) CleanupWorkspace(ctx context.Context, workspaceID string) (int, error) {
	removed := 0
	for _, prefix := range []string{"outboxq:", "outboxd:", "outboxdedupe:", "outboxlock:"} {
		pattern := prefix + workspaceID + ":*"
		var cursor uint64
		for {
			keys, next, err := s.client.Scan(ctx, cursor, pattern, 100).Result()
			if err != nil {
				return removed, err
			}
			if len(keys) > 0 {
				if err := s.client.Del(ctx, keys...).Err(); err != nil {
					return removed, err
				}
				removed += len(keys)
			}
			if next == 0 {
				break
			}
			cursor = next
		}
	}
	return removed, nil
}
