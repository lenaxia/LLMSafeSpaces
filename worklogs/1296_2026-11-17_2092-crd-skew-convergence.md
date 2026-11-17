# Worklog — 1296 · 2026-11-17 · issue #2092: born-unbound CRD-skew convergence — release-train guard, alerting, degraded UX, create-time probe, recovery helper

**Branch:** `fix/2092-crd-skew-convergence` (worktree `/tmp/opencode/yeasty-magnet-scores`, off `origin/main` @ `bd56d8464`)
**Issue:** #2092 (evidence chain + root-cause verified by orchestrator review 2026-11-17; owner-directed rulings 1–6 layered on the issue's follow-up list)

## The incident class (one paragraph)

`helm upgrade` never upgrades `crds/`; on the Flux-managed production cluster the `cluster-llmsafespaces-crds` kustomization path was not bumped with the .15/.16/.17 train, so the cluster served the pre-0.44.15 Workspace CRD (no `spec.harness`). The structural apiserver PRUNES the unknown field on every write and returns success → every bind write landed nowhere → born-unbound workspaces (the frontend-picker path) wedged in `Creating` forever: no pod, no convergence, no alert. The .16 fix made the bind FAIL LOUD (`harness_bind_not_persisted`) but the wedge itself stayed eternal. This PR closes the six ruling gaps around it — process, observability, UX, creation-time honesty, recovery, and pins.

## Ruling 1 — release-train CRD guard (repolint + release doc)

