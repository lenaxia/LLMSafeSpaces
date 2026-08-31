// The client discard rule (US-69.10, design 0055 I12): unit pins for the
// fold semantics plus the property test proving random snapshot/event
// interleavings (with duplicates and stale re-deliveries) converge to the
// server fold — the TS port of abiclient's TestDiscardRulePropertyFuzz.

import { describe, it, expect } from "vitest";
import { create } from "@bufbuild/protobuf";
import { fromJson } from "@bufbuild/protobuf";
import {
  StreamFrameSchema,
  SnapshotFrameSchema,
  SequencedEventSchema,
  type SnapshotFrame,
  type SequencedEvent,
  type SessionSnapshot,
} from "../abi/llmsafespaces/abi/v1/abi_pb";
import {
  EventSchema,
  EventType,
  InputRequestSchema,
  PartSchema,
  SessionStatus,
  type Event,
  type InputRequest,
  type Part,
} from "../abi/llmsafespaces/abi/v1/contract_pb";
import { applySequenced, applySnapshot, cloneSessionSnapshot, newFold } from "./fold";

function ev(partial: Partial<Event>): Event {
  return create(EventSchema, {
    sessionId: "s0",
    ...partial,
  } as Parameters<typeof create>[1]);
}

function seq(n: number, event?: Event): SequencedEvent {
  return create(SequencedEventSchema, { seq: BigInt(n), event });
}

function snapshotFrame(atSeq: number, sessions: Array<Pick<SessionSnapshot, "sessionId" | "status" | "inFlightParts" | "queueDepth" | "pendingInputs">>): SnapshotFrame {
  // JSON round-trip: mirrors the parsed-JSON body the SSE transport
  // hands fromJson.
  return fromJson(SnapshotFrameSchema, JSON.parse(JSON.stringify({
    atSeq: String(atSeq),
    snapshot: {
      sessions: sessions.map((s) => ({
        sessionId: s.sessionId,
        status: statusJson(s.status),
        inFlightParts: s.inFlightParts.map(partToJson),
        queueDepth: s.queueDepth,
        pendingInputs: s.pendingInputs.map(inputToJson),
      })),
    },
  })));
}

function partToJson(p: Part): unknown {
  const base: Record<string, unknown> = { id: p.id, type: partTypeJson(p.type) };
  switch (p.payload.case) {
    case "text":
      base.text = p.payload.value;
      break;
    case "reasoning":
      base.reasoning = p.payload.value;
      break;
    case "tool":
      base.tool = {
        callId: p.payload.value.callId,
        name: p.payload.value.name,
        state: p.payload.value.state ? { status: p.payload.value.state.status } : undefined,
      };
      break;
  }
  return base;
}

function partTypeJson(t: number): string {
  switch (t) {
    case 1: return "PART_TYPE_TEXT";
    case 2: return "PART_TYPE_REASONING";
    case 3: return "PART_TYPE_TOOL";
    case 4: return "PART_TYPE_FILE_CHANGE";
    case 5: return "PART_TYPE_CUSTOM";
    default: return "PART_TYPE_UNSPECIFIED";
  }
}

function inputToJson(i: InputRequest): unknown {
  return { id: i.id, sessionId: i.sessionId, kind: i.kind === 2 ? "INPUT_KIND_PERMISSION" : "INPUT_KIND_QUESTION" };
}

function statusJson(s: number): string {
  const names = ["SESSION_STATUS_UNSPECIFIED", "SESSION_STATUS_UNKNOWN", "SESSION_STATUS_IDLE",
    "SESSION_STATUS_BUSY", "SESSION_STATUS_ERROR", "SESSION_STATUS_COMPACTING", "SESSION_STATUS_ARCHIVED"];
  return names[s] ?? "SESSION_STATUS_UNSPECIFIED";
}

function textPart(id: string, text: string): Part {
  return create(PartSchema, { id, type: 1, payload: { case: "text", value: text } });
}

function inputReq(id: string, sessionId = "s0"): InputRequest {
  return create(InputRequestSchema, { id, sessionId });
}

