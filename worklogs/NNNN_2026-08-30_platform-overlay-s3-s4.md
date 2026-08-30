# Worklog: platform overlay delivery S3 + S4 (base strip, mandatory pins, factory)

**Date:** 2026-08-30
**Session:** Implement design 0053 S3 (mandatory pins, direct overlay exec, base strip, entrypoint deletion) + S4 (factory: ENTRYPOINT drop, MinBaseVersion deletion, platform-train sync removal), plus the S1 remainder (standalone opencode artifact + CI stamping) that S3's mandatory opencode pin structurally requires.
**Status:** Complete (S5 validation/flip remains)

---

## Objective

Make the two digest-pinned overlay artifacts the only platform-contract delivery path: pins mandatory at render + startup + pod build, main container execs the pinned agentd directly, the base image becomes a pure dev-OS, and the factory stops stamping ENTRYPOINT/floor/platform-train coupling.

---

## Work Completed

### S1 remainder — opencode artifact (required by S3's mandatory pin)

- `runtimes/opencode/Dockerfile` (new): `FROM scratch`, opencode at `/usr/local/bin/opencode` (matches `opencodeBinaryRelPath`), pinned `ARG OPENCODE_VERSION=1.18.15` with the full validation/do-not-bump comment block moved from the base, TLS-only upstream verification note (unchanged accepted gap).
- CI (`ci.yml`): `build-opencode` matrix + `merge-opencode` jobs mirroring agentd's — per-arch binary sha256 extraction (docker create/cp) + index annotations `dev.llmsafespaces/opencode.sha256-{amd64,arm64}` + Helm values notice; `OPENCODE_IMAGE` env.
- Release (`release.yml`): build/merge pair + **all four downstream jobs** (sign-images, scan-images, generate-sbom loops gain `opencode`; create-release table row + needs) — the repolint release-artifact invariant test caught the incompleteness on first run.

### S3 — controller/agentd/helm

