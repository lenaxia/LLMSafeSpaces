import { describe, expect, it, vi } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "../../test/utils";
import { ChatView } from "./ChatView";
import { PermissionPrompt } from "./PermissionPrompt";
import { QuestionPrompt } from "./QuestionPrompt";
import type { Message, PermissionRequest, QuestionRequest } from "../../api/types";
import type { ModelInfo } from "../../api/workspaces";

vi.mock("../../api/input", () => ({
  inputApi: {
    permissionReply: vi.fn().mockResolvedValue(true),
    questionReply: vi.fn().mockResolvedValue(true),
    questionReject: vi.fn().mockResolvedValue(true),
  },
}));

describe("ChatView", () => {
  const defaultProps = {
    messages: [] as Message[],
    streaming: false,
    streamParts: [] as Array<{ type: "thinking" | "text" | "tool"; text: string }>,
    disabled: false,
    onSend: vi.fn(),
    onAbort: vi.fn(),
  };

  it("renders message list and composer", () => {
    render(<ChatView {...defaultProps} />);
    expect(screen.getByPlaceholderText("Type a message...")).toBeInTheDocument();
  });

  it("renders messages", () => {
    const messages: Message[] = [
      { id: "1", role: "user", parts: [{ type: "text", text: "Hello" }] },
      { id: "2", role: "assistant", parts: [{ type: "text", text: "Hi!" }] },
    ];
    render(<ChatView {...defaultProps} messages={messages} />);
    expect(screen.getByText("Hello")).toBeInTheDocument();
    expect(screen.getByText("Hi!")).toBeInTheDocument();
  });

  it("shows streaming indicator when streaming", () => {
    render(<ChatView {...defaultProps} streaming={true} />);
    const dots = document.querySelectorAll(".animate-bounce");
    expect(dots.length).toBe(3);
  });

  it("shows abort button when streaming", () => {
    render(<ChatView {...defaultProps} streaming={true} />);
    expect(screen.getByRole("button", { name: /stop/i })).toBeInTheDocument();
  });

  it("does not show abort button when not streaming", () => {
    render(<ChatView {...defaultProps} streaming={false} />);
    expect(screen.queryByRole("button", { name: /stop/i })).not.toBeInTheDocument();
  });

  it("calls onAbort when abort button clicked", async () => {
    const user = userEvent.setup();
    const onAbort = vi.fn();
    render(<ChatView {...defaultProps} streaming={true} onAbort={onAbort} />);
    await user.click(screen.getByRole("button", { name: /stop/i }));
    expect(onAbort).toHaveBeenCalled();
  });

  it("calls onSend when message submitted", async () => {
    const user = userEvent.setup();
    const onSend = vi.fn();
    render(<ChatView {...defaultProps} onSend={onSend} />);
    await user.type(screen.getByPlaceholderText("Type a message..."), "test");
    await user.click(screen.getByRole("button", { name: "Send message" }));
    expect(onSend).toHaveBeenCalledWith("test");
  });

  it("disables composer when disabled", () => {
    render(<ChatView {...defaultProps} disabled={true} />);
    expect(screen.getByPlaceholderText("Type a message...")).toBeDisabled();
  });

  // ── viewOnly (subtask read-only) ─────────────────────────────────────────

  it("hides the composer when viewOnly is true", () => {
    render(<ChatView {...defaultProps} viewOnly={true} />);
    expect(screen.queryByPlaceholderText("Type a message...")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /send/i })).not.toBeInTheDocument();
  });

  it("shows the default view-only message when viewOnly is true", () => {
    render(<ChatView {...defaultProps} viewOnly={true} />);
    expect(screen.getByRole("status")).toBeInTheDocument();
    expect(screen.getByText(/Subtasks are view-only/i)).toBeInTheDocument();
  });

  it("shows a custom view-only message when provided", () => {
    render(<ChatView {...defaultProps} viewOnly={true} viewOnlyMessage="Custom read-only reason" />);
    expect(screen.getByText("Custom read-only reason")).toBeInTheDocument();
  });

  it("does not render the queue section when viewOnly is true", () => {
    render(
      <ChatView
        {...defaultProps}
        viewOnly={true}
        queuedMessages={[{ id: "q1", text: "queued", status: "pending", sessionId: "sess-1" }]}
        onQueueRetry={vi.fn()}
        onQueueDismiss={vi.fn()}
      />,
    );
    expect(screen.queryByText("queued")).not.toBeInTheDocument();
  });

  it("still renders messages when viewOnly is true (read-only view)", () => {
    const messages: Message[] = [
      { id: "1", role: "assistant", parts: [{ type: "text", text: "Subtask output" }] },
    ];
    render(<ChatView {...defaultProps} viewOnly={true} messages={messages} />);
    expect(screen.getByText("Subtask output")).toBeInTheDocument();
  });

  it("renders the composer when viewOnly is false (default)", () => {
    render(<ChatView {...defaultProps} viewOnly={false} />);
    expect(screen.getByPlaceholderText("Type a message...")).toBeInTheDocument();
  });

  it("still renders prompts when viewOnly is true (agent-driven, not user chatting)", () => {
    render(
      <ChatView
        {...defaultProps}
        viewOnly={true}
        prompts={<div>Agent has a question</div>}
      />,
    );
    expect(screen.getByText("Agent has a question")).toBeInTheDocument();
    expect(screen.getByRole("status")).toBeInTheDocument();
  });

  it("shows streamed text parts", () => {
    render(<ChatView {...defaultProps} streaming={true} streamParts={[{ type: "text", text: "Partial response..." }]} />);
    expect(screen.getByText("Partial response...")).toBeInTheDocument();
  });

  it("shows streamed thinking parts", () => {
    render(<ChatView {...defaultProps} streaming={true} streamParts={[{ type: "thinking", text: "Thinking deeply..." }]} />);
    expect(screen.getByText("Thinking deeply...")).toBeInTheDocument();
  });

  it("does not show streaming bubble when no parts", () => {
    render(<ChatView {...defaultProps} streaming={true} streamParts={[]} />);
    expect(screen.queryByText("Thinking")).not.toBeInTheDocument();
  });

  it("partitions streaming parts into separate bubbles by messageID", () => {
    // Opencode emits parts across multiple assistant messages within one
    // turn (each turn ends at a tool call, then a new message begins).
    // Streaming render must match the post-refresh render: one bubble per
    // opencode messageID.
    const { container } = render(
      <ChatView
        {...defaultProps}
        streaming={true}
        streamParts={[
          { type: "text", text: "First message text.", messageID: "msg_a" },
          { type: "tool", text: "bash: build", toolState: "completed", messageID: "msg_a" },
          { type: "text", text: "Second message text.", messageID: "msg_b" },
          { type: "tool", text: "bash: test", toolState: "completed", messageID: "msg_b" },
        ]}
      />,
    );
    // Two separate assistant bubbles rendered — one per messageID.
    const bubbles = container.querySelectorAll(".bg-muted.text-foreground");
    expect(bubbles.length).toBe(2);
    // First bubble contains only msg_a content.
    expect(bubbles[0]!.textContent).toContain("First message text.");
    expect(bubbles[0]!.textContent).not.toContain("Second message text.");
    // Second bubble contains only msg_b content.
    expect(bubbles[1]!.textContent).toContain("Second message text.");
    expect(bubbles[1]!.textContent).not.toContain("First message text.");
  });

  it("groups parts without messageID into a single bubble (backward compat)", () => {
    const { container } = render(
      <ChatView
        {...defaultProps}
        streaming={true}
        streamParts={[
          { type: "text", text: "one" },
          { type: "text", text: "two" },
        ]}
      />,
    );
    const bubbles = container.querySelectorAll(".bg-muted.text-foreground");
    expect(bubbles.length).toBe(1);
  });

  it("preserves messageID encounter order across bubbles", () => {
    const { container } = render(
      <ChatView
        {...defaultProps}
        streaming={true}
        streamParts={[
          { type: "text", text: "A-1", messageID: "msg_a" },
          { type: "text", text: "B-1", messageID: "msg_b" },
          { type: "text", text: "A-2", messageID: "msg_a" },
        ]}
      />,
    );
    const bubbles = container.querySelectorAll(".bg-muted.text-foreground");
    expect(bubbles.length).toBe(2);
    // msg_a was encountered first, so its bubble is rendered first;
    // A-2 is grouped into msg_a's bubble.
    expect(bubbles[0]!.textContent).toContain("A-1");
    expect(bubbles[0]!.textContent).toContain("A-2");
    expect(bubbles[1]!.textContent).toContain("B-1");
  });

  it("passes models to MessageList for model name resolution", () => {
    const models: ModelInfo[] = [
      { id: "gpt-4o", providerID: "openai", name: "GPT-4o", tier: "pro", freeTier: false, selected: false, enabled: true },
    ];
    const messages: Message[] = [
      { id: "1", role: "assistant", parts: [{ type: "text", text: "Response" }], modelID: "gpt-4o" },
    ];
    render(<ChatView {...defaultProps} messages={messages} models={models} />);
    expect(screen.getByText(/gpt-4o/i)).toBeInTheDocument();
  });

  it("renders prompts inside the scroll container (inline with messages)", () => {
    const messages: Message[] = [{ id: "1", role: "user", parts: [{ type: "text", text: "Hello" }] }];
    render(
      <ChatView
        {...defaultProps}
        messages={messages}
        prompts={<div data-testid="test-prompt">Permission needed</div>}
      />,
    );
    const scrollContainer = screen.getByRole("log");
    const prompt = screen.getByTestId("test-prompt");
    expect(scrollContainer.contains(prompt)).toBe(true);
  });

  it("renders prompts even when there are no messages", () => {
    render(
      <ChatView
        {...defaultProps}
        messages={[]}
        prompts={<div data-testid="test-prompt">Question</div>}
      />,
    );
    expect(screen.getByTestId("test-prompt")).toBeInTheDocument();
  });

  it("renders real PermissionPrompt inside the scroll container and allows interaction", async () => {
    const onResolved = vi.fn();
    const request: PermissionRequest = {
      id: "per_int",
      session_id: "ses_1",
      permission: "shell",
      patterns: ["rm -rf /tmp/cache"],
    };
    render(
      <ChatView
        {...defaultProps}
        messages={[{ id: "1", role: "user", parts: [{ type: "text", text: "do something" }] }]}
        prompts={<PermissionPrompt workspaceId="ws-1" request={request} onResolved={onResolved} />}
      />,
    );
    const scrollContainer = screen.getByRole("log");
    expect(scrollContainer.contains(screen.getByText("Run shell command"))).toBe(true);
    expect(scrollContainer.contains(screen.getByText("rm -rf /tmp/cache"))).toBe(true);
    fireEvent.click(screen.getByText("Allow once"));
    await waitFor(() => expect(onResolved).toHaveBeenCalled());
  });

  it("renders real QuestionPrompt inside the scroll container and allows interaction", async () => {
    const onResolved = vi.fn();
    const request: QuestionRequest = {
      id: "que_int",
      session_id: "ses_1",
      questions: [{
        header: "Pick one",
        question: "Which language?",
        options: [{ label: "Go", description: "fast" }],
      }],
    };
    render(
      <ChatView
        {...defaultProps}
        messages={[{ id: "1", role: "user", parts: [{ type: "text", text: "help" }] }]}
        prompts={<QuestionPrompt workspaceId="ws-1" request={request} onResolved={onResolved} />}
      />,
    );
    const scrollContainer = screen.getByRole("log");
    expect(scrollContainer.contains(screen.getByText("Which language?"))).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Go" }));
    fireEvent.click(screen.getByText("Submit answers"));
    await waitFor(() => expect(onResolved).toHaveBeenCalled());
  });
});
