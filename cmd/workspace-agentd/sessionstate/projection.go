// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate

import (
	"time"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"google.golang.org/protobuf/proto"
)

// SessionSeed is the store-truth seed for one session at reseed time
// (I3/I4): the authoritative status plus the pending inputs the store
// still holds. Busy/streaming and in-flight parts are NEVER seeded — they
// are live-turn state, rebuilt by events after the reseed.
type SessionSeed struct {
	Status        abiv1.SessionStatus
	PendingInputs []*abiv1.InputRequest
}

// SessionView is one session's projected state (I12: a snapshot of these
// fields alone renders the session).
type SessionView struct {
	Status abiv1.SessionStatus
	// Busy is the DERIVED #1574 busy truth (streaming || in-flight
	// parts || queue depth) — not the raw streaming flag. Components
	// carry the why.
	Busy           bool
	BusyComponents *abiv1.BusyComponents
	InFlightParts  []*abiv1.Part
	PendingInputs  []*abiv1.InputRequest
}

// sessionView is the internal mutable projection record. lastBusySeq is
// the seq of the event that last marked the session busy — the freshness
// gate for #1311's evidence-driven busy-clear (a busy-mark newer than the
// evidence read survives the pass).
type sessionRecord struct {
	status abiv1.SessionStatus
	busy   bool
	// busySince stamps the busy-mark's wall clock — the lease-window gate
	// for #1310 slice B's harness-side status re-derivation (a fresh
	// busy-mark holds; past the bound, harness truth wins).
	busySince time.Time
	title     string
	inFly     []*abiv1.Part
	pending   map[string]*abiv1.InputRequest
	// pendingSince/resolvedSeq carry each ask's last fold seq — the
	// lease diff's staleness gate: a gather's truth is only applied to
	// entries whose fold predates the gather (r5 findings 2-3).
	pendingSince map[string]uint64
	resolvedSeq  map[string]uint64
	lastBusySeq  uint64
}

func newSessionRecord(status abiv1.SessionStatus) *sessionRecord {
	return &sessionRecord{status: status, pending: map[string]*abiv1.InputRequest{}}
}

// markBusy stamps the busy flag with the marking event's seq (the
// evidence-freshness gate for the reconcile busy-clear).
func (r *sessionRecord) markBusy(seq uint64) {
	r.busy = true
	r.busySince = time.Now()
	r.lastBusySeq = seq
}

// view returns a PRIVATE copy: views (and the snapshots built from them)
// are serialized outside the lock — sharing projection pointers would race
// live mutations against in-flight sends.
func (r *sessionRecord) view() *SessionView {
	v := &SessionView{Status: r.status, Busy: r.busy, InFlightParts: make([]*abiv1.Part, len(r.inFly))}
	for i, p := range r.inFly {
		v.InFlightParts[i] = proto.Clone(p).(*abiv1.Part)
	}
	v.PendingInputs = make([]*abiv1.InputRequest, 0, len(r.pending))
	for _, in := range r.pending {
		v.PendingInputs = append(v.PendingInputs, proto.Clone(in).(*abiv1.InputRequest))
	}
	return v
}

func (r *sessionRecord) partIndex(id string) int {
	for i, p := range r.inFly {
		if p.GetId() == id {
			return i
		}
	}
	return -1
}

