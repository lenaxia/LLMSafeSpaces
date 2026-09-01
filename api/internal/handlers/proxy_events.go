// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

func (h *ProxyHandler) onPhaseChange(workspace *v1.Workspace) {
	phase := workspace.Status.Phase

	prior, hadPrior := h.state().GetPriorPhase(context.Background(), workspace.Name)
	h.state().SetPriorPhase(context.Background(), workspace.Name, string(phase))

	if h.userBroker != nil && workspace.Spec.Owner.UserID != "" {
		h.userBroker.RecordWorkspaceOwner(workspace.Name, workspace.Spec.Owner.UserID)
		h.userBroker.PublishToUser(workspace.Spec.Owner.UserID, apitypes.WorkspaceSSEEvent{
			Type:        "workspace.phase",
			WorkspaceID: workspace.Name,
			Phase:       string(phase),
		})
	}

	if h.meteringSvc != nil && workspace.Spec.Owner.UserID != "" {
		// RecordLifecycleEvent is called unconditionally — including on seed calls
		// (prior=="") that fire when the API restarts with already-Active workspaces.
		// Seed calls produce a phantom lifecycle record with from_phase="" and
		// to_phase="Active". This was a deliberate tradeoff: the alternative (guarding
		// with prior!="") silently drops Creating→Active events for workspaces that
		// transition while the API is restarting, which corrupts billing data worse than
		// a phantom record. The metering service is expected to handle from_phase="" as
		// a no-op or a restart-artifact marker.
		if err := h.meteringSvc.RecordLifecycleEvent(
			context.Background(),
			workspace.Name,
			workspace.Spec.Owner.UserID,
			types.OwnerTypeUser,
			prior,
			string(phase),
			workspace.Spec.SecurityLevel,
			time.Now(),
		); err != nil {
			h.logger.Error("Failed to record lifecycle event", err,
				"workspace_id", workspace.Name,
				"phase", string(phase),
			)
		}
	}

	if phase == phaseSuspending || phase == phaseSuspended || phase == phaseTerminating || phase == phaseTerminated {
		h.invalidateCaches(context.Background(), workspace.Name)
		h.UsageStream().Close(workspace.Name)
		if phase == phaseTerminated || phase == phaseTerminating {
			h.state().DeletePriorPhase(context.Background(), workspace.Name)

			if h.activityTracker != nil {
				h.activityTracker.Delete(workspace.Name)
			}
			// #1119 (from the #1211 F2 triage): a deleted Workspace CR
			// left orphaned outbox keys — no agent to verify against, no
			// queue UI to dismiss from, no expiry; the rows inflated the
			// outbox gauges forever. Terminal phases clean the residue
			// (idempotent — a missed sweep retries on the next one).
			if h.outbox != nil {
				wsName := workspace.Name
				go func() {
					sctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 15*time.Second)
					defer cancel()
					if n, err := h.outbox.CleanupWorkspace(sctx, wsName); err != nil {
						h.logger.Warn("outbox cleanup on termination failed", "error", err, "workspace_id", wsName)
					} else if n > 0 {
						h.logger.Info("outbox cleanup on termination removed keys", "count", n, "workspace_id", wsName)
					}
				}()
			}
		}
		return
	}

	if phase == v1.WorkspacePhaseFailed {
		h.invalidateCaches(context.Background(), workspace.Name)
		return
	}

	if phase == phaseActive {
		// hadPrior==false means this is the first invocation for this
		// workspace in the handler — either a seed call (workspace was
		// already Active on API restart) or a real transition from a
		// phase not yet seen by the handler (e.g. Creating→Active on a
		// new workspace whose Creating event arrived before the handler
		// was aware of it). prior != phaseActive means a real transition
		// into Active (e.g. Creating → Active, Resuming → Active).
		//
		// #902: arm on EVERY Active event, transition or not. Prior-phase
		// state is Redis-backed and survives API restarts, so the
		// post-restart seed used to take the no-transition path and skip
		// arming entirely; gates that later died (pod churn) had no
		// re-arm path. Open is an idempotent map check — arming when
		// already armed costs nothing, and it must NOT be preceded by
		// Close in the no-transition case or every activity-driven
		// status update would tear down a healthy gate.
		if !hadPrior || prior != string(phaseActive) {
			// Real transition (or first sighting): full cache reset +
			// a fresh tracker connection — the old one targets the
			// previous pod.
			h.invalidateCaches(context.Background(), workspace.Name)
			h.UsageStream().Close(workspace.Name)
			// Resume self-heal: an agent that was unreachable (suspend,
			// OOM, restart) is now reachable — re-arm outbox entries
			// parked as "delivery unverifiable" so the #987 verify-first
			// path can confirm-and-remove them (or resume bounded
			// verification) instead of waiting for manual Retry.
			// Detached + bounded: phase handling must not block on Redis.
			if h.outbox != nil && h.adapter != nil {
				wsName := workspace.Name
				go func() {
					sctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 30*time.Second)
					defer cancel()
					if n, err := h.outbox.SweepWorkspaceUnverifiable(sctx, wsName); err != nil {
						h.logger.Warn("outbox unverifiable sweep failed", "error", err, "workspace_id", wsName)
					} else if n > 0 {
						h.logger.Info("outbox unverifiable sweep re-armed entries", "count", n, "workspace_id", wsName)
					}
				}()
			}
		} else {
			// No-transition update: only cached config (the original
			// else-branch semantics — do NOT nuke password/parent
			// caches on activity-driven status updates).
			h.state().InvalidateWorkspaceConfig(context.Background(), workspace.Name)
		}
	}
}

