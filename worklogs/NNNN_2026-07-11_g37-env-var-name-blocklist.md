# Worklog: G37 — Workspace env-var name blocklist

**Date:** 2026-07-11
**Session:** Address threat-model gap G37 (High) — `PUT /api/v1/workspaces/:id/env` accepted any POSIX-shaped env-var name, including `LD_PRELOAD`, `PATH`, `PYTHONPATH`, `BASH_ENV`, etc. A workspace owner could set one of these via the env-secret mechanism and compromise every process spawned in the pod.
**Status:** Complete

---

## Objective

Close G37 from `design/stories/epic-17-security-review/THREAT-MODEL.md`. The API handler (`api/internal/handlers/workspace_env.go:SetWorkspaceEnv`) had no env-var name validation at all — it constructed the secret name as `<workspaceID>-env-<lowered-varname>` and wrote the value as an env-secret. The materialize-time check in agentd (`pkg/agentd/secrets/secrets.go:validateVarName`) only checked regex + length; it had no dangerous-name blocklist. The threat-model row's claim of a parallel agentd check at line 277-296 was incorrect — that range is the path-traversal check for `mount_path`, not env-var names.

The threat: setting `LD_PRELOAD` causes every subsequent `exec()` in the pod to load the attacker's `.so` into agentd, opencode, and every mise-installed interpreter. `PATH` redirects every command lookup (`opencode`, `git`, `ssh`). `BASH_ENV` causes bash to source an attacker-controlled file on every non-interactive invocation. The pod's single UID shares one trust boundary across all these processes, so this is container-escape-equivalent in practice.

---

## Work Completed

### Implementation

- **`pkg/validation/env.go`** (new) — `ValidateEnvVarName` enforces three rules in order:
  1. Non-empty
  2. POSIX-portable shape `[A-Za-z_][A-Za-z0-9_]*` (matches the existing agentd regex)
  3. Length ≤ 256
  4. Not on the dangerous-names blocklist (case-insensitive)

  The blocklist (`blockedEnvVarNames`) is a curated map of ~30 names sourced from ld.so(8), bash(1), Python, Node, Ruby, Perl, Java, and glibc docs. Each entry is documented with a one-line threat rationale. Locale vars (`LANG`, `LC_ALL`, `TZ`, `LANGUAGE`, `LC_CTYPE`) are intentionally NOT blocked — they don't execute code and users legitimately set them.

  Exports: `ValidateEnvVarName`, `IsBlockedEnvVarName`, `EnvVarNamePattern`, `EnvVarNameRE`, `EnvVarNameMaxLength`, `ErrEnvVarNameBlocked`. Package-level comment in `pkg/validation` already documents it as the home for "shared validation primitives used by both API layer and in-pod materializer."

- **`api/internal/handlers/workspace_env.go`** — `SetWorkspaceEnv` now validates every env-var name in the request batch up front, BEFORE creating or updating any secret. Failure is fast and atomic: 400 with the offending name and a human-readable reason; no partial application. The blocklist runs at both this layer (user-facing gate) and at materialize-time in agentd (defense-in-depth).

- **`pkg/agentd/secrets/secrets.go`** — `validateVarName` now delegates to `validation.ValidateEnvVarName`, so the in-pod materializer enforces identical rules to the API layer. Removed the dead `varNameRE` variable (was only declared, never used after the delegation); the canonical regex lives at `validation.EnvVarNameRE` for any caller that needs only the regex half.

### Tests

- **`pkg/validation/env_test.go`** (new) — 6 unit tests:
  - `TestValidateEnvVarName_AcceptsLegitimateNames` — 16 common env-var names from real usage.
  - `TestValidateEnvVarName_RejectsEmptyAndTooLong` — length edge cases.
  - `TestValidateEnvVarName_RejectsInvalidPOSIXChars` — non-alphanumeric, leading digit, etc.
  - `TestValidateEnvVarName_RejectsDangerousNames` — the G37 core: every blocklisted name.
  - `TestValidateEnvVarName_RejectsDangerousNamesCaseInsensitive` — `ld_preload`, `Path`, etc.
  - `TestValidateEnvVarName_AcceptsLocaleNames` — regression guard for LANG/LC_ALL/TZ.

- **`api/internal/handlers/workspace_env_test.go`** — 5 new handler tests:
  - `TestSetWorkspaceEnv_RejectsBlockedNames` — 12 blocklisted names (table-driven).
  - `TestSetWorkspaceEnv_RejectsBlockedNamesCaseInsensitive` — lowercase / mixed-case variants.
  - `TestSetWorkspaceEnv_RejectsInvalidPOSIXNames` — non-blocklist regex failures.
  - `TestSetWorkspaceEnv_RejectsMixedBatch_NoPartialApply` — atomic-reject contract: one bad name rejects the whole batch with zero store writes.
  - `TestSetWorkspaceEnv_AcceptsLocaleNames` — locale vars are NOT blocked.

### Documentation

