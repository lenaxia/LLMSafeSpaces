# Epic 63: Inboard Session Queue — Adopt opencode V2 Session API

**Status:** Planning
**Created:** 2026-08-02
**Priority:** High
**Depends On:** Epic 41 (delivered the current Redis-backed queue whose complexity this epic eliminates), Epic 29 (AgentClient abstraction in `pkg/agent/opencode/`)
**Related epics:**
- **Epic 41** — shipped the Redis-backed message queue + 409 guard + drain-on-idle. This epic **supersedes** US-41.2 (409 guard), US-41.3 (409 retry), and the `drainQueuedMessage` / `redirectPromptToQueue` / `flushAndAbortAfterIdle` machinery. US-41.1 (frontend streaming-state timing) and US-41.4 (onSessionIdle dead-code fix) are orthogonal and remain.
- **Epic 62** (US-62.6) — adds queue methods to all four SDKs against the current Redis-backed API. This epic changes the queue model; US-62.6 must target the V2 model or be deferred until US-63.4 lands. See US-63.8.
- **Epic 44** — `ForceAbortSession` (admin escape hatch for stuck pods) is orthogonal and retained; this epic does not touch it.

---

## Problem Statement

The session message queue in llmsafespaces is implemented **outside** opencode as a Redis FIFO drained by the API proxy (`api/internal/services/msgqueue/service.go`, `api/internal/handlers/proxy_events.go:442-498`). The proxy speaks V1 HTTP (`/session/:sid/prompt_async`, `/session/:sid/abort`) to opencode, and opencode's V1 runner silently discards work submitted while busy (`ensureRunning` → discard, Epic 41 V4). The external Redis queue exists to bridge that impedance mismatch.

The external queue has accumulated significant incident-scarred complexity to compensate:

1. **The message-id hack** (`msgqueue/service.go:27-86`): queue IDs are generated in opencode's `msg_...` format but deliberately **not sent** to opencode, because opencode's loop-exit predicate compares raw id lex order and a wrong id causes either an infinite self-talk loop or a silent drop. Both failure modes were observed in production on **2026-06-29**.

2. **The 409-requeue path** (`proxy_events.go:460-474`): when drain-on-enqueue fires on a false-idle read, opencode returns 409. The proxy requeues once without burning the retry budget — a targeted patch for a race that shouldn't exist.

3. **The destructive abort** (`proxy_handlers.go:625-750`): `AbortSession` clears the Redis queue, then asynchronously `flushAndAbortAfterIdle` re-sends each message one-at-a-time and aborts again so they appear in the transcript unprocessed. This 75-line workaround exists solely because the external queue cannot survive an abort.

4. **The stranded-queue sweep** (`proxy_events.go:578-664`): `reconcileSessionState` queries `/v1/statusz` on SSE reconnect to find sessions that went idle while the connection was down, then re-triggers `drainQueuedMessage`. This entire class of bug exists because the queue is external to the agent that processes it.

opencode's V2 session infrastructure (`packages/core/src/session/`) already solves all of these problems internally with a durable event-sourced input table, atomic admission, and non-destructive interrupt. **The V2 session API is already served over HTTP by the bundled opencode binary** (see F4 below). This epic's job is to make the proxy use it and delete the external queue.

---

## Key Findings (Code-Verified)

