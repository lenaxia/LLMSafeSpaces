import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "../../test/utils";
import { MessageBubble, extractMessageText } from "./MessageBubble";
import type { Message } from "../../api/types";

const FIXED_NOW = new Date("2024-06-15T12:00:00Z").getTime();

vi.mock("../../hooks/useNow", () => ({
  useNow: () => FIXED_NOW,
}));

describe("MessageBubble", () => {
  it("renders user message with primary background", () => {
    const msg: Message = { id: "1", role: "user", parts: [{ type: "text", text: "Hello" }] };
    render(<MessageBubble message={msg} />);
    expect(screen.getByText("Hello")).toBeInTheDocument();
    const bubble = screen.getByText("Hello").closest("div[class*='bg-']");
    expect(bubble?.className).toContain("bg-primary");
  });

  it("renders assistant message with muted background", () => {
    const msg: Message = { id: "2", role: "assistant", parts: [{ type: "text", text: "Hi there" }] };
    render(<MessageBubble message={msg} />);
    expect(screen.getByText("Hi there")).toBeInTheDocument();
    const bubble = screen.getByText("Hi there").closest("div[class*='bg-']");
    expect(bubble?.className).toContain("bg-muted");
  });

  it("aligns user messages to the right", () => {
    const msg: Message = { id: "1", role: "user", parts: [{ type: "text", text: "Right" }] };
    render(<MessageBubble message={msg} />);
    const wrapper = screen.getByText("Right").closest("div[class*='justify-']");
    expect(wrapper?.className).toContain("justify-end");
  });

  it("aligns assistant messages to the left", () => {
    const msg: Message = { id: "2", role: "assistant", parts: [{ type: "text", text: "Left" }] };
    render(<MessageBubble message={msg} />);
    const wrapper = screen.getByText("Left").closest("div[class*='justify-']");
    expect(wrapper?.className).toContain("justify-start");
  });

  it("renders multiple parts", () => {
    const msg: Message = { id: "3", role: "assistant", parts: [{ type: "text", text: "Part 1" }, { type: "text", text: "Part 2" }] };
    render(<MessageBubble message={msg} />);
    expect(screen.getByText("Part 1")).toBeInTheDocument();
    expect(screen.getByText("Part 2")).toBeInTheDocument();
  });

  describe("overflow containment", () => {
    it("has overflow-hidden to prevent content from escaping bubble", () => {
      const msg: Message = { id: "4", role: "assistant", parts: [{ type: "text", text: "content" }] };
      render(<MessageBubble message={msg} />);
      const bubble = screen.getByText("content").closest("div[class*='overflow-hidden']");
      expect(bubble).not.toBeNull();
    });

    it("has min-w-0 to allow flex shrinking below content width", () => {
      const msg: Message = { id: "5", role: "assistant", parts: [{ type: "text", text: "content" }] };
      render(<MessageBubble message={msg} />);
      const bubble = screen.getByText("content").closest("div[class*='min-w-0']");
      expect(bubble).not.toBeNull();
    });

    it("has break-words to wrap long unbroken strings", () => {
      const msg: Message = { id: "6", role: "user", parts: [{ type: "text", text: "content" }] };
      render(<MessageBubble message={msg} />);
      const bubble = screen.getByText("content").closest("div[class*='break-words']");
      expect(bubble).not.toBeNull();
    });

    it("uses responsive max-width (90% mobile, 80% desktop)", () => {
      const msg: Message = { id: "7", role: "assistant", parts: [{ type: "text", text: "content" }] };
      const { container } = render(<MessageBubble message={msg} />);
      const bubble = container.querySelector("div[class*='max-w-\\[90\\%\\]']");
      expect(bubble).not.toBeNull();
      expect(bubble?.className).toContain("sm:max-w-[80%]");
    });
  });

  describe("copy button", () => {
    beforeEach(() => {
      // @ts-expect-error jsdom doesn't implement clipboard
      navigator.clipboard = { writeText: () => Promise.resolve() };
    });

    afterEach(() => {
      // @ts-expect-error — resetting test spy on read-only property
      delete navigator.clipboard;
    });

    it("renders a copy button with accessible label", () => {
      const msg: Message = { id: "c1", role: "assistant", parts: [{ type: "text", text: "Copy me" }] };
      render(<MessageBubble message={msg} />);
      expect(screen.getByRole("button", { name: /copy message/i })).toBeInTheDocument();
    });

    it("shows check icon after successful copy", async () => {
      const user = userEvent.setup();
      const msg: Message = { id: "c3", role: "assistant", parts: [{ type: "text", text: "Check test" }] };
      render(<MessageBubble message={msg} />);
      await user.click(screen.getByRole("button", { name: /copy message/i }));
      await waitFor(() => {
        expect(screen.getByRole("button", { name: /copied/i })).toBeInTheDocument();
      });
    });

    it("reverts to copy icon after timeout", async () => {
      vi.useFakeTimers();
      const msg: Message = { id: "c4", role: "assistant", parts: [{ type: "text", text: "Timeout test" }] };
      render(<MessageBubble message={msg} />);
      const btn = screen.getByRole("button", { name: /copy message/i });
      await act(async () => { btn.click(); });
      await act(async () => { await vi.advanceTimersByTimeAsync(0); });
      expect(screen.getByRole("button", { name: /copied/i })).toBeInTheDocument();
      await act(async () => { vi.advanceTimersByTime(2100); });
      expect(screen.getByRole("button", { name: /copy message/i })).toBeInTheDocument();
      vi.useRealTimers();
    });

    it("does not show check icon when clipboard write fails", async () => {
      // @ts-expect-error — overriding read-only clipboard for test
      navigator.clipboard = { writeText: () => Promise.reject(new Error("denied")) };
      const msg: Message = { id: "c5", role: "assistant", parts: [{ type: "text", text: "Fail test" }] };
      render(<MessageBubble message={msg} />);
      const btn = screen.getByRole("button", { name: /copy message/i });
      await act(async () => { btn.click(); });
      expect(screen.getByRole("button", { name: /copy message/i })).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: /copied/i })).not.toBeInTheDocument();
    });
  });

  describe("extractMessageText", () => {
    it("extracts text from text parts", () => {
      expect(extractMessageText([{ type: "text", text: "hello" }])).toBe("hello");
    });

    it("joins multiple parts with double newline", () => {
      expect(extractMessageText([
        { type: "text", text: "a" },
        { type: "text", text: "b" },
      ])).toBe("a\n\nb");
    });

    it("prefixes thinking parts", () => {
      expect(extractMessageText([{ type: "thinking", text: "hmm" }])).toBe("[Thinking]\nhmm");
    });

    it("formats tool parts with output", () => {
      expect(extractMessageText([{
        type: "tool_use",
        text: "bash",
        toolOutput: "result",
      }])).toBe("[Tool: bash]\nOutput: result");
    });

    // Owner report 2026-11-12 (tsk-copper-archer-rushes): copying a failed
    // tool's transcript text must carry the error (the owner was forced to
    // re-quote errors manually).
    it("formats failed tool parts with their error", () => {
      expect(extractMessageText([{
        type: "tool_use",
        text: "bash",
        toolError: "exit status 1",
      }])).toBe("[Tool: bash]\nError: exit status 1");
    });

    it("formats failed tool parts with error and output", () => {
      expect(extractMessageText([{
        type: "tool_use",
        text: "bash",
        toolError: "exit status 1",
        toolOutput: "stderr noise",
      }])).toBe("[Tool: bash]\nError: exit status 1\nOutput: stderr noise");
    });

    it("formats tool parts without output", () => {
      expect(extractMessageText([{ type: "tool_use", text: "read" }])).toBe("[Tool: read]");
    });

    it("skips empty parts", () => {
      expect(extractMessageText([
        { type: "text", text: "keep" },
        { type: "text" },
      ])).toBe("keep");
    });
  });

  describe("timestamp", () => {
    it("displays 'just now' for a message created at FIXED_NOW", () => {
      const msg: Message = {
        id: "t1",
        role: "user",
        parts: [{ type: "text", text: "Timestamped" }],
        createdAt: new Date(FIXED_NOW).toISOString(),
      };
      render(<MessageBubble message={msg} />);
      expect(screen.getByText("just now")).toBeInTheDocument();
    });

    it("displays minutes ago for a message created 5 minutes before FIXED_NOW", () => {
      const msg: Message = {
        id: "t2",
        role: "user",
        parts: [{ type: "text", text: "Five min" }],
        createdAt: new Date(FIXED_NOW - 5 * 60_000).toISOString(),
      };
      render(<MessageBubble message={msg} />);
      expect(screen.getByTestId("message-timestamp").textContent).toBe("5m ago");
    });

    it("does not render timestamp when createdAt is absent", () => {
      const msg: Message = { id: "t3", role: "user", parts: [{ type: "text", text: "No time" }] };
      const { container } = render(<MessageBubble message={msg} />);
      expect(container.querySelectorAll("[data-testid='message-timestamp']")).toHaveLength(0);
    });

    it("displays clock time for messages older than 60 minutes", () => {
      const msg: Message = {
        id: "t4",
        role: "assistant",
        parts: [{ type: "text", text: "Older" }],
        createdAt: new Date(FIXED_NOW - 2 * 60 * 60 * 1000).toISOString(),
      };
      render(<MessageBubble message={msg} />);
      // Older than 60m on same day → locale time string e.g. "10:00 AM"
      expect(screen.getByTestId("message-timestamp").textContent).toMatch(/\d{1,2}:\d{2}/);
    });
  });

  describe("model name", () => {
    it("displays model name for assistant messages when provided", () => {
      const msg: Message = {
        id: "m1",
        role: "assistant",
        parts: [{ type: "text", text: "Response" }],
        createdAt: new Date(FIXED_NOW).toISOString(),
        modelID: "gpt-4o",
      };
      render(<MessageBubble message={msg} modelName="GPT-4o" />);
      expect(screen.getByText(/gpt-4o/i)).toBeInTheDocument();
    });

    it("does not display model name when modelName prop is undefined", () => {
      const msg: Message = {
        id: "m2",
        role: "assistant",
        parts: [{ type: "text", text: "No model" }],
      };
      const { container } = render(<MessageBubble message={msg} />);
      expect(container.querySelectorAll("[data-testid='message-model']")).toHaveLength(0);
    });

    it("does not display model name for user messages", () => {
      const msg: Message = {
        id: "m3",
        role: "user",
        parts: [{ type: "text", text: "User msg" }],
      };
      const { container } = render(<MessageBubble message={msg} modelName="GPT-4o" />);
      expect(container.querySelectorAll("[data-testid='message-model']")).toHaveLength(0);
    });
  });
});
