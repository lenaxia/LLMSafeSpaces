// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// reclaim.go — #1847 spec item 5: board_reclaim, the prior-claimant
// recovery path. A lapsed-then-alive claimant reclaims WITH CONTEXT:
// attempts unchanged (attempts counts context-loss events — a
// same-session reclaim lost nothing).
//
// Identity follows the claim token from the worker's charter envelope:
// the prior token's assign history (the claim_assign event written at
// the spawn-stamp) names the context-holding session. No LLM-declared
// session id exists anywhere on the path — the #1847 trust rule
// (worker-declared identity is not input) holds here too.
//
// Guards: the prior token must appear in THIS task's assign history;
// no live claimant holds the row (backlog, or in_progress with a
// lapsed lease); non-terminal; pending_input is the human gate (its
// exits are manager-surface-only — reclaim cannot bypass it); paused
// rows refuse the mint (#1747, the bounce's belt).

import (
	"context"
)

// stub: #1847 red-first — behavior lands with the claim-history work.
func (s *Service) ReclaimTask(ctx context.Context, actor Actor, taskID, shortIDOrID, priorClaimToken, requestID string) (*Claim, error) {
	_ = ctx
	_ = actor
	_ = taskID
	_ = shortIDOrID
	_ = priorClaimToken
	_ = requestID
	return nil, &GuardError{Code: GuardClaimToken, Detail: "board_reclaim lands with #1847 PR3 (stub)"}
}
