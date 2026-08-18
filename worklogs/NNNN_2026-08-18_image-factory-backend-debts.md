# Worklog: image-factory backend debts — 409 collisions, one-call default move, seed vs runtime default

**Date:** 2026-08-18

**PR:** (this PR)

**Issue:** #936 (closes)

**Session:** follow-through on the #928/#930/#931 review findings

**Status:** all three debts fixed; store + handler tests green

---

## Objective

Close the three backend debts recorded during the base-update-pill
reviews: opaque 500s on scoped-name collisions (with an orphaned GH
dispatch on the fresh path), a bases upsert that cannot move the
platform default in one call, and a boot seed that silently reverts
runtime default moves.

---

## Context

PR #931's reviews (N1/N2 findings, and round-1's C3) traced each to
specific store/handler behavior; workarounds were documented in the
operator runbook. Issue #936 filed them together since they share the
bases-config surface and one review context.

---

## Work Completed

1. **23505 → ErrConflict** in both create paths (`CreateConfig`,
   `CreateConfigAndBuild`); handlers map `errors.Is(ErrConflict)` to
   **409** with a message naming the colliding config. The fresh path
   also cancels the already-dispatched GH run on conflict via the new
   `buildDispatcher.Cancel` (workflow_dispatch does not return run IDs,
   so today Cancel is a logged no-op with runID 0 — the orphan is
   bounded by the callback 404ing since no build row exists; the
   interface is future-proof for a synchronous-ID dispatch variant).
2. **Transactional default move**: `UpsertBase` with `isDefault: true`
   clears all other defaults in the same tx. `isDefault: false` upserts
   never auto-promote.
3. **SeedUpsertBase**: the boot seed's `is_default` applies only to
   INSERTed rows — runtime default moves survive API restarts. The
   seed stays authoritative for fresh installs and new bases.

---

## Key Decisions

- **Cancel on the dispatcher interface** rather than handler-level
  list-and-cancel: the run ID is genuinely unknowable at dispatch time
  (GitHub API limitation), so the honest contract is best-effort +
  bounded orphan, made explicit in the interface doc.
- **Seed split (SeedUpsertBase) vs conditional logic in UpsertBase**:
  explicit methods keep the admin path's clear-defaults semantics and
  the seed's preserve-semantics from interacting; one interface entry
  documents the distinction.

---

## Assumption validated (Rule 7)

- pq 23505 is the violation code for the scoped partial unique indexes
  (same mechanism as RenameConfig's existing mapping — reused, not
  guessed).
- SeedCatalog runs at every API boot (app.go:755) — validated by the
  reviewer's trace in #931 round 5; the insert-only fix keys off ON
  CONFLICT DO UPDATE excluding is_default.

---

## Tests Run

- Store integration (real Postgres, tag `integration`, CI-executed):
  CreateConfig collision → ErrConflict (+ scoped-uniqueness
  cross-owner OK); CreateConfigAndBuild collision → ErrConflict;
  UpsertDefaultBase clears others (and explicit-false leaves zero);
  Seed does not revert a runtime default (and still defaults a fresh
  insert).
- Handler: both paths return 409 with the colliding name; fresh path
  asserts Cancel called with the dispatched run ID; coalesced path
  asserts no cancel.
- `go test ./api/...` green; gofmt/vet clean.

---

## Blockers

None.

---

## Next Steps

- #936 closes with this PR.

---

## Files Modified

api/internal/services/database/imagefactory.go (+interface);
api/internal/handlers/imagefactory_create.go,
imagefactory_dispatcher.go, imagefactory_create_test.go,
imagefactory_test.go; api/internal/imagefactory/seed.go,
seed_test.go; store integration tests; docs/operator/image-factory.md.

---

## Addendum: GO-2026-6173 (lib/pq) — removed the module from all live paths

Mid-PR, govulncheck began failing on a same-day advisory
(GO-2026-6173, lib/pq pre-protocol unbounded read; NO fixed version).
The CI integration failures also exposed that the original pq-only
23505 type assertion never matched the production driver (pgx) —
two birds:

- isUniqueViolation: driver-agnostic SQLState() interface (pgx
  *pgconn.PgError + any legacy shape); unit-pinned with both drivers'
  error types (pgx direct, errors.Join-wrapped, stub, non-23505, nil).
  RenameConfig's latent same-bug fixed too.
- pq.Array (last live pq use, 25+ sites) replaced by a local
  stringArray Valuer/Scanner (canonical array-literal encoding,
  escape-safe, NULL-preserving); round-trip tests cover nil/empty/
  plain/quote/backslash/comma elements.
- testharness migrations: golang-migrate database/postgres →
  database/pgx/v5 (the v3 'pgx' driver still links lib/pq).
- Result (verified locally, govulncheck ./...): **0 affected
  vulnerabilities** — pq remains only as an uncalled transitive of
  golang-migrate, which symbol-level analysis correctly ignores.

Also fixed the seed test's fresh-install isolation (seed never clears
defaults; fresh-install semantics need an empty-default start).
