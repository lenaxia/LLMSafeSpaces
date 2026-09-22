// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/lenaxia/llmsafespaces/controller/internal/metrics"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// Session-aware drain before controller-initiated pod deletion (#761).
//
// Problem: deletePodByName was a plain r.Delete on every path (suspend,
// restart-generation bump, architecture drift, password-secret heal, health
// restart, terminate). Combined with a 5s terminationGracePeriodSeconds this
// killed in-flight LLM turns mid-stream — the SSE connection the user was
// watching was severed with no chance for agentd to drain or for opencode to
// flush partial output.
//
// Design (resurrecting the never-merged #756/#761 fix, re-verified against
// the #1342/#1374 agentd semantics):
//
//   - Before a deferrable deletion, consult agentd's /v1/statusz. Idle →
//     proceed. Busy → defer (requeue) while the sessions are making progress.
//   - Progress is the statusz-observable proxy for #1342's part-level
//     lastEventAt signal: any change in the busy session set, per-session
//     busy/idle status, or per-session ContextUsed (step-finish cadence)
//     between consecutive drain polls. agentd does NOT expose lastEventAt on
//     statusz (verified: BusyAges is wall-clock; busyPartitions is
//     agentd-internal), so the controller's notion is coarser — the bound is
//     sized accordingly (60m vs agentd's 30s LeaseConvergenceBound).
//   - Force when busy sessions show NO observable progress for
//     drainStallBound. The bound deliberately exceeds documented legitimate
//     turn lengths (30-40 min silent builds — the 2026-09-11 incident class
//     that made the owner retire fixed 15-minute wall-clock bounds agentd
//     side): a wall-clock bound below legitimate turn lengths IS the
//     force-kill bug again. A turn that produces zero statusz-visible change
//     for a full hour is wedged with high confidence, and the force lands as
//     a graceful SIGTERM under the full termination grace — #1374's
//     transcript repair folds any orphaned running parts as aborted on the
//     next session read.
//   - Fail open when statusz is unreachable: a dead agentd must never block
//     deletion (this is what makes the password-secret path safe — with the
//     Secret gone the consult cannot authenticate and deletion proceeds).
//
// Not integrated, by design:
//   - terminate (handleTerminating): explicit user destroy — intent trumps
//     in-flight turns.
//   - health-restart (restartAgentPod): by the time agentd has failed the
//     health threshold the drain could at best fail open; consulting it only
//     delays recovery.
//   - suspendFromPreActive: pre-Active workspaces proxy 503 (the API's
//     not-Active path), so no user turns can be in flight.
//
// Controller-restart semantics: the drain window is in-memory (mirrors
// lastDeepStatus). A controller restart mid-drain resets the window — the
// next reconcile re-observes busy state and starts a fresh window. This can
// extend a drain past drainStallBound by at most one controller lifetime;
// acceptable for a best-effort protection layer.

const (
	drainReasonRestartGeneration     = "restart_generation"
	drainReasonArchitectureDrift     = "architecture_drift"
	drainReasonPasswordSecretMissing = "password_secret_missing"
)

// drainPollInterval is the requeue cadence while a deletion is deferred
// behind busy sessions. 10s: fast enough that an idle transition is noticed
// well inside the user's patience, slow enough that a draining controller
// does not hammer statusz (which is expensive on the agentd side — multiple
// opencode calls under a mutex on cache expiry).
var drainPollInterval = 10 * time.Second

// drainStallBound is how long busy sessions may show NO statusz-observable
// progress before the deletion is forced. See the file-header rationale:
// this is the controller-side analog of agentd's progress-keyed stall
// bound (sessionstate.LeaseConvergenceBound, 30s at part-level granularity),
// inflated to 60m because the controller's progress signal is step-level
// (ContextUsed / busy-set churn) — an order of magnitude coarser than
// agentd's SSE event stream.
var drainStallBound = 60 * time.Minute

// drainHTTPClient bounds the drain's statusz consult. Deep-status keeps its
// own 30s client because it enriches many fields; the drain only needs the
// session block, and it runs inline in the reconcile loop.
var drainHTTPClient = &http.Client{Timeout: 10 * time.Second}

// drainSnapshot is the comparable fingerprint of a statusz observation used
// as the progress signal: which sessions are busy and how much context each
// has consumed. Busy-age is excluded — it grows with wall-clock by
// construction and would report progress for a dead turn.
type drainSnapshot struct {
	busy map[string]int64 // busy session ID → ContextUsed
}

func snapshotFromStatusz(status *agentd.StatuszResponse) drainSnapshot {
	s := drainSnapshot{busy: map[string]int64{}}
	if status == nil {
		return s
	}
	for _, sess := range status.Sessions {
		if sess.Status == "busy" {
			s.busy[sess.ID] = sess.ContextUsed
		}
	}
	return s
}

// differsFrom reports whether this snapshot carries any observable change
// versus the previous one: a session joining/leaving the busy set, or a
// busy session's consumed context moving (a completed model step).
func (s drainSnapshot) differsFrom(prev drainSnapshot) bool {
	if len(s.busy) != len(prev.busy) {
		return true
	}
	for id, used := range s.busy {
		prevUsed, ok := prev.busy[id]
		if !ok || prevUsed != used {
			return true
		}
	}
	return false
}