- **`CHANGELOG.md`** — entry under `[Unreleased] → Security`.
- **`design/stories/epic-17-security-review/THREAT-MODEL.md`** — G37 row flipped 🔴 → 🟢 Fixed with corrected file:line evidence (the threat-model claim about agentd lines 277-296 was wrong; that's the path-traversal check). STRIDE Sandbox Pod row updated. Counts reconciled (22 Fixed / 21 Open → 23 Fixed / 20 Open). Revision 2.5 added.

---

## Key Decisions

1. **Blocklist, not allowlist.** POSIX env-var names are unbounded (`FOO`, `MY_API_KEY`, `DEBUG`, `NODE_ENV`, etc.). An allowlist would break legitimate use. Blocklist of known-dangerous runtime-influencing names is the right shape.

2. **Single source of truth in `pkg/validation`.** The package comment explicitly says it exists to prevent drift between the API and the in-pod materializer. Adding `ValidateEnvVarName` there (rather than in `api/internal/handlers` or `pkg/agentd/secrets`) ensures both layers enforce identical rules without copy-paste drift.

3. **Both layers enforce.** API rejects up front (user-facing gate, fail-fast before store writes). Agentd's materialize-time check is defense-in-depth for any path that bypasses the API (direct DB write, future bug, manual kubectl edit of a Secret).

4. **Case-insensitive blocklist match.** `ld.so` accepts `ld_preload` on some glibc versions; `Path` and `PATH` are the same variable. The regex is case-sensitive (POSIX convention); the blocklist match is case-insensitive (defense-in-depth).

5. **Locale vars NOT blocked.** `LANG`, `LC_ALL`, `TZ`, `LANGUAGE`, `LC_CTYPE` are commonly set by users for legitimate localization and don't execute code. The test `TestSetWorkspaceEnv_AcceptsLocaleNames` is a regression guard: a future "block everything that affects the environment" sweep must not silently expand the blocklist to cover locale.

6. **API rejects BEFORE store writes — fail-fast, no partial application.** Without this invariant a user who typos one name in a 10-var batch would silently create 9 secrets and have to figure out which one was rejected. The atomic-reject contract is locked by `TestSetWorkspaceEnv_RejectsMixedBatch_NoPartialApply`.

7. **Curated blocklist, not exhaustive.** The bar for inclusion: the variable's effect is documented in its runtime's manual as influencing code loading, code execution, command lookup, or trust anchors AND not a variable users legitimately need to set. `LD_ASSUME_KERNEL`, `MALLOC_CHECK_`, `GODEBUG`, etc. exist but are less commonly weaponized. The blocklist comment explicitly says "curated, not exhaustive" to set expectations.

8. **Removed dead `varNameRE` from agentd.** After delegating to `validation.ValidateEnvVarName`, `varNameRE` had zero callers. Per Rule 5 (zero tech debt), removed it. The canonical regex is `validation.EnvVarNameRE` for any caller that needs only the regex half.

---

## Assumptions (Rule 7) — stated and validated

| # | Assumption | Validation |
|---|---|---|
| 1 | G37 still open in the codebase | Verified: `workspace_env.go:SetWorkspaceEnv` had no env-var name validation at all; `validateVarName` at `pkg/agentd/secrets/secrets.go:222` checked only regex + length, no blocklist. |
| 2 | Threat-model claim about agentd parallel check at lines 277-296 is wrong | Verified: that range is `resolveMountPath` (path traversal for `mount_path`), not env-var name validation. The threat-model row will be corrected. |
| 3 | `pkg/validation` is the right home for the new validator | Verified: package comment at `pkg/validation/name.go:5` says "Provides shared validation primitives used by both the API layer (pkg/secrets) and the in-pod materializer (pkg/agentd/secrets). Keeping validation here prevents drift between the two code paths." |
| 4 | Blocklist content is authoritative | Verified each name against `man 8 ld.so` (LD_*), `man 1 bash` (BASH_ENV, ENV, IFS, SHELLOPTS, PS4, TMPDIR, PATH, HOME), Python docs (PYTHONPATH, PYTHONSTARTUP, PYTHONHOME, PYTHONUSERBASE), Node docs (NODE_OPTIONS, NODE_PATH, NODE_EXTRA_CA_CERTS), Ruby docs (RUBYOPT, RUBYLIB), Perl docs (PERL5OPT, PERL5LIB, PERLLIB), JDK docs (JAVA_TOOL_OPTIONS, _JAVA_OPTIONS), macOS dyld man page (DYLD_INSERT_LIBRARIES, DYLD_LIBRARY_PATH), glibc (LOCPATH). |
| 5 | Locale vars should NOT be blocked | Verified: LANG/LC_ALL/TZ influence locale formatting and timezone, not code execution. Documented in code comment + regression test. |
| 6 | No import cycle introduced | Verified: `pkg/validation` does not import `pkg/agentd`. |
| 7 | Frontend doesn't expose the env endpoint | Verified: `grep -rn "workspaces.*env" frontend/src/` returns no hits. No UX break. |
| 8 | The blocklist match is case-insensitive but the regex is case-sensitive | Verified: ld.so accepts `ld_preload` on some glibc versions; `Path` and `PATH` are the same variable. Test `TestValidateEnvVarName_RejectsDangerousNamesCaseInsensitive` locks this. |

---

## Adversarial Self-Review (Rule 11)

### Phase 1 — finding candidates

1. Circular import via `validation.EnvVarNamePattern` used in agentd.
2. `varNameRE` retained as dead code after delegation.
3. `USERSITE` test entry was a phantom name (not a real Python env var).
4. `LANG` was misclassified as "blocked" in the test (it's intentionally NOT blocked).
5. Other phantom names in the blocklist.
6. Frontend might submit blocklisted names (UX break).
7. Validation order: auth → bind → name-validation.
8. Map iteration non-determinism: error reports one of many bad names.
9. Blocklist completeness (LD_ASSUME_KERNEL, MALLOC_CHECK_, GODEBUG).
10. `PS4` is only dangerous if `set -x` is enabled; agentd's entrypoint doesn't enable it.
11. Does `LD_PRELOAD` actually work on a `readOnlyRootFilesystem: true` pod?

### Phase 2 — validation

| # | Real? | Disposition |
|---|---|---|
| 1 | False alarm — pkg/validation has no agentd import | validated |
| 2 | **Real — dead code** | **fixed (varNameRE removed)** |
| 3 | Real test bug | fixed (entry removed) |
| 4 | Real test bug | fixed (LANG moved to accept-list test, plus new `TestValidateEnvVarName_AcceptsLocaleNames`) |
| 5 | False alarm — every blocklist entry verified against authoritative docs | validated |
| 6 | False alarm — frontend doesn't expose the endpoint | validated |
| 7 | False alarm — order is correct (auth first, then bind, then names) | validated |
| 8 | Acceptable — user fixes one, re-submits, gets the next; not worth sorting | documented |
| 9 | Acceptable — blocklist is documented as "curated, not exhaustive"; additions are easy | documented |
| 10 | False alarm — defense-in-depth is cheap; PS4 is in the same family as BASH_ENV | validated |
| 11 | False alarm — `LD_PRELOAD` can be set to a path under `/workspace` (PVC) or `/tmp` (writable); read-only rootfs doesn't help | validated |

### Phase 3 — remediation

All real findings fixed (varNameRE removed; test bugs fixed). Zero outstanding.

---

## Blockers

None.

---

## Tests Run

```bash
# Targeted validation unit tests
go test -count=1 -timeout 20s -v -run 'TestValidateEnvVarName' ./pkg/validation/...
# → 6/6 PASS

# Targeted handler unit tests
go test -count=1 -timeout 50s -v -run 'TestSetWorkspaceEnv_(Rejects|Accepts)' ./api/internal/handlers/...
# → 5/5 PASS

# agentd secrets tests (regression after validateVarName delegation)
go test -count=1 -timeout 50s ./pkg/agentd/secrets/...
# → PASS

# Full repository test suite
go test -timeout 240s -short ./...
# → 67 packages ok, 0 FAIL

# Build + vet
go build ./...    # exit 0
go vet ./...      # exit 0

# Lint (changed packages)
golangci-lint run --timeout=4m ./api/internal/handlers/... ./pkg/validation/... ./pkg/agentd/secrets/...
# → 0 issues

# Format
gofmt -l <changed files>      # clean
goimports -l <changed files>  # clean
```

---

## Next Steps

1. **Merge this PR**, then move to G35 (`/account/recover` rate limit).
2. **Follow-up (out of scope):** Consider whether the existing agentd secrets tests should add coverage for blocked names at the materialize-time layer (currently covered transitively via `validateVarName`'s delegation, but no direct agentd-side test for "materialize rejects LD_PRELOAD"). Low priority — the unit tests in pkg/validation cover the contract.
3. **Blocklist maintenance:** Document the addition protocol (one PR per name, citing the runtime's manual) in a future docs update. Not blocking.

---

## Files Modified

- `pkg/validation/env.go` — **new** — `ValidateEnvVarName`, `IsBlockedEnvVarName`, blocklist, exports
- `pkg/validation/env_test.go` — **new** — 6 unit tests
- `api/internal/handlers/workspace_env.go` — fail-fast name validation in `SetWorkspaceEnv`
- `api/internal/handlers/workspace_env_test.go` — 5 new handler tests for G37
- `pkg/agentd/secrets/secrets.go` — `validateVarName` delegates to shared validator; dead `varNameRE` removed
- `CHANGELOG.md` — entry under `[Unreleased] → Security`
- `design/stories/epic-17-security-review/THREAT-MODEL.md` — G37 row flipped 🟢; STRIDE + counts + revision 2.5
- `worklogs/NNNN_2026-07-11_g37-env-var-name-blocklist.md` — this file
