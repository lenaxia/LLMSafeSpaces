import { beforeEach, describe, expect, it } from "vitest";
import {
  eventInternalSessionId,
  hasSessionAliases,
  learnSessionAlias,
  rekeySessionMap,
  resetSessionAliases,
  resolveSessionAlias,
  translateSessionIds,
} from "./sessionAliases";

const WS = "ws-1";
const PHRASE = "quiet-mountain-9";
const HARNESS = "ses_abc123";

beforeEach(() => {
  resetSessionAliases();
});

describe("sessionAliases (#1817 heal-window seam)", () => {
  it("learns the additive pair (phrase on the wire, harness internal) and resolves the harness", () => {
    const pair = translateSessionIds(WS, PHRASE, HARNESS);
    expect(pair).toEqual({ sessionId: PHRASE, internalSessionId: HARNESS });
    expect(resolveSessionAlias(WS, HARNESS)).toBe(PHRASE);
    expect(resolveSessionAlias(WS, PHRASE)).toBe(PHRASE);
  });

  it("translates a legacy harness-keyed frame once the pair is known, preserving the harness additively", () => {
    learnSessionAlias(WS, PHRASE, HARNESS);
    expect(translateSessionIds(WS, HARNESS, undefined)).toEqual({
      sessionId: PHRASE,
      internalSessionId: HARNESS,
    });
  });

  it("is the identity before any pair is learned (never worse than pre-fix)", () => {
    expect(translateSessionIds(WS, HARNESS, undefined)).toEqual({
      sessionId: HARNESS,
      internalSessionId: undefined,
    });
    expect(translateSessionIds(WS, PHRASE, undefined)).toEqual({
      sessionId: PHRASE,
      internalSessionId: undefined,
    });
  });

  it("does not translate a frame whose additive id equals its wire id (decoy guard)", () => {
    learnSessionAlias(WS, PHRASE, HARNESS);
    expect(translateSessionIds(WS, HARNESS, HARNESS)).toEqual({
      sessionId: HARNESS,
      internalSessionId: HARNESS,
    });
  });

  it("ignores empty or equal pairs (no invented mappings)", () => {
    learnSessionAlias(WS, "", HARNESS);
    learnSessionAlias(WS, PHRASE, "");
    learnSessionAlias(WS, PHRASE, PHRASE);
    expect(hasSessionAliases(WS)).toBe(false);
  });

  it("scopes aliases per workspace", () => {
    learnSessionAlias(WS, PHRASE, HARNESS);
    expect(resolveSessionAlias("ws-2", HARNESS)).toBe(HARNESS);
  });

  it("updates a harness that is re-minted under a new phrase", () => {
    learnSessionAlias(WS, PHRASE, HARNESS);
    learnSessionAlias(WS, "another-phrase-2", HARNESS);
    expect(resolveSessionAlias(WS, HARNESS)).toBe("another-phrase-2");
  });

  it("reads the additive id from the user-stream data map and a top-level field", () => {
    expect(eventInternalSessionId({ data: { internal_session_id: HARNESS } })).toBe(HARNESS);
    expect(eventInternalSessionId({ internal_session_id: HARNESS })).toBe(HARNESS);
    expect(eventInternalSessionId({ data: {} })).toBeUndefined();
    expect(eventInternalSessionId(null)).toBeUndefined();
  });

  it("re-keys a legacy fold entry that landed before the pair was learned", () => {
    const sessions = new Map<string, { sessionId?: string; internalSessionId?: string }>([
      [HARNESS, { sessionId: HARNESS, internalSessionId: "" }],
    ]);
    learnSessionAlias(WS, PHRASE, HARNESS);
    expect(rekeySessionMap(WS, sessions)).toBe(true);
    expect(sessions.has(HARNESS)).toBe(false);
    expect(sessions.get(PHRASE)).toEqual({ sessionId: PHRASE, internalSessionId: HARNESS });
  });

  it("re-key never moves the same-id decoy or overwrites an existing phrase entry", () => {
    learnSessionAlias(WS, PHRASE, HARNESS);
    const decoy = new Map<string, { sessionId?: string; internalSessionId?: string }>([
      [HARNESS, { sessionId: HARNESS, internalSessionId: HARNESS }],
    ]);
    expect(rekeySessionMap(WS, decoy)).toBe(false);
    expect(decoy.has(HARNESS)).toBe(true);

    const existing = new Map<string, { sessionId?: string; internalSessionId?: string }>([
      [HARNESS, { sessionId: HARNESS, internalSessionId: "" }],
      [PHRASE, { sessionId: PHRASE, internalSessionId: HARNESS }],
    ]);
    expect(rekeySessionMap(WS, existing)).toBe(false);
    expect(existing.has(HARNESS)).toBe(true);
  });

  it("bounds the per-workspace alias table", () => {
    for (let i = 0; i < 600; i++) {
      learnSessionAlias(WS, `phrase-${i}`, `harness-${i}`);
    }
    // Oldest entries evicted; the newest still resolve.
    expect(resolveSessionAlias(WS, "harness-0")).toBe("harness-0");
    expect(resolveSessionAlias(WS, "harness-599")).toBe("phrase-599");
  });
});
