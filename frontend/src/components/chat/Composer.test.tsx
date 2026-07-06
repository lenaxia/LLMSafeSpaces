import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "../../test/utils";
import { Composer } from "./Composer";

// Mock useUserSetting so sendOnEnter is deterministic per-test, decoupled
// from the settings storage/cache mechanism. Default is false (the new
// schema default). Tests that need legacy behavior override the mock.
let mockSendOnEnter = false;
vi.mock("../../hooks/useUserSettings", () => ({
  useUserSetting: vi.fn((_key: string, defaultValue: unknown) => {
    if (_key === "sendOnEnter") return mockSendOnEnter;
    return defaultValue;
  }),
}));

// --- matchMedia mock helper (for useIsMobile) ---
// jsdom returns matches:false for any query by default, which corresponds
// to DESKTOP (useIsMobile = !matchMedia("(min-width: 768px)").matches).
// Tests that need MOBILE behavior flip the match.
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

// Dispatch a keydown event with arbitrary extra fields (e.g. isComposing).
// userEvent does not expose isComposing, so we drop down to fireEvent for
// the IME-specific tests.
function keyDown(target: HTMLElement, props: {
  key: string;
  ctrlKey?: boolean;
  metaKey?: boolean;
  shiftKey?: boolean;
  isComposing?: boolean;
  keyCode?: number;
}) {
  fireEvent.keyDown(target, {
    key: props.key,
    code: props.key === "Enter" ? "Enter" : undefined,
    ctrlKey: !!props.ctrlKey,
    metaKey: !!props.metaKey,
    shiftKey: !!props.shiftKey,
    bubbles: true,
    cancelable: true,
    // isComposing and keyCode are read-only on KeyboardEvent; define them.
    ...({ isComposing: props.isComposing ?? false, keyCode: props.keyCode } as object),
  });
}

