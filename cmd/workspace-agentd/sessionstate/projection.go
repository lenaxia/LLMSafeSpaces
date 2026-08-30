// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate

import (
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
	Status        abiv1.SessionStatus
	Busy          bool
	InFlightParts []*abiv1.Part
	PendingInputs []*abiv1.InputRequest
}

// sessionView is the internal mutable projection record.
type sessionRecord struct {
	status  abiv1.SessionStatus
	busy    bool
	title   string
	inFly   []*abiv1.Part
	pending map[string]*abiv1.InputRequest
}

func newSessionRecord(status abiv1.SessionStatus) *sessionRecord {
	return &sessionRecord{status: status, pending: map[string]*abiv1.InputRequest{}}
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
			rec.busy = true
		case abiv1.SessionStatus_SESSION_STATUS_IDLE:
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
		rec.busy = true
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
		rec.busy = true
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
		}
	case abiv1.EventType_EVENT_TYPE_INPUT_RESOLVED:
		if in := evt.GetInput(); in != nil {
			delete(rec.pending, in.GetId())
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
// harness under -race.
func (a *Authority) upsertPartLocked(rec *sessionRecord, p *abiv1.Part) {
	if p == nil || p.GetId() == "" {
		return
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

// sessionSnapshotLocked renders one session's I12-complete snapshot:
// status (busy-aware), in-flight parts with partials, pending inputs.
// Queue depth is ledger-derived and lands with US-69.7.
func sessionSnapshotLocked(id string, rec *sessionRecord) *abiv1.SessionSnapshot {
	v := rec.view()
	snap := &abiv1.SessionSnapshot{
		SessionId:     id,
		Status:        v.Status,
		InFlightParts: v.InFlightParts,
		PendingInputs: v.PendingInputs,
	}
	if v.Busy {
		snap.Status = abiv1.SessionStatus_SESSION_STATUS_BUSY
	}
	return snap
}

// podSnapshotsLocked renders the pod-wide snapshot payload.
func podSnapshotsLocked(sessions map[string]*sessionRecord) []*abiv1.SessionSnapshot {
	out := make([]*abiv1.SessionSnapshot, 0, len(sessions))
	for id, rec := range sessions {
		out = append(out, sessionSnapshotLocked(id, rec))
	}
	return out
}
