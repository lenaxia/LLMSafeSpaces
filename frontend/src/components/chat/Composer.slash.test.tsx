// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "../../test/utils";
import { Composer } from "./Composer";

// Sibling components stubbed (their wiring is their own tests' subject).
vi.mock("./ModelSelector", () => ({ ModelSelector: () => <div data-testid="model-selector-stub" /> }));
vi.mock("./RoleSelector", () => ({ RoleSelector: () => <div data-testid="role-selector-stub" /> }));
// Settings: in-memory store so /model's drawer open is observable.
let mockSettings: Record<string, unknown> = {};
vi.mock("../../hooks/useUserSettings", () => ({
  useUserSetting: vi.fn(<T,>(key: string, defaultValue: T): T => (mockSettings[key] as T) ?? defaultValue),
  setUserSetting: vi.fn((key: string, value: unknown) => {
    mockSettings[key] = value;
    return Promise.resolve();
  }),
}));
// Adapters mocked at the seam the commands route through.
const renameSession = vi.fn((_ws: string, _ses: string, _title: string) => Promise.resolve());
const sessionAction = vi.fn((_ws: string, _ses: string, _action: Record<string, unknown>) => Promise.resolve({}));
vi.mock("../../api/workspaces", () => ({
  workspacesApi: {
    renameSession: (ws: string, ses: string, title: string) => renameSession(ws, ses, title),
    sessionAction: (ws: string, ses: string, action: Record<string, unknown>) => sessionAction(ws, ses, action),
  },
}));
// Prompt library: irrelevant for slash tests; empty list.
vi.mock("../../api/promptLibrary", () => ({ promptLibraryApi: { list: () => Promise.resolve([]) } }));

const onSend = vi.fn();

function renderComposer(props: Record<string, unknown> = {}) {
  return render(
    <Composer
      onSend={onSend}
      workspaceId="ws-1"
      sessionId="ses-1"
      onAbort={vi.fn()}
      onNewSession={vi.fn()}
      {...props}
    />,
  );
}

async function typeIn(text: string) {
  const box = screen.getByRole("textbox");
  await userEvent.type(box, text);
  return box;
}


function setMobileMatchMedia(isMobile: boolean) {
  vi.spyOn(window, "matchMedia").mockImplementation((query) => {
    const isMinWidthQuery = query.includes("min-width");
    return {
      matches: isMinWidthQuery ? !isMobile : false,
      media: query,
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      dispatchEvent: () => false,
    } as unknown as MediaQueryList;
  });
}

