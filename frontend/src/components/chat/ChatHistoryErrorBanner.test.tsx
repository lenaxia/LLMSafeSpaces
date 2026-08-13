import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { ChatHistoryErrorBanner } from "./ChatHistoryErrorBanner";
import { ApiClientError } from "../../api/client";

// LLMSafeSpaces#490: banner renders when useMessageHistory returns
// isError. These tests target the banner in isolation (integration with
// ChatPage.tsx is covered separately by ChatPage.historyError.test.tsx).
//
// IMPORTANT — fixture fidelity: the production error shapes are
// distinct at both /message endpoints. The GET history path
// (proxy_handlers.go:154-157) passes opencode's raw envelope through
// verbatim, so `body.error` does NOT exist at the top level — the
// human-readable message is at `body.data.message` and the ref is at
// `body.data.ref`. The POST prompt path runs EnrichChatErrorBody
// (proxy_chat_enrichment.go) which promotes the allowlisted fields
// (including `message` and `ref`) to the top level. Tests below use
// the real shapes; synthetic top-level `error` fields would let bugs
// pass artificially. This was the finding from the first #491 review.

// A cast type for the opencode nested envelope shape. The API's
// ApiError type doesn't declare `name` or `data` because those are
// opencode-owned fields, not LLMSafeSpaces API fields. Cast at the
// fixture boundary rather than augmenting production types.
type OpencodeErrorEnvelope = {
  name: string;
  data: { message?: string; ref?: string };
};