describe("Composer", () => {
  beforeEach(() => {
    mockSendOnEnter = false;
    setMobileMatchMedia(false); // desktop by default
  });
  afterEach(() => {
    vi.restoreAllMocks();
  });

  // ── Basic rendering ──────────────────────────────────────────

  it("renders textarea with placeholder", () => {
    render(<Composer onSend={vi.fn()} />);
    expect(screen.getByPlaceholderText("Type a message...")).toBeInTheDocument();
  });

  it("renders custom placeholder", () => {
    render(<Composer onSend={vi.fn()} placeholder="Custom..." />);
    expect(screen.getByPlaceholderText("Custom...")).toBeInTheDocument();
  });

  it("renders send button with accessible name", () => {
    render(<Composer onSend={vi.fn()} />);
    expect(screen.getByRole("button", { name: "Send message" })).toBeInTheDocument();
  });

  it("send button is disabled when textarea is empty", () => {
    render(<Composer onSend={vi.fn()} />);
    expect(screen.getByRole("button", { name: "Send message" })).toBeDisabled();
  });

  it("send button is enabled when textarea has text", async () => {
    const user = userEvent.setup();
    render(<Composer onSend={vi.fn()} />);
    await user.type(screen.getByPlaceholderText("Type a message..."), "hello");
    expect(screen.getByRole("button", { name: "Send message" })).not.toBeDisabled();
  });

  it("calls onSend with trimmed text on submit", async () => {
    const user = userEvent.setup();
    const onSend = vi.fn();
    render(<Composer onSend={onSend} />);
    await user.type(screen.getByPlaceholderText("Type a message..."), "  hello world  ");
    await user.click(screen.getByRole("button", { name: "Send message" }));
    expect(onSend).toHaveBeenCalledWith("hello world");
  });

  it("clears textarea after send", async () => {
    const user = userEvent.setup();
    render(<Composer onSend={vi.fn()} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.type(textarea, "hello");
    await user.click(screen.getByRole("button", { name: "Send message" }));
    expect(textarea).toHaveValue("");
  });

  it("is disabled when disabled prop is true", () => {
    render(<Composer onSend={vi.fn()} disabled />);
    expect(screen.getByPlaceholderText("Type a message...")).toBeDisabled();
  });

  it("does not send whitespace-only messages", async () => {
    const user = userEvent.setup();
    const onSend = vi.fn();
    render(<Composer onSend={onSend} />);
    await user.type(screen.getByPlaceholderText("Type a message..."), "   ");
    await user.click(screen.getByRole("button", { name: "Send message" }));
    expect(onSend).not.toHaveBeenCalled();
  });

  // ── Feature A: send-key behavior ────────────────────────────
  //
  // Default mode (sendOnEnter=false, new default):
  //   - Desktop: Enter = newline, Ctrl/Cmd+Enter = send, Shift+Enter = newline
  //   - Mobile:  Enter = newline, only button sends
  //
  // Legacy mode (sendOnEnter=true):
  //   - Desktop: Enter = send, Shift+Enter = newline, Ctrl/Cmd+Enter = send
  //   - Mobile:  Enter = newline, only button sends (mobile ignores the setting)

  it("DESKTOP default mode: Enter adds a newline (does NOT send)", async () => {
    const user = userEvent.setup();
    const onSend = vi.fn();
    render(<Composer onSend={onSend} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.type(textarea, "hi");
    await user.keyboard("{Enter}");
    expect(onSend).not.toHaveBeenCalled();
    expect(textarea).toHaveValue("hi\n");
  });

  it("DESKTOP default mode: Ctrl+Enter sends", async () => {
    const user = userEvent.setup();
    const onSend = vi.fn();
    render(<Composer onSend={onSend} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.type(textarea, "hello");
    await user.keyboard("{Control>}{Enter}{/Control}");
    expect(onSend).toHaveBeenCalledWith("hello");
  });

  it("DESKTOP default mode: Cmd+Enter sends (Mac meta key)", async () => {
    const user = userEvent.setup();
    const onSend = vi.fn();
    render(<Composer onSend={onSend} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.type(textarea, "hello");
    await user.keyboard("{Meta>}{Enter}{/Meta}");
    expect(onSend).toHaveBeenCalledWith("hello");
  });

  it("DESKTOP default mode: Shift+Enter adds a newline (does NOT send)", async () => {
    const user = userEvent.setup();
    const onSend = vi.fn();
    render(<Composer onSend={onSend} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.type(textarea, "hi");
    await user.keyboard("{Shift>}{Enter}{/Shift}");
    expect(onSend).not.toHaveBeenCalled();
    expect(textarea).toHaveValue("hi\n");
  });

  it("DESKTOP legacy mode (sendOnEnter=true): Enter sends", async () => {
    mockSendOnEnter = true;
    const user = userEvent.setup();
    const onSend = vi.fn();
    render(<Composer onSend={onSend} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.type(textarea, "hi");
    await user.keyboard("{Enter}");
    expect(onSend).toHaveBeenCalledWith("hi");
  });

  it("DESKTOP legacy mode (sendOnEnter=true): Shift+Enter adds newline", async () => {
    mockSendOnEnter = true;
    const user = userEvent.setup();
    const onSend = vi.fn();
    render(<Composer onSend={onSend} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.type(textarea, "hi");
    await user.keyboard("{Shift>}{Enter}{/Shift}");
    expect(onSend).not.toHaveBeenCalled();
    expect(textarea).toHaveValue("hi\n");
  });

  it("MOBILE default mode: Enter adds newline (does NOT send) even with no modifier", async () => {
    setMobileMatchMedia(true);
    const user = userEvent.setup();
    const onSend = vi.fn();
    render(<Composer onSend={onSend} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.type(textarea, "hi");
    await user.keyboard("{Enter}");
    expect(onSend).not.toHaveBeenCalled();
    expect(textarea).toHaveValue("hi\n");
  });

  it("MOBILE default mode: Ctrl+Enter does NOT send (mobile is button-only)", async () => {
    setMobileMatchMedia(true);
    const user = userEvent.setup();
    const onSend = vi.fn();
    render(<Composer onSend={onSend} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.type(textarea, "hi");
    await user.keyboard("{Control>}{Enter}{/Control}");
    expect(onSend).not.toHaveBeenCalled();
  });

  it("MOBILE legacy setting is ignored: Enter still adds newline (no send)", async () => {
    setMobileMatchMedia(true);
    mockSendOnEnter = true;
    const user = userEvent.setup();
    const onSend = vi.fn();
    render(<Composer onSend={onSend} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.type(textarea, "hi");
    await user.keyboard("{Enter}");
    expect(onSend).not.toHaveBeenCalled();
    expect(textarea).toHaveValue("hi\n");
  });

  // ── IME composition guard ───────────────────────────────────
  //
  // While a CJK IME is composing (pinyin/kana/hangul input), Enter must
  // finalize the candidate character, not send the message. The native
  // KeyboardEvent exposes isComposing=true during composition; some old
  // browsers report keyCode===229 instead.

  it("DESKTOP default mode: Ctrl+Enter during IME composition does NOT send", async () => {
    const user = userEvent.setup();
    const onSend = vi.fn();
    render(<Composer onSend={onSend} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.type(textarea, "ni");
    // Simulate the composition-finalizing Enter. userEvent does not let
    // us set isComposing, so dispatch directly.
    keyDown(textarea as HTMLElement, { key: "Enter", ctrlKey: true, isComposing: true });
    expect(onSend).not.toHaveBeenCalled();
  });

  it("DESKTOP legacy mode: Enter during IME composition does NOT send", async () => {
    mockSendOnEnter = true;
    const user = userEvent.setup();
    const onSend = vi.fn();
    render(<Composer onSend={onSend} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.type(textarea, "ni");
    keyDown(textarea as HTMLElement, { key: "Enter", isComposing: true });
    expect(onSend).not.toHaveBeenCalled();
  });

  it("DESKTOP legacy mode: Enter with keyCode 229 (legacy IME signal) does NOT send", async () => {
    mockSendOnEnter = true;
    const user = userEvent.setup();
    const onSend = vi.fn();
    render(<Composer onSend={onSend} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.type(textarea, "ni");
    keyDown(textarea as HTMLElement, { key: "Enter", keyCode: 229 });
    expect(onSend).not.toHaveBeenCalled();
  });

  // ── Tooltip ─────────────────────────────────────────────────
  //
  // Desktop shows a tooltip on the send button so the user discovers the
  // active shortcut. Mobile has no hover, so no tooltip.

  it("DESKTOP default mode: send button tooltip advertises Ctrl+Enter", async () => {
    const user = userEvent.setup();
    render(<Composer onSend={vi.fn()} />);
    // Type so the button is enabled (Radix tooltip suppresses on disabled).
    await user.type(screen.getByPlaceholderText("Type a message..."), "x");
    // Hover the Send icon (the button's only child) — Radix Trigger attaches
    // pointer handlers via Slot; hovering the inner element reliably fires
    // them across re-renders, whereas hovering the button role can race the
    // re-render that enabling the button triggers.
    const sendIcon = screen.getByRole("button", { name: "Send message" }).querySelector("svg");
    if (sendIcon) await user.hover(sendIcon);
    await waitFor(() => {
      expect(screen.getAllByText(/Ctrl\+Enter/).length).toBeGreaterThan(0);
    });
  });

  it("DESKTOP legacy mode: send button tooltip advertises Enter", async () => {
    mockSendOnEnter = true;
    const user = userEvent.setup();
    render(<Composer onSend={vi.fn()} />);
    await user.type(screen.getByPlaceholderText("Type a message..."), "x");
    const sendIcon = screen.getByRole("button", { name: "Send message" }).querySelector("svg");
    if (sendIcon) await user.hover(sendIcon);
    await waitFor(() => {
      // Legacy tooltip should mention Enter, NOT Ctrl+Enter.
      expect(screen.getAllByText(/Send \(Enter\)/).length).toBeGreaterThan(0);
    });
    expect(screen.queryByText(/Ctrl\+Enter/)).not.toBeInTheDocument();
  });

  it("MOBILE: send button has NO tooltip", async () => {
    setMobileMatchMedia(true);
    const user = userEvent.setup();
    render(<Composer onSend={vi.fn()} />);
    await user.type(screen.getByPlaceholderText("Type a message..."), "x");
    const btn = screen.getByRole("button", { name: "Send message" });
    await user.hover(btn);
    // Give Radix a tick; the tooltip content should never appear.
    expect(screen.queryByText(/Ctrl\+Enter/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Send \(/)).not.toBeInTheDocument();
  });

  // ── Streaming / Queue behavior (existing, preserved) ─────────

  it("textarea is NOT disabled when streaming is true", () => {
    render(<Composer onSend={vi.fn()} streaming />);
    expect(screen.getByPlaceholderText("Type a message...")).not.toBeDisabled();
  });

  it("shows send button (not stop) during streaming", () => {
    render(<Composer onSend={vi.fn()} streaming />);
    expect(screen.getByRole("button", { name: "Send message" })).toBeInTheDocument();
  });

  it("shows stop button during streaming when onAbort is provided", () => {
    render(<Composer onSend={vi.fn()} onAbort={vi.fn()} streaming />);
    expect(screen.getByLabelText("Stop generating")).toBeInTheDocument();
  });

  it("clicking send during streaming calls onSend (queues the message)", async () => {
    const user = userEvent.setup();
    const onSend = vi.fn();
    render(<Composer onSend={onSend} onAbort={vi.fn()} streaming />);
    await user.type(screen.getByPlaceholderText("Type a message..."), "queued msg");
    await user.click(screen.getByRole("button", { name: "Send message" }));
    expect(onSend).toHaveBeenCalledWith("queued msg");
  });

  it("clicking stop during streaming calls onAbort", async () => {
    const user = userEvent.setup();
    const onAbort = vi.fn();
    render(<Composer onSend={vi.fn()} onAbort={onAbort} streaming />);
    await user.click(screen.getByLabelText("Stop generating"));
    expect(onAbort).toHaveBeenCalled();
  });

  it("Ctrl+Enter works during streaming (does not early-return)", async () => {
    const user = userEvent.setup();
    const onSend = vi.fn();
    render(<Composer onSend={onSend} onAbort={vi.fn()} streaming />);
    await user.type(screen.getByPlaceholderText("Type a message..."), "hello");
    await user.keyboard("{Control>}{Enter}{/Control}");
    expect(onSend).toHaveBeenCalledWith("hello");
  });

  it("does not show queued indicator by default", () => {
    render(<Composer onSend={vi.fn()} />);
    expect(screen.queryByText(/queued/)).not.toBeInTheDocument();
  });

  // ── Feature B: user-message history navigation (desktop only) ─
  //
  // Up/Down walk the userMessageHistory prop (newest-first), but only
  // when the cursor is on the first/last line of the textarea. Mobile
  // is a no-op (cursor moves normally). IME composition suppresses.

  it("DESKTOP: Up on first line loads previous user message from history", async () => {
    const user = userEvent.setup();
    const onSend = vi.fn();
    // Prop contract: newest-first. ["recent msg", "older msg"].
    render(<Composer onSend={onSend} userMessageHistory={["recent msg", "older msg"]} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.click(textarea); // focus, cursor at 0 (first line)
    await user.keyboard("{ArrowUp}");
    expect(textarea).toHaveValue("recent msg");
  });

  it("DESKTOP: repeated Up walks further back in history", async () => {
    const user = userEvent.setup();
    const onSend = vi.fn();
    // Newest-first: index 0 = most recent, index 2 = oldest.
    render(<Composer onSend={onSend} userMessageHistory={["recent", "middle", "oldest"]} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.click(textarea);
    await user.keyboard("{ArrowUp}"); // → recent (index 0)
    await user.keyboard("{ArrowUp}"); // → middle (index 1)
    await user.keyboard("{ArrowUp}"); // → oldest (index 2)
    expect(textarea).toHaveValue("oldest");
  });

  it("DESKTOP: Up stops at the oldest loaded message (does not wrap)", async () => {
    const user = userEvent.setup();
    render(<Composer onSend={vi.fn()} userMessageHistory={["only-one"]} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.click(textarea);
    await user.keyboard("{ArrowUp}"); // → only-one
    await user.keyboard("{ArrowUp}"); // already at oldest — stay
    expect(textarea).toHaveValue("only-one");
  });

  it("DESKTOP: Down on last line moves forward to the next most recent", async () => {
    const user = userEvent.setup();
    render(<Composer onSend={vi.fn()} userMessageHistory={["recent", "middle", "oldest"]} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.click(textarea);
    await user.keyboard("{ArrowUp}{ArrowUp}{ArrowUp}"); // → oldest (index 2)
    // Cursor is at start of "oldest" (first line). Move to last line then Down.
    await user.keyboard("{Control>}{End}{/Control}"); // jump to end (last line)
    await user.keyboard("{ArrowDown}"); // → middle (index 1)
    expect(textarea).toHaveValue("middle");
  });

  it("DESKTOP: Down past newest restores the pre-browse draft", async () => {
    const user = userEvent.setup();
    render(<Composer onSend={vi.fn()} userMessageHistory={["old"]} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.type(textarea, "my draft"); // draft captured
    await user.keyboard("{ArrowUp}"); // → "old" (index 0)
    expect(textarea).toHaveValue("old");
    // Move cursor to last line of "old" then Down past newest.
    await user.keyboard("{Control>}{End}{/Control}");
    await user.keyboard("{ArrowDown}"); // restore draft
    expect(textarea).toHaveValue("my draft");
  });

  it("DESKTOP: empty draft is preserved when browsing history from an empty box", async () => {
    const user = userEvent.setup();
    render(<Composer onSend={vi.fn()} userMessageHistory={["old"]} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.click(textarea);
    await user.keyboard("{ArrowUp}"); // → "old"
    await user.keyboard("{Control>}{End}{/Control}");
    await user.keyboard("{ArrowDown}"); // back to draft (empty)
    expect(textarea).toHaveValue("");
  });

  it("DESKTOP: Up when cursor is NOT on first line moves cursor (no history nav)", async () => {
    const user = userEvent.setup();
    render(<Composer onSend={vi.fn()} userMessageHistory={["should-not-load"]} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.type(textarea, "line1\nline2");
    // Cursor at end of "line2" (last line). Up should move within textarea,
    // NOT load history.
    await user.keyboard("{ArrowUp}");
    expect(textarea).toHaveValue("line1\nline2");
  });

  it("DESKTOP: Up on first line of multi-line draft DOES load history", async () => {
    const user = userEvent.setup();
    render(<Composer onSend={vi.fn()} userMessageHistory={["history-msg"]} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.type(textarea, "line1\nline2");
    // Move cursor to start of first line.
    await user.keyboard("{Control>}{Home}{/Control}");
    await user.keyboard("{ArrowUp}"); // first line → load history
    expect(textarea).toHaveValue("history-msg");
  });

  it("DESKTOP: Down when not browsing history does nothing (cursor moves normally)", async () => {
    const user = userEvent.setup();
    render(<Composer onSend={vi.fn()} userMessageHistory={["old"]} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.type(textarea, "hi");
    await user.keyboard("{ArrowDown}"); // not browsing → no-op for history
    expect(textarea).toHaveValue("hi");
  });

  it("DESKTOP: empty history list — Up/Down are no-ops", async () => {
    const user = userEvent.setup();
    render(<Composer onSend={vi.fn()} userMessageHistory={[]} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.type(textarea, "hi");
    await user.keyboard("{Control>}{Home}{/Control}");
    await user.keyboard("{ArrowUp}");
    expect(textarea).toHaveValue("hi");
  });

  it("DESKTOP: Up advances historyCursor even when draft text equals the loaded entry (React bail-out regression)", async () => {
    // A5 fix: React bails out of setText when the new value === old value.
    // Without a navigation tick in the cursor-effect deps, the cursor
    // wouldn't move and historyCursor would be ambiguous. Verify the
    // cursor lands at start after loading an identical-text entry, and
    // that a second Up on a multi-entry list advances correctly.
    const user = userEvent.setup();
    render(<Composer onSend={vi.fn()} userMessageHistory={["hello", "world"]} />);
    const textarea = screen.getByPlaceholderText("Type a message...") as HTMLTextAreaElement;
    // Draft a value EQUAL to history[0] ("hello" — the most recent).
    await user.type(textarea, "hello");
    expect(textarea).toHaveValue("hello");
    // Cursor is at end (5). Press Up — should load "hello" (same text)
    // AND move cursor to 0.
    await user.keyboard("{ArrowUp}");
    expect(textarea).toHaveValue("hello");
    expect(textarea.selectionStart).toBe(0);
    // Press Up again — historyCursor must have advanced to index 1 ("world").
    await user.keyboard("{ArrowUp}");
    expect(textarea).toHaveValue("world");
  });

  it("DESKTOP: typing after browsing does NOT strand the snapshot (Up reloads original)", async () => {
    // Per the revised state machine (F5 fix): edits don't reset navigation.
    // Pressing Up again reloads the history entry, discarding edits.
    const user = userEvent.setup();
    render(<Composer onSend={vi.fn()} userMessageHistory={["old"]} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.click(textarea);
    await user.keyboard("{ArrowUp}"); // → "old" (index 0)
    expect(textarea).toHaveValue("old");
    // User edits the loaded entry.
    await user.type(textarea, " EDITED");
    expect(textarea).toHaveValue("old EDITED");
    // Move to first line, press Up — reloads the entry, discarding the edit.
    await user.keyboard("{Control>}{Home}{/Control}");
    await user.keyboard("{ArrowUp}");
    expect(textarea).toHaveValue("old");
  });

  it("DESKTOP: history cursor resets when userMessageHistory prop reference changes", async () => {
    // F4 fix: a refetch mid-browse must not strand the cursor at a stale index.
    // After the prop changes, pressing Up starts fresh from the new array's
    // most-recent entry, NOT from the old cursor position.
    const user = userEvent.setup();
    const { rerender } = render(<Composer onSend={vi.fn()} userMessageHistory={["a", "b"]} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.click(textarea);
    await user.keyboard("{ArrowUp}{ArrowUp}"); // cursor at index 1, text = "b"
    // Refetch: prop changes to a brand new array.
    rerender(<Composer onSend={vi.fn()} userMessageHistory={["NEW-recent", "NEW-older"]} />);
    // Cursor was reset to -1 by the useEffect. Text is still "b" (stale).
    // Pressing Up now starts fresh → loads "NEW-recent" (index 0 of new array).
    await user.keyboard("{Control>}{Home}{/Control}");
    await user.keyboard("{ArrowUp}");
    expect(textarea).toHaveValue("NEW-recent");
  });


  it("MOBILE: Up/Down never navigate history (cursor moves normally)", async () => {
    setMobileMatchMedia(true);
    const user = userEvent.setup();
    render(<Composer onSend={vi.fn()} userMessageHistory={["should-not-load"]} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.type(textarea, "hi");
    await user.keyboard("{Control>}{Home}{/Control}");
    await user.keyboard("{ArrowUp}");
    expect(textarea).toHaveValue("hi");
  });

  it("DESKTOP: history navigation suppressed during IME composition", async () => {
    const user = userEvent.setup();
    render(<Composer onSend={vi.fn()} userMessageHistory={["old"]} />);
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.type(textarea, "ni");
    keyDown(textarea as HTMLElement, { key: "ArrowUp", isComposing: true });
    expect(textarea).toHaveValue("ni");
  });
});
