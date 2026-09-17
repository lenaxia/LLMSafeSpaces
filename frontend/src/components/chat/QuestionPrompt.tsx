import { useState, useCallback } from "react";
import { inputApi } from "../../api/input";
import type { InputRequest } from "../../api/types";
import { AgentPrompt } from "./AgentPrompt";

interface QuestionPromptProps {
  workspaceId: string;
  request: InputRequest & { whileAway?: boolean };
  onResolved: () => void;
}

export function QuestionPrompt({ workspaceId, request, onResolved }: QuestionPromptProps) {
  const [answers, setAnswers] = useState<string[][]>([[]]);
  const [customInputs, setCustomInputs] = useState<string[]>([""]);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const toggleOption = useCallback((qIdx: number, label: string, multiple: boolean) => {
    setAnswers((prev) => {
      const next = [...prev];
      const current = next[qIdx] ?? [];
      if (multiple) {
        next[qIdx] = current.includes(label) ? current.filter((l) => l !== label) : [...current, label];
      } else {
        next[qIdx] = current.includes(label) ? [] : [label];
      }
      return next;
    });
  }, []);

  const setCustom = useCallback((qIdx: number, value: string) => {
    setCustomInputs((prev) => { const next = [...prev]; next[qIdx] = value; return next; });
  }, []);

  const allAnswered = answers.every((a, i) => a.length > 0 || (customInputs[i] ?? "").trim().length > 0);

  const handleSubmit = async () => {
    setSubmitting(true);
    setError(null);
    try {
      const finalAnswers = answers.map((a, i) => {
        const custom = (customInputs[i] ?? "").trim();
        return custom ? [...a, custom] : a;
      });
      await inputApi.questionReply(workspaceId, request.id, finalAnswers);
      onResolved();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to submit");
    } finally {
      // #1365: reset on both outcomes — a surviving instance must stay
      // clickable (removal can miss on whileAway/fold-lag paths).
      setSubmitting(false);
    }
  };

  const handleDismiss = async () => {
    setSubmitting(true);
    setError(null);
    try {
      if (request.whileAway) {
        await inputApi.dismissInboxRecord(workspaceId, request.sessionId ?? "", request.id);
      } else {
        await inputApi.questionReject(workspaceId, request.id);
      }
      onResolved();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to dismiss");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <AgentPrompt
      variant="question"
      title={request.whileAway ? "While you were away, the agent asked" : undefined}
      onDismiss={handleDismiss}
      dismissDisabled={submitting}
    >
      <div className="border border-blue-200 dark:border-blue-800 rounded p-3 mb-3">
        {request.whileAway && (
          <div className="text-xs text-muted-foreground mb-2">
            This question waited unanswered. Submitting sends your answer to the chat and continues the work.
          </div>
        )}
        <div className="font-medium text-sm mb-1">{request.header}</div>
        <div className="text-sm mb-2">{request.question}</div>
        <div className="flex flex-wrap gap-2 mb-2">
          {(request.options ?? []).map((opt) => (
            <button
              key={opt.label}
              type="button"
              disabled={submitting}
              onClick={() => toggleOption(0, opt.label, !!request.multiple)}
              className={`px-3 py-1 rounded text-sm border transition-colors ${
                (answers[0] ?? []).includes(opt.label)
                  ? "bg-blue-600 text-white border-blue-600"
                  : "bg-white dark:bg-gray-800 border-gray-300 dark:border-gray-600 hover:border-blue-400"
              }`}
              title={opt.description}
              aria-pressed={(answers[0] ?? []).includes(opt.label)}
            >
              {opt.label}
            </button>
          ))}
        </div>
        <input
          type="text"
          placeholder="Or type your own..."
          value={customInputs[0] ?? ""}
          onChange={(e) => setCustom(0, e.target.value)}
          disabled={submitting}
          className="w-full px-2 py-1 text-sm border rounded bg-white dark:bg-gray-800 border-gray-300 dark:border-gray-600"
        />
      </div>

      {error && <div className="text-red-600 text-sm mb-2">{error}</div>}

      <div className="flex justify-end gap-2">
        <button onClick={handleDismiss} disabled={submitting} className="px-3 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 hover:bg-gray-100 dark:hover:bg-gray-800">
          Dismiss
        </button>
        <button onClick={handleSubmit} disabled={!allAnswered || submitting} className="px-3 py-1.5 text-sm rounded bg-blue-600 text-white hover:bg-blue-700 disabled:opacity-50 disabled:cursor-not-allowed">
          {submitting ? "Submitting..." : "Submit answers"}
        </button>
      </div>
    </AgentPrompt>
  );
}
