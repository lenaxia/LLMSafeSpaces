# Worklog: #1901 — carrier-reap follow-ons (kind discriminator + drain tombstone visibility + restart posture)

## Charter

Issue #1901 (split from PR #1897's r1 review): (a) origins-marked
carrier reap bypass needs a per-row discriminator; (b) the
PreserveAlways/onFailure-preserved sessions must keep the
operator-review guard (design question, packeted pre-implementation);
(c) drain-time tombstone visibility for queued RecordMessage events;
(d) restart-wiped tombstones — verify + document the accepted posture
with the reap net as the converging backstop. Branch
`fix/1901-carrier-reap` off origin/main @ 8a35e5c6; PR #1897's squash
expected mid-flight (ordinary rebase only; union is the orchestrator's).

Rider (approved, this charter): dedupe the doubled
`// reconcileOutcomes counts…` comment opener in
api/internal/services/sessionindex/service.go left by an earlier union
— target verified present on the #1897 BRANCH (service.go:27-28) and
ABSENT on main at lane start; applied at rebase time if the squash
lands it, else reported.

## Ground truth at lane start (main @ 8a35e5c6)

The #1882 first squash (1764a1a9) had ALREADY landed a BLANKET origins
bypass: `SessionOriginLister.ListSessionOrigins` (map[string]string)
fed BOTH reap paths (reconcile plan closure,
proxy_session_index.go:228-231; read-path reapSessionIfGone :126) with
`marks[id] != ""` — every session_origins mark admitted a
history-bearing row past DefaultReapGuard. The (b) defect was live:
`recordRoutineSessionArtifacts` (engine.go:1207) writes PERSISTENT
marks for sessions that SURVIVED their fire (PreserveAlways,
PreserveOnFailure-failed) — a harness store reset would reap their
history rows.

## Design (item b — packeted pre-implementation, orchestrator APPROVED)

Options considered: (1) per-row `kind` column written at mark time;
(2) resolve the trigger's preserve mode at reap time via TriggerID —
REJECTED: PreserveOnFailure produces both preserved (failure leg) and
deleted (success leg) sessions from the SAME trigger; the mode cannot
discriminate a row's fate, and triggers are mutable/deletable;
(3) infer from mark persistence (carriers' marks are cleaned
post-effects) — REJECTED: the mark survives exactly when propagation
partially failed, the precise case the bypass exists for.

PICK (1): the two writers are the only components that know the fate
at write time — `propagateCarrierDelete` runs ONLY for
agentd-reported-deleted ephemerals (mark transient, cleaned after
effects); `recordRoutineSessionArtifacts` runs ONLY for survivors.
Write-time discrimination, durable across restart (feeds (d)'s
backstop), no reap-time lookups.

Accepted residual (orchestrator ack): DEFAULT 'session' backfill means
pre-migration carrier marks that outlived their cleanup stop being
reaped — stale ghosts until manual/aging cleanup. Safe direction:
deletion is irreversible.

## The changes

### (a)+(b) — migration 000046 + discriminator (red caf91e52 → green 90f53608)

- `api/migrations/000046_session_origins_kind{.up,.down}.sql`:
  `session_origins.kind` text NOT NULL DEFAULT 'session', CHECK
  ('carrier','session'). Down drops both (post-down the carrier lookup
  errors → bypass fail-safes to NO marks).
- `pkg/types/workflows.go`: SessionOriginKindCarrier/SessionOriginKindSession.
- `pkg/workflows/store.go`: Kind on SessionOriginRow (zero value
  normalizes to 'session' in Go AND the column DEFAULT in SQL);
  RecordSessionOrigin writes it on INSERT + conflict legs; the upsert
  leg RE-CLASSIFIES (a re-driven fire cannot leave a stale verdict);
  ListSessionOrigins round-trips it; NEW ListCarrierOriginMarks returns
  kind='carrier' ids only (map[string]bool).
- `api/internal/workflows/engine.go`: propagateCarrierDelete →
  Kind=carrier; recordRoutineSessionArtifacts → Kind=session.
- handlers: `SessionOriginLister` seam becomes
  `ListCarrierOriginMarks` — preserved marks are UNREACHABLE at the
  guard on both reap paths; `reaped_carrier` metric semantics updated.
- app.go: adapter delegates to the discriminated store lookup.
- helm/migrations mirror synced (repolint).

### (c) — drain-time tombstone visibility (red 4cb6e4d9 → green d398a329)

- `sessionindex.Service` gains `DeletedSessionChecker`
  (SetDeletedSessionChecker; nil = pre-fix apply-unconditionally
  semantics, pinned). `drain()` consults it on BOTH legs (live loop +
  shutdown flush) and drops tombstoned events as a queryable
  `session_index_events_total{outcome=drain_skip_tombstoned}`.
- `ProxyHandler.DeletedSessionChecker()` exposes the live-store consult
  (resolves h.state() per call — survives the redis SetStateStore
  swap; inherits wsstate's fail-closed outage posture: a Redis outage
  suppresses drain re-mints, data integrity over availability —
  worklog 371 M1's intended drain gating made real).
- app.go wires it beside SetSessionOriginLister; Start() runs later —
  no setter/drain race.
- Enqueue-path filtering deliberately NOT added: the drain consult is
  at the minting moment (last possible); an enqueue filter would add a
  per-message store hit and still leave the check-to-drain window open.

### (d) — restart posture (pin only)

`TestSessionIndexReconcile_RestartRebornCarrierRow_Converges`: fresh
tombstone map (asserted empty — the accepted posture), re-minted
carrier row with history, durable kind='carrier' mark → two reconcile
passes reap it, counted as reaped_carrier. The wsstate tombstones, the
known-registered LRU, and (same class) the drain checker's map die on
restart; the durable deleted_at mark dies with hard-deleted rows; the
kind='carrier' mark is the ONE death signal that survives — hence the
(a) reap net is the backstop. Residual (accepted, per store design):
carriers whose propagation NEVER ran (transport blind spot #1470-class)
leave no mark → kept_for_operator until operator review.

## Red-first evidence + mutation recipes

- (c) RED 4cb6e4d9: both drain-leg pins PANIC (mock receives the
  tombstoned pair's UpsertSessionMessage — the unconditional apply IS
  the defect); nil-degradation pin passes by design.
  Recipes: (1) delete the two `if s.tombstoned(ctx, ev)` guards →
  both pins RED; (2) drop the `!` in tombstoned()'s consult → same +
  nil pin RED; (3) capture a stale store in the handler pass-through
  instead of resolving per call → wiring pin RED after a store swap.
- (a) RED caf91e52: `TestExecuteRoutine_OriginKind_DiscriminatesFate`
  fails both legs ('' vs 'carrier'/'session').
  Recipes: (1) delete `Kind:` from either engine writer → that leg
  RED; (2) revert ListCarrierOriginMarks's SQL filter (drop the kind
  predicate) → PG pin + (via seam) the preserved-kept pin's premise
  breaks (CI tier); (3) revert the handlers admit sites to
  `marks[id] != ""` shape → compile (map[string]bool vs string), and
  with the old seam shape restored the preserved-kept pin RED against
  the blanket adapter — the committed red demonstrates the mechanism
  at the writer tier.
- (b)/(d) pins green at HEAD: preserved-kept/carrier-reaped same pass;
  restart-reborn convergence.

## Observations (disclosed, NOT touched — follow-up candidates)

1. The DAG reconciler's `propagateDeletedSession` (engine.go:572) runs
   effects but never carrier-marks (ReconcilerStore lacks the origin
   methods) — a DAG ephemeral whose effects' index write fails is an
   UNMARKED ghost (kept_for_operator). Same class:
   `deleteSessionAuthorized` legs (PreserveOnFailure success,
   retry-intermediate, DAG node-retry) delete sessions that were never
   origin-marked. Both limit the reap net's coverage, neither risks
   the (b) wrong-reap (they are not preserved sessions). #1897's
   branch reshapes this area — union concern.
2. `fetchAndPersistTitle`'s title-race row is count-0 → self-heals via
   the existing reap grace (the #1897 r2 review's residual note);
   belongs to the same family as (c) but sub-harm.
3. e2e harness rows for the discriminated reap would ride
   local/issue1452-routine-session-index-e2e.sh (R3's neighborhood);
   not extended this lane — unit/integration coverage pins the
   mechanism; flagged for the harness lane.

## Verification

- `go test -count=1` green: api/internal/services/sessionindex (+
  -race), api/internal/workflows (full 80s), api/internal/handlers
  (full 99s), api/internal/app, pkg/workflows, pkg/types.
- vet + gofmt + goimports + golangci-lint (pre-commit) clean;
  repolint clean (46 migrations, chart mirror synced).
- PG-gated: store integration pin (kind round-trip + carrier-only
  filter + upsert re-classification) compiles locally, skips without
  PG, runs in CI; migration 000046 rides CI's migration-safety suite
  (docker unavailable locally — pre-commit skipped it, disclosed).
