import { describe, beforeEach, afterEach, expect, it, vi } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "../../test/utils";
import { MessagePart, extractToolError } from "./MessagePart";

// Env mock mirrors MessagePart.test.tsx (DevPreviewOutput's env import
// chain — the component module imports getEnv unconditionally).
vi.mock("../../env", () => ({
  getEnv: () => ({ apiBaseUrl: "/api/v1", turnstileSiteKey: "" }),
  loadEnv: async () => ({ apiBaseUrl: "/api/v1", turnstileSiteKey: "" }),
}));

vi.mock("../../lib/shiki", () => ({
  highlight: vi.fn().mockResolvedValue(null),
}));

// Owner report 2026-11-12 (tsk-copper-archer-rushes): a failed tool call
// rendered as a bare ✗ + invocation parameters — the error payload was
// never displayed. The runtime sends ToolState.error on both the SSE and
// history paths; the frontend dropped it, and any output rode behind TWO
// collapsed toggles. These pins hold the error path visible.
describe("MessagePart failed-tool error rendering", () => {
  let consoleSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    consoleSpy = vi.spyOn(console, "error").mockImplementation(() => {});
  });

  afterEach(() => {
    consoleSpy.mockRestore();
  });

  it("renders the toolError text visible WITHOUT expanding anything (pin a)", () => {
    render(
      <MessagePart
        part={{
          type: "tool_use",
          name: "llmsafespaces_board_import_plan",
          toolState: "error",
          toolError: "HTTP 409 epic_state: epic epc-951a is not active — activate it first",
          input: { projectId: "prj-1", planFile: "plan.json" },
        }}
        isUser={false}
      />,
    );
    // The error text must be in the DOM and visible without opening any
    // <details> — the dev-preview 1eee7f28 precedent: content behind the
    // collapsed LazyDetails pane effectively does not exist.
    const err = screen.getByTestId("tool-error");
    expect(err).toBeVisible();
    expect(screen.getByText(/epic epc-951a is not active/)).toBeVisible();
  });

  it("renders the error on the bare ✗ card shape too (no input, no output)", () => {
    render(
      <MessagePart
        part={{
          type: "tool_use",
          name: "bash",
          toolState: "error",
          toolError: "command timed out after 30s",
        }}
        isUser={false}
      />,
    );
    expect(screen.getByTestId("tool-error")).toBeVisible();
    expect(screen.getByText(/command timed out after 30s/)).toBeVisible();
    expect(screen.getByText(/✗ bash/)).toBeInTheDocument();
  });

  it("extracts the most specific message from a JSON body with a string error field", () => {
    render(
      <MessagePart
        part={{
          type: "tool_use",
          name: "llmsafespaces_board_epic_activate",
          toolState: "error",
          toolOutput: '{"error":"invalid reference: no such role reviewerx","requestId":"req-9"}',
          input: { epicId: "epc-1" },
        }}
        isUser={false}
      />,
    );
    // The message pre (the block's first pre) carries the EXTRACTED
    // message — not the raw JSON (which also contains the substring).
    const err = screen.getByTestId("tool-error");
    const messagePre = err.querySelector("pre");
    expect(messagePre?.textContent).toBe("invalid reference: no such role reviewerx");
    // The raw payload rides behind its own expand control — present but
    // collapsed by default (jsdom visibility for closed <details> is
    // ambiguous; the open state is the deterministic pin).
    const rawSummary = screen.getByText(/Raw output/i);
    const rawDetails = rawSummary.closest("details");
    expect(rawDetails).not.toBeNull();
    expect(rawDetails?.open).toBe(false);
    expect(rawDetails?.textContent).toContain("requestId");
  });

  it("extracts error.message from a JSON body whose error is an object", () => {
    render(
      <MessagePart
        part={{
          type: "tool_use",
          name: "llmsafespaces_board_import_plan",
          toolState: "error",
          toolOutput: '{"error":{"code":409,"message":"epic_state: the epic is INACTIVE"}}',
          input: { projectId: "prj-1" },
        }}
        isUser={false}
      />,
    );
    const err = screen.getByTestId("tool-error");
    const messagePre = err.querySelector("pre");
    expect(messagePre?.textContent).toBe("epic_state: the epic is INACTIVE");
  });

  it("shows a plain-text output payload as the error itself", () => {
    render(
      <MessagePart
        part={{
          type: "tool_use",
          name: "bash",
          toolState: "error",
          toolOutput: "grep: /etc/shadow: Permission denied",
          input: { command: "grep root /etc/shadow" },
        }}
        isUser={false}
      />,
    );
    expect(screen.getByText(/Permission denied/)).toBeVisible();
  });

  it("prefers the explicit toolError over the JSON body's message", () => {
    render(
      <MessagePart
        part={{
          type: "tool_use",
          name: "bash",
          toolState: "error",
          toolError: "process killed: OOM",
          toolOutput: '{"error":"something else"}',
        }}
        isUser={false}
      />,
    );
    expect(screen.getByText(/process killed: OOM/)).toBeVisible();
    expect(screen.queryByText(/something else/)).not.toBeVisible();
  });

  // Pin c: long payloads collapse behind a VISIBLE expand affordance —
  // the head shows inline, the full payload does not.
  it("collapses a long error payload behind a visible expand affordance (pin c)", async () => {
    const lines = Array.from({ length: 30 }, (_, i) => `trace frame ${i + 1}: failure detail`);
    const longError = lines.join("\n");
    const user = userEvent.setup();
    render(
      <MessagePart
        part={{ type: "tool_use", name: "bash", toolState: "error", toolError: longError, input: { command: "x" } }}
        isUser={false}
      />,
    );
    // The affordance is visible with the head inline; the full payload
    // rides behind it, collapsed by default (the open state is the pin).
    const affordance = screen.getByText(/Show full error/i);
    expect(affordance).toBeVisible();
    const err = screen.getByTestId("tool-error");
    const headPre = err.querySelector("pre");
    expect(headPre?.textContent).toContain("trace frame 3:");
    expect(headPre?.textContent).not.toContain("trace frame 4:");
    const details = affordance.closest("details");
    expect(details).not.toBeNull();
    expect(details?.open).toBe(false);
    // Opening the affordance reveals the tail of the payload.
    await user.click(affordance);
    expect(details?.open).toBe(true);
    expect(details?.textContent).toContain("trace frame 30:");
  });

  it("shows short multi-line errors (≤10 lines) fully inline", () => {
    const short = Array.from({ length: 10 }, (_, i) => `line ${i + 1}`).join("\n");
    render(
      <MessagePart part={{ type: "tool_use", name: "bash", toolState: "error", toolError: short }} isUser={false} />,
    );
    expect(screen.getByText(/line 10/)).toBeVisible();
    expect(screen.queryByText(/Show full error/i)).not.toBeInTheDocument();
  });

  it("renders no error block when an error-state tool carries no payload (honest ✗ card)", () => {
    render(<MessagePart part={{ type: "tool_use", name: "bash", toolState: "error" }} isUser={false} />);
    expect(screen.getByText(/✗ bash/)).toBeInTheDocument();
    expect(screen.queryByTestId("tool-error")).not.toBeInTheDocument();
  });

  // Pin b: the success path renders exactly as before — output behind the
  // collapsed "Output (N chars)" toggle inside the tool pane, no error
  // block anywhere. The pane must be opened to see the Output affordance
  // (LazyDetails defers mounting its children) — that indirection IS the
  // old behavior and must stay.
  it("renders a completed tool part unchanged (pin b — regression)", async () => {
    const user = userEvent.setup();
    render(
      <MessagePart
        part={{
          type: "tool_use",
          name: "bash",
          toolState: "completed",
          input: { command: "ls" },
          toolOutput: "file.go\nfile_test.go",
        }}
        isUser={false}
      />,
    );
    expect(screen.getByText(/✓ bash/)).toBeInTheDocument();
    expect(screen.queryByTestId("tool-error")).not.toBeInTheDocument();
    // Open the tool pane: the success output keeps its collapsed
    // "Output (…)" affordance (the byte-identical success rendering).
    await user.click(screen.getByText(/✓ bash/));
    const summary = await screen.findByText(/Output \(/);
    expect(summary).toBeInTheDocument();
    expect(summary.closest("details")?.open).toBe(false);
  });

  it("renders no error block on a running tool", () => {
    render(
      <MessagePart
        part={{ type: "tool_use", name: "bash", toolState: "running", toolOutput: "partial…", input: { command: "ls" } }}
        isUser={false}
      />,
    );
    expect(screen.queryByTestId("tool-error")).not.toBeInTheDocument();
  });
});

