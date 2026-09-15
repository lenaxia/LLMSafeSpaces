import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { ChatHistoryErrorBanner } from "./ChatHistoryErrorBanner";
import { ApiClientError } from "../../api/client";

// LLMSafeSpaces#490: banner renders when useMessageHistory returns
// isError. These tests target the banner in isolation (integration with
// ChatPage.tsx is covered separately by ChatPage.historyError.test.tsx).
//
// IMPORTANT — fixture fidelity (#1303): the API authors every error
// body on the message routes. GET history failures return the API's
// own `{"error": "failed to fetch history"}` (proxy_handlers.go
// GetHistory); the POST prompt path runs EnrichChatErrorBody
// (proxy_chat_enrichment.go) which promotes the allowlisted fields
// (including `message` and `ref`) to the top level. The raw agent
// envelope `{name, data:{...}}` stopped reaching the client when #828
// deleted the passthrough — a fixture keeps only as a graceful-fallback
// row below. Tests use the real API-authored shapes; synthetic shapes
// would let bugs pass artificially (the finding from the first #491
// review).

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

  it("shows HTTP status + the API's error message for the GET-history 502 body (#486 regression intent)", () => {
    // The API-authored GET history failure body (proxy_handlers.go
    // GetHistory): {"error":"failed to fetch history"} with a 502. The
    // #486 regression intent — a meaningful message must render where
    // the pre-#490 UI was silently empty — now holds against the
    // API-authored shape: `extractAgentErrorMessage` finds no top-level
    // `message`, the banner falls back to `body.error`.
    // `ApiClientError`'s super(body.error) carries the same string.
    const err = new ApiClientError(502, { error: "failed to fetch history" });
    render(<ChatHistoryErrorBanner error={err} onRetry={vi.fn()} />);

    fireEvent.click(screen.getByText("Details"));
    expect(screen.getByText("HTTP 502")).toBeInTheDocument();
    expect(screen.getByText("failed to fetch history")).toBeInTheDocument();
    // No ref in the API-authored body — none may be invented.
    expect(screen.queryByText(/^Ref:/)).not.toBeInTheDocument();
    // Explicit negative: the literal "undefined" string must never render.
    expect(screen.queryByText(/^undefined$/)).not.toBeInTheDocument();
  });

  it("shows HTTP status + message + ref for the flat allowlisted shape (POST prompt path)", () => {
    // After EnrichChatErrorBody's allowlist, `message`, `ref`, `_tag`
    // etc. sit at the top level. body.error is still absent — the
    // allowlist does not synthesize it. Banner reads message via
    // extractAgentErrorMessage, not body.error.
    const body = {
      _tag: "SomeAgentError",
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

  it("degrades gracefully on a legacy nested envelope: no nested extraction, placeholder message (#1303)", () => {
    // Pre-#828, GET history passed the raw agent envelope
    // `{name, data:{message, ref}}` through verbatim (the shape #486
    // hit). That passthrough is deleted and the nested extractor went
    // with it — a stray legacy body must extract NOTHING (no
    // data.message, no data.ref) and fall through to the "Unknown
    // error" placeholder (body.error is absent and super(undefined)
    // leaves err.message = "").
    const body = {
      name: "UnknownError",
      data: {
        message: "Unexpected server error. Check server logs for details.",
        ref: "err_b8d02ae9",
      },
    } as unknown as ConstructorParameters<typeof ApiClientError>[1];
    const err = new ApiClientError(500, body);
    render(<ChatHistoryErrorBanner error={err} onRetry={vi.fn()} />);

    fireEvent.click(screen.getByText("Details"));
    expect(screen.getByText("HTTP 500")).toBeInTheDocument();
    // The nested message and ref must NOT surface.
    expect(screen.queryByText(/Unexpected server error/)).not.toBeInTheDocument();
    expect(screen.queryByText(/^Ref:/)).not.toBeInTheDocument();
    expect(screen.getByText("Unknown error")).toBeInTheDocument();
    expect(screen.queryByText(/^undefined$/)).not.toBeInTheDocument();
  });

  it("shows the API's own `error` field when the promoted message is absent (real 503 not-ready body)", () => {
    // Real API shape (proxy_adapter_crosscutting.go
    // resolveWorkspaceForAdapter — the only 503 the history routes
    // emit): `error` + `phase` + `retryAfter`, no promoted agent
    // fields, no reason → red error state, message from body.error.
    const err = new ApiClientError(503, {
      error: "workspace not ready",
      phase: "Suspended",
      retryAfter: 5,
    });
    render(<ChatHistoryErrorBanner error={err} onRetry={vi.fn()} />);
    fireEvent.click(screen.getByText("Details"));

    expect(screen.getByText("HTTP 503")).toBeInTheDocument();
    expect(screen.getByText(/workspace not ready/)).toBeInTheDocument();
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
    // chain preferred `error.message` over the body's message, so it
    // rendered a blank line in Details.
    //
    // This test constructs a body with NO top-level `error` AND NO
    // extractable message/ref, forcing every extraction step to fail
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
    // Defensive display branch only — NO API producer currently emits
    // a reason-keyed 503 body on the message routes (the real 503 is
    // the not-ready shape above; the branch awaits a recovery-reason
    // producer, tracked with #796's parity sweep). Rows keep the
    // branch covered until then.
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
    // Defensive display branch only — see the agent_unreachable row.
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
    // Defensive display branch only — see the agent_unreachable row.
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
