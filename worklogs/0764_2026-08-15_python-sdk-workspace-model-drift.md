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
2. **`sdks/openapi.yaml`** — backfilled the same missing fields onto the `Workspace` and `WorkspaceListItem` schemas (single source of truth per `sdks/README.md:22`); `agentNeedsRefresh` documented as always-present; `Workspace.phase` enum gained `Creating` (was missing vs the CRD authority `pkg/apis/llmsafespaces/v1` — review round 2) and `WorkspaceListItem.phase` now carries the same enum for consistency.
3. **Round-trip regression tests** (`sdks/python/tests/test_client.py`): full-payload round-trips for `get` (incl. `devPreviewEnabled: True`) and `list` (incl. `agentNeedsRefresh`, `imageTag`, `agentVersion`, `orgId`). **Red/green verified**: removing `devPreviewEnabled` from the dataclass fails `test_workspace_get_full_payload_round_trip` with the exact TypeError; restoring passes (55/55).
4. **`sdks/canary/python/scenarios/*.py` env-secret creates** — the API requires `metadata.var_name` for `env-secret` type (validation: "invalid secret metadata: env-secret requires metadata with var_name field"); the Go twins all use `CreateWithMetadata(..., {"var_name": "CANARY_VAR"})` but the Python scenarios passed no metadata. Added `metadata={"var_name": "CANARY_PY_VAR"}` to every env-secret create (s_secret_crud ×7 incl. the empty-name/duplicate/uppercase negative tests, s_secret_reveal, s_secret_audit, s_secret_bindings, s_ownership, and the 3 parked d_* twins), and `metadata: { var_name: 'CANARY_TS_VAR' }` to the 10 TypeScript env-secret creates (SDK already typed `metadata?: unknown`) — otherwise the identical deterministic failure reappears in the TS section of the same CI job.
5. **`sdks/canary/python/scenarios/s_ws_status.py`** — phase/conditions assertions relaxed to the Go twin's semantics: canary CI runs a controller-less kind cluster, so a freshly-created workspace has empty phase and possibly absent conditions; the contract is a 200 with parseable shape, not non-empty phase (`"phase" in st and isinstance(st["phase"], str)` — falsifiable, CI-green with `"phase": ""`; Python twin's stricter reading was the next deterministic false-failure once `s_ws_crud` passed). The TypeScript twin's `!== ''` conjunct got the same relaxation (`s-ws-status.ts:20`) so the false-failure doesn't reappear in the next CI section.
6. **`sdks/canary/python/scenarios/s_ws_crud.py`** — N3 empty-runtime now mirrors the Go twin (assert success + defaulted runtime); cleanup wrapped in try/except so a transient delete failure can't abort N5/N6 (Python raises where Go discards the error).

---

## Key Decisions

1. DTO-accurate over kitchen-sink: dataclasses mirror exactly what each endpoint emits; the in-file comment names the Go authority struct to prevent the next Metadata-vs-DTO confusion.
2. Round-trip tests pin the full server shape, so the next added server field fails in the SDK test suite first (not as a canary crash weeks later).
3. OpenAPI backfilled in the same PR — the spec is the cross-language source of truth.

---

## Tests Run

- `pytest sdks/python/tests/` — 87/87 pass (7 new: 2 workspace round-trips, 2 async round-trips, 1 unknown-field rejection, 2 SecretResponse round-trips) (sync round-trips, async round-trip twins for the duplicated async_client.py parse paths, unknown-field rejection test)
- Red/green mutation check on `devPreviewEnabled` — fails pre-fix, passes post-fix
- `py_compile` both touched Python files; `yaml.safe_load` on openapi.yaml

---

### Round 4: SecretResponse.globalDefault

The CI canary advanced past s_secret_crud's metadata fix to reveal `SecretResponse.__init__() got an unexpected keyword argument 'globalDefault'` — the same strict-expansion class; `globalDefault` is always emitted (no omitempty, `pkg/secrets/types.go:158`). Backfilled the field across all four surfaces per the PR's own parity standard: Python dataclass, Go SDK type (was silently ignoring it), TS interface, and the OpenAPI `SecretResponse` schema; added a full-payload round-trip test (86/86).

### Round 5: bindings/env-vars twin parity (review findings #5/#6)

- `s_secret_bindings` (py+ts): asserted `id`; the API's `BoundSecret` emits `secretId` (`pkg/secrets/types.go:170-178`) — the Go SDK passes only because its type remaps the tag. Fixed both twins to `secretId`, and the TS SDK's `getBindings` return type (`client.ts:218`) to match.
- `s_env_vars` (py+ts): asserted the raw var name; the API stores/returns mangled secret names `<wsId>-env-<lowercased_var>` (`workspace_env.go:121,185-193`), empirically confirmed by the Go twin's CI-passing suffix check. Fixed both twins to `endsWith/endswith("-env-canary_var")` per Go-twin parity.

