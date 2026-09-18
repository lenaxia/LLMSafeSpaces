# Worklog: Fix #1453 — strip the agent envelope at `{{.prevResult}}` injection (memoryMode=last_result prompt compounding)

**Date:** 2026-09-18
**Session:** Fix issue #1453 on branch `fix/1453-memory-prompt-growth` (worktree wt-1453)
**Status:** Complete

---

## Objective

For routine triggers with `memoryMode=last_result` + `captureMode=full`, round N's prompt embedded round N-1's full captured result — the agent-node output envelope — which itself embeds round N-1's prompt (which embedded round N-2's result...). Unbounded recursive prompt growth per fire until the turn fails on context limits. Implement issue option (b): at injection time, inject only `{response, tokens}` extracted from the stored envelope; capture side and stored format unchanged (back-compat with existing stored fires).

---

## Work Completed

### Envelope shape validated (Rule 7 — before any code)

Empirically confirmed the agent-node output envelope from `cmd/workspace-agentd/workflow_execute.go:467-488` (`execAgentNode` handler), marshaled by `writeWorkflowSuccess` into `NodeExecResponse.Output` (workflow_execute.go:555-564), captured verbatim by the engine at `engine.go` (`resultData = agentResp.Output` under `CaptureMode == types.CaptureFull`), and read back for memory by `pkg/workflows/store.go:758-792` — **filtered to `status='delivered' AND result IS NOT NULL'`**:

```json
{
  "response":   "<joined assistant text parts>" | <arbitrary JSON under enforceStructuredOutput>,
  "session_id": "<opencode session id>" | "" (ephemeral),
  "tokens":     {"input":N,"output":N,"total":N},
  "prompt":     "<THE FULL RENDERED PROMPT of the round>",   // ← the round-compounding field
  "parts":      [{"type":"text","text":...}, ...],
  "session_deleted": true   // only in ephemeral mode
}
```

`tokens` is always present (value struct, not a pointer). Corroborated by `workflow_dedupe_test.go:186` (`output["response"]` pins the harness text) and `parseAgentNodeResponse` (workflow_execute.go:620-645).

**Single-commit origin:** `git log -S` shows the memory feature, the capture, and the envelope-with-prompt ALL landed together in #688 (2026-08-08, v0.10.0). Every `delivered` routine result ever stored in any real deployment is this envelope — there are no legacy non-envelope rows. The old test fixture `{"action":"check email"}` (engine_test.go) was a lazy fake, not a real shape; updated to the real envelope.

### Assumptions stated and validated