- Mandatory pins: `validateOverlayDeliveryPins` at buildPod (fail the workspace, values-key-named errors) + `ValidateAgentdDelivery`/`ValidateOpencodeDelivery` now fatal on empty image (startup); Helm render gate fails when either `*.image` empty (deployment template) + gate test (`delivery_pins_gate_test.go`); chart test helper injects synthetic pins; `make helm-render` passes synthetic digests (render stays a syntax check).
- Main container: `Command: [/agentd/usr/local/bin/workspace-agentd]`, `Args: [--supervise]` (sidecar mode re-points to `supervise-opencode` as before); entrypoint reference deleted.
- Platform inits unconditional; bash `workspace-dirs`/`credential-setup` inits + `buildCredentialSetupInit`/`buildWorkspaceDirsInit` + overlay `enabled()` gates deleted; `detectPlatformBootFailure` now name-filters init containers (real gap: post-gate-removal, user-plane `workspace-setup` crashes were misclassified as platform failures — surfaced-not-recovered — instead of #935 crashloop recovery).
- Base ENV relocation: `platform_env.go` — mise/cargo/gem/go/npm/python homes, git-credential env layer, PATH composition on main container + workspace-setup init. OPENCODE_* env names stay behind the agent seam: `opencodeChildEnv` in `opencodeServeCmd` sets OPENCODE_CONFIG (mode-aware via `LLMSAFESPACES_AGENT_CONFIG_PATH`), XDG_DATA_HOME, OPENCODE_EXPERIMENTAL_EVENT_SYSTEM, OPENCODE_SERVER_PASSWORD (single-container from boot password; sidecar env wins) — 4 tests.
- agentd `--supervise` self-verify: `runSupervisorSelfVerify` wired into main() before any work (exit 81, termination log) — exec-level TDD test `TestSupervise_SelfVerifyMismatch_Exit81`.
- rebase: US-69.2 platformFlag/`buildPlatformDirsInit` integrated into the unconditional platform-init block; `mainContainerName`/`containerIndexByName` helpers restored after the cred-init deletion swept them.

### S3 — runtimes

- `runtimes/base/Dockerfile`: single stage — agentd/redact builders, opencode install, entrypoint/redact/agentd COPYs, ENV blocks (runtime + GIT_CONFIG), and ENTRYPOINT all deleted. Keeps apt/mise/gh/aws, /etc/gitconfig, useradd, USER, WORKDIR. 383→~300 lines.
- Entrypoint files deleted (`entrypoint-opencode.sh`, `entrypoint-common.sh`, their tests); smoke-test trimmed to OS-content asserts (redact/agentd/opencode lines removed — design §4.3 split).
- repolint `TestEntrypointExportsEventSystemFlag` → `TestSupervisorExportsEventSystemFlag` (guardian repointed at `opencodeChildEnv`).

### S4 — factory

- `RenderDockerfile`: no ENTRYPOINT; `MinBaseVersion` + floor deleted (with `base_floor_test.go`).
- Platform-train base sync deleted (`base_sync.go` + tests + app.go wiring): the base is content-versioned on its own cadence (D5); catalog rows advance by operator-reviewed seed updates.

### Deployment wiring

- e2e-nightly: opencode artifact build/push/pin (local registry) + helm sets.
- e2e-attachments-single-container: local registry added + BOTH artifacts + pins (was legacy baked mode — the only remaining consumer).
- `local/bootstrap.sh`: local dev registry + both artifacts + pins; `die` for failures.
- `opencode-version-bump.yml`: reads/seds the ARG from `runtimes/opencode/Dockerfile`.

---

## Key Decisions

- **OPENCODE_* env in the supervisor, not the controller** — #942 containment: the controller must not know opencode env names; `opencodeChildEnv` mirrors entrypoint exports with mode-aware resolution. `TestPodBuilder_ContainerEnv_NoOpencodeEventFlag` kept (inverted into the S3 guardian).
- **Render gate + mandatory startup land WITH the strip in one PR** — design D4 (no dual delivery regime); every baked-mode consumer (bootstrap, both e2e workflows) switched in the same change.
- **S1 remainder folded in** — the mandatory opencode pin requires the artifact to exist; shipping the gate without the artifact would make every deploy unconfigurable.
- **Platform-boot failure detection name-filters init containers** — user-plane inits (workspace-setup) crashloop-recover; platform inits surface. Preserves #935 semantics post-legacy-deletion.
- **`make helm-render` uses synthetic digests** — render sanity ≠ pin check; chart tests own the gate.

## Assumptions validated

- "Nothing consumes the legacy branches post-strip" — all `agentdOverlayEnabled` callers traced; legacy tests deleted (28 in the controller slice) or rewritten against the platform path.
- "release downstream jobs enumerate artifacts" — enforced by `TestReleaseArtifactCompleteness_ActualRepoWorkflow`, which failed until sign/scan/SBOM/release were complete.
- "kind digest resolution needs a real registry" — attachments/bootstrap follow the nightly's registry + certs.d alias pattern (RepoDigests require a push).

---

## Blockers

None. (helm binary absent locally — chart tests deferred to CI; YAML validated structurally.)

---

## Tests Run

- `go test ./controller/...` — all packages ok (incl. 8 new S3 tests, rewritten consistency/security/wedge suites).
- `go test ./cmd/workspace-agentd/` — ok (supervise self-verify exec test + opencodeChildEnv suite).
- `go test ./api/internal/imagefactory/ ./api/internal/app/ ./pkg/repolint/` — ok (repolint release-artifact invariants green with opencode artifact).
- `go build ./...` — clean; `bash -n` on bootstrap; YAML parse on all five touched workflows.

---

## Next Steps

- **S5**: kind suite extension — overlay verify failure → condition, missing-pin render/startup legs, resume-path pull cost with the opencode volume, gVisor (`runsc`) image-volume leg (design 0051 open item), full launch→ready on stripped base + overlays; flip when green. Then merge #1152 (S2) FIRST — this branch deletes the pre-S3 `/usr/local/bin/redact` wrapper path S2 adds; rebase expected to compose (S2's supervisor wrapper is the surviving UX).
- **Base tag scheme (D5 open decision)**: base is still tagged with the platform VERSION by release.yml (harmless post-strip — nothing baked couples anymore); decide content-version scheme (`bookworm-2026.08` vs semver) and reseed `catalog.seed.yaml` deliberately.
- Stale docs sweep: `docs/operator/runtime-environments.md` entrypoint section, `docs/operator/agentd-delivery.md` entrypoint-verify wording, `scripts/us2-kind-integration.sh` (US-2 era script referencing deleted entrypoints).
- Epic 69 (0055) flip order unblocked once this lands: capability → S1 shadow → (S3 done) → V2 flag → authority flag.

---

## Files Modified

Controller: `pod_builder.go`, `platform_env.go` (new), `agentd_overlay.go`, `opencode_overlay.go`, `boot_failure.go`, `controller_test.go`, + 12 test files updated/deleted (cred-script/entrypoint/legacy-mode suites removed; consistency/security/wedge rewritten).
agentd: `main.go`, `managed_process.go`, `supervise_selfverify_test.go`, `opencode_child_env_test.go` (new), `path_consistency_test.go`.
Runtimes: `base/Dockerfile`, `tools/smoke-test.sh`, `tools/entrypoints/*` (deleted), `opencode/Dockerfile` (new).
Factory: `imagefactory/dockerfile.go`, `base_sync.go` (deleted), `base_floor_test.go` (deleted), `dockerfile_test.go`, `app.go`.
Helm: `templates/controller-deployment.yaml`, `chart_test.go`, `delivery_pins_gate_test.go` (new).
CI/release: `ci.yml`, `release.yml`, `e2e-nightly.yml`, `e2e-attachments-single-container.yml`, `opencode-version-bump.yml`, `Makefile`, `local/bootstrap.sh`, `pkg/repolint/entrypoint_event_flag_test.go` → `event_flag_relocation_test.go`.

---

## Addendum (same session): S4 remainder — content-versioned base, off the release train

**Decision (owner, live):** CalVer `YYYY.MM.x` — month = when the OS content last changed; `x` = patch bump for further changes within the same month. Chosen over bare month-CalVer: in-month collisions (two content changes) resolve without waiting for calendar rollover; monotonic under the factory's numeric `CompareVersions`.

**Implementation:**
- **Single source of version truth: the catalog seed row** (`catalog.seed.yaml` bookworm → `2026.08.0`). A base change ships as ONE reviewed PR: `runtimes/base/**` diff + row bump. No registry tag-arithmetic anywhere — the same pattern as the opencode pin (diffable, reviewable, revertible).
- **`base-image.yml` (new)**: triggered by pushes touching `runtimes/base/**` or the seed; reads the row, CalVer shape-guards it, builds both arches, merges the manifest, cosign-signs + SBOM-attests. Idempotent: an existing tag is never rebuilt — a rebuild requires a patch bump (surfaced as a notice).
- **release.yml de-coupled**: `build-runtime`/`merge-runtime` deleted; base removed from sign/scan/SBOM loops, case arms, env. RELEASE SUCCESS == platform artifacts only; base success == its own workflow. repolint's release-artifact invariants stay green (discovery is env-driven); the now-dead `RUNTIME_IMAGE` mapping removed from `componentFor`.
- **Seed semantics on existing fleets**: `SeedUpsertBase` inserts the new row NON-default (never moves an operator default, #936) — promotion is an admin action; fresh installs default to `2026.08.0`.
- ci.yml's PR-validation base build drops the now-unused agentd build-args.

**Tests:** imagefactory + database + repolint suites green against the reseeded catalog (seed tests parse the real embedded YAML, so the new row shape is exercised); the awk extraction in `base-image.yml` verified against the seed locally (2026.08.0).
