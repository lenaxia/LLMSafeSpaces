import { useState } from "react";
import { inputApi } from "../../api/input";
import type { PermissionRequest } from "../../api/types";
import { AgentPrompt } from "./AgentPrompt";

interface PermissionPromptProps {
  workspaceId: string;
  request: PermissionRequest;
  onResolved: () => void;
}

const PERMISSION_LABELS: Record<string, string> = {
  shell: "Run shell command",
  write: "Write file",
  edit: "Edit file",
  read: "Read file",
};

function formatPermission(p: string): string {
  return PERMISSION_LABELS[p] ?? p.charAt(0).toUpperCase() + p.slice(1);
}

export function PermissionPrompt({ workspaceId, request, onResolved }: PermissionPromptProps) {
  const [submitting, setSubmitting] = useState(false);
  const [showFeedback, setShowFeedback] = useState(false);
  const [feedback, setFeedback] = useState("");
  const [error, setError] = useState<string | null>(null);

  const handleReply = async (reply: "once" | "always" | "reject") => {
    if (reply === "reject" && !showFeedback) {
      setShowFeedback(true);
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      const message = reply === "reject" && feedback.trim() ? feedback.trim() : undefined;
      await inputApi.permissionReply(workspaceId, request.id, reply, message);
      onResolved();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to respond");
      setSubmitting(false);
    }
  };

  return (
    <AgentPrompt
      variant="permission"
      title={request.whileAway ? "While you were away, the agent asked" : undefined}
    >
      {request.whileAway && (
        <div className="text-xs text-muted-foreground mb-2">
          This request already ended without permission. Your decision lands in the conversation as guidance for future turns.
        </div>
      )}
      <div className="text-sm mb-1">
        The agent wants to: <strong>{formatPermission(request.permission)}</strong>
      </div>
      {request.patterns.length > 0 && (
        <div className="text-sm mb-3">
          <span className="text-muted-foreground">On: </span>
          {request.patterns.map((p, i) => (
            <code key={i} className="bg-gray-100 dark:bg-gray-800 px-1 rounded text-xs mr-1">{p}</code>
          ))}
        </div>
      )}

      {error && <div className="text-red-600 text-sm mb-2">{error}</div>}

      {showFeedback && (
        <input
          type="text"
          placeholder="Feedback (optional)"
          value={feedback}
          onChange={(e) => setFeedback(e.target.value)}
          disabled={submitting}
          className="w-full px-2 py-1 text-sm border rounded bg-white dark:bg-gray-800 border-gray-300 dark:border-gray-600 mb-2"
          aria-label="Feedback"
          autoFocus
        />
      )}

      <div className="flex gap-2">
        <button onClick={() => handleReply("once")} disabled={submitting} className="px-3 py-1.5 text-sm rounded border border-green-500 text-green-700 dark:text-green-400 hover:bg-green-50 dark:hover:bg-green-950">
          Allow once
        </button>
        <button onClick={() => handleReply("always")} disabled={submitting} className="px-3 py-1.5 text-sm rounded bg-green-600 text-white hover:bg-green-700">
          Allow always
        </button>
        <button onClick={() => handleReply("reject")} disabled={submitting} className="px-3 py-1.5 text-sm rounded border border-red-500 text-red-700 dark:text-red-400 hover:bg-red-50 dark:hover:bg-red-950">
          {showFeedback ? "Confirm deny" : "Deny"}
        </button>
      </div>
    </AgentPrompt>
  );
}
