import { lazy, Suspense, useCallback, useEffect, useRef, useState } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import rehypeSanitize from "rehype-sanitize";
import { Brain, Check, Copy, Wrench, Server } from "lucide-react";
import { cn } from "../../lib/utils";
import { useUserSetting } from "../../hooks/useUserSettings";
import { useTheme } from "../../providers/ThemeProvider";
import { highlight } from "../../lib/shiki";
import { LazyDetails } from "../ui/LazyDetails";
import type { MessagePart as MessagePartType } from "../../api/types";

const ReactDiffViewer = lazy(() => import("react-diff-viewer-continued"));

function MarkdownLink({ href, children, node: _node, ...props }: React.AnchorHTMLAttributes<HTMLAnchorElement> & { node?: unknown }) {
  return (
    <a {...props} href={href} target="_blank" rel="noopener noreferrer">
      {children}
    </a>
  );
}

/**
 * If the text ends with an unclosed CommonMark fenced code block, append
 * the appropriate closing fence. Used during streaming to prevent an
 * in-progress code block from swallowing the rest of the message.
 *
 * Handles:
 *   - 3+ backtick fences (```python, ````go, etc.)
 *   - 3+ tilde fences (~~~, ~~~~)
 *   - Language info strings on opening fence
 *   - 0-3 leading spaces (CommonMark §4.5 indented fences)
 *   - CRLF/CR line endings (normalized to LF)
 *
 * Known limitation: fences inside blockquotes (> ```) are not detected.
 * LLMs rarely produce these; handling them would require a full parser.
 */
export function closeOpenFence(text: string): string {
  const fenceLine = /^([ ]{0,3})(`{3,}|~{3,})[^`~]*$/m;

  let openChar: string | null = null;
  let openLen = 0;
  let openIndent = "";

  const normalized = text.replace(/\r\n?/g, "\n");
  for (const line of normalized.split("\n")) {
    const match = fenceLine.exec(line);
    if (!match) continue;

    const fenceStr = match[2];
    if (!fenceStr) continue;
    const indent = match[1] ?? "";
    const char = fenceStr.charAt(0);
    const len = fenceStr.length;

    if (openChar === null) {
      openChar = char;
      openLen = len;
      openIndent = indent;
    } else if (char === openChar && len >= openLen) {
      openChar = null;
      openLen = 0;
      openIndent = "";
    }
  }

  if (openChar !== null) {
    return normalized + "\n" + openIndent + openChar.repeat(openLen);
  }

  return normalized;
}

interface CodeBlockProps {
  code: string;
  lang: string | null;
  wordWrap: boolean;
  isStreaming: boolean;
}