describe("fold: snapshot rule", () => {
  it("snapshot@S replaces carried sessions and sets seq=S", () => {
    const fold = newFold();
    applySnapshot(fold, snapshotFrame(7, [
      { sessionId: "s0", status: SessionStatus.BUSY, inFlightParts: [textPart("p1", "hi")], queueDepth: 2, pendingInputs: [] },
    ]));
    expect(fold.seq).toBe(7n);
    expect(fold.sessions.get("s0")?.status).toBe(SessionStatus.BUSY);
    expect(fold.sessions.get("s0")?.inFlightParts).toHaveLength(1);
    expect(fold.sessions.get("s0")?.queueDepth).toBe(2);
  });

  it("snapshot clones wire objects — later mutation of the input does not leak", () => {
    const frame = snapshotFrame(3, [
      { sessionId: "s0", status: SessionStatus.IDLE, inFlightParts: [textPart("p1", "a")], queueDepth: 0, pendingInputs: [] },
    ]);
    const fold = newFold();
    applySnapshot(fold, frame);
    frame.snapshot!.sessions[0]!.inFlightParts[0]!.payload = { case: "text", value: "MUTATED" };
    expect(fold.sessions.get("s0")?.inFlightParts[0]?.payload).toEqual({ case: "text", value: "a" });
  });

  it("snapshot merge retains sessions it does not carry (Go-reference parity)", () => {
    const fold = newFold();
    applySnapshot(fold, snapshotFrame(5, [
      { sessionId: "s0", status: SessionStatus.IDLE, inFlightParts: [], queueDepth: 0, pendingInputs: [] },
      { sessionId: "s1", status: SessionStatus.BUSY, inFlightParts: [], queueDepth: 0, pendingInputs: [] },
    ]));
    applySnapshot(fold, snapshotFrame(9, [
      { sessionId: "s0", status: SessionStatus.BUSY, inFlightParts: [], queueDepth: 0, pendingInputs: [] },
    ]));
    expect(fold.seq).toBe(9n);
    expect(fold.sessions.get("s0")?.status).toBe(SessionStatus.BUSY);
    expect(fold.sessions.get("s1")?.status).toBe(SessionStatus.BUSY);
  });
});

describe("fold: discard rule", () => {
  it("events with seq ≤ S are discarded; seq > S apply", () => {
    const fold = newFold();
    applySnapshot(fold, snapshotFrame(10, []));
    expect(applySequenced(fold, seq(10, ev({ type: EventType.SESSION_STATUS, status: SessionStatus.BUSY })))).toBe(false);
    expect(applySequenced(fold, seq(9, ev({ type: EventType.SESSION_STATUS, status: SessionStatus.BUSY })))).toBe(false);
    expect(applySequenced(fold, seq(11, ev({ type: EventType.SESSION_STATUS, status: SessionStatus.BUSY })))).toBe(true);
    expect(fold.seq).toBe(11n);
    expect(fold.sessions.get("s0")?.status).toBe(SessionStatus.BUSY);
  });

  it("events with no payload still advance seq but change nothing", () => {
    const fold = newFold();
    applySequenced(fold, seq(4));
    expect(fold.seq).toBe(4n);
    expect(fold.sessions.size).toBe(0);
  });

  it("duplicate delivery (same seq twice) applies once", () => {
    const fold = newFold();
    const e = ev({ type: EventType.INPUT_REQUEST, input: inputReq("q1") });
    expect(applySequenced(fold, seq(1, e))).toBe(true);
    expect(applySequenced(fold, seq(1, e))).toBe(false);
    expect(fold.sessions.get("s0")?.pendingInputs).toHaveLength(1);
  });
});

