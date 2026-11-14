# Worklog: Per-Session Chat-Box Drafts — Typing Survives Session Switches

**Date:** 2026-11-13
**Session:** tsk-sober-oxbow-indicates (owner ask 2026-11-13; epic "Frontend reliability mopup")
**Status:** Complete — review rounds 1-3 remediated (r3: the F4 restore scoped to its owning session — cross-session bleed closed and pinned; prototype pin re-seeded with raw JSON after the r2 pin proved vacuous; records corrected), pushed for re-review

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

### 4d. Review round 2 (CHANGES_REQUESTED → remediated)

r1's findings 1 and 3 ruled RESOLVED by the reviewer (sanitizer covers every consumer, born-RED pins verified, comment accurate). One BLOCKING residual on finding 2, plus two minors:

1. **BLOCKING — mid-browse history refetch persisted the recalled entry over the real draft (reviewer-executed twice on the exact head)**: the F4 reset effect fires on any `userMessageHistory` identity change — reachable in production because the composer is NOT disabled during a running stream (stream finalization refetches the messages query) and "Load earlier messages" pages in. The reset cleared `historyCursor`/`savedDraft` WITHOUT restoring `text`: the box still showed the recalled entry, the persistOverride flipped to undefined, and the write-through refired (persistOverride is in its deps) — persisting the RECALLED entry as the draft. Follow-on: a second Up then snapshotted the recalled text as `savedDraft`. My r1 fix upgraded the pre-existing cosmetic box quirk to silent persistent data loss. Fix: the F4 reset now restores the pre-browse draft into the box (`if (historyCursor !== -1) setText(savedDraft ?? "")`) — cursor, box, override, and store stay coherent on every path. Pinned by `Composer.drafts.test.tsx` "a history refetch MID-BROWSE restores the draft" — the reviewer's exact probe shape (rerender with a changed history list mid-browse + the follow-on second-Up path), mutation-verified born-RED without the restore.
2. **Minor — `__proto__` keys in sanitizeStore**: `out[k] = v` with a `__proto__` key assigns the prototype, not an own property. Unreachable with server-generated ids, but closed anyway: `UNSAFE_KEYS` (__proto__/constructor/prototype) dropped for both bucket and entry keys. Pinned by the prototype-pollution test.
3. **Minor — executeCommand mid-browse (documented decision)**: a slash command does NOT consume the persisted draft (the cursor is deliberately not reset, so the override keeps the snapshot as the stored draft); sending remains the only consumer. Comment added at executeCommand.

### 4e. Review round 3 (CHANGES_REQUESTED → remediated)

r2's blocking finding ruled RESOLVED (born-RED pin verified), but the reviewer proved my r2 fix itself introduced a NEW blocking regression, plus two accuracy defects:

1. **BLOCKING — the r2 restore fired unconditionally, bleeding the outgoing draft across a warm-cache session switch (reviewer-executed on the exact head, with a passing control on the pre-r2 behavior)**: a session switch is ALSO a `userMessageHistory` identity change. On the no-remount path, Composer effect order is write-through (guard skips) → hydrate (loads the INCOMING draft into `text`) → F4 reset LAST — still seeing `historyCursor !== -1` from the outgoing render, it restored the OUTGOING `savedDraft` over the incoming draft; the next write-through then persisted the outgoing draft under the INCOMING key (box and store both bled). Fix: the restore is scoped by `browseOwnerRef` — stamped when Up-recall enters browse mode, compared against the current session at reset; a mismatched owner resets the browse state WITHOUT restoring. The F4 effect also gained workspaceId/sessionId deps so a switch resets browse state even when the history identity coincidentally survives. Pinned by `Composer.drafts.test.tsx` "a warm-cache session switch MID-BROWSE never bleeds the outgoing draft" — the reviewer's exact probe (sess-2 seeded with its own draft; both box and store asserted), mutation-verified born-RED without the owner scoping.
2. **Vacuous prototype pin (my r2 test bug)**: `JSON.stringify({__proto__: …})` serializes to `{}` — the object-literal `__proto__` key is the Annex-B prototype SETTER, not an own property — so the seeded store never contained the key and the pin exercised nothing (emptying UNSAFE_KEYS left it green; the reviewer proved it). Re-seeded with a RAW JSON string (JSON.parse creates `__proto__` as an OWN data property — the shape a hostile store actually presents); now mutation-verified born-RED without the filter. The production filter was already correct; only the pin was hollow.
3. **Records corrected**: CHANGELOG/worklog had claimed the vacuous pin as coverage — both now describe the re-seeded pin; the executeCommand comment's "sending is the only consumer" overclaim fixed (mid-browse a slash command keeps the draft via the override; outside browse mode its `setText("")` consumes, like a send).

## Verification

- `vitest run` full frontend suite: **2643/2643 green (233 files)** post-r3 (+48 new; 2638 at the first push, 2595 pre-change).
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
