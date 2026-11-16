// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// proxy_worker_silent.go — the #2047 silent-hang arm (parent #2040).
//
// session_hung (escalateHungs) fires on long BUSY age — the 2026-11-15
// incident's workers were SILENT: their event tables were static, so
// busy age never accumulated and the synthesized IDLE settles kept the
// busy view clean. Zero alerts fired while 3 of 4 board slots were
// held 4+ hours. Busy-age is structurally blind to silent hangs.
//
// This arm asks the other question, per claimed in_progress board
// task: has the assignee's WORKER-activity stamp (the #2040
// movement-allowlist index — post-#2042 the hub stamps worker activity
// only, so a quiet stamp is a TRUE quiet) been silent past the
// threshold AND is the task's own progress clock (post-#2043
// two-clock split: claim mints + worker heartbeats) equally stale?
// Both legs stale → one SESSION-SCOPED worker_silent alert carrying
// the task facts (short id, project, lease state) so the on-call sees
// WHICH slot is held. Same cooldown/resolution discipline as
// session_hung: one alert per cooldown window, auto-resolve on resumed
// activity, notify-only.
//
// The board join rides a NARROW seam (QuietBoardLookup — the
// SetFrameObserver one-function wiring style): the proxy handler never
// holds the board service; app.go passes a read-only lookup. Unknown
// stamps (hub restart, unindexed session — QuietSeconds nil) are NOT
// evidence of silence and NEVER alert (#1685's fail-toward-silence,
// applied to absence as well). Derived-busy truth outranks the stamp:
// a mid-tool-call worker is ALIVE however silent (2026-10-29 ruling).

import (
	"context"
	"time"

	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
)

// Fatigue guards (issue #2047 R5): the threshold is floored at 30
// minutes and the cooldown is floored at session_hung's
// (busyAlertCooldown) so the silent arm can never be configured noisier
// than the busy arm. Vars for tests, like the d6 knobs.
var (
	silentWorkerOlderThan = 30 * time.Minute
	silentWorkerCooldown  = 30 * time.Minute
)

// silentWorkerThresholdFloor is the hard minimum evidence window.
const silentWorkerThresholdFloor = 30 * time.Minute

// silentWorkerThreshold is the effective evidence window — the
// configured value clamped to the floor.
func silentWorkerThreshold() time.Duration {
	if silentWorkerOlderThan < silentWorkerThresholdFloor {
		return silentWorkerThresholdFloor
	}
	return silentWorkerOlderThan
}

// silentWorkerCooldownWindow is the effective re-alert cooldown — the
// configured value clamped to session_hung's (never noisier than the
// busy arm).
func silentWorkerCooldownWindow() time.Duration {
	if silentWorkerCooldown < busyAlertCooldown {
		return busyAlertCooldown
	}
	return silentWorkerCooldown
}

// quietLookupBudget bounds one workspace's board-join read. The sweep
// rides the state reconciler's tick; stalling here stalls every other
// periodic duty (the boardObserveBudget hygiene, proxy-side).
const quietLookupBudget = 5 * time.Second

// QuietTaskFact is one in_progress board task's assignee-liveness
// fact — the evaluator's entire world per task. The task facts ride
// the alert so the on-call sees WHICH slot is held, not just which
// session (issue #2047 R2).
type QuietTaskFact struct {
	// TaskShortID is the task's phrase-form short id (tsk-…).
	TaskShortID string
	// ProjectID is the owning project's short id.
	ProjectID string
	// LeaseState is the claim's lease posture: "live", "lapsed", or
	// "none" (no claim on the row).
	LeaseState string
	// AssigneeSession is the assignee session ref (storage-canonical
	// raw id; the alert is session-scoped on it).
	AssigneeSession string
	// QuietSeconds is how long since the assignee's last WORKER-
	// activity stamp. nil = unknown (no stamp this process — hub
	// restart, unindexed): never evidence of silence.
	QuietSeconds *int
	// Busy is the derived-busy truth (mid-turn). Busy outranks quiet:
	// a busy worker is alive however silent.
	Busy bool
	// LastProgressAt is the task's progress clock (post-#2043 split:
	// claim mints + worker heartbeat responses). nil = no evidence.
	LastProgressAt *time.Time
}

