# Worklog: #1602 — the frontend busy truth: derived BusyComponents through the session.status SSE pipe

**Date:** 2026-09-30
**Session:** Carry the #1574 derived busy truth from the sessionstate authority to the frontend's busy indicator; fold in #786 (aborted/deleted events).
**Status:** Complete (pending review)

---

## Objective

Issue #1602: sessions running tool executions rendered IDLE in the frontend. The provider's busy state (SessionActivityProvider.tsx:120-135) is SSE-driven with REST refetches deliberately suppressed — but the API's session.status SSE emission bridged the RAW dialect status (streaming-only semantics), so the harness's idle-while-a-tool-runs reached the UI as idle and the stale-clobber protection preserved the lie.

---

## Work Completed

### First read (traced, reported to the orchestrator before any push)

- **The pipe today:** agentd authority folds opencode SSE → ABI Events stream → API `usagestream.Consumer` (busy-gated, one per workspace) → `Bridge.SessionStatus` on `EVENT_TYPE_SESSION_STATUS` (raw status) → `usageBridge` → userBroker `session.status` SSE → the provider (sole post-seed truth).
- **Symptom anatomy:** the REST seed is ALREADY truthful (GET /workspaces/:id/sessions overlays statusz, which carries the #1578 authority overlay) — the first raw SSE idle clobbers the correct seed and the provider's protection keeps it.
- **Two aggravators found by tracing:** (1) `abiclient.cloneSessionSnapshot` DROPPED the `Busy` field — the snapshot-served components never survived into the client fold; (2) the consumer's idle-drop gate derives busy from the raw fold — a silent >30s tool run could tear the gate down mid-turn and miss the final idle (stuck-busy class).
- **One derivation gap found:** a COMPACTING status event set `rec.status` only — a bare compacting session derived NOT busy (statusz rendered it idle against the owner's rule), and the overlay would have regressed compacting→idle at the bridge.

### Backend half (all red-first)

- `cmd/workspace-agentd/sessionstate/projection.go`: COMPACTING status events mark busy (the streaming leg — compaction is autonomous progress). Event path ONLY; the seed path deliberately never marks (the #1584 fold-dead wedge must not resurrect). Pinned in `busy_truth_test.go` (TestBusyCompactingIsBusy / TestBusyCompactingHoldsAcrossParts).
- `pkg/abi/abiclient/client.go`: `cloneSessionSnapshot` deep-copies `Busy` (nil-safe). Pinned against the real authority surface (TestStreamSnapshotCarriesBusyComponents).
- `api/internal/services/usagestream/consumer.go` — the overlay:
  - SESSION_STATUS emissions consult the authority's BusyComponents via the ABI `GetSnapshot` op (read verbatim — one definition, never recomputed). Consult failure or projection-unknown (NotFound) fails open to the legacy raw mapping: the pipe goes legacy, never quiet.
  - PART_START / PART_END / MESSAGE_START / ERROR are busy-flip candidates: the incident's silent class (tool starts after the harness's idle — no status event) publishes the flip, exactly once per flip.
  - The idle-drop gate ORs in the consult-maintained derived truth (silent tool runs hold the gate; the final idle is seen).
  - Reconnect reconcile: the connection's first fold publication (the busy-aware snapshot frame) rebuilds the flip baseline — a stale-true entry cannot lease a stream forever (adversarial-review finding; scale-to-zero preserved).
- E2E (`busy_truth_e2e_test.go`): REAL sessionstate authority + REAL abiclient over the REAL ABI HTTP surface + REAL consumer → bridge. Pins the running-bash row (part-start flip → busy; busy holds between tool completion and the harness's word; final idle), the compacting leg, and the permission-wait carve-out.
- `api/internal/handlers/{proxy_handlers,admin_session,proxy_events}.go` (#786): the deleted/aborted/reconcile-idle emitters used the WORKSPACE-scoped publish — the provider (user stream) never saw them at all. All three now use `publishWorkspaceAndUserEvent` (workspace copy keeps the unstamped convention; the user copy carries the routing key).

### Review round 1 (automated reviewer findings, all red-first fixed)

- **In-stream reseed reconcile** (Finding 1, validated): the reconnect reconcile missed abiclient's IN-STREAM `projection.reseeded` redial (handled without returning). abiclient gained `WithResynced`/`ResyncedOf` (fired on every snapshot-frame fold rebuild, BEFORE onUpdate); the consumer re-arms `seededFold` there. Pinned at the consumer level (fake fires resynced → republish without Stream returning) AND e2e (real authority generation-change reseed → gate drops).
- **Observable fail-open** (Finding 2): consult failures warn rate-limited (first + every 50th) via the Logger seam; distinct messages for error vs Busy-less snapshots.
- **#786 completion** (Finding 3): the interrupted indicator (provider `abortedSessions` + `useIsSessionAborted` + Sidebar `OctagonX` marker; cache records `aborted`, not erased-to-idle), deleted-session navigation (ChatPage route-level effect — the provider sits ABOVE the routes where useParams is always empty), per-session cache teardown (`removeQueries` predicate on the session id). 14 ChatPage test mock factories gained the new export; the hook-count guard updated (70→72).

### Review round 2 (one validated finding, red-first fixed)

- **Prompt-content orphaning**: the aborted/deleted branch cleared `pendingActions[sid]` but left the request-keyed content maps — and by clearing the indicator first it blinded the `snapshot_complete` reconcile (which walks pendingActions to compute doomed), a regression vs main where the fold sync pruned the orphan. Fix: collect the session's doomed ask ids BEFORE the clear and prune content + refs (the clearSessionPendingPrompts shape). Pinned: "no orphan pills in parent tabs" provider row.

### CI hygiene

- `brace-expansion` transitive bump past CVE-2026-102276/102278 (advisory landed after main's last green scan; lockfile-only).
- contextcheck nolint with justification (the ctx-less abiclient callback seam; consult derives from the gate ctx).
- e2e race fix: no `SetStoreForTest` on a live-served authority (swaps the store pointer against concurrent GetSnapshot consults — the CI race detector caught it); the store is wired at construction, its mutex-guarded seed swapped instead.
- The SDK live-API canary (`get-audit` / duplicate `canary-py-dup`) failed once and passed on rerun — a flake, unrelated to this diff (no secrets/audit surface touched).

### Frontend half (red-first)

- `SessionActivityProvider.tsx`: `aborted` and `deleted` statuses handled (#786) — both clear busy + the session's pending prompts; `deleted` additionally drops unread and removes the session from the sessions cache (sidebars render the removal live). Neither marks unread (no response arrived). Stale-clobber protections untouched — the SSE pipe itself is now the truthful source.
- `api/types.ts`: the status union carries `"aborted" | "deleted"`.

---

## Key Decisions

1. **Bridge-emission overlay (GetSnapshot consult) over an ABI stream change.** The briefing: "the #1578 ABI carries the components — use them, don't recompute." GetSnapshot IS the ABI op serving them; no schema change, no new frame type, the authority remains the single definer. Cost: one bounded RPC (2s budget) per flip-candidate event — rare (turn/part boundaries), and the serve is a projection read plus a coalesced lease gather. Rejected: busy-flip frames on the Events stream (ABI freeze extension + flip-detection machinery at every authority mutation site + fold-equivalence hazards — synthetic IDLE application would clear #1576's renderable corpses).
2. **Fail-open to legacy on consult failure.** A glitching pod must not silence the pipe; it degrades to today's raw mapping. Documented in the switch.
3. **COMPACTING markBusy in the projection, not a consumer-side special case.** Forking the definition at the consumer is exactly the two-definitions bug class #1574 killed.
4. **Status events keep their every-event cadence; candidates are flip-gated.** Status events always bridged per-event (frontend expectations, e.g. the hung-clear on idle); the new candidate legs publish only on flips to avoid spamming the user stream.
5. **#786 folded in** — the provider was the only live status consumer (the ChatPage render path retired it), and the fix needed BOTH the provider handling AND the user-stream delivery (the events never reached the provider at all).

## Assumptions stated and validated (Rule 7)

- "The authority's BusyComponents are the derived truth" — verified in `enrichBusyLocked`/`sessionSnapshotLocked` and pinned by busy_truth_test.go.
- "GetSnapshot serves Busy on every read" — verified in service.go GetSnapshot (`sessionSnapshotLocked` always enriches).
- "Status events are rare (turn boundaries)" — verified via the dialect translation (session.status/session.idle at segment boundaries; the wsstate comment's busy-once-per-turn).
- "No production Deliver caller rides the frontend message path today" — verified: no `.Deliver(` callers outside tests in api/ and pkg/agent.
- "The provider is the only session.status status consumer in the frontend" — verified via grep; types.ts notes the chat render path retired it.

## Residuals (documented, accepted by the orchestrator)

- Non-event flips (the #1576 reconcile busy-clear of fold-dead sessions) are not pushed to the pipe — they self-heal via agent_died (the stream dies with the harness), the hung-workspace alert surface, and the truthful REST seed.
- Queue-leg busy (ledger Deliver) rides any candidate-event consult automatically (Busy.busy includes queue depth) but has no dedicated push — dormant on the live frontend path today (no production Deliver callers).

---

## Blockers

None.

---

## Tests Run

- `go test ./cmd/workspace-agentd/sessionstate/ -timeout 300s` — ok (38.7s, full package)
- `go test ./pkg/abi/abiclient/ -timeout 120s` — ok (84.4s, full package)
- `go test ./api/internal/services/usagestream/ -timeout 180s` — ok; `-race` — ok; the e2e reseed row additionally verified `-race -count=3`
- `go test ./api/internal/handlers/ -timeout 840s` — ok (67.7s, full package)
- `go build ./...` — ok; `golangci-lint run` on all touched packages — 0 issues
- `npx vitest run` (frontend) — 178 files, 1969 tests passed (r2: 94 provider rows, 239 pages rows, 106+9 layout rows)
- Red-first evidence: every test named above failed against the pre-fix tree (compacting derivation, Busy clone-drop, the five consumer overlay rows, the reconnect + in-stream reseed reconciles, the observability row, the two user-stream delivery rows, the #786 provider rows incl. the orphan-pills row). The reviewer independently mutation-verified the r1/r2 pins (reverting WithResynced re-arm, the warn, and the idle-erase each turns its row red).

---

## Next Steps

- Iterate the automated review to APPROVED; the orchestrator (ses_f499ee9e6ffe52BJ8jxc2TEQQJ) adjudicates against the incident-sequence E2E and the running-bash row, then merges.
- The #1576-layer umbrella consumers (US-65.8 / #1576 layers) can now consume the truthful pipe as the shipped precedent.

---

## Files Modified

- pkg/abi/abiclient/client.go (also: WithResynced/ResyncedOf seam — r1)
- frontend/src/pages/ChatPage.tsx + ChatPage.navigate.test.tsx + ChatPage test mocks (r1/r2 — deleted navigation)
- frontend/src/components/layout/Sidebar.tsx + Sidebar.sessions.test.tsx (r1 — interrupted indicator) + Sidebar.test.tsx/AppShell.test.tsx (mock export)
- frontend/package-lock.json (CVE bump)
- (everything below from the initial round)
- cmd/workspace-agentd/sessionstate/projection.go
- cmd/workspace-agentd/sessionstate/busy_truth_test.go
- pkg/abi/abiclient/client.go
- pkg/abi/abiclient/client_test.go
- api/internal/services/usagestream/consumer.go
- api/internal/services/usagestream/consumer_test.go
- api/internal/services/usagestream/busy_truth_e2e_test.go (new)
- api/internal/handlers/proxy_usagestream_test.go
- api/internal/handlers/proxy_handlers.go
- api/internal/handlers/admin_session.go
- api/internal/handlers/admin_session_test.go
- api/internal/handlers/proxy_events.go
- api/internal/handlers/proxy_test.go
- frontend/src/providers/SessionActivityProvider.tsx
- frontend/src/providers/SessionActivityProvider.test.tsx
- frontend/src/api/types.ts
- COORDINATE.md