function CodeBlock({ code, lang, wordWrap, isStreaming }: CodeBlockProps) {
  const [highlightedHtml, setHighlightedHtml] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const timerRef = useRef<ReturnType<typeof setTimeout>>(undefined);

  useEffect(() => {
    // Do not highlight during streaming. The code content changes on every
    // token, which would cause a highlight() call per token and a visible
    // plain→highlighted flash on every update.
    // Wait until streaming is complete before calling highlight().
    if (isStreaming || !lang) return;

    let cancelled = false;
    highlight(code, lang)
      .then((html) => {
        if (!cancelled) setHighlightedHtml(html);
      })
      .catch(() => {
        if (!cancelled) setHighlightedHtml(null);
      });
    return () => {
      cancelled = true;
    };
  }, [code, lang, isStreaming]);

  // Reset highlighted state if streaming restarts (defensive — streaming
  // never restarts on an existing message part in current architecture).
  useEffect(() => {
    if (isStreaming) {
      setHighlightedHtml(null);
    }
  }, [isStreaming]);

  useEffect(() => () => clearTimeout(timerRef.current), []);

  const handleCopy = useCallback(async () => {
    try {
      await navigator.clipboard.writeText(code);
      clearTimeout(timerRef.current);
      setCopied(true);
      timerRef.current = setTimeout(() => setCopied(false), 2000);
    } catch {
      // clipboard write failed — stay in idle state
    }
  }, [code]);

  const CopyIcon = copied ? Check : Copy;
  const copyLabel = copied ? "Copied" : "Copy code";

  return (
    // not-prose: opt out of Tailwind Typography's pre/code rules so they
    // don't double-pad or conflict with shiki's inline background styles.
    <div className="not-prose my-4 rounded-md overflow-hidden border border-border">
      {lang !== null && (
        <div className="flex items-center justify-between px-3 py-1.5 bg-muted/60 border-b border-border">
          <span className="text-xs font-mono text-muted-foreground">{lang}</span>
          <button
            type="button"
            onClick={handleCopy}
            aria-label={copyLabel}
            className={cn(
              "rounded p-0.5 transition-all text-muted-foreground/70",
              "hover:text-muted-foreground hover:scale-110 active:scale-95",
              copied && "text-green-500",
            )}
          >
            <CopyIcon className="h-3.5 w-3.5" />
          </button>
        </div>
      )}

      {highlightedHtml ? (
        // shiki output is safe: contains only <pre><code><span style="..."> elements.
        // No href, src, event handlers, or <script> tags are possible in shiki's
        // output format. dangerouslySetInnerHTML is appropriate here.
        <div
          className={cn(
            "overflow-x-auto touch-manipulation text-sm [&_pre]:p-4 [&_pre]:m-0",
            wordWrap && "[&_pre]:whitespace-pre-wrap [&_pre]:break-words",
          )}
          dangerouslySetInnerHTML={{ __html: highlightedHtml }}
        />
      ) : (
        <pre
          className={cn(
            "overflow-x-auto touch-manipulation p-4 text-sm font-mono",
            wordWrap && "whitespace-pre-wrap break-words",
          )}
        >
          <code>{code}</code>
        </pre>
      )}
    </div>
  );
}

function ToolInput({ input }: { input: unknown }) {
  if (!input || typeof input !== "object") {
    return <pre className="text-xs text-muted-foreground font-mono">{String(input)}</pre>;
  }
  const obj = input as Record<string, unknown>;
  if ("command" in obj && typeof obj.command === "string") {
    return <code className="block text-xs font-mono text-muted-foreground bg-muted/50 rounded px-2 py-1 whitespace-pre-wrap">$ {obj.command}</code>;
  }
  if ("url" in obj && typeof obj.url === "string") {
    return <code className="block text-xs font-mono text-muted-foreground truncate">{obj.url}</code>;
  }
  if ("path" in obj && typeof obj.path === "string" && Object.keys(obj).length <= 2) {
    return <code className="block text-xs font-mono text-muted-foreground truncate">{obj.path}</code>;
  }
  return <pre className="text-xs text-muted-foreground font-mono whitespace-pre-wrap max-h-20 overflow-y-auto">{JSON.stringify(input, null, 2)}</pre>;
}

function ToolDetails({ borderColor, textColor, statusIcon, toolName, filePath, children }: {
  borderColor: string; textColor: string; statusIcon: string; toolName: string; filePath: string; children: React.ReactNode;
}) {
  return (
    <LazyDetails
      className={cn("my-1.5 rounded-md border", borderColor)}
      contentClassName="border-t border-inherit py-1 space-y-1 min-w-0 overflow-hidden"
      summary={
        <summary className={cn("flex cursor-pointer items-center gap-2 px-3 py-2 text-xs font-medium overflow-hidden", textColor)}>
          <Wrench className="h-3.5 w-3.5 flex-shrink-0" />
          <span className="truncate">{statusIcon} {toolName || "tool"}{filePath ? ` — ${filePath}` : ""}</span>
        </summary>
      }
    >
      {children}
    </LazyDetails>
  );
}

