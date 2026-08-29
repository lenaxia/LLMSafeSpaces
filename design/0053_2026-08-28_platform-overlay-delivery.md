# 0053 — Platform overlay delivery: the base is the OS

**Status:** Approved (design — no implementation yet)
**Date:** 2026-08-28
**Issue:** #1116
**Related:** #863 + `docs/operator/agentd-delivery.md` (image-volume mechanism), `design/0051_2026-08-18_agentd-uid-separation.md` (sidecar architecture, uid tiers), `design/0046_2026-08-01_image-factory.md` (factory; ruling #29), worklog 0657 (opencode 1.15.12→1.18.10 validation), incident 2026-08-25 (#871 baked-agentd contract drift)
**Supersedes:** the baked platform-artifact layout of `runtimes/base/Dockerfile`, the `MinBaseVersion` compatibility floor (`api/internal/imagefactory/dockerfile.go`), and the optional/legacy mode of `agentdDelivery`.

---

## 1. Problem statement

The workspace base image is versioned on the platform release train, but almost nothing the platform actually versions about it is OS content. Four **platform-contract artifacts** — things whose behavior must stay in sync with the API and controller — are baked into `runtimes/base`:

1. `workspace-agentd` — built from this repo; holds the secrets/MCP contract with the API.
2. `redact` — built from this repo; reads API-staged `/sandbox-cfg/redact-patterns.json`.
3. The entrypoint scripts (`entrypoint-opencode.sh`, `entrypoint-common.sh`) — encode mount topology and the process contract.
4. `opencode` — upstream-versioned, but platform-**validated** (proxy routes, session shape, config-merge semantics; worklog 0657) and platform-**pinned** (`runtimes/base/Dockerfile:57`).

Because release CI tags the base with the platform `VERSION` (`release.yml` prints `ghcr.io/lenaxia/llmsafespaces/base:${VERSION}`), the image-factory base catalog rows (`bookworm@0.8.0`, `0.15.7`) carry a hidden platform-version axis. Consequences:

- **Every platform release needs a base rebuild** before agentd/redact/entrypoint fixes reach new pods — even though #863 already solved this for agentd when overlay delivery is enabled.
- **The `MinBaseVersion` floor exists** (`dockerfile.go:25`) only because a baked agentd can be contract-stale. The 2026-08-25 incident — factory staged onto operator-pinned `bookworm@0.8.0`, five weeks stale, whose baked agentd crash-looped on contract-shape MCP metadata — is this coupling failing in production.
- **opencode upgrades force base rebuilds + catalog churn**, and the validated opencode version is implicit in whichever platform tag built the base rather than an auditable coordinate.
- **Users must reason about platform versions** when the only axis they should ever care about is the OS: *bookworm*.

## 2. Goals / non-goals

**Goals:**

- The workspace base becomes a pure **dev-OS image**: Debian suite + apt set + mise runtimes + `gh`/`aws` CLIs. Its version axis is its own content, on its own cadence.
- All platform-contract artifacts are delivered at pod time via digest-pinned read-only image volumes — the #863 pattern, extended — so any pod (re)creation pulls the currently pinned platform artifacts. "The image is the workspace" is preserved; the *platform runtime* is not the image.
- opencode becomes an independently pinned, independently rolled-back coordinate.
- The factory's base rows mean OS content; `MinBaseVersion` dissolves.

**Non-goals:**

- **No generic overlay framework.** Two concrete pins, explicitly wired. If a third artifact ever needs overlay delivery, generalize then (README-LLM Rule 12 — recurrence, not speculation, is the abstraction trigger).
- **No legacy baked mode.** There are no customers. Both pins are mandatory; baked-fallback code paths are deleted, not maintained.
- **Not moving** apt packages, mise, baked runtimes, or `gh`/`aws` into overlays — that would re-couple "what tools do I have" to the platform cadence, the exact opposite of this design.

## 3. Verified current state

