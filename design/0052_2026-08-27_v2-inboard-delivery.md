# 0052 — V2 Inboard Delivery: admit-and-return for the outbox

**Status:** Proposed (design; implementation gated on 1.18.15 landing — PR #1106)
**Date:** 2026-08-27
**Depends on:** opencode ≥ 1.18.15 (V2 queue-drain fixes #40990/#40991), design 0050 (D3 outbox), Epic 63 (inboard session queue)
**Supersedes:** the "one-line SendAsync swap" notion — empirically disproven below

## Problem

The D3 outbox delivers each entry through the **synchronous** V1 send
(`adapter.Send` → `POST /session/:id/message`), which returns only when
the assistant's whole turn completes. Consequences (all server-side
carrying cost; the *display* was fixed by #1098's picked-up seam):

- Each delivery occupies a worker slot + the per-session lock for up to
  `DeliveryTimeout` (10 min; `LockTTL` 12 min). With the 32-slot
  semaphore, >32 concurrently-draining sessions queue behind each other.
- A connection cut mid-turn leaves the outcome UNKNOWN — the entire
  verify/ambiguous machinery (#987) exists to compensate.

Epic 63 built the platform for opencode's V2 queue
(`POST /api/session/:sid/prompt`, `delivery:"queue"`) whose admission
returns in milliseconds — but #755 found it admits without draining and
the platform fell back to V1 sync. Upstream #40990/#40991 (first in
1.18.15) fix the drain. This design revives the V2 path behind a flag.

## Empirical findings (2026-08-27, both binaries, mock provider)

Reproducible harness: local `opencode serve`, OpenAI-compatible mock
provider, Basic auth `opencode:<pw>` (scripts sketched in worklog for
the 1.18.15 bump PR).

| Check | 1.18.10 | 1.18.15 |
|---|---|---|
| V2 admit (`delivery:"queue"`) | 200 + admittedSeq | 200 + admittedSeq |
| V2 drain | **corrupt** — loop re-persists and re-answers a stale prior message; queued texts never land | **correct** — FIFO, exact texts, turns complete |
| V2 messages in V1 history (`GET /session/:sid/message`) | n/a | **never visible** (no backfill after later V1 sends) |
| V2 turn events on V1 `/event` | `session.next.*` | **only** `session.next.*` — no `message.part.*` |

The two splits are the load-bearing discoveries:

1. **Store split.** V2-queue messages persist only in the V2 store
   (`GET /api/session/:sid/message`, shape `{data:[{type, prompt?, parts?}]}`).
   V1-queue/sync messages persist only in the V1 store. A session mixing
   both shows a split transcript — verified, not theorized.
2. **Taxonomy split.** V2 turns emit `session.next.prompt.admitted`,
   `session.next.prompted`, `session.next.step.started/ended`,
   `session.next.text.started/delta/ended` — the contract bridge's
   `part.*` translation never sees them.

## Non-goals

- Mixed per-session delivery modes. The flag is deployment-wide; a
  session's lifetime must not straddle a flag flip mid-queue. Rollout
  drains queues before flipping (operational step below).
- Upstream changes (contributing a V1-compat projector fix so V2
  messages appear in V1 history). Desirable, out of scope.

## Design

**Flag**: `OPENCODE_V2_DELIVERY` (env → config), default off. Wiring
mirrors the existing flag plumbing (`SetV2QueueShadow`-style setter from
`app.go`; the outbox deliverer and history paths read it at the handler
seam, not inside the adapter).

### 1. Delivery (`outboxDeliver`)

Flag on → `adapter.SendAsync` (V2 admit-and-return) instead of `Send`.
Error classification unchanged: HTTP-status errors are definitive;
transport errors wrap `outbox.Ambiguous`. On admission the entry
completes immediately — `queue.update/sent` fires ~instantly after
pickup, and #1098's `delivering`→`sent` pair collapses to near-zero
spacing. `DeliveryTimeout` drops to 30s and `LockTTL` to 2min under the
flag (admission-scale, not turn-scale).

### 2. History reads (transcript + verify)

Flag on → `adapter.GetHistory`/`GetHistoryPage` serve from
`GET /api/session/:sid/message` with a V2→contract translation
(`type`→role, `prompt.text`→text part, parts pass through the existing
part translation). **`VerifyDelivery` must read the same store** — a
false-absent verify against the V1 view would re-send and duplicate
turns (the exact #987 incident class). One seam, switched together.

### 3. SSE contract translation

The tracker's V1 `/event` subscription already receives
`session.next.*` (verified on both versions). Add translation rules:
`session.next.text.delta` → `part.delta`, `session.next.text.started/
ended` + step boundaries → `part.end`/`part.updated` equivalents for the
frontend contract, `session.next.prompted` → the user-message echo
(replaces the V1 echo the queue path relied on; keep the
`pendingQueuedTexts` strip keyed on the same text).

### 4. Rollout

1. Land on a staging workspace pool with the flag on; run the
   `us-63-v2-behavior-e2e.sh` scenarios (enqueue-while-busy,
   abort-survival, kill-restart drain) — all three must pass.
2. Production: drain all outbox queues (they empty naturally — delivery
   is fast), flip the flag, watch the drift counters
   (`llmsafespaces_agent_events_total` unknown label must stay flat).
3. Rollback = flag off; V1 store sessions keep working (V2-store
   messages remain readable through the V2 history endpoint which
   stays wired).

## Risks

| Risk | Mitigation |
|---|---|
| V2 store loses messages on opencode crash | Durable SQLite (F8); `Recover`-style sweep already exists for the stranded-input class (US-63.9 drain trigger must be re-verified under the flag) |
| Provider runtime differences (ai-sdk vs native) change V2 event shapes | Fixture pair per version (REFRESH.md procedure) extended with a V2-turn capture before flag-on rollout |
| Model selection on V2 sessions | V2 prompt takes per-prompt model overrides (client_v2.go already sends it); verify against mock + real provider |
| Frontend double-render during rollout (both taxonomies live) | Contract bridge emits one canonical stream; the `isReconnectMode` boundary gate suppresses the duplicate |