describe("Composer slash commands (#1496 part 1)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockSettings = {};
    setMobileMatchMedia(false); // desktop: Enter/Ctrl+Enter paths active
  });

  it("opens the palette on a leading / and lists every registered command", async () => {
    renderComposer();
    await typeIn("/");
    const palette = screen.getByTestId("slash-palette");
    for (const id of ["help", "compact", "model", "rename", "new", "abort"]) {
      expect(palette.querySelector(`[data-command="${id}"]`)).not.toBeNull();
    }
  });

  it("filters by the typed word", async () => {
    renderComposer();
    await typeIn("/re");
    const palette = screen.getByTestId("slash-palette");
    expect(palette.querySelectorAll("[data-command]").length).toBe(1);
    expect(palette.querySelector('[data-command="rename"]')).not.toBeNull();
  });

  it("hides the palette for an unknown command — Enter sends literal text", async () => {
    renderComposer();
    const box = await typeIn("/definitelynot");
    expect(screen.queryByTestId("slash-palette")).toBeNull();
    fireEvent.keyDown(box, { key: "Enter", ctrlKey: true });
    expect(onSend).toHaveBeenCalledWith("/definitelynot", []);
  });

  it("does not open for a slash mid-text", async () => {
    renderComposer();
    await typeIn("hi /re");
    expect(screen.queryByTestId("slash-palette")).toBeNull();
  });

  it("Enter with args executes the matched command with the args remainder", async () => {
    renderComposer();
    const box = await typeIn("/rename Weekly review");
    // The palette stays armed while the first word matches a known
    // command — args do not close it (Enter must execute, not newline).
    expect(screen.getByTestId("slash-palette")).toBeTruthy();
    fireEvent.keyDown(box, { key: "Enter" });
    await waitFor(() =>
      expect(renameSession).toHaveBeenCalledWith("ws-1", "ses-1", "Weekly review"),
    );
    expect((box as HTMLTextAreaElement).value).toBe(""); // input consumed
    expect(screen.getByTestId("command-notice").textContent).toContain("Weekly review");
  });

  it("rename without args surfaces usage and never calls the adapter", async () => {
    renderComposer();
    const box = await typeIn("/rename");
    fireEvent.keyDown(box, { key: "Enter" });
    await waitFor(() => expect(screen.getByTestId("command-notice")).toBeTruthy());
    expect(renameSession).not.toHaveBeenCalled();
    expect(screen.getByTestId("command-notice").textContent).toContain("Usage: /rename");
  });

  it("Tab completes the highlighted word", async () => {
    renderComposer();
    const box = await typeIn("/com");
    fireEvent.keyDown(box, { key: "Tab" });
    expect((box as HTMLTextAreaElement).value).toBe("/compact ");
    // The palette stays armed (exact word + args) — Enter now executes.
    expect(screen.getByTestId("slash-palette")).toBeTruthy();
  });

  it("Escape dismisses the current token; editing to a new word reopens", async () => {
    renderComposer();
    const box = await typeIn("/");
    fireEvent.keyDown(box, { key: "Escape" });
    expect(screen.queryByTestId("slash-palette")).toBeNull();
    // Any word change re-arms: typing "r" makes a new token (word "r").
    await userEvent.type(box, "r");
    expect(screen.getByTestId("slash-palette")).toBeTruthy();
    fireEvent.keyDown(box, { key: "Escape" });
    expect(screen.queryByTestId("slash-palette")).toBeNull();
  });

  it("arrow keys move the active item and Enter executes it", async () => {
    // Seed a collapsed drawer so /model's open is observable — on the
    // desktop "auto" default the drawer is already visible and the
    // command correctly only shows the notice.
    mockSettings = { composerDrawerOpen: "collapsed" };
    renderComposer();
    const box = await typeIn("/");
    fireEvent.keyDown(box, { key: "ArrowDown" }); // help → compact
    fireEvent.keyDown(box, { key: "ArrowDown" }); // compact → model
    fireEvent.keyDown(box, { key: "Enter" });
    await waitFor(() => expect(mockSettings["composerDrawerOpen"]).toBe("open"));
    expect(screen.getByTestId("command-notice").textContent).toContain("Model picker opened");
  });

  it("compact routes the typed action through the adapter — protojson union member", async () => {
    // The wire contract is the discriminated union member {"compact":{}}
    // (sdks/openapi.yaml; a `type` field is silently discarded by the
    // protojson decoder and the oneof stays unset → action.unknown).
    renderComposer();
    const box = await typeIn("/co");
    fireEvent.keyDown(box, { key: "Enter" });
    await waitFor(() =>
      expect(sessionAction).toHaveBeenCalledWith("ws-1", "ses-1", { compact: {} }),
    );
    expect(screen.getByTestId("command-notice").textContent).toContain("Compaction scheduled");
  });

  it("compact surfaces the REAL 501 body shape — nested error object, not [object Object]", async () => {
    // ApiClientError's message is "[object Object]" for nested error
    // payloads (the actions 501 body is {"error":{code,capability,detail}})
    // because the client constructs Error from the raw body.error value.
    // Construct the real shape; the notice must render the detail.
    const { ApiClientError } = await import("../../api/client");
    const realBody = { error: { code: "not_supported", capability: "abi.actions", detail: "typed actions require AGENTD_STATE_AUTHORITY (design 0055 M4/D4: single delivery regime)" } } as never;
    sessionAction.mockRejectedValueOnce(new ApiClientError(501, realBody));
    renderComposer();
    const box = await typeIn("/co");
    fireEvent.keyDown(box, { key: "Enter" });
    await waitFor(() => expect(screen.getByTestId("command-notice")).toBeTruthy());
    const notice = screen.getByTestId("command-notice");
    expect(notice.getAttribute("aria-label")).toBe("command-error-notice");
    expect(notice.textContent).toContain("AGENTD_STATE_AUTHORITY");
    expect(notice.textContent).toContain("abi.actions");
    expect(notice.textContent).not.toContain("[object Object]");
  });

  it("requirements gate: /compact without a session errors instead of calling", async () => {
    renderComposer({ sessionId: undefined });
    const box = await typeIn("/co");
    fireEvent.keyDown(box, { key: "Enter" });
    await waitFor(() => expect(screen.getByTestId("command-notice")).toBeTruthy());
    expect(sessionAction).not.toHaveBeenCalled();
    expect(screen.getByTestId("command-notice").textContent).toContain("needs an active session");
  });

  it("/new routes to the page callback", async () => {
    const onNewSession = vi.fn();
    renderComposer({ onNewSession });
    const box = await typeIn("/ne");
    fireEvent.keyDown(box, { key: "Enter" });
    await waitFor(() => expect(onNewSession).toHaveBeenCalled());
  });

  it("/abort routes to the existing abort path", async () => {
    const onAbort = vi.fn();
    renderComposer({ onAbort });
    const box = await typeIn("/ab");
    fireEvent.keyDown(box, { key: "Enter" });
    await waitFor(() => expect(onAbort).toHaveBeenCalled());
  });

  it("/help opens the overlay listing all commands", async () => {
    renderComposer();
    const box = await typeIn("/he");
    fireEvent.keyDown(box, { key: "Enter" });
    const overlay = await screen.findByTestId("slash-help-overlay");
    for (const id of ["help", "compact", "model", "rename", "new", "abort"]) {
      expect(overlay.textContent).toContain(`/${id}`);
    }
  });
});