// applyContractLocked is the contract-state state machine: every dialect
// detail has already been translated away; this fold is the sole place
// projection state changes (I1: under the authority lock).
func (a *Authority) applyContractLocked(evt *abiv1.Event) {
	if evt.SessionId == "" && evt.GetSession() == nil {
		return
	}
	sid := evt.SessionId
	if sid == "" {
		sid = evt.GetSession().GetId()
	}
	if sid == "" {
		return
	}
	rec := a.sessions[sid]
	if rec == nil {
		rec = newSessionRecord(abiv1.SessionStatus_SESSION_STATUS_UNKNOWN)
		a.sessions[sid] = rec
	}

	switch evt.Type {
	case abiv1.EventType_EVENT_TYPE_SESSION_STATUS:
		rec.status = evt.Status
		switch evt.Status {
		case abiv1.SessionStatus_SESSION_STATUS_BUSY:
			rec.markBusy(a.seq)
		case abiv1.SessionStatus_SESSION_STATUS_IDLE, abiv1.SessionStatus_SESSION_STATUS_ERROR:
			// ERROR ends the turn exactly as IDLE does (the ERROR event
			// case below already cleared busy; the status arrival must
			// too, or the snapshot's busy override keeps reporting BUSY
			// forever — r5 finding 1).
			rec.busy = false
			rec.inFly = nil
		}
	case abiv1.EventType_EVENT_TYPE_SESSION_UPDATED:
		if s := evt.GetSession(); s != nil {
			if s.GetId() != "" {
				sid = s.GetId()
				if sid != evt.SessionId {
					if target := a.sessions[sid]; target != nil {
						rec = target
					} else {
						rec = newSessionRecord(abiv1.SessionStatus_SESSION_STATUS_UNKNOWN)
						a.sessions[sid] = rec
					}
				}
			}
			rec.title = s.GetTitle()
			// Session cost presence is display-only (Epic 33 consumers
			// read cost from message events); the transcript itself stays
			// on the adapter path (I12 stitch by ID).
		}
	case abiv1.EventType_EVENT_TYPE_MESSAGE_START:
		rec.markBusy(a.seq)
		if m := evt.GetMessage(); m != nil {
			for _, p := range m.GetParts() {
				a.upsertPartLocked(rec, p)
			}
		}
	case abiv1.EventType_EVENT_TYPE_MESSAGE_END:
		if m := evt.GetMessage(); m != nil {
			for _, p := range m.GetParts() {
				a.upsertPartLocked(rec, p)
			}
		}
		// The turn ends when the status says so; MESSAGE_END alone keeps
		// parts renderable (completed, in place).
	case abiv1.EventType_EVENT_TYPE_PART_START:
		rec.markBusy(a.seq)
		if p := evt.GetPart(); p != nil {
			a.upsertPartLocked(rec, p)
		}
	case abiv1.EventType_EVENT_TYPE_PART_DELTA:
		pid := evt.PartId
		if p := evt.GetPart(); pid == "" && p != nil {
			pid = p.GetId()
		}
		if i := rec.partIndex(pid); i >= 0 {
			if t := rec.inFly[i].GetText(); t != "" || evt.Delta != "" {
				rec.inFly[i].Payload = &abiv1.Part_Text{Text: t + evt.Delta}
			}
		} else if pid != "" {
			rec.inFly = append(rec.inFly, &abiv1.Part{Id: pid, Type: abiv1.PartType_PART_TYPE_TEXT,
				Payload: &abiv1.Part_Text{Text: evt.Delta}})
		}
	case abiv1.EventType_EVENT_TYPE_PART_END:
		if p := evt.GetPart(); p != nil {
			a.upsertPartLocked(rec, p)
		} else if i := rec.partIndex(evt.PartId); i >= 0 {
			rec.inFly = append(rec.inFly[:i], rec.inFly[i+1:]...)
		}
	case abiv1.EventType_EVENT_TYPE_INPUT_REQUEST:
		if in := evt.GetInput(); in != nil && in.GetId() != "" {
			rec.pending[in.GetId()] = proto.Clone(in).(*abiv1.InputRequest)
			if rec.pendingSince == nil {
				rec.pendingSince = map[string]uint64{}
			}
			rec.pendingSince[in.GetId()] = a.seq
		}
	case abiv1.EventType_EVENT_TYPE_INPUT_RESOLVED:
		if in := evt.GetInput(); in != nil {
			delete(rec.pending, in.GetId())
			if rec.resolvedSeq == nil {
				rec.resolvedSeq = map[string]uint64{}
			}
			rec.resolvedSeq[in.GetId()] = a.seq
		}
	case abiv1.EventType_EVENT_TYPE_ERROR:
		// A failed step clears busy (the 2026-08-15 orphaned-busy class):
		// the turn is over; the store will confirm on the next reseed.
		rec.busy = false
		if evt.GetError() != nil {
			rec.status = abiv1.SessionStatus_SESSION_STATUS_ERROR
		}
	}
}

// upsertPartLocked stores a PRIVATE clone: the event object is also
// referenced by the fanout frame (serialized outside the lock by the
// Events handler) — retaining the shared pointer would race later
// mutations (PART_DELTA) against in-flight sends. Caught by the S1 shadow
// harness under -race. A CUSTOM part application also bumps the
// custom-valve counter (the retired API-side unknown-taxonomy signal's
// agentd successor: extension kinds flowing through the valve, counted at
// the projection — the sole place parts are applied).
func (a *Authority) upsertPartLocked(rec *sessionRecord, p *abiv1.Part) {
	if p == nil || p.GetId() == "" {
		return
	}
	if p.GetType() == abiv1.PartType_PART_TYPE_CUSTOM {
		a.customValveEvents++
	}
	clone := proto.Clone(p).(*abiv1.Part)
	if i := rec.partIndex(p.GetId()); i >= 0 {
		rec.inFly[i] = clone
		return
	}
	rec.inFly = append(rec.inFly, clone)
}

