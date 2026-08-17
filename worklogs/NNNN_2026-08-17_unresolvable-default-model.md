# Worklog: unresolvable default model must not poison every prompt (incident 2026-08-16)

**Date:** 2026-08-17
**Session:** Incident fix — workspace 946a442f / session ses_ff31324deffeJS0tn4774k0Kl6: every prompt returned 502 `failed to send message`; opencode logged `ProviderModelNotFoundError: Model not found: deepseek-v4-flash-free/.` (refs err_661e326d, err_de616758). Root cause: persisted default model referenced a relay-only provider that did not exist at boot (one-shot relay injector failed: `decode /provider: unexpected EOF` → deadline exhausted → skipped), and `applyWorkspaceConfig` wrote the flat ID bare. opencode splits the model string on "/" — a bare ID parses as providerID with an EMPTY modelID, so every prompt in every session failed until pod rebuild. Compounding: the per-prompt model selector the frontend already sends was dropped by `SendPromptAsync` (`session.SendOpts{}` hardcoded), so the user could not route around the poison.
**Status:** Complete

---

## Objective

1. Never write a model value opencode is guaranteed to reject; degrade to omission (opencode default) + a user-visible warning.
2. Forward the per-prompt model selector end-to-end so interactive selection works regardless of the persisted default.

## Root-cause chain (validated)

1. API log: `POST .../prompt` → 502, `SendPromptAsync: adapter failed ... returned 500 {"name":"UnknownError","ref":"err_661e326d"}` (23:34:38, 23:35:32).
2. opencode in-pod log (`/workspace/.local/opencode/log/opencode.log` lines 5461/5464): `ProviderModelNotFoundError: Model not found: deepseek-v4-flash-free/.` at `SessionPrompt.getModel` — the `/.` suffix is the empty modelID after provider split.
3. In-pod state: `/sandbox-cfg/workspace-config.json` = `{"defaultModel":"deepseek-v4-flash-free"}`; `/sandbox-runtime/agent-config.json` `"model": "deepseek-v4-flash-free"` (bare); connected providers = `[opencode thekaocloud]` — no relay provider.
4. agentd boot log: relay injector failed 4× (`decode /provider: unexpected EOF`), 30s deadline exhausted, `skipping` — one-shot per pod lifetime, so `opencode-relay` never existed this boot.
5. API request log: frontend sends `{"model":{"modelID":"glm-5.3","providerID":"thekaocloud"},...}` with every prompt — handler discarded it.

## Work Completed

### agentd (`cmd/workspace-agentd`)

- `secrets.go` `applyWorkspaceConfig`: when `resolveModelWithProvider` cannot resolve the flat default to a provider, the `model` key is **deleted** (any stale bare value included) instead of written bare; a warning marker (`{"defaultModel":...}`) is written beside agent-config.json. Successful resolution writes the qualified form and **removes** any stale marker (self-heals when the relay returns). The false comment ("per-prompt model override still routes correctly") replaced with the incident account.
- `healthz.go`: `healthzHandler` gains `modelWarnPath`; new `modelResolutionWarnings` renders the user-facing warning (`default model "X" unavailable — using the agent default model`). Absent/corrupt marker → no warnings; never affects `Healthy`.
- `server.go`: `buildStatuszHandler` gains `modelWarnPath`; both handlers wired with `agentd.ModelResolutionWarningPath`.

### shared types (`pkg/agentd`)

- `ModelResolutionWarningPath = "/sandbox-runtime/model-resolution-warning.json"` — same tmpfs lifecycle as `ReloadSecretsCachePath` (survives container restart, wiped on pod death, matching the agent-config.json it describes).
- `Warnings []string` added to `HealthzResponse` and `StatuszResponse` (observability only).

### controller (`controller/internal/workspace/health.go`)

- `appendAgentWarnings` suffixes warnings onto BOTH AgentHealthy condition message writers (liveness "agentd alive, uptime=Ns" and deep-status "connected=... version=...") — the deep-status rewrite would otherwise intermittently erase the warning. Warning text contains no `key=value` tokens, so the API's `connectedRe`/`versionRe`/`configuredRe` parsers are unaffected (validated: regexes match anywhere; test asserts both message forms).

### API (`api/internal/handlers/proxy_handlers.go`)

- `extractPromptModel`: parses the `{"model":{"modelID","providerID"}}` selector into the contract `session.ModelRef`. Absent/empty/malformed → nil (session default); malformed never rejects the prompt.
- `SendPromptAsync` adapter path forwards `session.SendOpts{Model: extractPromptModel(bodyBytes)}`.

### adapter (`pkg/agent/opencode/adapter.go`)

- `qualifiedModelID`: renders ModelRef as opencode's `"providerID/modelID"` string form (verified against the incident's own stack trace — opencode splits on "/"). Bare unqualified ID → "" (omit; session default applies) — sending it bare reproduces the incident. Already-qualified ID passes through.

## Key Decisions

