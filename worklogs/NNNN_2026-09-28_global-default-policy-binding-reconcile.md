# Worklog: Global-default secret policy → binding reconcile (closes the "all workspaces" delivery gap)

**Date:** 2026-09-28
**Session:** Root-caused why global_default user secrets never reached existing workspaces; implemented binding provenance + level-triggered policy convergence.
**Status:** Complete

---

## Objective

A user created two global-default secrets ("Include in all workspaces") and assigned them, but no existing workspace received them — the queue/batch pipeline never saw the secrets at all. Diagnose why, then implement the correct long-term fix.

---

## Work Completed

### Root cause (diagnosed on the live cluster, workspace d8bed486)

- Batch construction is strictly binding-row-driven: `loadWorkspaceRows` (`pkg/secrets/injection.go`) reads `user_secret_bindings` only; the `global_default` flag is never consulted.
- The flag's only consumer is `SeedGlobalDefaultSecrets`, called solely from `CreateWorkspace (`api/internal/services/workspace/workspace_service.go:475`). Workspaces created before the secret exists never get a binding row: revision stayed seq=33, no push, `AGENTIC_*` absent from the pod, `staged-secret-files/` empty. Restart/resume cannot fix it (bootstrap pulls build from the same empty bindings).
- Contrasting precedent: LLM credentials sweep existing workspaces (`BindCredentialToAllUserWorkspaces`); user secrets never had the equivalent.

### Decision (see Key Decisions)

Chose **policy materialization** (flag → binding rows, level-triggered by the existing `secretsreconcile` loop) over **builder-side union**, preserving the "one builder, one truth" contract.

### Implementation

- **Migration 000033** (`bind_source`): `user_secret_bindings` gains `bind_source varchar(16) NOT NULL DEFAULT 'manual'` + CHECK (`manual` | `global_default`). Existing rows backfill `manual` (conservative: policy convergence never removes them). Mirrored byte-identical into `helm/migrations/`.
- **Store seam**: `SecretStore.SyncGlobalDefaultBindings(workspaceID, secretIDs) (added, removed, err)` — one tx + the existing workspace advisory lock; inserts missing auto rows (`ON CONFLICT DO NOTHING` — a manual row shadows), removes auto rows absent from the keep-set (flag flips; deleted secrets' rows already cascade via the table's `ON DELETE CASCADE` FKs — verified live). Implemented on `PgSecretStore`, `AsyncAuditLogger`, the in-memory `dbSecretStoreAdapter`, and every test fake that implements `SecretStore`.
- **Service seam**: `SecretService.SyncGlobalDefaultBindings(owner, ws)` — lists the owner's defaults, calls the store sync, audits `bind`/`unbind` with `source=global_default`. `SeedGlobalDefaultSecrets` now delegates to it (same semantics, now provenance-tagged).
- **Reconcile step 0** (`secretsreconcile`): optional `PolicySource` dep (`WithPolicySource`), called per Active workspace BEFORE `ManifestFor` so a flag flip diverges the manifest in the SAME pass and rides the existing mint+notify machinery. Failure → per-workspace skip (`policy_sync`), never fatal. New counter `llmsafespaces_secrets_reconcile_policy_bindings_total{op=added|removed}`; wired in `app.go` with `secretService`.
- **Boot-time convergence** (`pod_bootstrap.go`): suspended workspaces are outside the Active enumeration, so the bootstrap handler runs the same sync (optional type-assertion seam) after workspace lookup, before the v2 manifest compare — best-effort, failure logged and the boot proceeds.
- **Pre-existing lint failure fixed** (Rule 5): `pkg/repolint/ci_semver_tag_race_test.go` S1005 (`raw, _ := push["tags"]` → `raw := push["tags"]`).
- **Pre-existing flaky test fixed** (Rule 5): `TestUploadApply_ConcurrentWallTime` (`cmd/workspace-agentd`) failed deterministically on loaded hosts (observed 235–329ms vs the 150ms threshold) — relaxed to the actual serialization signature (wall > N×duration) which is the load-robust discriminator; regression still caught (serialized ≥ 200ms fails).

---

## Key Decisions

