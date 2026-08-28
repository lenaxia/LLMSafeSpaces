# opencode 1.18.10 → 1.18.15 bump (V2 queue-drain fixes)

**Date:** 2026-08-27
**PR:** (this PR)
**Scope:** runtime pin only — `runtimes/base/Dockerfile` ARG + `local/bootstrap.sh` default. No API/proxy/adapter code changes.

## Why 1.18.15

The outbox delivery send is synchronous turn-to-completion (`adapter.Send` →
V1 `POST /session/:id/message`) because the V2 queue path has been dormant
since #755: on 1.18.10, `POST /api/session/:sid/prompt` with
`delivery:"queue"` admits prompts but the queue never drains correctly.

Upstream root cause: the legacy message loop ordered messages by raw ID lex
order (`lastUser.id < lastAssistant.id`) — upstream #40990 "order legacy
message loop by time" and #40991 "use chronological message boundaries"
(Aug 7 2026) switch to `time.created` ordering and a structural exit
predicate (`lastAssistant.parentID === lastUser.id`). Both first ship in
**v1.18.15**.

## Empirical validation (local mock provider, both binaries)

Method: `opencode serve` for each version with an OpenAI-compatible mock
provider (2.5s turns); Basic auth `opencode:<pw>`; V1 sync send to prime the
session model; then V2 queue prompts via `POST /api/session/:sid/prompt`.

| Check | 1.18.10 | 1.18.15 |
|---|---|---|
| V1 sync send + history | works | works (same shapes) |
| V2 queue admission | 200 + admittedSeq | 200 + admittedSeq |
| V2 queue drain | **broken**: loop re-persists and re-answers the stale prior message; queued texts never land | **correct**: FIFO, exact texts, turns complete |
| SSE on V1 `/event` | `message.*`/`session.*` | identical type strings + new `step-start`/`step-finish` (harmless superset) |
| `packages/schema` diff | — | **zero commits** v1.18.10..v1.18.15 (event + config contracts frozen) |
| V1 HTTP routes diff | — | untouched (only web-UI blob/5xx-logging fixes) |

## Full-range commit triage (all 114, v1.18.10..v1.18.15)

Every commit enumerated and classified; 106 are opencode.ai
website/console, desktop/web app UI (RTL/i18n/diff-viewer), TUI, docs,
website model content, tests, and `chore: generate` — zero platform
surface. The 8 platform-relevant commits:

| Commit | Change | Platform impact |
|---|---|---|
| #40990 | loop orders by `time.created`, structural exit predicate | **the #755 drain fix** |
| #40991 | chronological boundaries in session/revert | same family; correctness |
| #40987 | truncation cleanup by file mtime (not ID-timestamp parse) | internal housekeeping; robustness (same ID-ordering family) |
| #40800 | serialize orphaned compaction history | `/compact` correctness; no wire change |
| #40718 | patch: @ai-sdk/openai-compatible preserves stream errors | relay/custom openai-compatible providers surface stream errors instead of swallowing — **the platform relay benefits** |
| #40707 + #40694 | broadened retryable-error patterns (rate/5xx/network/timeout regexes) | fewer failed turns on transient provider errors; retry budget respected. **Relay path benefits directly** |
| #39556 | provider config: wider interleaved-reasoning field + regenerated SDK openapi/types | additive optional config; configwriter unaffected. **Precision note: `packages/sdk/openapi.json` DID change (additively) — the "schema frozen" claim is scoped to `packages/schema` event contracts** |
| #39697 | patch: MCP SDK stops SSE reconnect loops | MCP transport stability (platform's llmsafespaces MCP server) |

No blocking findings; two precision notes folded into this worklog.

## The hazard window: v1.18.16–v1.18.18 must be skipped

A projector/DB change in that window breaks v1-format session databases;
fixed only in v1.18.19 (upstream #42444 "preserve v1 database
compatibility"). Our pod session volumes persist across suspend/resume and
were created on v1-format opencode — bumping into that window puts every
existing session through the broken migration. The Dockerfile comment now
documents this; any later bump jumps .15 → .19+ in one hop.

## Residual findings for the future V2 revival (NOT this PR)

Validated on 1.18.15, scoped for a follow-up design doc:

- **Store split**: V2-queue messages persist only in the V2 store
  (`GET /api/session/:sid/message`); the V1 history endpoint never sees
  them (no backfill after later V1 sends). A session mixing V1-sync and
  V2-queue delivery shows a split transcript.
- **Taxonomy split**: V2-queue turns emit only `session.next.*` events
  (admitted/prompted/step.*/text.*) over `/event` — no
  `message.part.updated`/`message.part.delta`.

Consequence: reviving `SendAsync` in `outboxDeliver` requires (a) V2 history
reads for the transcript and `VerifyDelivery` (a false-absent verify against
the V1 view would re-send and duplicate turns), and (b) `session.next.*`
translation in the SSE contract bridge. Deliverable as one flag-gated epic;
do NOT ship the delivery switch alone.

## Rollout

- Runtime image rebuild picks up 1.18.15 on next `make`/bootstrap; existing
  workspaces get it on pod recreation. No data migration (v1.15→v1.18.15
  storage format unchanged — `.16+` is where formats moved).
- Rollback: revert the ARG; images are tagged per build.
