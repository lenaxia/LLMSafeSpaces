# Worklog: #1561 — the MCP HTTP parse boundary made strict (silent unknown-key/trailing-data salvage ended)

**Date:** 2026-09-24
**Session:** Triage + fix of the /v1/mcp silent-JSON-salvage finding (filed by w3 during #1530); characterized the salvage mechanics, ruled bug-vs-contract, hardened the boundary with TDD pins
**Status:** Complete

---

## Objective

Characterize when agentd's /v1/mcp layer "salvages" malformed transport input (#1561's repro: a body whose injected fragment closes `arguments` early, demoting `message` to a params-level key, gets a schema-level "message is required" instead of a boundary rejection), then decide bug-vs-contract per the repo's emission-hardening conventions and fix if warranted.

---

## Work Completed

### The characterization (when the salvage triggers)

The issue title says "salvages invalid JSON" — the triage's first finding is that this is TWO distinct silent tolerances, and neither is prefix salvage:

1. **Unknown keys at the tools/call params level — silently dropped** (the repro's actual mechanism). The repro body is VALID JSON (verified: `json.loads` passes; params keys = name/arguments/message). The #1530 probe's injected fragment (`"lsp_injected_session":"ses_Y"}`) legally closed `arguments` early, demoting `message` to a params-level key; Go's default `Unmarshal` into the `{Name, Arguments}` struct drops unknown keys, so the tool received arguments without `message` and errored at the schema layer — masking the misplacement.
2. **Trailing data after the first JSON value — silently skipped.** `json.NewDecoder(r.Body).Decode(&req)` reads ONE value and never looks again: a second document or garbage after a complete body was never examined.
3. **True mid-string garbage ALREADY failed loud** (-32700) — the issue's framing overstated this; no prefix salvage of invalid documents exists. Pinned (see tests) so the hardening can't loosen it.
4. **Unknown keys at the request-object level — silently dropped** (jsonrpc/id/method/params struct): KEPT tolerant deliberately — this is protocol-revision surface (MCP revs add request-level keys, e.g. `_meta`; `pkg/session/agentmessage`'s additive-only contract is the in-repo precedent for that pole).

### The verdict: BUG (per the #1529/#1537 emission-hardening lineage)

Silent tolerance of malformed transport input is the pattern those lanes ended; #1530 explicitly deferred this boundary to #1561. A malformed request must fail loudly AT the boundary, not degrade into schema-level errors that mask the corruption.

### The fix (TDD — 4 new tests, the 2 salvage shapes watched red first)

- **tools/call params strict** (`DisallowUnknownFields`): an unknown params key → `-32602 Invalid params: json: unknown field "message"` — the diagnostic names the misplaced key. Precedent cited in-code: the control socket ("a rejection, not an ignored unknown field"). Tool-ARGUMENT keys stay free-form (the tool schemas own that layer); the client is this repo's own injected entry sending exactly name+arguments (verified: every in-repo caller — scripts/mcp-tools-liveprobe.sh, local/test.sh, dev-preview-tunnel e2e — sends clean single-document bodies).
- **Trailing-data check** (a second `Decode` must return `io.EOF`): → `-32700 Parse error: trailing data at offset N (one JSON document per request)`.
- **Parse errors now carry diagnostics** (`Parse error: %v`) — the old bare "Parse error" masked what failed.
- Request-object additive tolerance PINNED as a decision (not an accident) by `TestMCPHandler_RequestBodyAdditiveTolerancePinned`; the strict boundary is the params wire, documented in-code both ways.

---

## Key Decisions

- **Strict at the params wire, tolerant at the request object.** The tools/call params wire is this repo's fixed own-client surface (control-socket precedent: reject); the request object is MCP-revision surface where additive keys are legitimate forward-compat (agentmessage precedent: tolerate). Pinning BOTH poles as tests makes the boundary a documented contract.
- **JSON-RPC error codes over HTTP 400.** The endpoint's convention is JSON-RPC error bodies on HTTP 200 (writeMCPError); the issue's "expect 400" was loose — loudness here is the code + the named-field/offset diagnostics.
- **No change to tool-argument freedom.** `Arguments map[string]any` stays free-form — strictness at the transport boundary, schemas at the semantic layer; conflating them would break every tool.

---

## Blockers

None.

---

## Tests Run

- `go test -count=1 -run 'TestMCPHandler_ToolsCall_MisplacedParamsKeyRejected|TestMCPHandler_TrailingDataRejected|...' -v` — the 2 salvage shapes RED pre-fix (misplaced key → salvaged "message is required"; trailing data → silently skipped), the 2 characterization pins PASS pre-fix (mid-string garbage already -32700; request-object tolerance).
- `go test -count=1 -run 'TestMCPHandler_' -v ./cmd/workspace-agentd/` — 23 PASS post-fix (4 new + the full existing family: auth, initialize, tools/list, unknown method/tool, session read/list paths).
- `go test -count=1 -run 'TestMCPHandler_|TestMCPSession' ./cmd/workspace-agentd/` — ok.

---

## Next Steps

- If a future opencode rev legitimately adds request-level MCP keys (e.g. `_meta`), the pinned additive pole keeps them working; a future rev adding tools/call PARAMS keys would trip the strict wire — that's deliberate (renegotiate the wire consciously in that PR, not silently).
- Parked elsewhere, unaffected: #1560 (chart-pins loud skip, on `fix/chart-pins-loud-skip` in this worktree per PARK-NOTE-1560.md); the worklog self-numbering hook fix.

---

## Files Modified

- `cmd/workspace-agentd/mcp_server.go` — trailing-data check, diagnostics on parse errors, strict tools/call params decode (both poles documented in-code)
- `cmd/workspace-agentd/mcp_server_test.go` — 4 new tests: MisplacedParamsKeyRejected (the #1561 repro verbatim), TrailingDataRejected, MidStringGarbageStillParseErrors, RequestBodyAdditiveTolerancePinned
- `worklogs/NNNN_2026-09-24_mcp-parse-boundary-strict.md` — this worklog
