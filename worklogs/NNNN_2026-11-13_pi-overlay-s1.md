# Worklog: pi overlay artifact + CI pinning (second harness S1)

**Date:** 2026-11-13
**Session:** Story tsk-torrid-squee-greets — build the digest-pinned `pi` overlay: FROM-scratch image from the pi release source archive, CI stamping (`dev.llmsafespaces/pi.sha256-{amd64,arm64}`), controller flag/validation/condition surface, agentd supervisor verify (exit 85/86), helm piDelivery, and the kind validation legs.
**Status:** Complete

---

## Objective

Deliver the delivery foundation for the second harness per design 0053 §5.4 ("a second agent is a different image at the same mount point, not a redesign") and the pi scoping report §2.3 (story S1 of the seven-story split): tri-overlay workspace pods (agentd + opencode + pi), digest-pinned with per-arch binary sha256 annotations, a distinct controller condition for pi verify failures, and the hash-the-binary + digest-the-tree trust ruling recorded.

## Work Completed

### Upstream validation (spike U-1 settled, read-only)
- Downloaded `pi-linux-x64.tar.gz` (v1.5.2, 58.9 MB) and verified its sha256 against the release `SHA256SUMS` (811c6f2c…). The artifact is a `pi/` directory TREE: the binary at the tree root, `package.json` adjacent, `native/linux/prebuilds/linux-x64/linux-platform-x11.node`, `theme/`, `assets/`, `export-html/`, `docs/`, `examples/`, `photon_rs_bg.wasm`. The binary is a dynamically-linked ELF x86-64 (~123 MB uncompressed) — glibc comes from the workspace base, like every overlay.
- Read upstream's `scripts/build-binaries.sh` + `.github/workflows/build-binaries.yml`: the official binaries are built from the SOURCE archive (`pi-1.5.2-source.tar.gz`, 14.8 MB, sha256 da14b72e… verified) with `--offline-model-data`; toolchain = node 22 + bun 1.4.4; `npm ci --ignore-scripts` needs the registry (no git deps — validated the lockfile); "offline" scopes to model data.
- bun `--compile-autoload-package-json` resolves a package.json NEXT TO the executable → the tree layout must stay upstream-shaped (binary at `/pi/pi`), NOT the sketched `/pi/bin/pi`.

