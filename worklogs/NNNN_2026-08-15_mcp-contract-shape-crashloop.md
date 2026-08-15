# Worklog: materialize contract-shape mcp-server metadata (boot crash-loop)

**Date:** 2026-08-15
**PR:** TBD
**Incident:** brand-new v0.15.6 workspace 38f4b5e6 Init:Error crash-loop the moment a platform MCP server (opengist) was bound.

## Root cause chain

1. The platform-level opengist server existed (admin/`_platform`, enabled) but had **no auto-apply rule and no bindings** — Epic 53 routing (`SeedWorkspaceMCPServers`) only stages servers listed in `mcp_server_auto_apply` / `mcp_server_bindings`, both empty. That answered "why isn't it available" — it was never routed.
2. Adding the auto-apply rule (`target_type='all'`) + backfilling 16 workspaces exposed the REAL bug: **the injection pipeline and the materializer never agreed on the metadata shape.** `loadMCPServers` (pkg/secrets/injection.go) writes contract-conformant native JSON (`"args": [...]`, `"timeoutMs": 5000` — exactly MATERIALIZE-CONTRACT.md), but `Secret.Metadata` was `map[string]string`; the whole-file unmarshal aborted (`cannot unmarshal array into ... Secret.metadata of type string`), and because `LoadSecretsFile` fails the entire batch, ONE bound MCP server took down ALL secrets → Init:Error crash-loop. This cluster never saw it because no MCP server had ever been bound — Epic 53's staging path was never exercised end-to-end here.

## Fix (materialize side — the contract is authoritative)

- `Secret.UnmarshalJSON`: dual-shape metadata. String members pass through; numbers/booleans/arrays/objects are carried JSON-encoded as strings — exactly the form the mcp staging branch already expects (`json.Unmarshal(argsStr)`, `strconv.Atoi(timeoutMs)`). Metadata that is not a JSON object sets `MetadataInvalid` instead of failing the parse.
- `applyOne` reports `MetadataInvalid` as a per-entry `OutcomeFailed` with the reason; `LoadSecretsFile` therefore no longer lets one malformed entry kill the file, and `ErrPartialFailure` is already tolerated by the entrypoint (exit 0, results logged).
- No API-side change: the contract document governs, and the API already implements it.

## Assumptions stated and validated

1. The API only emits non-string metadata for mcp-server entries (`args`, `timeoutMs`) — validated by reading `loadMCPServers`; all other types marshal flat string maps (unchanged pass-through).
2. JSON-encoded-string `args` is what the staging branch consumes — validated by the contract test asserting `json.Unmarshal` round-trip.
3. Legacy all-string files parse identically — pinned by `TestLoadSecretsFile_LegacyStringMetadata`.
4. Per-entry failure surfaces as `ErrPartialFailure`, which the materialize entrypoint treats as exit 0 — validated at cmd/workspace-agentd/secrets.go:415.

## Verification

- 3 new tests: contract-shaped entries (http + stdio variants), malformed-entry skip-not-fatal (asserts per-entry results through Materialize), legacy shape.
- Red confirmed pre-fix (both new-shape tests failed on main).
- Full `./pkg/agentd/...` + `./cmd/workspace-agentd/` (incl. e2e, 323s) pass.

## Cluster remediation (this deployment)

- `mcp_server_auto_apply`: opengist → `target_type='all'` (mirrors the admin API's CreateAutoApply SQL).
- `mcp_server_bindings`: backfilled 16 live workspaces (mirrors BackfillMCPServerAutoApply).
- After the fix ships and pods restart, staged entries parse; opengist tools appear alongside llmsafespaces.

## Follow-up

- The wiring-level gap "no e2e ever staged an MCP server on a real cluster" is already tracked in #860 (wiring e2e item) — this incident is its proof.
