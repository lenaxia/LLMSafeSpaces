# Worklog: chat entity auto-linking (tsk-/epc-/prj- ids + session phrases; injected-message headers show session title + link)

Task `tsk-vibrant-cadet-pearls` (epic: Recovered lanes: production
defects + lost filings). Owner direction 2026-11-18: entity ids in
chat text should render as links to the matching task/epic/session
view, and injected agent-message headers ("message from session
quaint-glacier-rushes") should show the session as a link carrying
its ACTUAL TITLE.

## What landed (all frontend; no backend changes)

- **`frontend/src/lib/entityLinks.ts`** — the shared, conservative
  linkify core:
  - Tokenizer walks text OUTSIDE guarded regions (fenced blocks,
    inline code spans, markdown link spans, autolinks, bare URLs) —
    nothing in code or a URL is ever linkified.
  - Vocabulary: `tsk-*` / `epc-*` / `prj-*` short ids, raw `ses_*`
    harness ids, and EXACT 3-word kebab session phrases, all with
    boundary guards (no fragments of longer tokens/domains).
  - Conservative resolution — pattern-matching alone never links.
    Tier (a): the loaded index (react-query caches mirrored into a
    module-level registry by `EntityIndexSync`). Tier (b): a debounced
    (250ms), batched (≤12 probes/flush, prefixed ids first) lazy
    resolver over EXISTING endpoints (`boardApi.getTask/getEpic/
    getProject`, `workspacesApi.getSession` with a single-workspace
    scope). No new backend endpoint was needed. Misses cached with a
    30s TTL (a session created later can still link); results cached
    for the tab.
  - `linkifyMarkdown` is pure, idempotent (its own output
    re-transforms to itself), and quote-escapes minted link titles.
- **`frontend/src/components/shared/EntityIndexSync.tsx`** — mounted
  once in `App` inside `QueryClientProvider`; subscribes to the query
  cache (coalesced 150ms) and mirrors `["sessions", wsId]`,
  `["board","projects"|"tasks"|"epics",…]` into the registry.
  Idempotent publishes (version bumps only on real change).
- **`frontend/src/hooks/useEntityLinks.ts`** — `useEntityLinkify`
  (useSyncExternalStore over the registry; stable per version — the
  rerender-stability posture) and `useSessionEntityRef` (one-shot
  lazy probe per candidate per TTL).
- **`MessagePart.tsx`** — the ONE shared markdown pipeline
  (`MarkdownBody`) runs the transform on the markdown source before
  ReactMarkdown: no double-render, links minted as
  `[raw](href "Board story <id> — <title>")`. `MarkdownLink` now
  routes in-app hrefs (`/board…`, `/chat/…`) through the shared
  `EntityLink` (react-router `Link`, SPA navigation) and promotes the
  title to `aria-label` (type + title for screen readers; real
  anchors are keyboard-navigable). Streaming renders skip lazy
  probing (tier-a still applies) so half-streamed ids never poison
  the miss cache.
- **`AgentOriginBadge.tsx`** — the injected-message header (which is
  also how subscription-notification injections render — they arrive
  as sentinel-carrying agent messages, the only session-naming
  surface in the app) now shows `message from session <LINK>` with
  the session's ACTUAL TITLE as display text when resolved, falling
  back to the raw phrase/id when unknown (never blank). Hover title
  keeps the raw id; aria-label carries `Session <id> — <title>`.
- **`frontend/src/lib/boardDeepLink.ts` + `BoardPage.tsx`** — the
  minimal story/epic deep-link: `/board?story=<short>` opens the
  story drawer, `?epic=<short>` the epic drawer, `?project=<short>`
  focuses the lane. Consumed once the read model resolves it (or
  proves it cannot — stale ids drop silently), then the param is
  cleared so refresh does not re-open drawers. Semantics are a pure,
  unit-pinned helper.

## Deliberate cuts (noted for review)

- Thinking/reasoning parts keep their plain markdown render (charter
  scopes message text; thinking is collapsed auxiliary content).
- Board task rows' `assignee_session` phrases are NOT harvested as
  session tier-a entities (no title, workspace needs a join through
  the projects cache) — the session index covers chat pages.
- Raw `ses_*` probes happen only when exactly one workspace's session
  index is loaded (the chat-page shape); with several, they resolve
  via alias/index only.

## Pins (born-red, all in this branch)

- `lib/entityLinks.test.ts` — vocabulary, guarded regions, fragments
  rejected, resolution gating, idempotency, quote escaping, registry
  idempotent versioning, ses_* alias lookup, href/aria shapes, lazy
  resolver caching (probe-once, miss-TTL).
- `lib/boardDeepLink.test.ts` — story/epic/project actions, waiting
  while loading, stale-id drop.
- `components/chat/MessagePart.entityLinks.test.tsx` — exactly one
  link to the right target with type+title label; session link to the
  chat route; unresolvable kebab stays plain text; code spans never
  linkified; rerender-stability (three renders → one link); lazy
  resolution lands async then stops probing; streaming resolves
  tier-a without probes.
- `components/chat/AgentOriginBadge.entityLinks.test.tsx` — linked
  title when known; phrase fallback when unknown; ses_* via alias;
  raw-id fallback.

## Verification

- `npm run test` (vitest): full frontend suite green — 2812 passed
  including the 31 new pins; the only initial failure (house
  rules-of-hooks static analysis on `return useHook(...)` shapes) was
  fixed by assigning-then-returning.
- `npm run typecheck`: clean. `npm run lint`: 0 errors (7
  pre-existing warnings, among them abi-file disable directives and a
  pre-existing BoardPage exhaustive-deps warning).
- `make fmt-check`: unchanged Go surface (frontend-only branch) —
  verified clean.
