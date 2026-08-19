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