| # | Finding | Evidence |
|---|---------|----------|
| F1 | opencode 1.18.10 (bundled in llmsafespaces, `runtimes/base/Dockerfile:49`) contains the V2 session infrastructure with `delivery: "steer" \| "queue"` | `git cat-file -e v1.18.10:packages/schema/src/session-delivery.ts` succeeds; `packages/core/src/session.ts` references `delivery` in `prompt()` at v1.18.10 |
| F2 | The V2 session endpoints are defined in `packages/protocol/src/groups/session.ts:205-355`: `POST /api/session/:sid/prompt` (accepts `delivery`), `POST /api/session/:sid/interrupt`, `GET /api/session/:sid/event` (SSE) | Verified at v1.18.10: `HttpApiEndpoint.post("session.prompt", "/api/session/:sessionID/prompt", { payload: { prompt, delivery: SessionInput.Delivery.pipe(Schema.optional) } })` (line 205-210); `session.interrupt` at line 345 |
| F3 | **The V2 `Api` group IS mounted and served by the shipped binary.** `packages/opencode/src/server/routes/instance/httpapi/server.ts:74` imports `Api` from `@opencode-ai/server/api`; `:177-180` mounts it as `serverRoutes` with `v2SchemaErrorLayer`; `:281` includes it in the served app. Verified at both v1.18.10 and HEAD. | `git show v1.18.10:packages/opencode/src/server/routes/instance/httpapi/server.ts` — line 74, 177-180, 281 |
| F4 | **The TUI already uses the V2 prompt route today.** `sdk.client.session.prompt` (called by the TUI submit at `packages/tui/src/component/prompt/index.tsx:1093`) is generated to POST `/api/session/{sessionID}/prompt` with the `delivery` field. | `packages/sdk/js/src/v2/gen/sdk.gen.ts:5622-5647` — `public prompt(...)` posts to `/api/session/{sessionID}/prompt` with `delivery?: "steer" \| "queue"` |
| F5 | V2 routes share the same `ServerAuth` Basic-auth layer as V1 (`serverHttpApiAuthLayer` at `server.ts:179`), so the proxy's existing `opencode`+password Basic auth works against `/api/session/...` unchanged | `server.ts:177-180` — `serverHttpApiAuthLayer` is applied to `serverRoutes` (V2) the same way `httpApiAuthLayer` is applied to `instanceApiRoutes` (V1) |
| F6 | V2 paths (`/api/session/...`) do not collide with V1 paths (`/session/...`) — both are served concurrently on the same listener | V1 group: `/session/:sid/prompt_async`; V2 group: `/api/session/:sid/prompt` — distinct prefixes |
| F7 | The V2 runner's queue/promotion/interrupt logic is complete for the single-pod model. Unchecked TODOs (`runner/llm.ts:52-53`) concern durable multi-node ownership and status marking — clustering features that do not apply to llmsafespaces' single-pod-per-workspace architecture | `packages/core/src/session/runner/llm.ts:43-91` — header comment; `[x]` items cover the full queue/promotion/interrupt lifecycle |
| F8 | opencode's V2 `interrupt` is non-destructive: kills the running fiber, fails open tools/assistant, but **preserves** admitted-but-unpromoted input rows for `resume` | `packages/core/src/session/run-coordinator.ts:94-101` (`entry.pendingWake = false; Fiber.interrupt`); test `packages/core/test/session-runner.test.ts:1901` (`"preserves durable queued input for a later wake after interruption"`) |
| F9 | The V2 `Prompted` event fires when a queued input is promoted (`promoted_seq` set), providing the "sent" signal the frontend needs | `packages/core/src/session/input.ts:225-231` — `events.publish(SessionEvent.Prompted, ...)` in `publish()` |
| F10 | The current proxy talks V1 HTTP exclusively: `proxy_input.go:169` (`/session/` + path), `proxy_handlers.go:105` (`/session/:sid/prompt_async`), `proxy_handlers.go:636` (`/session/:sid/abort`) | Code-verified |
| F11 | The `msgqueue.Service` Redis queue is consumed by: `EnqueueMessage`, `ListQueue`, `DeleteQueueMessage`, `redirectPromptToQueue`, `drainQueuedMessage`, `AbortSession` (peek+clear), `reconcileSessionState`, and `app.go:1244` (dispose clears pending). ~8 call sites. | `grep -r "queueSvc" api/internal/handlers/ api/internal/app/` |
| F12 | The opencode V2 SDK client generation lives at `packages/sdk/js/src/v2/gen/`; a Go equivalent would be hand-written against the documented `/api/session/...` contract (no Go SDK is generated today) | `packages/sdk/js/src/v2/gen/sdk.gen.ts`; `sdks/go/services.go` currently targets the V1 llmsafespaces API, not opencode's V2 API |
| F13 | **V2 events DO reach the proxy's existing SSE subscription.** The V1 `/event` route is bridged from V2 via `EventV2Bridge.Service` (`packages/opencode/src/server/routes/instance/httpapi/handlers/event.ts:91`) → `events.listen` → `GlobalBus` → SSE. V2 `PromptAdmitted`/`Prompted` appear on `/event` with their base type strings. | `handlers/event.ts:32-40`; `event-v2-bridge.ts` emits to `GlobalBus` |
| F14 | **V2 event wire type strings** (load-bearing for US-63.5): `session.next.prompt.admitted` (admitted, `promoted_seq` null) and `session.next.prompted` (promoted). Durable events also carry a versioned type on the `sync` channel, but the primary `/event` stream uses the base string. | `packages/schema/src/session-event.ts:86-99` |
| F15 | **`resume` is NOT exposed in the V2 HTTP API.** Endpoint enumeration yields: `list, create, active, get, switchAgent, switchModel, prompt, compact, wait, revert.{stage,clear,commit}, context, history, events, interrupt, message`. No `resume`. `session.wait` is a stub returning `OperationUnavailableError`. | `packages/protocol/src/groups/session.ts`; `packages/core/src/session.ts:421-424` (`wait` returns OperationUnavailableError) |
| F16 | **Nothing auto-drains queued input on pod restart.** `runner.run` is triggered only by `execution.wake` (on a new prompt) or `execution.resume`. opencode's startup has no scan for sessions with pending `SessionInput` rows. Durable rows survive in SQLite but strand until the next prompt arrives. | `packages/core/src/session/runner/llm.ts:383-406` (the only `run` entry); no startup hook found via grep |
| F17 | **V2 `prompt` can return 409 `PromptConflictError`** if a caller-supplied `id` collides with an existing durable row; it can also 404 if the session doesn't exist (V1 `prompt_async` auto-creates, V2 does not). The proxy must omit `id` and operate only on existing sessions. | `packages/core/src/session.ts:380-381` (conflict); `:363` (`result.get` dies if not found) |

