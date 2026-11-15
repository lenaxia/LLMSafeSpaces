/**
 * Per-session composer drafts — client-side persistence so switching
 * sessions (and returning) never loses typed text (owner ask
 * 2026-11-13). The Composer's local state dies with its remount (the
 * history-loading spinner branch unmounts ChatView on every session
 * switch); this store outlives it in localStorage, following the
 * existing client-persistence convention (`llmsafespaces_*` keys —
 * useUserSettings, lsp-theme).
 *
 * Storage shape (keyed per workspace because the store is shared
 * across workspaces):
 *   llmsafespaces_session_drafts = {
 *     "<workspaceId>": { "<sessionId>": { text, updatedAt } }
 *   }
 *
 * Cleanup is BOTH shapes the owner named:
 *   - lifecycle (primary): SessionActivityProvider deletes a
 *     session's draft on the session.status deleted/archived events
 *     and the whole workspace subtree on workspace.phase
 *     Terminating/Terminated (deletion — suspend keeps drafts, a
 *     suspended workspace resumes with its drafts intact).
 *   - TTL (~10 days, backstop): every draft carries `updatedAt`; the
 *     lazy sweep drops expired entries so nothing accumulates
 *     unboundedly even when lifecycle events are never observed
 *     (archived from a tab with a dead stream, etc.). The sweep runs
 *     on the first store access per page load and at most once per
 *     hour after that — no timers. Semantics: TTL is measured from
 *     the draft's LAST WRITE — and mounting a session re-saves its
 *     draft (the hook's write-through), so in practice a draft lives
 *     ~10 days since the session was last opened or edited. The
 *     backstop's job is bounded accumulation, not edit-time
 *     archaeology; a returning user keeps their draft.
 *
 * Storage errors (quota, privacy mode, corrupted or WRONG-SHAPE JSON)
 * are swallowed: losing draft persistence is cosmetic; breaking
 * typing is not.
 */

const STORAGE_KEY = "llmsafespaces_session_drafts";

/** Drafts older than this are swept (the ~10-day backstop). */
export const SESSION_DRAFT_TTL_MS = 10 * 24 * 60 * 60 * 1000;

/** The lazy sweep re-runs at most this often (per tab, on save/load). */
const SWEEP_MIN_INTERVAL_MS = 60 * 60 * 1000;

/** 0 = never swept; the first store access per tab sweeps. */
let lastSweepAt = 0;

/**
 * Per-draft size cap: an enormous paste must not crowd out every
 * other draft (or wedge the store against the localStorage quota).
 * Oversized text stays in Composer memory for the tab's lifetime but
 * is not persisted. 128 KiB of UTF-16 is far beyond any real chat
 * draft (the backend's own composer attachments ride files instead).
 */
const MAX_DRAFT_CHARS = 64 * 1024;

interface DraftEntry {
  text: string;
  updatedAt: number;
}

/** workspaceId → (sessionId → entry). Empty workspace buckets are pruned. */
type DraftStore = Record<string, Record<string, DraftEntry>>;

/**
 * Read the store on every access (no in-memory cache): a second tab's
 * reads stay coherent with this one (concurrent WRITES are
 * last-writer-wins — full-store read-modify-write), and the store is
 * small — the per-keystroke read is negligible.
 *
 * The parsed value is SANITIZED shape-first: anything that is not a
 * workspace→(session→entry) object tree (valid JSON of the wrong
 * shape — `{"ws-1": 5}`, an entry missing its fields, an array at the
 * top level) is dropped at the boundary, so every downstream `in`/
 * `Object.entries` operates on well-shaped data. Malformed JSON is
 * caught here too: the corruption contract ("storage errors are
 * swallowed") covers both.
 */
function isDraftEntry(v: unknown): v is DraftEntry {
  return (
    !!v && typeof v === "object" && typeof (v as DraftEntry).text === "string" &&
    typeof (v as DraftEntry).updatedAt === "number"
  );
}

// Prototype-polluting key names never become store keys: `out[k] = v`
// with k === "__proto__" would assign the prototype, not an own
// property (the bucket silently vanishing on the next write). Keys are
// server-generated workspace/session ids, so these are unreachable in
// practice — dropped defensively (review r2).
const UNSAFE_KEYS = new Set(["__proto__", "constructor", "prototype"]);