| Fact | Evidence |
|---|---|
| Base bakes agentd + redact + entrypoints + smoke-test | `runtimes/base/Dockerfile:258-264` |
| Base installs opencode 1.18.10, pinned + validated | `runtimes/base/Dockerfile:47-159`; worklog 0657 |
| Base is tagged with the platform VERSION | `.github/workflows/release.yml:1324` (`base:${VERSION}`) |
| agentd overlay delivery exists: digest-pinned image volume `/agentd`, per-arch sha256 as OCI index annotations, controller-resolved at startup, cached in `llmsafespaces-agentd-pins` ConfigMap, verified before exec | `docs/operator/agentd-delivery.md`; `helm/values.yaml` (`controller.agentdDelivery`); `controller/internal/controller/controller.go:61-63` |
| Sidecar mode runs platform boot (init-fs/bootstrap/materialize) from the agentd artifact and bypasses the baked entrypoint; #863 verify moved into the supervisor | `helm/values.yaml` (`agentdSidecar`, migration-state note); design 0051 |
| The agentd artifact's trust contract is "one file, one sha256" — `FROM scratch`, ~25MB, "do not add anything executable" | `cmd/workspace-agentd/Dockerfile` |
| agentd already dispatches subcommands: `init-fs`, `supervise-opencode`, `--sidecar`, `materialize`, `bootstrap` | `cmd/workspace-agentd/main.go:82-112` |
| `redact` is a ~40-line stdin→stdout filter consumed only by platform-owned paths: the entrypoint (pipes opencode stdout/stderr in high-security mode) and PATH-shadowing wrappers | `cmd/redact/main.go`; `docs/reference/cli.md:148-167`; `docs/operator/runtime-environments.md:82` |
| The factory floor blocks builds onto bases older than `0.15.7` (the #871 contract) | `api/internal/imagefactory/dockerfile.go:25,41-44` |
| The factory renderer stamps `ENTRYPOINT ["/usr/local/bin/entrypoint-opencode.sh"]` into every built image | `api/internal/imagefactory/dockerfile.go:59` |
| The base ENV block (`MISE_DATA_DIR`, `CARGO_HOME`, `GOPATH`, `PATH`, …) encodes PVC mount topology — platform contract | `runtimes/base/Dockerfile:315-321` |

## 4. Design

### 4.1 Artifact 1 — `agentdDelivery.image` (existing, role extended)

The existing #863 artifact and its contract are **unchanged**: `FROM scratch`, one binary, one per-arch sha256. It already carries the supervisor and the platform-boot phases; after this design it also carries `redact` — **as a subcommand**, not a second file:

- `workspace-agentd redact` — `cmd/redact/main.go` folds into the agentd binary using the existing subcommand dispatch (`main.go:82-112`). The standalone `cmd/redact` is deleted.
- The supervisor writes a wrapper at `/sandbox-runtime/bin/redact` (RW tmpfs, uid-1000 space) — `exec /agentd/usr/local/bin/workspace-agentd redact "$@"` — and includes `/sandbox-runtime/bin` in opencode's PATH. This preserves the documented UX (`some-command | redact`, `docs/reference/cli.md`) for the PATH-shadowing wrappers with zero bytes of a second executable in the trusted artifact.
- The entrypoint scripts are **deleted**, not migrated. Sidecar mode already bypasses them; their remaining responsibilities — env assembly, optional redact piping of opencode stdout/stderr, exec — belong to `supervise-opencode`, which is the PID-1 contract from design 0051.

### 4.2 Artifact 2 — `opencodeDelivery.image` (new)

Same construction as the agentd artifact, one binary, one hash:

- `FROM scratch`; the opencode binary at a fixed path (`/usr/local/bin/opencode`); per-arch binary sha256 stamped onto the image index as OCI annotations by the same CI job that stamps agentd's.
- Helm: `controller.opencodeDelivery.image` (+ optional `binarySHA256Amd64`/`Arm64` break-glass overrides, set-both-or-neither, mirroring `agentdDelivery`). **Mandatory**: the render fails when empty — as `agentdDelivery` becomes after this design.
- Controller: resolves the annotations at startup, caches them for registry outages (`llmsafespaces-opencode-pins` ConfigMap, same RBAC pattern as the agentd pins); mounts the image read-only at `/opencode` on the workspace container (and sidecar where spawn requires it); stamps the resolved binary path + per-arch pin into pod env.
- Supervisor: verifies the per-arch sha256 before spawn — the same trust chain the #863 verify already uses — and execs opencode from the mount. A verify failure is a pod-level condition with the same exit-code/alert surface as the agentd verify.

### 4.3 The base after the strip

`runtimes/base/Dockerfile` loses: the redact/agentd builders and COPYs, the entrypoint COPYs, the smoke-test COPY, the opencode install RUN, the ENV block, and `ENTRYPOINT`.

It keeps: the apt set, `mise` + the baked runtimes, `gh`/`aws` CLIs, `useradd -u 1000 sandbox`, `WORKDIR /workspace`, `USER sandbox`.

- **ENV → controller pod env.** The ENV block encodes PVC mount topology (`MISE_DATA_DIR=/workspace/...`), which is platform contract, not OS content. The controller injects it as pod env; PATH composition lands there too.
- **Smoke test splits.** OS-content assertions (tools present, runtimes launch) remain a CI test on the base image. Platform-contract assertions (agentd spawns, opencode serves, redact pipes) move to the overlay images' own CI — each artifact validates itself, which is the point of the split.
- **Versioning.** The base is content-versioned on its own cadence (Debian suite + tool pins). The exact tag scheme (`bookworm-2026.08` vs. semver) is a story-level decision; what is decided here is that it is **not** the platform `VERSION` and moves only when OS content changes.

### 4.4 Factory changes

- `RenderDockerfile` drops the `ENTRYPOINT` line — the entrypoint no longer exists, and the pod spec's overlay supervisor command owns the process. `USER sandbox` / `WORKDIR` stay (harmless, and `USER` documents the uid contract).
- `MinBaseVersion` and its floor check are **deleted**. Nothing platform-side remains baked, so there is no contract to be stale. The incident class the floor guarded against becomes structurally impossible for overlay-mode pods — which is now the only mode.
- The base catalog is reseeded with content-versioned rows. The hash preimage (sorted selection IDs + base name) is unchanged; ruling #29 is untouched.

### 4.5 Mandatory pins, no fallback

`controller.agentdDelivery.image` and `controller.opencodeDelivery.image` are both required. The Helm render fails when either is empty; the controller fails startup on an unparsable pin. The legacy baked-binary branches in the entrypoint/supervisor are deleted rather than kept behind a flag.

## 5. Why two pins, not one bundle

1. **Independent rollback.** An agentd regression is reverted by one digest bump; the validated opencode version is untouched. A bundle couples them — every agentd rollback also rolls opencode, possibly past the version the platform validated.
2. **Independent cadence.** agentd ships with every platform release; opencode moves only after an upstream release *and* a worklog-0657-style validation pass. Bundling either republishes unchanged opencode bytes every release, or hides the validated opencode version inside whichever platform tag built the bundle.
3. **Auditable validation coordinate.** "What opencode is running, and is it the validated one?" is one diffable values.yaml line — the exact question the 1.15.12→1.18.10 upgrade had to reconstruct from image build history.
4. **Epic 65 alignment.** The agent is the external dependency the platform is decoupling from (`pkg/agent/opencode/`). A dedicated delivery coordinate makes the containment seam physical: a second agent is a different image at the same mount point, not a redesign.

## 6. Decisions summary

| # | Decision | Rationale |
|---|---|---|
| 1 | Two digest-pinned overlay artifacts (agentd; opencode), not one bundle | §5 — rollback, cadence, auditability, Epic 65 |
| 2 | `redact` absorbed as an `agentd` subcommand; supervisor provides the PATH wrapper | Preserves one-file-one-hash; all consumers are platform-owned paths |
| 3 | Entrypoints deleted; `supervise-opencode` absorbs the glue | Sidecar mode already bypasses them (design 0051); no dead code |
| 4 | No legacy baked mode; both pins mandatory at render + controller startup | No customers; clean break beats a flag we would delete anyway |
| 5 | Base = dev-OS only, content-versioned, own cadence | The user-facing version axis becomes the OS (bookworm), nothing else |
| 6 | Base ENV block → controller-injected pod env | It encodes PVC mount topology — platform contract, not OS content |
| 7 | `useradd`, `WORKDIR`, `USER sandbox` stay in base | uid-1000 contract (design 0051); not release-coupled |
| 8 | Factory: drop `ENTRYPOINT`, delete `MinBaseVersion`, reseed catalog | Floor existed only for baked-agentd drift; preimage unchanged |
| 9 | Smoke tests split per artifact | Each artifact validates its own contract |
| 10 | No generic overlay framework | Rule 12; two concrete pins; generalize on a third consumer |

## 7. Rollout (stories)

- **S1 — opencode artifact.** Standalone `opencode` image (`FROM scratch`, fixed path), CI per-arch stamping, `opencodeDelivery` Helm values + controller wiring, supervisor spawn + verify. Inert until the base is stripped.
- **S2 — redact subcommand.** Fold `cmd/redact` into agentd; supervisor PATH wrapper; delete `cmd/redact`.
- **S3 — base strip + pod env.** Remove platform artifacts + ENV from `runtimes/base`; controller injects env; pins become mandatory; delete baked-fallback branches; entrypoints deleted with glue absorbed.
- **S4 — factory.** Renderer `ENTRYPOINT` removal, `MinBaseVersion` deletion, catalog reseed with content-versioned base rows.
- **S5 — validation + flip.** kind suite extension: overlay verify failure surfaces as condition, missing-pin render/startup failures, resume-path pull cost with the opencode volume mounted, gVisor (`rununc`/`runsc`) image-volume behavior (design 0051's open item), full launch→ready e2e on stripped base + overlays. Flip defaults when green.

Story ordering allows S1/S2 in parallel with S3 prep; S4 must land with S3 (factory must not stamp `ENTRYPOINT` onto a base that lacks it).

## 8. Risks / open questions

- **Resume-path pull cost.** The ~22s suspend→active time is dominated by PVC re-attach + opencode boot. agentd's 25MB volume was sized for cheap resume pulls; opencode is ~10×. Digest-pinned + node layer cache should absorb it, but S5 must measure resume before/after on a cold node.
- **gVisor + image volumes.** Design 0051 flags nested RO+RW subPath mount behavior under `runsc` as the big unvalidated item for sidecar mode; image volumes under gVisor inherit that gap. S5 must include a `runsc` leg before default-on.
- **Two pins in the release process.** Renovate needs per-artifact `helm-values` rules (automerge like agentd's). The release runbook gains the opencode artifact row.
- **mise-baked runtime pins** (python/node/rust/go/java) previously moved implicitly with the platform train; after the strip they move on the base's own cadence. Validation ownership transfers to the base image's CI — must be explicit in S3, or tool bumps ship untested.
- **Registry availability at pod creation.** Overlay delivery already depends on pulling the agentd image volume; this adds a second pull. The pins ConfigMap protects annotation resolution, not pulls — same exposure as today's #863, but doubled. Accepted; node caching mitigates.

## 9. Assumptions (stated and validated — README-LLM Rule 7)

| Assumption | Validation |
|---|---|
| `redact` has no consumers outside platform-owned paths (safe to fold into agentd) | `grep -r redact` across repo: entrypoint + PATH wrappers + smoke-test only (`docs/reference/cli.md:148-167`, `runtime-environments.md:82`) |
| agentd subcommand dispatch supports adding `redact` without new machinery | `cmd/workspace-agentd/main.go:82-112` — five subcommands already dispatched by `os.Args[1]` |
| The supervisor can own opencode spawn from a mount | Design 0051: supervisor is PID 1 of the workspace container and #863 verify already lives there |
| Per-arch OCI-annotation stamping generalizes to a second artifact | CI already stamps agentd's (`helm/values.yaml` `agentdDelivery` comment; e2e-nightly builds the artifact) |
| Factory preimage/ruling #29 unaffected | Hash is over selection IDs + base *name* (`design/0046`); only base *version* semantics change |
| Base version today == platform version | `release.yml:1324` tags `base:${VERSION}`; catalog rows `0.8.0`/`0.15.7` match platform tags |
