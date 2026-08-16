// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// Self-healing recovery for send-path adapter failures (#817).
//
// Production signature (v0.15.4, observed twice): adapter.Send fails
// after ~2m5s while opencode completed the turn server-side — messages
// persisted, session back to idle — yet the client received 502 and the
// UI sat on a stale transcript until a manual refresh (#818). The
// in-flight POST response was lost somewhere between the API pod and
// the workspace pod; the turn itself succeeded.
//
// Rather than guess at the middlebox responsible for dropping the
// response (the #851 logging will name the transport error on the next
// occurrence), these helpers make the send path self-healing: when a
// send fails, ask the workspace whether the turn actually completed. If
// it did — session idle with a completion timestamp after the submit —
// return the assistant's message exactly as a successful send would.
// The client never sees the failure.
//
// The recovery is deliberately conservative:
//   - Session still busy or status unknown → keep the 502 (the turn may
//     legitimately still be running; the client's SSE/history fallback
//     owns that case).
//   - Completion timestamp predates the submit (± skew tolerance) →
//     keep the 502 (a previous turn's completion is not this turn's).
//   - History fetch fails or holds no assistant message → keep the 502
//     (never fabricate a response body for a caller that expects one).
const (
	// sendRecoverySessionTimeout bounds the GetSession probe. The probe
	// may run against a freshly-poisoned connection pool, so it must
	// never inherit an unbounded client timeout.
	sendRecoverySessionTimeout = 10 * time.Second
	// sendRecoveryHistoryTimeout bounds the recovery history fetch.
	// GetHistory stream-decodes the full transcript (multi-MB for long
	// sessions); the recovery must not wedge the handler longer than the
	// original send did.
	sendRecoveryHistoryTimeout = 30 * time.Second
	// sendRecoveryClockSkewTolerance absorbs pod clock skew when
	// comparing the session's completion time against the API pod's
	// submit time.
	sendRecoveryClockSkewTolerance = 2 * time.Second
)

// sendRecoveryOutcome describes why a recovery attempt did not yield a
// message. Used for logging only.
type sendRecoveryOutcome string

const (
	recoveryCompleted     sendRecoveryOutcome = "completed"
	recoveryProbeFailed   sendRecoveryOutcome = "probe_failed"
	recoveryBusy          sendRecoveryOutcome = "session_busy"
	recoveryStaleComplete sendRecoveryOutcome = "stale_completion"
	recoveryNoMessage     sendRecoveryOutcome = "no_assistant_message"
)

// recoverCompletedSend probes the workspace for a turn that completed
// after submittedAt despite the send failure. Returns outcome
// recoveryCompleted only when the session is idle and its completion
// timestamp is newer than submittedAt (modulo skew).
func (h *ProxyHandler) recoverCompletedSend(ctx context.Context, workspaceID, sessionID string, submittedAt time.Time) (*session.Session, sendRecoveryOutcome) {
	probeCtx, cancel := context.WithTimeout(ctx, sendRecoverySessionTimeout)
	defer cancel()

	s, err := h.adapter.GetSession(probeCtx, "", workspaceID, sessionID)
	if err != nil || s == nil {
		h.logger.Info("send recovery: session probe failed", "workspaceID", workspaceID, "sessionID", sessionID, "error", err)
		return nil, recoveryProbeFailed
	}
	if s.Status != session.StatusIdle {
		h.logger.Info("send recovery: session not idle — turn still in flight or unknown",
			"workspaceID", workspaceID, "sessionID", sessionID, "status", string(s.Status))
		return s, recoveryBusy
	}
	if s.Time == nil || s.Time.CompletedAt == nil {
		h.logger.Info("send recovery: idle session lacks completion time",
			"workspaceID", workspaceID, "sessionID", sessionID)
		return s, recoveryStaleComplete
	}
	if s.Time.CompletedAt.Before(submittedAt.Add(-sendRecoveryClockSkewTolerance)) {
		h.logger.Info("send recovery: completion predates submit — previous turn",
			"workspaceID", workspaceID, "sessionID", sessionID,
			"completedAt", s.Time.CompletedAt.Format(time.RFC3339Nano),
			"submittedAt", submittedAt.Format(time.RFC3339Nano))
		return s, recoveryStaleComplete
	}
	return s, recoveryCompleted
}

// fetchLastAssistantMessage returns the most recent assistant message
// from history, or nil when the fetch fails or no assistant message
// exists.
func (h *ProxyHandler) fetchLastAssistantMessage(ctx context.Context, workspaceID, sessionID string) *session.Message {
	fetchCtx, cancel := context.WithTimeout(ctx, sendRecoveryHistoryTimeout)
	defer cancel()

	msgs, err := h.adapter.GetHistory(fetchCtx, "", workspaceID, sessionID)
	if err != nil {
		h.logger.Info("send recovery: history fetch failed", "workspaceID", workspaceID, "sessionID", sessionID, "error", err)
		return nil
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Type == session.MessageAssistant {
			return &msgs[i]
		}
	}
	return nil
}

// attemptSendRecovery runs the full #817 recovery: verify the turn
// completed server-side, fetch the assistant's message, and respond as
// a successful send (same status, same body shape, same
// postAdapterSuccess side effects — the turn really happened, so
// activity/session-index/metering must record it).
//
// Returns true when the response was written (caller must return).
// Returns false without writing when the turn did not verifiably
// complete; the caller falls through to its existing 502 path.
func (h *ProxyHandler) attemptSendRecovery(c *gin.Context, workspace *v1.Workspace, workspaceID, sessionID string, submittedAt time.Time) bool {
	if _, outcome := h.recoverCompletedSend(c.Request.Context(), workspaceID, sessionID, submittedAt); outcome != recoveryCompleted {
		return false
	}

	msg := h.fetchLastAssistantMessage(c.Request.Context(), workspaceID, sessionID)
	if msg == nil {
		h.logger.Info("send recovery: turn completed but no assistant message recoverable",
			"workspaceID", workspaceID, "sessionID", sessionID)
		return false
	}

	h.logger.Info("send recovery: adapter send failed but turn completed server-side — returning response",
		"workspaceID", workspaceID, "sessionID", sessionID,
		"messageID", msg.ID,
		"elapsedMs", time.Since(submittedAt).Milliseconds())

	// Mirror the success path's side effects. The turn consumed the
	// same resources a successful send would have; activity tracking,
	// session-index recording, and metering must not be skipped.
	h.postAdapterSuccess(c, workspace, workspaceID, sessionID, true)
	if h.sessionIndex != nil {
		go h.fetchAndPersistTitle(workspaceID, sessionID)
	}
	c.JSON(http.StatusOK, msg)
	return true
}
