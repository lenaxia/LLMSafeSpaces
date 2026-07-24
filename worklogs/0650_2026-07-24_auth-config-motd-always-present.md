# Worklog: Fix SDK canary failure — motd field omitted from /auth/config

**Date:** 2026-07-24
**Session:** Fix the `s-auth-config` SDK canary failure observed on PR #595 (and every PR since PR #594 enabled it).
**Status:** Complete

---

## Objective

The `SDK Integration (live API canaries)` CI job fails on every PR with `FAIL auth/config: motd field present`. The `motd` JSON key was omitted from `GET /api/v1/auth/config` when `instance.motd` was unset (the default on every fresh install), because `AuthConfig.MOTD` had `json:"motd,omitempty"`. All three SDK canaries (Go, TypeScript, Python) and the TESTPLAN document assert the field should always be present.

---

## Root cause

`pkg/types/auth.go:108`:
```go
MOTD string `json:"motd,omitempty"`
```

`omitempty` drops the key from the JSON response when the value is the empty string. On a fresh install where no admin has set `instance.motd`, the response is:
```json
{"instanceName":"LLMSafeSpaces","oidcEnabled":false,"registrationEnabled":true}
```
— no `motd` key.

The canary TESTPLAN (`sdks/canary/TESTPLAN.md:212`) documents the contract explicitly: `Response has motd (string, may be empty)`. All three SDK canaries assert presence:
- Go: `sdks/canary/go/scenarios/s-auth-config/main.go:62-63`
- TypeScript: `sdks/canary/typescript/scenarios/s-auth-config.ts:15`
- Python: `sdks/canary/python/scenarios/s_auth_config.py:28`

The existing unit test `TestAuthConfig_DefaultInstanceName_WhenNotSet` did not catch this because it unmarshals into a typed Go struct (`types.AuthConfig`) — the zero-value string satisfies `assert.Empty(t, cfg.MOTD)` regardless of whether the JSON key was present.

---

## Fix

One-line change: remove `omitempty` from the `MOTD` field tag so the key is always serialized (as `""` when unset). This matches the contract of the other always-present fields (`registrationEnabled`, `oidcEnabled`, `instanceName`) — none of which use `omitempty`.

`SSOProviders` retains `omitempty` — an empty/nil slice should be omitted, which is a different contract (slice vs scalar string).

Also added `required: [registrationEnabled, oidcEnabled, instanceName, motd]` to the `AuthConfig` OpenAPI schema (`sdks/openapi.yaml:2531`) to match the actual contract the canaries enforce.

---

## Why this surfaced now

PR #594 (`d43f9007`, "enable SDK integration tests — live API canaries") wired the `s-auth-config` canary into the CI workflow. The canary and the `omitempty` tag predate that PR by months — the bug was latent. Every PR since #594 hits this failure on the `SDK Integration (live API canaries)` check; it was visible on PR #595 and confirmed unrelated to that PR's changes (which don't touch AuthConfig, the /auth/config endpoint, or the canary).

---

## Tests Run

```
go test ./api/internal/server/... -run TestAuthConfig -v   # all 7 pass
go test ./api/internal/server/... ./pkg/types/... -count=1 # all pass
go vet ./api/internal/server/... ./pkg/types/...            # clean
```

---

## Files Modified

- `pkg/types/auth.go` — removed `omitempty` from `AuthConfig.MOTD` json tag.
- `api/internal/server/router_auth_config_settings_test.go` — new `TestAuthConfig_MOTDKeyAlwaysPresentInJSON` that unmarshals into `map[string]any` (like the canary does) and asserts key presence.
- `sdks/openapi.yaml` — added `required` list + description to `AuthConfig` schema.
- `worklogs/0650_2026-07-24_auth-config-motd-always-present.md` — this worklog.
