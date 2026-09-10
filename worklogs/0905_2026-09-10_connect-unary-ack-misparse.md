# 0905 — 2026-09-10: Connect unary ack misparse (outbox error pills)

## Incident

User report: both messages they sent while a turn was running still show
in the frontend's message queue for session `ses_f73cf5a66ffeEAqSUM7XAUboqE`
(ws `ad8e6669`). Transcript inspection showed both messages **were**
delivered — exactly one copy each, with completed assistant turns. The
messages were never lost; the queue pills were the lie.

## Root cause

The outbox deliverer's hand-rolled Connect client
(`api/internal/handlers/outbox_terminus.go` `post()`) parsed unary
responses as `{"message": {...}}` envelopes. connect-go's unary codec
never emits that shape: a 200 body IS the response message (bare JSON);
errors ride HTTP >= 400 with bare `{"code","message"}`. Every real
Deliver ack therefore failed as `empty message envelope` while the
agentd ledger held LEDGER_STATE_ADMITTED — the API recorded
`status:error` and retried; each retry's GetDeliveryStatus failed the
same parse, so I6 prior-attempt resolution was skipped and the entry
re-POSTed (agentd's ledger idempotency per (entryId, attempt) absorbed
the re-POSTs; no duplicate turns). Same parser bug in the typed-actions
edge (`proxy_actions.go` `abiAct()`).

Production proof (probed against live agentd :4097):
- `GetDeliveryStatus` for all 5 attempts of both stuck entries → HTTP 200,
  `{"entryId","attempt","state":"LEDGER_STATE_ADMITTED"}` — bare body.
- not-found probe → HTTP 404, `{"code":"not_found","message":...}`.

Why the wire was never seen before shipping: the unit stubs encoded the
wrong envelope (`writeJSONBody` claimed "the generated clients decode
{"message": {...}}" — they do not), and the pool e2e rows assert
transcript arrival (which worked), never queue clearance through a real
connect server. `pkg/abi/abitest` — a real generated handler over HTTP —
existed the whole time and was never wired into these tests.

## Fix (v0.28.2, PR #1308)

- Both call sites parse the unary shapes: bare JSON on 200;
  `{"code","message"}` on >= 400 (mapped to `connectCodeError` in the
  actions edge so `mapConnectError` works against real servers).
- Wire-shape pins against the REAL generated handler
  (`abiconnect.NewHarnessABIServiceHandler` over `abitest.Server`, plus
  a new `SetDeliveryState` test driver): Deliver-ack parse, LEDGERED
  window-timeout, 404 lookup, Act success + unimplemented. The pins
  reproduce the exact production error string against the old parser
  (verified via stash before merging).
- Stubs in terminus/actions tests now emit the real shapes.

## Production

- v0.28.2 tag → Release workflow green (23m) → digests:
  agentd `sha256:aa4173d3…`, opencode `sha256:52db7fe7…`.
- talos-ops-prod #2448 (five images + both overlay digests) merged;
  Flux rolled; `deploy/llmsafespaces-api` at `api:0.28.2`, Ready.
- Stranded rows re-armed (Valkey `status:error`→`pending`): all four
  (two per session on `ad8e6669`) completed via the ADMITTED prior
  attempt and left the queues — `outboxq:*` for both sessions now len 0.
- Post-rollout API logs: zero `empty message envelope` occurrences.

## Open notes

- The stranded-queue sweep only re-arms `lastErrUnverifiable` rows;
  this incident's error class needed manual re-arm. If the ack-parse
  class ever recurs, the sweep predicate is the place to generalize.
- The drain logs nothing per successful delivery (by design), so
  queue-length (Valkey) is the observable for verification.
