# Worklog: #1575 — applyModelAllowlist delivers provider models named "default"

**Date:** 2026-10-03
**Session:** Fix the delivery-time allowlist filter that hard-dropped any provider model whose ID is literally "default"; red-first pin; synthesis-branch mis-form skip preserved
**Status:** Complete

---

## Objective

`applyModelAllowlist` (pkg/secrets/injection.go) skipped `id == "default"` while building the allowed set, so a provider legitimately serving a model named `default` (production case: TheKaoCloud, confirmed live via `GET /v1/models`) could never deliver it to any workspace — no allowlist edit could fix it (#1575).

---

## Work Completed

### Triage: the selector is a separate mechanism

- The platform's default-model **selector** is `workspace-config.json`'s `defaultModel` (qualified `providerID/modelID` or legacy flat), resolved by `resolveModelWithProvider` (`cmd/workspace-agentd/secrets.go:859`) against the rendered provider map. It never flows through `ModelAllowlist`. The pre-drop therefore protected nothing selector-side; it only stripped a legitimate catalog entry.
- The allowlist's job is filtering the model **catalog**. `pd.Models` is the live-fetched provider model list (model enricher, `cmd/workspace-agentd/model_enricher.go` — `GET /v1/models` on custom OpenAI-compatible endpoints). An id that survives the intersection exists upstream by construction; "default" as a catalog entry is a real model.
- `FormatOpenCodeConfig` renders model IDs verbatim (`op.Models[m.ID]`) with no `default` special-casing downstream.

### The fix (TDD)

- RED first: `TestCredentialPrecedence_AllowlistDefaultIDIsCatalogEntry/live_catalog_model_named_default_is_allowed_and_delivered` failed on the pre-fix tree with exactly the production symptom — delivered set `[{glm-5.1}]` instead of `[{default} {glm-5.1}]`.
- `applyModelAllowlist`: the allowed-set build now skips only `id == ""` (never a real id). The literal `"default"` is allowed — the catalog intersection decides.
- The mis-formed-artifact skip moved to the **synthesis branch only** (issue ask #1, option 2): when no live list vouches for allowlist IDs (`pd.Models` empty), `""` and `"default"` are still not synthesized — an allowlist "default" with nothing backing it is the stale selector literal from a mis-formed create request; synthesizing it produced `"models":{"default":{}}` blocks opencode read as an unconfigured provider (0 models). With a live catalog present the intersection already filters stale entries, so "default" survives only when the provider actually serves it.

### Pins (both poles)

- `live catalog model named default is allowed and delivered` — allowlist `["default","glm-5.1"]` over a live catalog `[default, glm-5.1, bge-m3]` → `[default, glm-5.1]`. RED pre-fix.
- `live catalog model named default filtered out when unlisted` — allowlist `["glm-5.1"]` → `[glm-5.1]`; "default" gains no special pass (characterization, green pre-fix).
- Existing synthesis-branch pins unchanged and green: `TestCredentialPrecedence_AllowlistOnlyDefaultID` (allowlist `["default"]`, no catalog → provider present, models empty), `TestCredentialPrecedence_AllowlistMixedValidAndInvalid` (`["default","",glm-5.1,gpt-4o]` → only the two real ids synthesized).

### Issue ask #3 check (related surfaces)

Grepped the acceptance path and UI: no handler, probe, or frontend credentials surface special-cases the literal `default` (`api/internal/handlers/*credential*.go`, `frontend/src` credentials components). The DB accepts the entry and the delivery filter was the only disagreeing surface — now aligned. The empty-string id remains skipped everywhere (truly invalid at both accept and delivery; create-time rejection of `""` is a separate, non-blocking hardening the issue floats — not needed for this fix since `""` is inert in every branch).

---

## Key Decisions

- **Gate the skip to the synthesis branch rather than removing it entirely.** The two existing pins document a real failure mode (`"models":{"default":{}}` → opencode 0 providers) that lives entirely in the synthesis branch, where no live list vouches for IDs. Removing the skip there would regress that case for zero benefit — a live catalog containing "default" never reaches synthesis (the intersection delivers it).
- **Selector path untouched.** `resolveModelWithProvider` semantics (flat/qualified resolution, provider-existence strictness) are pinned by their own tests in `cmd/workspace-agentd/secrets_test.go` — all green post-fix.

### Assumptions stated and validated (Rule 7)

1. `pd.Models` is the live-fetched catalog for custom endpoints — verified: `model_enricher.go` populates it from `GET /v1/models`; first-party keys carry empty Models (catalog merged config-side by opencode), so they take the synthesis branch where the skip is preserved.
2. The selector never flows through `ModelAllowlist` — verified: selector resolution reads `workspace-config.json` + the rendered agent-config provider map (`applyWorkspaceConfig`/`resolveModelWithProvider`), a different file and path.
3. nil vs empty `pd.Models` (the allowlist-`["default"]`-only edge now returns an empty non-nil slice from synthesis) is wire-identical — verified: `json:"models,omitempty"` omits both; `FormatOpenCodeConfig` branches on `len()`.
4. No other delivery surface special-cases model id "default" — verified by grep across `pkg/`, `cmd/`, `api/internal/handlers`, `frontend/src` credentials surfaces.

---

## Blockers

None.

---

## Tests Run

- `go test -count=1 -run 'TestCredentialPrecedence_AllowlistDefaultIDIsCatalogEntry' ./pkg/secrets/` — RED pre-fix (delivered `[glm-5.1]`, missing `default`), GREEN post-fix.
- `go test -count=1 -run 'TestCredentialPrecedence_' ./pkg/secrets/` — ok (full precedence family incl. the two preserved synthesis pins).
- `go test -count=1 -run 'TestCredentialPrecedence_Allowlist|TestResolveModelWithProvider|TestApplyWorkspaceConfig' ./pkg/secrets/ ./cmd/workspace-agentd/` — all PASS (selector-side pins unchanged).
- `go test -count=1 -timeout 600s ./pkg/secrets/` — ok (full package, 12.6s).
- `go test -count=1 -run 'TestStaging|TestLLMProviders' ./controller/internal/workspace/` — ok (ResolveLLMProviders consumer side).
- `gofmt -l pkg/secrets/` clean; `go vet ./pkg/secrets/` clean.

---

## Next Steps

- PR review iterate to APPROVED; then #1565 (same lane): apply the #1561/#1564 strict-decode convention to `workflow_execute.go` and `user_timezone.go`.

---

## Files Modified

- `pkg/secrets/injection.go` — `applyModelAllowlist`: allowed-set skip narrowed to `""`; `"default"` skip gated to the synthesis branch.
- `pkg/secrets/credential_precedence_test.go` — new `TestCredentialPrecedence_AllowlistDefaultIDIsCatalogEntry` (two poles).
- `worklogs/NNNN_2026-10-03_allowlist-default-model-id.md` — this worklog.
