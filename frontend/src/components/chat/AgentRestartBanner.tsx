import { AlertTriangle } from "lucide-react";
import { useState } from "react";
import type { AgentRestartEvent } from "../../api/types";

interface Props {
  restart?: AgentRestartEvent;
}

// Human copy for the closed reason set — agent-neutral (Rule 12
// containment: no tool names on user-facing surfaces).
const REASON_LABELS: Record<string, string> = {
  health_watchdog: "the agent stopped responding to health checks",
  health_watchdog_unknown_episode: "health evidence was unavailable for too long",
  crash: "the agent process exited unexpectedly",
  oom: "the agent process ran out of memory",
};

const VERDICT_LABELS: Record<string, string> = {
  hung: "classified as unresponsive",
  starved: "the agent was starved of CPU but making progress",
  flat: "the agent was blocked on upstream I/O",
  respawn: "recovery was already in progress",
  unknown: "evidence was unavailable",
};

function restartStamp(restart: AgentRestartEvent): string {
  return restart.at;
}

/**
 * Dismissible banner shown when the workspace's agent was restarted by
 * the supervisor (#1760; the mechanism rides the reason field —
 * health_watchdog/crash/oom). Rendered on chat load from the
 * workspace status (supervisor.lastAgentRestart) and live from the
 * agent_restarted SSE event. Dismissal is keyed by the restart's
 * timestamp: a NEW restart re-shows the banner.
 */
export function AgentRestartBanner({ restart }: Props) {
  const [dismissedStamp, setDismissedStamp] = useState<string | null>(null);

  if (!restart || !restart.at) return null;
  if (dismissedStamp === restartStamp(restart)) return null;

  const reason = REASON_LABELS[restart.reason] ?? `reason: ${restart.reason}`;
  const verdict = restart.verdict && VERDICT_LABELS[restart.verdict]
    ? ` (${VERDICT_LABELS[restart.verdict]})`
    : "";
  const time = new Date(restart.at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });

  return (
    <div
      role="alert"
      data-testid="agent-restart-banner"
      className="flex items-center gap-2 border-b border-yellow-200 bg-yellow-50 px-4 py-2 text-xs text-yellow-800 dark:border-yellow-800 dark:bg-yellow-950 dark:text-yellow-200"
    >
      <AlertTriangle className="h-3.5 w-3.5 shrink-0" />
      <span>
        The agent in this workspace was restarted by the supervisor at{" "}
        {time} — {reason}{verdict}. In-flight work may have been
        interrupted.
      </span>
      <button
        className="ml-auto shrink-0 underline hover:no-underline"
        onClick={() => setDismissedStamp(restartStamp(restart))}
      >
        Dismiss
      </button>
    </div>
  );
}
