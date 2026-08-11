# Worklog: 0.14.0 GetHistory 502 on opencode 1.18.10 — flat-string tool shape (issue #730)

**Date:** 2026-08-11
**Session:** Diagnose a production 502 on every history fetch after the v0.14.0 deploy, root-cause the Epic 65 typed parser's incompatibility with opencode 1.18.10's flat tool-part wire shape, mitigate by rolling back, then implement the forward fix with regression tests.
**Status:** Fix complete; ready for 0.14.1 deploy.

---

## Objective

Every `GET .../sessions/{sid}/message` returned `502 {"error":"failed to fetch history"}` for all sessions in all workspaces within seconds of the 0.14.0 deploy. Diagnose from cluster surfaces, mitigate immediately, and fix the underlying defect with regression tests so the next opencode wire-shape change degrades gracefully instead of repeating the Sev1.

---

## Root Cause (Proven)

### What changed at the moment of breakage

PR #721 (US-65.4 batch 2, "GetHistory returns contract shapes") shipped a typed JSON parser for opencode session history in `pkg/agent/opencode/translate.go`. The parser declared `ocPart.Tool` as `*ocTool` (a JSON object) and decoded the entire response body in one shot via `json.Unmarshal(body, &[]ocMessage{})`. A single unparseable part failed the whole unmarshal → `Adapter.GetHistory` returned the error → `ProxyHandler.GetHistory` wrote `502`.

### The wire-shape mismatch

opencode 1.18.10 flattened the tool part. `"tool"` is now the **tool name as a bare string**, with `callID`, `state`, `input`, and `output` hoisted to the part level:

```json
{
  "type": "tool",
  "callID": "call_80396e4d...",
  "tool": "bash",
  "state": {
    "status": "completed",
    "input": { "command": "git clone ..." },
    "output": "Cloning into ...",
    "time": { "start": 1786374885930, "end": 1786374894033 }
  }
}
```

The Epic 65 parser expected the legacy nested-object shape (`"tool": {"name": ..., "callID": ..., ...}`). `json.Unmarshal` rejected the string with:

```
cannot unmarshal string into Go struct field ocPart.parts.tool of type opencode.ocTool
```

### Ground truth

Validated by fetching the raw history directly from the live pod via `kubectl exec` + `curl` (Basic auth with `/sandbox-cfg/password`):

- HTTP 200, 7.1 MB body
- `jq '[.[] | .parts[]? | select(.type=="tool") | .tool | type] | unique'` → `["string"]` — 100% of tool parts use the flat shape
- Tool names observed: `bash`, `edit`, `glob`, `grep`, `question`, `read`, `task`, `todowrite`, `write`

### Why the tests didn't catch it (silent assumption drift, README §7)

Same failure class as #707 and #486:

1. **Stale validation claim.** README-LLM.md recorded shapes as "validated from opencode 1.15.12 binary." Cluster pods run opencode **1.18.10**. The schema changed between versions; the parser was never re-validated.
2. **Canned tests encoded the wrong shape.** `translate_test.go` built `ocPart` values with `Tool: &ocTool{Name: "bash", ...}` (legacy nested). Every test fed the parser the shape the parser expected — neither challenged the other.
3. **No schema pin.** Nothing in the suite was anchored to a real captured payload.

---

## Work Completed

### Incident response (live cluster)

- `kubectl rollout undo deploy/llmsafespaces-api -n llmsafespaces` → rolled from `api:0.14.0` (revision 244) back to `api:0.13.1` (revision 245)
- Verified `0` occurrences of `adapter failed` / `cannot unmarshal` / `502` in API logs post-rollback
- The 0.13.1 legacy path (`paginateOpencodeHistory`) decodes into `[]json.RawMessage` and passes opencode's bytes through untouched — tolerates 1.18.10 natively
- Outage window: ~24 minutes (03:28Z → 03:52Z)

### Issue filed