> **Stress-test note (2026-08-02):** F15-F16 are the epic's largest residual risk and motivated US-63.9 (stranded-input recovery) and US-63.10 (fresh-load queue visibility). F13-F14 de-risk US-63.5. F17 corrects US-63.2's `id` handling.

---

## Architecture Decision

### Move the queue inboard by adopting opencode's V2 session API over HTTP

The bundled opencode binary already serves `POST /api/session/:sid/prompt` (accepting `delivery: "queue"`) and `POST /api/session/:sid/interrupt`. The proxy will POST `delivery: "queue"` prompts directly to opencode, which handles admission, promotion, and draining atomically in its own SQLite-backed event log. The external Redis queue, drain loop, message-id hack, 409 guard, and destructive-abort machinery are deleted.

### Why now / why not contribute `delivery` to V1?

Not needed. The V2 endpoint already accepts `delivery` and is already mounted (F2-F4). Adding delivery to the V1 runner would require invasive changes to the V1 state machine — swimming against the tide of the upstream project's V1→V2 migration. There is nothing to upstream; this epic is entirely llmsafespaces-side.

### V2 alongside V1 (not replace)

The V2 API uses a different path prefix (`/api/session/...` vs `/session/...`) and a different event taxonomy. The proxy migrates only session prompt and interrupt to V2; everything else stays on V1. No V1 consumer is touched.

### Loss of revoke is accepted

opencode's V2 `SessionInput` has no revoke/delete on admitted-but-unpromoted rows. Abort becomes non-destructive (stops work, queue survives, `resume` re-runs). This is accepted as a UX improvement — it matches user expectations of "stop" in most tools. The `DELETE /sessions/:sid/queue/:messageId` endpoint is deprecated.

---

## Scope

