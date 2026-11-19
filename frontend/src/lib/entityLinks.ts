// entityLinks.ts — conservative entity auto-linking for chat text
// (owner direction 2026-11-18 / tsk-vibrant-cadet-pearls).
//
// When entity ids appear in message prose — board story short ids
// (tsk-yeasty-magnet-scores), epics (epc-*), projects (prj-*), or
// session references (3-word phrases like merry-finch-loses, raw
// ses_* harness ids) — they render as links to that entity's view.
//
// CONSERVATIVE RESOLUTION is the core posture: pattern-matching alone
// never creates a link. A candidate links only if it RESOLVES, via
// two tiers:
//   (a) the loaded index — entities already in react-query caches
//       (the workspace's session list; board projects/tasks/epics),
//       mirrored into this registry by <EntityIndexSync>. Zero-cost.
//   (b) a batched lazy resolver — unknown candidates are queued on a
//       debounced timer and probed through EXISTING read endpoints
//       (boardApi.getTask/getEpic/getProject, workspacesApi.
//       getSession). No new backend endpoint. Results — including
//       misses — are cached client-side; misses expire (TTL) so a
//       session created later can still link.
// Unresolvable candidates render as plain text: no dead links, no
// error states. A 3-word kebab that matches nothing stays text.
//
// The tokenizer walks text OUTSIDE guarded regions — fenced code
// blocks, inline code spans, markdown link spans, autolinks, and bare
// URLs — so nothing inside code or a URL is ever linkified.

import { boardApi } from "../api/board";
import { workspacesApi } from "../api/workspaces";
import { resolveSessionAlias } from "../session/sessionAliases";

export type EntityKind = "task" | "epic" | "project" | "session";

/** One resolvable entity. `key` is the lowercased lookup key (the
 * candidate text that resolves here); `id` is the canonical id used
 * in hrefs (board short id; the session PHRASE for sessions). */
export interface EntityRef {
  kind: EntityKind;
  key: string;
  id: string;
  title?: string;
  /** Sessions only: the workspace the session lives in (chat href). */
  workspaceId?: string;
}

export interface EntityCandidate {
  start: number;
  end: number;
  raw: string;
  kind: EntityKind;
}

// --- hrefs + labels --------------------------------------------------------

export function entityHref(ref: EntityRef): string {
  switch (ref.kind) {
    case "task":
      return `/board?story=${encodeURIComponent(ref.id)}`;
    case "epic":
      return `/board?epic=${encodeURIComponent(ref.id)}`;
    case "project":
      return `/board?project=${encodeURIComponent(ref.id)}`;
    case "session":
      // Without a workspace there is no chat route to link — the
      // caller renders plain text instead of a dead link.
      return ref.workspaceId
        ? `/chat/${encodeURIComponent(ref.workspaceId)}/${encodeURIComponent(ref.id)}`
        : "";
  }
}

/** Accessible label: entity type + canonical id + title. Screen
 * readers get the type and (when known) the human title, not raw-id
 * soup. */
export function entityAriaLabel(ref: EntityRef): string {
  const type =
    ref.kind === "task"
      ? "Board story"
      : ref.kind === "epic"
        ? "Board epic"
        : ref.kind === "project"
          ? "Board project"
          : "Session";
  return ref.title ? `${type} ${ref.id} — ${ref.title}` : `${type} ${ref.id}`;
}

// --- tokenizer -------------------------------------------------------------

const CANDIDATE_PATTERNS: Array<{ kind: EntityKind; re: RegExp }> = [
  // Board short ids: tsk-/epc-/prj- + one or more kebab words
  // (covers both the phrase form tsk-crimson-finch-blooms and the
  // legacy random form tsk-1a2b3c4d).
  { kind: "task", re: /tsk-[a-z0-9]+(?:-[a-z0-9]+)*/g },
  { kind: "epic", re: /epc-[a-z0-9]+(?:-[a-z0-9]+)*/g },
  { kind: "project", re: /prj-[a-z0-9]+(?:-[a-z0-9]+)*/g },
  // Raw harness session ids (mixed case after ses_).
  { kind: "session", re: /ses_[A-Za-z0-9]+/g },
  // Session phrases: EXACTLY three kebab-joined lowercase words.
  // Resolution gating is what keeps this broad pattern safe: an
  // unresolvable kebab ("on-the-fly") stays plain text.
  { kind: "session", re: /[a-z][a-z0-9]*-[a-z][a-z0-9]*-[a-z][a-z0-9]*/g },
];

