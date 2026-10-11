# NNNN — #1672 initial-load busy composition wrong (1-of-3 vs platform's 3-of-3)

Charter: fresh app restart on v0.36.0 showed 1-of-3 sessions busy in
the UI while the platform's own session-status endpoint — read seconds
apart — reported all 3 busy. Layer triage with evidence, then fix +
red-first pins at the guilty layer.

## Layer triage (all verified by direct code read on main @ 653ea772)

Two sources answer "which sessions are busy", and the guilty one is the
listing's derivation — **layer (a), server-side in agentd**:

| Consumer | Busy source | Where |
|---|---|---|
| Platform session-status (session_metadata) | opencode live `GET /session/status` registry | mcp_tools.go:826 (`client.GetSessionStatuses`) |
| Sessions listing `status` field | agentd `/v1/statusz` = SSE-event-fed tracker ∥ #1574 ABI overlay | router.go sessions merge → proxy_connections.go:176 → session_tracker.go:567 (`cachedState`) |
| Drain gate / orphaned-tool repair | live registry (same as session_metadata) | agent_drain.go:51, adapter.go:628 |

- The DB leg contributes nothing: `ListSessionIndex` hard-codes
  `item.Status = "idle"` (database.go:1151).
- **Leg A (the 1-of-3 signature, FIXED):** statusz's busy content is
  entirely event-fed. The tracker learns `session.status=busy` only
  from SSE (session_tracker.go:462-479); the connection is force
  recycled every 5 min (`sseConnectionTimeout` :334) with 2-30s
  reconnect backoff — a turn starting inside a gap is never marked
  busy for the remainder of the turn. The #1574 overlay
  (`applyBusyTruthLocked`) then actively UN-busies quiet mid-turn
  workers the tracker did mark (busyTruth derives not-busy for a
  session with no observed streaming/parts/queue). The one
  constantly-streaming session (the orchestrator) re-derives busy from
  continuous part events; the two quiet workers (w23 "review loop
  polling", w24 queued send-ping) miss both legs → exactly 1-of-3.
- **Leg B (0-of-N class, deferred — NOT this incident's signature):**
  the API's statusz fetch has a 3s budget
  (proxy_connections.go:194) against a handler documented to block up
  to ~15s under three concurrent turns; on timeout/non-200/decode
  failure it returns an EMPTY set (:200-208), indistinguishable from
  "nothing busy". Deliberate for the turn-admission gating consumer
  (#792: "better to allow a new turn than block on stale state") but
  wrong-shaped for the listing consumer; would render 0-of-N, not the
  observed 1-of-3. Deferred as follow-up (needs per-consumer failure
  semantics, out of this charter's scope).
- **Layer (b) active-runs: RULED OUT** — the sidebar's busy pill
  renders from SessionActivityProvider's `busySessions` via
  `useSessionStatus` (Sidebar.tsx:884,892-893); the sidebar query
  composes getSessions + sessionOrigins only (Sidebar.tsx:570-583);
  active-runs is not in the busy path.
- **Layer (c) cache scoping: RULED OUT** — one query key
  (`["sessions", workspaceId]`), no persistQueryClient/persister
  anywhere under frontend/src; fresh bundle = empty cache, live fetch.
- **Why it stayed wrong:** #1646's seed gate latches
  (`seeded.add(wsId)`, SessionActivityProvider.tsx:246) and the 60s
  floor deliberately never re-derives busy (Sidebar.tsx:590-593) —
  whatever the one full fetch said is sticky for the tab's lifetime.
  Frontend therefore NOT guilty (its input was wrong), and untouched:
  w20's 10-pin sseStaleness suite + clobber guards (666/714/758/490/
  1540) unaffected by construction — zero frontend diff.

## Fix (agentd, render layer — one pod, one busy answer)

`cmd/workspace-agentd/`:

- **client.go**: new `GetSessionStatuses` — the harness live-turn
  registry (`GET /session/status`, strict decode, loud non-200 error);
  mirrors the pkg/agent/opencode/client.go:209 seam the platform's own
  consumers use.
- **session_tracker.go**: `cachedState` now composes THREE inputs per
  render: tracker ∥ authority verdict ∥ harness registry as a **busy
  FLOOR** —
  - `busyVerdict{Busy, PendingUserAsk}` widens `busyTruthFn`
    (map[string]bool → map[string]busyVerdict); `busyTruthFrom` derives
    `PendingUserAsk` from `BusyComponents.GetPendingUserInputs()`.
  - **#1574 carve-out survives the floor** (ruling confirmed by the
    orchestrator mid-flight): registry-busy renders busy UNLESS the
    authority confirms a pending user ask with zero streaming/parts/
    queue (`Busy=false && PendingUserAsk=true`) — user-blocked stays
    idle; a pending ask must not be hidden behind a busy pill.
  - Floor is add-only: a registry "idle" never un-busies (idle leg
    belongs to tracker/authority; #1646-adjacent stale-busy classes
    untouched). Registry-blind sessions keep the tracker ∥ authority
    answer.
  - Registry fetched once per 15s TTL window inside the miss path with
    its own 2s budget (`registryFetchBudget` — cachedState holds mu
    across sync opencode calls; the floor is the least-critical input
    and must not extend the documented starvation window); on refresh
    failure a previously-fetched registry survives only inside a 60s
    staleness grace (`registryStalenessGrace`), then the floor goes
    inert — no phantom-busy off unrefreshably-stale data. Registry
    busy never feeds tracker internals (busySince/#998 ages,
    #1342 stall partitions, restart deferral) — render-only overlay,
    exactly like applyBusyTruthLocked.
- **server.go**: statusz handler's one-truth-snapshot is now
  `map[string]busyVerdict` (snapshot-backed closure: both overlays +
  the busy-age filter judge the same map — invariant preserved).

## Evidence chain (red → green, verified locally)

1. **RED on main @ 653ea772** (pins authored before the fix, run
   against a clean tree): `TestCachedState_RegistryFloorRecoversMissedBusy`
   and `TestStatuszHandler_RegistryFloorThroughTheRealHandler` failed
   with exactly the incident signature — orch busy, w23/w24 idle
   against a 3-of-3-busy registry, empty tracker, authority
   busy/not-busy/blind. Two guard pins (NeverUnbusies,
   UnavailableKeepsComposition) passed on main by design — they pin
   today's behavior as mutation guards.
2. **GREEN after fix**: all 6 registry-floor pins +
   TestOpenCodeClient_GetSessionStatuses pass.
3. **Mutation self-checks (pre-empting the orchestrator's)**:
   floor application removed → the 4 behavior pins FAIL; carve-out
   check alone removed → the carve-out pin FAILS; restored → green.
4. **Suites**: full `cmd/workspace-agentd` package (326.9s, includes
   w20-adjacent #1646-era suites and the #1574 busy_overlay pins with
   assertions byte-identical — only closure literals widened to the
   verdict type) + `sessionstate` package green; `go vet`, `goimports`,
   `golangci-lint run cmd/workspace-agentd/...` → 0 issues. Frontend
   untouched (no diff) → the ~2040-test suite is unaffected by
   construction; CI re-runs it on the PR.

## Pins (cmd/workspace-agentd/registry_floor_test.go)

1. `TestCachedState_RegistryFloorRecoversMissedBusy` — the 1-of-3
   incident repro at the composition layer, both the TTL-miss and
   cache-hit render paths.
2. `TestStatuszHandler_RegistryFloorThroughTheRealHandler` — the same
   through the production /v1/statusz wiring (the surface the API
   listing merge reads).
3. `TestCachedState_RegistryFloorNeverUnbusies` — add-only guard.
4. `TestCachedState_RegistryFloorRespectsPermissionCarveOut` — #1574
   ruling survives the floor (the naive-floor mutation detector).
5. `TestCachedState_RegistryFloorStalenessGraceBounded` — transient
   refresh failure keeps the floor, unrefreshable staleness drops it.
6. `TestOpenCodeClient_GetSessionStatuses` — seam decode + loud error.

## Disclosure / follow-ups

- The carve-out ruling (harness-busy floor, carve-out only on confirmed
  pending ask) was proposed in the issue thread, ratified by the
  orchestrator during this work: "a session blocked on a confirmed
  pending user-ask is genuinely not-busy; rendering it busy would hide
  the actionable ask."
- Leg B (statusz-unreachable → empty set on the listing surface)
  deferred — different failure signature, deliberate #792 semantics for
  admission gating; needs its own ruling to split per-consumer failure
  behavior.
- Adjacent surface OBSERVED, not changed: restart deferral
  (`hasAnyBusy`/US-44.2) and #1342 stall partitions read the tracker,
  not the render — a registry-busy session the tracker missed is not
  restart-deferred. Same missed-event class, different blast radius;
  left for the owners to charter.
- Verified vs reasoned: everything in the triage table and the fix
  mechanics is code-read verified on this tree; the historical claim
  that THIS incident's sessions hit the SSE-gap (vs some other missed-
  event path) is reasoned from the signature match — agentd SSE
  reconnect logs from the incident window would confirm, but the fix
  covers the class either way.

## Round 2 (AI review r1 — CHANGES_REQUESTED → addressed)

Findings and dispositions (review at PR #1673, commit c1a14c44):

1. **[r1 finding 1] ≤15s TTL busy-linger after a cleanly-observed turn
   end** (hit path applied the cached registry with no staleness
   check; reached the listing pill AND /sessions/active). **FIXED, not
   just disclosed**: the tracker now stamps `idleObservedAt` on every
   OBSERVED session.status=idle (and on the generation-change busy
   reset — its own contract calls that state honest idle; a busy mark
   clears it), and the floor defers to observed-idle evidence strictly
   NEWER than the registry's read (`observedIdleAfter`). A cleanly
   observed turn end renders idle on the very next render — zero
   linger on the normal path, and during a refresh outage the observed
   idle still wins over the grace-stale registry. Rejected alternative
   during design: keying off `lastEventAt` (ANY activity) instead of
   observed idle — sparse-but-alive mid-turn workers (w23's polling
   loop) would have refreshed lastEventAt and DEFEATED the floor for
   exactly the incident's sessions; only a status TRANSITION to idle is
   end-of-turn evidence.
2. **[r1 finding 2] authority-blind pending-ask floors to busy**
   (the same gap that blinded the tracker can blind the authority to
   the ask events → carve-out cannot confirm → floor applies for the
   ask's duration). **PINNED AS ACCEPTED + disclosed**: the ratified
   carve-out requires CONFIRMATION; an unconfirmable ask does not
   override the harness live-turn truth, and the ask surfaces on its
   own channel (frontend pendingActions pills ride the browser event
   stream, not this composition). `TestCachedState_BlindPendingAskFloorsBusy`.
3. **[r1 missing-1]** → `TestCachedState_RegistryFlipAppliedOnNextRefresh`
   (refresh result stored AND applied — reorder/dropped-store guard).
4. **[r1 robustness]** starvation-window claim reworded (the fetch DOES
   extend the mu window, bounded ≤2s); `io.LimitReader(64KB)` added to
   the agentd registry read (now truly mirrors the pkg twin).
5. **[r1 noted, disclosed]** positive side effect: the floor also
   corrects GET /sessions/active admission data (same statusz source).

Mutation-checked r2: observed-idle check removed → the r2 pin fails;
restored → green. Full cmd/workspace-agentd suite green (326.7s),
sessionstate green, vet/lint/goimports clean. Leg B tracking issue to
be filed per review ask.