function sanitizeStore(parsed: unknown): DraftStore {
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return {};
  const out: DraftStore = {};
  for (const [wsId, sessions] of Object.entries(parsed as Record<string, unknown>)) {
    if (UNSAFE_KEYS.has(wsId)) continue;
    if (!sessions || typeof sessions !== "object" || Array.isArray(sessions)) continue;
    const bucket: Record<string, DraftEntry> = {};
    for (const [sessionId, entry] of Object.entries(sessions as Record<string, unknown>)) {
      if (UNSAFE_KEYS.has(sessionId)) continue;
      if (isDraftEntry(entry)) bucket[sessionId] = entry;
    }
    if (Object.keys(bucket).length > 0) out[wsId] = bucket;
  }
  return out;
}

function readStore(): DraftStore {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    return raw ? sanitizeStore(JSON.parse(raw)) : {};
  } catch {
    return {};
  }
}

function writeStore(store: DraftStore): void {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(store));
  } catch {
    // Quota exceeded / privacy mode: move on — losing draft
    // persistence is cosmetic; breaking typing is not.
  }
}

/** Test isolation: drop the sweep throttle stamp. */
export function _resetSessionDraftStoreForTests(): void {
  lastSweepAt = 0;
}

/**
 * Drop drafts older than the TTL (and prune empty workspace buckets).
 * Returns the number of entries removed. Exported for the expiry pin.
 */
export function sweepExpiredSessionDrafts(
  now: number = Date.now(),
  ttlMs: number = SESSION_DRAFT_TTL_MS,
): number {
  const store = readStore();
  let removed = 0;
  const next: DraftStore = {};
  for (const [wsId, sessions] of Object.entries(store)) {
    const kept: Record<string, DraftEntry> = {};
    for (const [sessionId, entry] of Object.entries(sessions)) {
      if (entry && typeof entry.updatedAt === "number" && now - entry.updatedAt <= ttlMs) {
        kept[sessionId] = entry;
      } else {
        removed++;
      }
    }
    if (Object.keys(kept).length > 0) next[wsId] = kept;
  }
  if (removed > 0) writeStore(next);
  return removed;
}

/** Lazy, throttled sweep — rides ordinary load/save traffic. */
function maybeSweep(): void {
  const now = Date.now();
  if (lastSweepAt !== 0 && now - lastSweepAt < SWEEP_MIN_INTERVAL_MS) return;
  lastSweepAt = now;
  sweepExpiredSessionDrafts(now);
}

/** The draft for one session, or "" when none is stored. */
export function loadSessionDraft(workspaceId: string, sessionId: string): string {
  maybeSweep();
  const entry = readStore()[workspaceId]?.[sessionId];
  return typeof entry?.text === "string" ? entry.text : "";
}

/**
 * Persist (or consume) a draft. An empty text REMOVES the entry — a
 * sent message clears the draft, and empty drafts never accumulate.
 * Oversized text is skipped (never persisted, never throws).
 */
export function saveSessionDraft(workspaceId: string, sessionId: string, text: string): void {
  maybeSweep();
  const store = readStore();
  const sessions = store[workspaceId];
  if (text === "") {
    if (!sessions || !(sessionId in sessions)) return;
    const nextSessions = { ...sessions };
    delete nextSessions[sessionId];
    const next: DraftStore = { ...store };
    if (Object.keys(nextSessions).length > 0) next[workspaceId] = nextSessions;
    else delete next[workspaceId];
    writeStore(next);
    return;
  }
  if (text.length > MAX_DRAFT_CHARS) return;
  writeStore({
    ...store,
    [workspaceId]: { ...(sessions ?? {}), [sessionId]: { text, updatedAt: Date.now() } },
  });
}

/** Lifecycle: the session is archived or deleted — its draft goes with it. */
export function clearSessionDraft(workspaceId: string, sessionId: string): void {
  saveSessionDraft(workspaceId, sessionId, "");
}

/**
 * Lifecycle: the workspace is being deleted (phase Terminating/
 * Terminated) — every draft under it goes. Idempotent; unknown
 * workspaces are a no-op.
 */
export function clearWorkspaceDrafts(workspaceId: string): void {
  const store = readStore();
  if (!(workspaceId in store)) return;
  const next: DraftStore = { ...store };
  delete next[workspaceId];
  writeStore(next);
}
