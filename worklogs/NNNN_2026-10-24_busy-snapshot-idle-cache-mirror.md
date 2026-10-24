# Worklog: #1883 — busy-snapshot commit must mirror its idle answer onto the sessions cache (un-latch completion)

**Date:** 2026-10-24
**Session:** Lane 1883 (orchestrator novel-nutmeg-verifies) — audit the landed busy-latch machinery against issue #1883's freeze scenarios, close the residual un-latch gap, red-first.
**Status:** Complete

---

## Objective

Issue #1883: sidebar busy indicators freeze — background-session busy latches never clear. The lane charter: reproduce per the issue, find what landed vs what #1883 still lacks (the #1821/#1848 busy-snapshot+lease machinery, #1899's StreamResynced flight, #1646/#1687 browser-leg wipes all landed ahead of this lane), fix the remaining gap red-first, minimal.

## Audit: what landed vs what #1883 still lacks

Every leg of the incident now has a heal on HEAD (all verified by reading the code and running the suites):

| Truth change | Heal | Landed |
| --- | --- | --- |
| idle delta lost, stream healthy | 90s busy lease → `POST /sessions/status-snapshot` → authoritative flight (T0.2e pins the exact issue scenario) | #1848 |
| broker drop (browser leg) | `resync` wipe + recheck + watermark reset | #1687, #1848 r1/r4 |
| browser disconnect | onReconnect wipe + birth-guarded re-seed + watermark reset (per-replica ids) | #1646, #1848 r1 f3 |
| pod→api usage-stream silent loss | `BridgeResync.StreamResynced` → `triggerBusySnapshotFlight` (plus #1666 per-session corrections) | #1899 |
| subscriber joins after flip | connect-path per-Active-workspace flights | #1848 |
| terminal (agent_died / non-Active phase) | full workspace busy clear + no-retry guards | #1848 r1/r3/r4 |
| force-stop / delete | `aborted`/`deleted` user-stream deltas (phrase-keyed) | #786, #1817 |

The issue's other two evidence links are already dispositioned: `/sessions` list rows have carried statusz-stamped `status` since #795 (the logged status-free bodies were `/internal/v1/session-registry`, excluded from request logging by #1899 ask 3), and the frontend `/sessions/active` surface was deleted by #1899 ask 2 (route+SDK deliberately stay — canary scenarios consume `Sessions.GetActive` for MaxActive).

## The residual gap this PR closes (root cause)

The provider mirrors busy transitions onto the react-query sessions cache (the busy delta stamps the row `status:"active"`, the idle delta stamps it back `"idle"`), and the #1848 snapshot commit mirrors the **busy direction only** (`busyRowIds` → `"active"`). When the lease's authoritative flight answers **idle** for a session whose idle delta was lost — the exact #1883 incident shape — the commit clears the provider's busy map (the dot clears) but leaves the cached row at the stale `"active"`.

That stale projection is a re-freeze vector: `seedBusy`'s **mount-time pass is add-only** (`SessionActivityProvider.tsx:673`), and its gate/birth guards pass trivially on a fresh provider instance (`wipeAt=0`, fresh refs). Any provider remount that keeps the QueryClient (route-level remount, error-boundary recovery) re-latches the cleared badge from the stale row **before** the invalidation refetch can correct it — and the add-only gate then ignores the fresh idle rows the refetch brings back. The frozen indicator *returns* after the authority already healed it: a cached projection silently diverging from harness truth, the same family as #1879/#1880.

## Fix (frontend-only, provider)

1. `BusyFlight` stages resolved idle rows (`idleRowIds`) — the authority's explicit per-session "not busy" answers. Degraded (harness-keyed, `session_id_deprecated`) rows stage nothing: their id join is partial and no phrase-keyed row can match them.
2. The ok=true commit mirrors `idleRowIds` onto the sessions cache (`status:"idle"`), the symmetric half of the existing busy mirror — guarded by the **same per-session watermark discipline** the commit's busy-row latch uses (`sessionLastDeltaIDRef ≤ cursor`): a row superseded by a newer same-session delta keeps the fresher answer (M2 pin).
3. No `hasUnread` invention: the flight is truth reconciliation, not turn-completion UX — the unread flip belongs to the idle delta the user's stream actually delivers.

Documented choice: **provider-side fix, no backend change** — the backend already sends everything needed (the flight's rows carry every session's answer); the defect was the client dropping the idle half of the answer on the floor at the mirror. The `api/internal/handlers` busy-snapshot code is LANDED stable base per the fleet agreement — untouched.

## Tests (red-first, `SessionActivityProvider.snapshotIdleMirror.test.tsx`)

- **M1 (RED on HEAD, fails at `expected 'active' to be 'idle'`)**: latch busy → row `"active"`; lose the idle frame; lease expiry fires the one recheck; the flight answers idle; badge clears; PIN the cached row flips `"idle"`; then REMOUNT the provider on the same QueryClient and PIN the badge stays clear (on HEAD the stale row re-latches it).
- **M2 (watermark guard)**: a newer same-session busy delta (id > cursor) racing mid-flight keeps both the badge and the row `"active"` — the idle mirror must not invert fresher evidence.
- **M3 (degraded no-flip invariant)**: a harness-keyed `session_id_deprecated` idle row flips no phrase-keyed row. Non-discriminating by construction — it pins the invariant, not the staging guard.
- **M4 (r1, out-of-order flights)**: a newer busy flight (cursor 50) then an older overlapping idle flight (cursor 21) completing in the same batch — badge stays yes, row stays `"active"` (pinned the reviewer's demonstrated race; RED until the dispatch-synchronous watermark pre-stamp).

RED proof: `git stash` of the provider fix → M1 fails at the row assertion (the remount assertion sits behind it); fix restored → green (4/4 with M4 after r1).

## Test commands

- `npx vitest run src/providers/SessionActivityProvider.snapshotIdleMirror.test.tsx` — 3/3 (RED pre-fix, GREEN post-fix)
- `npx vitest run src/providers/ src/api/contract.test.ts` — 187/187
- `npx vitest run` (full frontend) — **2486/2486**
- `npm run typecheck` — clean; `npx eslint` on both touched files — clean
- Go side untouched (`git diff --stat` is frontend-only); no backend test surface affected

## Review r1 (CHANGES_REQUESTED → remediated)

Three findings, all accepted and fixed on this branch:

1. **Flight-vs-flight mirror race (validated by the reviewer's probe, reproduced as M4)**: the mirror's watermark guard ran at complete-dispatch while the commit's watermark stamps lived in the deferred `setBusySessions` updater — an older overlapping idle flight (cursor 21) completing right after a newer busy flight (cursor 50) in the same batch flipped the cached row against the newer authority's answer. **Fix**: the answered sessions' watermark stamps are hoisted to complete-DISPATCH (idempotent max-stamps outside the updater; the updater's `Math.max` absorbs them) so the mirror — and everything after — sees the newest committed cursor per session.
2. **Missing pin**: M4 added (newer-busy flight then older-idle flight, same batch → badge yes, row stays `"active"`). RED under mutation 1, GREEN with the pre-stamp.
3. **Worklog mutation claims were false** (reviewer ran them; re-run and corrected here — every claim below was executed on this branch's final code):
   - pre-stamp block deleted → **M4 red (only)** — the hoist is load-bearing for the flight-race guard.
   - mirror watermark guard deleted (unconditional flip) → **M2 + M4 red** — the guard is load-bearing in both race directions.
   - idle mirror block deleted entirely → **M1 red (only)**.
   - `<=` flipped to `<` → **M1 red on THIS head** (the pre-stamp makes the answered session's watermark equal the cursor, so the equality arm is now exercised; on the r0 head the reviewer's non-discriminating verdict held — M2's margin 25-vs-21 skipped either way).
   - degraded staging guard deleted → **4/4 green**: confirmed non-discriminating by construction (a harness-keyed idle row can never match a phrase-keyed cache row by id). M3 is reframed as the documented no-flip invariant / defense-in-depth pin, not a guard discriminator.

## Residuals

- Sessions **absent** from a flight's rows (pod no longer knows them) still keep a stale `"active"` cache row until a refetch — the commit's removal loop is prev-dependent (inside the updater) and absent rows are outside the answered-idle mirror. Out of the incident shape (statusz lists idle sessions too); corrected by any reconnect/invalidation refetch; noted for a future pass if field evidence shows it.
- `wipeBusyForEventLoss` (reconnect/resync) intentionally does not mirror rows: the post-wipe REST re-seed refreshes them.
- #1899's deferred usage-leg staleness watchdog (half-open TCP death) remains open as designed — the lease bounds the damage.

## Mutation-verify suggestions (each EXECUTED on the final branch code — see "Review r1" for outcomes)

- Delete the dispatch-synchronous pre-stamp block → M4 must go red (flight-race guard).
- Delete the mirror's watermark guard (flip unconditionally) → M2 + M4 must go red (both race directions).
- Delete the idle mirror block → M1 must go red (the feature itself).
- Flip the mirror's `<=` to `<` → M1 must go red (the equality arm the pre-stamp creates).
- The degraded staging guard has NO discriminating mutation by construction (id matching alone prevents the flip) — do not claim one.
