import { useEffect, useState } from "react";

// MermaidDiagram (#1694): renders a ```mermaid fenced block as a diagram.
//
// Bundle discipline: mermaid is ~1MB+ and must NEVER enter the main
// chunk — it is loaded via dynamic import() only when a diagram is
// actually on screen (vite emits it as a separate lazy chunk; verified
// by the production build output check in this PR's gates).
//
// Streaming: render-on-closed-fence. During streaming the fence content
// is partial; parsing per token would spam mermaid parse errors (and
// pay the lazy import for garbage). closeOpenFence keeps the markdown
// tree stable while tokens arrive; this component shows a placeholder
// and defers the parse until isStreaming flips false.
//
// Security: securityLevel 'strict' is pinned — mermaid escapes HTML in
// labels and disables click callbacks/links, required for
// user-supplied diagram text. startOnLoad false: render() is invoked
// explicitly below, never by mermaid's own DOM scan.

// mermaid.render requires a unique id per call — ids become svg-internal
// references; reuse corrupts output. A module-scoped counter is stable
// across React StrictMode double-invocation of effects.
let renderSeq = 0;

interface Props {
  code: string;
  /** resolved app theme — mermaid's own theme follows it */
  theme: "light" | "dark";
  isStreaming: boolean;
}

export function MermaidDiagram({ code, theme, isStreaming }: Props) {
  const [svg, setSvg] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (isStreaming) return;

    let cancelled = false;
    // Reset stale state from a previous code/theme so an old diagram or
    // error never flashes next to new content.
    setSvg(null);
    setError(null);

    const id = `lsp-mermaid-${renderSeq++}`;
    import("mermaid")
      .then((mod) => {
        if (cancelled) return null;
        const mermaid = mod.default;
        mermaid.initialize({
          startOnLoad: false,
          securityLevel: "strict",
          // mermaid's light theme is named "default"
          theme: theme === "dark" ? "dark" : "default",
        });
        return mermaid.render(id, code);
      })
      .then((result) => {
        if (cancelled || !result) return;
        setSvg(result.svg);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        // Malformed syntax → graceful inline error state, never a crash.
        // mermaid also leaves a diagnostic element (#d<id>) in the body
        // on parse failure — remove it so it can't leak into the page.
        document.getElementById(`d${id}`)?.remove();
        setError(err instanceof Error ? err.message : String(err));
      });

    return () => {
      cancelled = true;
    };
  }, [code, theme, isStreaming]);

  if (isStreaming) {
    return (
      <div
        data-testid="mermaid-pending"
        className="not-prose my-4 rounded-md border border-border bg-muted/30 px-3 py-2"
      >
        <p className="text-xs font-medium text-muted-foreground">
          Mermaid diagram — renders when the message completes
        </p>
        <pre className="overflow-x-auto touch-manipulation text-xs font-mono text-muted-foreground whitespace-pre-wrap">
          {code}
        </pre>
      </div>
    );
  }

  if (error !== null) {
    return (
      <div
        data-testid="mermaid-error"
        className="not-prose my-4 rounded-md border border-red-500/30 bg-red-500/5 px-3 py-2"
      >
        <p className="text-xs font-medium text-red-600 dark:text-red-400">
          Mermaid diagram failed to render (invalid mermaid syntax)
        </p>
        <details className="mt-1">
          <summary className="cursor-pointer text-xs text-muted-foreground">
            parse error
          </summary>
          <pre className="overflow-x-auto touch-manipulation mt-1 text-xs font-mono text-muted-foreground whitespace-pre-wrap">
            {error}
          </pre>
        </details>
        <pre className="overflow-x-auto touch-manipulation mt-1 text-xs font-mono text-muted-foreground whitespace-pre-wrap">
          {code}
        </pre>
      </div>
    );
  }

  if (svg !== null) {
    // mermaid output under securityLevel 'strict' is sanitized by
    // mermaid itself (HTML in labels escaped, links/callbacks disabled),
    // so dangerouslySetInnerHTML is appropriate here — same posture as
    // the shiki highlight path in CodeBlock.
    return (
      <div
        data-testid="mermaid-diagram"
        role="img"
        aria-label="Mermaid diagram"
        className="not-prose my-4 flex justify-center overflow-x-auto touch-manipulation rounded-md border border-border px-4 py-3 [&_svg]:max-w-full"
        dangerouslySetInnerHTML={{ __html: svg }}
      />
    );
  }

  return (
    <div
      data-testid="mermaid-loading"
      className="not-prose my-4 rounded-md border border-border px-4 py-3 text-sm text-muted-foreground"
    >
      <span className="animate-pulse">Rendering diagram…</span>
    </div>
  );
}
