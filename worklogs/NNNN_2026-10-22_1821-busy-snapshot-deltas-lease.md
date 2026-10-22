# Worklog: #1821 — busy-snapshot handshake, EventID-ordered deltas, frontend busy lease (pieces 2-4)

**Date:** 2026-10-22
**Session:** mighty-harbor (implementation worker, wt-1821, branch feat/1821-busy-snapshot-deltas-lease; base rebased 874d975d → 5d745efa before work began).
**Status:** Complete

---

## Objective

Land #1821 §2 pieces 2-4 on top of piece 1's merged emission discipline (#1824): the busy-snapshot handshake on the user-stream connect path (D9/D10 `session.status.snapshot` flights with `snapshot_ok` semantics), EventID-ordered delta application client-side, and the frontend 90s busy lease with exactly-one authoritative recheck.

## Work Completed

### Piece 2 — backend busy snapshot (proxy_busy_snapshot.go)

- `fetchStatuszSessions` — the ok-typed statusz read (3s budget, 1 MB decode cap, bearer candidates, Active+PodIP gate). ok=false on ANY failure: an indicator authority must answer "unknown", never fail-open-to-empty the way the send-gating `GetAuthoritativeActiveSessions` correctly does for its own purpose.
- `emitSessionStatusSnapshot` — the D9/D10 flight modeled on `emitPendingInputRequests` (proxy_input.go:442): `session.status.snapshot_begin {snapshot_id}` → one `session.status.snapshot` frame `{data: {cursor, sessions: [rows]}}` → deferred `snapshot_complete {snapshot_ok}`. Cursor = `UserEventBroker.LastEventID(userID)` read BEFORE the authority fetch, so a delta published during the flight carries id > cursor and outranks the snapshot at commit (piece 3's ordering).
- Rows phrase-keyed via `sessionIndex.Register` (idempotent, LRU-cached); harness id additive as `internal_session_id`; unresolved sessions degrade to the harness id flagged `session_id_deprecated` (never worse than the pre-fix wire, mirroring `markDeprecatedSessionID`).
- Wired into `snapshotUserWorkspaces`'s Active-workspace loop as a second detached flight (same `context.WithoutCancel` + recover containment the input flight documents for the ctx -race).
- `POST /workspaces/:id/sessions/status-snapshot` (`RequestSessionStatusSnapshot`) — RequestInputSnapshot pattern: 202 now, flight lands on the user stream. Registered beside the input-snapshot route on idGroup.
- `UserEventBroker.LastEventID(userID)` — shard-locked `nextID-1`, per-user.

### Pieces 3+4 — frontend (SessionActivityProvider.tsx)

- `busySessions` values became `BusyEntry {workspaceId, confirmedAt, confirmEventId?}` — the lease lives with the state atomically instead of parallel refs (all 9 mutation sites updated: latch, idle/aborted/deleted clears, agent_died, phase wipe, reconnect/resync wipe, alias rekey, REST seed, contract-stream clear bridge reads only).
- Delta ordering: `lastSessionStatusIDRef` watermark; a session.status delta with `event_id ≤` watermark is skipped (stale replay overlap — idempotent across snapshot/replay overlap); snapshot commits advance the watermark to their cursor. Unordered events (event_id absent) always apply — never worse.
- Snapshot flights: `busyFlightsRef` per-workspace/per-flight staging (begin/snapshot/complete keyed by snapshot_id; orphan frames ignored by the input-snapshot flight rule). ok=true commits authoritatively: entries of the workspace with ordered confirm ≤ cursor AND absent from the busy rows clear; fresher deltas (> cursor) and unorderable entries (REST seeds, legacy frames) survive; busy rows latch/re-confirm (lease refresh). ok=false keeps state — the D10 "keep your state" rule.
- Lease sweep (15s tick, whileAway-sweep pattern): entries older than `BUSY_LEASE_MS = 90_000` (≈3×25s heartbeat, above the 60s liveness window) fire exactly ONE `sessionsApi.requestStatusSnapshot(wsId)` per workspace per window; the lease re-arms at trigger, so an unreachable authority holds the badge (unknown ≠ idle) and retries once per window — never hammers, never self-clears.
- Reconnect/resync keep the #1646/#1687 wipe-before-invalidate ordering; open busy flights die with the connection and the connect-path flights rebuild authoritatively. REST seed stays as the degraded fallback (stamped unorderable so a snapshot governs it later).

## Red-first evidence

Backend (pre-implementation run, all 5 red — connect window delivered no flight, deadline expiry):
`TestStreamUserEvents_BusySnapshotOnConnect_RestoresBusy / _AuthorityDown_HoldsUnknown / _AfterReplayGap_Resync / _Flight_DoesNotDuplicateDeltas / _Rows_PhraseKeyed` — each failed with "Expected value not to be nil" / `[]string{} != [busy idle busy]` / snapshot-count 0 within the 3s connect window (quoted in the commit message).
Frontend (pre-implementation run, 5/6 red): T0.2a (snapshot→busy: badge stayed "no"), T0.2c (stale replayed idle cleared a snapshot-confirmed busy), T0.2d (authoritative idle snapshot did not clear latched busy), T0.2e (lease expiry fired 0 rechecks), T0.2f (0 recheck attempts). T0.2b passed vacuously pre-fix (nothing cleared on ok=false then either) — it is the mutation guard pinning that the NEW commit path never wipes on a failed authority read.

## Key Decisions

- **statusz as the busy-snapshot authority** (not per-session ABI `GetSnapshot`): the connect path needs the workspace-wide set in one flight; statusz is the surface `GetAuthoritativeActiveSessions` already consults; `derived_busy = status != "idle"` matches that consumer's rule. The consumer's consult (`BusyComponents`) still governs the delta surface — unchanged (#1781's emission legs untouched).
- **Cursor read before fetch**: a delta published in (cursor-read, fetch-done) has id > cursor AND reflects state the fetch likely saw — keeping it at commit is idempotent overlap; reading the cursor after the fetch would let an older snapshot wrongly supersede a newer delta.
- **Lease in the state value, not parallel refs**: one source of truth; StrictMode-safe (no ref/state divergence); the alias rekey moves entries with their leases for free.
- **Exactly-one recheck per window via trigger-time re-arm**: bounded worst-case staleness ~L under total event-path failure (charter piece 4) with a down-authority retry cadence of one-per-L, not a loop.

## Verification

- `go test ./internal/handlers/` — full package green (79s), including the 5 new wire tests + endpoint test; `./internal/services/eventbroker/` + `./internal/services/usagestream/` green; `TestUserEventBroker_LastEventID` green.
- One pre-existing test adapted: `TestSnapshotUserWorkspaces_FansOutPendingForActiveWorkspaces` now matches input-flight markers by type (`recvTypeWithTimeout`) — the connect path legitimately fans out two concurrent flights per Active workspace now, so strict next-event sequencing no longer holds; the behavioral assertions are unchanged.
- Frontend: `vitest run` — 2382/2382 green (211 files) including the 6 new T0.2 tests; provider family 132/132.
- gofmt / `go vet` / `golangci-lint` (handlers + eventbroker): 0 issues. `tsc -b`: clean. `npm run lint`: 0 errors (6 pre-existing warnings in generated abi_pb files).

## Review round r0 (PR #1848 bot review, CHANGES_REQUESTED) — all findings addressed

1. **Cursor-0/REST-seeded permanently-protected (BLOCKING)** — fixed: BusyEntry.confirmEventId is now 0 for REST-overlay evidence (orderable-at-0 — any snapshot governs), and snapshot-latched rows always carry a number (cursor 0 included). Only genuinely unorderable frames (no event_id — unreachable in practice) stay protected.
2. **Stale overlapping flight re-latch (BLOCKING)** — fixed: a flight whose cursor is below the applied-evidence high-water (max of applied delta ids + committed snapshot cursors) is discarded at complete — it cannot add or remove anything.
3. **Global watermark drops valid deltas (BLOCKING)** — fixed: replaced the watermark with dedup-by-applied-set + snapshot-cursor subsumption. The broker delivers after releasing the shard lock (arrival order not guaranteed), so an event is skipped ONLY on exact replay (id in the applied set) or snapshot subsumption (id ≤ cursor); a never-applied out-of-order delta still applies. Set pruned at commit (ids ≤ cursor) and hard-capped (256).
4. **statusz 200 with failed session listing committed as authoritative empty** — fixed: agentd's cachedState now returns a sessionsOK verdict (persisted across cache hits; false until a successful ListSessions), threaded into StatuszResponse.SessionsOK (pointer — older pods omit it and the API reads absent as ok, never worse); fetchStatuszSessions gates ok on it.
5. **Degraded rows not healed client-side** — fixed: every staged busy row runs through translateSessionIds (healthy rows are the phrase and teach the pair; degraded harness-keyed rows resolve through a learned alias or stay harness — never worse).

Robustness: the flight Registers only busy rows (idle rows never join client-side — a cold replica's thousands of idle sessions can no longer burn the 5s budget); emitSessionStatusSnapshot coalesces per workspace (sync.Map in-flight guard) so the on-demand POST cannot amplify concurrent statusz+Register flights. Style: sse_event.go SnapshotOK/SnapshotID docs cover the busy family; seedBusy's renamed entry variable; REST re-seed never downgrades a delta-confirmed entry.

E2 tier: deferred to #1818's coordinated suite per the issue #1821 coordination ruling (reviewer acknowledged the ruling; noted for the human reviewer).

Red-first round 2 (both suites observed red pre-fix): backend F4 (sessions_ok=false → asserted ok=false, got ok=true + empty rows committed); frontend F1a/F1b/F2/F3/F5 (5 failed — cursor-0 latch never cleared; REST seed never cleared; stale flight re-latched; out-of-order 99 dropped; degraded row unhealed). All green post-fix; agentd suite full green (361s — includes the new TestCachedState_SessionsOKSignal); handlers + eventbroker green; frontend 2387/2387; golangci-lint 0 issues (handlers, agentd, pkg/agentd); tsc clean; eslint 0 errors.

## Review round r1 (PR #1848 second bot review) — findings addressed

NOTE: the r0/r1 sections above describe the INTERMEDIATE designs as they shipped in each round (the global watermark, then the global cursor set); the FINAL design is what this section describes.

1. **Global subsumption cursor dropped cross-workspace deltas (BLOCKING)** — the ordering state is now per-workspace: `wsSnapshotCursorRef` (subsumption bound keyed by the delta's workspace_id) + `wsAppliedEvidenceRef` (per-workspace stale-flight high-water). The applied-delta id SET stays global (exact-replay dedup — ids are unique per broker).
2. **Global stale-flight high-water starved connect seeding/rechecks cross-workspace (BLOCKING)** — same scoping: a flight is discarded only against ITS OWN workspace's high-water.
3. **Cross-replica reconnect kept stale ordering state** — reconnect AND resync now clear all three ordering refs (the broker is process-local, ids restart at 1 per replica; the chart runs 2 API replicas with no session affinity). Additionally, resync now requests one status-snapshot flight per workspace that had latched busy (the authority re-seed — the "resync converges via the snapshot" claim now holds for resync too, not just reconnect).
4. **Degraded row with no learned alias deleted a phrase-keyed badge** — flights track `hasDegradedRows` (a busy row that stayed harness-keyed after the client-side heal); such flights ADD busy rows but never REMOVE (the id join is partial — absence cannot be proven for an entry the flight may be reporting busy under an untranslatable id).
5. **Late flight re-latched after terminal events** — agent_died and non-Active workspace.phase delete the workspace's open busyFlights entries before clearing.
6. **Agentd ListSessions status-blind residual** — non-2xx is now an error (a 502 body decoding as `[]` previously reported sessions_ok=true).

Red-first round 3 (observed red against the r1 code via stash-revert): frontend X1 (ws-B cursor suppressed ws-A delta), X2 (ws-B delta discarded ws-A flight), X3 (post-resync low-id flight+delta suppressed; recheck not requested), X4 (degraded no-alias flight cleared REST-seeded phrase entry), X5 (post-agent_died complete re-latched) — 5/5 red, 5/5 green post-fix. Backend X6: coalescing pin (behavior shipped in r1 — coverage-only, no red state existed). Agentd X7: 502-with-[]-body red (sessionsOK wrongly true) → green via the client.go status check.

One adjacent test updated: SessionActivityProvider.resyncBusy.test.tsx now mocks ../api/sessions (the resync path's additive authority recheck would otherwise hit real fetch and break that test's one-connection accounting; its REST-healing premise is unchanged).

Verification at r2: frontend 2395/2395 (211 files); agentd full green (363s); handlers + eventbroker green; golangci-lint 0 issues; tsc clean; eslint 0 errors.

## Review round r2 (PR #1848 third bot review) + CI red at da9fcac7

The third review (CHANGES_REQUESTED) plus a red CI taught the same lesson twice: VERIFY THE FULL PACKAGE SET. `TestOpenAPIRouterContract` (api/internal/server — never run locally before pushing) failed on the undocumented route; my "handlers + eventbroker green" claim was accurate but incomplete. Posture Gate's failure was infra (mise python@3.12.13 download failing 3×) — unrelated, retriggered.

Findings addressed:
1. **OpenAPI parity (BLOCKING)** — `POST /workspaces/{id}/sessions/status-snapshot` documented in sdks/openapi.yaml (input-snapshot's shape; typed responses, coalescing + D10 semantics in the description).
2. **Coalescing orphaned mid-flight joiners (BLOCKING)** — the guard moved from the emitter to the on-demand trigger only; the connect path always emits its own per-subscriber flight (a fresh subscriber has no backlog). Red: behavioral revert of the guard's location → joiner sees no begin (2.00s timeout); green at head.
3. **Same-session out-of-order overwrite (BLOCKING)** — ordering now enforced PER-SESSION (`sessionLastDeltaIDRef`): a delta applies only above its session's watermark; a snapshot's row latches only when no newer per-session evidence supersedes it. The global applied-id set is deleted (its per-workspace-cursor pruning was itself a cross-workspace mixing bug).
4. **Degraded-row removal poisoning (HIGH)** — latched entries carry `internalId` (the additive harness id from deltas; the row key for degraded snapshot rows). A degraded busy row blocks removal of exactly the entries matching its harness id (plus internalId-less REST/legacy entries) — resolved sessions stay clearable.
5. **Same-workspace flight starvation (MEDIUM)** — the wholesale stale-flight discard is REPLACED by per-entry guards: every ANSWERED session's watermark (busy or idle row) moves to the cursor, so an unrelated session's mid-flight delta no longer discards the recheck's rows.
6. **ok=false after a wipe-initiated re-seed (MEDIUM)** — `pendingRecheckRef` schedules exactly ONE authority retry per lease window (the sweep processes it; an authoritative answer clears it). Scoped honestly: during a statusz blip the post-reconnect badge still reads idle until the first successful flight (the #1646 wipe + REST-overlay semantics are pinned elsewhere; full unknown-hold needs statusz-failure semantics on the session-list surface — noted as follow-up class).
7. Style: typed `busySnapshotPayload` struct (Rule 5); the DoesNotDuplicateDeltas comment no longer claims concurrency.

E2E (the thrice-flagged gate): added `frontend/tests/e2e/busy-snapshot-lease.spec.ts` — refresh-mid-busy (REST idle, snapshot-only badge, double reload), down-then-recovered (ok=false corrupts nothing; first ok=true converges), abort-reconnect. 3/3 green locally (9.4s). The lease-timing tier stays in vitest (fake timers).

Red-first round 4 (observed red via stash-revert): frontend Y2 (same-session inversion ended busy), Y3 (degraded row starved a resolved clear), Y4 (unrelated delta discarded the recheck's rows), Y5 (no pending-recheck retry) — 4/4 red, green post-fix; backend Y1 (guard-location revert → joiner unseeded, behavioral red captured).

Verification at r3 (the FULL set this time): api ./internal/server + ./internal/handlers + ./internal/services/eventbroker green; cmd/workspace-agentd FULL green (360s); frontend vitest 2399/2399 (211 files) + eslint 0 errors + tsc clean; Playwright busy-snapshot-lease 3/3; golangci-lint 0 issues (handlers, server, agentd, pkg/agentd); repolint ok.

## Review round r3 (PR #1848 fourth bot review)

1. **Stale-closure reconcile (BLOCKING, my r2 regression)** — the doomed-decision loop read the render-closure `busySessions`; same-chunk frames (SSE dispatches a chunk's frames synchronously) saw pre-chunk state and an older-cursor idle commit could delete a same-chunk newer busy re-confirm. Fixed: the entire reconcile (removals + watermarks + latches) runs INSIDE the setBusySessions updater against its fresh `prev`; every ref write inside is an IDEMPOTENT max-stamp, so StrictMode double-invocation is harmless (the #1824 r11 hazard is non-idempotent writes). Red pin: Z1b.
2. **REST entries pinned by any degraded row (BLOCKING, F4 incomplete)** — v2 rule: a session with a RESOLVED answer under its own key (answeredIds) is always clearable; unresolved entries fall back to identity matching (own harness id / reverseSessionAlias of the phrase — new export / protected when no identity exists). Red pin: Z2.
3. **Harness-keyed idle rows missed the phrase watermark (MEDIUM)** — root-caused to my r1 "Register only busy rows" optimization; idle rows are REGISTERED too (every row joins the phrase space; Register remains idempotent/LRU-cached; per-register 2s budget; degrade path unchanged). Pin: Z3 (+ the wire suite).
4. **pendingRecheck leak on terminal states (MEDIUM)** — agent_died / non-Active phase now also drop pendingRecheckRef + wipedBusyRef; a DROPPED flight (ok=true, staged missing — backpressure or terminal invalidation) schedules nothing. Red pin: Z4 (0 POSTs across two windows post-terminal).
5. **Unknown-hold delivered (HIGH — the charter ask)** — the wipe REMEMBERS what it cleared (wipedBusyRef, filled inside the wipe's updater — idempotent); an ok=false answer to the post-wipe re-seed re-latches the memory as unknown (badge never settles idle across the blip) with the freshly-stamped lease owning the once-per-window recovery; when there is no memory (nothing was wiped), the pending recheck owns the identical cadence. The sweep dedupes lease-expiry and pending-recheck per workspace. Pinned: vitest Y5 (fake timers, deterministic, including the steady-state one-per-window cadence while genuinely busy) + e2e test 2 (authority-down across reconnect churn never clears the live badge; the memory re-latch itself stays in vitest because real-clock polling races the ms-scale dip).
6. **Backend asks** — fetchStatuszSessions unhappy-path table (nil k8s / Get error / non-Active / empty PodIP / non-200 / bad JSON / oversized: all NOT-authoritative); T1.4 now drives TRUE concurrency (deltas publish the moment the subscriber registers, racing the in-flight fetch — 3/3 stable). Style: the test seam went unexported (requestSessionStatusSnapshotSync).

Red-first round 5 (stash-revert of the provider): Z1b (stale-closure deletion), Z2 (REST pin), Z4 (retry leak), Y5 (memory re-latch + cadence) red; Z1/Z3 passed on r3 (they pinned properties r3 partially had — kept as regression pins).

Verification: api handlers (full, 126s) + server + eventbroker green; agentd targeted green; frontend vitest 2404/2404 (211 files); eslint 0 errors; tsc clean; Playwright 3/3; golangci-lint 0 issues.

## Blockers

None.