function ToolDiffView({ oldStr, newStr, isDark }: { oldStr: string; newStr: string; isDark: boolean }) {
  return (
    <Suspense fallback={<pre className="px-3 text-xs text-muted-foreground">Loading diff...</pre>}>
      <div className="text-xs overflow-auto touch-manipulation max-h-60">
        <ReactDiffViewer
          oldValue={oldStr}
          newValue={newStr}
          splitView={false}
          useDarkTheme={isDark}
          hideLineNumbers={false}
          styles={{
            contentText: { fontSize: "11px", lineHeight: "1.4" },
            gutter: { minWidth: "20px", padding: "0 4px" },
            lineNumber: { fontSize: "9px" },
          }}
        />
      </div>
    </Suspense>
  );
}

interface Props {
  part: MessagePartType;
  isUser: boolean;
  isStreaming?: boolean;
}

export function MessagePart({ part, isUser, isStreaming }: Props) {
  const wordWrap = useUserSetting("codeBlockWordWrap", false);
  const { resolved } = useTheme();

  if (part.type === "text" && part.text) {
    if (isUser) {
      return <p className="whitespace-pre-wrap text-sm">{part.text}</p>;
    }
    let text = part.text;
    if (isStreaming) {
      text = closeOpenFence(text);
    }
    return (
      <div className={cn(
        "prose prose-sm dark:prose-invert max-w-none",
        "[&_table]:block [&_table]:overflow-x-auto [&_table]:touch-manipulation",
      )}>
        <ReactMarkdown
          remarkPlugins={[remarkGfm]}
          rehypePlugins={[rehypeSanitize]}
          components={{
            a: MarkdownLink,
            pre({ children }) {
              const child = (Array.isArray(children) ? children[0] : children) as
                | React.ReactElement<{ className?: string; children?: string }>
                | null
                | undefined;
              const classStr = child?.props?.className ?? "";
              const lang = classStr.startsWith("language-")
                ? classStr.slice("language-".length)
                : null;
              const childrenProp = child?.props?.children;
              if (typeof childrenProp !== "string") {
                return <pre className="overflow-x-auto touch-manipulation p-4 text-sm font-mono">{children}</pre>;
              }
              const code = childrenProp.trimEnd();
              if (!code && lang === null) {
                return <pre className="overflow-x-auto touch-manipulation p-4 text-sm font-mono">{children}</pre>;
              }
              return <CodeBlock code={code} lang={lang} wordWrap={wordWrap} isStreaming={isStreaming ?? false} />;
            },
            code({ className, style, children }) {
              return (
                <code className={cn("break-all", className)} style={style}>
                  {children}
                </code>
              );
            },
          }}
        >
          {text}
        </ReactMarkdown>
      </div>
    );
  }

  if ((part.type === "thinking" || part.type === "reasoning") && part.text) {
    const content = (
      <div className="border-l-2 border-muted-foreground/30 pl-3 text-xs text-muted-foreground italic">
        <ReactMarkdown remarkPlugins={[remarkGfm]} rehypePlugins={[rehypeSanitize]} components={{ a: MarkdownLink }}>
          {part.text}
        </ReactMarkdown>
      </div>
    );

    if (isStreaming) {
      return (
        <div className="my-2 rounded-md border border-muted-foreground/20 bg-muted/30">
          <div className="flex items-center gap-2 px-3 py-1.5 text-xs font-medium text-muted-foreground">
            <Brain className="h-3.5 w-3.5 animate-pulse" />
            <span>Thinking…</span>
          </div>
          <div className="border-t border-muted-foreground/10 px-3 py-2">
            {content}
          </div>
        </div>
      );
    }

    return (
      <details className="group my-2 rounded-md border border-muted-foreground/20 bg-muted/30">
        <summary className="flex cursor-pointer items-center gap-2 px-3 py-1.5 text-xs font-medium text-muted-foreground hover:text-foreground">
          <Brain className="h-3.5 w-3.5" />
          Thinking
        </summary>
        <div className="border-t border-muted-foreground/10 px-3 py-2">
          {content}
        </div>
      </details>
    );
  }

  if (part.type === "tool_use" || part.type === "tool_call") {
    const toolName = part.name ?? part.text ?? "tool";
    const hasDetails = part.input || part.toolOutput;
    const statusIcon = part.toolState === "completed" ? "✓" : part.toolState === "error" ? "✗" : part.toolState === "running" ? "⟳" : "…";
    const borderColor = part.toolState === "error" ? "border-red-500/20 bg-red-500/5" : "border-blue-500/20 bg-blue-500/5";
    const textColor = part.toolState === "error" ? "text-red-600 dark:text-red-400" : "text-blue-600 dark:text-blue-400";

    const input = part.input as Record<string, unknown> | undefined;
    const isFileEdit = input && typeof input === "object" && (
      ("oldString" in input && "newString" in input) || ("oldStr" in input && "newStr" in input)
    );
    const isFileWrite = !isFileEdit && input && typeof input === "object" && "content" in input && "filePath" in input;
    const filePath = input && typeof input === "object" ? (input.filePath as string) || (input.path as string) || (input.file_path as string) || "" : "";

    if (!hasDetails) {
      return (
        <div className={cn("my-1.5 rounded-md border px-3 py-2", borderColor)}>
          <div className={cn("flex items-center gap-2 text-xs font-medium overflow-hidden", textColor)}>
            <Wrench className="h-3.5 w-3.5 flex-shrink-0" />
            <span className="truncate">{statusIcon} {toolName || "tool"}</span>
          </div>
        </div>
      );
    }

    return (
      <ToolDetails borderColor={borderColor} textColor={textColor} statusIcon={statusIcon} toolName={toolName} filePath={filePath}>
        {isFileEdit ? (
          <ToolDiffView
            oldStr={String((input as Record<string, unknown>).oldString ?? (input as Record<string, unknown>).oldStr ?? "")}
            newStr={String((input as Record<string, unknown>).newString ?? (input as Record<string, unknown>).newStr ?? "")}
            isDark={resolved === "dark"}
          />
        ) : isFileWrite ? (
          <pre className="overflow-x-auto touch-manipulation text-xs text-muted-foreground whitespace-pre-wrap font-mono max-h-60 overflow-y-auto px-3 py-1">
            {String((input as Record<string, unknown>).content ?? "")}
          </pre>
        ) : (
          <>
            {part.input != null && (
              <div className="px-3 py-1">
                <ToolInput input={part.input} />
              </div>
            )}
            {part.toolOutput && (
              <details className="border-t border-muted">
                <summary className="px-3 py-1 text-xs text-muted-foreground cursor-pointer hover:text-foreground">
                  Output ({part.toolOutput.length > 200 ? `${Math.ceil(part.toolOutput.length / 1024)}KB` : `${part.toolOutput.length} chars`})
                </summary>
                <pre className="overflow-x-auto touch-manipulation text-xs text-muted-foreground whitespace-pre-wrap font-mono max-h-60 overflow-y-auto px-3 py-1">
                  {part.toolOutput}
                </pre>
              </details>
            )}
          </>
        )}
      </ToolDetails>
    );
  }

  if (part.type === "tool_result" && (part.text || typeof part.text === "string")) {
    return (
      <div className="my-1.5 rounded-md border border-green-500/20 bg-green-500/5 px-3 py-2">
        <div className="flex items-center gap-2 text-xs font-medium text-green-600 dark:text-green-400">
          <Server className="h-3.5 w-3.5" />
          Tool result
        </div>
        <pre className="mt-1 overflow-x-auto touch-manipulation text-xs text-muted-foreground whitespace-pre-wrap font-mono">
          {part.text ?? ""}
        </pre>
      </div>
    );
  }

  if (part.type === "error" && part.text) {
    return (
      <div className="my-1.5 rounded-md border border-destructive/30 bg-destructive/10 px-3 py-2">
        <p className="text-sm text-destructive">{part.text}</p>
      </div>
    );
  }

  return null;
}
