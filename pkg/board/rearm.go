// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import "context"

// rearm.go — #1832 WS3: the attempts-exhaustion re-pool path. RED-FIRST
// SKELETON: the kill-tests in rearm_integration_test.go pin the behavior
// (escalated pending_input → backlog, attempts reset to 0 with the
// pre-reset count preserved in the audit event, replay idempotent,
// escalated-rows-only guard) before the implementation lands.

// RearmTask re-pools an attempts-exhausted (escalated) pending_input
// row. RED-FIRST SKELETON — returns ErrNotImplemented until the
// behavior lands.
func (s *Service) RearmTask(ctx context.Context, actor Actor, taskID, shortIDOrID, requestID string) (*Task, error) {
	_ = ctx
	_ = actor
	_ = taskID
	_ = shortIDOrID
	_ = requestID
	return nil, ErrNotImplemented
}
