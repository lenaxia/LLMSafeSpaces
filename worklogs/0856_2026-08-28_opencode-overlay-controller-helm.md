# Worklog: opencode overlay delivery S1, controller + helm half (design 0053 §4.2)

**Date:** 2026-08-28
**Session:** Controller/helm implementation of design 0053 S1 "opencodeDelivery" — the receiving half of the opencode overlay seam (the supervisor half is the sibling worklog `NNNN_2026-08-28_opencode-overlay-supervisor-verify.md`); plus the same-day validator-remediation deltas that landed in this working tree.
**Status:** Complete

---

## Objective

Deliver the controller and helm halves of design 0053 §4.2: ship the opencode binary to workspace pods as a digest-pinned read-only image volume (independent pin from agentdDelivery), resolve its per-arch binary sha256 pins from CI-stamped index annotations at controller startup (single Renovate-updatable coordinate), detect supervisor verify failures (exit 83/84) as a Workspace condition + one event + one metric increment per failure episode with a critical alert, and gate the whole thing helm-side with the same all-or-nothing guards as agentdDelivery. Opt-in and inert by default (S3 strips the baked binary later).

---

## Work Completed

### Generalized overlay pin resolver (second consumer pays for the abstraction)

- **`controller/internal/workspace/overlay_pins.go`** (new): the #863 pin-resolution pattern parameterized into `overlayPinSource` + `cachedPinResolver` — per-artifact annotation keys, ConfigMap cache name, and error sentinel. Resolution: image-only form fetches the pinned index's annotations; on success rewrites the cache; on registry failure falls back to the cache ONLY for the same digest (digest = content identity, re-tags don't invalidate); missing/malformed annotations fail closed. Runs before the manager starts so a broken pin fails fast.
- **`controller/internal/workspace/agentd_pins.go`** (refactor): the agentd instance of the resolver, behavior unchanged.
- **`controller/internal/workspace/opencode_pins.go`** (new): the opencode instance — annotations `dev.llmsafespaces/opencode.sha256-{amd64,arm64}`, cache ConfigMap `llmsafespaces-opencode-pins`, sentinel `ErrOpencodePinsUnavailable`. The two instances share behavior but never state: caches and annotation namespaces are per-artifact and can never cross-satisfy.

### Pod wiring + failure detection

- **`controller/internal/workspace/opencode_overlay.go`** (new): `wireOpencodeOverlay` (digest-pinned image volume, PullIfNotPresent; readOnly mount at `/opencode` on the workspace container ONLY; env pins `OPENCODE_IMAGE_VOLUME=1` marker, `LLMSAFESPACES_OPENCODE_BINARY` absolute path, per-arch sha256s); `podHasOpencodeOverlay` gate keyed on the pod's env marker (not just the controller flag — rollout-window safety from the #863 live-cluster finding); `detectOpencodeVerificationFailure` (exit 83 → `ReasonOpencodeVerificationFailed`, 84 → `ReasonOpencodeOverlayMissing`; one event + metric increment per episode; caller skips crashloop recovery — a digest mismatch cannot be fixed by restarting); `markOpencodeVerified` (True condition once a marked pod runs clean); `ValidateOpencodeDelivery` startup guard (all-or-nothing, digest-pinned image, both-or-neither hash overrides).
- **`controller/internal/workspace/agentd_overlay.go`**: `latestTerminatedState` single-attribution semantics documented (timeless comment, remediation round).

### Controller startup

- **`controller/main.go`**: `--opencode-image`, `--opencode-binary-sha256-amd64/-arm64` flags; `ValidateOpencodeDelivery` + image-only `ResolveOpencodePinsWithCache` (30s bound, `ErrOpencodePinsUnavailable` hint to the manual-pin break-glass, exit 1 before the manager starts); `controller.OpencodeDelivery` config threaded into the reconciler setup.

### API surface, metrics, alerting

- **`pkg/apis/llmsafespaces/v1/workspace_types.go`**: `WorkspaceConditionOpencodeVerified` + `ReasonOpencodeVerificationFailed` / `ReasonOpencodeOverlayMissing` / `ReasonOpencodeVerified`. No CRD regeneration needed (see assumptions).
- **`controller/internal/metrics/metrics.go`**: `WorkspaceOpencodeVerifyFailuresTotal` (outcome / node / digest-version labels — low-cardinality 12-hex version identity).
- **`helm/templates/prometheus-rules.yaml`**: `LLMSafeSpacesOpencodeVerificationFailed` page-on-any alert, severity critical (mirrors the agentd alert — should-never-fire signal).

### Helm chart

