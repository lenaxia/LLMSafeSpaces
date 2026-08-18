# Worklog: Epic 65 S1+S3+core-S2 — wire seam, ContextUsage contract, live-usage persistence through the adapter

**Date:** 2026-08-18
**Session:** Land the first implementation slice of the agreed long-term architecture for the broken context counter: golden fixtures from the live event store, the `Session.ContextUsage` contract amendment, the `pkg/agent/opencode/wire` parser seam, and `persistContextFromEvent` migrated onto `Adapter.ContextUsageFromEvent`. Plus one pre-existing test-hermeticity fix discovered en route.
**Status:** Complete (this slice; agentd migration, CRD-chain deletion, frontend S4 remain — see Next Steps)

---

## Objective

After Epic 65, the frontend context counter broke: `persistContextFromEvent` keyed on `session.next.step.ended`, an event opencode 1.18.10 never emits, and the frontend's realtime listener + compaction detector are equally dead. The agreed long-term fix (stress-tested in session ses_fed1c0845ffe): one wire parser behind the seam, usage as contract session state, API as single usage truth. This session delivers the backend core of that.

---

## Work Completed

### 1. Golden fixture from the live event store (S1)

- Discovered the sandbox itself runs on the platform: `/workspace/.local/opencode/opencode.db` holds the CURRENT build's persisted event stream. Extracted, redacted (synthetic sequential IDs, >120-char strings trimmed), and committed `pkg/agent/opencode/testdata/sse_events_1_18_10.jsonl` — 967 events in the real `/event` envelope `{id, type, properties}`.
- **Taxonomy definitively settled (0741-vs-0743 contradiction):** `session.next.step.ended` = 0 occurrences. Event types are `message.part.updated.1` (628), `message.updated.1`, `session.updated.1`, `session.created.1` — **version-suffixed**. Per-step tokens live in `step-finish` parts inside part-update events (58 in fixture); cumulative tokens on session/message updates.
- Envelope shape follows the live captures (epic-63 spike, worklog 0743): nested `properties`. The DB `event.data` rows supplied payloads; envelope reconstructed per capture format.
- `TestGoldenFixtureTaxonomy` (wire_test.go) pins: no legacy event, suffixed types exist, every golden step-finish decodes with tokens+sessionID.

### 2. Contract amendment (S3)

- `pkg/session/session.go`: `ContextUsage{Used, Window}` + `Session.ContextUsage` (`omitempty`). TDD: tests first (round-trip, omit-when-unset, bare-Used).
- Semantics per the design-0049 amendment: `Used` = tokens of window occupied (adapter-computed; opencode: `input + cache.read + cache.write`), non-monotonic (compaction resets), display-only; raw ledgers stay in `Cost`. The type's doc notes `ModelInfo.ContextWindow` is its denominator — the contract anticipated the gauge but never had the numerator.

### 3. Wire parser seam + adapter method (S2 core)

- New `pkg/agent/opencode/wire` package: `Tokens`, `Tokens.PromptTokens()`, `Envelope`, `IsPartUpdated` (suffix-tolerant: `message.part.updated(.N)`), `IsStepEnded` (exact — legacy name unversioned), `ParseStepUsage(eventType, raw string)`.
- Dual-shape tolerant by design (mixed fleet during image rollouts): decodes BOTH the legacy standalone step-ended event and 1.18.x step-finish parts. Type check runs before any byte conversion/JSON decode — non-usage traffic (hot deltas) costs string compares only.
- **Usage-typed-but-undecodable returns a drift error** (surfaced as adapter warn) — silent drops are how the 1.18.10 drift went unnoticed.
- `Adapter.ContextUsageFromEvent(eventType, rawData string)` added to `pkg/agent.Adapter`; opencode implementation computes `Used = PromptTokens()`, leaves `Window` unset (windows come from ListAvailableModels).
- Repolint containment holds: only `pkg/agent/opencode/adapter.go` imports `wire`.

### 4. Handler migration

