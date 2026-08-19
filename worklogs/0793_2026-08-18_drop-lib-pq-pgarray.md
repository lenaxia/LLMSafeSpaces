# Worklog: Drop lib/pq (GO-2026-6166..6173) — pgarray migration

**Date:** 2026-08-18
**Session:** Remove the lib/pq dependency flagged by govulncheck CI (7 advisories, no fixed release exists; v1.12.3 is latest). Discovered while landing PR #938, whose security-scan went red on advisories published hours earlier — go.mod unchanged by that PR, so main's next scheduled scan fails identically. This is the standalone fix, branched off main.
**Status:** Complete

---

## Objective

Make `govulncheck ./...` exit-clean again without changing any runtime behavior.

---

## Work Completed

### Diagnosis

- CI trace: `testharness.New calls sql.Open → pq.Driver.Open` — "affected by 7 vulnerabilities, exit 3".
- Reality check: production opens every connection with `sql.Open("pgx", ...)` (pgx/v5 stdlib has always been the driver); lib/pq was linked ONLY for helpers — `pq.Array` (text[] bind/scan), one `pq.StringArray` var, one `*pq.Error` code assertion. The `sql.Open→pq.Driver.Open` trace was static over-approximation of the driver registry (pq's init registers it; the tool cannot know the name dispatch never selects it).
- All 7 advisories: `Fixed in: N/A`. Nothing to upgrade to; the only remediation is removing the dependency.

### Fix — `api/internal/services/database/pgarray` (new, ~150 lines + tests)

- `pgarray.New(v)` wraps any string-kind slice (plain or named, e.g. `type Selection []string`) as `driver.Valuer` + `sql.Scanner`.
- **Bind format byte-identical to pq's** (`{"a","b"}` — every element quoted): sqlmock arg expectations and any DB-side comparisons see no change.
- **Parser accepts both literal forms** — pq's all-quoted bind form AND PostgreSQL's own output form (`{a,b}`, quotes only when needed) — because row fixtures carry the latter.
- NULL semantics preserved: nil slice ↔ SQL NULL both directions (an empty `{}` stays `{}`).
- Strings-only by design (every array column in the schema is text[]); non-string slices fail loudly.

### Migration (mechanical, 3 prod files + 2 test files)

- `pq.Array(` → `pgarray.New(` everywhere (database.go AllowedCIDRs, pg_org_store.go SSO domains, imagefactory.go selection/architectures/bases).
- `var supportedBases pq.StringArray` → `[]string` + wrapped the bare scan in `scanExtension` (StringArray's built-in Scanner was load-bearing — found via test failure, not by eye).
- `*pq.Error` → `*pgconn.PgError` (pgconn ships with pgx/v5, already a direct dep) for the 23505 conflict checks.
- `go mod tidy`: lib/pq demoted to `// indirect` (golang-migrate's database/postgres package imports it; testharness-only, and `WithInstance` wraps the pgx connection so pq's driver never executes). Module-level-only per govulncheck — the CI gate fails on CALLED vulns only, and nothing calls it.

### Hermeticity fix carried onto this branch

`pod_bootstrap_e2e_test.go` env-scrub (identical to the fix on feat/epic65-wire-context-usage): the two bootstrap e2e tests fail inside real workspace pods because live `INFERENCE_RELAY_*` env leaks into the agentd subprocess. Both branches need it; whichever merges first, the other's copy drops out.

---

## Key Decisions

| Decision | Rationale |
|---|---|
| Internal helper vs "pass raw slices, let pgx codecs handle" | The stores run under sqlmock in unit tests — raw slices never reach pgx there (database/sql's default converter rejects them). Valuer/Scanner is driver-agnostic: identical behavior under sqlmock and pgx. |
| Byte-identical bind format (always-quote) | Zero migration delta for sqlmock arg expectations; both forms valid PG literals. Discovered via test failure after initially implementing minimal-quoting. |
| Keep pq as indirect dep | It rides in via golang-migrate (test-only usage path); purging it entirely means replacing golang-migrate's postgres driver — out of scope, and unnecessary (gate + runtime both clean). |
| TDD for pgarray | Tests-first caught two real bugs before commit: missing string-kind elem check (silent []int garbage) and empty-vs-nil slice NULL round-trip. |

---

## Blockers

None. PR stacks independently of #938 (branched off main); merge order is free.

---

## Tests Run

- `go test ./api/internal/services/database/...` — ok (pgarray + full database suite, incl. the previously-red TestUpsertSSOConfig/TestGetExtension after the StringArray fix)
- `go test ./api/...` — all 35 packages ok (incl. the two bootstrap e2e hermeticity tests)
- `go build ./...` — clean; `govulncheck ./api/... ./pkg/... ./cmd/...` — **"Your code is affected by 0 vulnerabilities"**, exit-clean

---

## Next Steps

- Land this PR → re-run security-scan on #938 → merge.
- Optional follow-up (not urgent): track upstream golang-migrate pgx-driver migration to purge the indirect pq requirement entirely.

---

## Files Modified

- `api/internal/services/database/pgarray/pgarray.go`, `pgarray_test.go` — NEW
- `api/internal/services/database/database.go`, `pg_org_store.go`, `imagefactory.go` — pq → pgarray/pgconn
- `api/internal/services/database/imagefactory_test.go`, `imagefactory_integration_test.go` — error type + comments
- `api/internal/handlers/pod_bootstrap_e2e_test.go` — env-scrub hermeticity fix
- `go.mod` — pq → indirect (go.sum untouched)
