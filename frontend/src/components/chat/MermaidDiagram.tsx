import { useEffect, useState } from "react";
import { Button } from "../ui/Button";

// MermaidDiagram (#1694): renders a ```mermaid fenced block as a diagram.
//
// Bundle discipline: mermaid is ~1MB+ and must NEVER enter the main
// chunk — it is loaded via dynamic import() only when a diagram is
// actually on screen (vite emits it as a separate lazy chunk; verified
// by the production build output check in this PR's gates).
//
// Failure classification (#2013): the lazy chunk cuts both ways — minutes
// after a deploy, a stale open tab still holds the OLD index.html asset
// manifest, so its `import("mermaid")` asks for a hashed chunk that 404s
// (browser phrasing: TypeError "Failed to fetch dynamically imported
// module"). That is a LOAD failure about the APP, not a verdict on the
// diagram — the incident's diagram was valid, and the agent burned two
// rewrite rounds "fixing" correct mermaid because this component claimed
// "invalid mermaid syntax". Classification is TWO-LAYERED:
//
//   1. PRIMARY — the import BOUNDARY: `import("mermaid")` carries a
//      sentinel-rethrow catch (MermaidChunkLoadError), so ANY import()
//      rejection is tagged structurally, whatever the browser phrasing.
//   2. FALLBACK — the terminal catch, TypeError-gated message shape:
//      mermaid 12's Diagram.fromText awaits its per-diagram-type lazy
//      imports UNGUARDED (`await loader38()` → import("./chunks/
//      mermaid.core/<type>Diagram-*.mjs")), so on a stale tab whose
//      mermaid CORE is already in memory, a not-yet-loaded diagram type
//      404s the INNER chunk and the TypeError escapes render() untagged
//      — the boundary never sees it (#2013 review r1). A TypeError
//      carrying one of the three engines' fetch-failure phrasings is
//      still a load failure; a mermaid parse error is a plain Error with
//      jison position text and never carries a fetch phrase, so this
//      fallback cannot swallow genuine syntax failures.
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

// Sentinel for the #2013 load class: the import() itself failed (chunk
// 404 on a stale tab, offline, CSP). The PRIMARY classification layer —
// where the failure occurred, not what its message says.
class MermaidChunkLoadError extends Error {}

// FALLBACK layer (#2013 review r1): mermaid's own inner per-diagram-type
// lazy imports reject with these browser phrases INSIDE render(), past
// the boundary sentinel. TypeError-gated so a plain-Error parse failure
// (or anything else) can never match; the three spellings are Chrome's,
// Safari's, and Firefox's dynamic-import fetch failures respectively.
function isChunkFetchFailure(err: unknown): boolean {
  return (
    err instanceof TypeError &&
    /failed to fetch dynamically imported module|importing a module script failed|error loading dynamically imported module/i.test(
      err.message,
    )
  );
}

type MermaidFailure =
  | { kind: "chunk-load"; message: string }
  | { kind: "syntax"; message: string };

interface Props {
  code: string;
  /** resolved app theme — mermaid's own theme follows it */
  theme: "light" | "dark";
  isStreaming: boolean;
}

export function MermaidDiagram({ code, theme, isStreaming }: Props) {
  const [svg, setSvg] = useState<string | null>(null);
  const [error, setError] = useState<MermaidFailure | null>(null);

  useEffect(() => {
    if (isStreaming) return;

    let cancelled = false;
    // Reset stale state from a previous code/theme so an old diagram or
    // error never flashes next to new content.
    setSvg(null);
    setError(null);

    const id = `lsp-mermaid-${renderSeq++}`;
    import("mermaid")
      .catch((err: unknown) => {
        // #2013: tag ANY import() rejection as the load class BEFORE it
        // merges with render failures downstream — a chunk that cannot be
        // fetched says nothing about the diagram's syntax.
        throw new MermaidChunkLoadError(err instanceof Error ? err.message : String(err));
      })
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
        // Malformed syntax → graceful inline error state, never a crash.
        // mermaid also leaves a diagnostic element (#d<id>) in the body
        // on parse failure — remove it so it can't leak into the page.
        // This runs BEFORE the cancelled check: an unmount mid-failed-parse
        // must still clean up the bombsite (r1). Harmless no-op for the
        // load class (no render was attempted, so no bombsite exists).
        document.getElementById(`d${id}`)?.remove();
        if (cancelled) return;
        const message = err instanceof Error ? err.message : String(err);
        setError(
          err instanceof MermaidChunkLoadError || isChunkFetchFailure(err)
            ? { kind: "chunk-load", message }
            : { kind: "syntax", message },
        );
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
    // #2013: the load class never claims anything about the diagram's
    // syntax — distinct testid, distinct copy, and a reload affordance
    // (the only remedy: the tab must re-fetch the new asset manifest).
    if (error.kind === "chunk-load") {
      return (
        <div
          data-testid="mermaid-load-error"
          className="not-prose my-4 rounded-md border border-amber-500/30 bg-amber-500/5 px-3 py-2"
        >
          <p className="text-xs font-medium text-amber-600 dark:text-amber-400">
            The app updated in the background — reload the tab to load the new version
          </p>
          <p className="mt-1 text-xs text-muted-foreground">
            The diagram below could not be drawn because the app's diagram module
            failed to load. This is not a problem with the diagram itself — your
            text is unchanged. If reloading does not fix it, check your
            connection and try again.
          </p>
          <details className="mt-1">
            <summary className="cursor-pointer text-xs text-muted-foreground">
              load error
            </summary>
            <pre className="overflow-x-auto touch-manipulation mt-1 text-xs font-mono text-muted-foreground whitespace-pre-wrap">
              {error.message}
            </pre>
          </details>
          <Button
            variant="outline"
            size="sm"
            className="mt-2"
            onClick={() => window.location.reload()}
          >
            Reload tab
          </Button>
          <pre className="overflow-x-auto touch-manipulation mt-2 text-xs font-mono text-muted-foreground whitespace-pre-wrap">
            {code}
          </pre>
        </div>
      );
    }

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
            {error.message}
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
