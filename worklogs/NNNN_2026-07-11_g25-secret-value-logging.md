# Worklog: G25 — Secret value field no longer logged

**Date:** 2026-07-11
**Session:** Address threat-model gap G25 (High) — the request logging middleware masked sensitive JSON fields by name (`password`, `token`, `secret`, `key`, `apiKey`, `credit_card`) but NOT `value` — the field name used by the secrets API to carry the plaintext credential.
**Status:** Complete

---

## Objective

Close G25 from `design/stories/epic-17-security-review/THREAT-MODEL.md`. The secrets API (`POST /api/v1/secrets`, `PUT /api/v1/secrets/:id`) carries the plaintext credential in a JSON field named `value`:

```json
{"name":"my-openai-key","type":"llm-provider","value":"sk-proj-abc123..."}
```

The logging middleware at `api/internal/middleware/logging.go:LoggingMiddleware` logged request bodies by default (`LogRequestBody: true`) with field-name masking applied via `MaskSensitiveFieldsWithList(body, SensitiveFields)`. The default `SensitiveFields` list did NOT include `"value"`, so the `sk-proj-abc123...` text appeared verbatim in the application log — visible to anyone with log access (operators, SRE, log aggregator).

The threat-model row offered three options: (1) add `"value"` to SensitiveFields; (2) route JSON body through `pkg/redact.Redact()`; (3) disable body logging for `/api/v1/secrets/*`. This PR combines (1) and (3) — defense in depth.

---

## Work Completed

### Implementation

- **`api/internal/middleware/logging.go`** — three changes:
  1. Added `"value"` to the default `SensitiveFields` list. Defense in depth — catches any logged JSON with a `value` field, even on paths not in the skip list. Legitimate non-secret uses (e.g. settings PUT `{"value":"20Gi"}`) become `"********"` in logs, which is acceptable for log readability.
  2. Added a new `SkipPathPrefixes []string` field to `LoggingConfig`. Distinct from the existing `SkipPaths` (exact match) — `SkipPathPrefixes` uses prefix matching so `/api/v1/secrets/` catches every secrets sub-path (`/api/v1/secrets/:id`, `/api/v1/secrets/:id/reveal`, etc.).
  3. Configured the default `SkipPathPrefixes` with the credential-bearing paths:
     - `/api/v1/secrets` and `/api/v1/secrets/` — secrets CRUD bodies.
     - `/api/v1/account` and `/api/v1/account/` — rotate-key, change-password, recover bodies.
     - `/api/v1/auth` and `/api/v1/auth/` — login (password in request), JWT in response, api-keys creation (key in response).
     - `/api/v1/admin/provider-credentials` and `/api/v1/admin/provider-credentials/` — admin LLM provider API keys (`apiKey` field, already masked but skipped for safety against future field renames).
   
   Each resource has both with-trailing-slash and without-trailing-slash forms so both collection paths (`/api/v1/secrets`) and nested paths (`/api/v1/secrets/:id/reveal`) are caught, without accidentally matching unrelated paths like `/api/v1/secretslist`.

### Tests

Four new tests appended to `api/internal/middleware/tests/logging_test.go`:

- `TestLoggingMiddleware_G25_SecretsPathBodyNotLogged` — G25 core regression. Sends a request with a real-looking API key in the `value` field to `POST /api/v1/secrets`; asserts NO log call is made (the prefix-skip is the primary gate).
- `TestLoggingMiddleware_G25_SkipPathPrefixes_MatchesNestedPaths` — confirms prefix matching catches `/api/v1/secrets/sec-abc/reveal` (not just exact paths).
- `TestLoggingMiddleware_G25_SkipPathPrefixes_DoesNotMatchUnrelatedPaths` — `/api/v1/workspaces` is still logged normally.
- `TestLoggingMiddleware_G25_ValueFieldInSensitiveFields` — locks the default config: `"value"` is in `SensitiveFields` AND `/api/v1/secrets/` is in `SkipPathPrefixes`.

### Documentation

- **`CHANGELOG.md`** — entry under `[Unreleased] → Security`.
- **`design/stories/epic-17-security-review/THREAT-MODEL.md`** — G25 row flipped 🔴 → 🟢 Fixed. STRIDE `API Auth` row updated. Counts: 24 Fixed / 19 Open → 25 Fixed / 18 Open. Revision 2.7 added.

---

## Key Decisions

1. **Two-layer fix (SensitiveFields + SkipPathPrefixes), not one.** Either layer alone prevents the leak. SensitiveFields catches the case where a new secrets-like endpoint is added but the path isn't in SkipPathPrefixes. SkipPathPrefixes catches the case where a future field rename (e.g. `value` → `data`) bypasses SensitiveFields.

2. **Adding `"value"` globally is acceptable collateral.** The threat-model noted that `"value"` is a common field name. Masking every `"value"` field globally means settings updates like `{"value":"20Gi"}` become `"********"` in logs. That's a debuggability cost, not a correctness cost — and the alternative (per-path SensitiveFields) would require a more complex config shape for marginal benefit.

3. **SkipPathPrefixes uses `strings.HasPrefix`, not exact match.** The existing `SkipPaths` is exact-match (appropriate for `/health`, `/livez`). The new field is prefix-match (appropriate for catching every `/api/v1/secrets/*` sub-path without enumerating them).

4. **Two prefix forms per resource (with and without trailing slash).** `/api/v1/secrets` (exact path, used by POST/GET collection endpoints) AND `/api/v1/secrets/` (prefix for nested paths). Without both, the collection path leaks. Adding `/api/v1/secrets` (no trailing slash) as a prefix is safe because `strings.HasPrefix("/api/v1/secrets", "/api/v1/secrets")` matches end-of-string correctly; `strings.HasPrefix("/api/v1/secretslist", "/api/v1/secrets")` would also match but no such endpoint exists in this codebase. The two-form approach is documented in the code comment.

