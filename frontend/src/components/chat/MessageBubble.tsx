import { useCallback, useEffect, useRef, useState } from "react";
import type { Message, MessagePart } from "../../api/types";
import { cn } from "../../lib/utils";
import { MessagePart as MessagePartComponent } from "./MessagePart";
import { Copy, Check } from "lucide-react";
import { useNow } from "../../hooks/useNow";

interface Props {
  message: Message;
  isStreaming?: boolean;
  modelName?: string;
}

export function extractMessageText(parts: MessagePart[]): string {
  return parts
    .map((part) => {
      if (part.type === "text" && part.text) return part.text;
      if ((part.type === "thinking" || part.type === "reasoning") && part.text) return `[Thinking]\n${part.text}`;
      if ((part.type === "tool_use" || part.type === "tool_call") && part.text) {
        const output = part.toolOutput ? `\nOutput: ${part.toolOutput}` : "";
        return `[Tool: ${part.text}]${output}`;
      }
      if (part.type === "error" && part.text) return part.text;
      return "";
    })
    .filter(Boolean)
    .join("\n\n");
}

function formatTimestamp(iso: string, now: number): string {
  const date = new Date(iso);
  const diffMs = now - date.getTime();
  const diffMinutes = Math.floor(diffMs / 60000);

  if (diffMinutes < 1) return "just now";
  if (diffMinutes < 60) return `${diffMinutes}m ago`;

  const nowDate = new Date(now);
  const isToday =
    date.getDate() === nowDate.getDate() &&
    date.getMonth() === nowDate.getMonth() &&
    date.getFullYear() === nowDate.getFullYear();

  const timeStr = date.toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" });
  if (isToday) return timeStr;

  return date.toLocaleDateString(undefined, { month: "short", day: "numeric" }) + ", " + timeStr;
}

export function MessageBubble({ message, isStreaming, modelName }: Props) {
  const isUser = message.role === "user";
  const [copied, setCopied] = useState(false);
  const timerRef = useRef<ReturnType<typeof setTimeout>>(undefined);
  const now = useNow();

  const handleCopy = useCallback(async () => {
    const text = extractMessageText(message.parts);
    try {
      await navigator.clipboard.writeText(text);
      clearTimeout(timerRef.current);
      setCopied(true);
      timerRef.current = setTimeout(() => setCopied(false), 2000);
    } catch {
      // clipboard write failed — stay in idle state
    }
  }, [message.parts]);

  useEffect(() => () => clearTimeout(timerRef.current), []);

  const showTimestamp = !!message.createdAt;
  const showModel = !isUser && !!modelName;

  const CopyIcon = copied ? Check : Copy;
  const copyLabel = copied ? "Copied" : "Copy message";

  return (
    <div className={cn("flex w-full group", isUser ? "justify-end" : "justify-start")}>
      <div
        className={cn(
          "max-w-[90%] sm:max-w-[80%] rounded-lg px-4 py-2.5 min-w-0 overflow-hidden break-words",
          isUser
            ? "bg-primary text-primary-foreground dark:bg-slate-600 dark:text-slate-100 dark:border dark:border-slate-500"
            : "bg-muted text-foreground",
        )}
      >
        {message.parts.map((part, i) => (
          <MessagePartComponent key={i} part={part} isUser={isUser} isStreaming={isStreaming} />
        ))}

        <div className={cn(
          "flex items-center justify-between gap-2",
          (showTimestamp || showModel) && "mt-1.5 -mb-0.5",
        )}>
          <span
            className={cn(
              "text-xs leading-none truncate",
              // #1716: /70 dilution measured 2.58:1 on the muted assistant
              // bubble — below the 4.5:1 AA bar. Full-strength muted-
              // foreground measures 4.88:1 (post-palette-fix). The user
              // side keeps its /70 — primary-foreground/70 over bg-primary
              // still measures 8.8:1.
              isUser ? "text-primary-foreground/70 dark:text-slate-100/80" : "text-muted-foreground",
            )}
          >
            {showTimestamp && (
              <span data-testid="message-timestamp">{formatTimestamp(message.createdAt!, now)}</span>
            )}
            {showTimestamp && showModel && " · "}
            {showModel && (
              <span data-testid="message-model">{modelName}</span>
            )}
          </span>

          <button
            type="button"
            onClick={handleCopy}
            aria-label={copyLabel}
            className={cn(
              "shrink-0 rounded p-0.5 transition-all",
              "opacity-40 group-hover:opacity-100 focus:opacity-100",
              "hover:scale-110 active:scale-95",
              copied
                ? "text-green-500"
                : isUser
                  ? "text-primary-foreground/70 hover:text-primary-foreground dark:text-slate-100/80 dark:hover:text-slate-100"
                  : "text-muted-foreground/70 hover:text-muted-foreground",
            )}
          >
            <CopyIcon className="h-3.5 w-3.5" />
          </button>
        </div>
      </div>
    </div>
  );
}
