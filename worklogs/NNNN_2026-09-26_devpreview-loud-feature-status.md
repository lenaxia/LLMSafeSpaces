# Worklog: #1580 — dev-preview loud-fail + the feature_status read tool

**Date:** 2026-09-26
**Session:** #1580's two parts — the dev-preview MCP tool's silent no-op made loud (controller projection + tool gate), and the feature-flag READ tool (inspectable flags with sources; the controllable set reported EMPTY as fact)
**Status:** Complete

---

## Objective

(1) Calling dev_preview_url with the space's dev preview disabled must fail LOUD with reason + recovery hint — the caller must distinguish disabled from broken, not learn it as a late 503. (2) A feature-status MCP tool: inspectable flags (all, with source: operator/chart-set vs space-set); the CONTROLLABLE set only for flags explicitly designated dynamic. First read approved by the orchestrator: controller-projection fix with skew tolerance; READ-only ship with the empty controllable set stated as fact; the not-projected class (instance settings) enumerated honestly.

---

## Work Completed

### Part 1 — the bug (red-first, both halves)

- **Root cause**: `mcpDevPreviewURL` minted unconditionally; `spec.networkAccess.devPreview` (the "Workspace Settings → Dev Preview" toggle) was never projected into the pod — the tool's own text admitted "otherwise the URL returns 503" (a late, indirect failure).
- **Controller half**: pod_builder projects `WORKSPACE_DEV_PREVIEW_ENABLED` into the workspace container (nil-safe: NetworkAccess is a pointer; unset = the CRD default false); agentd_sidecar projects it into the SIDECAR container too (#1332: the tool runs there in sidecar mode). Explicit true AND false both project — absence is reserved for the one-pod-generation upgrade-skew case.
- **agentd half**: explicit `false` → LOUD tool error: "dev preview is DISABLED for this space — no URL is minted. Enable it in Workspace Settings → Dev Preview… This is a configuration state, not a failure: nothing is broken." Absent (skew) → mint WITH an explicit "the controller did not report this space's dev-preview state (upgrade skew?)" note — never hard-fail a transition. The tool description updated to match.
- **Tests (watched red first)**: DisabledFailsLoud (isError + "disabled" + "Workspace Settings" + NO LSP_DEV_PREVIEW_V1 marker), EnabledMintsURL, ControllerSkewStillMints (mint + the explicit note); the controller's DevPreviewStateProjected (both arms); the sidecar pin (the env present, true|false).

### Part 2 — feature_status (the READ tool, additive)

- New tool `feature_status` (listed with an empty-args schema; machine-readable JSON array of `{feature, active, source, source_detail, controllable}`).
- The inventory — every entry is a flag agentd sees AUTHENTICALLY from the boot env:
  - `dev_preview` — source **space** (`spec.networkAccess.devPreview`, projected; skew = active:false + UNREPORTED in the detail — never guessed).
  - `dev_preview_per_workspace_origin` — operator (PREVIEW_ORIGIN_BASE_DOMAIN presence; the URL-shape topology).
  - `inference_relay_plane` — operator (INFERENCE_RELAY_BASEURL; Epic 72's relay-only emission).
  - `upload_staging` — operator (LLMSAFESPACES_UPLOADS_STAGING_PATH + the 0060 §8 knobs).
  - `single_container_mode` — operator (SINGLE_CONTAINER_SPAWN_MARKER; the deployment shape).
- **The controllable set is EMPTY, reported as fact**: every entry `controllable:false`. No write path exists (CRD = owner territory; instance settings = operator territory; pod env immutable at runtime). No gate/audit scaffolding invented for a nonexistent surface.
- **The not-projected class named, not fabricated**: instance-level settings (the API's registry: rateLimiting.*, workflows.*, triggers.*, the devPreview kill-switch) are structurally unreadable in-pod (the D3 no-API-credentials posture); the tool's description says so — absence reads as design.

---

## Key Decisions

- **Env projection, not an API call**: the tool stays API-call-free (its description's promise); the controller is the authoritative projector of space state (the established PREVIEW_ORIGIN_BASE_DOMAIN pattern).
- **Skew tolerance**: absent env ≠ disabled — one pod-generation of controller/agentd skew mints with an explicit note; explicit false is the only loud-fail. Hard-failing a transition would break previews during every upgrade.
- **Booleans only, no knob values**: the read tool reports feature ACTIVITY + sources, not chart-internal knob values (configuration detail, not feature state).
- **ABI additive**: the tools/list surface grows (TestMCPHandler_ToolsList pins >=2, no exact-count freeze); no renames/removals; the new tools route through the #1561 strict params wire like every other tool.

---

## Blockers

None.

---

## Tests Run

- `go test -count=1 -run 'TestMCPHandler_DevPreview|TestMCPHandler_FeatureStatus' -v ./cmd/workspace-agentd/` — 4 PASS (red-first: DisabledFailsLoud + FeatureStatus failed pre-implementation).
- `go test -count=1 -run 'TestPodBuilder' -v ./controller/internal/workspace/` — 18 PASS (the projection pin red-first).
- `go test -count=1 -run 'TestPodBuilder|TestAgentdSidecar|TestSidecar' ./controller/internal/workspace/` — ok (the sidecar env pin green).
- The full MCP family re-run: `TestMCPHandler_` green (the strict wire applies to the new tool's params).

---

## Next Steps

- The PR body enumerates the CANDIDATE-DYNAMIC flags for the owner's blessing (the control tool is a follow-up lane IF a set is blessed — it needs an authenticated API write path + an authz ruling, neither of which exists today).
- The next controller+agentd rollout carries the projection; until then live pods read the skew note (the transitional case by design).

---

## Files Modified

- `controller/internal/workspace/pod_builder.go` — WORKSPACE_DEV_PREVIEW_ENABLED projection (nil-safe)
- `controller/internal/workspace/agentd_sidecar.go` — the sidecar projection (#1332's container)
- `controller/internal/workspace/pod_builder_test.go` — the projection pin (both arms)
- `controller/internal/workspace/agentd_sidecar_pod_test.go` — the sidecar env pin
- `cmd/workspace-agentd/mcp_server.go` — the loud gate + skew note; the feature_status listing + case
- `cmd/workspace-agentd/feature_status.go` — NEW: the read tool's inventory
- `cmd/workspace-agentd/mcp_server_test.go` — 4 new tests + the marshal helper
- `worklogs/NNNN_2026-09-26_devpreview-loud-feature-status.md` — this worklog
