# Worklog: Python SDK workspace model drift fix

**Date:** 2026-08-15
**Session:** PR #870 remediation — DTO-accurate models, OpenAPI backfill, round-trip tests
**Status:** Complete

---

## Objective

Close #867 fully: `workspaces.list()` crashed with `TypeError: ... unexpected keyword argument 'agentNeedsRefresh'`, and `workspaces.get()` after a dev-preview PUT crashes on `devPreviewEnabled` (reproduced by review). Root cause: strict `**kwargs` dataclass expansion against server payloads that grew fields across Epics 11/27a/66.

---

## Work Completed

1. **`sdks/python/llmsafespaces/types.py`**
   - `Workspace` now mirrors the **API transfer object** (`pkg/types/workspace.go` `Workspace`): adds `defaultModel`, `agentNeedsRefresh`, `credentialsPendingSince`, **`devPreviewEnabled`**. An earlier revision of this PR wrongly added `imageTag`/`agentVersion`/`orgId` — those live on `WorkspaceMetadata` (the DB record) and `WorkspaceListItem`, NOT the get/create DTO; removed.
   - `WorkspaceListItem` gains `imageTag`, `agentVersion`, `defaultModel`, `agentNeedsRefresh` (no omitempty — always emitted), `credentialsPendingSince`, `orgId` (matches `WorkspaceListItem` struct, `pkg/types/workspace.go:55-77`).
2. **`sdks/openapi.yaml`** — backfilled the same missing fields onto the `Workspace` and `WorkspaceListItem` schemas (single source of truth per `sdks/README.md:22`); `agentNeedsRefresh` documented as always-present.
3. **Round-trip regression tests** (`sdks/python/tests/test_client.py`): full-payload round-trips for `get` (incl. `devPreviewEnabled: True`) and `list` (incl. `agentNeedsRefresh`, `imageTag`, `agentVersion`, `orgId`). **Red/green verified**: removing `devPreviewEnabled` from the dataclass fails `test_workspace_get_full_payload_round_trip` with the exact TypeError; restoring passes (55/55).
4. **`sdks/canary/python/scenarios/s_ws_crud.py`** — N3 empty-runtime now mirrors the Go twin (assert success + defaulted runtime); cleanup wrapped in try/except so a transient delete failure can't abort N5/N6 (Python raises where Go discards the error).

---

## Key Decisions

1. DTO-accurate over kitchen-sink: dataclasses mirror exactly what each endpoint emits; the in-file comment names the Go authority struct to prevent the next Metadata-vs-DTO confusion.
2. Round-trip tests pin the full server shape, so the next added server field fails in the SDK test suite first (not as a canary crash weeks later).
3. OpenAPI backfilled in the same PR — the spec is the cross-language source of truth.

---

## Tests Run

- `pytest sdks/python/tests/test_client.py` — 55/55 pass
- Red/green mutation check on `devPreviewEnabled` — fails pre-fix, passes post-fix
- `py_compile` both touched Python files; `yaml.safe_load` on openapi.yaml

---

## Blockers

None.

---

## Next Steps

- After merge with #869: the Python canary section should complete fully green for the first time; consider flipping `sdk-canary.continue-on-error` to `false` (ci.yml TODO) after a few consistently green runs.

---

## Files Modified

- sdks/python/llmsafespaces/types.py
- sdks/python/tests/test_client.py
- sdks/openapi.yaml
- sdks/canary/python/scenarios/s_ws_crud.py
- worklogs/NNNN_2026-08-15_python-sdk-workspace-model-drift.md (this file)