// QuietBoardLookup is the narrow board seam: the in_progress tasks
// with assignees for one workspace, with their liveness facts. Mirrors
// the SetFrameObserver one-function wiring style — the proxy handler
// NEVER holds the board service (issue #2047 R4: no new polling loops
// either — the evaluator rides the existing alert pass).
type QuietBoardLookup interface {
	InProgressAssignees(ctx context.Context, workspaceID string) []QuietTaskFact
}

// SetQuietBoardLookup installs the board seam (app.go's board wiring;
// nil = inert — board disabled or pre-wiring boot). Safe before or
// after Start: the sweep reads it per pass.
func (h *ProxyHandler) SetQuietBoardLookup(lookup QuietBoardLookup) {
	h.quietBoardLookupMu.Lock()
	h.quietBoardLookup = lookup
	h.quietBoardLookupMu.Unlock()
}

func (h *ProxyHandler) currentQuietBoardLookup() QuietBoardLookup {
	h.quietBoardLookupMu.RLock()
	defer h.quietBoardLookupMu.RUnlock()
	return h.quietBoardLookup
}

// workerSilentEvidence is the two-legged gate: the assignee's worker
// stamp quiet past the threshold AND the task's progress clock
// equally stale (nil progress = evidence-unavailable — never a silent
// verdict, the #1685 precedent). Busy truth vetoes the whole verdict.
func workerSilentEvidence(f QuietTaskFact, threshold time.Duration, now time.Time) bool {
	if f.Busy {
		return false // mid-turn is alive (2026-10-29 ruling)
	}
	if f.QuietSeconds == nil {
		return false // unknown stamp: never silence
	}
	if time.Duration(*f.QuietSeconds)*time.Second < threshold {
		return false
	}
	if f.LastProgressAt == nil {
		return false // no progress evidence: fail toward silence
	}
	return now.Sub(*f.LastProgressAt) >= threshold
}

// workerAliveEvidence is positive liveness: fresh worker frames, a
// busy truth, or a fresh task progress clock (the worker declared
// progress post-#2043 — only claims and heartbeats write it). Only
// this resolves a live alert; unknown shapes (nil stamp) resolve
// nothing (the read-side stale-heal bounds the dangler).
func workerAliveEvidence(f QuietTaskFact, threshold time.Duration, now time.Time) bool {
	if f.Busy {
		return true
	}
	if f.QuietSeconds != nil &&
		time.Duration(*f.QuietSeconds)*time.Second < threshold {
		return true
	}
	if f.LastProgressAt != nil && now.Sub(*f.LastProgressAt) < threshold {
		return true
	}
	return false
}

// escalateSilentWorkers is the #2047 sweep, riding the same reconciler
// tick as escalateHungs (the existing alert evaluation pass — no new
// polling loop). Per workspace: ask the seam for the in_progress
// facts, one session-scoped verdict per assignee (a session holding
// two tasks alerts once — the first fact's), d6's cooldown discipline
// per (workspace, session).
func (h *ProxyHandler) escalateSilentWorkers(workspaceIDs []string) {
	lookup := h.currentQuietBoardLookup()
	if lookup == nil || len(workspaceIDs) == 0 {
		return // inert seam: nothing to ask
	}
	ctx, cancel := context.WithTimeout(context.Background(), quietLookupBudget)
	defer cancel()
	now := time.Now()
	threshold := silentWorkerThreshold()
	for _, wid := range workspaceIDs {
		facts := lookup.InProgressAssignees(ctx, wid)
		if len(facts) == 0 {
			continue
		}
		bySession := make(map[string]QuietTaskFact, len(facts))
		for _, f := range facts {
			if f.AssigneeSession == "" {
				continue
			}
			if _, seen := bySession[f.AssigneeSession]; !seen {
				bySession[f.AssigneeSession] = f
			}
		}
		for ses, f := range bySession {
			if workerSilentEvidence(f, threshold, now) {
				if h.silentWorkerCooling(wid, ses) {
					// Still silent inside the re-alert cooldown: no
					// new alert, no resolution — the silence persists.
					continue
				}
				h.publishWorkerSilent(wid, ses, f)
				continue
			}
			if h.silentWorkerAlerted(wid, ses) && workerAliveEvidence(f, threshold, now) {
				h.resolveWorkerSilent(wid, ses)
			}
		}
	}
}

