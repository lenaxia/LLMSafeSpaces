// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "../../test/utils";
import { Composer } from "./Composer";

vi.mock("./ModelSelector", () => ({ ModelSelector: () => <div data-testid="model-selector-stub" /> }));
vi.mock("./RoleSelector", () => ({ RoleSelector: () => <div data-testid="role-selector-stub" /> }));
let mockSettings: Record<string, unknown> = {};
vi.mock("../../hooks/useUserSettings", () => ({
  useUserSetting: vi.fn(<T,>(key: string, defaultValue: T): T => (mockSettings[key] as T) ?? defaultValue),
  setUserSetting: vi.fn((key: string, value: unknown) => {
    mockSettings[key] = value;
    return Promise.resolve();
  }),
}));
vi.mock("../../api/workspaces", () => ({ workspacesApi: {} }));

// The prompt-library seam, at the API-CLIENT boundary (the live-swap
// re-seam): the REAL adapter runs — envelope unwrap and all — against
// a mocked network whose fixture is the #1499 named
// {"prompts":[...]} shape the backend actually ships.
let mockPrompts: { id: string; name: string; content: string }[] = [];
vi.mock("../../api/client", () => ({
  api: { get: (path: string) => (path === "/me/prompts" ? Promise.resolve({ prompts: mockPrompts }) : Promise.reject(new Error(`unmocked api.get ${path}`))) },
}));

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

const onSend = vi.fn();

function renderComposer() {
  return render(<Composer onSend={onSend} workspaceId="ws-1" sessionId="ses-1" />);
}

async function typeIn(text: string) {
  const box = screen.getByRole("textbox");
  await userEvent.type(box, text);
  return box as HTMLTextAreaElement;
}

async function waitForPrompts() {
  await waitFor(() => {
    if (mockPrompts.length > 0) {
      expect(screen.queryByTestId("prompt-recall-popup")).not.toBeNull();
    }
  });
}

