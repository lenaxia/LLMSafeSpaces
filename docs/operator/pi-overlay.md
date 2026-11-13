# The pi overlay (second-harness delivery)

Status: delivered (design 0053 §5.4 — *"a second agent is a different
image at the same mount point, not a redesign"*). This page records the
delivery contract and the **trust ruling** for the third image-volume
artifact. For the agentd/opencode overlays see
[agentd-delivery.md](agentd-delivery.md); for the scoping that
authorized this see the pi second-harness scoping report (§2) and design
0053 §5.4.

## What ships

`ghcr.io/lenaxia/llmsafespaces/pi` — a `FROM scratch` image carrying the
[pi](https://github.com/earendil-works/pi) coding agent as a **directory
tree at `/pi`**:

```
/pi/pi                      the bun-compiled executable (~123 MB, dynamically linked)
/pi/package.json            adjacency REQUIRED by bun's package.json autoload
/pi/native/linux/prebuilds/ per-arch native helpers
/pi/theme/ /pi/assets/ /pi/export-html/ /pi/docs/ /pi/examples/
/pi/photon_rs_bg.wasm       runtime wasm
```

The image volume mounts **read-only at `/pi` on the workspace container
only** (the supervisor lives there; neither the agentd sidecar nor init
containers mount it). It is pinned by the controller
(`--pi-image`, Helm `controller.piDelivery.image`) and **mandatory from
day one** — unlike agentd/opencode there was never a baked-in mode to
fall back to.

## Trust ruling: hash-the-binary + digest-the-tree

pi's release artifact is a directory TREE (binary + runtime assets it
resolves relative to itself), not one file. The design-0053 §4.1 "one
executable, one hash" contract extends to a tree as (the #1416
CA-bundle / #1469 plugin precedent — non-executable data inside the
digest):

| Surface | Integrity mechanism |
|---|---|
| The **binary** `/pi/pi` | per-arch sha256 pins — CI stamps them as OCI index annotations (`dev.llmsafespaces/pi.sha256-{amd64,arm64}`), the supervisor verifies before any harness spawn, **exit 85/86** on mismatch |
| Every other tree member | covered by the **image digest** (the digest-pinned image volume is the transport-integrity boundary) — non-executable data consumed by the pinned binary |

Never add a second executable to the tree. A CI mode-pin check asserts
the tree stays traversable/executable for uid 1000 and that the
`package.json` adjacency survives every build.

### Why the binary is at `/pi/pi`, not `/pi/bin/pi`

The scoping sketch said `/pi/bin/pi`; the delivered layout preserves the
upstream release tree byte-for-byte instead. bun's
`--compile-autoload-package-json` resolves a `package.json` **next to
the executable**, and the native prebuilds sit at fixed relative paths —
relocating the binary inside its tree would silently break runtime
resolution. The controller pins `LLMSAFESPACES_PI_BINARY=/pi/pi`.

## Build provenance (reproducible, offline model data)

`runtimes/pi/Dockerfile` builds from the **release source archive**
(`pi-<version>-source.tar.gz`, sha256-verified against the release's
`SHA256SUMS` before the build — pi publishes checksums; the opencode
overlay could not do better than TLS) using upstream's own pipeline:

```
npm ci --ignore-scripts
./scripts/build-binaries.sh --offline-model-data --platform linux-<arch>
```

That is exactly how upstream's release workflow builds the official
standalone binaries from the source archive. `--offline-model-data`
builds against the model data **hydrated into the archive at release
time** — no pi.dev catalog traffic at build or in the delivered image.
(The npm registry is still needed for `npm ci`; "offline" scopes to
model data, matching upstream's usage.) Toolchain pins: node 22 (base
image digest-pinned) + bun 1.4.4 (upstream's release pin).

CI (`build-pi`/`merge-pi` in `.github/workflows/ci.yml`, cloned from the
opencode stamp jobs) builds both arches natively, extracts the binary
sha256 per arch, and stamps them onto the image index — one
Renovate-updatable coordinate (`repo:tag@sha256:<manifest-digest>`)
carries the whole verify contract.

## Verify contract and failure attribution

Supervisor exit codes in the workspace container are a ladder, disjoint
per artifact:

| Artifact | Verify failed | Overlay missing |
|---|---|---|
| agentd (self-verify) | 81 | 82 |
| opencode | 83 | 84 |
| **pi** | **85** | **86** |

The controller maps exit 85/86 to the `PiVerified=False` condition with
reasons `PiVerificationFailed`/`PiOverlayMissing`, one warning event and
one `llmsafespaces_workspace_pi_verify_failures_total` increment per
failure episode — a should-never-fire signal (page on any occurrence).
`PiVerified=True` is stamped once a pod carrying the overlay is observed
running clean.

> The **controller process** also exits 85 for "relay staging not armed"
> (design 0061 §3) — a different process on a different pod;
> attribution is by which pod crashed.

## Pin resolution and the ConfigMap cache

The normal operator form is **image-only**
(`ghcr.io/lenaxia/llmsafespaces/pi:1.5.2@sha256:…`): the controller
resolves the per-arch binary sha256s from the index annotations at
startup and caches them in the `llmsafespaces-pi-pins` ConfigMap — a
controller restart during a registry outage falls back to the cache only
for the **same digest** (fail-closed otherwise). Break-glass
`binarySHA256Amd64`/`binarySHA256Arm64` overrides exist (both or
neither), same as agentd/opencode.

## Bumping the pi version

`runtimes/pi/Dockerfile` pins `ARG PI_VERSION` + `ARG PI_SOURCE_SHA256`
(from the target release's `SHA256SUMS`). Bump both in one change; the
S6 drift-gate story adds the RPC-fixture refresh regime (opencode's
REFRESH.md pattern) — until then, a bump is a reviewed, manual pin move
validated by CI's source build and the kind suite.
