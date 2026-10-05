# Worklog: list_models tool, call_with_model failure guidance, and credential-declared vision metadata

**Date:** 2026-10-05
**Session:** Wire the half-added model-catalog plumbing into agent-facing tools; make call_with_model failures self-correcting; fix the vision gate's false refusals on custom-endpoint models. TDD where the contract is pin-able; probe-verified where the contract is opencode's.
**Status:** Complete — tests green (pkg/agent/opencode, pkg/secrets full; cmd/workspace-agentd full suite in the run), PR open.

---

## Objective

Three gaps found while debugging a live `call_with_model` failure ("classifier", a vision model, refused images):

1. `Client.ListModels` (GET /provider) existed with tests but NO MCP tool exposed it — agents had no way to learn valid model names.
2. call_with_model failures were cryptic: bare names died at `SplitModelRef` with schema-level text; unknown qualified models surfaced as opaque seam 500s at send time.
3. The vision pre-check refused images for every custom-endpoint model — `thekaocloud/classifier` included — because opencode synthesizes an all-text-only capabilities block for models absent from its bundled models.dev catalog, and the gate trusted that as known-false.

## Work Completed

### list_models tool
- `mcpListModels` (`cmd/workspace-agentd/mcp_tools.go`): seam `Client.AvailableModels` → sorted `{model: "provider/id", name, contextWindow, maxOutput}` entries with a count. Registered in tools/list next to call_with_model; dispatched in `callMCPTool`.
- Seam-level `Client.AvailableModels` (`pkg/agent/opencode/client.go`): ListModels + `parseProviderCatalogForContract`, raw bytes stay in the seam (Rule 12). `Adapter.ListAvailableModels` now delegates — the duplicated parse is gone.
- Guidance rows in `TestMCPHandler_ToolDescriptionGuidance` (descriptions are agent-visible contract; changed in the same commit).

### call_with_model failures that self-correct
- Bare name → `bareModelRefError`: catalog lookup for case-insensitive ID matches; "did you mean 'p/text'?" (sorted, quoted, multi-provider safe); every variant appends the list_models pointer.
- Qualified but absent → `verifyModelInCatalog` pre-check BEFORE the carrier session exists: unknown provider names the connected providers; known provider names its model IDs. **Fail-open by design** (#1307 direction): unreachable/unparseable/empty catalog never blocks a call the wire would accept — pinned by test.
- The model InputSchema description now points at list_models.

### Credential-declared vision metadata (the classifier fix)
- Probe (2026-10-05, pinned opencode, throwaway serve on :4199): config per-model `attachment` lands in `/config/providers` `capabilities.attachment`; config CANNOT reach `capabilities.input.image`; models.dev-absent models get a synthesized all-false block (verified on the live workspace catalog: `bedrock-claude-sonnet-4.6` marked image:false).
- `LLMModelConfig.Attachment *bool` (`pkg/secrets/types.go`): tri-state declaration; nil = undeclared.
- `FormatOpenCodeConfig` passes it through (`opencodeModel.Attachment`); field is in the pinned schema and accepted by the live binary.
- `ModelInfo` (`pkg/agent/opencode/loopback.go`) OR-merges both signals: either present = KNOWN, either true = vision-capable. Pointerized parsing preserved (#1307 r1 finding 1: partial blocks stay unknown).
- Vision refusal message now names the declaration path (attachment:true on the credential's model entry) so agents escalate the fix to the user instead of dead-ending.

## Key Decisions

- **Data-side fix, not a gate-side heuristic.** The gate's contract (known-false refuse / unknown allow) is correct; the wrongness was the data. Sniffing the "synthesized all-false block" was rejected: indistinguishable from declared-false in the merged catalog, brittle against opencode template changes, and muddies #1307's tri-state discipline.
- **Residual trap documented, not papered over:** custom models with NO declaration still refuse images (synthesized false is indistinguishable from declared false). The refusal message names the remedy; the credential author declares truth once and every gate (pre-check, transcript repair) honors it.
- **Fail-open on the unknown-model pre-check but loud when it fires:** the pre-check turns send-time 500s into named-alternatives errors only when the catalog is positively known; any catalog problem defers to the wire.
- **Relay models (`RelayModel`) do NOT carry attachment yet:** free-models.json has no capability field; extending the controller wire format is out of this lane's scope.

## Evidence

- `TestModelInfo_ImageSignalVariants` — 8-row signal table incl. the classifier case (attachment:true + synthesized image:false → vision-capable).
- `TestMCPCallWithModel_DeclaredAttachment_OverridesSynthesizedTextOnly` — end-to-end through the fake seam: image rides a declared custom model.
- `TestFormatOpenCodeConfig_AttachmentDeclaration` — tri-state pass-through golden.
- `TestMCPCallWithModel_BareModelDidYouMean` / `_UnknownQualifiedModel` / `_UnknownProvider` / `_CatalogUnreachable_FailOpen`; `TestMCPListModels`.
- Live probe transcript (config with attachment true/false/undeclared → catalog) recorded in the session; the throwaway opencode was killed after.
