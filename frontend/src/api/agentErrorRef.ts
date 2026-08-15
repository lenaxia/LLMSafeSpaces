/**
 * extractAgentErrorRef pulls the opencode error reference (err_XXXXXXXX)
 * from either of the two shapes it appears in on API responses:
 *
 *   1. `{ ref: "err_abcdef12", ...allowlisted fields }` — the shape after
 *      the API's EnrichChatErrorBody allowlist runs (POST /prompt path,
 *      backend proxy_chat_enrichment.go promotes `ref` to top level).
 *
 *   2. `{ name: "UnknownError", data: { ref: "err_abcdef12", ... } }` —
 *      raw opencode envelope, passed through verbatim on GET history
 *      (proxy_handlers.go:155 skips the allowlist for reads).
 *
 * Returns undefined when the ref is absent OR when the input is not an
 * object-shaped body. Never throws.
 *
 * See LLMSafeSpaces#488 for the server-side companion (metric + log line
 * carry the same ref). Together an operator can go: banner shows the ref
 * → grep opencode logs → root cause.
 */
export function extractAgentErrorRef(body: unknown): string | undefined {
  return extractOpencodeField(body, "ref");
}

/**
 * extractAgentErrorMessage pulls the human-readable error message from
 * either of the two error-body shapes (same reasoning as
 * extractAgentErrorRef). The GET-history path (#486) hit shape 2 with
 * the message nested at `body.data.message`; the POST-prompt path hits
 * shape 1 where the API's EnrichChatErrorBody promotes `message` to
 * top level.
 *
 * A third shape — the API's own error responses like
 * `{ error: "workspace connection failed" }` — uses the `error` field.
 * Callers should try this helper first and fall back to `body.error`
 * or `err.message` (Error base class) if neither top-level nor nested
 * `message` is present.
 */
export function extractAgentErrorMessage(body: unknown): string | undefined {
  return extractOpencodeField(body, "message");
}

// extractOpencodeField is the shared implementation for the two
// nested-or-flat extractors. Prefers top-level, falls back to `data.*`.
// Returns undefined for empty strings, non-strings, non-objects, and
// arrays. Never throws.
function extractOpencodeField(
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

  const data = record.data;
  if (data !== null && data !== undefined && typeof data === "object" && !Array.isArray(data)) {
    const nested = (data as Record<string, unknown>)[field];
    if (typeof nested === "string" && nested.length > 0) {
      return nested;
    }
  }

  return undefined;
}
