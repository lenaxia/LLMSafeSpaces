# Worklog: US-70.4 — login-independent re-wrap reconciler (#1208)

**Date:** 2026-09-01
**Session:** Epic 70 US-70.4 (design 0052 §4.5 / Phase-1 remainder; blast-radius controls W9–W11). Implemented in one delegation with full unit + PG-integration coverage; rebased over the merged US-70.3.
**Status:** Complete.

## Work Completed

- **Migration 000030** (+ helm mirror, idempotent): `wrapped_dek_previous` (retained wrap as **ciphertext re-wrapped under the CURRENT KEK** — W10: reversible with current keys, zero plaintext at rest), `wrapped_dek_previous_kek_version`, `wrapped_dek_retained_until` (30d), and `user_keys.updated_at` (backfilled; the CAS/listing need it — no such column existed).
- **Store**: `ListUserKeysForReconcile` (oldest-first batching — highest-risk rows first), `CompareAndSwapWrappedDEK` (bytea-equality CAS; 0 rows = a concurrent legitimate rotation won → back off), `DeleteExpiredRetainedWraps`.
- **`api/internal/services/keyrewrap`**: periodic walk (10m default, env-overridable; immediate startup pass); per row: unwrap with the current master provider → healthy skip; on failure, recovery from the session-cache DEK (`GetDEKForUser` walk — K1/K2 while they exist) with **source agreement** (the recovered DEK must decrypt an existing user secret — W9 poison guard); **verify-after-write** (new wrap round-trips before any commit); CAS write carrying the retained wrap; **halt on ≥3 verify failures per pass + `LLMSAFESPACES_KEY_REWRAP_DISABLED` kill switch**; retention cleanup; audit rows; metrics (`key_rewrap_rows_total{outcome}`, halted gauge) + promtool-validated alerts. **W11**: zero-secret users surface `unwrappable:no_secret_to_verify` and are never healed unverified.
- Degraded workspaces converge after a heal via US-70.3's reconcile loop (no delivery coupling — by design).
- Proven pre-existing fix: testharness `Reset()` truncated the migration-seeded `image_factory_platform_config` singleton, poisoning an unrelated integration test (demonstrated failing on stashed HEAD).

## Assumptions (Rule 7 — validated)

| # | Assumption | Validation |
|---|---|---|
| A-1 | `user_keys` had no `updated_at` | grep of all migrations |
| A-2 | Master provider reachable via `SetAPIKeyStore` wiring | app.go:953 (`apiKeyProv`) |
| A-3 | `ListSecrets` is the cheapest per-user secret listing for source agreement | interface survey |
| A-4 | Audit action column width fits all new actions | varchar(50) vs action strings |

## Key Decisions

1. **CAS on the raw wrap bytes** — the simplest collision domain that exactly distinguishes "row changed since I read it".
2. **Retained wrap re-encrypted under the current KEK** — retention adds no at-rest exposure and stays reversible with current keys.
3. `healLegacyDEK` (request-path opportunistic heal) left un-CAS'd — benign same-DEK race with the reconciler; US-70.5 demolishes it.
4. Outcome vocabulary extended (`unwrappable_no_source`, `source_disagreement`, `error`) — the issue's "surface everything" reading.

## Tests Run

`go build ./...`; `go test -race ./api/internal/services/keyrewrap/ ./pkg/secrets/` (16 unit: healthy-skip, heal, W11, W9 poison, verify-halt+reset, CAS-loss, kill switch, retention, ordering, env); `-tags integration` against a real PG (CAS race exactly-one-wins; retained-wrap round-trip; migration idempotency; oldest-first); `go test ./helm/...` (promtool); full `./api/... ./pkg/...` sweep; repolint.

## Next Steps

1. PR → review → merge.
2. US-70.5 (#1209): demolition + fleet-version gauge (its gate: 70.3 ✓ + 70.4 ✓ once merged).

## Files Modified

- `api/migrations/000030_key_rewrap_retention.{up,down}.sql` + `helm/migrations/` mirrors
- `pkg/secrets/`: reconcile_store.go (new), pg_key_store.go
- `api/internal/services/keyrewrap/` (new: service + tests + PG integration)
- `api/internal/app/app.go` (wiring), `api/internal/testharness/harness.go`
- `helm/templates/prometheus-rules.yaml`, `helm/tests/alerts_promtool_test.yaml`