// publishWorkerSilent emits the session-scoped alert with the task
// facts (issue #2047 R2: the on-call sees WHICH slot) and persists it
// through the same session-alerts drainer as session_hung (best-effort
// durability; the SSE path is the primary surface). Non-blocking by
// construction on both legs.
func (h *ProxyHandler) publishWorkerSilent(workspaceID, sessionID string, f QuietTaskFact) {
	data := map[string]any{
		"alert":         "worker_silent",
		"task_short_id": f.TaskShortID,
		"project_id":    f.ProjectID,
		"lease_state":   f.LeaseState,
		"policy":        "notify_only",
		"guidance": "claimed board task with no worker-activity and no task progress beyond the evidence window — " +
			"the slot is likely held by a silent worker; depose, nudge, or intervene manually",
	}
	quiet := 0
	if f.QuietSeconds != nil {
		quiet = *f.QuietSeconds
		data["assignee_quiet_seconds"] = quiet
	}
	if f.LastProgressAt != nil {
		data["last_progress_seconds"] = int(time.Since(*f.LastProgressAt).Seconds())
	}
	h.publishWorkspaceAndUserEvent(workspaceID, apitypes.WorkspaceSSEEvent{
		Type:      "workspace.alert",
		SessionID: sessionID,
		Status:    "worker_silent",
		Data:      data,
	})
	h.markSilentWorkerAlerted(workspaceID, sessionID)
	if h.sessionAlerts != nil {
		h.sessionAlerts.RecordTaskAlert(workspaceID, sessionID, "worker_silent", quiet, map[string]string{
			"task_short_id": f.TaskShortID,
			"project_id":    f.ProjectID,
			"lease_state":   f.LeaseState,
		})
	}
	h.logger.Warn("#2047 escalation: board worker silent (notify-only)",
		"workspaceID", workspaceID, "sessionID", sessionID,
		"taskShortID", f.TaskShortID, "projectID", f.ProjectID,
		"leaseState", f.LeaseState, "assigneeQuietSeconds", quiet)
}

// resolveWorkerSilent closes one session's live silent condition: the
// SSE clear (session-scoped — only the recovered worker's badge
// drops), the persisted session-scoped resolution, and the cooldown
// drop (a fresh silence re-alerts immediately).
func (h *ProxyHandler) resolveWorkerSilent(workspaceID, sessionID string) {
	h.publishWorkspaceAndUserEvent(workspaceID, apitypes.WorkspaceSSEEvent{
		Type:      "workspace.alert_resolved",
		SessionID: sessionID,
		Status:    "worker_silent",
		Data: map[string]any{
			"alert":  "worker_silent",
			"policy": "notify_only",
		},
	})
	if h.sessionAlerts != nil {
		h.sessionAlerts.ResolveSession(workspaceID, sessionID)
	}
	h.clearSilentWorkerAlerted(workspaceID, sessionID)
	h.logger.Info("#2047 escalation: board worker resumed (silence resolved)",
		"workspaceID", workspaceID, "sessionID", sessionID)
}

// silentAlertsKey is the (workspace, session) cooldown key — the alert
// is session-scoped, so the cooldown is too (3 silent workers = 3
// independent cooldowns, never one workspace-wide gate).
func silentAlertsKey(workspaceID, sessionID string) string {
	return workspaceID + "\x00" + sessionID
}

func (h *ProxyHandler) silentWorkerCooling(workspaceID, sessionID string) bool {
	h.silentAlertsMu.Lock()
	defer h.silentAlertsMu.Unlock()
	last, ok := h.silentAlerts[silentAlertsKey(workspaceID, sessionID)]
	return ok && time.Since(last) < silentWorkerCooldownWindow()
}

func (h *ProxyHandler) silentWorkerAlerted(workspaceID, sessionID string) bool {
	h.silentAlertsMu.Lock()
	defer h.silentAlertsMu.Unlock()
	_, ok := h.silentAlerts[silentAlertsKey(workspaceID, sessionID)]
	return ok
}

func (h *ProxyHandler) markSilentWorkerAlerted(workspaceID, sessionID string) {
	h.silentAlertsMu.Lock()
	h.silentAlerts[silentAlertsKey(workspaceID, sessionID)] = time.Now()
	h.silentAlertsMu.Unlock()
}

func (h *ProxyHandler) clearSilentWorkerAlerted(workspaceID, sessionID string) {
	h.silentAlertsMu.Lock()
	delete(h.silentAlerts, silentAlertsKey(workspaceID, sessionID))
	h.silentAlertsMu.Unlock()
}