- `persistContextFromEvent` now takes eventType, calls the adapter (nil-adapter → debug+skip; production always wires it since #716), warns on adapter-provided usage missing sessionID. The `session.next.step.ended` literal is GONE from platform code; `onRawEvent` dispatches unconditionally.
- Handler tests converted to wiring tests (`newUsageStubAdapter`); translation math pinned against real wire bytes in wire + adapter tests (incl. all 58 golden step-finish events). Both wire generations driven through the full `onRawEvent` path.

### 5. Pre-existing failure fixed (Rule 5): test env hermeticity

- `TestE2E_BootstrapMaterialize_TokenRejected_StillBoots` + `TestE2E_PasswordReset_FullPurgeThenBoot_NoProviders` failed on the CLEAN tree in this environment: `runAgentd` passed `os.Environ()` through to the materialize subprocess, and this sandbox is a real workspace pod — live `INFERENCE_RELAY_BASEURL`/free-models env activated the pre-boot relay, materializing an `opencode-relay` provider into zero-provider assertions. CI never sees this; dev-inside-a-pod (the repo's own multi-agent workflow!) always would.
- Fix: `scrubRelayEnv` strips `INFERENCE_RELAY_*` + `LLMSAFESPACES_FREE_MODELS_PATH` before caller overrides. Both tests now pass.

### 6. Skeptical validator loop (Rule 11 / multi-agent workflow)

Independent validator returned 6 real findings, all fixed:
1. **Hot-path copy + false comment** (material): `[]byte(rawData)` conversion happened before the seam's type check, and the "two string compares" comment was false for part-updated events. Fix: string end-to-end; conversion inside the seam after the type check; comments now state the true cost profile (and that `onRawEvent` already full-parses every event for the broker relay).
2. Package doc claimed agentd already consumes `wire` — rewritten as deferred.
3. Matched-type-undecodable silently dropped → now returns drift error + explicit tests.
4. Three stale comments naming the dead event (sessionindex, database, workspace_service) → reworded to "adapter-declared usage events".
5. Mock's silent default contradicted its "panics loudly" header → exception documented.
6. `StepUsage.Cost` speculative/unconsumed → deleted.
False alarms documented by validator (signed-suffix tolerance, negative tokens, nil-usage guard) — accepted.

---

## Key Decisions

| Decision | Rationale |
|---|---|
| Fixture from opencode.db (persisted events) + capture-format envelope | The DB is the only live current-build source reachable from this sandbox; envelope shape cross-checked against two independent live captures. Unverifiable from here: whether `/event` also synthesizes `session.status` (0 rows in DB) — flagged for the S5 live-cluster run. |
| `ContextUsageFromEvent` on the Adapter interface, not a wire import in handlers | Containment: platform code must not import `pkg/agent/opencode`; the interface is the seam. |
| Handler tests use stub adapters | Prevents tautological tests; real translation pinned at wire+adapter layers against golden bytes. Repolint forbids handlers tests importing the opencode package. |
| Unconditional `persistContextFromEvent` in `onRawEvent` | Kills the last event-name literal in platform code; cost is string compares for non-usage events. |
| Env scrub rather than skipping the failing tests | The repo's own workflow develops inside workspace pods; hermetic tests must not depend on host env. |

Assumptions validated this session: `Adapter.Stream` unwired (grep); agentd tracker lacks step-finish handling (read); no import cycle for shared wire pkg (grep both directions); token semantics — per-step (step-finish) vs cumulative (session/message updated) confirmed distinct in fixture; usage-event hot path already pays a full JSON parse for the broker relay (read).

---

## Blockers

None for this slice. Open verification item for S5: whether `/event` emits `session.status` events (agentd's busy/idle tracker depends on them; DB store shows none — may be SSE-read-path synthesis).

---

## Tests Run

- `go test -timeout 300s -count=1 -race ./pkg/session/ ./pkg/agent/...` — ok
- `go test -timeout 300s -count=1 ./api/internal/handlers/` — ok (103s, incl. previously-failing bootstrap e2e)
- `go test ./api/internal/services/sessionindex/ ./api/internal/services/workspace/` — ok (comment-only touch)
- `go build ./...`, `go vet` on all touched packages — clean
- Pre-existing failures verified on clean tree (stash + fixture removed) before fixing — not introduced by this diff

---

## Next Steps

1. **S2 remainder:** migrate `cmd/workspace-agentd/session_tracker.go` handling of usage onto `wire` (busy/idle classification stays; usage map deletion comes with S-cutover). Metering/inference callback (`sse/tracker.go handleSessionUpdated`) still parses `session.updated` inline — route through the seam; verify billing path against fixture (flat vs nested envelope question).
2. **Cutover (Decision 2):** API gap-fill via `adapter.GetSession` on SSE reconnect (wire into `reconcileSessionState`); then delete agentd `promptTokens`, statusz ContextUsed fields, CRD per-session mirror + `backfillContextUsed` CRD path; recompute workspace aggregate from session_index. Token-semantics trap: session-level tokens are CUMULATIVE — gap-fill must use the same `PromptTokens` computation as live path (per-step), or reuse last known value.
3. **S4 frontend:** ChatPage listens for contract usage (via `agent.event` carrying adapter-normalized payload or contract EventSessionUpdated bridge); compaction banner on contract signal; OpenAPI/SDK regen; delete `session.next.step.ended` literal + >50%-drop heuristic from frontend.
4. **S5:** live-cluster validation; drift counters (prometheus metric for unmatched usage-typed events); event-name-literal lint rule; opencode upgrade runbook (re-capture fixtures on bump); `OPENCODE_EXPERIMENTAL_EVENT_SYSTEM` env moves from pod_builder to runtime entrypoint.
5. Design doc 0049 amendment section for ContextUsage (types landed; doc edit pending before S4 SDK regen).

---

## Files Modified

- `pkg/session/session.go`, `pkg/session/session_test.go` — ContextUsage type + tests
- `pkg/agent/adapter.go` — interface method
- `pkg/agent/adapter_test.go` — fakeAdapter stub
- `pkg/agent/opencode/wire/wire.go`, `wire_test.go` — NEW seam package + tests
- `pkg/agent/opencode/adapter.go`, `adapter_test.go` — implementation + tests
- `pkg/agent/opencode/testdata/sse_events_1_18_10.jsonl` — NEW golden fixture
- `api/internal/handlers/proxy_events.go` — persistence through adapter
- `api/internal/handlers/mock_adapter_test.go` — mock + stub helper
- `api/internal/handlers/context_usage_e2e_test.go`, `opencode_upgrade_test.go`, `context_observability_test.go`, `proxy_test.go` — test conversion
- `api/internal/handlers/pod_bootstrap_e2e_test.go` — env scrub fix
- `api/internal/services/sessionindex/service.go`, `api/internal/services/database/database.go`, `api/internal/services/workspace/workspace_service.go` — comment corrections only

---

## Corrections (PR #938 review round 1)

Three claims in this worklog were wrong; corrected here rather than rewritten (append-only discipline):

1. **"Repolint forbids handlers tests importing the opencode package" — FALSE.** Repolint's agent-import rule excludes `_test.go` files (`pkg/repolint/agent_import.go`), and 15 handler test files already import `pkg/agent/opencode`. The stub-adapter approach was a choice, not a constraint — and it left the handler↔adapter seam untested against the real adapter. Fixed: `context_usage_adapter_e2e_test.go` drives real captured 1.18.10 wire shapes through `onRawEvent` with the REAL adapter wired (both type namings, legacy shape, drift path with warn assertion via zap observer, non-usage traffic).
2. **"The event-name literal is gone from platform code" — overreach.** True for `api/` only; `cmd/workspace-agentd/session_tracker.go:320,336` still switches on `session.next.step.ended` (the documented deferred agentd migration). Corrected claim: gone from `api/`.
3. **Fixture taxonomy test said "from a live capture"** — the fixture is reconstructed from the persisted 1.18.10 event store into the /event envelope shape (cross-checked against two live captures). Comment corrected.

Also addressed from the review: nil-usage guard + explicit interface guarantee (`ok=true` ⇒ non-empty sessionID, non-nil usage); `TestE2E_ContextUsed_JSONWireFormatThroughRouter` renamed to `...JSONWireShape` (it never went through a router).

**Open from review, resolved by evidence next:** the `session.updated` exact-match literals (title persistence `proxy_events.go:199`, billing `tracker.go:672`) vs the version-suffixed store types — a live /event SSE capture is running and will settle suffixed-vs-unsuffixed on the wire; decision (tolerant matching vs fixture regeneration) follows the evidence.
