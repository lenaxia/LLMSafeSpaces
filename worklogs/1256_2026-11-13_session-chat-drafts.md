# Worklog: Per-Session Chat-Box Drafts — Typing Survives Session Switches

**Date:** 2026-11-13
**Session:** tsk-sober-oxbow-indicates (owner ask 2026-11-13; epic "Frontend reliability mopup")
**Status:** Complete — review round 1 remediated (crash-shape fix, recall-override, comment correction), pushed for re-review

---

## Objective

Typing into a session's chat box, clicking away to another session, and returning lost the drafted text. Persist drafts per session so switching sessions and returning restores what was typed; sending a message consumes the draft; cleanup so nothing accumulates unboundedly. Frontend scope only — NO backend persistence for drafts.

---

## Root cause

The Composer held its text in a local `useState`. Switching sessions changes the `["messages", ws, sid]` query key → `historyLoading` flips true → ChatPage's spinner branch (`ChatPage.tsx:1376`) unmounts ChatView and the Composer with it → the state dies. The Composer remounts empty when the next session's history lands.

## Work completed

### 1. The store — `frontend/src/lib/sessionDrafts.ts` (NEW)

- localStorage key `llmsafespaces_session_drafts` (the `llmsafespaces_*` client-persistence convention: useUserSettings, lsp-theme), shape `{wsId: {sessionId: {text, updatedAt}}}` — keyed per workspace AND session because the store is shared across workspaces.
- NO in-memory cache: reads hit localStorage each access so a second tab's writes stay visible (the store is tiny; the per-keystroke read is negligible).
- `saveSessionDraft(ws, sid, "")` REMOVES the entry — a sent message consumes the draft, and empty drafts never accumulate.
- Storage hygiene: 64 KiB per-draft cap (oversize stays in Composer memory, never persisted — one giant paste cannot crowd out the store or wedge it against the quota); `setItem`/`JSON.parse` wrapped (quota/privacy/corruption failures swallow — losing draft persistence is cosmetic, breaking typing is not).
- **Cleanup BOTH shapes** (the owner named either; both were cheap):
  - lifecycle (primary): `clearSessionDraft` / `clearWorkspaceDrafts`, wired in `SessionActivityProvider` — `session.status=deleted` and `=archived` clear that session's draft (archived is read-only at the API — a draft there can never be sent; unarchive does NOT resurrect); `workspace.phase` Terminating/Terminated clears the workspace subtree. Suspending/Suspended/Failed deliberately KEEP drafts: those workspaces resume, and deletion is not suspension.
  - TTL (~10 days, backstop): `sweepExpiredSessionDrafts` drops `updatedAt`-expired entries and prunes empty workspace buckets. Lazy — first store access per tab, then at most hourly on ordinary save/load traffic. No timers. Covers drafts whose lifecycle events were never observed (archived from a tab with a dead stream, etc.).

### 2. The hook — `frontend/src/hooks/useSessionDraft.ts` (NEW)

Drop-in replacement for the Composer's `useState("")`. Lazy-initializes from the store (the remount path — the common one), write-through persists every edit, "" consumes.

**The owner-guard seam**: on a session switch WITHOUT remount (defensive — covers a future keepPreviousData composer; in the current page the spinner usually remounts anyway), both effects' deps change in one commit and fire in DECLARATION ORDER: write-through first — its owner-ref still names the outgoing session, so it skips; hydrate second — re-keys the owner and loads the incoming draft. Wrong order (hydrate first) would save the outgoing session's text under the incoming key for one commit. The order is documented at the hook and pinned by the never-cross-saves tests.

No session (`!workspaceId || !sessionId`) → plain useState, nothing persisted.

### 3. Wiring

- `Composer.tsx`: `useState("")` → `useSessionDraft(workspaceId, sessionId)` (one line + comment; both props already threaded by ChatPage/ChatView).
- `SessionActivityProvider.tsx`: the three lifecycle insertions (deleted / archived / Terminating+Terminated).

### 4. Pins (43 new tests, all green; the three behavior pins the charter named)

- `lib/sessionDrafts.test.ts` (12): roundtrip + per-workspace keying, reload survival, empty-save removal, clearSession/clearWorkspace, TTL expiry + bucket pruning + the throttled lazy sweep, oversize skip, corrupted-JSON degradation, quota never throws.
- `hooks/useSessionDraft.test.tsx` (6): mount hydration, write-through/consume, switch-without-remount never cross-saves (session and workspace variants), unkeyed no-persist.
- `components/chat/Composer.drafts.test.tsx` (5, real Composer): remount restores; send consumes (box AND store); per-session + per-workspace isolation; unmount persists.
- `pages/ChatPage.drafts.test.tsx` (3, REAL ChatPage→ChatView→Composer over real route navigation): **PIN (a)** type→switch→switch-back→restored; **PIN (b)** send→consumed, store checked empty, switch-back empty; independent drafts both directions.
- `providers/SessionActivityProvider.drafts.test.tsx` (7, real provider + captured event callback): **PIN (c)** deleted/archived clear the session draft; unarchive no-op; Terminated/Terminating clear the workspace subtree; Suspended KEEPS drafts; busy/idle never touches them.

### 4b. Test-harness interactions (the honest cost of a persistence feature)

