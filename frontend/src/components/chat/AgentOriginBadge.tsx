import { Bot } from "lucide-react";
import type { AgentMessageOrigin } from "../../lib/agentMessage";

interface Props {
  origin: AgentMessageOrigin;
}

/**
 * Provenance badge for agent-originated messages (#1465): a user-side
 * message delivered by the agentd send_message tool renders with a
 * visible "message from session X" header so it is never mistaken for
 * human input. Placement stays user-side (that is the role the message
 * arrives as). The session ID is a return address, not an identity
 * proof (all sessions in a workspace share one credential).
 *
 * #1716: the badge renders INSIDE the user bubble — an inverted surface
 * (bg-primary + text-primary-foreground; dark: bg-slate-600 +
 * text-slate-100). Its original muted-surface tokens (text-muted-
 * foreground on bg-muted/40) composed over that surface measured 1.05:1
 * in light mode — invisible text (owner report). The badge now borrows
 * the bubble's OWN foreground as its ink (primary-foreground in light,
 * slate-100 in dark, mirroring MessageBubble's dark override), so its
 * contrast is the bubble's contrast by construction (≥10:1 both
 * schemes). Never key this component to muted tokens again — muted is
 * the assistant-surface palette; pinned by AgentOriginBadge.contrast.
 */
export function AgentOriginBadge({ origin }: Props) {
  return (
    <div
      data-testid="agent-origin-badge"
      className="mb-1.5 inline-flex max-w-full items-center gap-1.5 rounded-md border border-primary-foreground/25 bg-primary-foreground/10 px-2 py-0.5 font-mono text-[11px] text-primary-foreground dark:border-slate-100/25 dark:bg-slate-900/40 dark:text-slate-100"
    >
      <Bot className="h-3 w-3 shrink-0" aria-hidden="true" />
      {/* The session ID is the return address — it must render in full.
          break-all (house convention for monospace IDs) wraps it at the
          container edge; the old `truncate` here hid it behind an
          ellipsis (#1465 owner report). title carries the full ID for
          hover-copy. */}
      <span data-testid="agent-origin-label" className="min-w-0">
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
