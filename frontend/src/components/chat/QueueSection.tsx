import { useState, useEffect } from "react";
import type { QueuedMessage } from "../../hooks/useMessageQueue";
import { cn } from "../../lib/utils";

interface Props {
  messages: QueuedMessage[];
  onRetry: (id: string) => void;
  onDismiss: (id: string) => void;
  isMobile: boolean;
}

export function QueueSection({ messages, onRetry, onDismiss, isMobile }: Props) {
  const [open, setOpen] = useState(!isMobile);
  const count = messages.length;

  useEffect(() => {
    if (count === 0) setOpen(false);
  }, [count]);

  if (count === 0 && !open) return null;

  return (
    <div className="border-t border-border">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        className={cn(
          "flex w-full items-center gap-2 px-4 py-1.5 text-xs text-muted-foreground hover:bg-muted/50 transition-colors",
        )}
      >
        <span className={cn("transition-transform", open ? "rotate-90" : "")}>▸</span>
        {count > 0 ? (
          <span>{count} message{count !== 1 ? "s" : ""} queued</span>
        ) : (
          <span>No queued messages</span>
        )}
      </button>
      <div
        className={cn(
          "overflow-hidden transition-all duration-200",
          open ? "max-h-96 opacity-100" : "max-h-0 opacity-0",
        )}
      >
        <div className="flex flex-col gap-2 px-4 pb-2">
          {count === 0 ? (
            <p className="text-xs text-muted-foreground py-1">No queued messages</p>
          ) : (
            messages.map((m) => (
              <div
                key={m.id}
                className={cn(
                  "flex justify-end",
                )}
              >
                <div
                  className={cn(
                    "max-w-[90%] sm:max-w-[80%] rounded-lg px-4 py-2.5 min-w-0 overflow-hidden break-words",
                    m.status === "error"
                      ? "bg-destructive/20 text-destructive dark:bg-destructive/30"
                      : "bg-primary text-primary-foreground dark:bg-slate-600 dark:text-slate-100 dark:border dark:border-slate-500",
                  )}
                >
                  {m.status === "error" ? (
                    <>
                      <p className="line-through opacity-70 text-sm">{m.text}</p>
                      {m.error && <p className="text-xs mt-1 opacity-80">{m.error}</p>}
                      <div className="flex gap-2 mt-1.5">
                        <button
                          type="button"
                          aria-label="Retry"
                          className="text-xs font-medium underline hover:no-underline"
                          onClick={() => onRetry(m.id)}
                        >
                          Retry
                        </button>
                        <button
                          type="button"
                          aria-label="Dismiss"
                          className="text-xs font-medium underline hover:no-underline"
                          onClick={() => onDismiss(m.id)}
                        >
                          Dismiss
                        </button>
                      </div>
                    </>
                  ) : (
                    <p className="text-sm">{m.text}</p>
                  )}
                </div>
              </div>
            ))
          )}
        </div>
      </div>
    </div>
  );
}
