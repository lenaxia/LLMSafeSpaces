import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import {
  SESSION_DRAFT_TTL_MS,
  _resetSessionDraftStoreForTests,
  clearSessionDraft,
  clearWorkspaceDrafts,
  loadSessionDraft,
  saveSessionDraft,
  sweepExpiredSessionDrafts,
} from "./sessionDrafts";

const KEY = "llmsafespaces_session_drafts";

describe("sessionDrafts store", () => {
  beforeEach(() => {
    localStorage.clear();
    _resetSessionDraftStoreForTests();
  });

  it("save→load roundtrips per workspace+session", () => {
    saveSessionDraft("ws-1", "sess-1", "hello draft");
    expect(loadSessionDraft("ws-1", "sess-1")).toBe("hello draft");
    // Same session id under a DIFFERENT workspace is a different draft
    // (the store is shared across workspaces).
    saveSessionDraft("ws-2", "sess-1", "other workspace");
    expect(loadSessionDraft("ws-2", "sess-1")).toBe("other workspace");
    expect(loadSessionDraft("ws-1", "sess-1")).toBe("hello draft");
    expect(loadSessionDraft("ws-1", "sess-2")).toBe("");
  });

  it("persists through localStorage (a fresh store instance restores)", () => {
    saveSessionDraft("ws-1", "sess-1", "survives reload");
    // Simulate a page reload: drop the in-memory cache.
    _resetSessionDraftStoreForTests();
    expect(loadSessionDraft("ws-1", "sess-1")).toBe("survives reload");
    expect(JSON.parse(localStorage.getItem(KEY)!)).toMatchObject({
      "ws-1": { "sess-1": { text: "survives reload" } },
    });
  });

  it("saving an empty text REMOVES the entry (sent = consumed)", () => {
    saveSessionDraft("ws-1", "sess-1", "draft");
    saveSessionDraft("ws-1", "sess-1", "");
    expect(loadSessionDraft("ws-1", "sess-1")).toBe("");
    // No empty-string entries accumulate: the workspace bucket is gone.
    expect(JSON.parse(localStorage.getItem(KEY) ?? "{}")).toEqual({});
  });

  it("clearSessionDraft deletes exactly one session's draft", () => {
    saveSessionDraft("ws-1", "sess-1", "a");
    saveSessionDraft("ws-1", "sess-2", "b");
    clearSessionDraft("ws-1", "sess-1");
    expect(loadSessionDraft("ws-1", "sess-1")).toBe("");
    expect(loadSessionDraft("ws-1", "sess-2")).toBe("b");
  });

  it("clearWorkspaceDrafts removes the whole workspace subtree", () => {
    saveSessionDraft("ws-1", "sess-1", "a");
    saveSessionDraft("ws-1", "sess-2", "b");
    saveSessionDraft("ws-2", "sess-1", "c");
    clearWorkspaceDrafts("ws-1");
    expect(loadSessionDraft("ws-1", "sess-1")).toBe("");
    expect(loadSessionDraft("ws-1", "sess-2")).toBe("");
    expect(loadSessionDraft("ws-2", "sess-1")).toBe("c");
    // Idempotent on unknown workspaces.
    expect(() => clearWorkspaceDrafts("ws-404")).not.toThrow();
  });

  describe("TTL sweep (~10 days — the backstop shape)", () => {
    it("drops expired drafts and keeps fresh ones", () => {
      const now = Date.now();
      vi.useFakeTimers();
      try {
        vi.setSystemTime(now - SESSION_DRAFT_TTL_MS - 1);
        saveSessionDraft("ws-1", "sess-old", "stale");
        vi.setSystemTime(now - 1000);
        saveSessionDraft("ws-1", "sess-fresh", "current");
        vi.setSystemTime(now);

        const removed = sweepExpiredSessionDrafts(now);
        expect(removed).toBe(1);
        expect(loadSessionDraft("ws-1", "sess-old")).toBe("");
        expect(loadSessionDraft("ws-1", "sess-fresh")).toBe("current");
      } finally {
        vi.useRealTimers();
      }
    });

    it("prunes empty workspace buckets (no unbounded accumulation)", () => {
      const now = Date.now();
      vi.useFakeTimers();
      try {
        vi.setSystemTime(now - SESSION_DRAFT_TTL_MS - 1);
        saveSessionDraft("ws-1", "only-stale", "x");
        vi.setSystemTime(now);
        sweepExpiredSessionDrafts(now);
      } finally {
        vi.useRealTimers();
      }
      expect(JSON.parse(localStorage.getItem(KEY) ?? "{}")).toEqual({});
    });

    it("the lazy sweep rides store access: a load drops an expired draft", () => {
      const now = Date.now();
      // Seed an expired entry directly in localStorage.
      localStorage.setItem(
        KEY,
        JSON.stringify({ "ws-1": { "sess-old": { text: "stale", updatedAt: now - SESSION_DRAFT_TTL_MS - 5 } } }),
      );
      _resetSessionDraftStoreForTests();
      expect(loadSessionDraft("ws-1", "sess-old")).toBe("");
      expect(localStorage.getItem(KEY)).toBe("{}");
    });
  });

  it("oversized text is never persisted (quota hygiene)", () => {
    const huge = "x".repeat(64 * 1024 + 1);
    saveSessionDraft("ws-1", "sess-1", huge);
    expect(loadSessionDraft("ws-1", "sess-1")).toBe("");
    // The composer keeps it in memory for the tab; nothing landed.
    expect(localStorage.getItem(KEY)).toBe(null);
  });

  it("corrupted storage degrades to an empty store, not a crash", () => {
    localStorage.setItem(KEY, "{not json");
    _resetSessionDraftStoreForTests();
    expect(loadSessionDraft("ws-1", "sess-1")).toBe("");
    expect(() => saveSessionDraft("ws-1", "sess-1", "recovers")).not.toThrow();
    _resetSessionDraftStoreForTests();
    expect(loadSessionDraft("ws-1", "sess-1")).toBe("recovers");
  });

  it("valid-JSON WRONG-SHAPE stores never crash (review r1: the non-object bucket throw)", () => {
    // A non-object workspace bucket: `in` on a number used to throw.
    localStorage.setItem(KEY, JSON.stringify({ "ws-1": 5 }));
    expect(() => clearSessionDraft("ws-1", "sess-1")).not.toThrow();
    expect(() => saveSessionDraft("ws-1", "sess-1", "x")).not.toThrow();
    expect(loadSessionDraft("ws-1", "sess-1")).toBe("x"); // the save repaired the bucket

    // A non-object session entry, an array top level, and a bad entry
    // shape are all dropped by the read-time sanitizer.
    localStorage.setItem(
      KEY,
      JSON.stringify({ "ws-2": { bad: "not-an-entry", ok: { text: "kept", updatedAt: Date.now() } }, "ws-3": [1, 2] }),
    );
    expect(loadSessionDraft("ws-2", "bad")).toBe("");
    expect(loadSessionDraft("ws-2", "ok")).toBe("kept");
    expect(loadSessionDraft("ws-3", "anything")).toBe("");
    expect(() => sweepExpiredSessionDrafts(Date.now())).not.toThrow();
    // And a save on top of the wrong shape rewrites a clean store.
    saveSessionDraft("ws-3", "sess-1", "repaired");
    expect(JSON.parse(localStorage.getItem(KEY)!)).toEqual({
      "ws-2": { ok: { text: "kept", updatedAt: expect.any(Number) } },
      "ws-3": { "sess-1": { text: "repaired", updatedAt: expect.any(Number) } },
    });
  });

  it("a quota failure on write never throws (typing must not break)", () => {
    saveSessionDraft("ws-1", "sess-1", "first");
    const spy = vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new DOMException("QuotaExceededError", "QuotaExceededError");
    });
    try {
      expect(() => saveSessionDraft("ws-1", "sess-1", "second")).not.toThrow();
    } finally {
      spy.mockRestore();
    }
    // The failed write simply never landed; reads keep serving the
    // last successfully persisted value.
    expect(loadSessionDraft("ws-1", "sess-1")).toBe("first");
  });
});