describe("fold: per-event semantics", () => {
  it("unknown session materializes as UNKNOWN status", () => {
    const fold = newFold();
    applySequenced(fold, seq(1, ev({ type: EventType.PART_START, part: textPart("p1", "x") })));
    expect(fold.sessions.get("s0")?.status).toBe(SessionStatus.UNKNOWN);
  });

  it("IDLE clears in-flight parts", () => {
    const fold = newFold();
    applySequenced(fold, seq(1, ev({ type: EventType.PART_START, part: textPart("p1", "x") })));
    applySequenced(fold, seq(2, ev({ type: EventType.SESSION_STATUS, status: SessionStatus.IDLE })));
    expect(fold.sessions.get("s0")?.inFlightParts).toHaveLength(0);
    expect(fold.sessions.get("s0")?.status).toBe(SessionStatus.IDLE);
  });

  it("INPUT_REQUEST appends; INPUT_RESOLVED removes by id", () => {
    const fold = newFold();
    applySequenced(fold, seq(1, ev({ type: EventType.INPUT_REQUEST, input: inputReq("q1") })));
    applySequenced(fold, seq(2, ev({ type: EventType.INPUT_REQUEST, input: inputReq("q2") })));
    applySequenced(fold, seq(3, ev({ type: EventType.INPUT_RESOLVED, input: inputReq("q1") })));
    const pend = fold.sessions.get("s0")?.pendingInputs ?? [];
    expect(pend.map((p) => p.id)).toEqual(["q2"]);
  });

  it("PART_START upserts by part id (no duplicates)", () => {
    const fold = newFold();
    applySequenced(fold, seq(1, ev({ type: EventType.PART_START, part: textPart("p1", "a") })));
    applySequenced(fold, seq(2, ev({ type: EventType.PART_END, part: textPart("p1", "ab") })));
    const parts = fold.sessions.get("s0")?.inFlightParts ?? [];
    expect(parts).toHaveLength(1);
    expect((parts[0]!.payload as { value: string }).value).toBe("ab");
  });

  it("PART_DELTA appends to the part's text payload", () => {
    const fold = newFold();
    applySequenced(fold, seq(1, ev({ type: EventType.PART_START, part: textPart("p1", "Hello") })));
    applySequenced(fold, seq(2, ev({ type: EventType.PART_DELTA, partId: "p1", delta: " world" })));
    const parts = fold.sessions.get("s0")?.inFlightParts ?? [];
    expect((parts[0]!.payload as { value: string }).value).toBe("Hello world");
  });

  it("PART_DELTA for an unseen part id is a no-op (client rule)", () => {
    const fold = newFold();
    applySequenced(fold, seq(1, ev({ type: EventType.PART_DELTA, partId: "ghost", delta: "x" })));
    expect(fold.sessions.get("s0")?.inFlightParts).toHaveLength(0);
  });
});

// ---------------------------------------------------------------------------
// Property test: random snapshot/event interleavings converge to the
// server fold (the port of abiclient's TestDiscardRulePropertyFuzz).
// ---------------------------------------------------------------------------