1. **Omit, don't guess**: on unresolvable default, omitting the model key (opencode applies its own default) is strictly safer than writing any value; falling back to another provider's model would silently change user intent. Selection self-heals on the next boot where the provider exists.
2. **Warning rides the existing pipeline**: marker file (materialize is a separate process — tmpfs files are the established IPC: `last-reload-secrets.json`, `allowed-dirs.json`) → healthz/statusz → AgentHealthy condition message → API `agentHealth.message` → existing frontend rendering. No new endpoints, no schema changes, no frontend work.
3. **Opencode form knowledge stays in the seam** (Rule 12): the handler passes the contract `ModelRef`; only `pkg/agent/opencode` knows the string form.
4. **Bare per-prompt ID degrades to session default** rather than the handler resolving providers from the catalog — the frontend always sends providerID (validated in live request logs); catalog resolution on the hot path would couple the proxy to catalog I/O and duplicate the adapter's job.

## Known edge (documented, not fixed here)

A default model already stored QUALIFIED (`provider/model`) whose provider is absent this boot passes through `resolveModelWithProvider` and would still fail resolution inside opencode — same poison class, out of incident scope; `SetModel` persistence validation (fix 3) was explicitly descoped by the user.

## Assumptions (stated + validated)

1. opencode parses the per-message `model` field as a `providerID/modelID` string — validated from the incident stack trace (`SessionPrompt.getModel`, split semantics evidenced by `deepseek-v4-flash-free/.`).
2. agent-config.json is rebuilt from scratch every pod boot (tmpfs; FlushProviders writes fresh) — no durable stale-qualified model key can survive; deleting on unresolvable is correct. Verified against the volume-layout table (README §Relay Config Subsystem).
3. The frontend renders `agentHealth.message` — validated from the live /status response consumed by the chat UI.
4. Condition message append is safe for API regex parsing — validated by reading `agentHealthFromConditions` (workspace_service.go:1934) — regexes are unanchored `FindStringSubmatch`; warning text has no `=` tokens.

## Tests Run

- `go test ./cmd/workspace-agentd/...` — ok (366s, full suite incl. new: UnresolvableModel_OmittedWithWarning, Resolvable_RemovesStaleWarning, HealthzHandler_SurfacesModelResolutionWarning [3 subtests], BuildStatuszHandler_SurfacesModelResolutionWarning)
- `go test ./controller/...` — ok (new: TestCheckAgentHealth_WarningsAppendedToCondition)
- `go test ./api/internal/handlers/...` — ok (103s; new: ForwardsModelOverride, NoModelInBody_NilOptsModel, TestExtractPromptModel [6 cases])
- `go test ./pkg/agent/... ./pkg/agentd/... ./pkg/session/...` — ok (new: TestAdapter_Send_ModelReferenceForm [4 subtests])
- `golangci-lint run` on all touched packages — 0 issues; `go vet` clean; `gofmt` clean

## Files Modified

- `cmd/workspace-agentd/secrets.go`, `secrets_test.go`, `healthz.go`, `healthz_test.go`, `has_user_creds_test.go`, `server.go`, `main_test.go`
- `pkg/agentd/types.go`
- `controller/internal/workspace/health.go`, `health_test.go`
- `api/internal/handlers/proxy_handlers.go`, `adapter_path_test.go`
- `pkg/agent/opencode/adapter.go`, `adapter_test.go`
- `README-LLM.md` (boot-sequence line: model key conditional)

## Remediation (live workspace 946a442f)

Suspend → resume to force a fresh materialize with the current image; with relay-router healthy since 20:48 the free-models fetch is expected to succeed, `opencode-relay` gets written, `deepseek-v4-flash-free` resolves qualified, prompts work. Verified post-resume in-pod.

## Review round 1 (AI reviewer, REQUEST CHANGES → addressed)

1. **`versionRe` regression (validated by reviewer)**: appending `"; warnings: …"` right after `version=%s` made `version=(\S+)` capture `"1.18.10;"` into DB + frontend. Fixed: regex anchored to `[^\s;]+`; my comment claiming parsers were unaffected was wrong and is replaced; new parse test with warnings-suffixed message pins exact AgentVersion AND the structured field.
2. **Warning never reached the user (validated)**: the only `agentHealth.message` consumer (`HealthBanner.agentLabel`) returns null when status "Healthy" — and warnings ride `AgentHealthy=True` only. My "validated from live /status" validated consumption, not rendering. Fixed end-to-end: `AgentHealthResult.Warnings` (structured, parsed from the suffix) + `AgentHealth.warnings` frontend type + HealthBanner renders warnings when Healthy (3 new tests). The banner now shows e.g. `default model "deepseek-v4-flash-free" unavailable — using the agent default model`.
3. **E2E model forwarding (checklist-mandatory)**: `TestE2E_Adapter_SendPromptAsync_ModelForwarding` through the real handler→adapter→fake-opencode pipeline — asserts `"model":"thekaocloud/glm-5.3"` on the backend body (a swapped-field mapping at any seam now fails), plus absent-selector and malformed-selector degradation subtests.
4. **`/message` asymmetry**: `SendMessage` now forwards the same selector (extractMessageText returns body bytes; same policy check). SDK bodies today carry no model — symmetric and future-proof.
5. **`qualifiedModelID` double-prefix**: slash-bearing IDs are treated as already qualified regardless of Provider (was `"x/x/y"`, a hand-crafted-body reachable failure). New subtest.
6. **Round-trip pin**: `TestModelResolutionWarning_RoundTrip` (write→read, no-semicolon contract).

Verification after round 1: full Go suites green (handlers 116s, workspace-agentd, controller, pkg/...), golangci-lint 0 issues, frontend tsc + 1664 vitest tests green.