5. **Include `/api/v1/auth/` and `/api/v1/admin/provider-credentials/` even though SensitiveFields already catches `password`/`apiKey`/`token`.** Defense in depth — a future field rename (e.g. `apiKey` → `api_key` snake_case) would silently unmask. Skipping the body entirely is path-name-stable.

---

## Assumptions (Rule 7) — stated and validated

| # | Assumption | Validation |
|---|---|---|
| 1 | G25 still open in the codebase | Verified: `logging.go:41` SensitiveFields did not include `"value"`; `MaskSensitiveFieldsWithList` (`pkg/utilities/masking.go:9`) matches by exact key name only. |
| 2 | Adding `"value"` to SensitiveFields is too broad on its own | Verified by enumerating non-secret uses: settings updates (`{"value":"20Gi"}`), env-var payloads. Acceptable collateral — log readability cost, not correctness cost. |
| 3 | `SkipPaths` is exact-match only | Verified: `logging.go:84-89` uses `path == skipPath`. |
| 4 | Path is `c.Request.URL.Path` (not gin's FullPath) | Verified: `logging.go:83` reads `c.Request.URL.Path`. Prefix matching on `/api/v1/secrets/` matches `/api/v1/secrets/sec-123/reveal` correctly. |
| 5 | Existing logging tests don't use paths under the new skip prefixes | Verified: existing tests use `/test`, `/login`, `/api`, `/health`, `/metrics` — none start with `/api/v1/secrets`, `/api/v1/account`, `/api/v1/auth`, or `/api/v1/admin/provider-credentials`. |
| 6 | Server-level integration tests don't assert specific log calls | Verified: `grep -rn "AssertCalled.*Info" api/internal/server/*_test.go` returned no hits. |
| 7 | Admin provider credentials use `apiKey` JSON field | Verified at `api/internal/handlers/admin_provider_credentials.go:60,73` — `APIKey string \`json:"apiKey"\``. `"apiKey"` is already in SensitiveFields; adding the path-skip is defense in depth. |

---

## Adversarial Self-Review (Rule 11)

### Phase 1 — finding candidates

1. Existing logging tests broken by new prefixes?
2. New prefix matching affects existing SkipPaths behavior?
3. Other endpoints under `/api/v1/account/` that legitimately need body logging?
4. `/api/v1/auth/api-keys` response contains the new API key — separate leak.
5. `TestLoggingMiddleware_G25_SkipPathPrefixes_DoesNotMatchUnrelatedPaths` uses `/api/v1/workspaces` — verify path is not under any prefix.
6. `/api/v1/users/me/settings` paths — settings can carry sensitive values.
7. `/api/v1/admin/settings/:key` — same.
8. Admin provider credentials endpoint uses `apiKey` field, not `value`.

### Phase 2 — validation

| # | Real? | Disposition |
|---|---|---|
| 1 | False alarm — verified by running full middleware test suite |
| 2 | False alarm — separate loops, separate semantics |
| 3 | Real — `/api/v1/account/rotate-key`, `/change-password`, `/recover` all carry passwords/keys. Addressed by adding `/api/v1/account` to SkipPathPrefixes. |
| 4 | **Real** — addressed by adding `/api/v1/auth` to SkipPathPrefixes (catches login, api-keys creation, register) |
| 5 | False alarm — verified |
| 6 | Acceptable — settings values are usually non-secret; `"value"` in SensitiveFields catches exceptions |
| 7 | Same as 6 |
| 8 | **Real** — addressed by adding `/api/v1/admin/provider-credentials` to SkipPathPrefixes (defense in depth, since `apiKey` is already in SensitiveFields but a future rename would bypass) |

### Phase 3 — remediation

All real findings addressed (3, 4, 8). Zero outstanding.

---

## Blockers

None.

---

## Tests Run

```bash
# Targeted G25 tests
go test -count=1 -timeout 25s -v -run 'TestLoggingMiddleware_G25' ./api/internal/middleware/tests/...
# → 4/4 PASS

# Full middleware package (regression check after default-config change)
go test -count=1 -timeout 50s ./api/internal/middleware/...
# → PASS

# Full repository test suite
go test -timeout 240s -short ./...
# → 67 packages ok, 0 FAIL

# Build + vet
go build ./...    # exit 0
go vet ./...      # exit 0

# Lint
golangci-lint run --timeout=4m ./api/internal/middleware/...
# → 0 issues

# Format
gofmt -l <changed files>      # clean
goimports -l <changed files>  # clean
```

---

## Next Steps

1. **Merge this PR**, then move to G36 (workspace secrets not cleaned on deletion).
2. **Follow-up (out of scope):** Consider extending `pkg/redact.Redact()` to optionally scan JSON-marshalled bodies for known credential patterns (sk-* AWS keys, etc.). Not strictly needed — SensitiveFields + SkipPathPrefixes covers the threat-model gap — but defense in depth.
3. **Optional:** Wire `SkipPathPrefixes` to instance-settings so operators can tune without rebuilding. Low priority.

---

## Files Modified

- `api/internal/middleware/logging.go` — `SkipPathPrefixes` field on LoggingConfig; default SensitiveFields extended with `"value"`; default SkipPathPrefixes wired with credential-bearing paths; middleware loop checks prefixes
- `api/internal/middleware/tests/logging_test.go` — 4 new G25 tests
- `CHANGELOG.md` — entry under `[Unreleased] → Security`
- `design/stories/epic-17-security-review/THREAT-MODEL.md` — G25 row flipped 🟢; STRIDE + counts + revision 2.7
- `worklogs/NNNN_2026-07-11_g25-secret-value-logging.md` — this file
