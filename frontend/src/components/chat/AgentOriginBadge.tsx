import { Bot } from "lucide-react";
import type { AgentMessageOrigin } from "../../lib/agentMessage";

interface Props {
  origin: AgentMessageOrigin;
}

/**
 * Provenance badge for agent-originated messages (#1465): a user-side
 * message delivered by the agentd send_message tool renders with a
 * visible "message from session X" header so it is never mistaken for
 * human input. Borrows tool-call-part visual styling (monospace label,
 * muted border) — placement stays user-side (that is the role the
 * message arrives as). The session ID is a return address, not an
 * identity proof (all sessions in a workspace share one credential).
 */
export function AgentOriginBadge({ origin }: Props) {
  return (
    <div
      data-testid="agent-origin-badge"
      className="mb-1.5 inline-flex max-w-full items-center gap-1.5 rounded-md border border-muted-foreground/25 bg-muted/40 px-2 py-0.5 font-mono text-[11px] text-muted-foreground"
    >
      <Bot className="h-3 w-3 shrink-0" aria-hidden="true" />
      {/* The session ID is the return address — it must render in full.
          break-all (house convention for monospace IDs) wraps it at the
          container edge; the old `truncate` here hid it behind an
          ellipsis (#1465 owner report). title carries the full ID for
          hover-copy. */}
      <span className="min-w-0">
        message from session{" "}
        <span data-testid="agent-origin-session-id" className="break-all" title={origin.fromSession}>
          {origin.fromSession}
        </span>
        {origin.workspace ? ` · workspace ${origin.workspace}` : ""}
        {origin.mode === "self-declared" ? " · self-declared origin" : ""}
      </span>
    </div>
  );
}