| # | Assumption | Validation |
|---|---|---|
| 1 | The stored captured result is the agent-node envelope with embedded prompt | `workflow_execute.go:467-488` + `engine.go` capture block + `workflow_dedupe_test.go:186` |
| 2 | Failed fires never surface in memory (only delivered rows read) | `store.go:762,776` (`status='delivered'` filter) — the `{"error":...}` rows are unreachable |
| 3 | No legacy stored shapes exist | `git log -S "MemoryLastResult"` / `-S '"prompt"'` → single commit 8dee6f7b (#688) introduced both |
| 4 | `response` can be arbitrary JSON, not just string | `enforceStructuredOutput` branch sets `result["response"] = parsed` (workflow_execute.go:475-482) |
| 5 | The memory block (engine.go ~749-768) is the only injection site for `{{.prevResult}}` | grep — only the two arms in `executeRoutine` |

### TDD: failing tests first

New file `api/internal/workflows/engine_memory_test.go` (all run through the real `executeRoutine` wiring with mock store/agentd — same style as the existing engine tests):

- `TestExecuteRoutine_MemoryLastResult_StripsEmbeddedPrompt` — **the compounding pin** (maxRuns=1): stored envelope with `ROUND1-PROMPT-MARKER` in its prompt field; the rendered prompt must equal exactly `... {"response":"round-1 response","tokens":{...}}` and must NOT contain the prompt marker, `ses_round1`, or `parts`.
- `TestExecuteRoutine_MemoryLastResult_MultiRun_StripsEachResult` — **multi-run pin**: two envelopes stripped individually before the `"\n---\n"` join; neither embedded prompt rides; exact-equality assertion.
- `TestExecuteRoutine_MemoryLastResult_EnvelopeVariants` — table: well-formed; missing tokens (omit key); tokens null; empty response; null response; structured-output object response (content preserved, compact encoding); numeric response.
- `TestExecuteRoutine_MemoryLastResult_UnrecognizedShape` — table: non-JSON, JSON string/array/null/number, `{}`, error-object, trailing garbage → placeholder stays unreplaced, fire still delivered, skip logged (recording logger asserts non-silent).
- `TestExecuteRoutine_MemoryLastResult_MultiplePlaceholders` — ReplaceAll semantics preserved (both occurrences replaced).
- `TestExecuteRoutine_MemoryLastResult_MultiRun_MixedShapes` — envelope+garbage → only envelope injected, drop logged; all-garbage → placeholder unreplaced, logged.

Failing-first evidence: before the fix, the compounding pin failed with the raw envelope (incl. `OLD-ROUND-PROMPT`) injected verbatim into the prompt — the bug reproduced exactly as #1453 describes.

### Fix

`api/internal/workflows/engine.go`, only the memory block in `executeRoutine` (~749-780) plus one new helper:

- Both arms now map each stored result through `routinePrevResultInjection` (new, placed after `executeRoutine`): parse with `map[string]json.RawMessage`; require the `response` key; marshal `routinePrevResultPayload{Response, Tokens}` (compact JSON, sorted keys → deterministic). Unrecognized entries are skipped with an Info log; multi-run joins only the recognized ones; all-unrecognized leaves the placeholder unreplaced (same as the existing no-result behavior).
- No changes to: capture side (`resultData = agentResp.Output`), stored format, the ScriptPath branch, the retry classifier, agentd files.

### Fail-safe semantics for unrecognized stored shapes (decision + justification)

**Decision:** a stored blob that is not a JSON object carrying a `response` key is NEVER injected into the prompt — the `{{.prevResult}}` placeholder is left unreplaced and the skip is Info-logged (single-run: one message; multi-run: per-join drop counts).

**Justification:** (1) Per assumption 3, no real delivered row can have a non-envelope shape — this path is corruption/foreign-data defense only. (2) Injecting unverifiable bytes into an LLM prompt is the exact hazard class #1453 closes (unbounded content + prompt-injection surface). (3) Withholding matches the block's pre-existing behavior when no stored result exists (store error/empty → placeholder unreplaced), so the failure mode is neither new nor silent. (4) Stored data is never modified — read path only, back-compat with every existing stored fire. This follows the #1374 strictness pattern: unknown shape → the conservative direction, never a silent pass-through.

---

## Key Decisions

1. **Injection payload = `{response, tokens}` compact JSON** (issue option (b), narrowed per owner direction). `response` rides content-intact (any JSON type); `tokens` included only when the key exists. Compact encoding because `json.Marshal` compacts `json.RawMessage` — deterministic across fires; string contents untouched (only inter-token whitespace collapses).
2. **Unrecognized shape → skip injection** (see above). Alternative (inject raw, one-shot-bounded) rejected: re-opens untrusted-bytes-in-prompt for zero real-world benefit.
3. **Multi-run: per-entry strip, drop unrecognized, join recognized** — preserves the newest-first order and `"\n---\n"` separator exactly as before.
4. **Old test fixture updated** (`{"action":"check email"}` → real envelope) with intent preserved (prevResult still injected; added a not-contains pin for the old round's prompt). Justified by assumption 3: the fixture shape never existed in storage.
5. **Helper placement** after `executeRoutine` (engine.go) — outside sibling lanes (retry classifier ~118-171, ScriptPath branch ~770-786 are wt-1457's; untouched).

---

## Blockers

None.

---

## Tests Run

- `go test -timeout 120s -run 'TestExecuteRoutine_MemoryLastResult' ./api/internal/workflows/` — FAILING before fix (compounding reproduced; all new tests failed) — TDD step 2 evidence.
- `go test -timeout 300s -count=1 ./api/internal/workflows/` — **ok** (full package, after fix).
- `go build ./...` — OK.
- `go test -timeout 600s -race ./api/internal/workflows/` — see PR (run before push).
- `make lint` — see PR.

---

## Next Steps

- Adversarial PR review loop until APPROVED; orchestrator merges (no CHANGELOG/chart changes from this worker).
- Optional follow-up (out of scope): consider surfacing memory-skip counts as a metric if operators ask for observability beyond the log line.

---

## Files Modified

- `api/internal/workflows/engine.go` — memory block in `executeRoutine` + `routinePrevResultInjection` helper + `routinePrevResultPayload` type
- `api/internal/workflows/engine_test.go` — mock `recentRoutineResults` field + `GetRecentRoutineResults` returns it; real-envelope fixture in `TestExecuteRoutine_MemoryLastResult_InjectsPrevResult` + prompt-leak pin
- `api/internal/workflows/engine_memory_test.go` — new: 6 test funcs (pins + tables + mixed shapes) + `recordingLogger`
- `worklogs/0993_2026-09-18_memory-prevresult-strip.md` — this worklog