// podDrainState is the in-memory per-workspace drain window. podIP pins
// the window to one pod incarnation: opencode session IDs survive pod
// recreation (the session DB lives on the PVC), so a same-ID/same-context
// busy set on a NEW pod must not inherit the old pod's progress age — the
// window resets when the pod identity changes.
type podDrainState struct {
	podIP          string
	startedAt      time.Time
	lastProgressAt time.Time
	lastSnapshot   drainSnapshot
	announced      bool // first-defer event + metric emitted
}

// drainKey namespaces the in-memory drain state: workspace NAME alone
// collides across namespaces (two tenants each with workspace "demo").
func drainKey(ws *v1.Workspace) string {
	return ws.Namespace + "/" + ws.Name
}

// drainDecision tells a deletion path what to do with the pod.
type drainDecision int

const (
	// drainProceed: delete now (idle agent, failed open, or stalled beyond
	// the bound — events/metrics already emitted where relevant).
	drainProceed drainDecision = iota
	// drainDefer: keep the pod, requeue at drainPollInterval.
	drainDefer
)

// drainBeforePodDeletion is the session-aware gate for controller-initiated
// pod deletion. Call it immediately before deletePodByName on deferrable
// paths; on drainDefer return RequeueAfter drainPollInterval without
// touching pod or status.
func (r *WorkspaceReconciler) drainBeforePodDeletion(ctx context.Context, ws *v1.Workspace, reason string) drainDecision {
	if ws.Status.PodIP == "" {
		return drainProceed
	}

	status, err := r.fetchAgentStatusz(ctx, drainHTTPClient, ws)
	if err != nil || status == nil || !status.Healthy {
		// Fail open (#761): an unreachable or unhealthy agentd must never
		// block deletion. Its session state is unknown, not known-idle.
		metrics.WorkspaceDrainFailedOpenTotal.WithLabelValues(reason).Inc()
		if r.Recorder != nil {
			r.Recorder.Eventf(ws, corev1.EventTypeNormal, "SessionDrainFailedOpen",
				"agentd statusz unreachable or unhealthy; proceeding with pod deletion (%s)", reason)
		}
		return drainProceed
	}

	snap := snapshotFromStatusz(status)
	if len(snap.busy) == 0 {
		r.clearDrainState(drainKey(ws))
		return drainProceed
	}

	now := time.Now()
	key := drainKey(ws)
	r.drainStatesMu.Lock()
	if r.drainStates == nil {
		r.drainStates = make(map[string]*podDrainState)
	}
	state := r.drainStates[key]
	if state == nil || state.podIP != ws.Status.PodIP {
		state = &podDrainState{podIP: ws.Status.PodIP, startedAt: now, lastProgressAt: now, lastSnapshot: snap}
		r.drainStates[key] = state
	} else if snap.differsFrom(state.lastSnapshot) {
		state.lastProgressAt = now
	}
	state.lastSnapshot = snap
	progressAge := now.Sub(state.lastProgressAt)
	announced := state.announced
	state.announced = true
	r.drainStatesMu.Unlock()

	if !announced {
		metrics.WorkspaceDrainDeferredTotal.WithLabelValues(reason).Inc()
		if r.Recorder != nil {
			r.Recorder.Eventf(ws, corev1.EventTypeNormal, "SessionDrainDeferred",
				"pod deletion deferred (%s): %d busy session(s), polling until idle or stalled",
				reason, status.SessionsActive)
		}
	}

	if progressAge >= drainStallBound {
		r.clearDrainState(key)
		metrics.WorkspaceDrainForcedTotal.WithLabelValues(reason).Inc()
		if r.Recorder != nil {
			r.Recorder.Eventf(ws, corev1.EventTypeWarning, "SessionDrainForced",
				"pod deletion forced (%s): busy sessions made no observable progress for %s (bound %s)",
				reason, progressAge.Round(time.Second), drainStallBound)
		}
		return drainProceed
	}

	log.FromContext(ctx).V(1).Info("deferring pod deletion behind busy sessions",
		"workspace", ws.Name, "reason", reason, "busySessions", status.SessionsActive,
		"progressAge", progressAge.Round(time.Second))
	return drainDefer
}

func (r *WorkspaceReconciler) clearDrainState(workspaceName string) {
	r.drainStatesMu.Lock()
	delete(r.drainStates, workspaceName)
	r.drainStatesMu.Unlock()
}

// fetchAgentStatusz GETs agentd's /v1/statusz on the admin mux, trying each
// Bearer credential in order (#887 D5.1 mixed fleet: distinct admin token
// first, workspace password fallback for legacy pods; a missing Secret
// degrades to one unauthenticated attempt). Shared by the deep-status
// enrichment (enrichAgentStatus, 30s client) and the pre-deletion drain
// (10s client — it runs inline in the reconcile loop).
func (r *WorkspaceReconciler) fetchAgentStatusz(ctx context.Context, client *http.Client, ws *v1.Workspace) (*agentd.StatuszResponse, error) {
	endpoint := fmt.Sprintf("http://%s:%d/v1/statusz", ws.Status.PodIP, agentdAdminPort)
	resp, err := statuszWithBearers(ctx, client, endpoint, r.agentStatuszBearers(ctx, ws))
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var status agentd.StatuszResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, err
	}
	return &status, nil
}
