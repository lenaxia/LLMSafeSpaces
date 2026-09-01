# Worklog: US-70.5 secret-delivery demolition + 70.4 review fix

**Date:** 2026-09-01
**Session:** Finish US-70.4 review iteration (PR #1221), then implement US-70.5 — the Epic 70 demolition story (#1209): delete the legacy push/rehydrate/autopush machinery, grep-clean, with behavior pins proving pull-alone convergence.
**Status:** Complete

---

## Objective

1. Land US-70.4 (PR #1221 was CHANGES_REQUESTED on a rebase-damaged `prometheus-rules.yaml` + 2 failing checks) — the sequencing gate for US-70.5.
2. Implement US-70.5 per issue #1209 + design 0052 §5 Phase 4: demolish the `InjectSecrets`-era push path, `rehydrateDEKFromJWTSession` (K2), the `GetDEKForUser` live-session unwrap walk (K3), the K4 durable soft-unlock write half, the reload-cache handoff, `secretautopush` + `UserCredsPresent`, and `pushInitialSpawnEnv` — leaving notify-pull + reconcile as the only live updater, with a repolint grep pin preventing resurrection.

---

## Work Completed

### US-70.4 review fix (PR #1221)

- Repaired the 70.3↔70.4 rebase damage in `helm/templates/prometheus-rules.yaml`: `SecretsConvergenceSLOBurn` and `SecretsReconcileStalled` had lost their labels/annotations (summary/description had been misattached inside `KeyRewrapVerifyFailures`' expr block — invalid duplicate-key YAML); `KeyRewrapHalted` carried both its own and the stalled alert's text. Restored each alert to exactly one correctly-paired summary+description, byte-consistent with origin/main's 70.3 content. This fixed both failing CI checks (`TestPromtoolRules` in the helm suite was the red).

### US-70.5 demolition (branch `feat/us-70-5-demolition`)

- **Pod-side reload push path deleted**: `reloadSecretsHandler` + the agentd `/v1/reload-secrets` route, `writeReloadSecretsCache`/`loadReloadSecretsCache`, the materialize-time replay merge + boot cache write, `ReloadSecretsCachePath` + the `LLMSAFESPACES_RELOAD_CACHE_PATH` override (agentd + controller sidecar env wiring). `applySecretsBatch` — the post-parse pipeline shared with resync — extracted and kept; deps renamed `reloadSecretsDeps`→`applySecretsDeps`, `reloadMu`→`applyMu`. The API-side `POST /workspaces/:id/reload-secrets` route survives as the manual resync trigger (it calls `Notify`).
- **K2/K3/K4 deleted** (`pkg/secrets/key_service.go`): `rehydrateDEKFromJWTSession`, `GetDEKForUser` + `tryUnwrapRowWithKnownKeys`, `writeDurableDEK` + `UnlockDEKWithSigningKey` (collapsed into Redis-cache-only `UnlockDEK`), `JWTSessionKEKInfo`, `SigningKeyEnumerator` + auth's `EachSigningKey`, `GetJWTSession`/`WriteJWTSession` (zero writers remained).
- **keyrewrap recovery adaptation**: `DEKRecoverer.GetDEKForUser` → `GetCachedDEKForUser(ctx, userID)` — enumerates active `jwt_sessions` (bounded 5, most-recent first) and reads **only** the Redis `dek:<jti>` cache; never derives KEKs, never unwraps. `unwrappable_no_source` semantics unchanged. Also backs `GetDEKServerSide`'s legacy-row heal.
- **`secretautopush` + `UserCredsPresent` deleted**: the service, app.go wiring, the `WorkspaceUpdateCallback` watcher machinery, auto-push metrics, the controller health mirror, agentd `hasUserCreds`, and the CRD field (Go type + deepcopy + `helm/crds/workspace.yaml`). Live CRD schemas drop the field only when the operator applies the pruned CRD (out-of-band, per design 0052 — Helm never upgrades `crds/`).
- **`pushInitialSpawnEnv` deleted** (missed by the implementation pass — caught in orchestrator review): the dead US-4a boot-push function + its 4 tests; the `SpawnEnv` control-client method survives (other callers).
- **Fleet-version marker**: `agentd.DeliveryCapability = "v2"` surfaced as `HealthzResponse.Delivery` (omitempty — old runtimes omit it). `secretsreconcile.classify()` still judges convergence from `spawned_rev`; the marker is fleet-gauge evidence only, legacy_format counter semantics untouched.
- **Grep pin**: `repolint.DeletedSymbolsCheck` (wired into `cmd/repolint` → pre-commit + CI) fails on any non-historical `.go` reference to the 12 deleted identifiers; exemptions: `worklogs/`, `design/`, the pin file itself. Pinned by `TestDeletedSymbolsCheck_RepoTree`.
- **Behavior pins**: boot materializes with no cache concept; container restart re-materializes by pull alone (`TestContainerRestart_PulledEnvelopeReapplied`, `…_RevocationConvergesByPull`, `…_LLMProviderSurvivesRestart` — the #443 replay scenario, now by design); #1087 suspend/resume class covered by resync crash-window-heal + apply-guard tests.
- Stale-comment cleanup post-rename (`reloadSecretsDeps`/`reloadMu` references).
- Epic README story table updated (70.3 → #1212, 70.4 → #1221, 70.5 → in review).

### Pre-existing test-hygiene fixes (Rule 5)

- **`api/internal/services/metering` quota integration failures** (duplicate-key 23505 on `usage_limits`): root cause — `testharness.newID()` is a process-local counter (`h1, h2, …`), so every fresh `go test` process reuses owner IDs and collides with the previous run's residue in the shared `llmsafespaces_test` DB. Fixed at the root: harness IDs now embed `time.Now().UnixNano()` (`h<epoch>-<n>`); `seedLimit` upserts (`ON CONFLICT DO UPDATE`) and registers a `t.Cleanup` delete. Verified green across repeated runs.
- Known limitation (documented, not fixed — outside this PR's scope): outbox stress tests are not `-count>1`-safe (intra-process iteration collisions on fixed IDs); CI only ever runs `-count=1` and the package is untouched by this diff.

---

## Key Decisions

1. **CRD field fully removed from Go + chart (deviation from the letter of the brief).** The grep gate is test-enforced and `UserCredsPresent` must read zero; a retained Go field would fail the pin. The out-of-band live-schema concern is honored via the operator note: apply `helm/crds/workspace.yaml` (`kubectl apply` or `make helm-deploy`) in the same release that ships the last reader's removal (design 0052 §5 Phase 4). Stale live values are harmless (no reader) and drop on the next status write after the CRD apply.
2. **`GetCachedDEKForUser` replaces the K3 walk as keyrewrap's recoverer** (decided from design 0052 + #1209): login keeps populating the Redis cache (K1 survives); recovery works when the owner has an active session with a warm cache; otherwise `ErrDEKUnavailable` → `unwrappable_no_source`. No unwrap fallback anywhere — that is the demolition's point.
3. **`GetDEK`'s `matchedSigningKey` param retained-but-ignored** — same convention as `UnlockDEK`'s ignored `password`; unwinding the middleware plumbing is a mechanical ripple with no behavior change, out of scope.
4. **W15 legacy bare-array response + W1 multi-version window untouched** — they retire on fleet evidence (the new healthz marker + 70.4's v1-rows metric), not in this PR.
5. **Metering fix at the harness root** (unique-per-process IDs) rather than per-test deletes only — any future integration test seeding on `h.ID()` inherits the residue-tolerance.

---

## Blockers

None. The kind-cluster e2e rows (`local/us-70-secret-delivery-e2e.sh`) were not runnable in this environment (no kubectl/kind); the exec-level Go rows cover the same pull-alone behaviors, and the nightly e2e workflow will exercise the cluster rows.

---

## Tests Run

- `go build ./...` — pass
- `go vet ./...` / gofmt — clean
- `go run ./cmd/repolint` — all checks passed (incl. the new DeletedSymbolsCheck)
- `go test ./pkg/secrets/... ./pkg/repolint/... ./api/internal/services/keyrewrap/... ./pkg/agentd/... -count=1` — pass
- `go test ./cmd/workspace-agentd/ -count=1 -timeout 900s` — pass (184s)
- `go test ./api/... -count=1 -timeout 1200s` — pass (incl. metering, twice)
- `go test ./controller/... ./helm/... ./pkg/... -count=1 -timeout 1200s` — pass
- Full-suite -race runs defer to CI (the same suites CI executes).

---

## Next Steps

1. Merge the US-70.5 PR after review; apply the pruned CRD on the cluster (`kubectl apply -f helm/crds/workspace.yaml`) with the release.
2. Epic 70 close-out: README-LLM.md §Relay Config Subsystem still documents the reload cache/replay handoff — update those rows to as-built (pull-only) wording; verify the `legacy_format` counter decays to zero on the fleet, then retire the W15 legacy bare-array response and (when `user_keys` v1 rows read zero) the W1 multi-version window.

---

## Files Modified

96 files (+745 / −5,166) on `feat/us-70-5-demolition`, plus this worklog. Key surfaces: `cmd/workspace-agentd/` (server.go, secrets.go, healthz.go, spawn_env_consumer*.go, + deleted reload/has_user_creds tests), `pkg/secrets/key_service.go`, `api/internal/services/keyrewrap/service.go`, `api/internal/app/` (app.go, secrets_adapters.go), `api/internal/services/workspace/watcher*.go`, `api/internal/services/secretautopush/` (deleted), `controller/internal/workspace/` (health.go, agentd_sidecar.go), `pkg/apis/llmsafespaces/v1/workspace_types.go` + deepcopy, `helm/crds/workspace.yaml`, `pkg/repolint/deleted_symbols*.go`, `api/internal/testharness/harness.go`, `api/internal/services/metering/quota_integration_test.go`, `design/stories/epic-70-secret-delivery-v2/README.md`, `sdks/canary/go/scenarios/d-cred-model-flow/main.go`.