describe("Composer #-prompt recall (#1496 part 2; symbol moved @→# pre-release)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockSettings = {};
    setMobileMatchMedia(false);
  });

  it("opens on # mid-sentence and lists prompts", async () => {
    mockPrompts = [
      { id: "p1", name: "deploy", content: "deploy steps" },
      { id: "p2", name: "review", content: "review checklist" },
    ];
    renderComposer();
    // Bare # at LINE START is the heading shape (suppressed) — the
    // mid-sentence form is the opener here.
    await typeIn("hey #");
    await waitForPrompts();
    const popup = screen.getByTestId("prompt-recall-popup");
    expect(popup.querySelector('[data-prompt="p1"]')).not.toBeNull();
    expect(popup.querySelector('[data-prompt="p2"]')).not.toBeNull();
  });

  it("filters live by the #word under the caret", async () => {
    mockPrompts = [
      { id: "p1", name: "deploy", content: "deploy steps" },
      { id: "p2", name: "review", content: "review checklist" },
    ];
    renderComposer();
    await typeIn("please dep");
    expect(screen.queryByTestId("prompt-recall-popup")).toBeNull(); // no # yet
    // Rewrite via clear+type so the caret tracks naturally (jsdom's
    // selection behavior on raw value writes is not dependable).
    const box = screen.getByRole("textbox");
    await userEvent.clear(box);
    await userEvent.type(box, "please #dep");
    const popup = screen.getByTestId("prompt-recall-popup");
    expect(popup.querySelectorAll("[data-prompt]").length).toBe(1);
    expect(popup.querySelector('[data-prompt="p1"]')).not.toBeNull();
  });

  it("Enter expands the selected prompt inline, replacing the #token", async () => {
    mockPrompts = [{ id: "p1", name: "deploy", content: "DEPLOY CONTENT" }];
    renderComposer();
    const box = await typeIn("run #dep");
    fireEvent.keyDown(box, { key: "Enter" });
    await waitFor(() => expect(box.value).toBe("run DEPLOY CONTENT"));
    expect(screen.queryByTestId("prompt-recall-popup")).toBeNull(); // closed after expand
    expect(box.selectionStart).toBe("run DEPLOY CONTENT".length); // caret at end of expansion
  });

  it("selection expands content that itself ends in a #token WITHOUT reopening the popup (recursion guard)", async () => {
    mockPrompts = [{ id: "p1", name: "chain", content: "first #second" }];
    renderComposer();
    const box = await typeIn("x #ch");
    fireEvent.keyDown(box, { key: "Enter" });
    await waitFor(() => expect(box.value).toBe("x first #second"));
    // The expansion's own trailing # must not re-trigger the popup.
    expect(screen.queryByTestId("prompt-recall-popup")).toBeNull();
  });

  it("Escape dismisses for the current token; a different token reopens", async () => {
    mockPrompts = [
      { id: "p1", name: "deploy", content: "d" },
      { id: "p2", name: "review", content: "r" },
    ];
    renderComposer();
    const box = await typeIn("x #dep");
    fireEvent.keyDown(box, { key: "Escape" });
    expect(screen.queryByTestId("prompt-recall-popup")).toBeNull();
    // Completing the same dismissed token stays dismissed.
    await userEvent.type(screen.getByRole("textbox"), "loy");
    expect(screen.queryByTestId("prompt-recall-popup")).toBeNull();
    // A DIFFERENT ANCHOR re-arms recall: dismissal keys on the # anchor
    // INDEX, so the re-arm fixture must anchor elsewhere ("x #dep"
    // anchors at 2; "yz #rev" anchors at 3 — a same-anchor "y #rev"
    // would stay dismissed and this row would fail).
    await userEvent.clear(box);
    await userEvent.type(box, "yz #rev");
    await waitFor(() => expect(screen.getByTestId("prompt-recall-popup")).toBeTruthy());
  });

  it("handles multiple #tokens: only the token under the caret offers recall", async () => {
    mockPrompts = [{ id: "p1", name: "a", content: "AAA" }];
    renderComposer();
    // Build "@a text @b" with the caret after the FIRST token.
    const box = screen.getByRole("textbox") as HTMLTextAreaElement;
    fireEvent.change(box, { target: { value: "x #a tail y #b" } });
    fireEvent.select(box, { target: { selectionStart: 4, selectionEnd: 4 } });
    await waitFor(() => expect(screen.getByTestId("prompt-recall-popup")).toBeTruthy());
    const popup = screen.getByTestId("prompt-recall-popup");
    expect(popup.querySelector('[data-prompt="p1"]')).not.toBeNull();
  });

  it("markdown heading shape does NOT open the popup (bare # at line start)", async () => {
    mockPrompts = [{ id: "p1", name: "Title", content: "t" }];
    renderComposer();
    const box = await typeIn("#");
    expect(screen.queryByTestId("prompt-recall-popup")).toBeNull();
    // Heading continuation stays closed; an explicit #word opens.
    await userEvent.type(box, " Title");
    expect(screen.queryByTestId("prompt-recall-popup")).toBeNull();
    await userEvent.clear(box);
    await userEvent.type(box, "#dep");
    await waitFor(() => expect(screen.getByTestId("prompt-recall-popup")).toBeTruthy());
  });

  it("C# mid-word never opens the popup", async () => {
    mockPrompts = [{ id: "p1", name: "sharp", content: "s" }];
    renderComposer();
    await typeIn("written in C#");
    await waitFor(() => expect(screen.queryByTestId("prompt-recall-popup")).toBeNull());
  });

  it("an empty prompt library never opens the popup", async () => {
    mockPrompts = [];
    renderComposer();
    await typeIn("hello #any");
    await waitFor(() => expect(screen.queryByTestId("prompt-recall-popup")).toBeNull());
  });

  it("a live token matching nothing shows the no-match state", async () => {
    mockPrompts = [{ id: "p1", name: "deploy", content: "d" }];
    renderComposer();
    await typeIn("x #zzz");
    await waitFor(() => expect(screen.getByTestId("prompt-recall-popup")).toBeTruthy());
    expect(screen.getByTestId("prompt-recall-popup").textContent).toContain("No prompt matches");
  });

  it("does not open while an IME composition is active", async () => {
    mockPrompts = [{ id: "p1", name: "deploy", content: "d" }];
    renderComposer();
    const box = screen.getByRole("textbox");
    fireEvent.compositionStart(box);
    await userEvent.type(box, "hey #");
    expect(screen.queryByTestId("prompt-recall-popup")).toBeNull(); // composing: suppressed
    fireEvent.compositionEnd(box);
    await userEvent.type(box, "dep");
    await waitFor(() => expect(screen.getByTestId("prompt-recall-popup")).toBeTruthy());
  });

  it("trailing-#token expansion stays suppressed (the jsdom fast-loop pin)", async () => {
    // The text-keyed suppression is decidable in jsdom (unlike the old
    // dead-ref mechanism): prompt content ENDING in a #token leaves a
    // live token at the new caret — the popup must stay closed.
    mockPrompts = [{ id: "p7", name: "ping", content: "now ping #deploy" }];
    renderComposer();
    const box = await typeIn("x #pi");
    fireEvent.keyDown(box, { key: "Enter" });
    await waitFor(() => expect((screen.getByRole("textbox") as HTMLTextAreaElement).value).toBe("x now ping #deploy"));
    expect(screen.queryByTestId("prompt-recall-popup")).toBeNull();
  });

  it("click selects: the popup row click expands the prompt", async () => {
    mockPrompts = [{ id: "p1", name: "deploy", content: "CLICKED CONTENT" }];
    renderComposer();
    await typeIn("go #dep");
    const row = await waitFor(() => {
      const el = screen.getByTestId("prompt-recall-popup").querySelector('[data-prompt="p1"]');
      expect(el).not.toBeNull();
      return el as HTMLElement;
    });
    fireEvent.click(row);
    await waitFor(() => expect((screen.getByRole("textbox") as HTMLTextAreaElement).value).toBe("go CLICKED CONTENT"));
  });
});