### Round 6: TS bootstrap + DEK parity (2b01d700 follow-through)

- **TS canary import-path break**: all 35 scenarios imported `'../../src/index.js'` (resolves to `sdks/canary/typescript/src/`, nonexistent); the SDK lives at `sdks/typescript/src/`. Repointed to `'../../../typescript/src/index.js'`. Like the Python `sys.path` bug, this was latent — the TS section had never executed in CI (earlier Python failures aborted the job first).
- **Six TS secret scenarios → `await jwtLogin(cfg)`** (s-secret-crud, s-secret-reveal, s-secret-audit, s-secret-bindings, s-env-vars, s-ownership): the same DEK-gate 403 class fixed for the Python twins earlier; the `s-cred-crud.ts` pattern applied.
- **Async `SecretResponse` round-trip twin** added — the duplicated async parse path is now pinned like every other model (87/87).
- `sdks/typescript/src/client.ts:218` — `getBindings` type declares `secretId` (server shape).

### Round 7: N6 + rate-limit parity

- `s_ownership` N6 (py+ts): 403-or-404 access-denied contract (Go twin; CI-proven red at `2b01d700`'s run).
- `s-rate-limit.ts`: 8 → 12 burst attempts (Python-twin parity — the login limit is 10/min, so 8 may not trip it).

### Round 8: email-reset login 429

First-ever full Python section run: scenarios 1-16 green through `s_ownership`; `s_email_reset` (17 of 19) then failed on `login: unexpected status: got 429` — the accumulated logins across the section (12 POSTs to /auth/login across 10 scenarios — s_auth, s_logout, the seven JWT-authenticated scenarios (all seven already used jwt_login at this PR's base — the six were converted by #861, s_cred_crud earlier still; this PR converted only the TS twins), and this one) share the per-IP login bucket (10/min). The scenario's contract is the 200-vs-403 email-verification semantics, so the login now retries on 429 (up to 7 attempts, 10s apart — one bucket refill), in both the Python and TS twins (the TS section's first-ever execution runs the same drained-bucket arithmetic).

### Round 9: MCP canary bootstrap

With Python fully green and the TS section's first-ever execution also fully green, the job reached the final MCP section — never executed before either — and failed on `go run ./sdks/canary/mcp/` from the workspace root: the MCP canary is its own Go module (`sdks/canary/mcp/go.mod`), invisible to the root module. Fixed to the Go-canary pattern: `working-directory: sdks/canary/mcp` + `go run .`. The section's own first execution then surfaced its contract drift — tracked in #874 (not this PR's scope).

## Blockers

None for this PR's diff. The TS section's first-ever execution ran fully green (round 8 run), closing that risk. The remaining open item is the MCP section's first-ever execution, newly enabled by Round 9 — its five findings — tool-contract drift (24 vs 15, run_resolve collapse), ungated d-mcp-workspace, the DEK-gate 403 on credential_create, the ListCredentials envelope-unmarshal bug, and the workspace_create name-contract mismatch (tool schema optional vs API-required, CI-observed 422) — are tracked in #874 (out of #867 scope).

---

## Next Steps

- Python (round 8 run) and TS (same round 8 run — the section's first execution) were fully green for the first time. Remaining for the `sdk-canary.continue-on-error: false` flip (ci.yml TODO): the MCP section fixes tracked in #874.

---

## Files Modified

- sdks/python/llmsafespaces/types.py
- sdks/python/tests/test_client.py, sdks/python/tests/test_async_client.py
- sdks/openapi.yaml
- sdks/canary/python/scenarios/s_email_reset.py
- sdks/canary/python/scenarios/s_ws_crud.py, s_ws_status.py, s_secret_crud.py, s_secret_reveal.py, s_secret_audit.py, s_secret_bindings.py, s_env_vars.py, s_ownership.py, d_account_recover.py, d_change_password.py, d_key_rotate.py
- sdks/canary/typescript/scenarios/*.ts (35 files: import repoint), s-secret-{crud,reveal,audit,bindings}.ts + s-env-vars.ts + s-ownership.ts (jwtLogin), s-ws-status.ts (phase relaxation), s-rate-limit.ts (8→12 burst attempts, Python-twin parity — this PR's only change to the file)
- sdks/typescript/src/types.ts, sdks/typescript/src/client.ts
- sdks/go/types.go
- sdks/canary/typescript/scenarios/s-email-reset.ts (login 429 retry — Python-twin parity)
- .github/workflows/ci.yml (MCP canary working-directory fix — its own Go module)
- worklogs/0764_2026-08-15_python-sdk-workspace-model-drift.md (this file)
