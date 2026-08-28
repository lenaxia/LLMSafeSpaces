# V2 inboard delivery — admit-and-return outbox (design 0052 implementation)

**Date:** 2026-08-28
**PR:** (this PR)
**Design:** design/0052 (#1108); prerequisite opencode ≥ 1.18.15 (#1106)

## What was built

`OPENCODE_V2_DELIVERY` (default off) switches the outbox from the
V1 turn-blocking sync send to the V2 admit-and-return prompt endpoint:

- **Delivery** (`outboxDeliver`): `adapter.SendAsync`; admission returns
  in ms → entry completes at pickup → `queue.update/sent` near-instant;
  #1098's delivering→sent pair collapses. Outbox bounds shrink to
  admission scale (10m→30s delivery, 12m→2m lock).
- **History + verify** (`WithV2Store`): `GET /api/session/:sid/message`
  with a new translation for the divergent V2 wire shape (top-level
  user `text`; assistant `content[]` with nested tool state — input,
  joined-content output, epoch-ms times; unknown types → Custom valve).
  Delivery and verification share the store by construction — the
  wiring pairs the flags in one place (app.go).
- **SSE**: `session.next.*` → contract (`text.delta`→`part.delta`,
  `text.ended`→`part.end`, `prompted`→user-echo `part.end` — the exact
  signal the frontend's queued-message strip matches on,
  `step.ended`→per-step ContextUsage).

## Review-driven fixes

1. **Error classification gap** (self-caught while addressing review):
   `PromptV2WithModel` returned bare errors for ≥400 — definitive HTTP
   rejections would misclassify as ambiguous. Now multi-wraps
   `agent.ErrHTTPStatus` (sentinels for 409/404 stay addressable).
2. **Test-env store fidelity**: the fake backend's V1/V2 stores were
   unified (persist fed both) — the V2 test env now rebuilds the
   adapter with `WithV2Store`, and the fake serves the V2 envelope
   newest-first from the persisted texts.

## Unhappy-path matrix (review request)

| Scenario | Expectation | Test |
|---|---|---|
| Admission 5xx | definitive: backoff retry (pending, Attempts++), never verifying/error | `admission 5xx is definitive` |
| Admission transport cut | ambiguous: verifying, never blind-retried; later store-read confirms | `admission transport cut` |
| V2 store 5xx during verify | inconclusive: stays verifying — never read as definitive absence | `V2 store 5xx` |
| V2 store malformed body | inconclusive: stays verifying | `V2 store malformed body` |

## Validation

Golden fixture `v2_messages_1_18_15.json` + event payloads captured
LIVE from a local 1.18.15 serve (mock provider; REFRESH.md provenance).
Full opencode + handlers suites, vet, golangci-lint clean.

## Rollout

Per design 0052: staging pool → `us-63-v2-behavior-e2e.sh` scenarios →
production with queues drained → rollback = flag off. No behavior
change with the flag off.
