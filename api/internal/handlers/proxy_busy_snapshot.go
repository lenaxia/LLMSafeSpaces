// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// #1821 pieces 2-4 — the busy-snapshot handshake on the user-stream
// connect path. Busy was delta-only: a subscriber joining after a busy
// flip received nothing until the next transition (class 1), and a page
// refresh re-seeded from the REST list overlay whose accuracy rests on
// the statusz join (class 2). This file adds the snapshot-on-subscribe
// the message path already had (contract-stream snapshot-first), with
// the same D9/D10 discipline the pending-input store enforces:
//
//	session.status.snapshot_begin    {workspace_id, snapshot_id}
//	session.status.snapshot          {workspace_id, snapshot_id,
//	                                  data: {cursor, sessions: [...rows]}}
//	session.status.snapshot_complete {workspace_id, snapshot_id, snapshot_ok}
//
// The authority is the pod's statusz — the same surface
// GetAuthoritativeActiveSessions consults (proxy_connections.go). An
// authority failure reads ok=false ("keep your state"), NEVER an
// authoritative empty set: a statusz blip mid-busy must hold unknown,
// not flip the indicator to idle.

// busySnapshotRow is one session's derived truth on the snapshot frame.
// SessionID is the PHRASE (the id space the frontend joins on since the
// #1756/#1775/#1806 re-keys — the #1821 piece-1 discipline); the
// harness id rides additively. A row the index cannot resolve keeps the
// harness id, flagged deprecated (never worse than the pre-fix wire).
type busySnapshotRow struct {
	SessionID           string `json:"session_id"`
	InternalSessionID   string `json:"internal_session_id,omitempty"`
	Status              string `json:"status"`
	DerivedBusy         bool   `json:"derived_busy"`
	SessionIDDeprecated bool   `json:"session_id_deprecated,omitempty"`
}

// statuszSessionRow is the raw statusz answer for one session.
type statuszSessionRow struct {
	ID     string
	Status string
}

// busySnapshotPayload is the session.status.snapshot frame's typed
// payload: the broker cursor (EventID the rows were read at) and the
// authority's per-session rows.
type busySnapshotPayload struct {
	Cursor   uint64            `json:"cursor"`
	Sessions []busySnapshotRow `json:"sessions"`
}

// fetchStatuszSessions reads the workspace pod's statusz and returns
// its per-session rows. ok=false means the authority could not be
// consulted (workspace not Active, resolve failure, transport failure,
// non-200, decode failure) — callers must treat the busy set as
// UNKNOWN, never empty. This is the ok-typed twin of
// GetAuthoritativeActiveSessions' set collapse (which correctly fails
// open for send-gating; an indicator must not).
func (h *ProxyHandler) fetchStatuszSessions(ctx context.Context, workspaceID string) ([]statuszSessionRow, bool) {
	if h.k8sClient == nil {
		return nil, false
	}
	v1Client, v1Err := h.k8sClient.LlmsafespacesV1()
	if v1Err != nil || v1Client == nil {
		return nil, false
	}
	ws, wsErr := v1Client.Workspaces(h.namespace).Get(ctx, workspaceID, metav1.GetOptions{})
	if wsErr != nil || ws == nil || ws.Status.Phase != phaseActive || ws.Status.PodIP == "" {
		return nil, false
	}

	password, pwErr := h.getPassword(ctx, workspaceID)
	if pwErr != nil {
		return nil, false
	}

	statuszCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	url := fmt.Sprintf("http://%s:%d/v1/statusz", ws.Status.PodIP, agentd.AgentdAdminPort) //nolint:gosec // G107: internal pod
	bearers := h.adminBearerCandidates(statuszCtx, workspaceID, password)
	resp, err := GetWithBearers(statuszCtx, h.httpClient, url, bearers)
	if err != nil {
		h.logger.Debug("busy snapshot: statusz unavailable",
			"workspaceID", workspaceID, "error", err)
		return nil, false
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, false
	}

	var statusz agentd.StatuszResponse
	// 1 MB cap — same bound as GetAuthoritativeActiveSessions (a
	// many-session statusz body exceeds the old 16 KB cap).
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&statusz); err != nil {
		h.logger.Debug("busy snapshot: failed to decode statusz",
			"workspaceID", workspaceID, "error", err)
		return nil, false
	}

	// #1848 r0 finding 4: a 200 whose session listing FAILED is not an
	// authoritative empty set — agentd swallows ListSessions errors and
	// caches the empty slice (session_tracker cachedState). The
	// sessions_ok signal carries the verdict; absent (older pods)
	// reads ok — never worse than the pre-signal behavior.
	if statusz.SessionsOK != nil && !*statusz.SessionsOK {
		h.logger.Debug("busy snapshot: statusz session list not authoritative",
			"workspaceID", workspaceID)
		return nil, false
	}

	rows := make([]statuszSessionRow, 0, len(statusz.Sessions))
	for _, sess := range statusz.Sessions {
		rows = append(rows, statuszSessionRow{ID: sess.ID, Status: sess.Status})
	}
	return rows, true
}

