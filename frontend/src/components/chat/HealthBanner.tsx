import { AlertTriangle, Info, Wifi } from "lucide-react";
import type { CredentialState, AgentHealth } from "../../api/types";

interface Props {
  credentialState?: CredentialState;
  agentHealth?: AgentHealth;
}

function credentialLabel(state?: CredentialState) {
  if (!state || state.available) return null;
  if (state.reason === "NotChecked") return null;
  if (state.reason === "CredentialSecretNotFound") {
    return {
      icon: Info,
      node: (
        <>
          No providers configured, using Opencode Zen free models.{" "}
          <a
            href="https://opencode.ai"
            target="_blank"
            rel="noopener noreferrer"
            className="underline hover:text-yellow-800 dark:hover:text-yellow-300"
          >
            Click here to learn more
          </a>
        </>
      ),
    };
  }
  const reasons: Record<string, string> = {
    CredentialEmpty: "Credentials are empty",
    CredentialInvalid: "Credentials are invalid",
    CredentialCheckError: "Credential check failed",
    CredentialValidationError: "Credential validation failed",
  };
  const label = reasons[state.reason ?? ""] ?? state.message;
  if (!label) return null;
  return { icon: AlertTriangle, node: label };
}

function agentLabel(health?: AgentHealth) {
  if (!health) return null;
  if (health.status === "Healthy") {
    // Healthy with warnings (e.g. default model unresolvable — incident
    // 2026-08-16) must still surface: the agent is fine but running
    // degraded, and silently substituting the model breaks user intent.
    return health.warnings?.length ? health.warnings : null;
  }
  // The backend appends "; warnings: ..." to the condition message; the
  // structured warnings render as their own rows (warningRows below), so
  // strip the suffix here to avoid duplication.
  const message = health.message?.split("; warnings: ")[0];
  const labels: Record<string, string> = {
    Degraded: message || "Agent degraded — no providers connected",
    Unhealthy: message || "Agent is unhealthy",
    Unknown: "Agent health unknown",
  };
  return labels[health.status] ?? message ?? null;
}

// Warning rows render for ANY status: the API parses the structured
// warnings field from the condition regardless of health state (a
// Degraded agent can also carry a model-resolution warning). The status
// label itself comes from agentLabel.
function warningRows(health?: AgentHealth): string[] {
  return health?.warnings ?? [];
}

export function HealthBanner({ credentialState, agentHealth }: Props) {
  const credIssue = credentialLabel(credentialState);
  const agentIssues = agentLabel(agentHealth);
  const agentNodes = Array.isArray(agentIssues) ? agentIssues : agentIssues ? [agentIssues] : [];
  const warnings = warningRows(agentHealth);
  const degraded = agentHealth?.status === "Degraded";

  if (!credIssue && agentNodes.length === 0 && warnings.length === 0) return null;

  return (
    <div className="flex flex-col gap-1 border-b border-border bg-yellow-500/5 px-4 py-2 text-sm">
      {credIssue && (
        <div className="flex items-center gap-2 text-yellow-600 dark:text-yellow-400">
          <credIssue.icon className="h-3.5 w-3.5 flex-shrink-0" />
          {credIssue.node}
        </div>
      )}
      {agentNodes.map((node) => (
        <div
          key={node}
          className="flex items-center gap-2 text-yellow-600 dark:text-yellow-400"
        >
          {degraded ? (
            <Wifi className="h-3.5 w-3.5 flex-shrink-0" />
          ) : (
            <AlertTriangle className="h-3.5 w-3.5 flex-shrink-0" />
          )}
          <span>{node}</span>
        </div>
      ))}
      {warnings
        .filter((w) => !agentNodes.includes(w))
        .map((w) => (
          <div
            key={`warn-${w}`}
            className="flex items-center gap-2 text-yellow-600 dark:text-yellow-400"
          >
            <AlertTriangle className="h-3.5 w-3.5 flex-shrink-0" />
            <span>{w}</span>
          </div>
        ))}
    </div>
  );
}
