# Frontend complexity audit — `frontend/src`

**Date:** 2026-11-18
**Scope:** read-only complexity assessment; no code changes.
**Baseline:** `origin/main` @ `bd56d8464` (worktree `audit/frontend-complexity`).
**Method:** full reads of the four hotspot files (`ChatPage.tsx` 1,467 ln, `BoardPage.tsx` 2,629 ln, `SessionActivityProvider.tsx` 2,346 ln, `App.tsx` + shell wiring); pairwise diff + `git log --all --follow` archaeology for the settings/org-admin tier surfaces; hook-census greps; spec-file inventory. Every finding is anchored `file:line` at the baseline SHA and, where history is recoverable, traced to the commit/issue that introduced or fixed it.

---

## 1. Executive summary

| Hotspot | Lines (src) | Spec files | Spec lines | Test:src |
|---|---|---|---|---|
| `pages/ChatPage.tsx` | 1,467 | 20 | 6,362 | 4.3:1 |
| `pages/BoardPage.tsx` | 2,629 | 16 | 4,388 | 1.7:1 |
| `providers/SessionActivityProvider.tsx` | 2,346 | 9 | 6,457 | 2.8:1 |
| Settings/org-admin credential trio | 1,853 (3 files) | — | — | ~50-60% duplicated |

Three structural patterns generate the complexity:

1. **Pages as orchestrators AND machine owners.** ChatPage and BoardPage each inline five-plus independent state machines (optimistic reconcile, stuck-session auto-abort, rail focus/lens, kebab overrides, drag optimism) behind raw `useState`/`useRef` counts of 25+ and 49 respectively. The bug record shows the same failure class repeatedly: *hand-maintained reset lists and cross-machine gating conditions that drift* (§3).
2. **A provider doing data-layer orchestration.** `SessionActivityProvider` owns a busy-lease/flight/tombstone distributed-ordering scheme across ~11 `useState` maps and ~17 `useRef` maps, driven by a single ~1,050-line SSE event handler closure, reaching directly into the React Query cache (`queryCache.getAll()`, provider:512-560). Its context value is a fresh object per provider render, so every busy delta for any session in any workspace re-renders every consumer — ChatPage consumes 12 of its ~20 selector hooks (§4).
3. **Tier copy-paste instead of scoped components.** The user/org/platform config tiers re-implement the same list/create/delete/probe machinery three times (~1,700 lines) with shipped divergence bugs, while `McpServersTab` in the same tree proves the one-component-three-scopes alternative works (§6).

The v0.15 refresh bug (`dc5632044`, 2026-11-17) is examined as a shape lesson in §5: the frontend was exonerated, but the investigation was expensive precisely because "workspace freshness" has no owner — it is smeared across a mutation hook, the query cache, and two SSE hooks, with three separate transport-liveness watchdogs and zero domain-staleness signal.

---

## 2. Findings matrix

Origin-traced (introducing commit/issue where recoverable), costed, with simplification and a functional-equivalence argument. IDs are stable for follow-up stories.

