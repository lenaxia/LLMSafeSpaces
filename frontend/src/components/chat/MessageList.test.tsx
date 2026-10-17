import { describe, expect, it } from "vitest";
import { act, fireEvent, screen, waitFor } from "@testing-library/react";
import { render } from "../../test/utils";
import { MessageList } from "./MessageList";
import type { Message } from "../../api/types";
import type { ModelInfo } from "../../api/workspaces";

const messages: Message[] = [
  { id: "1", role: "user", parts: [{ type: "text", text: "Hello" }] },
  { id: "2", role: "assistant", parts: [{ type: "text", text: "Hi!" }] },
  { id: "3", role: "user", parts: [{ type: "text", text: "How are you?" }] },
];

describe("MessageList", () => {
  it("renders empty state when no messages", () => {
    render(<MessageList messages={[]} />);
    expect(screen.getByText("Send a message to start the conversation")).toBeInTheDocument();
  });

  it("renders messages", () => {
    render(<MessageList messages={messages} />);
    expect(screen.getByText("Hello")).toBeInTheDocument();
    expect(screen.getByText("Hi!")).toBeInTheDocument();
    expect(screen.getByText("How are you?")).toBeInTheDocument();
  });

  it("has accessible log role", () => {
    render(<MessageList messages={messages} />);
    expect(screen.getByRole("log")).toBeInTheDocument();
  });

  it("has aria-live polite for screen readers", () => {
    render(<MessageList messages={messages} />);
    expect(screen.getByRole("log")).toHaveAttribute("aria-live", "polite");
  });

  it("does not show jump-to-bottom button when at bottom", () => {
    render(<MessageList messages={messages} />);
    expect(screen.queryByLabelText("Scroll to bottom")).not.toBeInTheDocument();
  });

  it("shows jump-to-bottom button when scrolled away from bottom", async () => {
    render(<MessageList messages={messages} />);
    const scrollContainer = screen.getByRole("log");
    Object.defineProperty(scrollContainer, "scrollHeight", { value: 1000, configurable: true });
    Object.defineProperty(scrollContainer, "clientHeight", { value: 200, configurable: true });
    Object.defineProperty(scrollContainer, "scrollTop", { value: 0, writable: true, configurable: true });
    scrollContainer.dispatchEvent(new Event("scroll"));
    await waitFor(() => {
      expect(screen.getByLabelText("Scroll to bottom")).toBeInTheDocument();
    });
  });

  it("button says 'Resume tailing' during streaming", async () => {
    render(<MessageList messages={messages} streaming={true} streamingBubble={<div>streaming...</div>} />);
    const scrollContainer = screen.getByRole("log");
    Object.defineProperty(scrollContainer, "scrollHeight", { value: 1000, configurable: true });
    Object.defineProperty(scrollContainer, "clientHeight", { value: 200, configurable: true });
    Object.defineProperty(scrollContainer, "scrollTop", { value: 0, writable: true, configurable: true });
    scrollContainer.dispatchEvent(new Event("scroll"));
    await waitFor(() => {
      expect(screen.getByText("Resume tailing")).toBeInTheDocument();
    });
  });

  it("button says 'Jump to bottom' when not streaming", async () => {
    render(<MessageList messages={messages} />);
    const scrollContainer = screen.getByRole("log");
    Object.defineProperty(scrollContainer, "scrollHeight", { value: 1000, configurable: true });
    Object.defineProperty(scrollContainer, "clientHeight", { value: 200, configurable: true });
    Object.defineProperty(scrollContainer, "scrollTop", { value: 0, writable: true, configurable: true });
    scrollContainer.dispatchEvent(new Event("scroll"));
    await waitFor(() => {
      expect(screen.getByText("Jump to bottom")).toBeInTheDocument();
    });
  });

  it("renders streaming bubble when provided", () => {
    render(<MessageList messages={messages} streaming={true} streamingBubble={<div data-testid="stream-bubble">streaming content</div>} />);
    expect(screen.getByTestId("stream-bubble")).toBeInTheDocument();
  });

  it("renders trailingPrompts inside the scroll container", () => {
    render(<MessageList messages={messages} trailingPrompts={<div data-testid="test-prompt">prompt</div>} />);
    const scrollContainer = screen.getByRole("log");
    const prompt = screen.getByTestId("test-prompt");
    expect(scrollContainer.contains(prompt)).toBe(true);
  });

  it("renders trailingPrompts instead of empty state when there are no messages", () => {
    render(<MessageList messages={[]} trailingPrompts={<div data-testid="test-prompt">prompt</div>} />);
    expect(screen.getByTestId("test-prompt")).toBeInTheDocument();
    expect(screen.queryByText("Send a message to start the conversation")).not.toBeInTheDocument();
  });

  it("renders trailingPrompts after the streaming bubble", () => {
    render(
      <MessageList
        messages={messages}
        streaming={true}
        streamingBubble={<div data-testid="stream-bubble">streaming</div>}
        trailingPrompts={<div data-testid="test-prompt">prompt</div>}
      />,
    );
    const scrollContainer = screen.getByRole("log");
    const bubble = screen.getByTestId("stream-bubble");
    const prompt = screen.getByTestId("test-prompt");
    expect(scrollContainer.contains(bubble)).toBe(true);
    expect(scrollContainer.contains(prompt)).toBe(true);
    const children = Array.from(scrollContainer.querySelector(".flex.flex-col.gap-2")!.children);
    expect(children.indexOf(bubble)).toBeLessThan(children.indexOf(prompt));
  });

  it("scrolls to bottom when trailingPrompts appear (stickToBottom)", () => {
    const { rerender } = render(<MessageList messages={messages} />);
    const scrollContainer = screen.getByRole("log");
    Object.defineProperty(scrollContainer, "scrollHeight", { value: 1000, configurable: true });
    Object.defineProperty(scrollContainer, "clientHeight", { value: 200, configurable: true });
    Object.defineProperty(scrollContainer, "scrollTop", { value: 0, writable: true, configurable: true });
    rerender(<MessageList messages={messages} trailingPrompts={<div data-testid="test-prompt">prompt</div>} />);
    expect(scrollContainer.scrollTop).toBe(1000);
  });

  it("prevents horizontal scroll on the scroll container", () => {
    render(<MessageList messages={messages} />);
    const scrollContainer = screen.getByRole("log");
    expect(scrollContainer.className).toContain("overflow-x-hidden");
  });

  it("renders load earlier button when hasOlderMessages is true", () => {
    render(<MessageList messages={messages} hasOlderMessages={true} />);
    expect(screen.getByText("Load earlier messages")).toBeInTheDocument();
  });

  it("shows spinner when loading older messages", () => {
    render(<MessageList messages={messages} hasOlderMessages={true} loadingOlder={true} />);
    expect(screen.queryByText("Load earlier messages")).not.toBeInTheDocument();
    expect(document.querySelector(".animate-spin")).toBeInTheDocument();
  });

  describe("load-earlier discoverability (#440)", () => {
    // The button is rendered at the top of the (potentially very long)
    // scrolled message list. On a fresh load the view auto-scrolls to the
    // bottom — so without a sticky/visible cue, users with long histories
    // never see the affordance and believe earlier messages are missing.
    it("renders the load-earlier button inside the scroll container at the top", () => {
      const { container } = render(
        <MessageList messages={messages} hasOlderMessages={true} />,
      );
      const log = container.querySelector("[role='log']") as HTMLElement;
      const btn = screen.getByText("Load earlier messages");
      // Must live inside the scroll container so it scrolls into view
      // when the user reaches the top, AND must be the FIRST interactive
      // element in render order so screen readers reach it before history.
      expect(log.contains(btn)).toBe(true);
      const firstButtonInLog = log.querySelector("button");
      expect(firstButtonInLog).toBe(btn.closest("button"));
    });

    it("button is announced to assistive tech via accessible label", () => {
      render(<MessageList messages={messages} hasOlderMessages={true} />);
      // getByRole('button', {name:...}) verifies accessible name.
      const btn = screen.getByRole("button", { name: /load earlier messages/i });
      expect(btn).toBeInTheDocument();
    });

    it("button container is sticky at top of scroll viewport when older history exists", () => {
      const { container } = render(
        <MessageList messages={messages} hasOlderMessages={true} />,
      );
      // Sticky positioning ensures the affordance remains visible while the
      // user is anywhere in the upper portion of the conversation.
      const stickyEl = container.querySelector("[data-testid='load-earlier-anchor']");
      expect(stickyEl).not.toBeNull();
      expect(stickyEl!.className).toMatch(/sticky/);
      expect(stickyEl!.className).toMatch(/top-0/);
    });

    it("does not render the sticky anchor when hasOlderMessages is false", () => {
      const { container } = render(<MessageList messages={messages} />);
      expect(container.querySelector("[data-testid='load-earlier-anchor']")).toBeNull();
    });
  });

  describe("new messages divider (US-37.7)", () => {
    const messagesWithTimestamps: Message[] = [
      { id: "1", role: "user", parts: [{ type: "text", text: "Old message" }], createdAt: "2026-06-10T10:00:00Z" },
      { id: "2", role: "assistant", parts: [{ type: "text", text: "Old reply" }], createdAt: "2026-06-10T10:00:05Z" },
      { id: "3", role: "user", parts: [{ type: "text", text: "New message" }], createdAt: "2026-06-10T11:00:00Z" },
      { id: "4", role: "assistant", parts: [{ type: "text", text: "New reply" }], createdAt: "2026-06-10T11:00:05Z" },
    ];

    it("renders divider before first message after lastSeenAt", () => {
      render(<MessageList messages={messagesWithTimestamps} lastSeenAt="2026-06-10T10:30:00Z" />);
      const separator = screen.getByRole("separator");
      expect(separator).toBeInTheDocument();
      expect(separator).toHaveAttribute("aria-label", "New messages");
      expect(screen.getByText("New messages")).toBeInTheDocument();
    });

    it("does not render divider when lastSeenAt is undefined", () => {
      render(<MessageList messages={messagesWithTimestamps} />);
      expect(screen.queryByRole("separator")).not.toBeInTheDocument();
    });

    it("does not render divider when all messages are before lastSeenAt", () => {
      render(<MessageList messages={messagesWithTimestamps} lastSeenAt="2026-06-10T12:00:00Z" />);
      expect(screen.queryByRole("separator")).not.toBeInTheDocument();
    });

    it("applies 1-second clock skew buffer — message at exact threshold excluded", () => {
      const edgeMessages: Message[] = [
        { id: "1", role: "user", parts: [{ type: "text", text: "Boundary" }], createdAt: "2026-06-10T10:00:00.000Z" },
        { id: "2", role: "user", parts: [{ type: "text", text: "After" }], createdAt: "2026-06-10T10:00:01.500Z" },
      ];
      render(<MessageList messages={edgeMessages} lastSeenAt="2026-06-10T10:00:01.000Z" />);
      const separators = screen.queryAllByRole("separator");
      expect(separators.length).toBe(1);
    });

    it("does not crash when messages lack createdAt (#28)", () => {
      const noTimestamps: Message[] = [
        { id: "1", role: "user", parts: [{ type: "text", text: "No timestamp" }] },
        { id: "2", role: "assistant", parts: [{ type: "text", text: "Also none" }] },
      ];
      const { container } = render(<MessageList messages={noTimestamps} lastSeenAt="2026-06-10T10:00:00Z" />);
      expect(container.querySelector("[role='log']")).toBeInTheDocument();
      expect(screen.queryByRole("separator")).not.toBeInTheDocument();
    });
  });

  describe("pagination with divider regression (#51)", () => {
    const paginatedMessages: Message[] = Array.from({ length: 20 }, (_, i) => ({
      id: `msg-${i}`,
      role: i % 2 === 0 ? "user" as const : "assistant" as const,
      parts: [{ type: "text" as const, text: `Message ${i}` }],
      createdAt: new Date(2026, 5, 10, 10, 0, i * 60).toISOString(),
    }));

    it("divider renders correctly alongside load earlier button", () => {
      render(
        <MessageList
          messages={paginatedMessages}
          lastSeenAt={paginatedMessages[10]!.createdAt}
          hasOlderMessages={true}
        />,
      );
      expect(screen.getByText("Load earlier messages")).toBeInTheDocument();
      expect(screen.getByRole("separator")).toBeInTheDocument();
      expect(screen.getByText("New messages")).toBeInTheDocument();
    });
  });

  describe("model name resolution", () => {
    const models: ModelInfo[] = [
      { id: "gpt-4o", providerID: "openai", name: "GPT-4o", tier: "pro", freeTier: false, selected: false, enabled: true },
      { id: "claude-3.5-sonnet", providerID: "anthropic", name: "Claude 3.5 Sonnet", tier: "pro", freeTier: false, selected: false, enabled: true },
    ];

    const messagesWithModels: Message[] = [
      { id: "1", role: "user", parts: [{ type: "text", text: "Hello" }] },
      { id: "2", role: "assistant", parts: [{ type: "text", text: "Hi!" }], modelID: "gpt-4o" },
    ];

    it("resolves model name from models prop for assistant messages", () => {
      render(<MessageList messages={messagesWithModels} models={models} />);
      expect(screen.getByText(/gpt-4o/i)).toBeInTheDocument();
    });

    it("uses raw modelID as fallback when model not found in models list", () => {
      const msgWithUnknown: Message[] = [
        { id: "1", role: "assistant", parts: [{ type: "text", text: "Response" }], modelID: "unknown-model" },
      ];
      render(<MessageList messages={msgWithUnknown} models={models} />);
      expect(screen.getByTestId("message-model").textContent).toContain("unknown-model");
    });

    it("does not show model name for user messages even with models prop", () => {
      render(<MessageList messages={messagesWithModels} models={models} />);
      const helloBubble = screen.getByText("Hello").closest("[class*='group']");
      expect(helloBubble?.querySelector("[data-testid='message-model']")).toBeNull();
    });
  });

  // ── Scroll anchoring for "Load earlier messages" ───────────────────────
  //
  // Regression coverage for the fix that preserves the user's visual
  // position when older messages are prepended. Without the fix, the
  // browser keeps the same pixel scrollTop but the content above that
  // offset has grown, so the viewport shifts to different (newer) content
  // than what the user was reading — making "Load earlier" feel broken.

  describe("scroll anchoring on prepend", () => {
    function setScrollDims(el: HTMLElement, scrollHeight: number, scrollTop: number, clientHeight = 200) {
      Object.defineProperty(el, "scrollHeight", { value: scrollHeight, configurable: true });
      Object.defineProperty(el, "clientHeight", { value: clientHeight, configurable: true });
      Object.defineProperty(el, "scrollTop", { value: scrollTop, writable: true, configurable: true });
    }

    it("preserves visual position when older messages are prepended (user not at bottom)", async () => {
      const initialMsgs: Message[] = [
        { id: "m3", role: "user", parts: [{ type: "text", text: "page1" }] },
      ];
      const { rerender } = render(<MessageList messages={initialMsgs} />);
      const el = screen.getByRole("log");

      // Set the baseline dims and scroll away from bottom.
      setScrollDims(el, 1000, 200);
      el.dispatchEvent(new Event("scroll"));
      await waitFor(() => {
        expect(screen.getByLabelText("Scroll to bottom")).toBeInTheDocument();
      });

      // Force a no-op layout-effect pass that captures the current
      // scrollHeight (1000) as prevScrollHeightRef. We do this by
      // appending a message (same first id → no anchor fires) so the
      // mount-init effect's stale baseline gets refreshed.
      setScrollDims(el, 1000, 200);
      rerender(<MessageList messages={[
        ...initialMsgs,
        { id: "m4", role: "assistant", parts: [{ type: "text", text: "append" }] },
      ]} />);
      // stickToBottom still false (scrollTop=200 unchanged). Re-fire scroll
      // so the rAF picks it up after the rerender.
      setScrollDims(el, 1000, 200);
      el.dispatchEvent(new Event("scroll"));
      await waitFor(() => {
        expect(screen.getByLabelText("Scroll to bottom")).toBeInTheDocument();
      });

      // Now: prepend older messages. Redefine scrollHeight to simulate the
      // DOM growing (delta=500). stickToBottom is false, first id changed.
      const prepended: Message[] = [
        { id: "m1", role: "user", parts: [{ type: "text", text: "page2-old" }] },
        { id: "m2", role: "assistant", parts: [{ type: "text", text: "page2-old-reply" }] },
        ...initialMsgs,
      ];
      Object.defineProperty(el, "scrollHeight", { value: 1500, configurable: true });
      rerender(<MessageList messages={prepended} />);

      // Anchoring: scrollTop = 200 + (1500 - 1000) = 700.
      expect(el.scrollTop).toBe(700);
    });

    it("does NOT anchor when content is appended (first id unchanged, stickToBottom=false)", async () => {
      const initialMsgs: Message[] = [
        { id: "m1", role: "user", parts: [{ type: "text", text: "first" }] },
      ];
      const { rerender } = render(<MessageList messages={initialMsgs} />);
      const el = screen.getByRole("log");

      setScrollDims(el, 1000, 200);
      el.dispatchEvent(new Event("scroll"));
      await waitFor(() => {
        expect(screen.getByLabelText("Scroll to bottom")).toBeInTheDocument();
      });

      // Refresh prevScrollHeightRef via a same-first-id append.
      setScrollDims(el, 1000, 200);
      rerender(<MessageList messages={[
        ...initialMsgs,
        { id: "m1b", role: "assistant", parts: [{ type: "text", text: "x" }] },
      ]} />);

      // Append more content (first id unchanged). Browser-default keeps
      // scrollTop; our fix must NOT anchor here.
      setScrollDims(el, 1000, 200);
      el.dispatchEvent(new Event("scroll"));
      await waitFor(() => {
        expect(screen.getByLabelText("Scroll to bottom")).toBeInTheDocument();
      });

      const appended: Message[] = [
        ...initialMsgs,
        { id: "m2", role: "assistant", parts: [{ type: "text", text: "new reply" }] },
      ];
      Object.defineProperty(el, "scrollHeight", { value: 1200, configurable: true });
      rerender(<MessageList messages={appended} />);

      // Anchoring should NOT have fired (first id unchanged).
      expect(el.scrollTop).toBe(200);
    });

    it("still jumps to bottom when stickToBottom=true (anchoring skipped)", () => {
      const initialMsgs: Message[] = [
        { id: "m1", role: "user", parts: [{ type: "text", text: "first" }] },
      ];
      const { rerender } = render(<MessageList messages={initialMsgs} />);
      const el = screen.getByRole("log");
      // stickToBottom defaults to true; don't scroll away.

      setScrollDims(el, 1000, 0);
      const prepended: Message[] = [
        { id: "m0", role: "user", parts: [{ type: "text", text: "older" }] },
        ...initialMsgs,
      ];
      Object.defineProperty(el, "scrollHeight", { value: 1500, configurable: true });
      rerender(<MessageList messages={prepended} />);

      // stickToBottom branch: jump to bottom.
      expect(el.scrollTop).toBe(1500);
    });
  });

  // ── Streaming scroll-follow regression ─────────────────────────────────
  //
  // During streaming the MutationObserver auto-scrolls to the bottom on
  // every token while the user is "pinned" (stickToBottom=true). The user's
  // scroll-up intent must be recorded synchronously — otherwise a token
  // whose MutationObserver rAF is registered before the user's scroll rAF
  // yanks the viewport back to the bottom before the user's intent is ever
  // observed, trapping the user at the tail and making earlier content
  // unreadable while waiting for a response.

  describe("streaming scroll-follow (don't trap the user at the bottom)", () => {
    function setScrollDims(el: HTMLElement, scrollHeight: number, scrollTop: number, clientHeight = 200) {
      Object.defineProperty(el, "scrollHeight", { value: scrollHeight, configurable: true });
      Object.defineProperty(el, "clientHeight", { value: clientHeight, configurable: true });
      Object.defineProperty(el, "scrollTop", { value: scrollTop, writable: true, configurable: true });
    }

    // Append a DOM node inside the observed subtree (a streaming token) and
    // flush the MutationObserver microtask so its auto-scroll rAF registers.
    async function flushMutation(el: HTMLElement) {
      await act(async () => {
        el.querySelector(".flex.flex-col.gap-2")!.appendChild(document.createTextNode("token"));
      });
    }

    // Flush rAF timers so any pending observer/scroll auto-scrolls run.
    function flushTimers() {
      return new Promise((r) => setTimeout(r, 50));
    }

    it("does not yank the user back to the bottom after they scroll up during streaming", async () => {
      render(
        <MessageList messages={messages} streaming={true} streamingBubble={<div data-testid="sb">a</div>} />,
      );
      const el = screen.getByRole("log");
      // Pinned at the bottom (stickToBottom=true).
      setScrollDims(el, 1000, 800);

      // A streaming token arrives; its MutationObserver rAF is registered
      // BEFORE the user's scroll rAF below.
      await flushMutation(el);

      // The user scrolls up to read earlier content.
      await act(async () => {
        setScrollDims(el, 1000, 400);
        fireEvent.scroll(el);
        await flushTimers();
      });

      // Buggy code: the observer rAF fires first and pulls scrollTop back to
      // 1000. Fixed code: the user's intent was recorded synchronously, so
      // the observer leaves the viewport alone.
      expect(el.scrollTop).toBe(400);
    });

    it("keeps auto-scrolling to the bottom during streaming while the user stays pinned", async () => {
      render(
        <MessageList messages={messages} streaming={true} streamingBubble={<div data-testid="sb">a</div>} />,
      );
      const el = screen.getByRole("log");
      // At the bottom (1000 - 800 - 200 = 0 < threshold).
      setScrollDims(el, 1000, 800);

      await flushMutation(el);
      await act(async () => {
        await flushTimers();
      });

      expect(el.scrollTop).toBe(1000);
    });

    it("resumes following after the user scrolls back to the bottom via the jump button", async () => {
      render(
        <MessageList messages={messages} streaming={true} streamingBubble={<div data-testid="sb">a</div>} />,
      );
      const el = screen.getByRole("log");
      setScrollDims(el, 1000, 400);
      fireEvent.scroll(el);
      await waitFor(() => {
        expect(screen.getByText("Resume tailing")).toBeInTheDocument();
      });

      // User clicks "Resume tailing" → pinned again → next token auto-scrolls.
      await act(async () => {
        fireEvent.click(screen.getByText("Resume tailing"));
      });
      await flushMutation(el);
      await act(async () => {
        await flushTimers();
      });

      expect(el.scrollTop).toBe(1000);
    });
  });

  // ── #1720 — streaming tail snap-back loop ────────────────────────────
  //
  // Owner report: during streaming the timeline keeps snapping to the
  // bottom after the user scrolls away — repeatedly, intermittently.
  // Root mechanics pinned here (normative rules in issue #1720):
  //   R1 any genuine user scroll away disarms tailing durably;
  //   R2 tailing continues until user scrolls away OR streaming stops;
  //   R3 tailing NEVER silently re-arms — in particular NO near-bottom
  //      re-arm while a stream is active (that heuristic IS the loop);
  //      resume is explicit via the "Resume tailing" control;
  //   R4 our own programmatic scrolls are invisible to the user-scroll
  //      detector (guard flag) and never re-arm tail.

  describe("#1720 streaming tail snap-back loop", () => {
    function setScrollDims(el: HTMLElement, scrollHeight: number, scrollTop: number, clientHeight = 200) {
      Object.defineProperty(el, "scrollHeight", { value: scrollHeight, configurable: true });
      Object.defineProperty(el, "clientHeight", { value: clientHeight, configurable: true });
      Object.defineProperty(el, "scrollTop", { value: scrollTop, writable: true, configurable: true });
    }

    async function flushMutation(el: HTMLElement) {
      await act(async () => {
        el.querySelector(".flex.flex-col.gap-2")!.appendChild(document.createTextNode("token"));
      });
    }

    function flushTimers() {
      return new Promise((r) => setTimeout(r, 50));
    }

    async function scrollAway(el: HTMLElement, to = 400) {
      await act(async () => {
        setScrollDims(el, 1000, to);
        fireEvent.scroll(el);
        await flushTimers();
      });
      await waitFor(() => {
        expect(screen.getByLabelText("Scroll to bottom")).toBeInTheDocument();
      });
    }

    // R3 (RED on HEAD): scrolling back NEAR the bottom while a stream is
    // active must NOT re-arm tail. On HEAD, checkIfAtBottom recomputes
    // stickToBottom from a near-bottom read — the same read our own
    // programmatic snap produces — so the tail silently re-arms and the
    // next token batch yanks the viewport back (the reported loop).
    it("does not silently re-arm when the user scrolls back near the bottom mid-stream (R3)", async () => {
      render(
        <MessageList messages={messages} streaming={true} streamingBubble={<div data-testid="sb">a</div>} />,
      );
      const el = screen.getByRole("log");

      // User scrolls away → tail disarmed (pill visible).
      await scrollAway(el, 400);

      // User scrolls back to just above the bottom (within the 60px
      // threshold): 1000 - 900 - 200 = -100 < 60.
      await act(async () => {
        setScrollDims(el, 1000, 900);
        fireEvent.scroll(el);
        await flushTimers();
      });

      // Next token batch: must NOT snap. HEAD snaps to 1000 (loop).
      await flushMutation(el);
      await act(async () => {
        await flushTimers();
      });
      expect(el.scrollTop).toBe(900);
    });

    // R3 (RED on HEAD): a stream starting must never re-arm or snap a
    // disarmed tail. On HEAD the [streaming] effect calls
    // scrollToBottom() unconditionally on the streaming transition.
    it("does not snap or re-arm when a stream starts while the user is scrolled away (R3)", async () => {
      const { rerender } = render(
        <MessageList messages={messages} streaming={false} />,
      );
      const el = screen.getByRole("log");
      await scrollAway(el, 400);

      await act(async () => {
        rerender(
          <MessageList messages={messages} streaming={true} streamingBubble={<div data-testid="sb">a</div>} />,
        );
        await flushTimers();
      });

      // No snap on the streaming transition...
      expect(el.scrollTop).toBe(400);
      // ...and no re-arm: subsequent tokens leave the viewport alone.
      await flushMutation(el);
      await act(async () => {
        await flushTimers();
      });
      expect(el.scrollTop).toBe(400);
      // The explicit resume affordance is offered instead.
      expect(screen.getByText("Resume tailing")).toBeInTheDocument();
    });

    // R3 (pin, green on fix): the ONLY re-arm during a stream is the
    // explicit control — after a near-bottom visit that did NOT re-arm,
    // clicking "Resume tailing" re-arms and follows again.
    it("re-arms only via the explicit Resume tailing control mid-stream (R3)", async () => {
      render(
        <MessageList messages={messages} streaming={true} streamingBubble={<div data-testid="sb">a</div>} />,
      );
      const el = screen.getByRole("log");
      await scrollAway(el, 400);

      // Drift near the bottom without re-arming...
      await act(async () => {
        setScrollDims(el, 1000, 900);
        fireEvent.scroll(el);
        await flushTimers();
      });

      // ...then click the explicit affordance.
      await act(async () => {
        fireEvent.click(screen.getByText("Resume tailing"));
      });
      expect(el.scrollTop).toBe(1000);

      // Explicitly re-armed → follows the next token batch.
      await flushMutation(el);
      await act(async () => {
        await flushTimers();
      });
      expect(el.scrollTop).toBe(1000);
    });

    // R4 (pin): scroll events generated by OUR programmatic snaps are
    // invisible to the user-intent detector — they neither disarm an
    // armed tail nor linger to swallow the next genuine user scroll.
    it("programmatic snap scroll events never disarm an armed tail (R4)", async () => {
      render(
        <MessageList messages={messages} streaming={true} streamingBubble={<div data-testid="sb">a</div>} />,
      );
      const el = screen.getByRole("log");
      setScrollDims(el, 1000, 800); // armed, at bottom

      // Token snap fires (assignment), then the browser dispatches the
      // scroll event for it (jsdom does not — we simulate the browser).
      await flushMutation(el);
      await act(async () => {
        await flushTimers();
      });
      expect(el.scrollTop).toBe(1000); // the snap happened

      await act(async () => {
        fireEvent.scroll(el); // the snap's own scroll event
        await flushTimers();
      });

      // Guard consumed it → tail still armed → next token still follows.
      await flushMutation(el);
      await act(async () => {
        await flushTimers();
      });
      expect(el.scrollTop).toBe(1000);
      expect(screen.queryByLabelText("Scroll to bottom")).not.toBeInTheDocument();
    });

    it("the guard does not swallow the next genuine user scroll after a snap (R4)", async () => {
      render(
        <MessageList messages={messages} streaming={true} streamingBubble={<div data-testid="sb">a</div>} />,
      );
      const el = screen.getByRole("log");
      setScrollDims(el, 1000, 800);

      await flushMutation(el);
      await act(async () => {
        await flushTimers();
      });
      await act(async () => {
        fireEvent.scroll(el); // snap's own event — consumed by the guard
        await flushTimers();
      });

      // The very NEXT scroll event is a genuine user scroll away →
      // must disarm immediately (the guard must not have lingered).
      await scrollAway(el, 400);
      await flushMutation(el);
      await act(async () => {
        await flushTimers();
      });
      expect(el.scrollTop).toBe(400);
    });

    // Convention pin (green on HEAD and fix): while IDLE, scrolling back
    // to the bottom re-arms follow — the near-bottom arm stays legal when
    // no stream is active. Anchors R3's "while a stream is active" scope.
    it("idle near-bottom scroll still re-arms follow (existing convention, R3 scope)", async () => {
      const { rerender } = render(<MessageList messages={messages} />);
      const el = screen.getByRole("log");
      await scrollAway(el, 400);

      // Scroll back to the bottom while idle.
      await act(async () => {
        setScrollDims(el, 1000, 850); // 1000-850-200 = -50 < 60
        fireEvent.scroll(el);
        await flushTimers();
      });
      await waitFor(() => {
        expect(screen.queryByLabelText("Scroll to bottom")).not.toBeInTheDocument();
      });

      // Appended content follows again.
      setScrollDims(el, 1200, 850);
      rerender(<MessageList messages={[...messages, { id: "m9", role: "user", parts: [{ type: "text", text: "next" }] }]} />);
      expect(el.scrollTop).toBe(1200);
    });

    // Acceptance: stream end settles tail state.
    it("stream end while disarmed: no snap, free position kept, control flips to Jump to bottom", async () => {
      const { rerender } = render(
        <MessageList messages={messages} streaming={true} streamingBubble={<div data-testid="sb">a</div>} />,
      );
      const el = screen.getByRole("log");
      await scrollAway(el, 400);

      await act(async () => {
        rerender(<MessageList messages={messages} streaming={false} />);
        await flushTimers();
      });

      expect(el.scrollTop).toBe(400);
      expect(screen.getByText("Jump to bottom")).toBeInTheDocument();
      expect(screen.queryByText("Resume tailing")).not.toBeInTheDocument();
    });

    it("stream end while tailing: stays at bottom, control hidden, state settled", async () => {
      const { rerender } = render(
        <MessageList messages={messages} streaming={true} streamingBubble={<div data-testid="sb">a</div>} />,
      );
      const el = screen.getByRole("log");
      setScrollDims(el, 1000, 800); // armed at bottom

      await act(async () => {
        rerender(<MessageList messages={messages} streaming={false} />);
        await flushTimers();
      });

      expect(el.scrollTop).toBe(800); // no scroll churn on the flip
      expect(screen.queryByLabelText("Scroll to bottom")).not.toBeInTheDocument();
    });
  });
});