### In Scope
- **Verification spike** (US-63.1): confirm the bundled opencode binary serves the V2 endpoints end-to-end (prompt with `delivery: "queue"`, interrupt, event stream, resume-on-restart behavior) before building against them
- **Proxy migration** (US-63.2–63.4): V2 session client, enqueue path (`delivery: "queue"`), abort path (non-destructive `interrupt`)
- **SSE event bridge** (US-63.5): map V2 `PromptAdmitted`/`Prompted` events (wire types `session.next.prompt.admitted` / `session.next.prompted`) to existing `queue.update` SSE taxonomy
- **Frontend simplification** (US-63.6): derive queue state from events, remove drain-on-idle
- **Stranded-input recovery** (US-63.9): ensure queued input drains after pod restart despite V2 having no `resume` HTTP endpoint and no startup auto-drain
- **Fresh-load visibility** (US-63.10): preserve queue-pill visibility on page load despite V2 having no list endpoint in 1.18.10
- **Legacy deletion** (US-63.7): delete `msgqueue`, `drainQueuedMessage`, message-id hack, 409 guard, stranded-queue sweep — **gated on US-63.9**
- **SDK coordination** (US-63.8): align Epic 62 US-62.6 with the V2 model

### Out of Scope (Deferred)
- **Mid-turn steering** (`delivery: "steer"`): the V2 API supports injecting mid-turn, but this epic defaults to `delivery: "queue"` to preserve current UX. Steering is a future enhancement once the V2 path is stable.
- **Full V2 migration**: only session prompt and interrupt migrate to V2. All other endpoints (message, history, events, question, permission, model, etc.) remain on V1.
- **Durable multi-node ownership** (opencode V2 TODO `runner/llm.ts:52`): clustering is irrelevant to llmsafespaces' single-pod model.
- **Upstream `SessionInput.revoke`**: not pursued; non-destructive abort is accepted.

---

## Story Map

```
US-63.1 (Verification spike: V2 endpoints work) ──────────┐
   │  de-risks the epic; settles F13-F17                   │
   ▼                                                       │
   ┌── US-63.2 (Proxy V2 session client; OMITS id) ────┐   │
   │                                                    │   │
   │   ┌── US-63.3 (Enqueue: delivery:"queue") ─────┐   │   │
   │   │   ── removes 409 guard, redirectPromptToQueue   │ │
   │   │                                              │   │ │
   │   ├── US-63.4 (Abort: non-destructive interrupt)┤   │ │
   │   │   ── removes PeekAll+Clear, flushAndAbortAfterIdle  │
   │   │                                              │   │ │
   │   └── US-63.5 (SSE bridge) ────────────────────┘   │   │
   │                                                    │   │
   │   ┌── US-63.9 (Stranded-input recovery) ◄── CRITICAL GATE FOR 63.7   │
   │   │   upstream resume endpoint OR proxy-side wake                  │ │
   │   │                                                                │ │
   │   └── US-63.10 (Fresh-load queue visibility) ◄── GATE FOR 63.7     │ │
   │                                                                    │ │
   └── US-63.6 (Frontend: derive queue from events) ─────────────────────┘ │
                                                             │
   US-63.7 (Delete legacy) ◄── gated on 63.3, 63.4, 63.6, 63.9, 63.10
   US-63.8 (SDK coordination with Epic 62)
```

---

## Stories

