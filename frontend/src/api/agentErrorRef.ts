/**
 * extractAgentErrorRef pulls the agent error reference (err_XXXXXXXX)
 * from the top level of an API error body:
 *
 *   `{ ref: "err_abcdef12", ...allowlisted fields }` — the shape after
 *   the API's EnrichChatErrorBody allowlist runs (proxy_chat_enrichment.go
 *   promotes the agent error object's `ref` to top level).
 *
 * The API authors every error body on the message routes (since #828
 * deleted the raw passthrough and #1302 moved question/permission to
 * the contract REST shapes) — no nested `{ name, data: { ref } }`
 * envelope can reach the client anymore, so none is parsed (#1303).
 *
 * Returns undefined when the ref is absent OR when the input is not an
 * object-shaped body. Never throws.
 *
 * See LLMSafeSpaces#488 for the server-side companion (metric + log line
 * carry the same ref). Together an operator can go: banner shows the ref
 * → grep agent logs → root cause.
 */
export function extractAgentErrorRef(body: unknown): string | undefined {
  return extractErrorField(body, "ref");
}

/**
 * extractAgentErrorMessage pulls the human-readable error message from
 * the top level of an API error body (same extraction as
 * extractAgentErrorRef): the allowlisted `message` the API promotes via
 * EnrichChatErrorBody, or the API's own structured `message` field
 * (e.g. the 507 disk-full body, proxy_handlers.go).
 *
 * The API's own error responses like `{"error": "workspace not ready"}`
 * use the `error` field — callers should fall back to `body.error`
 * or `err.message` (Error base class) when this helper
 * returns undefined.
 */
export function extractAgentErrorMessage(body: unknown): string | undefined {
  return extractErrorField(body, "message");
}

// extractErrorField reads a single allowlisted field from the top level
// of an API error body. Returns undefined for empty strings,
// non-strings, non-objects, and arrays. Never throws.
function extractErrorField(
  body: unknown,
  field: "ref" | "message",
): string | undefined {
  if (body === null || body === undefined || typeof body !== "object") {
    return undefined;
  }
  // Reject arrays — `typeof []` is "object" in JavaScript.
  if (Array.isArray(body)) {
    return undefined;
  }
  const record = body as Record<string, unknown>;

  const top = record[field];
  if (typeof top === "string" && top.length > 0) {
    return top;
  }

  return undefined;
}
