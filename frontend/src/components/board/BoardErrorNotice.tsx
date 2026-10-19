// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { Button } from "../ui/Button";
import { AlertTriangle } from "lucide-react";
import type { DescribedBoardError } from "../../lib/boardRules";

interface BoardErrorNoticeProps {
  error: DescribedBoardError;
  /** Heading for where the error surfaced (e.g. "Move refused"). */
  title?: string;
  /** Clicking a suggestion chip calls this (fills the invoking input). */
  onSuggestionPick?: (suggestion: string) => void;
  onDismiss?: () => void;
}

/**
 * BoardErrorNotice renders the board error classes verbatim (§4.8 +
 * renderBoardError's shapes): 404s carry did-you-mean suggestions
 * (clickable chips), 409s carry the engine's guard code — both shown
 * honestly, never swallowed into a generic banner.
 */
export function BoardErrorNotice({ error, title, onSuggestionPick, onDismiss }: BoardErrorNoticeProps) {
  return (
    <div
      role="alert"
      data-board-error={error.code ?? "error"}
      className="flex flex-col gap-1.5 rounded-md border border-destructive/40 bg-destructive/5 px-3 py-2 text-xs"
    >
      <div className="flex items-start justify-between gap-2">
        <div className="flex items-start gap-1.5">
          <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0 text-destructive" />
          <div>
            {title && <span className="block font-medium text-destructive">{title}</span>}
            <span className="block text-foreground">{error.message}</span>
            {error.hint && <span className="block text-muted-foreground">{error.hint}</span>}
            {error.code && (
              <span className="mt-0.5 inline-block rounded bg-muted px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground">
                {error.code}
              </span>
            )}
          </div>
        </div>
        {onDismiss && (
          <Button variant="ghost" size="sm" onClick={onDismiss} aria-label="Dismiss error">
            ×
          </Button>
        )}
      </div>
      {error.suggestions.length > 0 && (
        <div className="flex flex-wrap items-center gap-1" data-testid="board-error-suggestions">
          <span className="text-muted-foreground">Did you mean:</span>
          {error.suggestions.map((s) => (
            <button
              key={s}
              type="button"
              onClick={() => onSuggestionPick?.(s)}
              className="rounded bg-accent px-1.5 py-0.5 font-mono text-[10px] hover:bg-accent/70"
            >
              {s}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