Three existing suites mount the real Composer for one fixed session across tests without clearing localStorage (nothing persisted before, so it never mattered): `ChatPage.composer-history` (3 renders of ws-1/sess-1), `Composer.slash`, `Composer.atRecall` — a previous test's setText now hydrates into the next test's composer. Fixed by `localStorage.removeItem("llmsafespaces_session_drafts")` in each suite's beforeEach (1 line + comment each). The restore-across-mounts those suites tripped over IS the feature.

Debugging note for the record: the drafts integration test initially failed NOT because of the feature but because of a pre-existing harness trap — ChatPage renders a TRANSIENT enabled composer before the workspace-status query flips `isReady` (the messages query is disabled, `historyLoading` false), then replaces it when history loads; `waitFor(not.toBeDisabled)` resolves against the transient instance and `userEvent.type` lands on a detached node. The house pattern (composer-history's own comment) is to wait for history CONTENT first — the drafts test now gives each session a distinct history marker and waits for it.

### 4c. Review round 1 (CHANGES_REQUESTED → remediated)

The reviewer mutation-verified the architecture (reverting the Composer wiring fails 6/8 drafts tests) and confirmed the lifecycle wiring correct, with three findings — all remediated:

1. **BUG (reviewer-executed, not inferred)**: `saveSessionDraft(…, "")` / `clearSessionDraft` threw `TypeError` on a valid-JSON WRONG-SHAPE store (`{"ws-1": 5}` → `"sess-1" in 5`) — contradicting the module's own corruption contract, and the throw would escape through the SSE `onEvent` handler. Fix: `sanitizeStore` at the read boundary — every non-object bucket/entry (and array top-levels, and bad entry shapes) is dropped before any downstream `in`/`Object.entries`; the first save rewrites a clean store. Pinned: `sessionDrafts.test.ts` "valid-JSON WRONG-SHAPE stores never crash" (clear + save + sweep on wrong shapes; the save repairs the bucket).
2. **History-recall destroyed the draft**: Up-recall's `setText(entry)` write-through-persisted the RECALLED entry as the session draft while the pre-browse snapshot (`savedDraft`) was memory-only — browse history → switch → return would restore history text as "the draft". Fix: the hook gained `persistOverride` — while browsing (`historyCursor !== -1`) the Composer persists the pre-browse snapshot (`savedDraft ?? ""`) instead of the textarea's view; mid-browse edits to a recalled entry stay transient (the Composer's own F5 semantics: Up always reloads the original). Pinned: `ChatPage.drafts.test.tsx` "Up-recall then switch does NOT replace the draft" — mutation-verified born-RED against the pre-fix wiring (the recalled entry replaces the draft).
3. **Misattributed comment (reviewer mutation-verified)**: the hook doc claimed effect DECLARATION ORDER was the cross-save seam; the reviewer's mutations showed the OWNER GUARD is load-bearing (removing it fails 4 tests incl. PIN (a); swapping order passes all). Comment rewritten: the guard is the seam (without it, the transient cross-key write is loaded back by the hydrate as the incoming session's "draft" — box AND store bleed); order is secondary hygiene that keeps the transient write from happening at all, and may be simplified away — the guard may not.
4. Minor wording (addressed): "second tab's writes stay visible" qualified to reads (concurrent writes are last-writer-wins read-modify-write); TTL semantics documented honestly — `updatedAt` is last-WRITE and mounting re-saves, so the backstop reads as "~10 days since last opened or edited" (its job is bounded accumulation; a returning user keeps their draft).

Reviewer non-blocking notes recorded: PIN (b) also passes on pre-PR code trivially (it pins consume-semantics, not wiring); govulncheck red is pre-existing stdlib advisories in untouched Go files (zero Go files in this diff).

## Verification

- `vitest run` full frontend suite: **2640/2640 green (233 files)** post-r1 (+45 new; was 2638 at the first push, 2595 pre-change).
- `tsc --noEmit` clean; `eslint src/` 0 errors (warnings all pre-existing in generated `src/abi/*_pb.ts` — none in touched files); `make fmt-check` green; `make repolint` green.
- Isolated worktree `/tmp/opencode/drafts-wt` (branch `feat/session-drafts` off `origin/main` @ d5a638fae); `node_modules` symlinked from the shared checkout.

## Key decisions

- **localStorage over sessionStorage/memory**: survives reloads (a draft survives an accidental refresh — strictly more than the charter asked), matches the existing persistence convention, per-workspace keying keeps other workspaces' drafts isolated.
- **Write-through on every edit (no debounce)**: unmount needs no flush (the last effect already ran), and the store is small enough that per-keystroke writes are negligible. A debounce would add a flush-on-unmount seam and a loss window.
- **Both cleanup shapes**: the events are observable in `SessionActivityProvider` (the same seams every other tab converges through), and the TTL sweep is ~15 lines — the owner's "implement BOTH cheaply" recommendation applied cleanly.
- **Archived clears, unarchive does not resurrect**: an archived session's composer is replaced by the read-only banner; the draft can never be sent. Resurrecting a stale pre-archive draft on unarchive would be surprising; a fresh composer is honest.
- **agent_died does NOT clear drafts**: the agent restarting doesn't delete sessions; drafts surviving a restart is correct.

## Blockers

None. PR iterating with AI reviewer — never self-merge.
