# Worklog: #1315 close-out — legs 7/8 per-entry cardinality rows with mutation-honest fault surfaces

**Date:** 2026-09-13
**Session:** The last open item of #1315's test plan: fault legs 7/8 in the #1312 harness "asserting transcript cardinality per entry, not per attempt" (agent: opencode-vesper; closes the 0a stream)
**Status:** In Review

---

## Objective

Close #1315 by delivering the fault-leg rows its test plan demands, with teeth against the exact regression classes the incident rode.

## What already existed (2b's #1337 + #1352)

`TestRow_Leg7_OutOfOrderDelivery_S2_S9` (order-independence, shared-evidence resolution) and `TestRow_Leg8_SlowBoundary_S9_L5` / `TestRow_Leg8_TimeoutLadderRePOST_S2` (slow-inside-window; timeout-then-re-POST). Gap analysis: all three drive `InstantAdmitter`-class fakes whose upsert is keyed UNCONDITIONALLY — a key-send regression in the authority leaves them green (the cardinality assertion is vacuous against it), and none models the incident's decisive mechanism: **the write lands in the transcript while the client's outcome is lost**.

## What this adds

1. **`KeyedAdmitter`** — the G1-probed opencode 1.18.15 wire, faithfully: a keyed POST upserts (same key ⇒ one message); a **keyless POST appends** (the pre-0a shape); a `Hang` channel models the boundary-slow turn **incident-faithfully: the write lands, THEN the response hangs** — the client's ctx aborts without learning the outcome.
2. **Leg 7 row** (`TestRow_Leg7_DuplicateOutOfOrderDeliver_S2`): original send → same-attempt duplicate replay → attempt+1 re-drive after admission. Asserts per-entry cardinality == 1 AND `Writes() == 1` (the replays resolve before POSTing — the idempotency observable the existing rows cannot see).
3. **Leg 8 row** (`TestRow_Leg8_HungTurnReDrive_S2_L5`): the write lands, outcome lost, the entry re-driven at attempt+1 mid-hang — the sixteen-copy loop, miniature. Asserts cardinality == 1, `Writes() == 1` (the evidence check ends the re-POST loop), and L5 convergence of the stranded rows.

## Mutation verification (the teeth, run before reverting)

With `harnessMessageID` mutated to return `""` (the keyless wire):
- **Leg 8 row: RED** — `map[L5:1 S2.cardinality:1 S2.single-write:1]` (every ladder retry appends; the loop is the incident reborn).
- The existing leg-7/8 rows: **stay green** — confirming the vacuity gap this PR closes.
With the mutation reverted: all rows green; full faultmatrix package `-race` green (41s).

Also learned and documented in the rows: the re-drives SERIALIZE on the session single-flight lock (an early `Blocked() >= 2` gate was unreachable by design — the incident's copies were sequential ~180s apart, and the mutation teeth are temporal: the next retry's evidence check is what the key buys).

## Key Decisions

- The write-then-hang ordering (not hang-then-write) is the incident's mechanism: the transcript held each copy while the ledger stayed LEDGERED.
- `Writes()` counts completed POSTs — the cheapness/idempotency observable distinct from cardinality (the upsert could hold cardinality at 1 while still burning re-POSTs; the single-write pin forbids that too).

## Tests Run

`go test -race ./cmd/workspace-agentd/faultmatrix/` — green (41s), incl. the four pre-existing leg-7/8 rows and both new rows; mutation-red demonstrated as above.

## Next Steps

- PR → review → merge closes #1315 (every test-plan item then delivered: root cause + repro + unit + regression rows + fault legs 7/8 per-entry).

## Files Modified

- `cmd/workspace-agentd/faultmatrix/faultmatrix.go` — KeyedAdmitter (keyed/keyless/lost-outcome-hang)
- `cmd/workspace-agentd/faultmatrix/faultmatrix_test.go` — the legs 7/8 rows
- `worklogs/NNNN_2026-09-13_1315-legs-7-8-cardinality-rows.md` (this file)
