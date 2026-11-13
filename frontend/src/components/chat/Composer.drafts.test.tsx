/**
 * Composer drafts (owner ask 2026-11-13): the chat input's text is
 * persisted per session — remounting the Composer for the same
 * session restores its draft, sending consumes it, and a session
 * switch never bleeds one session's text into another. Exercises the
 * REAL Composer through its textarea (send path included).
 */
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "../../test/utils";
import { Composer } from "./Composer";

// Deterministic desktop mode + settings isolation (house pattern from
// Composer.test.tsx).
vi.mock("../../hooks/useUserSettings", () => ({
  useUserSetting: vi.fn((_key: string, defaultValue: unknown) => defaultValue),
}));

import { loadSessionDraft } from "../../lib/sessionDrafts";

function setDesktopMatchMedia() {
  vi.spyOn(window, "matchMedia").mockImplementation((query) => ({
    matches: query.includes("min-width"),
    media: query,
    onchange: null,
    addEventListener: () => {},
    removeEventListener: () => {},
    dispatchEvent: () => false,
  }) as unknown as MediaQueryList);
}

function renderComposer(ws = "ws-1", sid = "sess-1") {
  return render(<Composer onSend={vi.fn()} workspaceId={ws} sessionId={sid} />);
}

async function type(text: string) {
  const user = userEvent.setup();
  await user.type(screen.getByPlaceholderText("Type a message..."), text);
}

describe("Composer per-session drafts", () => {
  beforeEach(() => {
    localStorage.clear();
    setDesktopMatchMedia();
  });
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("type → unmount → remount (same session): the draft is restored", async () => {
    const r1 = renderComposer();
    await type("half-finished thought");
    r1.unmount();

    const r2 = renderComposer();
    expect(screen.getByPlaceholderText("Type a message...")).toHaveValue("half-finished thought");
    r2.unmount();
  });

  it("send → the stored draft is consumed (a remount shows an empty box)", async () => {
    const user = userEvent.setup();
    const r1 = renderComposer();
    await type("ship it");
    await user.click(screen.getByRole("button", { name: "Send message" }));
    expect(screen.getByPlaceholderText("Type a message...")).toHaveValue("");
    // The store itself is empty — a remount (or a page reload) starts fresh.
    expect(loadSessionDraft("ws-1", "sess-1")).toBe("");
    r1.unmount();

    const r2 = renderComposer();
    expect(screen.getByPlaceholderText("Type a message...")).toHaveValue("");
    r2.unmount();
  });

  it("session switch (remount with another session): each session sees its own draft", async () => {
    localStorage.setItem(
      "llmsafespaces_session_drafts",
      JSON.stringify({
        "ws-1": {
          "sess-1": { text: "draft one", updatedAt: Date.now() },
          "sess-2": { text: "draft two", updatedAt: Date.now() },
        },
      }),
    );

    const r1 = renderComposer("ws-1", "sess-1");
    expect(screen.getByPlaceholderText("Type a message...")).toHaveValue("draft one");
    r1.unmount();

    const r2 = renderComposer("ws-1", "sess-2");
    expect(screen.getByPlaceholderText("Type a message...")).toHaveValue("draft two");
    r2.unmount();
  });

  it("drafts are keyed per workspace (same session id, different workspace)", async () => {
    const r1 = renderComposer("ws-1", "sess-shared");
    await type("ws-1's version");
    r1.unmount();

    const r2 = renderComposer("ws-2", "sess-shared");
    expect(screen.getByPlaceholderText("Type a message...")).toHaveValue("");
    r2.unmount();
    expect(loadSessionDraft("ws-1", "sess-shared")).toBe("ws-1's version");
  });

  it("unmount alone persists the typed draft (switch-away mid-typing)", async () => {
    const r1 = renderComposer();
    await type("never sent");
    r1.unmount();
    expect(loadSessionDraft("ws-1", "sess-1")).toBe("never sent");
  });
});