- **`deploy/flux/crds/`** — the tracked Flux path mirror: byte-identical copies of ALL THREE platform CRDs (`workspace.yaml`, `runtimeenvironment.yaml`, `inferencerelay.yaml`) + a `kustomization.yaml` that lists exactly that set. This is the repo-local representation of what the ops repo's Flux kustomization must reference; the release checklist (§2's new CRD-apply step) carries it into the ops repo.
- **`pkg/repolint/flux_crd_drift.go`** — `FluxCRDDriftCheck`: every `helm/crds/*.yaml` must be byte-identical to its mirror; the kustomization must list exactly the mirrored set (unlisted = Flux never applies it; stale entry = breaks the whole path's reconciliation). Fails `make repolint` (pre-commit + CI) **in the same PR** as the schema change — born-red. Fix side: `SyncFluxCRDs` + `-fix-flux-crds` + `make flux-sync-crds` (the `chart-sync-migrations` shape). 9 test pins incl. `TestLive_FluxCRD_NoDrift` (the live-tree CI pin) and the synthetic-drift born-red (verified red then green on the real binary).
- **`docs/release-bump-checklist.md`** — §1 table row (CRDs / chart `crds/` source of truth / Flux mirror), §2 config-PR step ("CRD apply (when the release carries a CRD schema change)"), §5 guards-table row, and new **§6 "The CRD apply train gate"** — schema change = train gate on CRD apply, with the 2026-11-17 incident as the origin story.

## Ruling 2 — observability (metric + alerts, #2047 fatigue discipline)

- **Controller: `llmsafespaces_crd_skew`** gauge, labels `(crd, field)` — ONE series per pruned-field class, deliberately NOT per workspace (a fleet-wide wedge is one page, never a storm). Writers: `recordCRDSkewSet()` at the backfill pruned verdict; `recordCRDSkewHealed()` when a stamp LANDS (resolution-on-heal edge); the once-per-process startup probe; the on-skew re-probe.
- **Controller: the startup capability probe** (`crd_skew.go`) — a server-side DRY-RUN create of a minimal Workspace carrying `spec.harness=opencode` runs the full apiserver pipeline (admission + structural pruning) and returns the object as-it-would-persist. Pruned read-back = skewed (conclusive); kept = healed (conclusive); ANY error = inconclusive (#1685 absence-is-not-evidence — never arms the gauge). Rides the FIRST reconcile (not a boot hook): the reconciled workspace's namespace is by construction one the controller holds create RBAC for. Zero new RBAC (DryRun rides the existing create verb), zero CRD-read grant.
- **On-skew re-probe** (`maybeReprobeHarnessCRD`, piggybacked on the backfill verdict sites — no new loop): while skewed, re-probe every `reprobeHarnessCRDAfterHealWindow` (10m) so the gauge heals within minutes of the CRD landing even if NO legacy workspace ever backfills over it.
- **API: `llmsafespaces_harness_bind_not_persisted_total`** counter, label `surface` (`bind_verify` = live bind verifies failing; `create_probe` = harness-less creates refused at the gate) — registered on the default registry the API's authenticated /metrics serves.
- **Alerts** (`helm/templates/prometheus-rules.yaml`): `LLMSafeSpacesCRDSkew` — **critical** (a stale CRD wedges every new picker-path workspace — it pages), `for: 15m` (the gauge only moves on conclusive verdicts; rides out blips), auto-resolves on heal (gauge → 0), never needs a manual clear, cannot dangle after recovery. `LLMSafeSpacesHarnessBindNotPersisted` — warning echo on `increase(...[15m]) > 0` held 5m: only fires while users are actively blocked, self-resolves when binds verify clean; no absent() arm (a healthy cluster legitimately never ticks it). Chart pins: names added to `ContainsAllAlerts` + new `TestMonitoring_PrometheusRule_CRDSkewAlerts` pinning the SHAPE (critical/controller/max()/15m + warning/api/increase()/5m).

## Ruling 3 — UX (BindNotPersisted condition + degraded hint)

- **`v1.WorkspaceReasonBindNotPersisted`** — the degraded reason on a True `HarnessBindingPending`.
- **API writes it** (`markBindNotPersisted`, the `ReportRelayBatchOutcome` retry idiom): on a bind that verified un-persisted, the condition upserts to `True/BindNotPersisted` with the operator-actionable message, best-effort (never masks the bind verdict). Heals on the next successful bind (the controller's `clearHarnessBindingPending` bound path flips any True binding-pending to `False/HarnessBound` regardless of reason).
- **Controller respects it**: `harnessBindingPendingIsParked` now parks BOTH steady reasons (`HarnessUnbound`, `BindNotPersisted`); the gate's idempotence probe dropped the exact-message conjunct so a parked DEGRADED verdict writes NOTHING on later reconciles (pinned: zero status writes, reason stands — pre-change main rewrote it away, the flap that would have erased the surface).
- **DTO + frontend**: `WorkspaceListItem.bindBlocked` (set from the condition in `fetchUserWorkspaceStates`); `Sidebar` renders the degraded hint `blocked on cluster CRD — operator action required` (amber, tooltip with the remediation + "your harness choice is kept and will bind once it lands") INSTEAD of the bare `Creating` phase text — the honest state is operator-actionable, not "in progress". Pins: hint renders + bare `Creating` absent; contrast pin (healthy unbound wait unchanged). Types + tests updated.

## Ruling 4 — create-time capability probe (fail-loud-at-creation)

- **`api/internal/services/workspace/crd_capability.go`** — the empirical, CACHED probe: a server-side dry-run create (via the dynamic client — the typed seam has no options parameter; same endpoint, `CreateOptions.DryRun`) of the minimal harness-bearing Workspace. Verdicts `supported` / `skewed` / `unknown`; cache TTL 60s (bounds post-heal staleness to a minute, caps probe traffic at one call/window regardless of create QPS — never per-request), **invalidated the moment a bind verifies un-persisted** (the freshest skew evidence). Inconclusive NEVER gates.
- **The gate** (`CreateWorkspace`, omitted-harness branch only): skewed → **500 `crd_missing_harness_capability`** with "a workspace created without an explicit harness can never bind on this cluster … pass an explicit harness or upgrade the CRD (kubectl apply -f helm/crds/workspace.yaml, or bump the Flux crds path)". The explicit-harness branch is untouched (bound-at-create degrades to the legacy path under pruning — the controller's backfill owns it; never wedges).
- **Wiring**: the `SetAgentAdapter` house pattern — `New()` leaves the seam nil (inert; unwired tests never probe — the eager-arming shape panicked strict mocks and was replaced in review-of-self), `ArmCRDCapabilityProbe()` in `services.go` arms production.

## Ruling 5 — recovery (SOFTENED: no new controller loop)

- **`local/crd-skew-rebind.sh`** — the one-shot enumeration + re-bind helper: lists workspaces with `spec.harness==""` + the `llmsafespaces.dev/harness-unbound` marker (born-unbound ONLY — legacy marker-less empties stay the controller's backfill business, and the filter is pinned so a future edit can't sweep them), reports the condition reason; `--rebind[=opencode|pi]` re-drives each bind as ONE merge patch (harness + marker removal — webhook 7a admits ""→value on the marker) and **VERIFIES EVERY BIND READS BACK NON-EMPTY** — a still-empty read-back means the CRD is still stale: exit 1 with the remediation, never a silent partial success (the same discipline as the API's verify). Confirm prompt (`--yes` to skip). 10 structure pins (`crd_skew_rebind_script_test.go`): bash syntax, the filter's discrimination, the read-back verify fragments, the marker-null patch, the harness allowlist, enumerate-only default, the CRD-FIRST runbook order.
- **`docs/operator/upgrading.md` — "CRD skew recovery"**: detection (which alert, what the gauge heals on), the operator runbook (apply CRD → run the helper → pods build on next reconcile), the USER recovery path (re-run the picker / PUT harness once the CRD landed; the degraded hint while it hasn't), and prevention (the §6 train gate).

## Ruling 6 — regression pins

- skewed + harness-less create **fails loud** (named error, remediation text, counter ticks, CRD-create NEVER reached — mock has no expectation).
- skewed + explicit-harness create **still succeeds**; unknown verdict **never gates**; supported verdict invisible. Cache TTL + invalidation semantics pinned.
- backfill degrade unchanged: the full existing `harness_binding_skew_test.go` family green on the new tree (SKEW-1..5), plus the gauge now set/healed at the verdict sites.
- post-CRD-apply convergence via re-bind: the probe pins (pruning=true → skewed; flip to false → healed, conclusive both ways) + the recovery script's read-back verify + the existing SKEW-3 convergence pin.
- drift check catches an un-bumped Flux path: synthetic-drift born-red verified against the REAL repolint binary (FAIL + exit 1, then green after sync).
- controller: gauge set/heal edges; startup probe once-per-process; re-probe cadence (window respected, heal re-affirms/skew re-affirms); BindNotPersisted parked (zero status writes, reason stands) + the helper's truth table.

## Gates

`go build ./...`; `go vet` (api/controller/pkg/local/helm); `make fmt-check`; suites green: `api/internal/services/workspace` (9.8s), `controller/...` (68s — incl. the skew + new crd_skew families), `pkg/repolint` (31s), `local` (38s), `helm`; frontend `tsc` clean, `eslint` clean (touched files), full vitest **241 files / 2773 tests green**; repolint all-checks green on the tree incl. the new `flux-crd mirror (3 CRDs byte-identical)` row.

## Deliberate non-changes

- NO new controller re-attempt loop (ruling 5's softening): the 10m backfill cadence already retries; the alert brings the human; the helper is the sweep.
- NO CRD-read RBAC anywhere: both probes ride the dry-run create (the verb both tiers already hold).
- The v1 reason constant is a bare `string` const (the condition-reason vocabulary in `workspace_types.go` is stringly by existing convention).