[#730](https://github.com/lenaxia/LLMSafeSpace/issues/730) with verbatim production logs, the raw opencode payload from the pod, root cause, fix plan, and a detailed 6-test regression spec.

### Fix 1 — Parser: accept both tool shapes

Added a custom `UnmarshalJSON` on `ocPart` (`translate.go`) that normalizes two wire shapes into the existing canonical `ocTool`:

| Shape | Source | Canonical mapping |
|---|---|---|
| Flat string (1.18.10+) | `"tool":"bash"` + part-level `callID`, `state:{status,input,output,time:{start,end}}` | `Name=<string>`, `CallID=<part-level>`, `Input/Output` from `state`, `StartedAt/CompletedAt` from `state.time.{start,end}` (epoch-millis) |
| Legacy nested (≤1.15.x) | `"tool":{"name","callID","input","output","state":{status,startedAt,completedAt}}` | decoded directly into `ocTool` |

The `session.ToolPart` contract is unchanged — `translateTool` and all downstream code is untouched. New types: `ocFlatToolState`, `ocFlatToolTime`.

### Fix 2 — Resilience: per-message decode downgrade

Replaced the single `json.Unmarshal(body, &raw)` in `ParseHistoryWire` with a two-stage decode:

1. **Stage 1:** split into `[]json.RawMessage` — only fails if the body is not a JSON array at all (genuine error, surfaced).
2. **Stage 2:** decode each message independently. A decode failure (future opencode schema change in one part) downgrades that single message to a `session.MessageSystem` notice; the rest translate normally.

This ensures one bad upstream shape degrades to a visible-but-non-fatal notice instead of Sev1-ing the history surface (README §12 containment).

### Fix 3 — README correction

Updated the stale "validated from opencode 1.15.12 binary" note in README-LLM.md to clarify that config-loading is stable but the history message-part shape is NOT, and is now pinned by golden fixtures in `pkg/agent/opencode/testdata/`.

### Regression tests (TDD — written first, verified RED, then GREEN)

Golden fixtures (schema pins):
- `testdata/history_1_18_10_flat_tool.json` — verbatim captured 1.18.10 wire shape
- `testdata/history_1_15_12_nested_tool.json` — legacy nested-object shape guard

| Test | Purpose | Pre-fix | Post-fix |
|---|---|---|---|
| `TestParseHistoryWire_RealShape1_18_10_FlatTool` | Primary regression: verbatim 1.18.10 flat tool part → correct `session.ToolPart` with Name/CallID/State/Input/Output/StartedAt/CompletedAt | RED (production error) | GREEN |
| `TestParseHistoryWire_LegacyNestedTool_StillWorks` | Non-regression: legacy 1.15.x nested shape still decodes correctly | GREEN | GREEN |
| `TestParseHistoryWire_MixedShapesInOneHistory` | Forward-compat: both shapes in one array translate correctly | RED | GREEN |
| `TestParseHistoryWire_OneMalformedPart_DoesNot502` | Fix 2 validation: one bad message downgrades to system notice, rest survive | RED | GREEN |
| `TestParseHistoryWire_TotallyGarbage_StillErrors` | Defense-in-depth: genuinely malformed body still errors with "parse message array" | GREEN | GREEN |
| `TestParseHistoryWire_AllObservedToolNames_1_18_10` | Breadth: all 9 tool names observed in prod (bash/edit/glob/grep/question/read/task/todowrite/write) decode | RED | GREEN |

All existing `translate_test.go` and `adapter_test.go` tests continue to pass unchanged.

---

## Validation

```
$ go test ./pkg/agent/opencode/ -count=1 -timeout 120s
ok  	github.com/lenaxia/llmsafespaces/pkg/agent/opencode	11.378s

$ go test ./api/internal/handlers/ -run 'Proxy|Adapter|History|GetHistory|ParseHistory' -count=1 -timeout 120s
ok  	github.com/lenaxia/llmsafespaces/api/internal/handlers	33.299s

$ go test ./pkg/session/ -count=1
ok  	github.com/lenaxia/llmsafespaces/pkg/session	0.006s

$ go vet ./...      # clean
$ go build ./...    # clean
$ gofmt -l ...      # clean
```

---

## Files Changed

- `pkg/agent/opencode/translate.go` — Fix 1 (`UnmarshalJSON` on `ocPart`, `ocFlatToolState`/`ocFlatToolTime` types) + Fix 2 (two-stage decode in `ParseHistoryWire`)
- `pkg/agent/opencode/translate_test.go` — 6 new regression tests + `os`/`fmt` imports
- `pkg/agent/opencode/testdata/history_1_18_10_flat_tool.json` — golden fixture (schema pin)
- `pkg/agent/opencode/testdata/history_1_15_12_nested_tool.json` — golden fixture (legacy guard)
- `README-LLM.md` — Fix 3 (stale validation wording corrected)

---

## Follow-up

- **Schema-pin discipline across all opencode decode sites.** Same canned-shape gap likely exists in the SSE bridge (`translate_sse_test.go`), session-list parser (`ParseSessionListWire`), and V2 client. Audit + add captured-fixture regression tests.
- **Per-workspace opencode capability signal.** Prod fleet is a mix of 1.15.12 and 1.18.10 workspaces. The two-stage decode (Fix 2) is the down payment; a full version-gate is the longer-term hardening.
- **Deploy 0.14.1** with this fix to restore 0.14.0 features (dev-preview tunnel #725, MCP-port fix) that were lost in the rollback.
