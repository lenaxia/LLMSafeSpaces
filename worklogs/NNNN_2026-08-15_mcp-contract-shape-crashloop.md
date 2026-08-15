# Worklog: materialize contract-shape mcp-server metadata (boot crash-loop)

**Date:** 2026-08-15
**PR:** TBD
**Incident:** brand-new v0.15.6 workspace 38f4b5e6 Init:Error crash-loop the moment a platform MCP server (opengist) was bound.

## Root cause chain

1. The platform-level opengist server existed (admin/`_platform`, enabled) but had **no auto-apply rule and no bindings** — Epic 53 routing (`SeedWorkspaceMCPServers`) only stages servers listed in `mcp_server_auto_apply` / `mcp_server_bindings`, both empty. That answered "why isn't it available" — it was never routed.
2. Adding the auto-apply rule (`target_type='all'`) + backfilling 16 workspaces exposed the REAL bug: **the injection pipeline and the materializer never agreed on the metadata shape.** `loadMCPServers` (pkg/secrets/injection.go) writes contract-conformant native JSON (`"args": [...]`, `"timeoutMs": 5000` — exactly MATERIALIZE-CONTRACT.md), but `Secret.Metadata` was `map[string]string`; the whole-file unmarshal aborted (`cannot unmarshal array into ... Secret.metadata of type string`), and because `LoadSecretsFile` fails the entire batch, ONE bound MCP server took down ALL secrets → Init:Error crash-loop. This cluster never saw it because no MCP server had ever been bound — Epic 53's staging path was never exercised end-to-end here.

## Fix (materialize side — the contract is authoritative)

- `Secret.UnmarshalJSON`: dual-shape metadata. String members pass through; numbers/booleans/arrays/objects are carried JSON-encoded as strings — exactly the form the mcp staging branch already expects (`json.Unmarshal(argsStr)`, `strconv.Atoi(timeoutMs)`). Metadata that is not a JSON object sets `MetadataInvalid` instead of failing the parse.
- `applyOne` reports `MetadataInvalid` as a per-entry `OutcomeSkipped` with the reason (review F1: the first version used OutcomeFailed, which trips `HasFailures()` → ErrPartialFailure → the entrypoint's SECOND gate returns 3 → the very crash-loop this PR eliminates; empirically confirmed by the reviewer's subcommand run). Skipped keeps boot at exit 0 per invariant T5 ("an invalid secret skips that secret only") and makes the reload handler answer 200-with-reason instead of 500 — deliberate: malformed input is the client's, boot tolerance is the platform's.
- Reload-cache replay consistency (review N1): the verdict round-trips through the reserved `metadata_invalid` wire key — `json:"-"` alone lost it, and replay materialized rejected secrets with defaults (reviewer reproduced with a garbage-metadata ssh-key writing id_ed25519_deploy on run 2). Pinned by wire-form + two-run replay tests, mutation-verified. Known corollary (pre-existing cache semantics, unchanged): a stale cache entry shadows a corrected base entry for the pod's lifetime.
- No API-side change: the contract document governs, and the API already implements it.

## Assumptions stated and validated

1. ~~The API only emits non-string metadata for mcp-server entries~~ **Corrected (review F2):** user secrets pass `UserSecret.Metadata` through verbatim and the API's validateMetadata checks only key PRESENCE — a client can persist `{"var_name":123}` for any type, which pre-fix crash-looped the workspace identically. The dual-shape reader is load-bearing for every secret type, not just mcp-server.
2. JSON-encoded-string `args` is what the staging branch consumes — validated by the contract test asserting `json.Unmarshal` round-trip.
3. Legacy all-string files parse identically — pinned by `TestLoadSecretsFile_LegacyStringMetadata`.
4. ~~Per-entry failure surfaces as `ErrPartialFailure`, which the entrypoint treats as exit 0~~ **Corrected (review F1):** this was the exact misconception behind the round-1 bug — `ErrPartialFailure` fires only when `HasFailures()` is true, and those same failures exit 3 at the entrypoint's SECOND gate (cmd/workspace-agentd/secrets.go:477-482). Malformed metadata maps to `OutcomeSkipped` (not Failed), which trips neither gate; both the subcommand tests and the reviewer's real-binary runs pin exit 0.
5. Known outcome flip (review round-2, low, no security impact): a metadata-invalid entry of a metadata-ignoring type (api-key, llm-provider) is Skipped on first boot but Materialized after cache replay heals it — same plaintext, same batch. Noted, not fixed (fixing requires verdict persistence for skipped-by-validation entries too; tracked with the #860 wiring-e2e work).

## Verification

- Unit: contract-shaped entries (http + stdio variants), malformed-entry skip-not-fatal (per-entry results through Materialize), legacy shape. Red confirmed pre-fix.
- Subcommand e2e (real binary, real exit codes): `TestMaterializeSubcommand_MCPContractShape_Exits0` (the incident workflow: contract entries → exit 0 → mcp section rendered into agent-config.json) and `TestMaterializeSubcommand_MalformedMCPMetadata_SkipsNotCrashloops` (exit 0, sibling materialized, skip reported by name).
- Seam integration: `TestMCPInjection_MaterializerSeam_ContractShape` — real InjectSessionlessSecrets output (admin KEK round-trip) parsed and staged by the real LoadSecretsFile; mutation-verified in BOTH directions (writer emitting string-form args fails it; reader dropping raw values fails it).
- Full `./pkg/agentd/...` + `./cmd/workspace-agentd/` (incl. e2e, 329s) pass.
- Scope note: per-entry tolerance covers metadata-SHAPE malformations; a structurally malformed entry (non-object element / non-string type) still fails the whole file → exit 2 (unchanged, consistent with the PR's scoping).

## Cluster remediation (this deployment)

- `mcp_server_auto_apply`: opengist → `target_type='all'` (mirrors the admin API's CreateAutoApply SQL).
- `mcp_server_bindings`: backfilled 16 live workspaces (mirrors BackfillMCPServerAutoApply).
- After the fix ships and pods restart, staged entries parse; opengist tools appear alongside llmsafespaces.

## Follow-up

- The wiring-level gap "no e2e ever staged an MCP server on a real cluster" is already tracked in #860 (wiring e2e item) — this incident is its proof.