// A candidate is invalid when directly preceded/followed by these —
// it would be a fragment of a longer token (inside a longer kebab, a
// word, a path, or a domain). Sentence punctuation (,.;:!?) stays a
// valid boundary on both sides; "." additionally blocks the LEFT side
// so a domain fragment (example.com-like text) never probes.
const PRE_BOUNDARY = /[A-Za-z0-9_./-]/;
const POST_BOUNDARY = /[A-Za-z0-9_/-]/;

/** Guarded (never-linkified) spans of `text`: fenced code blocks,
 * inline code spans, inline markdown links `[..](..)`, autolinks
 * `<http..>`, and bare URL runs. Conservative by construction — an
 * unclosed backtick run guards to end of line. */
function guardedRanges(text: string): Array<[number, number]> {
  const ranges: Array<[number, number]> = [];
  let pos = 0;
  let inFence = false;
  while (pos < text.length) {
    const nl = text.indexOf("\n", pos);
    const lineEnd = nl === -1 ? text.length : nl;
    const line = text.slice(pos, lineEnd);
    const fence = /^ {0,3}(`{3,}|~{3,})/.exec(line);
    if (inFence || fence) {
      ranges.push([pos, lineEnd]);
      if (fence) inFence = !inFence;
    } else {
      let i = 0;
      while (i < line.length) {
        const rest = line.slice(i);
        let m: RegExpExecArray | null;
        if ((m = /^`+/.exec(rest))) {
          const close = line.indexOf(m[0], i + m[0].length);
          const end = close === -1 ? line.length : close + m[0].length;
          ranges.push([pos + i, pos + end]);
          i = end;
          continue;
        }
        const c = rest[0];
        if (c === "<") {
          m = /^<https?:\/\/[^ >]*>/.exec(rest);
          if (m) {
            ranges.push([pos + i, pos + i + m[0].length]);
            i += m[0].length;
            continue;
          }
        }
        if (c === "[") {
          m = /^\[[^\][]*\]\([^()]*\)/.exec(rest);
          if (m) {
            ranges.push([pos + i, pos + i + m[0].length]);
            i += m[0].length;
            continue;
          }
          i++;
          continue;
        }
        if (c === "h") {
          m = /^https?:\/\/\S+/.exec(rest);
          if (m) {
            ranges.push([pos + i, pos + i + m[0].length]);
            i += m[0].length;
            continue;
          }
        }
        i++;
      }
    }
    pos = lineEnd + 1;
  }
  return ranges;
}

function overlaps(ranges: Array<[number, number]>, start: number, end: number): boolean {
  for (const [rs, re] of ranges) {
    if (rs < end && start < re) return true;
  }
  return false;
}

/** All entity-id candidates in `text`, outside guarded regions. */
export function findEntityCandidates(text: string): EntityCandidate[] {
  if (!text) return [];
  const ranges = guardedRanges(text);
  const found: EntityCandidate[] = [];
  for (const { kind, re } of CANDIDATE_PATTERNS) {
    re.lastIndex = 0;
    let m: RegExpExecArray | null;
    while ((m = re.exec(text))) {
      const start = m.index;
      const end = start + m[0].length;
      if (overlaps(ranges, start, end)) continue;
      const before = start > 0 ? text[start - 1] : "";
      const after = end < text.length ? text[end] : "";
      if (before && PRE_BOUNDARY.test(before)) continue;
      if (after && POST_BOUNDARY.test(after)) continue;
      found.push({ start, end, raw: m[0], kind });
    }
  }
  found.sort((a, b) => a.start - b.start || b.end - a.end);
  const out: EntityCandidate[] = [];
  let lastEnd = -1;
  for (const c of found) {
    if (c.start >= lastEnd) {
      out.push(c);
      lastEnd = c.end;
    }
  }
  return out;
}

// --- transform -------------------------------------------------------------

export type ResolveEntity = (raw: string, kind: EntityKind) => EntityRef | undefined;

function escapeMdTitle(title: string): string {
  return title.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
}

/** Rewrites resolved candidates in markdown `text` as inline
 * `[raw](href "aria-label")` links. Unresolved candidates pass
 * through verbatim. Guarded regions are never touched, which also
 * makes the transform idempotent: its own output re-transforms to
 * itself (the minted spans sit inside guarded link syntax). */