// The extraction ladder as a unit surface: state error > JSON error
// string > JSON error.message > JSON.stringify(error) > plain text.
describe("extractToolError", () => {
  it("returns empty message when nothing is available", () => {
    expect(extractToolError(undefined, undefined)).toEqual({ message: "", raw: undefined });
    expect(extractToolError("  ", "  ")).toEqual({ message: "", raw: undefined });
  });

  it("state error wins; raw keeps the distinct output payload", () => {
    expect(extractToolError("boom", '{"error":"other"}')).toEqual({ message: "boom", raw: '{"error":"other"}' });
  });

  it("unwraps a JSON string error field", () => {
    expect(extractToolError(undefined, '{"error":"nope"}')).toEqual({ message: "nope", raw: '{"error":"nope"}' });
  });

  it("unwraps error.message from an object error field", () => {
    expect(extractToolError(undefined, '{"error":{"message":"deep"}}')).toEqual({
      message: "deep",
      raw: '{"error":{"message":"deep"}}',
    });
  });

  it("stringifies an object error without a message field", () => {
    const { message, raw } = extractToolError(undefined, '{"error":{"code":500}}');
    expect(message).toContain('"code"');
    expect(raw).toBe('{"error":{"code":500}}');
  });

  it("passes non-JSON text through as the message", () => {
    expect(extractToolError(undefined, "plain failure")).toEqual({ message: "plain failure", raw: "plain failure" });
  });

  it("ignores non-error JSON bodies (falls back to the payload text)", () => {
    const body = '{"ok":true,"data":[1,2]}';
    expect(extractToolError(undefined, body)).toEqual({ message: body, raw: body });
  });

  it("unwraps a JSON-string-literal output payload as the message", () => {
    // The SSE decode path (toolOutputText) already unwraps top-level JSON
    // string literals; the history adapter may deliver them raw.
    expect(extractToolError(undefined, '"escaped failure"')).toEqual({
      message: '"escaped failure"',
      raw: '"escaped failure"',
    });
  });
});
