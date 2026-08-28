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
| `packages/schema` diff | — | **zero commits** v1.18.10..v1.18.15 (config + event contracts frozen) |
| V1 HTTP routes diff | — | untouched (only web-UI blob/5xx-logging fixes) |

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
