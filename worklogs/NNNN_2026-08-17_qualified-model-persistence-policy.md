# Worklog: qualified default-model persistence + org-policy enforcement on selection (2026-08-16 incident follow-up)

**Date:** 2026-08-17
**Session:** Follow-up to PR #909 (incident 2026-08-16). #909 built the last line of defense (omit+warn at boot, per-prompt override forwarding). This work closes the gaps identified in the post-mortem review: (1) the persistence format that made boot re-resolution fragile, (2) org allowed-models/providers enforced by hiding only, (3) the V2 path's model-blindness, (4) assessment of the one-shot relay injector (#910) and the relay-router outage (#911).
**Status:** Complete

---

## Objective

1. **A — Qualified persistence**: SetModel persists `providerID/modelID`; boot resolution becomes a deterministic provider-presence check; the qualified-but-absent edge (same poison class as the incident) is closed at materialize.
2. **B — Org policy on SetModel**: explicit selection of a policy-disallowed model/provider → 403 (was: accepted and persisted; only ListModels filtered).
3. **C — Org policy on per-prompt override**: the prompt body's model selector is policy-checked (was: fully unvalidated pass-through).
4. **D — V2 SendAsync model forwarding**: same model-form contract as V1 Send (path dormant on opencode 1.18.10, #755; must not replay the incident on revival).
5. **E — Injector re-trigger**: assessed, deferred with design notes → issue #910. Relay-router outage → issue #911.

## Work Completed

### A. Qualified persistence

- `api/internal/handlers/models_handler.go` `SetModel`:
  - Accepts both request forms: flat (`gpt-5.5`) and qualified (`openai/gpt-5.5` — round-trip compatibility once the DB stores qualified; catalog remains ground truth for the provider, request prefix is advisory). Slashed CATALOG model IDs (OpenRouter-style `vendor/model`) are accepted verbatim — validation and resolution key on the full value when the catalog lists it (`modelExists` discriminator; round 3, finding 2).
  - Persistence rule: the **catalog-resolved** qualified form is persisted when the catalog resolves (the same value policy-checked and live-pushed); a qualified request on the degraded path (catalog unavailable) persists verbatim (its prefix is policy-checked); flat+catalog-unavailable degrades to flat (unchanged optimistic behavior).
  - Response contract unchanged (`model` echoes the request); new `persistedModel` field exposes the stored form.
  - `ListModels`: `GetDefaultModel` may return qualified or legacy flat — the qualified prefix is split (LastIndex) to derive `currentModelProviderID`, `currentModel` response stays flat for client compatibility, `markSelected` matches flat.
- `cmd/workspace-agentd/secrets.go`:
  - `resolveModelWithProvider` → `(string, bool)`. Qualified input: provider-entry-presence check (deterministic; `LastIndex` split). Flat input: provider scan as before. Unresolvable in ANY form (flat unclaimed, qualified-absent, missing/malformed provider map) → `("", false)` — fail-safe: unverifiable means do-not-write.
  - `applyWorkspaceConfig` now omits+warns for qualified-but-absent defaults too (was: passthrough — the same poison as the bare ID).
  - **Materialize order on BOTH paths** (round 2: the zero-credential early path — the incident's exact user shape — was caught by the reorder e2e still resolving before the relay block; `applyPreBootRelay` extracted and shared). New order: providers → MCP → pre-boot relay → model. NOTE: the early path now also fail-fasts (exit 3) on a hard catalog/write failure, matching the main path — acknowledged as newly coupling zero-credential boots to catalog health (round 3, finding 5).
- No DB migration: existing flat rows keep working through the omit+warn path and convert to qualified lazily on the next SetModel.

### B. Org policy on SetModel

- `modelAllowedByOrgPolicy(ctx, workspaceID, flatModelID, providerID)` on ModelsHandler: nil deps → allow; no org → allow; policy-infra error → fail open + warn (matches `filterByOrgPolicy`); else `IsModelAllowed(flat) && IsProviderAllowed(provider)`. Denial → 403 before persistence or live push.

### C. Org policy on per-prompt override

- `ProxyHandler.modelPolicyChecker` + `SetModelPolicyChecker` (pre-Start setter, same race invariant as `SetAdapter`). Org ID comes from the already-resolved workspace CRD (`Spec.Owner.OrgID`) — zero extra round-trips on the prompt path.
- `SendPromptAsync`: override extracted first, policy-checked before `adapter.Send`; denial → 403 `model not allowed by organization policy` (adapter never called). No override → policy not consulted (session default already screened by SetModel).

### D. V2 model forwarding

- `client_v2.go`: `v2PromptBody.Model` (`omitempty`); `PromptV2WithModel(...)`; `PromptV2` delegates with `""`. Deliberately NOT added to the `agent.V2SessionClient` interface — the adapter holds the concrete `*opencode.Client`, so no fake churn (Rule: seam knowledge stays in the opencode package).
- `adapter.go` `SendAsync`: `qualifiedModelID(opts.Model)` forwarded; bare → omitted. Same contract as `Send`.

### E. Deferred (tracked)

- **#910** re-armable injector: success path kills opencode (session-aware deferred kill); re-arming means mid-life restarts/SSE drops interacting with kill deferral — needs a design pass, not a bolt-on. With A–D the failure mode is graceful.
- **#911** relay-router `/provider` cluster-wide timeout: the observed incident trigger; router logs zero errors (handler hangs — suspected missing upstream read timeout).

## Key Decisions

1. **Qualified-in-request persists verbatim** (no strip-and-reresolve on the degraded path): the client-supplied provider context is better than nothing; boot verifies it. On the healthy path the catalog re-resolves and may correct the prefix.
2. **Fail-safe resolver contract** (`ok=false` for unverifiable) instead of string-identity checks — the incident's lesson generalized: any value we cannot verify against this boot's provider set must not be written.
3. **Fail-open policy on infra errors** across all three enforcement points (List/Set/prompt) — governance filter, not an availability gate; consistent with existing `filterByOrgPolicy` semantics. Denial requires a definitive policy answer.
4. **403 with explicit error on prompt-path denial** (vs silently dropping the override): the user stated intent explicitly; silent default-routing would be confusing and hide the policy from them.
5. **Response `model` echoes the request** — pinned by existing tests, zero client-visible change; `persistedModel` is additive.

## Assumptions (stated)

1. `SetModel`'s PATCH `/global/config` and the DB both receiving the qualified form is consistent: agentd's `applyWorkspaceConfig` already required `providerID/modelID` in agent-config.json (pinned since Epic 35).
2. Model IDs do not legitimately contain `/` in a way that breaks `LastIndex` splitting (opencode's own wire format is `provider/model`, so this is its convention too).
3. Frontend ignores unknown response fields (additive `persistedModel`) and uses `currentModel`+`currentModelProviderID` (flat) as today.
4. The materialize reorder is safe because the pre-boot relay writer preserves foreign keys in agent-config.json (`loadExisting` captures providers+model+mcp as initial sources) — verified by reading `pre_boot_relay.go` and by the existing pre-boot relay e2e tests still passing.

## Review round 2 (4 findings → all addressed)

1. **Policy checker unwired in production (blocker)** — `SetModelPolicyChecker` had zero production call sites: enforcement was test-only dead code. Fixed: `wirePolicyEnforcement(policySvc, modelsHandler, proxyHandler)` extracted in app.go (testable like `newWorkflowAgentdExecutor`), `HasModelPolicyChecker()` probe added, and `TestWirePolicyEnforcement_ReachesProxyHandler` fails if the proxy leg is dropped again.
2. **Pod-down/catalog-failure policy bypass** — the check was nested in `if catalog != nil`. Fixed: `modelSelectionAllowedByOrgPolicy` runs on every path; model axis always enforced (flat ID is catalog-independent), provider axis enforced against the resolved provider AND the request's own prefix.
3. **Check-vs-persist provider mismatch** — qualified request "evil/m" resolving to openai persisted `evil` verbatim (never policy-checked; restart routing flipped to it). Fixed: on the healthy path the catalog-resolved form is persisted (identical to what was checked and live-pushed); the request prefix is policy-checked on the degraded path where it IS persisted.
4. **Provider-less override asymmetry** — `{"model":{"modelID":"x"}}` 403'd under any allowed_providers policy while routing identically to a session default. Fixed: provider axis skipped when `m.Provider == ""`.
5. **Commit-message hygiene** — round-2 interim commit carried a byte-identical copy of the #909 message; branch restructured into a single accurate commit.

**Bonus (e2e caught a real bug):** the materialize-reorder e2e (`TestMaterializeSubcommand_RelayQualifiedDefault_ResolvesAfterRelayBoot`, real subcommand binary + `INFERENCE_RELAY_BASEURL` + free-models catalog via new `LLMSAFESPACES_FREE_MODELS_PATH` env override) revealed the ZERO-CREDENTIAL early path still resolved the model before the pre-boot relay — exactly the incident's user shape (free models, no own providers). Fixed: `applyPreBootRelay` extracted and run on both paths before `applyWorkspaceConfig`.

Also: stale "flat catalog ID" doc comment replaced; API qualified-ID splits aligned to LastIndex (agentd's convention); SetModel policy fail-open pinned.

## Review round 3 (#913 → addressed) — convention: FIRST-segment splits everywhere

Split convention adopted across the whole model pipeline (round 4 refined round 3's alignment): **the FIRST "/" separates provider from model-ID** — opencode's own routing rule, proven by the incident itself (bare `deepseek-v4-flash-free` parsed as first-segment provider + EMPTY modelID). So `a/b/c` routes via provider `a` with model ID `b/c`. Applied in: `modelOverrideAllowed` (prompt paths), `modelSelectionAllowedByOrgPolicy` (SetModel), `resolveModelWithProvider` (agentd boot), ListModels/SetModel request+DB splits. (Round 3 initially aligned some sites to LastIndex; round 4's two-slash test proved that breaks the OpenRouter shape — `openrouter/anthropic/claude` would demand a provider literally named `openrouter/anthropic` — so first-segment is now universal and red/green-pinned at every site.)

1. **Slash-bearing modelID provider bypass on the prompt path (blocker, empirically confirmed by the reviewer)** — `{"modelID":"deniedprov/x","providerID":"openai"}` passed allowed_providers because the embedded prefix was never checked while the adapter forwards slash-bearing IDs verbatim. Fixed in `modelOverrideAllowed`: the embedded first-segment prefix is policy-checked alongside the explicit providerID. Round 4: the first regression test set AllowedModels and denied on the model axis before the provider check ran (passed pre-fix); the committed shape leaves AllowedModels unset so the provider axis alone carries the denial — verified red pre-fix, green post-fix.
2. **Slashed catalog model IDs unselectable (major)** — SetModel's flat split made ListModels-advertised `vendor/model` IDs 400. Fixed: catalog validation and resolution key on the FULL request value when `modelExists` matches it; policy model axis checks both forms; persistence stores `provider + slashed ID`. Round 4: the persisted two-slash value now also round-trips BOOT-side — `resolveModelWithProvider` verifies the first-segment provider (LastIndex looked for `openrouter/anthropic` and omit+warned on every reboot); red/green-pinned.
3. **False denial on the healthy path (round 4)** — the raw-request prefix is now policy-checked ONLY on the degraded path (where rawReq is what persists); on the healthy path the resolution routes AND persists, so an advisory mismatched prefix is not load-bearing (`TestSetModel_RequestPrefixDiffersFromResolution` updated to pin 200 + resolution-following persistence).
4. **403 denial leaked an active-session slot (minor)** — both prompt paths now `removeActiveSession` on denial, matching the quota and adapter-error paths. Test pins slot release through the real handler.
5. **Zero-credential boots newly fail-fast on hard catalog failure (low)** — acknowledged: the early path shares `applyPreBootRelay`'s exit-3-on-hard-failure semantics with the main path (corrupt catalog / unwritable config is a real bug for every user class; CrashLoop surfaces it). Soft failures remain non-fatal skips.
6. **Worklog** — this section rewritten for round-4 accuracy (persistence prefers the catalog resolution on the healthy path; early path shares the relay step; first-segment convention everywhere).
7. **`LLMSAFESPACES_FREE_MODELS_PATH` env seam** — documented in-code as controller-controlled operator/test surface (pod env is never user-settable).
8. **Legacy non-adapter `/message` branch proxies selectors unchecked (info, round 4)** — dead in production (`SetAdapter` unconditional in app.go); noted for the adapter-migration completion.

## Tests Run (round 2)

`go test ./api/internal/handlers ./api/internal/app ./cmd/workspace-agentd ./pkg/agent/opencode` — all green (new: wiring 2, SetModel pod-down axis ×3, mismatch persistence, fail-open, provider-less skip, relay-reorder subcommand e2e).

## Tests Run (round 1)
- `go test ./cmd/workspace-agentd/` — ok, 364s (new: resolver 8 subtests incl. qualified-absent/relay-present, applyWorkspaceConfig qualified-default-omitted)
- `go test ./api/internal/handlers/` — ok, 112s (new: SetModel persists-qualified / accepts-qualified / flat-degraded / policy 403×2 / policy-allowed; ListModels qualified-in-DB; prompt override policy 6 tests)
- `go test ./pkg/agent/...` — ok (new: SendAsync model form 2 subtests)
- `golangci-lint` 0 issues; vet/gofmt clean

## Files Modified

- `api/internal/app/app.go`, `policy_enforcement.go`, `policy_enforcement_wiring_test.go` (round 2)
- `api/internal/handlers/models_handler.go`, `proxy_handlers.go`, `proxy.go`, `models_test.go`, `adapter_path_test.go`
- `cmd/workspace-agentd/secrets.go`, `secrets_test.go`, `pre_boot_relay.go` (round 2)
- `pkg/agent/opencode/client_v2.go`, `adapter.go`, `adapter_test.go`
