# Worklog: Epic 71 / 2b (part 1) — fault-matrix knobs legs 7/8/10 + the reference client's Deliver

**Date:** 2026-09-11
**Session:** Stream 2b kickoff (epic-71 / 2b, #1312 harness — remaining fault-injection knobs). Agent: opencode-kestrel [glm-5.3].
**Status:** In Progress

---

## Objective

Land #1312 change item 1's remaining knob legs (7 duplicate/out-of-order Deliver, 8 boundary slowness, 10 wire-drift corruption) as composable, inspectable abitest controls — test-scaffolding-only, production imports forbidden — plus the reference client surface the harness rows drive (abiclient.Deliver).

---

## Work Completed

### Leg 7 — Deliver call recording (`RecordDeliverCalls` / `DeliverCalls()`)
- Arms append-only recording of every Deliver invocation as `DeliveryCall{EntryID, Attempt}` — duplicates and out-of-order attempts recorded verbatim: the replay sequence IS the assertion surface for S2 (turn uniqueness) and S9 (ack exactly-once). Off by default (existing consumers stay inert); read returns a copy.

### Leg 8 — boundary slowness (`DelayDeliverAck(d)` / `DeliverAckDelay()`)
- The Deliver handler stalls d before answering — the slow-turn shape the 3.5-min delivery poll window and 3-min admission ctx must survive. Honors ctx cancellation (select on timer/ctx — a stalled test can cancel, and a canceled probe never wedges the handler beyond its own request). Zero disarms; other ops stay instant (pinned: Act untouched).

### Leg 10 — wire-drift corruption (`CorruptNextResponse(suffix, mode)` / `CorruptNextResponseArmed()`)
- One-shot, procedure-scoped (path-suffix match). Modes: InvalidJSON (truncated body), TrailingGarbage (valid JSON + junk), EmptyBody, HTMLErrorPage — all riding HTTP 200: the layer that defeats hand-adjacent parsers while passing transport checks (#1308 class). Implemented as a middleware wrapping the ABI mux (the wire is the fault surface; the typed handlers never see the corrupted request).
- Pins: the REAL abiclient fails to parse the corrupted GetSnapshot (the canonical #1308 reproduction); one-shot semantics (next call clean); procedure scoping (Deliver-scoped corruption leaves GetSnapshot intact).

### abiclient.Deliver
- The reference client gained the missing op: `Deliver(ctx, *DeliveryRequest) (*DeliveryAck, error)` — the harness rows drive delivery replay through the reference consumer instead of hand-rolled transport. Idempotency is the server's (per entry/attempt).

### Composability
- Pinned: leg-7 recording + leg-8 stall armed together — the stalled call records exactly once. Existing knob tests (legs 1/2/3/5) untouched and green.

---

## Key Decisions

1. **Recording is opt-in and verbatim** — dedup happens in the ledger under test; the knob must observe what the DRIVER sent, not what the server collapsed.
2. **Corruption is one-shot + procedure-scoped** — determinism for CI rows (a persistent corruption would poison unrelated assertions), matching `SetResolveNotFound`'s one-shot idiom.
3. **The stall lives in Deliver, not a global delay** — leg 8's boundary is the delivery/ack window; delaying GetSnapshot or Events would conflate leg 8 with leg 9 (storm cheapness) and leg 3 (event suppression).
4. **Middleware over handler edits for leg 10** — wire corruption must bypass the typed layer entirely; a typed-layer "corruption" would not reproduce the parser-facing bytes.

---

## Blockers

None. Soak row (merge gate) waits on 2a (#1329, in-review).

---

## Tests Run

- `go test -race ./pkg/abi/...` — ok (abiclient full suite 93s, abitest incl. the 9 new knob tests)
- `golangci-lint run ./pkg/abi/...` — 0 issues

---

## Next Steps

1. PR this unit; review-iterate.
2. Unit 2: the assertion harness (continuous S5/S7 diff, L1-L5 convergence histograms, must-be-zero violation counters) in tests/ or local/, consuming 1b's sweep + 2a's lease diff; leg 6 rows compose the existing API e2eFaultInjection with the outbox ladder; leg 9 storm rows against 2a's serve-refresh.
3. Soak row (N×M×λ ≥2h) after 2a merges.

---

## Files Modified

- pkg/abi/abitest/fault_knobs.go (new) + fault_knobs_test.go (new)
- pkg/abi/abitest/server.go (knobs field, middleware wrap, Deliver stall+record)
- pkg/abi/abiclient/client.go (Deliver)

---

## Review r1 remediation (2026-09-11, PR #1331)

- **Ctx-cancel of the leg-8 stall pinned** (`TestDelayDeliverAck_CtxCancelReturnsPromptly`): 5-minute stall armed, request ctx canceled mid-stall, Deliver returns promptly with `ErrorIs(context.Canceled)` — the regression guard against a `time.Sleep` replacement.
- **Empty-suffix guard pinned** (`TestCorruptNextResponse_EmptySuffixMatchesNothing`): arming `("", mode)` corrupts nothing and is never consumed; `CorruptNextResponseArmed()` now reports the STORED state (the suffix guard lives only in `takeCorruption`, where it is load-bearing — `strings.HasSuffix(path, "")` is always true).
- **Empty-body mode asserted exactly** (`assert.Empty`, both in the mode table and a dedicated test) — no vacuous `Contains ""`.
- **Dead test block deleted** (probe/`json.Unmarshal` + the `encoding/json` import); the canonical typed-parse-defeat pin remains as its own test.
- **Comment corrections:** leg-7 doc now states validation/file-part gate precede the record point + the under-`s.mu` invariant; `abiclient.Deliver` doc now carries the delivery_op semantics ("no second row", current state on duplicate — not frozen).
- **Wall-clock loosen:** the Act-unstalled bound is now relative to the armed delay (150ms) rather than an absolute 100ms.
- **Counts corrected:** the knob test file has 11 top-level test functions (14 cases counting subtests) at this revision — recount on every edit (r2: the counts had been wrong in every prior revision).

## Tests Run (r1)

- `go test -race ./pkg/abi/...` — ok (abitest incl. 12 knob tests; abiclient full suite)
- `golangci-lint run ./pkg/abi/...` — 0 issues

---

## Review r2 remediation (2026-09-11, PR #1331)

- **The leg-8 pin now discriminates at the assertion level:** `TestDelayDeliverAck_PreCanceledCtxBypassesStall` calls the handler IN-PROCESS with a pre-canceled ctx and a 3s armed stall — asserts `ErrorIs(context.Canceled)` and elapsed < 1s. Verified by injecting the reviewer's exact regression (select → `time.Sleep`): the pin FAILS at 3.00s; reverted, green. The HTTP-level cancel test stays (client-abort behavior) with its detection-path honesty corrected in the section comment.
- **The canceled-ctx contract is now real server behavior**, not just test framing: Deliver checks `ctx.Err()` after the stall — a canceled request returns typed `CodeCanceled` and is never recorded (never reaches the ledger). The in-process pin holds it.
- **Mode table reshaped to per-mode verifier closures** — every row asserts something falsifiable; the vacuous `Contains(body, "")` is gone (the dedicated zero-bytes test remains).
- **Worklog truths fixed:** counts above recount at each edit; the earlier "assert.Empty in the mode table" claim was false (the table then used Contains) — corrected here rather than silently.

## Tests Run (r2)

- `go test -race ./pkg/abi/abitest/` — ok (12 top-level tests; 15 executed cases — 11 single + the OneShotModes table's 4 subtests)
- Regression-injection validation: pin fails under `time.Sleep`, green when reverted
- `golangci-lint run ./pkg/abi/...` — 0 issues
