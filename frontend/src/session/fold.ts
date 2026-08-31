// The client side of the stamped-snapshot sync protocol (design 0055 M1,
// US-69.10): the TS port of pkg/abi/abiclient's reference fold. The rule
// exists exactly once per language — this port must stay semantically
// identical to the Go reference (client_discard_rule property tests pin
// the equivalence):
//
//   - keep the last-seen seq;
//   - a snapshot stamped atSeq=S replaces session state and sets seq=S;
//   - events apply in order; events with seq ≤ S are discarded.
//
// There is no replay buffer and no since-cursor by design; reconnect
// mid-turn re-fetches in-flight state via a fresh stamped snapshot.

import { create } from "@bufbuild/protobuf";
import {
  SessionSnapshotSchema,
  type SequencedEvent,
  type SessionSnapshot,
  type SnapshotFrame,
} from "../abi/llmsafespaces/abi/v1/abi_pb";
import { EventType, PartSchema, SessionStatus, type Part } from "../abi/llmsafespaces/abi/v1/contract_pb";

export interface SessionFold {
  seq: bigint;
  sessions: Map<string, SessionSnapshot>;
}

export function newFold(): SessionFold {
  return { seq: 0n, sessions: new Map() };
}

/** snapshot@S replaces the sessions it carries and sets seq=S (merge per
 * session — sessions absent from the snapshot are retained, mirroring the
 * Go reference). The incoming wire objects are cloned; the fold never
 * aliases them. */
export function applySnapshot(fold: SessionFold, snap: SnapshotFrame): void {
  fold.seq = snap.atSeq;
  for (const s of snap.snapshot?.sessions ?? []) {
    fold.sessions.set(s.sessionId, cloneSessionSnapshot(s));
  }
}

/** Applies one sequenced event under the client discard rule. Returns
 * true when the event was applied (seq advanced), false when discarded
 * (seq ≤ last-seen, or no event payload). */
export function applySequenced(fold: SessionFold, seqed: SequencedEvent): boolean {
  if (seqed.seq <= fold.seq) return false; // discard ≤ S (duplicate direction)
  fold.seq = seqed.seq;
  const evt = seqed.event;
  if (!evt) return true;

  const sid = evt.sessionId;
  let snap = fold.sessions.get(sid);
  if (!snap) {
    // A session observed before any status event is UNKNOWN — the same
    // convention as the server projection (never UNSPECIFIED: the zero
    // value is not a valid status).
    snap = create(SessionSnapshotSchema, {
      sessionId: sid,
      status: SessionStatus.UNKNOWN,
    });
    fold.sessions.set(sid, snap);
  }

  switch (evt.type) {
    case EventType.SESSION_STATUS: {
      snap.status = evt.status;
      if (evt.status === SessionStatus.IDLE) {
        // Turn over: the server projection clears in-flight parts on
        // idle — the fold must mirror it or reconnects show stale parts.
        snap.inFlightParts = [];
      }
      break;
    }
    case EventType.INPUT_REQUEST: {
      if (evt.input) snap.pendingInputs = [...snap.pendingInputs, evt.input];
      break;
    }
    case EventType.INPUT_RESOLVED: {
      if (evt.input) {
        snap.pendingInputs = snap.pendingInputs.filter((p) => p.id !== evt.input!.id);
      }
      break;
    }
    case EventType.PART_START:
    case EventType.PART_END: {
      if (evt.part) upsertPart(snap, evt.part);
      break;
    }
    case EventType.PART_DELTA: {
      // PART_DELTA: append to the part's text payload (the projection
      // builds streaming text this way; deltas target text parts).
      const pid = evt.partId;
      if (pid) {
        snap.inFlightParts = snap.inFlightParts.map((p) =>
          p.id === pid
            ? { ...p, payload: { case: "text", value: partText(p) + evt.delta! } }
            : p,
        );
      }
      break;
    }
  }
  return true;
}

/** A structural copy safe to hand to consumers or to seed the fold from
 * wire objects: containers are copied and part payloads are detached —
 * later folds replace entries (never mutate them). */
export function cloneSessionSnapshot(s: SessionSnapshot): SessionSnapshot {
  return create(SessionSnapshotSchema, {
    sessionId: s.sessionId,
    status: s.status,
    inFlightParts: s.inFlightParts.map(clonePart),
    queueDepth: s.queueDepth,
    pendingInputs: s.pendingInputs.map((i) => ({ ...i })),
  });
}

function clonePart(p: Part): Part {
  return create(PartSchema, {
    ...p,
    payload: p.payload.case ? { ...p.payload } : p.payload,
  });
}

function upsertPart(snap: SessionSnapshot, p: Part): void {
  const idx = snap.inFlightParts.findIndex((existing) => existing.id === p.id);
  if (idx >= 0) {
    snap.inFlightParts[idx] = p;
    return;
  }
  snap.inFlightParts = [...snap.inFlightParts, p];
}

function partText(p: Part): string {
  return p.payload.case === "text" ? p.payload.value : "";
}