- **`helm/values.yaml`**: `controller.opencodeDelivery` (image, binarySHA256Amd64/Arm64) with the Renovate-form documentation and the S1-follow-up caveat for annotation-less indexes (remediation round).
- **`helm/templates/controller-deployment.yaml`**: flag rendering for the three forms (inert / image-only / full manual pin).
- **`helm/templates/rbac.yaml`**: least-privilege pins grant — scoped `get`/`update` on `configmaps` with `resourceNames: [llmsafespaces-opencode-pins]` plus the separate unscoped `create` (K8s cannot resourceNames-scope creation).
- Guards: one-sided hash pair and hashes-without-image FAIL the render; `opencodeDelivery` does not gate `agentdSidecar` (independent artifacts, independent cadences).

### Tests — 44 for the implementation

18 (`opencode_overlay_test.go`: wiring, mount topology, env pins, detect/idempotency/Creating-phase, dual-overlay disambiguation ×3, legacy gating ×2, validation) + 12 (`opencode_pins_test.go`: annotation extraction, missing-annotations fail-closed, cache write/fallback/stale/malformed, sentinel, flag short-circuit, config errors) + 4 (`overlay_pins_test.go`: per-source ConfigMaps/annotation keys/errors, caches never cross-satisfy) + 10 (`opencode_delivery_chart_test.go`: default/configured/image-only renders, one-sided ×2 + hashes-without-image render failures, RBAC grant present/absent, sidecar independence, alert renders).

### Validator remediation (same working tree, same day)

- `opencode_overlay_contract_test.go` (new): mechanical controller↔agentd seam-contract guard (env names, path composition, 83/84, message prefixes, marker gate constant usages) — parses `cmd/workspace-agentd/opencode_overlay.go` const declarations off disk, `TestAgentdAnnotationKeys_MatchCIWorkflow` precedent; mutation-verified (renumbering the agentd exit code fails it).
- `TestOpencodeVerify_DeliveryDisabledOnReconciler_PodExit83Ignored` (new): rollout-down leg of the gating matrix; passes immediately by design (gate = reconciler config AND pod marker — documented in the test) and was mutation-verified to fail when the gate drops the reconciler-config half.
- Constant-usage fixes (`opencodeOverlayEnvKey` / `agentdOverlayEnvKey` instead of literals in the two wire functions), `latestTerminatedState` attribution comment, values.yaml follow-up caveat, agentd test-package hardening (shared-binary temp-dir cleanup via the existing `TestMain`; `filteredEnviron` in the selfverify subprocess test), and two pre-existing envtest test repairs (`TestEnvtestPlatformInit_*`).

---

## Key Decisions

1. **Generalize the pin resolver now, not at the third artifact.** The second consumer (opencode) is what pays for the abstraction per README Rule 12, and both instances are concrete — parameterized by data (`overlayPinSource`), not by callbacks. Per-artifact caches/annotation keys so one artifact's outage fallback can never satisfy the other's verification.
2. **Independent pin from agentdDelivery.** opencode moves on the upstream-validation cadence, agentd on the platform release cadence; bundling couples every agentd rollback to an opencode rollforward (design 0053 §5). Exit codes stay disjoint (83/84 vs 81/82) so dual-overlay failure attribution never crosses artifacts.
3. **Image-volume mount on the workspace container only.** The supervisor that spawns and verifies opencode is PID 1 of the workspace container; the agentd sidecar and init containers never exec it, so they mount nothing.
4. **Detection gated on pod marker AND reconciler config.** The rollout-window lesson from #863: flag-only gating stamps false-positive verified conditions on pre-enable pods; marker-only gating would misattribute exit codes from pods the controller no longer owns the contract for (pinned by the rollout-down test, mutation-proven).
5. **Fail closed on unresolvable pins.** A controller that cannot resolve or override the per-arch hashes would stamp unverifiable pins into pods — worse than not booting. Startup exits 1 with the break-glass hint.

## Assumptions → validation

1. **No CRD regeneration needed for the new condition/reasons** → **verified**: `helm/crds/workspace.yaml:209-228` defines `status.conditions` as a generic array of `{type, status, lastTransitionTime, reason, message}` string objects — any new condition type fits the existing schema; the envtest `TestEnvtestPlatformInit_BootFailureConditionPersists` round-trips a condition through the real status subresource.
2. **Workspace-container-only mount (supervisor is PID 1 of the workspace container)** → **verified**: the supervisor (supervise-opencode sidecar mode / `--supervise` single-container mode) is the process that spawns opencode; neither the agentd sidecar nor any init container execs it → **locked by** `TestOpencodeOverlay_WorkspaceContainerOnly` (no non-workspace container or init container mounts the volume).
3. **One terminated state = one exit code = one artifact attribution per episode** → **verified**: `latestTerminatedState` returns a single terminated state and each `detect*` maps exactly one exit-code band to one artifact; dual simultaneous failure is one root cause and the second artifact surfaces on the next episode → **documented** at `agentd_overlay.go` (`latestTerminatedState`) as deliberate semantics.
4. **Dual-overlay exit-code disjointness (81/82 vs 83/84) keeps attribution clean** → **locked by tests**: `TestOpencodeVerify_AgentdExit81_DoesNotSetOpencodeCondition`, `TestOpencodeVerify_OpencodeExit83_DoesNotSetAgentdCondition`, `TestOpencodeVerify_AgentdOverlayMissing82_DoesNotSetOpencodeCondition` (condition absent AND metric untouched in both directions).
5. **`OPENCODE_V2_DELIVERY` (api.extraEnv) is unrelated** → **verified**: it is the API-side design 0052 inboard-delivery flag — a coincidental name on a different subsystem; recorded as the NAMING NOTE in `values.yaml` next to `opencodeDelivery`.

