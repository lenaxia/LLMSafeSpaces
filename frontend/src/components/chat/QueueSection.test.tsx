import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueueSection } from "./QueueSection";
import type { QueuedMessage } from "../../hooks/useMessageQueue";

function makeMsg(overrides: Partial<QueuedMessage> = {}): QueuedMessage {
  return {
    id: "msg_test",
    text: "hello",
    status: "pending",
    sessionId: "ses_1",
    ...overrides,
  };
}

describe("QueueSection", () => {
  it("renders nothing when messages array is empty and not open", () => {
    const { container } = render(<QueueSection messages={[]} onRetry={vi.fn()} onDismiss={vi.fn()} isMobile={false} />);
    expect(container.innerHTML).toBe("");
  });

  it("renders pending messages as user-aligned chat bubbles", () => {
    render(
      <QueueSection
        messages={[makeMsg({ text: "fix the bug" }), makeMsg({ id: "msg_2", text: "add tests" })]}
        onRetry={vi.fn()}
        onDismiss={vi.fn()}
        isMobile={false}
      />,
    );
    expect(screen.getByText("fix the bug")).toBeInTheDocument();
    expect(screen.getByText("add tests")).toBeInTheDocument();
  });

  it("renders error messages with line-through text", () => {
    render(
      <QueueSection
        messages={[makeMsg({ status: "error", error: "failed" })]}
        onRetry={vi.fn()}
        onDismiss={vi.fn()}
        isMobile={false}
      />,
    );
    const el = screen.getByText("hello");
    expect(el.closest("p")?.className).toContain("line-through");
  });

  it("shows retry button on error messages", () => {
    render(
      <QueueSection
        messages={[makeMsg({ status: "error", error: "failed" })]}
        onRetry={vi.fn()}
        onDismiss={vi.fn()}
        isMobile={false}
      />,
    );
    expect(screen.getByLabelText("Retry")).toBeInTheDocument();
  });

  it("shows dismiss button on error messages", () => {
    render(
      <QueueSection
        messages={[makeMsg({ status: "error", error: "failed" })]}
        onRetry={vi.fn()}
        onDismiss={vi.fn()}
        isMobile={false}
      />,
    );
    expect(screen.getByLabelText("Dismiss")).toBeInTheDocument();
  });

  it("does not show retry/dismiss on pending messages", () => {
    render(
      <QueueSection
        messages={[makeMsg()]}
        onRetry={vi.fn()}
        onDismiss={vi.fn()}
        isMobile={false}
      />,
    );
    expect(screen.queryByLabelText("Retry")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Dismiss")).not.toBeInTheDocument();
  });

  it("retry click calls onRetry with message id", async () => {
    const user = userEvent.setup();
    const onRetry = vi.fn();
    render(
      <QueueSection
        messages={[makeMsg({ status: "error", error: "failed" })]}
        onRetry={onRetry}
        onDismiss={vi.fn()}
        isMobile={false}
      />,
    );
    await user.click(screen.getByLabelText("Retry"));
    expect(onRetry).toHaveBeenCalledWith("msg_test");
  });

  it("dismiss click calls onDismiss with message id", async () => {
    const user = userEvent.setup();
    const onDismiss = vi.fn();
    render(
      <QueueSection
        messages={[makeMsg({ status: "error", error: "failed" })]}
        onRetry={vi.fn()}
        onDismiss={onDismiss}
        isMobile={false}
      />,
    );
    await user.click(screen.getByLabelText("Dismiss"));
    expect(onDismiss).toHaveBeenCalledWith("msg_test");
  });

  it("shows queued count in toggle button", () => {
    render(
      <QueueSection
        messages={[makeMsg({ text: "first" }), makeMsg({ id: "msg_2", text: "second" })]}
        onRetry={vi.fn()}
        onDismiss={vi.fn()}
        isMobile={false}
      />,
    );
    expect(screen.getByText("2 messages queued")).toBeInTheDocument();
  });

  it("auto-closes drawer when messages are cleared", async () => {
    const { rerender } = render(
      <QueueSection
        messages={[makeMsg()]}
        onRetry={vi.fn()}
        onDismiss={vi.fn()}
        isMobile={false}
      />,
    );
    expect(screen.getByText("1 message queued")).toBeInTheDocument();

    rerender(
      <QueueSection
        messages={[]}
        onRetry={vi.fn()}
        onDismiss={vi.fn()}
        isMobile={false}
      />,
    );

    expect(screen.queryByText(/queued/)).not.toBeInTheDocument();
  });
});

describe("QueueSection in-flight states (#987)", () => {
  it("renders verifying with confirming label, Dismiss but no Retry", async () => {
    const onRetry = vi.fn();
    render(
      <QueueSection
        messages={[makeMsg({ status: "verifying", text: "long turn" })]}
        onRetry={onRetry}
        onDismiss={vi.fn()}
        isMobile={false}
      />,
    );
    expect(screen.getByText("Sent — confirming delivery…")).toBeInTheDocument();
    expect(screen.queryByLabelText("Retry")).not.toBeInTheDocument();
    expect(screen.getByLabelText("Dismiss")).toBeInTheDocument();
  });

  it("renders delivering with sending label", () => {
    render(
      <QueueSection
        messages={[makeMsg({ status: "delivering", text: "going out" })]}
        onRetry={vi.fn()}
        onDismiss={vi.fn()}
        isMobile={false}
      />,
    );
    expect(screen.getByText("Sending…")).toBeInTheDocument();
  });
});