describe("ChatHistoryErrorBanner", () => {
  it("renders the user-facing header and Retry action", () => {
    const onRetry = vi.fn();
    render(<ChatHistoryErrorBanner error={new Error("net")} onRetry={onRetry} />);

    expect(screen.getByText("Chat history unavailable")).toBeInTheDocument();
    const retryBtn = screen.getByRole("button", { name: /retry/i });
    fireEvent.click(retryBtn);
    expect(onRetry).toHaveBeenCalledTimes(1);
  });

  it("has role=alert so screen readers announce the failure", () => {
    render(<ChatHistoryErrorBanner error={new Error("x")} onRetry={vi.fn()} />);
    expect(screen.getByRole("alert")).toBeInTheDocument();
  });

  it("shows HTTP status + opencode message + ref for the raw envelope shape (GET history, the #486 shape)", () => {
    // This is the EXACT shape #486 hit — opencode's raw error envelope
    // passed through by the API on GET /message. `body.error` DOES NOT
    // EXIST at top level. The message is at data.message; the ref is
    // at data.ref. `ApiClientError`'s super(body.error) with
    // body.error=undefined leaves err.message = "" (empty string —
    // the Error constructor treats `undefined` as absent). The banner
    // must skip past empty err.message (via the truthy `&&` guard) and
    // fall back to opencode's data.message via extractAgentErrorMessage.
    // Regression test at line ~137 pins this end-to-end.
    const body = {
      name: "UnknownError",
      data: {
        message: "Unexpected server error. Check server logs for details.",
        ref: "err_b8d02ae9",
      },
    } as unknown as OpencodeErrorEnvelope;
    const err = new ApiClientError(
      500,
      body as unknown as ConstructorParameters<typeof ApiClientError>[1],
    );
    render(<ChatHistoryErrorBanner error={err} onRetry={vi.fn()} />);

    fireEvent.click(screen.getByText("Details"));
    expect(screen.getByText("HTTP 500")).toBeInTheDocument();
    expect(screen.getByText(/Unexpected server error/)).toBeInTheDocument();
    expect(screen.getByText("Ref: err_b8d02ae9")).toBeInTheDocument();
    // Explicit negative: the literal "undefined" string (from
    // super(body.error) with no top-level error) must never render.
    expect(screen.queryByText(/^undefined$/)).not.toBeInTheDocument();
  });

  it("shows HTTP status + opencode message + ref for the flat allowlisted shape (POST prompt path)", () => {
    // After EnrichChatErrorBody's allowlist, `message`, `ref`, `_tag`
    // etc. sit at the top level. body.error is still absent — the
    // allowlist does not synthesize it. Banner reads message via
    // extractAgentErrorMessage, not body.error.
    const body = {
      _tag: "SomeOpencodeError",
      message: "big-pickle rate-limited",
      ref: "err_topLevel",
      sessionID: "ses_abc",
    } as unknown as ConstructorParameters<typeof ApiClientError>[1];
    const err = new ApiClientError(502, body);
    render(<ChatHistoryErrorBanner error={err} onRetry={vi.fn()} />);

    fireEvent.click(screen.getByText("Details"));
    expect(screen.getByText("HTTP 502")).toBeInTheDocument();
    expect(screen.getByText("big-pickle rate-limited")).toBeInTheDocument();
    expect(screen.getByText("Ref: err_topLevel")).toBeInTheDocument();
  });

  it("shows the API's own `error` field when opencode's message is absent (503 workspace-connection-failed)", () => {
    // Real API shape from proxy_handlers.go:298. Body has `error`
    // (via ApiError type), no opencode fields.
    const err = new ApiClientError(503, {
      error: "workspace connection failed",
      retryAfter: 5,
    });
    render(<ChatHistoryErrorBanner error={err} onRetry={vi.fn()} />);
    fireEvent.click(screen.getByText("Details"));

    expect(screen.getByText("HTTP 503")).toBeInTheDocument();
    expect(screen.getByText(/workspace connection failed/)).toBeInTheDocument();
    expect(screen.queryByText(/^Ref:/)).not.toBeInTheDocument();
  });

  it("handles non-ApiClientError errors gracefully (network exception, etc.)", () => {
    // e.g. fetch() itself threw — no response, no body.
    render(
      <ChatHistoryErrorBanner error={new Error("Failed to fetch")} onRetry={vi.fn()} />,
    );
    fireEvent.click(screen.getByText("Details"));

    expect(screen.getByText(/Failed to fetch/)).toBeInTheDocument();
    // No HTTP status when we don't have one.
    expect(screen.queryByText(/^HTTP /)).not.toBeInTheDocument();
  });

  it("handles a completely opaque error (undefined) with the 'Unknown error' placeholder", () => {
    // Pathological — react-query surfaced something that isn't even
    // an Error. Banner still renders (alert role stays, retry still
    // works) and shows the placeholder message so the user isn't
    // stuck with a blank Details block.
    render(<ChatHistoryErrorBanner error={undefined as unknown} onRetry={vi.fn()} />);
    expect(screen.getByRole("alert")).toBeInTheDocument();

    fireEvent.click(screen.getByText("Details"));
    expect(screen.getByText("Unknown error")).toBeInTheDocument();
  });

  it("detects an empty err.message from ApiClientError.super(body.error) and falls through to the placeholder", () => {
    // Regression guard for the exact bug the first #491 review found:
    // `super(body.error)` when body.error is `undefined` produces
    // `err.message = ""` (the empty string — the Error constructor
    // treats `undefined` as absent). Pre-fix, the banner's fallback
    // chain preferred `error.message` over opencode's `data.message`,
    // so it rendered a blank line in Details for the #486 shape.
    //
    // This test constructs a body with NO top-level `error` AND NO
    // opencode message/data, forcing every extraction step to fail
    // through to the placeholder.
    const body = {
      some_other_field: 42,
    } as unknown as ConstructorParameters<typeof ApiClientError>[1];
    const err = new ApiClientError(500, body);

    // Sanity: err.message is empty (from super(undefined)).
    expect(err.message).toBe("");

    render(<ChatHistoryErrorBanner error={err} onRetry={vi.fn()} />);
    fireEvent.click(screen.getByText("Details"));

    // The banner must fall through to the placeholder, not render
    // an empty line.
    expect(screen.getByText("Unknown error")).toBeInTheDocument();
  });

  it("shows yellow 'Reconnecting…' state for 503 with agent_unreachable reason", () => {
    const err = new ApiClientError(503, {
      error: "workspace connection failed",
      message: "The agent is not responding. Please try again in a moment.",
      reason: "agent_unreachable",
      retryAfter: 10,
    });
    render(<ChatHistoryErrorBanner error={err} onRetry={vi.fn()} />);

    expect(screen.getByText("Reconnecting…")).toBeInTheDocument();
    expect(screen.queryByText("Chat history unavailable")).not.toBeInTheDocument();

    fireEvent.click(screen.getByText("Details"));
    expect(screen.getByText("Reason: agent_unreachable")).toBeInTheDocument();
  });

  it("shows yellow 'Reconnecting…' state for 503 with agent_restarting reason", () => {
    const err = new ApiClientError(503, {
      error: "Workspace is restarting",
      message: "The agent is restarting. Please try again in a moment.",
      reason: "agent_restarting",
      retryAfter: 5,
    });
    render(<ChatHistoryErrorBanner error={err} onRetry={vi.fn()} />);

    expect(screen.getByText("Reconnecting…")).toBeInTheDocument();
  });

  it("shows red error state for 503 with not_ready reason (not recovering)", () => {
    const err = new ApiClientError(503, {
      error: "workspace not ready",
      message: "Workspace is pending.",
      reason: "not_ready",
      retryAfter: 10,
    });
    render(<ChatHistoryErrorBanner error={err} onRetry={vi.fn()} />);

    expect(screen.getByText("Chat history unavailable")).toBeInTheDocument();
    expect(screen.queryByText("Reconnecting…")).not.toBeInTheDocument();
  });
});