// seedLocked rebuilds one session's record from store truth.
func seedLocked(seed SessionSeed) *sessionRecord {
	rec := newSessionRecord(seed.Status)
	for _, in := range seed.PendingInputs {
		if in != nil && in.GetId() != "" {
			rec.pending[in.GetId()] = in
		}
	}
	return rec
}

// deriveBusyComponents is the #1574 single busy definition, computed
// here once and served to every view: busy ⇔ autonomous progress
// pending. Streaming (the status-event busy-mark), tool parts running
// or queued, and ledger deliveries in flight all count; pending
// QUESTION/PERMISSION asks are the owner's carve-out — blocked on the
// USER, never busy, surfaced via pending_inputs as their own signal.
// Two definitions of busy is how the #1573 tracker/projection
// divergence happened; this is the only one.
func deriveBusyComponents(streaming bool, parts, queue, pendingUser int32) *abiv1.BusyComponents {
	return &abiv1.BusyComponents{
		Streaming:         streaming,
		InFlightParts:     parts,
		QueueDepth:        queue,
		PendingUserInputs: pendingUser,
		Busy:              streaming || parts > 0 || queue > 0,
	}
}

// enrichBusyLocked overlays the derived busy truth onto a record's
// view (State() and sessionSnapshotLocked share this — one definition,
// both surfaces).
func (a *Authority) enrichBusyLocked(id string, v *SessionView) {
	var queue int32
	if a.ledger != nil {
		queue = int32(a.ledger.queueDepth(id)) //nolint:gosec // G115: bounded by delivery rate limits
	}
	comp := deriveBusyComponents(v.Busy, int32(len(v.InFlightParts)), queue, //nolint:gosec // G115: part count bounded by admission
		pendingUserInputsOf(v.PendingInputs))
	// The terminal veto (#1578 r3): an errored session does nothing
	// autonomously — EVENT_TYPE_ERROR deliberately leaves its parts in
	// the record (renderable), and those orphans must never flip the
	// status back to BUSY (the mask was unbounded: the reconcile sweep
	// skips busy==false records, so nothing cleared it but a reseed).
	// Components still report the residual parts as data; only the
	// busy/status flip is vetoed. A new turn re-marks via status events.
	if v.Status == abiv1.SessionStatus_SESSION_STATUS_ERROR {
		comp.Busy = false
	}
	v.Busy = comp.GetBusy()
	v.BusyComponents = comp
	// The derived truth flips the rendered status on EVERY surface (the
	// #1574 incident shape: harness IDLE while a tool runs renders BUSY
	// in State() exactly as in the snapshots). The carve-out is
	// one-directional: a pending ask never flips busy by itself.
	if comp.GetBusy() {
		v.Status = abiv1.SessionStatus_SESSION_STATUS_BUSY
	}
}

// pendingUserInputsOf counts the carve-out asks on a rendered view.
func pendingUserInputsOf(pending []*abiv1.InputRequest) int32 {
	var n int32
	for _, in := range pending {
		switch in.GetKind() {
		case abiv1.InputKind_INPUT_KIND_QUESTION, abiv1.InputKind_INPUT_KIND_PERMISSION:
			n++
		}
	}
	return n
}

// sessionSnapshotLocked renders one session's I12-complete snapshot:
// status (busy-aware), in-flight parts with partials, pending inputs.
// Queue depth is ledger-derived and lands with US-69.7.
func (a *Authority) sessionSnapshotLocked(id string, rec *sessionRecord) *abiv1.SessionSnapshot {
	v := rec.view()
	a.enrichBusyLocked(id, v)
	snap := &abiv1.SessionSnapshot{
		SessionId:     id,
		Status:        v.Status,
		InFlightParts: v.InFlightParts,
		PendingInputs: v.PendingInputs,
	}
	// #1574: busy-from-data. The derived truth (not the raw status
	// event) flips the rendered status — the incident's shape (harness
	// IDLE while a tool runs) renders BUSY here. The carve-out is
	// one-directional: a pending ask never flips busy by itself.
	if v.BusyComponents.GetBusy() {
		snap.Status = abiv1.SessionStatus_SESSION_STATUS_BUSY
	}
	snap.Busy = v.BusyComponents
	if a.ledger != nil {
		snap.QueueDepth = int32(a.ledger.queueDepth(id)) //nolint:gosec // G115: bounded by delivery rate limits
	}
	return snap
}

// podSnapshotsLocked renders the pod-wide snapshot payload.
func (a *Authority) podSnapshotsLocked() []*abiv1.SessionSnapshot {
	out := make([]*abiv1.SessionSnapshot, 0, len(a.sessions))
	for id, rec := range a.sessions {
		out = append(out, a.sessionSnapshotLocked(id, rec))
	}
	return out
}