// busyFlightsInFlight coalesces concurrent ON-DEMAND busy-snapshot
// flights per workspace (#1848 r0 robustness, rescoped r2 finding 2):
// a second POST while one POST-triggered flight is running is a no-op
// — that flight's markers are still in flight and will answer the
// second caller on the shared user stream. The CONNECT path is
// deliberately EXEMPT: snapshotUserWorkspaces runs it per subscriber
// connect, a mid-flight joiner has no backlog (replay needs
// Last-Event-ID), and coalescing there would leave the new subscriber
// with no authoritative seed — the class-1/class-2 window this flight
// exists to close.
var busyFlightsInFlight sync.Map // workspaceID → struct{}

// emitSessionStatusSnapshot runs one busy-snapshot flight for the
// workspace on the user stream — the D9/D10 template from
// emitPendingInputRequests (proxy_input.go): announce the flight, fetch
// the authority, publish the rows, and close the flight with
// snapshot_ok unconditionally (a failed fetch is a non-authoritative
// marker; clients keep their state).
//
// The cursor (the broker's last event id, read BEFORE the authority
// fetch) orders the snapshot against concurrent session.status deltas:
// a delta published after the cursor read has id > cursor and reflects
// state at least as fresh as the fetch, so the client lets it survive
// the commit (#1821 piece 3).
func (h *ProxyHandler) emitSessionStatusSnapshot(ctx context.Context, workspaceID string) {
	ok := false

	flightID := fmt.Sprintf("%s-%d", workspaceID, time.Now().UnixNano())

	defer func() {
		if h.userBroker == nil {
			return
		}
		if userID := h.userBroker.WorkspaceOwner(workspaceID); userID != "" {
			h.userBroker.PublishToUser(userID, apitypes.WorkspaceSSEEvent{
				Type:        "session.status.snapshot_complete",
				WorkspaceID: workspaceID,
				SnapshotOK:  &ok,
				SnapshotID:  flightID,
			})
		}
	}()

	if h.userBroker == nil {
		return
	}
	userID := h.userBroker.WorkspaceOwner(workspaceID)
	if userID == "" {
		return
	}

	// Read the cursor before consulting the authority: any delta
	// published from here on carries id > cursor and outranks the
	// snapshot at commit time (the client keeps entries confirmed by a
	// newer delta; the snapshot governs only what it supersedes).
	cursor := h.userBroker.LastEventID(userID)

	h.userBroker.PublishToUser(userID, apitypes.WorkspaceSSEEvent{
		Type:        "session.status.snapshot_begin",
		WorkspaceID: workspaceID,
		SnapshotID:  flightID,
	})

	flightCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	rawRows, fetched := h.fetchStatuszSessions(flightCtx, workspaceID)
	ok = fetched
	if !fetched {
		return
	}

	rows := make([]busySnapshotRow, 0, len(rawRows))
	for _, raw := range rawRows {
		row := busySnapshotRow{
			SessionID:   raw.ID,
			Status:      raw.Status,
			DerivedBusy: raw.Status != "idle",
		}
		if !row.DerivedBusy {
			// Idle rows never latch client-side and never join against
			// latched entries (removal is keyed by the LATCHED session's
			// id, not the idle row's) — skip the Register seam for them.
			// #1848 r0 robustness: on an LRU miss Register is a DB
			// EnsureSessionRegistered; a cold replica with thousands of
			// idle sessions would otherwise burn the 5s flight budget
			// translating rows nobody keys on, degrading the busy tail.
			rows = append(rows, row)
			continue
		}
		// The #1821 piece-1 ID discipline on the new surface: key the
		// row by the phrase, harness id additive. Register is the
		// emission-surface seam (idempotent, LRU-cached after first
		// sight); an unresolved session degrades to the harness id,
		// flagged deprecated — never worse than the pre-fix wire.
		regCtx, regCancel := context.WithTimeout(flightCtx, 2*time.Second)
		phrase := ""
		if h.sessionIndex != nil {
			phrase = h.sessionIndex.Register(regCtx, workspaceID, raw.ID, "")
		}
		regCancel()
		if phrase != "" && phrase != raw.ID {
			row.SessionID = phrase
			row.InternalSessionID = raw.ID
		} else {
			row.SessionIDDeprecated = true
		}
		rows = append(rows, row)
	}

	h.userBroker.PublishToUser(userID, apitypes.WorkspaceSSEEvent{
		Type:        "session.status.snapshot",
		WorkspaceID: workspaceID,
		SnapshotID:  flightID,
		Data: busySnapshotPayload{
			Cursor:   cursor,
			Sessions: rows,
		},
	})
}

