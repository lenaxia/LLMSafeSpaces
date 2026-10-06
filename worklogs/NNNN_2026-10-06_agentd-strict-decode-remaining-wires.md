# Worklog: #1565 — the #1561 parse-boundary pattern on agentd's two remaining loose JSON surfaces

**Date:** 2026-10-06
**Session:** Triage + fix the workflow_execute and user_timezone loose decodes per the #1561/#1564 strict-decode convention; red-first pins; PR #1625
**Status:** Complete

---

## Objective

Apply the #1561/#1564 parse-boundary convention (exactly one JSON document, loud diagnostics, bounded reads) to `cmd/workspace-agentd/workflow_execute.go` and `user_timezone.go`, per the tracking issue filed from #1564's r1 review.

---

## Work Completed

### Triage

- `/v1/workflow/node/execute`: one legit client — this repo's `HTTPAgentExecutor.Execute` (api/internal/workflows/engine.go), single `json.Marshal` body. Had a 16 MiB `io.LimitReader` cap; no trailing-data treatment; additive keys tolerated.
- `/v1/user-timezone`: one legit client — `agentpush.PushUserTimezone` (api/internal/services/agentpush), single `json.Marshal` body. Had a 256-byte cap; no trailing-data treatment; bare "bad request" diagnostics.
- Verdict per the #1561 lineage: silent tolerance of malformed transport ends; strict one-document boundary + loud diagnostics + loud caps on both.

### The silent tolerations (each pinned RED against the pre-fix handlers)

1. Trailing data after the first JSON value — the first document EXECUTED (workflow: node ran, 200) or STORED (timezone: atomic set) with the remainder unexamined.
2. Cap by silent truncation — workflow: over-cap body → 400 "unexpected EOF" (confusing class); timezone: `{"timezone":"UTC"}` + 300 spaces was ACCEPTED (bytes past the LimitReader invisible).
3. Timezone parse errors carried no diagnostics (bare "bad request").

### The fix

- `http.MaxBytesReader` + shared `decodeOneDocument` (mcp_server.go's helper from #1564; comment generalized to name it the shared inbound-JSON boundary for agentd's HTTP wires).
- Parse class rides 400 with the decoder detail ("trailing data after offset N (one JSON document per request)"); oversize rides 413 with the cap named (MaxBytesError classification through the %w wrap — the #1564 r2 finding, covered here by the timezone trailing-scan pin).
- A rejected timezone push no longer stores the first half of a corrupted body.
- Additive request-object keys stay TOLERATED on both wires, pinned as a decision: the API server is version-skewed from the pods it drives (the #1564 request-object ruling).

---

## Key Decisions

- Reuse `decodeOneDocument` verbatim rather than a per-handler variant — one boundary helper, three wires, same error contract; the outbound `decodeStrict` (client.go) keeps its own.
- Keep the historical caps (16 MiB / 256 B) — socialized bounds, now loud.

---

## Blockers

None.

---

## Tests Run

- RED stage (pre-fix handlers): 5 pins failed exactly as the tolerances predict; 4 characterization poles green pre-fix (newline x2, additive x2).
- Post-fix: `TestWorkflowExecute_|TestUserTimezone_` families green; full `./cmd/workspace-agentd/` package green; gofmt/vet clean.

---

## Next Steps

- Iterate PR #1625 through review.
- #1623 (sidebar gesture zone) — the queued lane.

---

## Files Modified

- cmd/workspace-agentd/workflow_execute.go (decode + 413 class)
- cmd/workspace-agentd/user_timezone.go (decode + diagnostics + 413 class + cap constant)
- cmd/workspace-agentd/mcp_server.go (decodeOneDocument comment generalized)
- cmd/workspace-agentd/workflow_execute_test.go, user_timezone_test.go (9 pins)