export function linkifyMarkdown(
  text: string,
  resolve: ResolveEntity,
  onCandidate?: (candidate: EntityCandidate) => void,
): string {
  if (!text) return text;
  const candidates = findEntityCandidates(text);
  if (candidates.length === 0) return text;
  const parts: string[] = [];
  let pos = 0;
  for (const c of candidates) {
    parts.push(text.slice(pos, c.start));
    onCandidate?.(c);
    const ref = resolve(c.raw, c.kind);
    const href = ref ? entityHref(ref) : "";
    if (ref && href) {
      parts.push(`[${c.raw}](${href} "${escapeMdTitle(entityAriaLabel(ref))}")`);
    } else {
      parts.push(c.raw);
    }
    pos = c.end;
  }
  parts.push(text.slice(pos));
  return parts.join("");
}

// --- registry (tier a) -----------------------------------------------------

const registry = new Map<string, EntityRef>();
/** Workspaces with a loaded session index (for ses_* alias
 * resolution and the single-workspace session probe). */
let sessionWorkspaceScope = new Set<string>();
let registryVersion = 0;
type RegistryListener = () => void;
const registryListeners = new Set<RegistryListener>();

function candidateKey(raw: string, kind: EntityKind): string {
  return `${kind}:${raw.toLowerCase()}`;
}

function sameRef(a: EntityRef | undefined, b: EntityRef): boolean {
  return (
    !!a &&
    a.kind === b.kind &&
    a.id === b.id &&
    (a.title ?? "") === (b.title ?? "") &&
    (a.workspaceId ?? "") === (b.workspaceId ?? "")
  );
}

export function subscribeEntityRegistry(listener: RegistryListener): () => void {
  registryListeners.add(listener);
  return () => {
    registryListeners.delete(listener);
  };
}

export function getEntityRegistryVersion(): number {
  return registryVersion;
}

/** Merges entities into the registry. Idempotent: republishing
 * unchanged data bumps nothing (rerender-stability pin). Returns
 * true when anything changed. */
export function publishEntities(refs: Iterable<EntityRef>): boolean {
  let changed = false;
  for (const ref of refs) {
    if (!ref || typeof ref.id !== "string" || ref.id === "") continue;
    const key = ref.key || candidateKey(ref.id, ref.kind);
    const existing = registry.get(key);
    if (sameRef(existing, { ...ref, key })) continue;
    registry.set(key, { ...ref, key });
    changed = true;
  }
  if (changed) bumpRegistry();
  return changed;
}

/** Sets the workspaces whose session index is loaded. Returns true
 * when the scope changed (session alias resolution may now succeed).
 * Never bumps on its own — callers fold this into their sync pass. */
export function setSessionWorkspaceScope(wsIds: Iterable<string>): boolean {
  const next = new Set(wsIds);
  if (next.size === sessionWorkspaceScope.size) {
    let same = true;
    for (const id of next) {
      if (!sessionWorkspaceScope.has(id)) {
        same = false;
        break;
      }
    }
    if (same) return false;
  }
  sessionWorkspaceScope = next;
  return true;
}

function bumpRegistry(): void {
  registryVersion++;
  for (const listener of Array.from(registryListeners)) listener();
}

/** Tier-a lookup: direct key hit, plus ses_* harness ids resolved
 * through the learned alias tables of every workspace with a loaded
 * session index. */
export function lookupEntity(raw: string, kind: EntityKind): EntityRef | undefined {
  const direct = registry.get(candidateKey(raw, kind));
  if (direct) return direct;
  if (kind === "session" && raw.startsWith("ses_")) {
    for (const ws of sessionWorkspaceScope) {
      const phrase = resolveSessionAlias(ws, raw);
      if (phrase && phrase !== raw) {
        const ref = registry.get(candidateKey(phrase, "session"));
        if (ref) return ref;
      }
    }
  }
  return undefined;
}

// --- lazy resolver (tier b) ------------------------------------------------

const MISS_TTL_MS = 30_000;
const MAX_PROBES_PER_FLUSH = 12;
let resolveDelayMs = 250;

interface QueuedCandidate {
  raw: string;
  kind: EntityKind;
}
const queued = new Map<string, QueuedCandidate>();
const pendingKeys = new Set<string>();
const missKeys = new Map<string, number>();
let flushTimer: ReturnType<typeof setTimeout> | null = null;

