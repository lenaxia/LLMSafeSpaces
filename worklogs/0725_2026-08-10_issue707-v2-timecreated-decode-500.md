# Worklog: V2 session queue 500 on opencode 1.18.10 — timeCreated decode bug (issue #707)

**Date:** 2026-08-10
**Session:** Diagnose a production 500 on every prompt after the v0.13.0 deploy, root-cause the V2 session queue path, fix the decode bug, ship a regression test.
**Status:** Complete

---

## Objective

Every prompt to every workspace started returning `500 {"error":"failed to enqueue message"}` within minutes of the v0.13.0 deploy. Identify what changed, restore service, and fix the underlying defect with a regression test.

---

## Root Cause (Proven)

### What changed at the moment of breakage

The API deployment was running image `api:0.12.1` with the env var `LLMSAFESPACES_V2_SESSION_QUEUE=true` set live via `kubectl set env` at some earlier point. The env var was **dormant** under 0.12.1 — that version has no V2 session-queue code path (PR #695 landed in 0.13.0).

The 0.13.0 deploy (revision 229, both API pods ~9 min old at time of diagnosis) activated the V2 path. The V2 path contained a latent JSON decode bug that had been shipped in #695 the day before.

### The decode bug

`pkg/agent/opencode/client_v2.go` declared:

```go
type V2PromptResponse struct {
    AdmittedSeq int    `json:"admittedSeq"`
    ID          string `json:"id"`
    SessionID   string `json:"sessionID"`
    TimeCreated string `json:"timeCreated,omitempty"`   // ← opencode 1.18.10 returns a NUMBER
}
```

opencode 1.18.10 returns `timeCreated` as epoch-millis **number**, not an ISO-8601 string. `json.Decoder.Decode` fails with `*json.UnmarshalTypeError` ("cannot unmarshal number into Go struct field … of type string"). `PromptV2` returns the error → `enqueueV2` (`proxy_v2.go:271-273`) logs `V2 enqueue: PromptV2 failed` and returns the generic 500.

The field is **never read by any caller** — only `resp.ID` is used (at `proxy_v2.go:278`). The 500 was pure breakage for an unused field.

### Why the "spike-verified" claim is wrong

The doc comment on `v2PromptRequest` claimed:

> F18 (spike-verified): opencode 1.18.10 requires `{prompt:{text:"..."}}`...

The spike verified the **request body shape** (text vs parts). It did NOT verify the **response shape**. The unit test `TestPromptV2_Success` compounded the error by using `"timeCreated":"2026-08-09T12:00:00Z"` (a string) in its canned response — the test passed while production failed. This is exactly the silent-assumption-drift failure mode called out in README-LLM.md §7.

### Secondary issue (documented, not fixed in this PR)

opencode 1.15.12 returns `400 Expected Session.Delivery, got "queue"` because the `delivery` field predates it. The V2 path activates unconditionally when the flag is on, regardless of agent version. Tracked in #707 as follow-up — needs identifying the minimum opencode version that supports `delivery:queue` and adding a per-workspace capability signal.

---

## Work Completed

### Incident response (live cluster)

- `kubectl set env deploy/llmsafespaces-api -n llmsafespaces LLMSAFESPACES_V2_SESSION_QUEUE-` → unset the dormant-then-activated env var
- `kubectl rollout status` → both pods rolled, V1 Redis-queue path restored
- Verified the chart (`helm/values.yaml:474`) defaults `sessionQueue.v2Enabled: false` — Flux reconcile will not re-add the env var
- Verified the deployed template has no `V2_SESSION_QUEUE` env post-rollout

### Issue filed

[#707](https://github.com/lenaxia/LLMSafeSpaces/issues/707) with the verbatim production error log, root cause, and proposed fix. Documents the secondary version-gate issue as follow-up.

### Fix — remove the unused field

Deleted `TimeCreated string` from `V2PromptResponse`. No consumer reads it; only `resp.ID` is used. Removing is preferred over retyping because (a) the field carries no platform value, (b) `encoding/json` ignores unknown keys by default so the decode becomes resilient to any future opencode field additions, and (c) adding fields back later when an actual consumer appears is cheap.

Corrected the misleading "spike-verified" wording on `v2PromptRequest`'s doc comment. The spike verified the request shape, never the response shape; the NOTE now establishes the schema-pin discipline explicitly.

### Regression tests (TDD)

| Test | Validates |
|---|---|
| `TestPromptV2_RealShapeTimeCreatedAsNumber` (new) | Verbatim opencode 1.18.10 response with `timeCreated` as number decodes successfully. Fails RED pre-fix (number→string `UnmarshalTypeError`), passes GREEN post-fix. |
| `TestPromptV2_Success` (corrected) | Updated canned body to use the real number shape; previously masked the bug with an ISO-8601 string. |

Defense-in-depth: `startV2TestServer`'s canned body (used by every integration-level V2 handler test) also updated to include `"timeCreated":<number>` so any future re-typing of the field fails at the integration layer too.

---

## Key Decisions

1. **Remove `TimeCreated`, don't retype.** A `json.Number` or `int64` retyping would preserve a field no one reads and re-introduce the same fragility class if opencode ever changes the shape again. Removing makes the decode structurally resilient to any opencode response-shape drift on untyped fields. This is the cleanest expression of Rule 4 ("not over-engineered").

2. **Don't add a version gate in this PR.** The V2 path's lack of an opencode version gate is a real bug (1.15.12 returns 400), but it's a larger piece of work: identify the minimum opencode version supporting `delivery:queue`, design a per-workspace capability signal, gate the flag on it. Scope creep would delay the urgent decode fix. Tracked in #707; flag stays `false` by default.

3. **Don't roll back the deploy.** The 0.13.0 deploy contains other fixes (trigger concurrent-fire race #700, stuck-Creating escape hatch #702, workspace re-suspend #696) that the cluster needs. Unsetting the single env var was surgical and preserved all other 0.13.0 fixes.

---

## Assumptions Stated + Validated

1. **`V2PromptResponse.TimeCreated` has no consumer.** Validated by grepping the codebase: only `resp.ID` is read at `proxy_v2.go:278`; the only `TimeCreated` references remaining after deletion are in comments. ✓
2. **opencode 1.18.10 returns `timeCreated` as a number.** Validated by the production error log: `json: cannot unmarshal number into Go struct field V2PromptResponse.data.timeCreated of type string`. The error message is generated by `encoding/json` when the JSON value is a number and the target field is a string. ✓
3. **The V2 flag default is `false`.** Validated by reading `helm/values.yaml:474` (`v2Enabled: false`) and confirming the deployed template has no `V2_SESSION_QUEUE` env after the unset. ✓
4. **The flag was set live, not via the chart.** Validated by reading the v0.13.0 bump commit in talos-ops-prod (`6925b2ec`) — only image tags changed, no sessionQueue values. The env var was introduced via `kubectl set env` at an earlier date. ✓
5. **Unsetting the env var restores the V1 path.** Validated by reading `proxy_handlers.go:84` (`if h.v2SessionQueueEnabled {`) — the V1 path is the fallthrough when the flag is false. ✓

---

## Blockers

None. Fix shipped in PR #708; live mitigation stable.

---

## Tests Run

- `go test -timeout 60s ./pkg/agent/opencode/...` — all V2 client tests pass, including new regression test
- `go test -timeout 120s ./api/internal/handlers/... -run "V2|SendPrompt|Enqueue|Strand"` — green
- `go build ./...` — clean

---

## Next Steps

1. **Merge #708**, tag v0.13.1, bump talos-ops-prod to pick up the fix.
2. **Version gate (tracked in #707):** identify minimum opencode version supporting `delivery:queue`; add per-workspace capability signal; gate the V2 flag on it.
3. **Process:** any future opencode-response-shape change must land with a schema-pinned test using the verbatim real response, not a doc-only "spike-verified" assertion.
