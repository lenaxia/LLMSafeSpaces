import { ApiClientError } from "../../api/client";
import { extractAgentErrorMessage, extractAgentErrorRef } from "../../api/agentErrorRef";

interface ChatHistoryErrorBannerProps {
  error: unknown;
  onRetry: () => void;
}

/**
 * ChatHistoryErrorBanner (LLMSafeSpaces#490) — inline diagnostic banner
 * shown when the message-history query fails. Replaces the pre-#490
 * silent-empty-chat state that made session-history failures
 * indistinguishable from "no messages yet."
 *
 * Design decisions:
 *   - Inline banner (not toast) — a user staring at a blank chat needs
 *     the error visible in the chat area, not dismissed in a corner.
 *   - Retry action — the underlying react-query hook supports refetch;
 *     wire it here so the user can self-service the transient case
 *     (pod-restart, rate-limit, etc.) without a page reload.
 *   - Opencode ref — pulled from the API error body via
 *     extractAgentErrorRef, shown in a `<details>` block. Not front-and-
 *     center (users don't need it) but discoverable for operators
 *     debugging their own or a support-ticketed session.
 *   - Visual language reused from the sibling chatError + streamTimedOut
 *     banners in ChatPage.tsx:971-982 (destructive/10 background,
 *     destructive/50 border, text-destructive foreground).
 *
 * The companion server-side observability (#488) records the same ref
 * as a metric label + log line. An operator debugging an incident goes:
 * user reports "chat blank" → operator opens the DevTools banner →
 * copies the ref → greps opencode logs by that ref → stack trace.
 *
 * Message extraction hierarchy (verified against production shapes):
 *   1. `extractAgentErrorMessage(body)` — nested-or-flat opencode `message`.
 *      Handles both the raw envelope (#486 GET-history shape,
 *      `body.data.message`) and the allowlisted shape (POST-prompt path,
 *      where the backend promotes `message` to top level).
 *   2. `body.error` — the API's own error responses (e.g. 503
 *      "workspace connection failed" from proxy_handlers.go:298).
 *   3. `err.message` from the Error base class — network-layer failures
 *      where there is no body at all (fetch threw). Note: for an
 *      ApiClientError this equals `body.error` because the constructor
 *      does `super(body.error)`; when `body.error` is `undefined` the
 *      Error constructor produces `err.message = ""` (empty string).
 *      We treat empty as absent via truthy-check (`error.message && ...`).
 *      The `!== "undefined"` clause is defense-in-depth against a
 *      hypothetical stricter subclass that stringifies undefined.
 *   4. Placeholder "Unknown error" — pathological case where nothing
 *      is available.
 */
export function ChatHistoryErrorBanner({
  error,
  onRetry,
}: ChatHistoryErrorBannerProps) {
  const status = error instanceof ApiClientError ? error.status : undefined;
  const ref = error instanceof ApiClientError ? extractAgentErrorRef(error.body) : undefined;

  let message: string = "Unknown error";
  const reason = error instanceof ApiClientError ? error.body?.reason : undefined;
  const isRecovering = status === 503 && (reason === "agent_unreachable" || reason === "agent_restarting");

  if (error instanceof ApiClientError) {
    // Prefer the API's structured `message` field (always present on 503s).
    if (typeof error.body?.message === "string" && error.body.message.length > 0) {
      message = error.body.message;
    } else {
      message =
        extractAgentErrorMessage(error.body) ??
        (typeof error.body?.error === "string" && error.body.error.length > 0
          ? error.body.error
          : error.message && error.message !== "undefined"
            ? error.message
            : "Unknown error");
    }
  } else if (error instanceof Error && error.message) {
    message = error.message;
  }

  return (
    <div
      role="alert"
      className={`flex flex-col gap-2 border-b px-4 py-3 text-sm ${
        isRecovering
          ? "border-yellow-300/50 bg-yellow-50 text-yellow-800 dark:border-yellow-800/50 dark:bg-yellow-950 dark:text-yellow-200"
          : "border-destructive/50 bg-destructive/10 text-destructive"
      }`}
    >
      <div className="flex items-center justify-between gap-2">
        <span className="font-medium">{isRecovering ? "Reconnecting…" : "Chat history unavailable"}</span>
        <button
          type="button"
          onClick={onRetry}
          className="underline hover:no-underline"
        >
          Retry
        </button>
      </div>
      <details className="text-xs">
        <summary className="cursor-pointer">Details</summary>
        <div className="mt-1 space-y-0.5 font-mono">
          {status !== undefined && <div>HTTP {status}</div>}
          {reason && <div>Reason: {reason}</div>}
          <div>{message}</div>
          {ref && <div>Ref: {ref}</div>}
        </div>
      </details>
    </div>
  );
}
