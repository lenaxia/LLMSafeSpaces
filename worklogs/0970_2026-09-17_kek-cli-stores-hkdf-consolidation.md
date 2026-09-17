# Worklog: Wire KEK CLI stores + consolidate HKDF deriveKey (#830/#832)

**Date:** 2026-09-17
**Session:** Implement issues #830 (wire rotate-kek/migrate-kek store layers) and #832 (consolidate triplicated HKDF deriveKey with byte-compat proof) in one PR.
**Status:** Complete

---

## Objective

The rotate-kek / migrate-kek CLIs documented in four operator runbooks had stub store constructors ("postgres connection not yet wired") — an incident-time trap. Wire real pgx + go-redis connections into the existing store interfaces, verify every documented flag/promise against the runbooks (implement or de-document), and consolidate the three independent HKDF deriveKey implementations into one `pkg/secrets` export guarded by byte-compat golden fixtures captured BEFORE consolidation.

---

## Work Completed

### #832 — deriveKey consolidation (done first; #830 depends on it)

- **Byte-compat capture BEFORE any change**: throwaway generator tests in each of the three packages (cmd/rotate-kek, cmd/migrate-kek, api/internal/app) emitted a fixture matrix — 4 masters (32B/48B/31B/all-zero) × 7 purposes (4 production purposes + oidc-state-cookie + empty + a control) = 28 outputs. **The three implementations agreed byte-for-byte on all 28 records**, including the 31-byte-master → nil edge. No latent key-mismatch bug; the "mirrors" comments were truthful.
- Golden matrix frozen at `pkg/secrets/testdata/derive_server_key_golden.txt`; permanent gate `TestDeriveServerKey_GoldenByteCompat` (pkg/secrets/derive_server_key_test.go) asserts the consolidated function reproduces those bytes. `TestDeriveServerKey_MatchesDeriveKEKFromKeyServerPath` additionally pins the wrapper to the primitive the server used pre-consolidation (`DeriveKEKFromKey` + `llmsafespaces-server` salt).
- New export `pkg/secrets/derive_server_key.go`: `DeriveServerKey(master, purpose)` + `ServerKeyHKDFSalt` (separate file from the parallel agent's staging_provider.go).
- All three sites switched; both cmd copies deleted:
  - `api/internal/app/secrets_adapters.go` `deriveServerKey` now delegates (env/file loading stays local).
  - `cmd/rotate-kek/main.go`, `cmd/migrate-kek/main.go` call `secrets.DeriveServerKey`; short-master now fails fast with an explicit error (rotate-kek run() gained the check migrate-kek already had).
  - Consumer-side proofs: `api/internal/app/derive_server_key_golden_test.go` (env → deriveServerKey == golden), `TestCLIProviderConstruction_GoldenDerivedKey` in both cmd packages (CLI path cross-decrypts fixtures encrypted with the golden key bytes).

### #830 — store wiring

- `cmd/rotate-kek/store.go`: `newPgRotationStore` opens a real pgxpool (ping, 10s timeout); `ListRotationRows`/`UpdateRotationRow` implemented against the existing `pgConn`/`pgRow`/`pgRows` seam with a per-table column-map allowlist (`provider_credentials` id/ciphertext/owner_type, `api_keys` id/key_ciphertext, `org_sso_configs` org_id/oidc_client_secret, `user_keys` user_id/wrapped_dek; ::uuid casts for uuid-keyed tables; resume cursor drops the predicate for empty cursors instead of casting ''). Unknown table names are rejected (no interpolation of untrusted identifiers).
- `cmd/migrate-kek/store.go`: same wiring; **fixed the pre-existing wrong column mapping** — the never-run query selected nonexistent `ciphertext` columns on api_keys/org_sso_configs (org_sso_configs has no `id` column at all; its PK is org_id). `newRedisCacheFlusherImpl` wired to a real go-redis client.
- Redis flushers (both CLIs): `redis.ParseURL` + ping at construction (fail fast at startup, not post-rotation); `FlushDEKCache` SCANs `dek:*` (collect-then-delete, 500-key batches) — never FLUSHDB, which would nuke rate limits/sessions/revocations sharing the instance.
- `pkg/secrets`: exported `DEKCachePrefix` (redis_cache.go) so the CLIs flush exactly the right keys; added `LastRowID` to `KEKRotationResult`/`KEKMigrationResult`.
- **Resume cursor semantics** (found while writing the resume-from test): the cursor must freeze at the last success BEFORE the first failure — a later success must not push it past a failed row or `--resume-from` strands it. Implemented via `cursorFrozen` in both coordinators; regression tests `Test*Coordinator_LastRowIDStopsAtFailedRow` plus `Test*Coordinator_ReportsLastRowID` / `LastRowIDEmptyWhenNoRows`.
- Both CLIs print `last-row-id=<id>` per table plus the exact resume command (the runbook promise "the CLI prints the last processed row ID per table on exit" was documented-but-unimplemented; now implemented, in dry-run too).
- Deleted the SA4023 nolint stub anchors from both mains.

### Docs verified against binaries (implemented vs de-documented)

| Doc | Drift found | Resolution |
|---|---|---|
| helm/KEK-ROTATION.md | dry-run report listed 3 tables (code rotates 4); purpose table missing user_keys | doc fixed (4 tables; user_keys → master-kek row added) |
| helm/KEK-MIGRATION.md | "prints the last processed row ID" unimplemented | implemented (last-row-id + resume hint) |
| docs/operator/runbook.md | wrong flag names (`--old-master-key-file`), missing required `--database-url`/`--redis-url`, `--resume-from <table-name>` semantics that don't exist | doc fixed (real flags, row-id resume semantics, pointer to helm/KEK-ROTATION.md) |
| docs/operator/upgrading.md | same wrong flag names | doc fixed |
| migrate-kek flags | `--db-url/--kms/--aws-*/--gcp-*/--table/--resume-from/--redis-url/--dry-run/--audit` all real | no change needed |

### CI

- `.github/workflows/secrets-integration.yml`: path gate now includes `^cmd/rotate-kek/|^cmd/migrate-kek/`; new step runs both `-tags=integration` suites with `-race` against the existing Postgres+Redis services (this is the CI-able smoke form of the runbook promises the issue asked for).

### Testharness adoption (#845) notes

The CLI suites follow the testharness conventions exactly — same `TEST_DATABASE_URL` env contract + default DSN, same skip-if-unreachable gate, same embedded `api/migrations` set applied idempotently via golang-migrate, same unique-marker parallel isolation — but cannot `import api/internal/testharness` itself: Go's internal-package visibility forbids cmd/ (and pkg/) from importing api/internal/, the same constraint the harness README documents for pkg/secrets' getTestPool. The ~40-line convention replica per CLI test file is the documented, sanctioned pattern for out-of-api adopters.

---

## Key Decisions

1. **Store implementations stay in the CLIs** (per the issue's letter) rather than a shared pkg package — a shared pkg/secrets store would have exceeded the agreed pkg/secrets footprint for this PR (parallel agent owns pkg/secrets additions; my footprint is derive_server_key.go + the DEKCachePrefix export).
2. **`DeriveServerKey` returns nil on short masters** (not an error) — preserves the fail-closed semantics all three original implementations shared; callers already treat nil as configuration failure. Byte-compat with the originals is exact.
3. **DEKCachePrefix exported** from redis_cache.go (3 internal uses renamed) rather than hardcoding "dek:" in the CLIs — single source of truth for a shared key contract; zero conflict surface with the parallel agent's new file.
4. **Cursor-freeze-on-failure** for LastRowID (see above) — correctness requirement discovered via TDD, not in the original plan.
5. **collect-then-delete SCAN** for the flusher: interleaving SCAN+DEL is sensitive to server-side rehashing (miniredis surfaced it immediately with exactly one batch surviving).

## Assumptions stated and validated (Rule 7)

| Assumption | Validation |
|---|---|
| The three deriveKey implementations are byte-identical | Proven: 28/28 golden outputs identical (capture BEFORE consolidation) |
| pgxpool/pgx Row/Rows satisfy the pgConn seam | Proven by compile (`var _ pgRow = pgx.Row(nil)` etc.) + integration tests |
| org_sso_configs PK is org_id (no id column); api_keys ciphertext col is key_ciphertext; user_keys keyed by user_id | Verified in api/migrations/000001_initial_schema.up.sql:27,257,311,526 |
| The migrate-kek list query's (id, ciphertext) columns were wrong for 2 of 3 tables | Proven by the old query failing against the real schema in the red integration tests |
| Redis DEK cache keys are `dek:<session>` | pkg/secrets/redis_cache.go DEKCachePrefix |
| miniredis supports SCAN+DEL | redis_cache_test.go precedent + my flusher tests (caught the interleaving issue) |
| api/migrations is importable from cmd/ (not internal) | it is a plain package; used by the integration helpers |

## Blockers

None.

## Tests Run

- `go test ./cmd/rotate-kek/ ./cmd/migrate-kek/ -tags=integration -race -count=1` — 11 tests PASS against local Postgres 15 (externally provisioned per the harness contract).
- `go test ./pkg/secrets/ ./api/internal/app/ ./cmd/... -count=1` — PASS.
- `go test ./... -count=1` — PASS except `pkg/repolint TestLive_Worklogs_NoDuplicates`, a pre-existing remote-state failure: origin/main (27e015cf, PR #1405) carries an unrenamed `NNNNN_` sentinel worklog awaiting the post-merge numbering bot. The test reads only origin/main via `git ls-tree`; unaffected by this branch (verified on a clean origin/main checkout).
- `golangci-lint run` (v2.13.1, repo config) — 0 issues.
- `gofmt -l` / plain `goimports -l` — clean.

## Next Steps

- Watch the PR review loop; fix findings with regression tests.
- If the numbering bot still hasn't numbered the sentinel on main, the next repolint bot commit will; nothing to do from this branch.

## Files Modified

- pkg/secrets/derive_server_key.go (new), derive_server_key_test.go (new), testdata/derive_server_key_golden.txt (new)
- pkg/secrets/redis_cache.go (export DEKCachePrefix)
- pkg/secrets/rotation.go, migration.go (LastRowID + cursor freeze), rotation_test.go, migration_test.go (tests)
- cmd/rotate-kek/store.go (wired), store_test.go (new), store_integration_test.go (new), main.go (DeriveServerKey + last-row-id report)
- cmd/migrate-kek/store.go (wired + column-map fix), store_test.go (new), store_integration_test.go (new), main.go (DeriveServerKey + last-row-id report)
- api/internal/app/secrets_adapters.go (delegation), derive_server_key_golden_test.go (new)
- helm/KEK-ROTATION.md, helm/KEK-MIGRATION.md, docs/operator/runbook.md, docs/operator/upgrading.md (doc fixes)
- .github/workflows/secrets-integration.yml (CI adoption)

---

## Review iteration 1 (PR #1409, AI reviewer: REQUEST CHANGES)

All findings validated as REAL (Rule 11 Phase 2) and fixed with regression tests:

1. **Single-table runs never flushed the DEK cache** (docs promise it unconditionally; the runbook's own interrupted-run recovery is per-table). Fixed: rotate-kek/migrate-kek `run()` flushes `redisCacheStore`/`store` after a successful single-table apply (non-dry-run). Regression: `TestIntegration_SingleTableRunFlushesDEKCache` drives the real `run()` with `--table api_keys --redis-url ...` and asserts `dek:*` gone + non-dek keys survive + a dry-run follow-up does not flush.
2. **Dry-run resume hint could strand rows** (following "--resume-from <dry-run-cursor>" on the apply run skips everything before it). Fixed: dry-run hint now says to re-run plainly; `--resume-from` framed as interrupted-apply-only; KEK-ROTATION.md dry-run sentence corrected. Regression: `TestPrintResumeHint_DryRunNeverSuggestsResumeFrom`.
3. **KEK-ROTATION.md verify step still said "three tables"** — fixed to four + flush note now scoped to all invocation shapes.
4. Minor: `--table all` + `--resume-from` now a hard error at run() entry (was silently ignored) — `TestRun_TableAllWithResumeFromRejected` (+ migrate-kek symmetric guard). rotate-kek `run()` short-master fail-closed test added (`TestRun_MasterKeyFileTooShortFailsClosed`). Stale stub-era comment block removed from migrate-kek/main_test.go. Hand-rolled `itoa` → `strconv.Itoa`. `pgConnectTimeout` → `dialPingTimeout` (it bounds both pg and redis pings).

Reviewer's out-of-scope note (pre-existing, loud-failing by design): `purposeForTable` routes ALL api_keys rows to `master-kek`, so legacy v1 `dek-cache`-wrapped rows fail rotation loudly — safe failure mode, coordinator logic explicitly out of scope per #830.

## Tests Run (iteration 1)

- `go test ./cmd/rotate-kek/ ./cmd/migrate-kek/ -tags=integration -race` — PASS (incl. new flush e2e, 20 rotate-kek tests).
- Unit suites for cmd/, pkg/secrets, api/internal/app — PASS.
- `golangci-lint run` — 0 issues.

---

## Review iteration 2 (PR #1409, AI reviewer: REQUEST CHANGES — all iteration-1 fixes verified)

Both issues now rated FULLY ADDRESSED by the reviewer; remaining findings fixed:

1. **migrate-kek guard unit test** (twin of rotate-kek's): added `TestRun_TableAllWithResumeFromRejected` — the guard fires before any key-file read, so no fixtures needed.
2. **migrate-kek dry-run hint unit tests**: added `TestPrintResumeHint_DryRunNeverSuggestsResumeFrom` + `TestPrintResumeHint_ApplyRunSuggestsResumeFrom`.
3. **Flush asymmetry (advisory) aligned**: the single-table path now flushes after the walk even with per-row failures, matching RotateAll/MigrateAll (flushing is always safe — evicted DEKs re-derive on demand and still decrypt via the still-mounted old key during the window). Flush-before-failed-check in both CLIs.
4. **Stale worklog line corrected** (worklogs are append-only; correction recorded here): the iteration-1 entry said the flush e2e runs `--table api_keys`; the committed test deliberately uses `--table user_keys` for shared-CI-DB isolation (api_keys is seeded concurrently by the migrate-kek suite binary). The test's own comment is authoritative.

**Acknowledged gap (reviewer's item 3, accepted as follow-up):** migrate-kek's single-table flush is exercised at store level (`TestCompositeMigrationStore_FlushDelegatesToRedis`) but not via `run()` — driving it requires a live KMS (every row fails at Encrypt with unreachable KMS, and the flush-on-success path needs a successful walk). Closing it properly needs an injectable provider seam in `run()`, deliberately out of scope for this PR's footprint. Rotate-kek's symmetric path IS covered end-to-end (`TestIntegration_SingleTableRunFlushesDEKCache`).