// Deterministic PRNG (mulberry32) so failures reproduce.
function mulberry32(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a |= 0;
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

interface ModelServer {
  seq: number;
  // The server projection state at each seq cut: sessions by id.
  cuts: Array<Map<string, SessionSnapshot>>;
}

/** Folds events 1..n into authoritative per-cut session states — the
 * "server" the client must converge to. Reuses the fold itself (the
 * discard rule's semantics are the shared definition), recording a
 * CLONED state after each event (the live fold mutates sessions in
 * place; sharing objects with later cuts would leak future state into
 * the snapshots the client is seeded from). */
function runServer(events: SequencedEvent[]): ModelServer {
  const fold = newFold();
  const cuts: Array<Map<string, SessionSnapshot>> = [];
  for (const e of events) {
    applySequenced(fold, e);
    cuts.push(new Map([...fold.sessions].map(([k, v]) => [k, cloneSessionSnapshot(v)])));
  }
  return { seq: events.length, cuts };
}

function randomEvent(rng: () => number, seqNum: number): SequencedEvent {
  const sid = `s${Math.floor(rng() * 3)}`;
  const roll = rng();
  if (roll < 0.3) {
    return seq(seqNum, ev({
      sessionId: sid,
      type: EventType.SESSION_STATUS,
      status: rng() < 0.5 ? SessionStatus.BUSY : SessionStatus.IDLE,
    }));
  }
  if (roll < 0.5) {
    return seq(seqNum, ev({
      sessionId: sid,
      type: EventType.INPUT_REQUEST,
      input: inputReq(`q${Math.floor(rng() * 4)}`, sid),
    }));
  }
  if (roll < 0.65) {
    return seq(seqNum, ev({
      sessionId: sid,
      type: EventType.INPUT_RESOLVED,
      input: inputReq(`q${Math.floor(rng() * 4)}`, sid),
    }));
  }
  if (roll < 0.85) {
    return seq(seqNum, ev({
      sessionId: sid,
      type: EventType.PART_START,
      part: textPart(`p${Math.floor(rng() * 4)}`, rng() < 0.5 ? "x" : "xy"),
    }));
  }
  return seq(seqNum, ev({
    sessionId: sid,
    type: EventType.PART_DELTA,
    partId: `p${Math.floor(rng() * 4)}`,
    delta: "d",
  }));
}

describe("client_discard_rule property", () => {
  it("random snapshot/event interleavings with duplicates converge to the server fold", () => {
    const rng = mulberry32(7);
    for (let iter = 0; iter < 200; iter++) {
      const events: SequencedEvent[] = [];
      const count = 1 + Math.floor(rng() * 25);
      for (let i = 1; i <= count; i++) events.push(randomEvent(rng, i));
      const server = runServer(events);

      // Client: snapshot at a random cut, then a delivery stream that
      // replays stale events (already ≤ S), duplicates, and the remaining
      // suffix in order — the reconnect/rolling-deploy shapes.
      const cut = Math.floor(rng() * (count + 1));
      const fold = newFold();
      if (cut > 0) {
        applySnapshot(fold, snapshotFrame(cut, [...server.cuts[cut - 1]!.values()]));
      }

      // Delivery noise: each pre-cut event may re-arrive (stale); each
      // post-cut event may arrive twice (duplicate).
      const stream: SequencedEvent[] = [];
      for (let i = 0; i < cut; i++) {
        if (rng() < 0.3) stream.push(events[i]!);
      }
      for (let i = cut; i < count; i++) {
        stream.push(events[i]!);
        if (rng() < 0.3) stream.push(events[i]!);
      }

      for (const e of stream) applySequenced(fold, e);

      // Convergence: every session the server knows must match, and the
      // client must not know sessions the server doesn't.
      const serverFinal = server.cuts[count - 1]!;
      expect(fold.seq).toBe(BigInt(count));
      expect([...fold.sessions.keys()].sort()).toEqual([...serverFinal.keys()].sort());
      for (const [sid, srv] of serverFinal) {
        const cl = fold.sessions.get(sid)!;
        expect(cl.status, `iter ${iter} ${sid} status`).toBe(srv.status);
        expect(cl.pendingInputs.map((p) => p.id).sort(), `iter ${iter} ${sid} pendings`)
          .toEqual(srv.pendingInputs.map((p) => p.id).sort());
        expect(cl.inFlightParts.map((p) => p.id).sort(), `iter ${iter} ${sid} parts`)
          .toEqual(srv.inFlightParts.map((p) => p.id).sort());
      }
    }
  });

  it("mid-turn reconnect: fresh snapshot at a later cut, overlap events discarded exactly", () => {
    const rng = mulberry32(11);
    const events: SequencedEvent[] = [];
    for (let i = 1; i <= 20; i++) events.push(randomEvent(rng, i));
    const server = runServer(events);

    const client = newFold();
    applySnapshot(client, snapshotFrame(8, [...server.cuts[7]!.values()]));
    for (let i = 5; i <= 12; i++) applySequenced(client, events[i - 1]!); // overlap + new
    applySnapshot(client, snapshotFrame(15, [...server.cuts[14]!.values()])); // reconnect re-snapshot
    for (let i = 13; i <= 20; i++) applySequenced(client, events[i - 1]!);

    const serverFinal = server.cuts[19]!;
    expect(client.seq).toBe(20n);
    for (const [sid, srv] of serverFinal) {
      const cl = client.sessions.get(sid)!;
      expect(cl.status).toBe(srv.status);
      expect(cl.pendingInputs.map((p) => p.id).sort()).toEqual(srv.pendingInputs.map((p) => p.id).sort());
      expect(cl.inFlightParts.map((p) => p.id).sort()).toEqual(srv.inFlightParts.map((p) => p.id).sort());
    }
  });
});

// The wire frames the property test builds must round-trip through the
// same protojson parser the SSE transport uses (bigint seq as string).
describe("fold: protojson wire compatibility", () => {
  it("fromJson(StreamFrameSchema) parses the handler's camelCase SSE body", () => {
    const frame = fromJson(StreamFrameSchema, JSON.parse(`{
      "snapshot": { "atSeq": "7", "snapshot": { "sessions": [
        { "sessionId": "s0", "status": "SESSION_STATUS_BUSY", "inFlightParts": [
          { "id": "p1", "type": "PART_TYPE_TEXT", "text": "hi" }
        ], "queueDepth": 1, "pendingInputs": [] }
      ]}}
    }`));
    const fold = newFold();
    if (frame.frame.case === "snapshot") applySnapshot(fold, frame.frame.value);
    else throw new Error("expected snapshot frame");
    expect(fold.seq).toBe(7n);
    expect(fold.sessions.get("s0")?.inFlightParts[0]?.payload).toEqual({ case: "text", value: "hi" });
  });
});
