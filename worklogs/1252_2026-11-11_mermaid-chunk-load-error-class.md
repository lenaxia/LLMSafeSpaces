# Worklog: Mermaid Lazy-Chunk Load Failure — Reload-Class Error, Not a Syntax Verdict

**Date:** 2026-11-11
**Session:** tsk-petite-manger-speeds (issue #2013, the 2026-11-10 v0.44.12 stale-tab incident)
**Status:** Complete — pushed for AI review

---

## Objective

Classify `MermaidDiagram` failure classes: a failed dynamic `import("mermaid")` (stale-tab chunk 404 after a deploy) must render a distinct, actionable reload-class error — never the "invalid mermaid syntax" box that sent the incident's agent into two pointless rewrite rounds. Genuine mermaid parse errors keep the syntax box. Frontend scope only; no change to mermaid rendering itself.

---

## The incident (issue #2013, 2026-11-10 ~07:50 UTC)

User session `dual-pillow-seizes` on a stale open tab hit the lazy-import failure minutes after the v0.44.12 deploy. The component rendered:

```
Mermaid diagram failed to render (invalid mermaid syntax)
parse error
Failed to fetch dynamically imported module: https://chat.safespaces.dev/assets/mermaid.core-CGvHfpkw.js
```

The diagram was VALID — the hashed chunk 404s because the stale tab holds the old index.html asset manifest. The agent read the UI's (false) syntax verdict, spent two rewrite rounds "fixing" correct mermaid, then pivoted the user to PlantUML.

## Work Completed

### 1. The fix — classification at the import boundary (`MermaidDiagram.tsx`)

- Failures are classified by WHERE they occur, never by string-matching browser fetch phrasing (Chrome "Failed to fetch dynamically imported module", Firefox "error loading…", Safari "Importing a module script failed" — boundary classification is phrasing-immune by construction).
- `import("mermaid")` carries a `.catch` that rethrows a module-private sentinel (`MermaidChunkLoadError`) with the original message — the tag lands BEFORE the rejection can merge with render failures downstream.
- Component state goes `string | null` → `{ kind: "chunk-load" | "syntax"; message } | null`; the outer catch classifies by `instanceof`.
- Load class renders `data-testid="mermaid-load-error"` (amber, distinct from the red syntax box):
  - headline "The app updated in the background — reload the tab to load the new version";
  - an explicit "This is not a problem with the diagram itself" line — the anti-confusion contract for any downstream agent or user reading the box;
  - the raw failure in a `<details>` labeled **load error** (never "parse error");
  - a "Reload tab" button (`window.location.reload()`, the `ErrorBoundary` precedent — the only remedy is re-fetching the new asset manifest);
  - the diagram source preserved inline (valid content, not garbage).
- Syntax class: the existing box byte-equivalent (testid `mermaid-error`, the claim, the parse-error details, the source).
- Untouched by design: the `#d<id>` bombsite cleanup and its runs-BEFORE-cancelled-guard semantics (no-op for the load class — no render was attempted); streaming placeholder; `securityLevel: "strict"` pins; theme plumbing.

### 2. Pins (TDD: new pins verified born-RED on the pre-fix component — 3/3 fail, green post-fix)

- **NEW `MermaidDiagram.chunkLoad.test.tsx`** (SessionActivityProvider house style: per-behavior file, vi.hoisted mocks, render through the production surface):
  - L1 — a rejecting dynamic import (the incident's exact TypeError, verbatim chunk URL) renders `mermaid-load-error` with the reload copy; the box (and screen) NEVER matches /invalid mermaid syntax/; `mermaid-error` and `mermaid-diagram` absent; initialize/render proven never called.
  - L2 — the diagnostic is reframed: summary "load error", never "parse error"; the source survives inline.
  - L3 — the reload affordance: button present, click drives a `window.location.reload` spy.
  - Mock posture note: vitest wraps a rejecting `vi.mock` factory rejection; the original TypeError rides as `cause`. The component classifies at the import boundary, so ANY rejection shape exercises the same path a real browser 404 takes — the pin is on the boundary, not on vitest's wrapper text.
- **`MessagePart.markdownMermaid.test.tsx` pin 4 strengthened** (the mirror, in its natural home where the mermaid mock RESOLVES and `render` rejects): the syntax box still appears with the "invalid mermaid syntax" claim, `mermaid-load-error` absent, and `mockInitialize` reached — proving that failure was genuinely downstream of the import.

### 3. Mutation evidence

- Pre-fix component (`git stash` of the fix, pins in place): L1/L2/L3 all RED — the old code rendered the syntax box for the fetch failure.
- Post-fix: 20/20 green across both mermaid suites.

## Verification

- `vitest run` full frontend suite: **2595/2595 green (226 files)**.
- `tsc --noEmit` clean; `eslint .` 0 errors (7 warnings, all pre-existing in generated `src/abi/*_pb.ts` + `BoardPage.tsx` — none in touched files); `make fmt-check` clean.
- Isolated worktree `/tmp/opencode/mermaid-chunkload-wt` (branch `fix/mermaid-chunk-load-error` off `origin/main` @ f324452dc); `node_modules` symlinked from the shared checkout.

## Key Decisions

- **Boundary classification over message string-matching**: the import() rejection point IS the classification — deterministic across browsers, no phrasing table to maintain, and a rendering-stack error can never masquerade as a load failure or vice versa.
- **Sentinel class over a flag**: `instanceof` in the outer catch reads as one expression; the sentinel stays module-private (not exported — tests exercise it through the UI, not the class).
- **The source stays visible in the load box**: the incident's second-order failure was trust — the user/agent couldn't tell valid content from garbage. Keeping the (valid) source under a box that explicitly disclaims a syntax verdict restores the missing context.
- **No auto-reload**: a silent mid-session `location.reload()` would eat in-flight chat state; the affordance is explicit and user-driven (same posture as ErrorBoundary).

## Blockers

- None. Note: AI Commands lever status per #1844 — if `/review` dispatch is down, the verdict arrives via the synchronize path (PROCESS ruling).