| ID | Title | Priority | Effort | Depends On |
|----|-------|----------|--------|------------|
| [US-63.1](US-63.1-verification-spike.md) | Verification spike: confirm bundled opencode serves V2 prompt+interrupt end-to-end | Critical | S (1 day) | — |
| [US-63.2](US-63.2-proxy-v2-session-client.md) | Proxy: V2 session client (`prompt` with `delivery`, `interrupt`) | Critical | M (3 days) | US-63.1 |
| [US-63.3](US-63.3-enqueue-delivery-queue.md) | Enqueue path: POST prompt with `delivery: "queue"`; remove 409 guard | Critical | M (3 days) | US-63.2 |
| [US-63.4](US-63.4-abort-nondestructive-interrupt.md) | Abort path: non-destructive `interrupt`; remove queue clear + flushAndAbort | High | S-M (2 days) | US-63.2 |
| [US-63.5](US-63.5-sse-event-bridge.md) | SSE bridge: map V2 `PromptAdmitted`/`Prompted` → `queue.update` events | High | M (3 days) | US-63.2 |
| [US-63.6](US-63.6-frontend-queue-from-events.md) | Frontend: derive queue state from events; remove drain-on-idle | High | M (3 days) | US-63.5 |
| [US-63.7](US-63.7-delete-legacy-queue.md) | Delete legacy: `msgqueue`, `drainQueuedMessage`, message-id hack, 409-requeue, stranded-queue sweep | Normal | S (1 day) | US-63.3, US-63.4, US-63.6 |
| [US-63.8](US-63.8-sdk-coordination.md) | SDK coordination: align Epic 62 US-62.6 queue methods with V2 model | Normal | S (1 day) | US-63.3 |
| [US-63.9](US-63.9-stranded-input-recovery.md) | Stranded-input recovery: expose/use a resume trigger so queued input drains after pod restart (upstream `resume` endpoint OR proxy-side wake) | **Critical** | M (3 days; includes upstream PR if chosen) | US-63.3 |
| [US-63.10](US-63.10-fresh-load-queue-visibility.md) | Fresh-load queue visibility: keep a list endpoint or accept the gap (no V2 list endpoint in 1.18.10) | High | S (1 day) | US-63.5 |

**Total estimated effort:** ~21 days (one engineer) or ~3 weeks (two engineers parallelized). No upstream PR cycle for the core migration; US-63.9 may add a small upstream `resume` exposure PR.

---

## Parallelization Plan

```
Week 1:  US-63.1 (spike — settles all risk including F15-F17 in a day)
         US-63.2 (V2 client; omits id)  [starts as soon as spike confirms]
Week 2:  US-63.3 (enqueue) + US-63.4 (abort) + US-63.5 (SSE bridge)  [parallel; all need 63.2]
         US-63.9 (stranded-input recovery — START EARLY; has the only real upstream PR)  [needs 63.1]
         US-63.10 (fresh-load visibility decision)  [needs 63.5]
Week 3:  US-63.6 (frontend)  [needs 63.5, 63.10]
         US-63.8 (SDK coordination)  [needs 63.3]
Week 4:  US-63.7 (legacy deletion)  [HARD GATE: needs 63.3, 63.4, 63.6, 63.9, 63.10]
         Feature flag removed; cutover complete
```

US-63.9 should start in week 2 (not week 4) because (a) it carries the epic's only real upstream dependency if Option A is chosen, and (b) US-63.7 cannot delete the stranded-queue sweep until it lands. Starting it late blocks the deletion and extends the epic by a week.

---

## Acceptance Criteria (Epic-Level)

- [ ] Bundled opencode binary (1.18.10) confirmed serving `POST /api/session/:sid/prompt` accepting `delivery: "queue"` and `POST /api/session/:sid/interrupt` (US-63.1 spike report)
- [ ] `EnqueueMessage` and `SendPromptAsync` POST to the V2 prompt endpoint with `delivery: "queue"`; opencode handles admission and draining internally
- [ ] The 409 guard (`SendPromptAsync` conflict check) is removed — opencode admits atomically
- [ ] `AbortSession` proxies to `session.interrupt` (non-destructive); queued messages survive and run on resume
- [ ] `flushAndAbortAfterIdle` is deleted; `PeekAll`+`Clear` on abort is deleted
- [ ] Frontend `queue.update` events (`enqueued`/`sent`) are derived from V2 `PromptAdmitted`/`Prompted` events; no behavioral change to the queue pill UX
- [ ] `api/internal/services/msgqueue/` is deleted in its entirety; no Redis queue dependency remains
- [ ] `generateOpencodeMessageID` is deleted; the message-id hack comment block (`service.go:27-86`) is gone
- [ ] `drainQueuedMessage`, `redirectPromptToQueue`, `reconcileSessionState` stranded-queue sweep are deleted
- [ ] `ForceAbortSession` (Epic 44 admin escape hatch) is retained and unaffected
- [ ] All existing `proxy_queue_test.go`, `proxy_queue_drain_miss_test.go` tests are updated or replaced to cover the V2 paths
- [ ] Feature flag (`LLMSAFESPACE_V2_SESSION_QUEUE`) controls the rollout; default off, enabled per-deployment

