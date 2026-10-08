// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/services/outbox"
)

// sessionArchivedCode is the typed 409 discriminator for chat sends to
// an archived session (#1627). The frontend and SDK consumers key on
// the code to render the archived state and offer unarchive — the
// session-gone (410) and text-only-wedge (422) precedents' shape.
const sessionArchivedCode = "session_archived"

const sessionArchivedMessage = "This session is archived and read-only. Unarchive it to send messages; history stays viewable either way."

// rejectIfArchived enforces the #1627 read-only contract at the API
// proxy layer: chat sends to an archived session are refused with a
// typed 409 BEFORE any adapter call, slot reservation, or outbox
// accept. Reads are never guarded (history stays viewable).
//
// Enforced surfaces: /message, /prompt, /queue (+ sync fallback), the
// outbox delivery worker (both regimes), queue retry, question and
// permission replies (live + late-answer), question reject, inbox
// dismiss's live-reject arm, typed session actions (the
// answerQuestion/reject union), and the auto-approve bridge (skipped).
//
// Documented carve-outs (r-next findings A/B disposition):
//   - askDead inbox dismissal — terminalizes the record only; no
//     harness write (pure cleanup).
//   - AbortSession — lifecycle stop, content-neutral (issue §2:
//     history/files stay as-is); an archived session may still be
//     running a PEER-initiated in-pod turn (unblocked by the owner's
//     ruling) and stopping a stuck one is legitimate cleanup.
//   - RenameSessionInAgent — title metadata, cosmetic, content-neutral.
//   - DeleteSession — explicit destructive lifecycle action; nothing
//     is written INTO the conversation, and gating it would trap
//     archived sessions (user-confirmed intent guards it, same as for
//     live sessions).
//   - DeleteQueueMessage — platform-outbox dismissal only; no harness
//     write, not a read-only surface.
//
// Posture on a failed archive check: FAIL OPEN with a loud log. The
// marker is a platform UX guard, not a security boundary — the in-pod
// peer path (agentd send_message) bypasses the proxy by the owner's
// ruling, so fail-closed would trade chat availability for an
// enforcement that was never airtight. An absent index row reads as
// not-archived at the DB layer, so unindexed sessions always send.
func (h *ProxyHandler) rejectIfArchived(c *gin.Context, workspaceID, sessionID string) bool {
	if h.sessionIndex == nil {
		return false
	}
	archived, err := h.sessionIndex.IsArchived(c.Request.Context(), workspaceID, sessionID)
	if err != nil {
		h.logger.Warn("archived check unavailable, failing open",
			"workspaceID", workspaceID, "sessionID", sessionID, "error", err.Error())
		return false
	}
	if !archived {
		return false
	}
	c.JSON(http.StatusConflict, gin.H{
		"code":  sessionArchivedCode,
		"error": sessionArchivedMessage,
	})
	return true
}

// rejectIfRequestArchived resolves the session behind a pending
// question/permission request and applies the archived gate to it
// (#1627 review round 1 finding 2: reply surfaces are chat writes —
// user text delivered into a live turn — and must not bypass the
// read-only contract). An unresolvable request is NOT rejected here;
// the caller's own live-path flow owns its 404/503 semantics.
func (h *ProxyHandler) rejectIfRequestArchived(c *gin.Context, workspaceID, requestID string) bool {
	if h.sessionIndex == nil {
		return false
	}
	sessionID, resolvable, _ := h.inputRequestSession(c.Request.Context(), workspaceID, requestID)
	if !resolvable || sessionID == "" {
		return false
	}
	return h.rejectIfArchived(c, workspaceID, sessionID)
}

// archivedDeliveryRefusal is the outbox-worker arm of the #1627
// read-only contract: delivery for an archived session returns a
// TERMINAL outbox error — the entry parks as error on the first pass,
// never retried. Nil return means proceed (including on check failure:
// the same fail-open posture as the handler gate — a UX guard must not
// strand accepted messages on DB blips).
func (h *ProxyHandler) archivedDeliveryRefusal(ctx context.Context, workspaceID, sessionID string) error {
	if h.sessionIndex == nil {
		return nil
	}
	archived, err := h.sessionIndex.IsArchived(ctx, workspaceID, sessionID)
	if err != nil {
		h.logger.Warn("outbox archived check unavailable, delivering (fail-open)",
			"workspaceID", workspaceID, "sessionID", sessionID, "error", err.Error())
		return nil
	}
	if archived {
		return outbox.Terminal(fmt.Errorf("session archived: delivery refused (code %s) — unarchive and resend", sessionArchivedCode))
	}
	return nil
}