### Artifact
- `runtimes/pi/Dockerfile`: node:22-bookworm-slim (digest-pinned) build stage → source archive fetch with SHA256SUMS verification → bun 1.4.4 → `build-binaries.sh --offline-model-data --platform linux-<arch>` → normalized `/tree` → `FROM scratch` COPY to `/pi/`. Header records the trust ruling (hash-the-binary + digest-the-tree, #1416/#1469 precedent) and the tree-layout ruling (package.json adjacency).

### CI
- ci.yml: `PI_IMAGE` env, `build-pi` (native per-arch matrix, mode-pin check asserting /pi traversal + /pi/pi exec bits + package.json adjacency — the 2026-09-19 incident class) and `merge-pi` (annotations + piDelivery values block). Cloned into release.yml as well (sign/scan/create-release wired — repolint's release-artifact invariant caught the create-release gap).
- The four chart-installing workflows (posture-gate, e2e-attachments, e2e-nightly, us-70) + `local/bootstrap.sh` + `local/release-preflight.sh` + `local/s5-overlay-validation.sh` now build/pin the pi overlay (mandatory third pin).

### Controller
- `pi_overlay.go` (mirror of opencode_overlay.go): wirePiOverlay (RO mount /pi, `PI_IMAGE_VOLUME` marker, `LLMSAFESPACES_PI_*` env), PiDeliveryConfig + ValidatePiDelivery (empty image fatal — no fallback mode ever existed), detectPiVerificationFailure / markPiVerified (exit 85/86 → `PiVerified` condition, `PiVerificationFailed`/`PiOverlayMissing` reasons, metric `llmsafespaces_workspace_pi_verify_failures_total`, one event+increment per episode), wired into handleActive/handleCreating.
- `pi_pins.go`: the pi instance of overlayPinSource — annotations `dev.llmsafespaces/pi.sha256-{amd64,arm64}`, ConfigMap `llmsafespaces-pi-pins`, `ErrPiPinsUnavailable`.
- main.go flags `--pi-image` + break-glass hash pair; startup validation + annotation resolution block; SetupControllers plumb-through.

### agentd supervisor
- `cmd/workspace-agentd/pi_overlay.go`: verify-only seam (no spawn — that is S2) at BOTH supervisor boot paths (managedProcess.start lazy seam + newSupervisorProcess), sync.Once; exit 85/86 with stderr + termination-log attribution; marker semantics identical to opencode's. Real-binary subprocess tests prove exit 85/86 fire before any opencode spawn and that a matching pin passes through to a serving supervisor.

### Helm + lint + docs
- values.yaml piDelivery block, controller-deployment mandatory render gate + flag render + one-sided guards, rbac.yaml pi-pins ConfigMap grants (+ informer or-condition), repolint version_scheme (struct/shape/pins/pairwise digest distinctness), `docs/operator/pi-overlay.md` (the recorded ruling), mkdocs nav.

### Tests
- Controller: pi_overlay_test.go (wiring, detection, episode idempotency, tri-overlay disambiguation 81/82/83/84/85/86, legacy-pod gating, Creating-phase), pi_pins_test.go (annotations, cache write/fallback/stale/cross-artifact rejection, sentinel, validation table incl. empty-image-fatal), overlay_mandatory + reconcilerFor fixtures updated to the tri-pinned production shape.
- agentd: decision table + code-disjointness + 3 subprocess tests.
- helm: pi_delivery_chart_test.go + pi pins in every existing render site + delivery_pins_gate third rung.
- local: posture-gate goldens/allowlist/GITHUB_ENV-exact pins (4→6 writes), release-preflight fixtures/stubs + pi mandatory + P4-pi repo-pin alignment.

## Key Decisions

1. **Binary at `/pi/pi`, not `/pi/bin/pi`** (deviation from the scoping sketch, which said "e.g."): validated that bun's package.json autoload + native prebuild paths make the executable's position load-bearing; relocating it would silently break runtime resolution. The ruling is recorded in the Dockerfile header and docs/operator/pi-overlay.md.
2. **piDelivery mandatory at BOTH render and startup from day one** (unlike opencode's S1-opt-in→S3-mandatory path): pi never shipped in runtimes/base, so an empty pin has no fallback semantics; the charter's acceptance criterion names startup rejection, and the repo doctrine ("same operator hint from both surfaces") extends it to the chart gate. Consequence: every chart-installing pipeline now builds/pins the pi artifact (~10 min cold source build, layer-cached warm) — accepted as the artifact-under-test price, timeouts bumped accordingly.
3. **Exit codes 85/86** as chartered; the pre-existing collision with the CONTROLLER process's RelayStagingNotArmedExitCode=85 is a cross-PROCESS coincidence (workspace container vs controller pod) — documented as a treaty comment in both files rather than renumbering either.
4. **Supervisor verify now, spawn later**: the verify exists in S1 because the mount does — delivered-but-unverified pins would be decorative and PiVerified vacuous. S2's harness driver will consume the verified tree.
5. **us2-kind-integration.sh left untouched**: it has been render-broken since 2026-08-30 (never pins opencodeDelivery; pre-existing, orthogonal to pi — it fails at the second gate before ever reaching the pi gate). Flagged for the owners rather than fixed in this story.

### Review rounds r2–r4 (AI reviewer + dispatched kind runs)

- **r2 (CHANGES_REQUESTED)**: release.yml pi case arms carried a stray brace (`${VERSION }}` — bad substitution at tag time; sign/scan/SBOM would fail); release build-pi lacked the mode-pin clone; docs wanted the verify-ordering note + an upgrade note. All fixed (e7a471844).
- **r3 (the dispatched kind run caught a real bug)**: the healthy pod exited 86 `PiOverlayMissing` — the tree was copied under an in-image `/pi/`, but image-volume semantics CONCATENATE mount path + in-image path (the opencode precedent), so the binary landed at `/pi/pi/pi` while the supervisor verified `/pi/pi`. Fix: the tree IS the FROM-scratch image rootfs (binary at image `/pi`, package.json at the root) → pod `/pi/pi` binary + `/pi/package.json` adjacency. All nine image-side touch points moved (CI/release sha extraction, mode-pin probes, s5/bootstrap/pipeline extracts); pod-side contracts were already correct. Re-dispatched run on the fixed head: S5.2a–e green (tri-overlay Active, `PiVerified=True`), S5.4b green with the REAL exit 85, S5.5 cold-pull 17s with three overlays evicted, S5.6 gVisor green (df941c8b2, run 43363364911).
- **r4**: S5.4c failed by construction — image-only pin resolution needs the controller pod to dial the registry named in the reference, and `localhost:5011` is node-containerd-only (the us2 constraint). First rewrite used break-glass pins, which the r5 review correctly rejected as a resolver BYPASS (break-glass returns verbatim before any cache read — the leg would pass with the resolver deleted). Final form: IMAGE-ONLY pi pins + a pre-seeded `llmsafespaces-pi-pins` (exactly what a successful resolution writes, same digest) → the resolver RUNS, its fetch fails deterministically in-pod, and the same-digest cache fallback boots the controller — the registry-outage branch of overlay_pins.go end-to-end on a real cluster, then again across a controller restart with the cache asserted intact. Also: S5.4b made STRICT (exit code exactly 85, `.state.terminated` fallback), the write-only `ci-annotated` stamp removed (dead weight that killed one dispatch), and the ci.yml/bootstrap comment nits fixed.

## Blockers

None. (Full local pi source build not runnable on the authoring sandbox — no docker and only ~800 MB free disk; the Dockerfile's RUN sequence was validated input-by-input against primary sources instead, and the PR's `build-pi` job builds both arches as the reproducibility evidence.)

## Tests Run

- `go build ./...` — clean.
- `go test ./controller/... ./cmd/workspace-agentd/ ./helm/ ./local/ ./pkg/repolint/ ./pkg/apis/... -count=1` — all ok (workspace 68s, agentd 374s incl. subprocess legs).
- `make repolint` — all checks passed. `gofmt -l`/`goimports -l` — clean. `bash -n` on every touched script. `helm template` render checks via the chart tests (helm 3.16.4 locally).
- kind legs EXECUTED via dispatched s5-overlay-validation runs on this branch: run 43363364911 caught the double-nesting bug (exit 86 on the healthy pod); run 43369364032 on the fixed head passed every leg except S5.4c's first rewrite (resolver bypass — reworked per r5); the final form re-dispatched after the r5 fixes.

## Next Steps

- S2 (pi supervisor driver): spawn `pi --mode rpc --no-session` from the verified `/pi/pi`, JSONL pipes, statusz derivation — the §1.4 process-model product ruling (process-per-session vs single+switch_session) gates it.
- S6 (drift gates): PI_VERSION bump currently has no REFRESH.md-style fixture gate (nothing RPC-shaped exists to fixture yet); the bump flow is release-preflight's P4-pi + CI source build until then.
- Owners: us2-kind-integration.sh's pre-existing opencodeDelivery render breakage (since 2026-08-30).

## Files Modified

Created: runtimes/pi/Dockerfile; controller/internal/workspace/{pi_overlay,pi_pins,pi_overlay_test,pi_pins_test}.go; cmd/workspace-agentd/{pi_overlay,pi_overlay_test}.go; helm/pi_delivery_chart_test.go; docs/operator/pi-overlay.md; worklogs/NNNN_2026-11-13_pi-overlay-s1.md.
Modified: controller/{main.go,internal/controller/controller.go,internal/metrics/metrics.go}; controller/internal/workspace/{reconciler,platform_env,pod_builder,phase_active,phase_creating,constants,controller_test,overlay_mandatory_test,runtime_reader_test}.go; cmd/workspace-agentd/{managed_process,supervise_opencode,opencode_overlay_test}.go; pkg/apis/llmsafespaces/v1/workspace_types.go; pkg/repolint/version_scheme{,_test}.go; helm/{values.yaml,templates/controller-deployment.yaml,templates/rbac.yaml} + 11 helm test files; .github/workflows/{ci,release,posture-gate,e2e-attachments-single-container,e2e-nightly,us-70-delivery-pool,s5-overlay-validation}.yml; local/{bootstrap.sh,release-preflight.sh,s5-overlay-validation.sh,posture_gate_workflow_test.go,release_preflight_script_test.go}; mkdocs.yml.