---

## Risks & Mitigations

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| **Stranded queued input after pod restart** — deleting the stranded-queue sweep (US-63.7) without a replacement leaves durable `SessionInput` rows undrained until the next user prompt | **High** (F15-F16: no `resume` HTTP endpoint, no startup auto-drain) | **High** (silent data stuck; user must know to type something) | US-63.9 ships a replacement drain trigger (preferred: upstream `resume` endpoint) **before** US-63.7 deletes the sweep. US-63.7 is hard-gated on US-63.9. |
| **Fresh-load queue invisibility** — deriving pills from SSE events leaves a user who loads a session with pre-existing queued messages blind to them | **High** (no V2 list endpoint in 1.18.10) | Medium (UX gap for multi-session users) | US-63.10 picks a mitigation (proxy-side shadow marker recommended) before US-63.7 deletes `GET /queue`. |
| **V2 endpoint behavior differs from the V1 path in an untested way** (e.g., prompt returns different status, interrupt semantics differ from abort) | Low (F4: the TUI exercises prompt daily; F8: interrupt is unit-tested in opencode's suite) | Medium | US-63.1 spike settles this in a day against a real kind workspace. Acceptance criteria include e2e: enqueue-while-busy, enqueue-while-idle, abort-while-queued, abort-while-processing, **OOM-restart-drain**. |
| **V2 event taxonomy differs from V1** — the SSE event names/shapes change, breaking the frontend | Medium | Medium | US-63.5 translates V2 events (wire types `session.next.prompt.admitted` / `session.next.prompted`, F14) to the existing `queue.update` taxonomy at the proxy boundary. **Verified**: V2 events reach the proxy's existing `/event` subscription (F13). |
| **V2 prompt returns a streaming response where V1 `prompt_async` returned 204** — if the proxy buffers the body, it could hang | Low (`session.prompt` is documented as admit-and-schedule; the NoContent success is verified) | High | US-63.1 spike explicitly checks the response status/body. If streaming, the proxy drains/discards the body. |
| **`PromptConflictError` (409) if the proxy sends a caller-generated `id`** | Medium (easy to get wrong) | Medium (silent retry loops) | US-63.2 mandates the client OMITS `id` (F17). |
| **Loss of revoke triggers user complaints** | Low | Low | Release-note the behavior change. Follow-up upstream `SessionInput.revoke` if needed. |
| **Opencode version pinning** — a future opencode bump changes V2 route shapes | Low (V2 API is the forward path upstream; stable by design) | Low | US-63.1 spike runs against the pinned 1.18.10. The `agent_config_writer_schema_test.go` pattern (pinned schema + REFRESH.md) can be mirrored for the V2 session contract. |
| **Feature-flag cutover leaves both paths half-wired** | Medium | Medium | The flag toggles at the `EnqueueMessage`/`SendPromptAsync`/`AbortSession` entry points only. US-63.7 (deletion) is gated on the flag being removed in production for one release cycle. |

---

## Non-Goals

- **Replacing V1 entirely.** Only session prompt and interrupt migrate to V2. The rest of the V1 API surface (message, history, question, permission, model, agent, etc.) remains untouched.
- **Mid-turn steering.** `delivery: "steer"` is supported by the V2 API but is not exposed in this epic. All prompts use `delivery: "queue"`. Steering is a follow-up enhancement.
- **Cross-replica queue coordination.** Sessions are pod-pinned; any replica can forward a V2 prompt to the owning pod. The external Redis queue's cross-replica write-buffer property is not replicated — if the pod is unreachable, the prompt fails (same as today's `proxyToWorkspace` behavior for any other request).
- **SDK queue-list endpoint.** The V2 API does not expose a "list pending inputs" HTTP endpoint. The queue list view derives from accumulated `PromptAdmitted` events on the SSE stream. A V2 list endpoint can be contributed upstream as a follow-up if needed.
- **An opencode Go SDK.** The proxy hand-writes a thin V2 HTTP client (US-63.2) against the documented contract; generating/consuming a Go SDK from `@opencode-ai/sdk` is out of scope.
