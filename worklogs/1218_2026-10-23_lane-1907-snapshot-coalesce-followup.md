# Lane-1907 — #1907 coalesce de-flake → follow-up hardening (PR #1916)

Date: 2026-10-23 · Lane: lane-1907 (orchestrator novel-nutmeg-verifies) · Branch: fix/1907-snapshot-coalesce-flake (re-cut from origin/main 9ca8232fe) · Issue: #1907 (CLOSED by #1910 while this lane worked) · PR: #1916 · Test-only; no production changes.

## Objective

De-flake TestRequestSessionStatusSnapshot_Coalesces (CI run 39914818351, "got 2"). Executed in two phases after #1910 (another lane) merged mid-flight.

## Phase 1 — independent de-flake (superseded)

Full parked-first-fetch rewrite on main@8a35e5c6b (commit e8843c5f9): four timing-independent nets (parked-phase count, stub-side second-fetch flag, pre-release begin-count drain, post-release verdict), defer-based park release. Verified: 0/63 stress red across GOMAXPROCS 1/2 + race; mutations (neutered guard / early Delete) red 3/3 + 3/3 under `-race`, 0.01 s exits; full handlers -short green; vet/gofmt clean. Bot r1 on PR #1916 confirmed the construction sound and independently reproduced both mutation reds — but #1910 had merged (APPROVED 84c14b4c2, closing #1907) and e8843c5f9 hard-conflicted with main in 3 files (same test function rewritten differently). Verdict: re-propose the carry-over as a minimal follow-up against main's merged version, not via the conflicting duplicate.

## Phase 2 — minimal follow-up hardening (this diff, per r1 prescription)

Two surgical edits to the merged pin (no rewrite of #1910's body):

1. **Defer-based park release** (`releaseOnce` + `defer releaseOnce.Do(close)`; happy path closes inline). Hazard proven on the merged body: any require/fatal firing while flight 1 is parked mid-fetch — the trigger-2 assert, the drain verdict, or the park wait itself — Goexits through the harness's `t.Cleanup(stub srv.Close)`, which waits for outstanding requests; the parked fetch never returns, so the cleanup blocks until go test's 10-minute timeout. A/B with an injected early-fatal: merged body panics `test timed out after 20s` (rc=2); hardened body fails cleanly in 0.01 s (rc=1). Defers run before every t.Cleanup, so park-release strictly precedes stub Close on all exit paths.
2. **Begin-count net in the pre-release drain**: a second (mutant) flight's `snapshot_begin` is published in-process BEFORE its authority fetch (emit: cursor → begin → fetch), so `begins > 1` catches a begin-only second flight even while its fetch has not yet reached the stub — a mutant the merged complete-only drain never sees.

## Verification (phase 2, hardened body)

- Happy path: 10x GOMAXPROCS=1 + 10x GOMAXPROCS=2 single `-short -count=1` invocations: 0/20 red; `-race -count=10`: 0 red; full `./api/internal/handlers/` `-short -count=1`: green (40.3 s); `go vet` + gofmt clean.
- Mutations (production edit → run → restore byte-identical; `-race`, `-test.timeout 60s`):
  - Neutered guard (LoadOrStore result ignored): RED 3/3, begin-count net signature ("got 2 — a second begin"), 0.01 s exits.
  - Early Delete (entry released before flight runs): RED 3/3, begin-count net 3/3 + stub fetch flag 1/3 (independent nets).
- Deadlock A/B (test-side injected early-fatal, evidence only, never committed): old panics timeout, new fails 0.01 s (above).

Commands (representative):

```
PATH="/tmp/opencode/bin:$PATH" GOBIN=/tmp/opencode/bin go test -short -count=1 \
  -run TestRequestSessionStatusSnapshot_Coalesces ./api/internal/handlers/   # xN, GOMAXPROCS 1/2
PATH="/tmp/opencode/bin:$PATH" GOBIN=/tmp/opencode/bin go test -race -short -count=10 \
  -run TestRequestSessionStatusSnapshot_Coalesces ./api/internal/handlers/
```

## Residuals / assumptions

- Unchanged from the merged pin: the microscopic escape window (a second fetch that has neither begun nor been flagged by body end — every observed mutant reddened at begin-publish, strictly earlier); the package-global `busyFlightsInFlight` cross-test stale-entry exposure (false-red only, loud, pre-existing).
- The begin-count net narrows the escape window to a second flight whose begin was published but whose fetch never started within the body — smaller than the merged pin's (fetch-arrival-only) window.
- Assumption: `RequestSessionStatusSnapshot` runs its guard synchronously before the 202 (true of source, :291-293).

## Mutation-verify suggestion for review

Flip `triggerBusySnapshotFlight`'s guard to ignore `LoadOrStore`'s `loaded` (always spawn) — one `-race -count=1` run must redden with the begin-count net ("got 2 — a second begin") in 0.01 s. For the deadlock hardening: inject `t.Fatalf` immediately after the firstFetch wait on the PARENT version and on this one, run with `-test.timeout 20s` — parent panics `test timed out`, this one fails cleanly.
