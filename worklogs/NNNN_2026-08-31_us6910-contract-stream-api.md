# Worklog: US-69.10 part 1 — the API-proxied contract stream (on-demand pod Events proxy + SSE edge)

**Date:** 2026-08-31
**Session:** Epic 69 (#1134) US-69.10 (#1144, design 0055 S3/D1-B), **part 1 of 2**: the API-side endpoint the frontend hard cutover consumes — `GET /workspaces/:id/contract-events`. One refcounted pod `Events` connection per workspace while ≥1 browser subscriber is attached (scale-to-zero on last detach), fanning **raw StreamFrames** out as protojson SSE. Browsers run the stamped-snapshot client rule themselves; this proxy forwards frames, it never folds.
**Status:** Part 1 complete (in-repo). **Part 2 — the frontend hard cutover — remains**: the shared session provider on contract events (TS port of the discard rule), the I12 entity-ID stitch, reconnect e2e, and the old-dialect deletion. Scoping notes below.

---

## Work Completed

### `api/internal/services/contractstream` (new)
- **Manager**: per-workspace refcounted upstream + fan-out. Lifecycle invariants (all test-pinned, race-tested ×3): ONE pod connection shared by subscribers; **scale-to-zero on last detach**; **snapshot-first enforcement** (a violating first frame forces reconnect); **reseed → reconnect → the SAME subscribers get the fresh snapshot** (the protocol's own resync — no browser round trip); slow consumers dropped (buffer drained → `Resync` sentinel → close) without blocking anyone else. Reconnects pause 1s (a dead pod can't busy-loop the API); resolve re-resolves the pod per (re)connect (A7 resume-safe).
- **ConnectStream**: the default frame source — the generated connect client's `Events` op behind the §D1 Basic-auth transport; receives on one goroutine (connect streams aren't goroutine-safe), ctx-bounded.
- Two real bugs the tests caught: the cancel channel captured per-Subscribe-closure (nil for joiners → last-detach panic — moved to the stream struct), and the drop path racing fanout (both now serialize on the stream lock; drop drains stale frames so `Resync` is what the client sees next).

### `api/internal/handlers/proxy_contract_stream.go` (new)
- `ContractEvents`: typed 501 off-regime (D4), SSE headers, protojson `data:` frames (camelCase — byte-compatible with the protoc-gen-es TS types), `event: resync` named event for dropped consumers, 25s heartbeats, write deadlines. Test seam for the manager.

### Route + OpenAPI
- `GET /workspaces/:id/contract-events` registered; `sdks/openapi.yaml` documents the wire (the router-contract test's gate).

## Tests

- `contractstream/manager_test.go` — the six lifecycle invariants above, race ×3.
- `proxy_contract_stream_test.go` — flag-off typed 501; the SSE wire (snapshot-first, `"atSeq":"7"`/`"seq":"8"` protojson).
- Full `api/internal/handlers` (108s) + lint 0 issues.

## Part 2 scope (the frontend hard cutover — next)

The scoping pass found: the old dialect's dispatch lives in `ChatPage.tsx` (~1350 lines: `handleSSEEvent`/`handleContractEvent` + the reconnect/echo-stitching heuristics) and `SessionActivityProvider.tsx` (~939); the transport swap is trivial (`useEventStream` → `/contract-events`), the generated TS types exist unused (`frontend/src/abi/`), and Playwright's route-mocking pattern extends to the new endpoint. The work: a shared session provider implementing the discard rule + I12 stitch by entity ID (deleting the timestamp sort in `useMessageHistory` and the 15s reconnect window/boundary-gate stack), mapping every old envelope type (queue pills, agent.question/permission, agent_died, resync) to snapshot fields or contract events, then rewriting the ~6 ChatPage suites + the S3 e2e set (`client_discard_rule_unit_property`, `midturn_reconnect_e2e`, `standing_question_reconnect_e2e`, `api_rolling_deploy_e2e`).

## Files Modified

- `api/internal/services/contractstream/{manager.go(new),manager_test.go(new)}`
- `api/internal/handlers/{proxy_contract_stream.go(new),proxy_contract_stream_test.go(new)}`
- `api/internal/server/router.go`, `sdks/openapi.yaml`
