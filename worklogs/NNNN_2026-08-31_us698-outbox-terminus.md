# Worklog: US-69.8 — outbox terminus switch (inline-first, flag matrix, freeze armed)

**Date:** 2026-08-31
**Session:** Epic 69 (#1134) US-69.8 (#1142, design 0055 M2/M4 + D1-B): the outbox's delivery terminus switches to the agentd ledger — `agentdDeliverer` (POST `(entryID, attempt)` + inline poll, I10 state mapping in ONE function, prior-attempt resolution so admitted is never re-POSTed), the M4 flag matrix (illegal authority-on/V2-off rejected at boot), `SetAgentdTerminus` wiring in app.go, and **the S2 schema freeze armed** (`abi/FROZEN` = e58cecd9; `make abi-breaking` via a baseline worktree, proven to catch a deleted RPC).
**Status:** Complete (in-repo core). The text-scan oracle is bypassed on this terminus by construction (the deliverer never touches it); its deletion + `v2Shadow`/`v2Pending` retirements + the flip/rollback drills are the flagged-window work tracked for US-69.11/69.13 (display keeps working either way per the issue's scope note). Cluster-bound ACs (p99 on target storage, rollback-under-load drill) ride the staged pool.

---

## Objective

Delivery = POST/poll, display = stream (D1-B): the outbox delivers via the pod's ABI surface; the ledger is the only truth source the outbox consults; the illegal flag combination cannot boot; the schema stops moving (D5's irreversible commitment at S2 entrance).

---

## Work Completed

### The terminus deliverer (`api/internal/handlers/outbox_terminus.go`)
- `agentdDeliverer`: **inline-first** — POST `.../deliveries` with (entryID, attempt), parts `[text]` (D3 join happens agentd-side; the deliverer sends the entry's text as one text part), model override; then poll `GetDeliveryStatus` to a completion-eligible state within 10s (100ms cadence). Timeout-with-ledgered is a RETRYABLE error (never ambiguous — the ledger owns truth; the entry stays delivering while agentd's own retry loop drives admission).
- **I10 mapping in exactly one function** (`completionFor`): admitted/promoted/turn-ended/stalled complete (the latter three strictly imply admitted); ledgered polls; failed re-arms.
- **Prior-attempt resolution (I5/I6)**: on an outbox retry (`Attempts > 0`) the deliverer looks up the PRIOR attempt before any POST — completing without re-POST when admitted-or-later, polling (never re-POSTing) when still ledgered, re-arming at attempt+1 only on failed/not-found.
- Connect-protocol JSON transport (envelope decode, typed error propagation) — dependency-light, zero generated-code coupling in this path.
- Text-scan oracle: **not consulted anywhere on this terminus** (its deletion is mechanical follow-up in US-69.11's window).

### Flag matrix + wiring (M4/D4)
- `ValidateDeliveryFlags`: authority-on + V2-off → boot FATAL ("single delivery regime").
- `app.go`: `AGENTD_STATE_AUTHORITY` parsed alongside `OPENCODE_V2_DELIVERY`; violation exits non-zero; `proxyHandler.SetAgentdTerminus(true)` when armed.
- `proxy_lifecycle.go`: `outboxDeliver` branches to `agentdTerminusDeliver` (resolve pod IP + password with the proxy's existing resume-safe getters; `agentd.AgentdPort`).

### The S2 schema freeze (D5's irreversible commitment)
- `abi/FROZEN` = `e58cecd9` (US-69.7's merge). `make abi-breaking` materializes the baseline via a git worktree and runs `buf breaking` workspace-against-workspace.
- **Gate proven live**: deleting `GetSnapshot` from the schema → `Previously present RPC "GetSnapshot" on service "HarnessABIService" was deleted` (exit 100); clean tree passes. (First two "gate failed to catch" iterations were my own probe bug — sed targeted a wrong RPC name; no break had been introduced. The third probe with the correct target proved the gate.)
- Freeze marker documented in Makefile + the Epic 65 flip (#1161) referenced as the tracked follow-up (parity test guards meanwhile).

### Tests
`TestAgentdDeliver_InlineFirstAdmission` (POST→poll→admitted; exactly one admission), `TestAgentdDeliver_RetryChecksPriorAttemptFirst` (admitted prior completes with NO attempt-2 row — I6), `TestAgentdDeliver_TimeoutIsLedgeredPoll` (retryable, not ambiguous), `TestAgentdDeliver_FailedAttemptReArms` (attempt+1 admits), `TestFlagMatrix_IllegalComboRejected` (boot error). Stub = real HTTP server speaking the Connect JSON envelope with an async admission driver + scriptable failures.

---

## Key Decisions

1. **Completion-eligible = {admitted, promoted, turn-ended, stalled}**: the later three strictly imply admission happened (M2's table); completing on them avoids an outbox live-lock when the inline window spans promotion.
2. **Ledgered-timeout is retryable, never ambiguous**: the ledger is authoritative — there is nothing to "verify" out-of-band; the outbox's own backoff re-enters and re-polls the SAME attempt.
3. **Prior-attempt-first on every retry**: this is the structural I6 guard on the consumer side — the deliverer cannot manufacture a second turn even if the outbox retries aggressively.
4. **Freeze via worktree, not `git://` refs**: buf's git input needs the ref fetchable in CI without extra auth; a detached worktree of the recorded SHA is hermetic and identical.

## Assumptions (validated)

| # | Assumption | Validation |
|---|---|---|
| A1 | The Connect JSON envelope shape matches the generated clients | stub speaks it; the deliverer decodes it |
| A2 | Pod IP + password resolution works off the proxy's existing getters | reused `getPassword` + CRD Get unchanged |
| A3 | buf breaking over a worktree detects RPC deletion | proven live (exit 100) |

## Blockers

None in-repo. **Epic 65 flip (#1161) still open** — the parity test guards equivalence; the flip remains the tracked single-source-of-truth work before S3's frontend consumption.

## Tests Run

- `go test ./api/internal/handlers/ ./api/internal/app/` — green (108s incl. all prior suites)
- `make abi-check` — freeze gate armed + green; break-probe caught
- `golangci-lint --new-from-merge-base ./api/...` — 0 issues

## Next Steps

1. US-69.9 (#1143): typed actions op (the last S2 story).
2. Retirements window (with US-69.11): delete the text-scan oracle path, `v2Shadow`, migrate `v2Pending` wake; flip/rollback drills (US-69.13).
3. Epic 65 flip (#1161) before S3.

## Files Modified

- `api/internal/handlers/outbox_terminus.go` + `outbox_terminus_test.go` (new)
- `api/internal/handlers/{proxy,proxy_lifecycle}.go` (flag + deliverer branch)
- `api/internal/app/app.go` (flag matrix boot guard + SetAgentdTerminus)
- `abi/FROZEN` (new), `Makefile` (abi-breaking worktree baseline)