describe("sessionDrafts sweep throttle", () => {
  beforeEach(() => {
    localStorage.clear();
    _resetSessionDraftStoreForTests();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("re-sweeps at most once per hour on ordinary traffic", () => {
    const now = Date.now();
    vi.useFakeTimers();
    vi.setSystemTime(now);
    // First access sweeps (lastSweepAt 0 → armed) and persists a draft.
    saveSessionDraft("ws-1", "sess-1", "a");
    // Plant a second, expired entry mid-flight (no in-memory cache —
    // a raw localStorage write is immediately visible to the store).
    const raw = JSON.parse(localStorage.getItem(KEY)!);
    raw["ws-1"]["sess-expired"] = { text: "old", updatedAt: now - SESSION_DRAFT_TTL_MS - 1 };
    localStorage.setItem(KEY, JSON.stringify(raw));
    // Within the hour: ordinary access must NOT sweep it yet.
    vi.setSystemTime(now + 30 * 60 * 1000);
    loadSessionDraft("ws-1", "sess-1");
    expect(loadSessionDraft("ws-1", "sess-expired")).toBe("old");
    // After the hour: the next access sweeps it.
    vi.setSystemTime(now + 61 * 60 * 1000);
    expect(loadSessionDraft("ws-1", "sess-expired")).toBe("");
  });
});