function probeRank(kind: EntityKind): number {
  // Unambiguous prefixed ids probe first; free-form phrases are the
  // most false-positive-prone, so they go last under the batch cap.
  return kind === "session" ? 1 : 0;
}

function markMiss(key: string): void {
  missKeys.set(key, Date.now() + MISS_TTL_MS);
}

/** Queues unknown candidates for a debounced, batched probe through
 * existing read endpoints. Deduplicates against the registry,
 * in-flight probes, and unexpired misses. */
export function requestEntityResolution(candidates: Array<{ raw: string; kind: EntityKind }>): void {
  const now = Date.now();
  for (const c of candidates) {
    if (!c || typeof c.raw !== "string" || c.raw === "") continue;
    const key = candidateKey(c.raw, c.kind);
    if (registry.has(key) || pendingKeys.has(key)) continue;
    const missUntil = missKeys.get(key);
    if (missUntil !== undefined && missUntil > now) continue;
    queued.set(key, { raw: c.raw, kind: c.kind });
  }
  if (queued.size === 0) return;
  if (flushTimer === null) {
    flushTimer = setTimeout(() => {
      flushTimer = null;
      flushResolutionQueue();
    }, resolveDelayMs);
  }
}

function flushResolutionQueue(): void {
  const items = Array.from(queued.values());
  queued.clear();
  items.sort((a, b) => probeRank(a.kind) - probeRank(b.kind));
  const batch = items.slice(0, MAX_PROBES_PER_FLUSH);
  const deferred = items.slice(MAX_PROBES_PER_FLUSH);
  if (deferred.length > 0) {
    for (const d of deferred) queued.set(candidateKey(d.raw, d.kind), d);
    flushTimer = setTimeout(() => {
      flushTimer = null;
      flushResolutionQueue();
    }, resolveDelayMs);
  }
  for (const item of batch) {
    const key = candidateKey(item.raw, item.kind);
    pendingKeys.add(key);
    probeCandidate(item.raw, item.kind)
      .then((ref) => {
        if (ref) publishEntities([ref]);
        else markMiss(key);
      })
      .catch(() => markMiss(key))
      .finally(() => {
        pendingKeys.delete(key);
      });
  }
}

/** The single workspace whose session index is loaded, when exactly
 * one is — the common chat-page shape. Session probes need a
 * workspace scope; with zero or several loaded the probe degrades to
 * a (cheap, cached) miss. */
function singleSessionWorkspace(): string | undefined {
  return sessionWorkspaceScope.size === 1 ? Array.from(sessionWorkspaceScope)[0] : undefined;
}

async function probeCandidate(raw: string, kind: EntityKind): Promise<EntityRef | null> {
  if (kind === "task") {
    const t = await boardApi.getTask(raw);
    if (!t || typeof t.short_id !== "string") return null;
    return { kind, key: candidateKey(t.short_id, kind), id: t.short_id, title: t.title };
  }
  if (kind === "epic") {
    const e = await boardApi.getEpic(raw);
    if (!e || typeof e.short_id !== "string") return null;
    return { kind, key: candidateKey(e.short_id, kind), id: e.short_id, title: e.title };
  }
  if (kind === "project") {
    const p = await boardApi.getProject(raw);
    if (!p || typeof p.short_id !== "string") return null;
    return { kind, key: candidateKey(p.short_id, kind), id: p.short_id, title: p.name };
  }
  // session: only with a single-workspace scope (see above).
  const ws = singleSessionWorkspace();
  if (!ws) return null;
  const direct = lookupEntity(raw, "session");
  if (direct) return direct;
  const s = await workspacesApi.getSession(ws, raw);
  if (!s || typeof s.id !== "string") return null;
  return {
    kind: "session",
    key: candidateKey(s.id, "session"),
    id: s.id,
    title: s.title,
    workspaceId: ws,
  };
}

// --- test hooks ------------------------------------------------------------

/** Clears all registry/lazy-resolver state (tests). Cancels any
 * pending flush. */
export function resetEntityLinkState(): void {
  if (flushTimer !== null) {
    clearTimeout(flushTimer);
    flushTimer = null;
  }
  registry.clear();
  queued.clear();
  pendingKeys.clear();
  missKeys.clear();
  sessionWorkspaceScope = new Set();
}

/** Overrides the lazy-resolver debounce (tests only). */
export function setEntityResolveDelayForTests(ms: number): void {
  resolveDelayMs = ms;
}