1. **Materialization over builder-side union.** Union would split the effective-set truth across builder, `ManifestFor`, the 304 conditional pull, the reconcile convergence check, and the UI bindings view — and silently break the reconciler's "converged" certification (union in builder only = pod diverges while the loop says converged). Bindings stay the single truth; the flag is policy input materialized into rows. This mirrors the platform's existing K8s-style level-triggered philosophy (the loop already owns revision/pod convergence).
2. **Binding provenance (`bind_source`) is load-bearing, not cosmetic.** It resolves the three hard edges: toggle-off (reconciler removes only auto rows — never destructive to manual binds), manual-unbind respect (manual rows never touched), and `SetBindings` replace interplay (manual replace clears auto rows; reconciler re-asserts current policy next pass — correct, since "all workspaces" means exactly that).
3. **Loop = correctness path; bootstrap sync = coverage for suspended workspaces; create-time seeding = latency fast path.** A failed event-time path self-heals within one interval (60s default). No event-time fan-out notify added (follow-up if sub-interval latency is ever required).
4. **Existing rows backfill `manual`, not `global_default`.** Retroactive distinction is impossible; manual is the conservative label (nothing is auto-removed).
5. **Assumptions stated and validated** (Rule 7): bindings-table FKs are `ON DELETE CASCADE` (validated against live schema — invalidated my initial orphan-cleanup premise; the DeleteSecret tx change was reverted as unnecessary); `SyncGlobalDefaultBindings` on every bootstrap adds one manifest-tier query per boot (acceptable — boot is infrequent); reconcile pass cost grows by one `ListGlobalDefaultSecrets` + one advisory-locked tx per Active workspace per pass (same order as the existing manifest reads).

---

## Blockers

None.

---

## Tests Run

- `go test ./pkg/secrets/` — ok (new: 8 service-level sync tests incl. idempotence, manual-shadow, owner isolation, error propagation, seed provenance).
- `go test -tags integration -run 'TestPgSyncGlobalDefaultBindings|TestPgDeleteSecret' ./pkg/secrets/` with a throwaway Postgres 16 (docker, localhost:5433, migrations 1→33 applied) — ok (full lifecycle: add/idempotent/flag-flip-remove/empty-keep-set/manual-survives; FK cascade pinned).
- `go test ./api/internal/services/secretsreconcile/` — ok (new: policy-before-manifest ordering, metric counting, per-workspace skip on failure, nil-seam backwards compatibility).
- `go test ./api/internal/handlers/` — ok (new: bootstrap sync-before-manifest, best-effort on failure, legacy injector unaffected).
- `go vet ./...`, `make lint` (golangci-lint 0 issues), `gofmt` clean.
- `make test`: all packages pass except `TestRestart1342_ProgressStops_ForcePathFires` in `cmd/workspace-agentd` — **pre-existing intermittent flake**: reproduced failing on unmodified main (worktree at 1d00d4e4) under full-package load, passes in isolation everywhere. Unrelated to this diff (touches no agentd restart code). Left as a separate follow-up.

---

## Next Steps

- Reviewer iterate → merge → deploy; verify on the live cluster: the two `agentic-actor-coder*` secrets bind to all Active workspaces within one reconcile interval and push seq bumps (`kubectl get ws <id> -o yaml` secretsDelivery + pod healthz filesRev).
- Follow-up: investigate the `TestRestart1342_ProgressStops_ForcePathFires` inter-test flake.
- Follow-up (separate bug, found during diagnosis): `CredentialsStaged StageFailed — http: server gave HTTP response to HTTPS client` — agentd calls the internal llm-providers endpoint with the wrong scheme.
- Optional follow-up: unify `BindCredentialToAllUserWorkspaces` (credentials) under the same provenance/reconcile pattern — it has the same latent toggle-off/manual-unbind gaps.

---

## Files Modified

- api/migrations/000033_binding_source.{up,down}.sql (new) + helm/migrations mirror (new)
- pkg/secrets/types.go — BindSource consts
- pkg/secrets/store.go — SyncGlobalDefaultBindings interface contract
- pkg/secrets/pg_secret_store.go — PG impl + AsyncAuditLogger delegation
- pkg/secrets/secret_service.go — service seam + Seed delegation
- pkg/secrets/sync_defaults_test.go, sync_defaults_pg_test.go (new)
- pkg/secrets/secret_service_test.go — mock provenance model
- pkg/secrets/mcp_injection_test.go — fake grows seam
- api/internal/services/secretsreconcile/service.go — PolicySource + step 0 + reason
- api/internal/services/secretsreconcile/policy_test.go (new), zerodecrypt_test.go
- api/internal/services/metrics/metrics.go — policy bindings counter
- api/internal/handlers/pod_bootstrap.go — bootstrap policy seam
- api/internal/handlers/pod_bootstrap_policy_test.go (new), pod_bootstrap_e2e_test.go
- api/internal/handlers/secrets_test_helpers_test.go, secrets_notify_test.go, secrets_push_session_test.go
- api/internal/services/auth/auth_e2e_secrets_test.go — memSecretStore provenance
- api/internal/app/app.go — WithPolicySource wiring
- api/internal/app/secrets_adapters.go — in-memory adapter sync
- cmd/workspace-agentd/reload_credentials_e2e_test.go, upload_apply_test.go (flake fix)
- pkg/agentd/secrets/mcp_contract_seam_test.go
- pkg/repolint/ci_semver_tag_race_test.go (lint fix)
