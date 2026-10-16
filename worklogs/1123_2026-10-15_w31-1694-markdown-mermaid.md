# Worklog: #1694 — markdown for user messages + mermaid/UML diagram rendering

**Date:** 2026-10-15
**Session:** w31 — frontend lane for issue #1694 (user-message markdown + mermaid fences in MessagePart.tsx).
**Status:** Complete (review loop r1 findings addressed)

---

## Objective

Close #1694: route user text parts through the same markdown pipeline as assistant text (preserving the Epic 68 D11 attachment strip, the #1465 agent-origin sentinel, and the raw-text copy button), and render ` ```mermaid ` fences as diagrams — lazy-loaded/code-split, `securityLevel: 'strict'`, `startOnLoad: false`, theme-following, graceful on malformed syntax and streaming partials.

## Assumptions and validation

1. **User messages never render mid-stream** (so no `closeOpenFence` on the user path). Validated: `isStreaming` originates from SSE live agent parts; history user messages arrive complete; `MessageBubble` passes the flag but nothing streams user role text.
2. **rehypeSanitize is observable on the user path.** Validated by probe: react-markdown drops raw-HTML nodes and strips `javascript:` hrefs INTRINSICALLY — the plugin's observable effect in a no-rehype-raw pipeline is the default schema's `clobberPrefix` on markdown-generated ids (GFM footnote refs). Pinned via the double-`user-content-` prefix; probed both with and without the plugin (review r1 finding 2 — the originally suggested task-list `<input>` discriminator was probed and REJECTED: the GitHub schema allows task-list inputs, 1 rendered with the plugin present).
3. **mermaid 12 leaves a `#d<id>` diagnostic div in `<body>` on parse failure.** Validated by the reviewer against `mermaid@12.2.0` source (`parseEncounteredException` throw precedes `removeTempElements()`); component removes it in `.catch`, and r1 moved the removal BEFORE the cancelled guard (unmount-mid-failed-parse still cleans up — pinned in jsdom, exercised for real in the r1 e2e).
4. **Mermaid must not enter the main bundle.** Validated by `npm run build`: `mermaid-*.js` / `mermaid.core-*.js` / `mermaid-parser.core-*.js` lazy chunks; zero `classDiagram`/`flowchart-v2` markers in the index chunk (only the dynamic-import reference).

## Work done

- `MessagePart.tsx`: extracted shared `MarkdownBody` (ReactMarkdown + remarkGfm + rehypeSanitize + custom links/pre/code); user path now parseAgentMessage → parseAttachments → MarkdownBody with prose color vars scoped to `currentColor` (inverted `bg-primary` bubble keeps its surface); `pre` handler routes `language-mermaid` to `MermaidDiagram`.
- `MermaidDiagram.tsx` (new): dynamic `import("mermaid")` only; `initialize({ startOnLoad: false, securityLevel: "strict", theme })` per render with unique ids; states: `mermaid-pending` (streaming — render-on-closed-fence, no parse until stream end), `mermaid-error` (malformed — inline error + source + parse message), `mermaid-diagram` (svg container), `mermaid-loading`.
- `vite.config.ts`: r1 — dedicated `mermaid-chunks` CacheFirst runtime cache (own 20-entry budget) so diagram usage can't evict shiki grammar chunks; transitive lazy deps (elk/cytoscape) stay on the content-hashed generic bucket.
- Tests: `MessagePart.markdownMermaid.test.tsx` (13 red-first pins + 4 r1 pins), e2e `markdown-mermaid.spec.ts` (happy: user markdown in inverted bubble, real offline mermaid svg; r1 unhappy: malformed fence → error state + no bombsite, dangerous user HTML → no execution/no literal markup).
- Deliberate decision (r1): soft line breaks in user bubbles now collapse per CommonMark — same semantics as the assistant path, mandated by "SAME pipeline". `remark-breaks` considered and rejected for parity; pinned as a documenting test.

## Red-first record

Pin commit verified against `origin/main` before implementation: 12 red / 1 green (the green one is pin 2b, the green-then-stays-green forged-interior-manifest guard). Reviewer independently reproduced the red run.

## Gates

Full vitest (192 files / 2123+ tests), `tsc --noEmit`, `npm run lint` (0 errors; 6 pre-existing abi warnings on main), Playwright new spec green, `npm run build` split check green — re-run after each review round.

## Review loop

- r1 CHANGES_REQUESTED (github-actions[bot]): e2e unhappy paths missing, sanitize pin non-discriminating, soft-break change unacknowledged, bombsite cleanup skipped when cancelled, worklog missing, mermaid/shiki cache budget shared. All addressed in r1 commit; PR body updated with the soft-break acknowledgment.
