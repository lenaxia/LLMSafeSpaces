# Worklog: US-70.3 — notify-pull + reconcile loop + revocation + secrets_resync (#1207)

**Date:** 2026-09-01
**Session:** Epic 70 US-70.3 (design 0057 law 1/3/4; absorbs design 0052 Phase 3; replaces D5 self-heal). Four parts implemented in parallel (pod/controller, API, MCP tool, e2e), one full adversarial validation round, all must-fix findings remediated.
**Status:** Complete — full suite green; cluster rows first-run in CI (pool/nightly).

---

## Objective

Flip live change delivery from batch-body push to **notify → re-pull**, add the level-triggered **reconcile loop** (metrics + SLO alerts), implement **revocation-is-absence** with fan-out, and the **`secrets_resync`** agent self-heal (PR-4).

---

## Work Completed

### Pod + controller (Part A)

- `POST /v1/resync-secrets` on the agentd user mux (`cmd/workspace-agentd/resync_secrets.go`): §D1 auth; SA token read **fresh per call** (pinned); conditional v2 pull; **304 no-op** (with a crash-window heal: an on-disk envelope newer than the applied anchor is applied — guarded to never revert push-invalidated state); **200** → `applySecretsBatch` → anchor read-back (I4) → session-aware restart decision (#852 preserved); `429 rate_limited` (min-interval env, default 2s, admitted-attempt budget); `502 pull_failed/pull_unauthorized`; body **never** influences the applied batch (0050 finding-3, adversarially pinned).
- `applySecretsBatch` extraction: reloadSecretsHandler's post-parse pipeline (lock → materialize → anchor → deliverStaged → cache → enrich → configwrite → StageCredentials → restart) shared by reload (unchanged behavior, tests untouched) and resync; the anchor write moved under `reloadMu` (validator m1).
- Controller: single-container main container gains the `bootstrap-token` mount (RO) + `LLMSAFESPACES_BOOTSTRAP_SECRETS_OUT=/sandbox-runtime/rt/secrets.json` (RW coordinate — B1 fix; sidecar already had both). Pins: main-mount topology, single-container batch-path RW contrast, sidecar uid-1000-denied unchanged.

### API (Part B)

- `agentpush.Notify`: empty POST, §D1 Basic, 5s timeout, Active-pod gate; **429 → one bounded deferred retry** (`time.AfterFunc(min(retryAfterMs, 2.5s))`, shutdown-cancelling — M2 fix); outcomes counted. `Push` (body push) **deleted**; all call sites (bind/reload/env/MCP adapters) notify — zero body-push callers (grep-pinned). Mixed fleet: old pods 404 the notify — non-fatal, backoff-capped.
- Reconcile service (`api/internal/services/secretsreconcile/`): periodic (60s default, env-overridable; immediate first pass), **live-manifest compare** — per Active workspace: `ManifestFor` (zero decrypts, owner from CRD `spec.owner.userID`, same field the pull path uses — flapping disproven + pinned) → `EnsureRevision` mints drift (conditional 304→200 flips exactly when the loop observes change) → converged := storedSeq == appliedSeq (spawnedRev prefix). Rules: empty manifest converged only with a parseable matching rev (M1a fix — a legacy pod can no longer read "converged" while serving revoked plaintext); legacy/unparseable rev → `legacy_format` (counter, **no page** — rollout alert-storm fix, gauge stays converged; converges with the fleet upgrade per US-70.5); per-workspace error isolation; backoff 5s×2ⁿ cap 10m +25% jitter, reset on convergence; gauges withdrawn on deactivation.
- **ForceRevokeSecret** (I12): FK cascade unbinds everywhere (migration-verified); eager per-workspace revision refresh (ManifestFor+EnsureRevision — revoke latency never depends on loop timing); notify fan-out (failures non-fatal); `action='revoke'` audit rows; env-var DELETE routes through it. Plain `DeleteSecret` deleted (dead — folded).
- Metrics + alerts: notify/reconcile/divergent counters by reason, converged gauge, last-pass timestamp; `LLMSafeSpacesSecretsDeliveryDivergent` (>15m, critical), `ConvergenceSLOBurn` (<99%/5m, empty-fleet-guarded), `ReconcileStalled` (3× period incl. absent()) — promtool-validated.

### secrets_resync (Part C)

- Platform MCP = agentd's in-pod `/v1/mcp` (validated — the injected entry points at agentd, design 0052 §4.7): tool `secrets_resync` (no input properties; undeclared args ignored — pinned that they can never influence behavior) → authenticated loopback POST to the pod's own resync (bounded 10s) → `{appliedRev, converged}` | `{error:"rate_limited", retryAfterMs}` | `{converged:false, pending:true}` (API unreachable). Shares the endpoint's rate-limit budget with notify + loop (I15).

### E2E (Part D)

- Rows: AC-3 (seq bump ≤30s hard + env ≤60s hard, both wall-clocks reported — the split is the restart semantics, documented, not a silent loosening), AC-4-lite (pod-delete mid-bind, monotonic seq), AC-5 (revoke live: env absent ≤60s, audit row), AC-6 (revoke suspended → boots clean), AC-8/AC-10 (API down: loud 502 + last-good survives; recovery converges ≤2×interval+30s), AC-11 (resync via pod port-forward: applied/not_modified/429 shapes).
- Helpers: `spawned_seq`, `env_absent_from_child` (mid-restart guard), `resync_pod`, `api_down/api_up`; workflows set `api.extraEnv[0]=LLMSAFESPACES_SECRETS_RECONCILE_INTERVAL=5s` (single `--set` — helm list-clobber hazard avoided, pin-tested); pin suite extended.

---

## Assumptions (Rule 7 — stated and validated)

| # | Assumption | Validation |
|---|---|---|
| A-1 | Projected token readable in-container | uid-2000 sidecar reads the same kubelet-written file today; no new uid crossing (matrix checked) |
| A-2 | Chart-default (single-container) resync batch path writable | B1 fix: env-wired to `/sandbox-runtime/rt/secrets.json` + RW/RO contrast pin; `/sandbox-runtime/rt` created 0770 by init-fs |
| A-3 | Loop owner == builder owner (no 304/200 flapping) | Both read CRD `spec.owner.userID`; `TestManifestFor_MatchesBuilderRevision` |
| A-4 | FK cascade unbinds everywhere on secret delete | migration 000001:1691 `ON DELETE CASCADE`; affected set read pre-delete |
| A-5 | The platform MCP entry points at agentd, not the API | `injectAgentdMCPServer` → `http://127.0.0.1:4097/v1/mcp` (#847) |
| A-6 | SDK reload-response consumers | grep across sdks/ — go/ts/canaries updated; python/java carry no typed shape |

## Key Decisions

1. **Live-manifest compare over stored-row compare** — the loop derives expected from the same rows the builder uses and mints drift itself; no mutation-site refresh discipline, org/global effective-set changes converge within one period (closes the false-convergence class found in review).
2. **Legacy pods inform, don't page** — `legacy_format` counter without gauge divergence; notify attempted once under backoff; convergence for them is the fleet upgrade (US-70.5's gauge), not a loop that can never succeed.
3. **429 → one bounded deferred retry** — never blocks the bind path, shutdown-safe; the reconcile loop remains the backstop.
4. **Eager refresh only on revoke** — the one path where latency must not depend on loop timing.

## Adversarial review (Rule 11)

One validator round over the full diff. Must-fix (all fixed): single-container RO batch path (B1 — env coordinate + pins), mixed-fleet silent non-revocation + alert storm (M1 — classify + gauge semantics), 429-notify drop vs AC-3 budget (M2 — bounded retry), SDK/canary response-contract break (M3 — openapi + go/ts/canaries). Minors fixed: anchor-under-lock, dead DeleteSecret, stale worklog paragraph, metrics help text + skips counter. 13 false-alarm classes investigated and disproven with source evidence (phantom mints, loopback deadlock, owner flapping, token caching, body influence, FK cascade, gauge cardinality, workflow --set syntax, 304 heal revert, …) — key ones recorded above.

## Blockers

None in-repo. Cluster rows first-run in CI. Known deviations recorded: AC-8's healthz→CRD degrade leg is deliberately absent (a failed resync keeps last-good; the loop detects non-convergence — surfaced via divergent metrics instead); AC-5 file-class ≤10s timing not rowed (env-class ≤60s is); single-container cluster resync row unwired (controller-pinned; the attachments-single-container workflow can carry it later — noted to the operator).

---

## Tests Run

- `go build ./...`; vet/gofmt/goimports clean
- `-race`: secretsreconcile, agentpush, metrics, handlers, app, controller/workspace, pkg/secrets, cmd/workspace-agentd (237s), helm (real promtool 2.53), local pins, sdks (openapi validate + canary build)
- Full sweep: 69 packages ok + slow cmd suites ok; API sweep zero failures

---

## Next Steps

1. Commit → PR → review iterate → merge.
2. Triage pool run 7's failure (post-resources-fix; AC-1/AC-2 were green in run 6).
3. US-70.4 (#1208): re-wrap reconciler. Then US-70.5 (#1209): demolition + fleet gauge.
4. Epic close-out: README-LLM secret-delivery rewrite, story table, DoD sweep.

---

## Files Modified (summary)

- `cmd/workspace-agentd/`: resync_secrets.go (+tests, exec tests), secrets.go (applySecretsBatch + anchor lock), server.go, bootstrap.go, sidecar_boot.go/sidecar_mode.go, mcp_server.go (secrets_resync + tests)
- `controller/internal/workspace/`: pod_builder.go (token mount + batch-path env), agentd_sidecar.go, pins (pod_spec_consistency, security, agentd_sidecar_pod)
- `api/internal/services/agentpush/`: Notify + deferred retry (Push deleted) (+fakes/tests)
- `api/internal/services/secretsreconcile/`: new (service + tests + zero-decrypt 1k pin)
- `api/internal/handlers/`: secrets.go, workspace_env.go (notify flip + revoke routing) (+tests)
- `api/internal/app/`, `api/internal/services/metrics/`, `pkg/secrets/` (ForceRevoke, EnsureRevision passthrough, DeleteSecret removed)
- `helm/templates/prometheus-rules.yaml` + `helm/tests/alerts_promtool_test.yaml`
- `sdks/`: openapi.yaml, go/types.go, typescript/client.ts, canaries (go/py/ts) + TESTPLAN
- `local/`: lib helpers + suite rows + pins; both workflows (reconcile-interval set)
