# Worklog: rememberMe Helm fix + instance-default /tmp/* auto-approval

**Date:** 2026-07-30
**Session:** Two production-reported UX issues: (1) remember-me sessions bouncing to /login daily, (2) agents prompting for /tmp/* permission on every session. Both shipped as PR #602.
**Status:** Complete

---

## Objective

1. **WS1 — rememberMe silently disabled in Helm deploys.** The chart's generated `config.yaml` only emitted `auth.tokenDuration`, never `auth.rememberMeDuration`. The Go auth service only honours the `rememberMe` flag when `RememberMeDuration > 0` (`auth.go:1072`); the Go zero-value (0) means the feature is disabled and falls back to `tokenDuration` (24h). So in every Helm-deployed install, users who checked "remember me" were silently downgraded to 24h and bounced daily.

2. **WS2 — instance-default `/tmp/*` auto-approval.** opencode prompts for permission on every path outside the `/workspace` project root (the `external_directory` permission). `/tmp/*` is a PVC subPath that holds no credentials (US-35.7 moved them to tmpfs), so prompting for it on every session is pure friction. The platform never injected any permission config into `agent-config.json` before.

---

## Assumptions (stated + validated)

| # | Assumption | Validation |
|---|-----------|------------|
| A1 | `/sandbox-runtime` tmpfs is mounted RW in both the init container (runs bootstrap) and main container (runs agentd) | `pod_builder.go:165` (main), `pod_builder.go:520` (init `credMounts`). Shared volume — same pattern as adminPrompt (#483). |
| A2 | opencode's `external_directory` permission accepts both bare-string and object-map forms | `testdata/opencode-config.schema.json:138` — `PermissionRuleConfig → anyOf(PermissionActionConfig, PermissionObjectConfig)`. |
| A3 | Default `/tmp/*` is safe (no credentials) | US-35.7 moved `agent-config.json` + `secrets-env` to tmpfs; `/tmp` PVC subPath holds only init scripts + package caches. |
| A4 | `/sandbox-cfg/*` must NOT be auto-approved | That emptyDir holds `secrets.json`, `password`, `workspace-config.json` in plaintext. |
| A5 | Init script doesn't need a new flag for `--allowed-dirs-out` | `pod_builder.go:501` calls `bootstrap` without `--out` overrides; default `agentd.AllowedDirsPath` kicks in automatically. |

---

## Work Completed

### WS1: Helm chart fix (3 files, 3 tests)

- `helm/values.yaml:104` — added `rememberMeDuration: 720h` (operator-configurable)
- `helm/templates/configmap-api.yaml:41` — render it into the ConfigMap
- `helm/chart_test.go` — 3 regression tests:
  - `TestAuth_RememberMeDuration_DefaultRender` — default render includes `720h`
  - `TestAuth_RememberMeDuration_CustomValue` — operator override flows through
  - `TestAuth_TokenDuration_DefaultRender` — existing tokenDuration contract locked in

### WS2: Instance-default /tmp/* auto-approval (5 layers, 17 tests)

**Layer 1 — Settings (`pkg/settings/`):**
- New instance setting `workspace.allowedExternalDirectories` (TypeStrings, default `["/tmp/*"]`). Schema v6→v7.
- Registered in `KnownKeys` as `KeyWorkspaceAllowedExternalDirs`.
- `TestInstanceSetting_AllowedExternalDirectories` pins default/type/registration + asserts `/sandbox-cfg` is NOT in the default.

**Layer 2 — AgentConfigWriter (`cmd/workspace-agentd/agent_config_writer.go`):**
- 5th source: `allowedDirs []string` + `allowedDirsPath string` (default `agentd.AllowedDirsPath`).
- `loadAllowedDirs()` reads the JSON array once at init; `rebuild()` merges each pattern into `mode.permissions.external_directory` as `{pattern: "allow"}`.
- Existing `mode`/`permission` siblings (bash, edit) preserved via deep-merge from `modeRaw` (captured in `loadExisting`).
- **Bare-string `external_directory` preserved** — if the existing value is a bare action string ("ask"/"allow"/"deny"), it is NOT overwritten with a map (would silently narrow a global policy). Only the map form or absent key triggers injection.
- **No empty `{}` noise** — `external_directory` key is only written when `len(allowedDirs) > 0`.
- 6 tests: emits rules, preserves existing mode, missing file no-op, bare-string preserved, empty-dirs no-noise, schema-valid.

**Layer 3 — agentd bootstrap subcommand (`cmd/workspace-agentd/bootstrap.go`):**
- `--allowed-dirs-out` flag (default `agentd.AllowedDirsPath`), symmetric with `--admin-prompt-out`.
- `bootstrapResponse.AllowedExternalDirectories` field; `fetchBootstrapSecrets` returns 5-tuple.
- Writes `/sandbox-runtime/allowed-dirs.json` as a JSON array; skips write when empty.
- 3 tests: writes dirs, empty=no file, default path.

**Layer 4 — API bootstrap handler (`api/internal/handlers/pod_bootstrap.go`):**
- `SetSettingsReader(bootstrapSettingsReader)` setter (local interface — avoids `pkg/interfaces`/`api/internal/interfaces` alias collision with `interfaces.LoggerInterface`).
- `bootstrapAPIResponse.AllowedExternalDirectories` populated from `settings.KeyWorkspaceAllowedExternalDirs`; non-fatal on settings error (pod boots, agents prompt as before).
- `HasSettingsReader()` wiring guard.
- 3 tests: returns dirs, settings error omits, no reader omits.

**Layer 5 — Production wiring (`api/internal/app/app.go`):**
- `podBootstrapHandler.SetSettingsReader(instanceSettings)` after `SetLogger`.
- `TestPodBootstrapHandler_SettingsReaderWired` in `secrets_wiring_test.go` guards the wiring call (mirrors `TestPodBootstrapHandler_LoggerWired`).

### Path constant (`pkg/agentd/types.go`)

- `AllowedDirsPath = "/sandbox-runtime/allowed-dirs.json"` — same tmpfs rationale as `AdminPromptPath` (#483).

---

## Adversarial Self-Review (Rule 11)

### Phase 1: Weaknesses identified

1. **Bare-string external_directory silently corrupted** — original code unmarshalled into `map[string]string`, and on failure (bare string) reset to empty map + overwrote. Comment said "preserved" but code overwrote. **FIXED** — now preserves bare strings, only injects on map/absent.
2. **Empty `external_directory: {}` noise** — when allowedDirs empty but modeRaw non-empty. **FIXED** — guarded behind `len(w.allowedDirs) > 0`.
3. **Constructor/loadAllowedDirs ordering** — doc said "must be called before" but constructor already calls it. **FIXED** — doc updated to describe actual double-call pattern.
4. **Missing wiring guard test** — no test enforced `SetSettingsReader` in app.go. **FIXED** — `TestPodBootstrapHandler_SettingsReaderWired`.

### Phase 2: Validation

- All findings confirmed real by code re-read. Fixtures: bare-string test now passes (preserved, not overwritten); empty-dirs test asserts no `external_directory` key.
- `/sandbox-runtime` mount validated at `pod_builder.go:165,520` — confirmed RW in both init and main containers.

### Phase 3: Zero real findings remain

---

## Automated Review Feedback

The PR-review AI (GitHub Actions) reviewed PR #602 and flagged:
1. ~~gofmt violation~~ — fixed (commit 860a7367)
2. ~~misspelling "honours" → "honors"~~ — fixed (commit 4d224e57)
3. **Bare-string comment/code mismatch** — fixed (this revision)
4. **Empty `{}` noise** — fixed (this revision)
5. **Missing `SetSettingsReader` wiring guard** — fixed (this revision)
6. **Missing worklog** — this file

Verdict was REQUEST CHANGES; all items addressed.

---

## Decisions

1. **Default = `/tmp/*` only.** `/sandbox-cfg/*` deliberately excluded (plaintext credential emptyDir). Operators can extend via admin UI; schema description warns against credential paths.
2. **Local interface for `bootstrapSettingsReader`** instead of importing `api/internal/interfaces.SettingsReader`. The file already aliases `pkg/interfaces` as `interfaces` for `LoggerInterface`; importing the internal package too would collide. Matches the file's existing local-interface pattern (`TokenReviewer`, `bootstrapInjector`).
3. **No init script change needed.** Bootstrap already runs before materialize (`pod_builder.go:501-502`); the default `--allowed-dirs-out` flag value is correct.

---

## Files Modified

- `helm/values.yaml` — added `rememberMeDuration: 720h`
- `helm/templates/configmap-api.yaml` — render `rememberMeDuration`
- `helm/chart_test.go` — 3 chart tests (WS1) + misspelling fix
- `pkg/settings/schema.go` — new setting def, schema v7
- `pkg/settings/registry.go` — `KeyWorkspaceAllowedExternalDirs`
- `pkg/settings/schema_test.go` — setting contract test
- `pkg/agentd/types.go` — `AllowedDirsPath` constant
- `cmd/workspace-agentd/agent_config_writer.go` — 5th source + mode merge
- `cmd/workspace-agentd/agent_config_writer_test.go` — 6 writer tests
- `cmd/workspace-agentd/bootstrap.go` — `--allowed-dirs-out` flag + response field
- `cmd/workspace-agentd/bootstrap_test.go` — 3 bootstrap tests
- `api/internal/handlers/pod_bootstrap.go` — `SetSettingsReader` + response field
- `api/internal/handlers/pod_bootstrap_test.go` — 3 handler tests
- `api/internal/app/app.go` — production wiring
- `api/internal/app/secrets_wiring_test.go` — wiring guard test
- `worklogs/0653_2026-07-30_rememberme-helm-fix-and-tmp-permission-default.md` — this file

---

## Next Steps

1. Merge PR #602 after CI green + review approval.
2. Post-merge: verify in production that remember-me sessions last 30d (check `exp` claim in browser DevTools after login).
3. Post-merge: verify agents stop prompting for `/tmp/*` (observe no `permission.asked` events for `/tmp/*` in SSE stream).
4. Worklog will be auto-numbered by the post-merge bot.