| ID | Finding | Origin trace | Cost | Simplification | Functional equivalence |
|---|---|---|---|---|---|
| **FC-1** | ChatPage inlines ~10 state machines that belong in hooks/providers (inventory §3) | Accreted from `e58cecd9b` (US-69.7 ledger era) through `b934ae7f3` (contract-stream cutover), `fba5dba10` (#1687), `c6126ed6c` (#1760) | 1,467 ln; 25 hooks (10 `useState` + 15 `useRef`); 12 `useEffect` + 1 `useLayoutEffect`; 20 spec files (6,362 ln) — each machine family pinned by its own spec; a *meta-spec* (`hookcount.test.tsx`) statically scans the source because hook order itself is a hazard | Extract per-machine hooks (§3.2): `useOptimisticTranscript`, `useStuckSessionAutoAbort`, `useSessionResetKey`, banner-zoo reducer | Each extraction is a pure move: same state, same effects, same callbacks — specs render ChatPage, not the hooks, so all 20 families keep passing unchanged; the `hookcount` static scan constraint disappears with the early-return moved out of the machine-owning scope |
| **FC-2** | ChatPage's per-session reset is hand-rolled twice, in two parallel effects with different dep keys | Split introduced with the #1760 restart banner; fixed twice: `a2b497f57` (r1, stale `agentRestarted` reset), `f113cb97e` (r3, workspace-only-switch leak) | 2 effects (ChatPage:194-206, 534-555) resetting 15 pieces of state/refs by hand; **2 review rounds of bug fixes on one effect** | Single reducer (or `key=`-remount) keyed on `(workspaceId, sessionId)`; reset = key change | Identical reset semantics by construction — a keyed reducer re-initializes on key change instead of enumerating members; the r1/r3 bug class (missed member, wrong dep) is structurally unreachable |
| **FC-3** | `reconcileOnIdle`'s wipe gate conditions host at least three shipped bugs | #447 (optimistic wipe in the eventual-consistency window), #1687 pin 3 `fba5dba10` (mid-turn wipe destroyed streamed text), `72518b730` (r1: landed-user refetch needed page-1 bound + serialized anchor) | ~70 ln callback (ChatPage:569-634) with correctness dependent on fold-vs-event-vs-refetch ordering; 3 spec families (`optimistic-survival`, `midturnReconnect`, `sse-reconnect-duplicate`) exist solely to pin this gate | Extract `useOptimisticTranscript` with the identity-key, reconcile, and landed-user logic; gate conditions become the hook's documented contract | Same `messageIdentityKey` (byte-for-byte), same refetch-then-filter sequence, same mid-turn guard — the extraction moves code, not semantics; the three pinning spec families move with it or keep rendering the page |
| **FC-4** | BoardPage inlines 9 machines; StageRail/StageCell + 4 dialogs ≈ 950 ln unextracted; StageCell contract is 22 props drilled 3 levels | Growth: `88ff596a6` (#1702, +833 base) → `989975956` (+279 kebab) → `3c5591dfd` (+153 rail) → `c1ff21b04` (+337 focus mode); last-5-days fixes: `59476e682`/`6e3e87f98` (orientation round-trip), `cee5205a4` (r1: arrow-key hijack, snap drift), `8e2f65255` (r2: stale `cardOverrides` masked server rows) | 2,629 ln; 49 `useState` + 6 `useRef` + 11 `useEffect` in page+rail; 16 spec files (4,388 ln); zero `React.memo` — every SSE event re-renders the whole page (see §5.3) | Extract StageRail/StageCell to `components/board/` (props-preserving); move `cardOverrides` + `optimistic` into one `useReducer` machine; complete `boardRules.ts` (lifecycle allowed-map, search predicate) | Pure component move: same JSX, same props (the 22-prop contract is ugly but preserved); rail/focus specs mount the whole page today and keep passing; `useReducer` keeps the same override semantics with the r2 staleness fix (clear-on-authoritative-row) as its documented transition |
| **FC-5** | Settings/org/platform credential trio: ~50-60% of 1,853 ln duplicated with shipped divergence | `02adf0362` (A+U born), `a8f43239c` (#199 org tier, different impl), `61558f9a0` (#432 — the one commit that correctly touched all three) | 3 files × ~550-780 ln; divergence: org-tier delete has **no confirm dialog** (`64507dc42` migrated destructive confirms everywhere else, trio skipped), `parseInt` vs `Number` validation drift on the same field, org tier lacks the key-reveal toggle, user tier cannot update keys at all | One `CredentialList`/`CredentialForm` pair with a `scope` prop — the `McpServersTab`/`WorkspaceImagesTab` pattern already in-repo (`McpServersTab.tsx:25-54`, routed 3×, `router.tsx:58-66`) | Tier differences are endpoint + 4 genuinely-tier-specific blocks (§6.1); scope-injected API clients render identical markup for the shared 60%; per-tier spec files keep rendering each routed instance |
| **FC-6** | `SessionActivityProvider`: one ~1,050-ln event-handler closure owns busy-lease flights, EventID ordering, alias re-keying, tombstones, unread, hung, prompts | #1821 pieces 2-4 (`6ba0c5c0e` era), #1848 r0-r4 (`fb97056d0`), #1646 r3-r5 (wipe/birth millisecond semantics — three rounds on *ordering of two statements*) | 2,346 ln; 9 spec files (6,457 ln); every state change → new context object → all consumers re-render (ChatPage: 12 selector hooks; Sidebar; workers header) | Split into a non-React busy-store (`useSyncExternalStore` + per-slice selectors) and a prompt-content store; the event handler becomes module-scope functions writing the store | Selectors return the same values for the same inputs; consumers swap `useIsSessionBusy(x)` for the same-signature store selector — the 9 spec families assert behavior, not context shape, and keep passing |
| **FC-7** | OrgSettingsTab name collision (two different components, same export name, different dirs) + paged-admin-list scaffold duplicated ~70% (`settings/OrgSettingsTab.tsx` ↔ `PlatformUsersTab.tsx`) | `edfdf665d` (#416) era | Discoverability hazard (`router.tsx:52-53` vs `77-78` must disambiguate); two slug algorithms coexist (`slugify` local, 48-char, vs shared `slugFromName`, 64-char — `988dbc5a0` hardened only the local one) | Rename platform-tier component to `OrgDirectoryTab`; extract `<PagedListTab>` (filters + pagination + suspend flow) | Rename is a re-export shim away from zero call-site change; PagedListTab parameterizes the 6 shared blocks (statusVariant, applyStatusFilter, suspend handlers, footer) already line-anchored pairwise |
| **FC-8** | Board rules leakage: page re-implements what `boardRules.ts` exists for | `88ff596a6` base; rules module grew with `b5d4878f8` | Epic-lifecycle allowed-map inline conditionals (BoardPage:826-857) while the analogous drag transitions got a tested Set in boardRules:122-146; search predicate written twice (BoardPage:382-384, 407-412); `toBoardError` duplicated in StoryDrawer:214-218 | Move the allowed-map and predicate to `boardRules.ts` with unit tests | Same truth tables, now unit-testable without mounting the page; the kebab/swimlane specs that pin the behavior via the page are unchanged |
| **FC-9** | Staleness/liveness is a smeared cross-cutting concern: three watchdogs (SSE read-timeout 35s, liveness-silence 60s, busy lease 90s), zero domain-staleness signal | #1365 (liveness watchdog), #1821 piece 4 (lease), read-timeout since `e58cecd9b` | The v0.15 refresh exoneration (`dc5632044`) required cross-repo archaeology because "workspace freshness" spans 4 frontend files with no owner (§5) | A freshness/provenance seam (e.g. `dataUpdatedAt`-derived phase-age, or mutation-response diffing) owned by one hook | Out of scope for refactor-only (needs product decision); recorded because the next .15-class incident pays the same search cost until it exists |

---

## 3. ChatPage deep dive — the state-machine inventory

### 3.1 The machines (all inline in one component)

| # | Machine | State/refs (anchors) | Evidence it has hidden bugs |
|---|---|---|---|
| M1 | **Optimistic transcript reconcile** | `localMessages` (:186), `sessionErrors` (:191), `messageIdentityKey` (:64-77), `reconcileOnIdle` (:569-600), `reconcileLandedUserMessage` (:611-634) | #447: unconditional wipe destroyed the just-sent bubble during the eventual-consistency window → guard added (:586-591). #1687 pin 3: reconnect-time wipe destroyed in-flight streamed text → mid-turn fold gate added (:558-568). `72518b730` r1: the landed-user refetch grew a page-1 bound + serialization after review. Three bugs, one 70-line gate. |
| M2 | **Live stream render buffer + echo gate** | `sseStreamParts` (:835), `upsertStreamPart` (:840-857, the I12 entity-ID stitch), `appendStreamDelta` (:859-872), `knownUserMessageIds` echo gate (:495, :950, :958) | The echo gate's exclusion-by-message-ID (vs naive text matching) and the first-seen `toolStartedAt` anchor (:851) are both review-round hardenings documented in-line — the buffer's correctness is a stack of such patches. |
| M3 | **Stuck-session auto-abort** | 5-input evidence machine: stuck-tool-in-history scan (:773-784), `pendingPromptCount`, fold BUSY + empty pendingInputs (:792-793), snapshot-after-mount (:794), dwell anchor `abortDwellStartRef` (:512, :799-818) | The dwell-anchor semantics ("a persistent anchor, not a fresh timer", :507-512) and the send-resets-anchor rule (:1080-1083, "review round 5 recommendation") exist because earlier shapes deferred or carried over the abort incorrectly. `lastHistoryRef` (:513-519) guards against refetch-churn resetting evidence. |
| M4 | **Context-used / compaction detection** | `contextBySessionRef` + `contextVersion` (:422-433), `prevContextUsedRef` (:438), `compactionDetected` (:439), `useLayoutEffect` (:442-451) | Reset logic for this machine is written **twice** (:204-206 and :544-546) — the S36.4 and session-switch effects each hand-reset it (see M7). |
| M5 | **Pending-prompts fold sync** | ~80-line effect (:677-755): parent-chain resolution, `resolvedInputIdsRef` latch (:676), whileAway exemption (#1313), absence-evidence drops (#1365 r1), staleness timer (:476) | #1365 needed two lands (`985d3bd81` then `bc3a78b1c`) and a dedicated *timer* hook because the effect's deps "do not tick on a healthy-but-quiet stream" (provider:56-60) — the machine is spread across ChatPage AND the provider. |
| M6 | **Queue-vs-direct send** | `handleSend` (:1119-1145) with documented FIFO race (:1122-1137), `doSendNow` (:1061-1103) with model-resolution IIFE + anchor reset | `dbb3e8393` (queued messages landing out of order) + its r1 (`72518b730`) — the parked-error exemption (:1128-1131) is a patch on a patch. |
| M7 | **Session lifecycle / reset** | Two parallel reset effects (:194-206 keyed `[sessionId, workspaceId]`; :534-555 keyed `[workspaceId, sessionId]` — same deps, different order) hand-reset 15 state/ref members | **#1760 r1** (`a2b497f57`): stale `agentRestarted` not reset. **r3** (`f113cb97e`): the workspace-only-switch leak — the /chat/:workspaceId route carries no sessionId, so one effect's key never fired. Two shipped rounds on the single most mechanical machine in the file. |
| M8 | **Banner zoo** | 9 dismissible one-bit states: `hungAlert` (:441, #998/D6), `agentDied`+`agentDiedMessage` (:523-524), `agentRestarted` (:527, #1760), `sessionWasInterrupted` (:522), `compactionDetected`, `streamTimedOut`, `chatError`, `atCapRetryAfter`, `historyIsError` (#490) | The #1760 leak (r1/r3) lived here: a workspace-A toast shadowing workspace B because one of nine parallel one-bit states missed its reset. |
| M9 | **Auto-rename** | `hasAutoRenamedRef` + effect (:821-834) with auto-name regexes | Quiet; cited for completeness. |
| M10 | **Auto-create / deletion navigation** | auto-session-create (:359-363, double-invoke guard documented), deletion navigation (:248-251, #786) | The #786 comment (:243-246) records the provider/route split-of-authority decision this depends on. |

**Spec-file count as the complexity metric:** 20 spec files (6,362 lines) pin ChatPage — each machine family grew its own (`queue`, `midturnReconnect`, `sse-reconnect-duplicate`, `optimistic-survival`, `contractIdleBridge`, `agentRestart`, …). Additionally `hookcount.test.tsx` exists *only* because the file is too big for React's hook-order rules to be visibly safe: it statically scans the source for hooks after the early return (the documented React #310 incident, ChatPage:1058-1060). **When a component needs a source-scanning meta-test to guard its own shape, the shape is the finding.**

### 3.2 Extraction candidates (ranked within ChatPage)

1. **`useSessionResetKey` (M7+M8)** — collapse the two reset effects + 9 banner states into one reducer keyed `(workspaceId, sessionId)`. *Functional equivalence:* a keyed reducer re-initializes on key change — exactly the union of both current effects' behavior; the r1/r3 bug class (missed member) becomes unreachable because there is no member list to forget. All `agentRestart`/`queue`/`navigate` specs render ChatPage and keep passing.
2. **`useOptimisticTranscript` (M1)** — move `messageIdentityKey`, both reconcilers, `localMessages`/`sessionErrors`. *Equivalence:* byte-identical key function and refetch-then-filter sequence; the mid-turn gate moves with it. Pins: `optimistic-survival`, `drafts`, `sse*` families.
3. **`useStuckSessionAutoAbort` (M3)** — the evidence machine + dwell. *Equivalence:* same five inputs (passed as arguments), same dwell arithmetic; `queue.test`/`contractIdleBridge` keep rendering the page. This machine is the best-first extraction candidate *within* ChatPage because it is self-contained read-only over its inputs.
4. **M5 (fold sync) belongs in the provider** — it is provider-store business driven from the page; moving it behind a `usePromptFoldSync(workspaceId, sessionId)` hook (still page-mounted) or into the provider's fold publication keeps behavior identical while deleting the page's knowledge of latch/tombstone internals.

---

## 4. Provider-tree deep dive

### 4.1 Data-flow layers (as built)

```
useUserEventStream (AppShell-mounted, one connection)
  ├─ invalidation hub: workspace.phase/resync/board.event → invalidate 5 cache families (useUserEventStream.ts:92-112)
  └─ onEvent → SessionActivityProvider's ~1,050-ln handler (provider:732-1836)
       ├─ busy-lease machine: BusyEntry{confirmedAt, confirmEventId, internalId} + BUSY_LEASE_MS=90s
       ├─ #1821 busy-snapshot flights: begin/snapshot/complete staging (provider:1127-1420)
       ├─ #1821 EventID-ordered deltas (provider:1421-1835)
       ├─ alias re-keying (#1824: rekeyWorkspaceValueMap, provider:233-289)
       └─ prompts/unread/hung/aborted/deleted maps
React Query cache (also read by the provider: seedBusy walks queryCache.getAll(), provider:490-560)
  └─ useWorkspaceStatus / useMessageHistory / sessions lists → pages
Selector hooks (~20, provider:2191-2346) ← single context object, fresh identity per provider render
```

### 4.2 Re-render cost

The context value is an object literal (provider:2181-2184) — a **fresh identity on every provider render**, and the provider renders on every write to any of its ~11 state maps. Every consumer of every selector hook therefore re-renders on every such write: ChatPage (12 selector hooks, ChatPage:44), Sidebar, the workers-tree header (`useAnySessionBusy`). A `session.status` delta for *any* session in *any* workspace → `setBusySessions` → full re-render of the heaviest page in the app. During an active turn this fires per status event; BoardPage's board cache fan-out compounds it when both surfaces are live. The busy-snapshot flight machinery can write multiple times per flight (stage rows, then commit), multiplying the cadence.

### 4.3 What the machine costs in bugs

The provider's comment archaeology is the cost ledger: #1646 required **r3, r4, and r5** fixes about *which milliseconds* data births and wipes happen in (`dataBirthRef`, `wipeAtRef`, provider:517-547 — "a millisecond tick between the two statements" r5); #1848 required r0-r4 (degraded-row join semantics, per-session watermarks, `internalId` protection). These are the inherent complexity of an eventually-consistent mirror — but it is implemented as mutable refs + scattered `setState` in one closure rather than a reducer with explicit transitions, which is why each ordering rule had to be discovered in review.

### 4.4 The .15 refresh bug — what it says about the shape

`dc5632044` (2026-11-17): the refresh-compute UX died at the backend (CRD-skew starved the phase machine); **the frontend was exonerated — the invalidation map was byte-identical to .14**. The incident still indicts the shape:

1. **All refresh paths share one upstream truth and carry no provenance.** The refresh button's mutation → `invalidateQueries(["workspace-status"])` → refetch, and the SSE `workspace.phase` → same invalidation → refetch, and the poll belt all converge on the same API read. When the upstream is stale-but-200, N redundant paths look like strong liveness. The UI treated "refetch returned identical data" as success — there is no layer that owns "how old is this phase?"
2. **Liveness is watchdog-per-transport, not per-domain.** Read-timeout (35s, `useUserEventStream.ts:11`), liveness-silence (60s, :17 — #1365), busy lease (90s, provider:52) each solved one instance of "my truth is stale" for one slice. A healthy connection that never carries a phase event is indistinguishable from a quiet-but-correct one — by design of those watchdogs.
3. **The exoneration itself was expensive.** Proving the frontend innocent required cross-repo archaeology (invalidation-map diff against .14, provider/SSE suite runs) because "workspace freshness" spans four files (mutation hook → queryClient → SSE hook → status hook) with no single owner. That search cost is recurring: it is paid again on every .15-class incident until a freshness seam exists (FC-9).

**Shape conclusion:** the invalidation-hub pattern is correct and worth keeping; what is missing is a *domain-staleness* edge (phase-age watermark, or mutation-response-vs-cache diff) owned by one hook. This audit records it as a finding, not a refactor — it needs a product decision and backend cooperation.

---

## 5. Board surface deep dive (summary of the inventory)

### 5.1 The nine machines and their hosting bugs

BoardPage owns (hook census: 49 `useState` / 6+ `useRef` / 11 `useEffect` across page+rail): rail/swimlane layout (:128-153, :1060-1297, geometry :2038-2119), kebab menus (overrides :868-899, builders :826-1016), 9 drawer/dialog surfaces (:553-569, 4 dialogs inline raw Radix :1641-1814, :1932-1969), focus machinery (StageRail :2151-2426: roving tabindex, scrollend transit :2228-2253, pinch exit, md matchMedia :2270-2291, aria-live mode notice), drag-and-drop (:425-510), SSE (:762-766), filters (:341-422), scope selection (:168-208, :548-632), CRUD (:513-686).

Bug clusters and their host machines (all post-date the rail/focus accretion `3c5591dfd`/`c1ff21b04`):
- **Orientation round-trip** (`59476e682`, `6e3e87f98`): the `mdPlus` matchMedia effect — event-dependent force-exit left stale column sizes.
- **r1 accessibility trio** (`cee5205a4`): arrow keys hijacked from an open kebab (menu-origin guard :2305/:2318), 8px snap drift, title overflow.
- **Stale optimistic overrides** (`8e2f65255` r2): `cardOverrides` permanently masked authoritative rows — the kebab machine's optimism had no retirement rule.
- **Archived-expansion/scope staleness** (`fe8320e81`/`1f1f826e6`, #1964): the scope machine's prune effects.

### 5.2 The spec tax of the inline shape

Because StageRail/StageCell are *inline*, `BoardPage.focus.test.tsx` must drive the whole mounted page and shim jsdom geometry per test (`mockRailGeometry`, `fireScrollEnd`, 446 ln); `rail.test.tsx` re-declares width class tables; three spec files hand-roll the cross-context invalidation idiom. Extracting the rail would let those families mount a 500-line component instead of a 2,629-line page.

### 5.3 Re-render cost

Every `board.event` invalidates `["board"]` (useUserEventStream.ts:111) → tasks + projects + every epic-lane query refetch → new array identities → full page render; the page's own SSE buffer write (:762-766) renders again *before* refetches resolve; the 30s poll belt repeats the cycle with no user present. Zero `React.memo` anywhere in the board tree; `storyKebabItems(t)` mints a fresh array per card per render (:2603). Mitigations that exist: rail-local lens state, KebabMenu-internal open state, unmount-when-closed drawers.

---

## 6. Settings/org/admin tier duplication (summary)

### 6.1 Credential trio (worst offender)

`AdminProviderCredentialsTab.tsx` (779, platform) / `UserProviderCredentialsTab.tsx` (533, user) / `org-admin/OrgCredentialsTab.tsx` (541, org): pairwise common lines A↔U ≈ 67%, and ~50-60% of the family re-implements list/create/delete/probe/models machinery. Shipped divergence (each a real bug or missing hardening): org-tier delete fires with **no confirmation** (the `64507dc42` confirm-dialog migration skipped the trio entirely); numeric-limit parsing uses `parseInt` in A/U but `Number.isFinite` in O (same field, two validation semantics — `02adf0362` vs `a8f43239c`); org tier lacks the key-reveal eye toggle; user tier has *no* update path (no `update` method on `userProviderCredentialsApi` — key rotation = delete+recreate). The API layer mirrors the split: three client sets, one shared type module, zero shared UI hooks; `listBindings`/`bindToWorkspace` are dead code.

### 6.2 The rest

- `settings/OrgSettingsTab.tsx` (platform org directory) vs `org-admin/OrgSettingsTab.tsx` (org policy editor): **not copies, same name** — a discoverability hazard (FC-7). The real duplication partner is `PlatformUsersTab.tsx` (~70%: paged list scaffold).
- Agent-config pair (`PlatformAgentConfigTab` 231 / `OrgAgentConfigTab` 347): ~82% of the platform file reproduced in the org file (roles table 44/47 identical lines).
- Audit tabs: `PlatformAuditTab`/`OrgAuditTab` ~55% of the org file (row rendering strict subset).
- **The in-repo exemplar:** `McpServersTab.tsx` (418 ln, `scope: "admin"|"org"|"user"` prop, three API clients injected, routed 3×) and `WorkspaceImagesTab` (same pattern) prove single-component-multi-tier works here — `OrgCredentialsTab.tsx:20-22` even documents it as the canonical pattern.

---

## 7. Top 5 simplification candidates (value/risk)

Value = bug-class elimination + test-cost reduction + re-render savings. Risk = chance of behavioral drift during the move.

| # | Candidate | Value | Risk | Why this rank |
|---|---|---|---|---|
| **1** | **Extract BoardPage's StageRail + StageCell + 4 inline dialogs to `components/board/`** (~950 ln moved; FC-4) | High: the freshest bug cluster (orientation round-trip, arrow-key hijack, snap drift — 3 fix commits in 5 days) lives exactly in the code that would move; spec families shrink from page-mounts to component-mounts | Low: pure move, props contract preserved verbatim (including its ugliness — no renaming in the same change); rail/focus/declutter specs keep passing because they drive the same DOM | Biggest behavioral-surface-per-risk: zero semantic change, immediate test-cost win, contains the actively-burning machine |
| **2** | **Split SessionActivityProvider: busy-store behind `useSyncExternalStore` + prompt-content store; event handler → module-scope reducers (FC-6)** | High: kills the every-consumer re-render per busy delta; makes the lease/flight transitions explicit (the #1646 r3-r5 millisecond-ordering bugs became review-discoverable *because* transitions were implicit); 9 spec families (6,457 ln) keep asserting behavior | Medium: the store rewrite must preserve EventID-ordering semantics exactly; the 6,457 ln of provider specs are the safety net and stay green by construction (they assert outcomes, not internals) | Highest compounding value (perf + correctness surface), guarded by the strongest existing test wall |
| **3** | **Unify the credential trio on the `McpServersTab` scope pattern (FC-5)** | High: ~900-1,000 duplicated lines deleted; closes 4 shipped divergences (unconfirmed delete, validation drift, missing reveal, no-update-tier) — these are user-facing inconsistencies today | Medium: tier-specific blocks (rules UI, edit-mode re-encryption, anon probe) must stay as slot props; per-tier specs keep rendering routed instances | Proven pattern in-repo (#432 was the one 3-way-sync commit done right); divergence bugs argue urgency |
| **4** | **ChatPage: keyed session-state reducer first, then M1/M3 extractions (FC-1/2/3)** | High (long-term): eliminates the r1/r3 reset-leak class structurally; shrinks the file that needs a source-scanning meta-test for hook safety | Medium-high: the reconcile gate (M1) is the most subtle code in the frontend — move it *byte-identical* with its pins, no semantic edits in the same change; sequence reset-reducer → auto-abort → reconcile, one PR each | Ranked below 1-3 only because each step touches the highest-subtlety code; the 20-family spec wall (6,362 ln) enforces equivalence |
| **5** | **Complete `boardRules.ts` + board override machines to `useReducer` (FC-8 + kebab machine)** | Medium: rule truth tables become unit-testable without page mounts; the `cardOverrides` staleness class (r2 `8e2f65255`) gets a documented transition | Low: pure-function moves (allowed-map, predicates) + a reducer whose transitions mirror the existing handlers | Cheapest; ranked last only because the value, while real, is smaller than the structural wins above |

**Functional-equivalence argument (common to all five):** every candidate is a *move, not a redesign* — same state shapes, same event handling, same API calls, same DOM output. Equivalence is argued per candidate above and is enforced by the existing spec walls (20 ChatPage families / 16 BoardPage families / 9 provider families / per-tier settings specs), all of which render public surfaces (routed pages, provider-mounted trees), not internals. The one candidate that is more than a move (FC-9, freshness seam) is deliberately **excluded** from the top 5 and recorded as a finding needing product input.

---

## 8. Explicit non-goals

No code changes in this audit. Findings FC-1..FC-9 are inputs for follow-up stories; each carries the anchors and equivalence arguments those stories will need.