func (h *ProxyHandler) resolveRootSessionID(workspaceID, sessionID string) string {
	if h.sessionParents == nil || sessionID == "" {
		return sessionID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return h.sessionParents.resolveRoot(ctx, workspaceID, sessionID)
}

type queueUpdateData struct {
	Event     string `json:"event"`
	MessageID string `json:"messageID"`
	Error     string `json:"error,omitempty"`
}

func (h *ProxyHandler) publishQueueEvent(workspaceID, sessionID, event, messageID, errMsg string) {
	if h.userBroker == nil {
		return
	}
	data := queueUpdateData{
		Event:     event,
		MessageID: messageID,
	}
	if errMsg != "" {
		data.Error = errMsg
	}
	h.publishWorkspaceEvent(workspaceID, apitypes.WorkspaceSSEEvent{
		Type:      "queue.update",
		SessionID: sessionID,
		Data:      data,
	})
}

// reconcileSessionState runs on the state reconciler's tick. It queries
// /v1/statusz on the agentd admin port to get the current session
// states and reconciles stale activeSess entries: a session is idle in
// opencode (per statusz) but still marked active in our local activeSess map.
// This happens when opencode dies (OOM/SIGTERM) mid-stream — the
// session.status=idle event is never emitted, so onSessionIdle is never
// called, and our local map keeps the session marked busy forever. Without
// this fix, POST to a stuck session returns 409 Conflict indefinitely
// (until API restart). See incident report 2026-06-16 (sessions
// ses_13076538bffeYtLrhoZ2ccRM1E and ses_130c14344ffeVF52UQ6QGPmB0P stuck
// after pod OOMKill).
//
// Under Epic 63 V2, this also wakes idle sessions with pending
// queue-delivered input that stranded during a pod restart.
func (h *ProxyHandler) reconcileSessionState(workspaceID, podIP, password string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	url := fmt.Sprintf("http://%s:%d/v1/statusz", podIP, agentd.AgentdAdminPort) //nolint:gosec // G107: internal pod
	bearers := h.adminBearerCandidates(ctx, workspaceID, password)
	resp, err := GetWithBearers(ctx, h.httpClient, url, bearers)
	if err != nil {
		h.logger.Warn("reconcileSessionState: statusz unavailable", "workspaceID", workspaceID, "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		h.logger.Warn("reconcileSessionState: unexpected statusz status", "workspaceID", workspaceID, "status", resp.StatusCode)
		return
	}

	var statusz agentd.StatuszResponse
	// 1 MB cap mirrors the #801 fix (proxy_connections.go): statusz embeds
	// one entry per session in opencode's DB; >~55 sessions overflowed the
	// old 16 KB LimitReader, silently no-op'ing this reconcile (#892 D2 —
	// stale activeSess entries persisted client-side as phantom-busy).
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&statusz); err != nil {
		h.logger.Warn("reconcileSessionState: failed to decode statusz", "workspaceID", workspaceID, "error", err)
		return
	}

	for _, sess := range statusz.Sessions {
		if sess.Status != "idle" {
			continue
		}

		// Reconcile stale activeSess entries: opencode says idle, but our
		// local map says active. This is the OOM/SIGTERM case — clean up
		// regardless of whether there are queued messages.
		if h.isSessionActive(ctx, workspaceID, sess.ID) {
			h.logger.Info("reconcileSessionState: clearing stale activeSess entry",
				"workspaceID", workspaceID, "sessionID", sess.ID,
				"reason", "session is idle in opencode but marked active locally")
			h.removeActiveSession(ctx, workspaceID, sess.ID)
			// Publish session.status=idle so connected clients update their UI.
			// Without this, browsers showing the session keep their busy
			// indicator until the next page reload.
			if h.userBroker != nil {
				h.publishWorkspaceEvent(workspaceID, apitypes.WorkspaceSSEEvent{
					Type:      "session.status",
					SessionID: sess.ID,
					Status:    "idle",
				})
			}
		}
	}
}