// RequestSessionStatusSnapshot triggers a busy-snapshot flight for the
// workspace — the RequestInputSnapshot pattern (proxy_input.go): the
// request returns 202 immediately and the flight's events reach the
// caller on the user-event stream. #1821 piece 4: this is the frontend
// lease's authoritative recheck surface — a client whose busy lease
// expired (no confirming delta for L) re-requests the snapshot instead
// of guessing, and the D10 marker decides keep-vs-clear.
func (h *ProxyHandler) RequestSessionStatusSnapshot(c *gin.Context) {
	workspaceID := c.Param("id")
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace ID required"})
		return
	}
	detached := context.WithoutCancel(c.Request.Context()) //nolint:contextcheck // intentionally detached — the snapshot outlives the request
	// Detached context: the flight (≤5s) must outlive this request,
	// which returns immediately. The flight's events reach the caller
	// via the user-event stream, not this response. The coalescing
	// guard lives HERE (the on-demand trigger), not in the emitter —
	// the connect path must always emit its own per-subscriber flight.
	if _, loaded := busyFlightsInFlight.LoadOrStore(workspaceID, struct{}{}); !loaded {
		go func() {
			defer busyFlightsInFlight.Delete(workspaceID)
			h.emitSessionStatusSnapshot(detached, workspaceID)
		}()
	}
	c.Status(http.StatusAccepted)
}

// RequestSessionStatusSnapshotForTest drives the on-demand trigger
// synchronously (the POST handler's coalescing guard + detached
// goroutine, minus the HTTP ceremony) — the seam the coalescing and
// orphan-subscriber tests use.
func (h *ProxyHandler) RequestSessionStatusSnapshotForTest(workspaceID string) {
	if _, loaded := busyFlightsInFlight.LoadOrStore(workspaceID, struct{}{}); !loaded {
		defer busyFlightsInFlight.Delete(workspaceID)
		h.emitSessionStatusSnapshot(context.Background(), workspaceID)
	}
}
