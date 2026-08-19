# Worklog: #939 — agentd usage parser + metering decode onto the wire seam

**Date:** 2026-08-19
**Session:** Complete issue #939 (stacked on the #938 wire seam): migrate agentd's per-step usage parsing onto `pkg/agent/opencode/wire` (fixing the dead `session.next.step.ended` case — agentd now also handles the live 1.18.x step-finish shape it was blind to) and route the SSE tracker's metering decode through the seam. Busy/idle classification stays in agentd; billing dedup/delta policy stays in the tracker.
**Status:** Complete

---

## Objective

Eliminate two of the five independent opencode parser surfaces (worklog 0743 theme #1, issue #747): agentd's `handleStepEnded` and the API tracker's `handleSessionUpdated` inline decode. Both become consumers of the single wire seam landed in #938.

---

## Work Completed

### 1. wire: session-level usage decode (metering attribution)

- `SessionUsage{SessionID, ModelID, ProviderID, InputTokens, OutputTokens, CostUSD, CostMalformed}` — session-level CUMULATIVE usage from `session.updated` events.
- `IsSessionUpdated` (suffix-tolerant: `session.updated(.N)`) + `ParseSessionUpdated` (envelope) + `ParseSessionUpdatedProps` (pre-stripped properties — the tracker's dispatch and agentd's nested path hold props).
- Decode rules preserved from the old inline parser: model prefers `providerID` falling back to `provider`; cost accepts bare number OR object whose `cost` field is dollars (`total` is a token count — never cost); **session identity is `info.id`, not `properties.sessionID`** (the metering path's pinned contract — empty info.id must decode ok with empty SessionID so the caller warns+skips).
- Malformed cost (neither number nor object) sets `CostMalformed` + decodes as 0 — warn-and-bill-at-zero preserved, now as data instead of an inline branch.

### 2. agentd session_tracker migration

- `processEvent` usage branch → `wire.ParseStepUsageProps(evt.Type, evt.Properties)` — reuses the already-parsed envelope (part-updates are the dominant stream type; no second envelope unmarshal).
- **Fixes the dead-event blindness**: agentd now captures promptTokens from `message.part.updated` step-finish parts (both namings), not just the legacy `session.next.step.ended` that current opencode never emits. statusz ContextUsed is live-fed again on 1.18.x.
- Legacy nested-payload path (global-SSE-endpoint compat) preserved via the Props variant — pinned behavior kept, not silently dropped.
- `handleStepEnded` deleted (zero remaining callers; the seam owns the decode). Busy/idle (`handleSessionStatus`) untouched byte-for-byte.
- New tests: step-finish part capture (live + suffixed shapes), text parts carry no usage.

### 3. Metering reroute (api/internal/services/sse/tracker.go)

- `dispatchProperties`: `session.updated` exact-match → `wire.IsSessionUpdated` (suffix-tolerant; suffixed types exist on the store surface).
- `handleSessionUpdated`: decode via `wire.ParseSessionUpdatedProps`; the dedup/delta POLICY (cumulative-output keying, input-zeroing after first event, negative-cost clamp) stays inline — policy is not wire knowledge.
- Malformed-cost warn restored at the policy layer (the one behavioral delta the validator caught — old code's inline warn had been silently dropped in the first pass).

### 4. Validator loop

Independent validator: **zero real correctness/billing bugs**; behavior pins verified equivalent (identity=info.id incl. empty-ID warn+skip, dedup/delta math, busy/idle untouched, nested fallback intact). Four minor findings, all fixed: dropped cost-drift warn (→ CostMalformed + tracker warn), agentd double envelope parse (→ Props variant), no wire-layer identity test (→ added, with divergent properties.sessionID vs info.id), stale package doc + dead-API question (→ doc updated; envelope variant kept — API symmetry, golden-test use).

---

## Key Decisions

| Decision | Rationale |
|---|---|
| Props variants alongside envelope variants | Two production callers hold pre-stripped properties (tracker dispatch, agentd nested path); re-marshaling to satisfy an envelope-only API is waste. Symmetric naming, single decode core each. |
| agentd usage map kept (not deleted) | Deletion belongs to the #941 usage-authority cutover (agentd stops feeding statusz ContextUsed when the API becomes sole truth). This PR only fixes the parser ownership + dead shape. |
| `SessionUsage` carries `CostMalformed` as data | The decoder reports what it saw; warn-policy stays with the caller. Library has no logger; a bool keeps the API honest. |

Assumptions validated: nested step.ended support still pinned by test (kept via Props variant); part-updates dominant on live stream (fixture: 162/392 non-delta events); repolint permits agentd→wire import (exact-match semantics + allowlist prefix, validator-verified).

---

## Blockers

None. Stacked on #938 (wire package); merge after it.

---

## Tests Run

- `go test ./pkg/agent/opencode/wire/` — ok (incl. 19-event golden session.updated decode, identity pin, malformed-cost pin)
- `go test ./api/internal/services/sse/` — ok (inference callback, delta, regression pins incl. EmptyID warn)
- `go test -run TestSessionStatusTracker ./cmd/workspace-agentd/` — ok (legacy + nested + new step-finish shapes)
- Full `./cmd/workspace-agentd/` suite — ok (203s)
- `go build`, `go vet`, gofmt — clean

---

## Next Steps

1. #941 usage-authority cutover (gap-fill via GetSession on reconnect → staged deletion of agentd promptTokens, statusz fields, CRD mirror; workspace aggregate from session_index).
2. #940 ContextUsage.Window wiring (session→model→catalog join).
3. #942 drift counters + upgrade runbook + literal lint.

---

## Files Modified

- `pkg/agent/opencode/wire/wire.go`, `wire_test.go` — SessionUsage + ParseSessionUpdated family + tests
- `cmd/workspace-agentd/session_tracker.go` — processEvent migration; handleStepEnded deleted
- `cmd/workspace-agentd/main_test.go` — step-finish shape tests
- `api/internal/services/sse/tracker.go` — metering decode via seam; suffix-tolerant dispatch; malformed-cost warn

---

## Corrections + review round 1 (#947)

The reviewer's blocking finding was correct and important: routing `api/internal/services/sse` directly into `pkg/agent/opencode/wire` violated design 0049's boundary (platform code must import `pkg/agent`, never an implementation package) and escaped repolint only through an exact-match gap — the same gap a previous #939 attempt had already identified and hardened. Fixed by adopting that pattern:

1. **`Adapter.MeteringFromEvent(eventType, props []byte) (*agent.SessionUsage, bool, error)`** added to the seam; `agent.SessionUsage` is platform-owned (the struct moved out of wire's types into the interface package). The opencode adapter implements it via wire. The sse tracker now takes an injected `MeteringDecoder` (wired in app.go from the adapter via `GetAdapter()`); its last agent event-name knowledge is gone.
2. **Repolint prefix-match hardening**: the agent-import rule now flags subpackages of the implementation (`pkg/agent/opencode/wire` et al.), with the seam itself exempt from self-imports. Pinned by two new tests (subpackage-escape caught, seam self-import allowed).
3. **Tracker-layer pins for both behavior changes** (the reviewer's missing-test findings): table-driven suffixed-dispatch test (unsuffixed fires, suffixed fires, `.foo` and status don't) and the CostMalformed policy test (warn + onInference fires with cost 0, never drops).
4. Test helpers (`newTestSSETracker`, `newCapturingTracker`, the billing e2e trackers) now inject the REAL adapter's decoder — existing inference tests exercise the production decode path, not stubs.

### Round 2 (review on a5ba84be): regression I introduced + wiring hardening

The reviewer probe-verified a real regression in my round-1 fix: moving the `IsSessionUpdated` filter inside the adapter made `ok=false` mean both "not a metering event" AND "metering event, no info" — the tracker warned "incomplete billing fields" for **every non-usage event** (4/4 spurious warns vs 0/4 before; part-updates are the dominant stream type). Fixed by making the contract strict:
- `ok=false` ⇒ strictly "not a metering event" → tracker returns silently (pinned by `TestSSETracker_NonUsageEvents_NoWarnNoFire`, table over 4 event types).
- Info-less `session.updated` ⇒ adapter returns `ok=true` + zero-value `*agent.SessionUsage` → routes to the incomplete-fields warn (EmptyID/EmptyModel/ZeroOutput pins survive unchanged).

Wiring pin (the silent-billing-death gap): the decoder wiring moved from a separate app.go step INTO tracker construction (`newSSETracker` — cannot be forgotten at a call site), pinned behaviorally by `TestNewSSETracker_MeteringDecoderWiredAtConstruction`. `GetAdapter` (added round 1 for the app.go wiring) removed — unused, and its placement broke `SetAdapter`'s godoc. Double-warn on decode drift deduplicated (adapter warns with eventType; tracker returns).

Files added/changed in rounds 1-2 beyond the original list: pkg/agent/adapter.go (+SessionUsage, +MeteringFromEvent), pkg/agent/opencode/adapter.go (impl), api/internal/services/sse/tracker.go (decoder injection, contract split), api/internal/handlers/proxy_lifecycle.go (newSSETracker), api/internal/app/app.go (round-1 wiring, removed in round 2), api/internal/handlers/proxy.go (GetAdapter added then removed), pkg/repolint/agent_import.go (prefix-match + self-exemption) + tests, tracker_regression_test.go, sse_billing_e2e_test.go, tracker_test.go helpers, adapter_crosscutting-side helpers.

### Round 3: the contract pins + a claim I made that was false

The reviewer's round-3 mutation testing proved both round-3 behaviors (info-less zero-value routing; the adapter's metering drift warn — now the sole surviving signal after the round-2 dedup) were deletable with every suite green. Pinned:
- `TestAdapterMeteringFromEvent` (table, observed logger): non-metering nil/false silent; info-less zero-value/true; undecodable err + warn asserted by message; full attribution; suffixed type.
- `TestSSETracker_Inference_InfoLessSessionUpdated_WarnsNoFire`: end-to-end warn-not-skip-not-fire.

**Correction — my round-3 commit message claimed an interface-doc change that never landed.** The python edit targeted round-1 text that no longer existed on this branch; `str.replace` no-opped silently and I did not verify before claiming. The reviewer caught it (`git show --stat` vs the claim). Fixed for real in this commit, grep-verified pre-commit: `MeteringFromEvent`'s interface doc now states the MUST-log-before-err obligation; `MeteringDecoder`'s doc mirrors the contract.

Tests added round 3: the two above. Suites: pkg/agent, opencode, wire, sse, handlers — green.

### Round 5 (#949): my merge-resolution error + review fixes

The reviewer caught a real regression I introduced in the #949 main-merge: resolving database.go with `--ours` took the stacked branch's pre-#945 copy — silently reverting main's lib/pq removal (`pq.Array` back at 3 sites, `// indirect` label now wrong). Exactly the failure mode the #938 round-4 reviewer described ("take main's imports"); I applied the right rule to the wrong file set. Fixed by restoring main's version verbatim.

Also: agentd's nested-format fallback now gated on `evt.Type == ""` (flat deltas — the hottest class — no longer pay a third whole-payload parse); store-fixture session.updated rows (81) now decode through the metering path in a golden test (counting wasn't enough); agentd drift-warn path for step-finish-missing-tokens pinned; stale fixture-filename comment + duplicate SetMeteringDecoder call cleaned.

Issue-comment corrections (per review): no seam-side negative-counter clamp exists (clamp is policy-layer, pre-existing); nested-payload tolerance lives in callers, not wire.ParseStepUsage; the "84 golden lines decoded" claim was wrong (they were counted, now actually decoded).
