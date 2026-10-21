import { beforeEach, describe, expect, it } from "vitest";
import {
  eventInternalSessionId,
  hasSessionAliases,
  learnSessionAlias,
  rekeySessionMap,
  resetSessionAliases,
  resolveSessionAlias,
  sessionAliasRekeyCount,
  sessionAliasVersion,
  subscribeSessionAliases,
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

  it("re-key never moves the same-id decoy but retires a duplicate harness row when the phrase exists", () => {
    learnSessionAlias(WS, PHRASE, HARNESS);
    const decoy = new Map<string, { sessionId?: string; internalSessionId?: string }>([
      [HARNESS, { sessionId: HARNESS, internalSessionId: HARNESS }],
    ]);
    expect(rekeySessionMap(WS, decoy)).toBe(false);
    expect(decoy.has(HARNESS)).toBe(true);

    // #1824 r11: the phrase row is the newer projection of the same
    // session. A stale harness row left by a pre-fix snapshot must be
    // retired, not kept as a duplicate key.
    const existing = new Map<string, { sessionId?: string; internalSessionId?: string }>([
      [HARNESS, { sessionId: HARNESS, internalSessionId: "" }],
      [PHRASE, { sessionId: PHRASE, internalSessionId: HARNESS }],
    ]);
    expect(rekeySessionMap(WS, existing)).toBe(true);
    expect(existing.size).toBe(1);
    expect(existing.has(HARNESS)).toBe(false);
    expect(existing.get(PHRASE)).toEqual({ sessionId: PHRASE, internalSessionId: HARNESS });
  });

  it("bounds the per-workspace alias table", () => {
    for (let i = 0; i < 600; i++) {
      learnSessionAlias(WS, `phrase-${i}`, `harness-${i}`);
    }
    // Oldest entries evicted; the newest still resolve.
    expect(resolveSessionAlias(WS, "harness-0")).toBe("harness-0");
    expect(resolveSessionAlias(WS, "harness-599")).toBe("phrase-599");
  });

  it("bounds the number of workspaces, evicting the oldest", () => {
    for (let i = 0; i < 1200; i++) {
      expect(learnSessionAlias(`ws-${i}`, `phrase-${i}`, `harness-${i}`)).toBe(true);
    }
    expect(hasSessionAliases("ws-0")).toBe(false);
    expect(hasSessionAliases("ws-1199")).toBe(true);
  });

  it("reports whether a pair changed and bumps the observable version", () => {
    const v0 = sessionAliasVersion();
    expect(learnSessionAlias(WS, PHRASE, HARNESS)).toBe(true);
    const v1 = sessionAliasVersion();
    expect(v1).toBeGreaterThan(v0);

    // Re-learning the same pair is a no-op: no version bump.
    expect(learnSessionAlias(WS, PHRASE, HARNESS)).toBe(false);
    expect(sessionAliasVersion()).toBe(v1);
    expect(learnSessionAlias(WS, PHRASE, PHRASE)).toBe(false);
    expect(learnSessionAlias(WS, "", HARNESS)).toBe(false);
  });

  it("notifies subscribers once per learned change, per workspace", () => {
    const seen: string[] = [];
    const unsubscribe = subscribeSessionAliases((ws) => seen.push(ws));
    expect(learnSessionAlias(WS, PHRASE, HARNESS)).toBe(true);
    expect(learnSessionAlias(WS, PHRASE, HARNESS)).toBe(false);
    expect(learnSessionAlias("ws-2", PHRASE, HARNESS)).toBe(true);
    unsubscribe();
    learnSessionAlias(WS, "another-phrase-2", HARNESS);
    expect(seen).toEqual([WS, "ws-2"]);
  });

  it("rejects overlong, whitespace, and control-character ids", () => {
    const overlong = "x".repeat(300);
    expect(learnSessionAlias(WS, overlong, HARNESS)).toBe(false);
    expect(learnSessionAlias(WS, PHRASE, overlong)).toBe(false);
    expect(learnSessionAlias(WS, "has space", HARNESS)).toBe(false);
    expect(learnSessionAlias(WS, PHRASE, "har\nness")).toBe(false);
    expect(learnSessionAlias("bad ws", PHRASE, HARNESS)).toBe(false);
    expect(hasSessionAliases(WS)).toBe(false);
  });

  it("counts re-key passes (the hot-path assertion hook)", () => {
    const before = sessionAliasRekeyCount();
    rekeySessionMap(WS, new Map());
    expect(sessionAliasRekeyCount()).toBe(before + 1);
  });
});
