// TypeScript port of the v1 agent-message provenance sentinel parser
// (pkg/session/agentmessage/agentmessage.go, issue #1465). The sentinel
// format is a stable API contract locked by the golden fixtures in the
// Go package's testdata/ — this port is verified against those same
// fixtures.
//
// The frontend only ever PARSES (user-side render strips the leading
// sentinel line and shows a provenance badge instead); composition is
// agentd-side at send time, from the harness-injected origin.
//
// Format v1: one HTML-comment line at the START of the message text,
// followed by one newline and the message:
//
//   <!-- lsp:agent-message-v1 {"fromSession":"ses_..."} -->
//
// The JSON payload carries fromSession (always); workspace is a legal
// v1 key v1 senders never emit (future cross-workspace origin, #1260).
// Unknown versions, malformed payloads, missing fromSession, interior
// or forged lines → not a sentinel: plain text, payload never breaks.

export interface AgentMessageOrigin {
  fromSession: string;
  workspace?: string;
  /** "injected" = platform-plugin attested; "self-declared" = model-supplied
   *  fallback (#1469 hybrid ruling). Empty for pre-mode sentinels. */
  mode?: string;
}

export interface ParsedAgentMessage {
  origin: AgentMessageOrigin | null;
  text: string;
}

const sentinelPrefix = "<!-- lsp:agent-message-v1 ";
const sentinelLinePattern = /^<!-- lsp:agent-message-v1 (\{.*\}) -->$/;

export function parseAgentMessage(text: string): ParsedAgentMessage {
  if (!text.startsWith(sentinelPrefix)) {
    return { origin: null, text };
  }
  const newline = text.indexOf("\n");
  const line = newline === -1 ? text : text.slice(0, newline);
  const m = sentinelLinePattern.exec(line);
  if (!m) {
    return { origin: null, text };
  }
  let raw: AgentMessageOrigin;
  try {
    raw = JSON.parse(m[1]!) as AgentMessageOrigin;
  } catch {
    return { origin: null, text };
  }
  if (typeof raw.fromSession !== "string" || raw.fromSession.trim() === "") {
    return { origin: null, text };
  }
  // Strict parity with the Go parser (locked by shared fixtures): keys
  // match exactly (JSON.parse is case-exact) and known keys must be
  // well-typed — a non-string workspace rejects the sentinel rather
  // than silently dropping the field. Unknown additive keys are
  // ignored on both sides.
  // Explicit null reads as ABSENT (parity with Go's json null-into-string
  // no-op — foreign emitters may serialize omitempty-less nulls; pinned
  // by the parse_null_keys_tolerated fixture both suites consume).
  const workspace = raw.workspace ?? undefined;
  const mode = raw.mode ?? undefined;
  if (workspace !== undefined && typeof workspace !== "string") {
    return { origin: null, text };
  }
  if (mode !== undefined && typeof mode !== "string") {
    return { origin: null, text };
  }
  const origin: AgentMessageOrigin = { fromSession: raw.fromSession };
  if (workspace !== undefined && workspace !== "") {
    origin.workspace = workspace;
  }
  if (mode !== undefined && mode !== "") {
    origin.mode = mode;
  }
  return { origin, text: newline === -1 ? "" : text.slice(newline + 1) };
}