---

## Blockers

None.

---

## Tests Run

- `go test -count=1 -timeout 600s ./controller/... ./helm/... ./pkg/apis/...` — **ok** (all packages).
- `go test -count=1 -timeout 600s ./cmd/workspace-agentd/...` — **ok** (150s, incl. the seam-contract consumer side).
- `go vet ./...` and `go vet -tags envtest ./controller/...` — **clean**.
- `gofmt -l controller cmd helm pkg` — **empty**.
- `go test -count=1 -race` on the new suites (`-run 'TestOpencode|TestOverlayPin|TestResolveOpencode|TestValidateOpencode' ./controller/internal/workspace/`; `./helm/...`) — **ok**.
- `KUBEBUILDER_ASSETS=… go test -count=1 -timeout 300s -tags envtest ./controller/internal/workspace/ -run 'TestEnvtest'` — **ok** (agentd-pins cluster path + the repaired platform-init tests); also `-tags envtest` on `./pkg/apis/llmsafespaces/v1/` and `./controller/internal/webhooks/` — **ok**.
- Mutation proofs (this session): renumbering the agentd-side exit code → contract test FAILS (reverted); dropping the reconciler-config half of the detection gate → rollout-down test FAILS (reverted).

---

## Next Steps

1. Land the #1116 S1 follow-up: standalone opencode artifact build + CI annotation stamping (`dev.llmsafespaces/opencode.sha256-*` on the index — mirror the merge-agentd job), then extend the CI↔Go contract test to the opencode workflow like `TestAgentdAnnotationKeys_MatchCIWorkflow`.
2. Add `TestEnvtestPlatformInit_*` (or the whole platform-init tag) to `.github/workflows/envtest.yml` — the repaired tests were broken on HEAD precisely because nothing in CI executed them.
3. S2 per design 0053 (runtime base prep) and S3 base strip (pin becomes mandatory); the values.yaml default flips only at S3.

---

## Files Modified

- `controller/internal/workspace/overlay_pins.go` (new)
- `controller/internal/workspace/overlay_pins_test.go` (new)
- `controller/internal/workspace/opencode_pins.go` (new)
- `controller/internal/workspace/opencode_pins_test.go` (new)
- `controller/internal/workspace/opencode_overlay.go` (new)
- `controller/internal/workspace/opencode_overlay_test.go` (new; + rollout-down test in remediation)
- `controller/internal/workspace/opencode_overlay_contract_test.go` (new, remediation)
- `controller/internal/workspace/agentd_pins.go` (refactor onto the generalized resolver)
- `controller/internal/workspace/agentd_pins_test.go`, `agentd_pins_integration_test.go`, `agentd_pins_envtest_test.go` (adapted)
- `controller/internal/workspace/agentd_overlay.go` (attribution comment + constant usage, remediation)
- `controller/internal/workspace/reconciler.go`, `phase_active.go`, `phase_creating.go`, `pod_builder.go`, `constants.go` (reconciler fields + detection hooks)
- `controller/internal/controller/controller.go` (OpencodeDelivery config threading)
- `controller/internal/metrics/metrics.go` (opencode verify-failures counter)
- `controller/main.go` (flags + startup resolution)
- `pkg/apis/llmsafespaces/v1/workspace_types.go` (condition + reasons)
- `helm/values.yaml`, `helm/templates/controller-deployment.yaml`, `helm/templates/prometheus-rules.yaml`, `helm/templates/rbac.yaml`
- `helm/opencode_delivery_chart_test.go` (new)
- `controller/internal/workspace/platform_init_envtest_test.go` (envtest repairs, remediation)
- `cmd/workspace-agentd/secrets_test.go`, `cmd/workspace-agentd/e2e_test.go`, `cmd/workspace-agentd/supervise_selfverify_test.go` (test hardening, remediation — supervisor-half files touched only for the remediation findings)
