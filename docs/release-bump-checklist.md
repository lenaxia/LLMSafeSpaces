# Release bump checklist — components on divergent version schemes

> Born from issue #1237: three incidents in four days (2026-08-29 →
> 2026-09-02), one fleet-wide outage, all one class — a coordinated
> version bump applying the platform `VERSION` to a component that is
> **not** on the platform version scheme.
>
> **Rule zero: never set a component's coordinate by rote from the
> release VERSION.** Find the component in the table below, use its
> scheme's source of truth, and run its gate. The mechanical guards
> (§Guards) catch the two checkable classes at commit time; this
> checklist covers the rest of the procedure.

| Incident | Date | Class |
|---|---|---|
| #1119 thread | 2026-08-29 | config bump set five component tags with no runtime contract gate (wire-schema drift found by luck) |
| agentd digest paste | 2026-09-01 | agentd per-arch digest lines refreshed with a **controller** digest (caught in review) |
| fleet-wide outage | 2026-09-02 | bump set `runtimeEnvironments.base.tag: "0.26.0"` — base is CalVer; `base:0.26.0` never existed → every pod recycle `ImagePullBackOff` → all chats stuck on Creating |

---

## 1. The schemes

| Component | Scheme | Source of truth | Bump rule |
|---|---|---|---|
| `api` / `controller` / `frontend` / `relay-router` / `relay-proxy` images | platform semver | `helm/Chart.yaml` `appVersion` + the `vX.Y.Z` release tag; `release.yml` builds/tags all of them `:${VERSION}` | tag = platform VERSION |
| Helm chart | chart semver | `helm/Chart.yaml` `version` | bump on every chart change |
| GitRepository (chart source, Flux) | release tag | the platform release tag; chart is consumed from git, not a registry (`helm/Chart.yaml` header, #456) | tag = platform VERSION; `reconcileStrategy: Revision` |
| base runtime (`runtimes/base`) | **CalVer `YYYY.MM.x`** | `api/internal/imagefactory/catalog.seed.yaml` → `bases[bookworm].version` — the SINGLE SOURCE (design 0053 D5/S4; `base-image.yml` header) | **NEVER the platform VERSION.** Content change ships as ONE PR: `runtimes/base/**` change + seed row bump (month rollover if the calendar moved, else patch +1) + `helm/values.yaml` mirror. Merging triggers `base-image.yml`, which publishes exactly the seed tag (idempotent; an existing tag is never moved) |
| `runtimeEnvironments.base.image.tag` (chart) | CalVer (mirror) | mirrors the seed's default row; repolint fails on drift or non-CalVer (§Guards) | bump in the same PR as the seed row |
| agentd overlay artifact | digest pin | release workflow output: `release.yml` merge-agentd job prints the `controller.agentdDelivery` values block | image = `ghcr.io/lenaxia/llmsafespaces/agentd@sha256:<index digest>` from **the agentd index** — never any other image's digest |
| opencode overlay artifact | digest pin | `release.yml` merge-opencode job prints the `controller.opencodeDelivery` values block | image = `ghcr.io/lenaxia/llmsafespaces/opencode@sha256:…` from **the opencode index** |
| opencode upstream version | upstream semver, platform-validated | `runtimes/opencode/Dockerfile` `ARG OPENCODE_VERSION` (currently 1.18.15); mirrored in `local/bootstrap.sh` | moves ONLY via `.github/workflows/opencode-version-bump.yml` — never sed by hand (see §3) |
| per-arch binary sha256 (`binarySHA256Amd64/Arm64`) | break-glass overrides | stamped on each artifact's OCI index as annotations by CI; controller resolves them at startup | leave empty (image-only) in the normal form; if the index lacks annotations, set BOTH from the artifact's own build output — never from a platform image digest |
| relay-proxy binaries (VM fleet) | release assets + sha256 | `controller.inferenceRelay.artifact.{urls,sha256Arm64,sha256Amd64}`; built by `make relay-bin`, published on the release (`publish-relay-binaries.yml` for ad-hoc builds) | recompute `sha256sum` per arch from the assets this release attached |
| image-factory catalog seeds (extensions) | content pins (mise/apt versions) | `api/internal/imagefactory/catalog.seed.yaml` `extensions:` | ride the base's cadence, not the platform train |
| third-party images (`migrations`, `dbInit`) | upstream tags (Renovate domain) | `helm/values.yaml` (`migrations.image`, `dbInit.image`) | Renovate PRs; not part of a coordinated bump |

---

## 2. Platform release bump (the coordinated part)

1. **CHANGELOG + appVersion, same commit**: add the `## [X.Y.Z] - YYYY-MM-DD` section and bump `helm/Chart.yaml` `appVersion` to X.Y.Z (a chart-test guards the pairing: `helm/appversion_drift_test.go`). Bump chart `version` if any chart file changed.
2. **Cut the tag**: `make release-tag VERSION=X.Y.Z` (Makefile `release-tag` target — validates semver, CHANGELOG section, tag novelty, main up-to-date). `release.yml` builds, signs, scans, SBOMs, and publishes **all** platform images + the two overlay artifacts + relay binaries, and publishes the chart to the gh-pages Helm repo (chart-releaser). Release success == every artifact published (repolint `release_artifacts` check enforces the wiring).
3. **Config PR (ops repo)** — for each component, copy from the release output, not from memory:
   - `api`/`controller`/`frontend` (+ relay images if enabled): tag or digest `:${VERSION}` / `@sha256:<digest>` from the release's image table.
   - `controller.agentdDelivery.image`: paste the **merge-agentd printed values block** (`release.yml` "Print Helm values block for agentdDelivery").
   - `controller.opencodeDelivery.image`: same, from **merge-opencode**.
   - `runtimeEnvironments.base.image.tag`: **leave alone** unless the seed row moved (§1) — it is CalVer, off the train.
   - GitRepository/ HelmRelease `ref.tag`: `vX.Y.Z` with `reconcileStrategy: Revision`.
4. **Pre-flight the config PR** (the four checks from #1237):
   - every `tag:`/`@sha256:` image+coordinate **resolves in ghcr** (`docker buildx imagetools inspect <ref>` or a manifest HEAD) — catches the Incident-3 class at PR time;
   - every per-arch `sha256:` belongs to the **named image's** index — catches Incident 2;
   - the base tag equals the **catalog seed version** — drift is a red light (and in this repo, a repolint failure);
   - if the opencode pin changed anywhere in the bump: §3's gates are green against the runtime the bump ships.

## 3. The opencode pin — contract gates are MANDATORY pre-bump

The opencode version is platform-**validated** (worklog 0657 lineage): the wire
shapes the API/adapter depend on are NOT stable across upstream releases. The
#1119 incident (2026-08-29) was a wire-schema drift that a version bump shipped
with no behavioral gate. Since #1120 there are three gates — a bump that skips
any of them is incomplete:

1. **Golden wire fixtures** — `pkg/agent/opencode/testdata/` (event store,
   history, SSE, config schema; upgrade runbook `REFRESH.md`). Every parser
   pins these shapes; a bump without a fixture refresh ships stale parsers.
2. **CI fixture-freshness gate** — `.github/workflows/ci.yml` ("Opencode bump
   requires fixture refresh"): a PR that changes `ARG OPENCODE_VERSION` without
   touching `pkg/agent/opencode/testdata/` fails CI.
3. **The behavioral contract script** — `local/opencode-binary-contract.sh`:

   ```bash
   OPENCODE_BIN=/path/to/candidate ./local/opencode-binary-contract.sh
   ```

   It boots the candidate binary and probes the semantics the unit goldens
   cannot: **B1** V1 message route executes (control) · **B2** V2 prompt route
   exists (400-on-invalid, not SPA/404) · **B3** `Model.Ref` shape
   (`{id,providerID}` accepted, legacy `{modelID}` rejected — the exact
   1.18.15 drift) · **B4** idle admission makes `prompted` observable for
   defect-class deaths (the #1119 outbox completion seam) · **B5** durable
   event log monotonic seq + `messageID` on admitted. Any FAIL = do NOT bump.
   Validate polarity against a **pristine upstream tarball** — a locally
   installed fork can report an old version with new behavior.

The supported path for moving the pin is
`.github/workflows/opencode-version-bump.yml` (weekly + dispatch with a target
version): it refuses known-bad windows (v1.18.16–.18 — v1-DB breakage), opens
a bump PR updating `runtimes/opencode/Dockerfile` + the `local/bootstrap.sh`
mirror with the fixture-refresh runbook front and center, and never
automerges — the fixture refresh is a human+spike decision (REFRESH.md).
Manual sed bumps bypass the fixture gate's intent — don't.

After the pin builds, the **opencode artifact digest** for
`controller.opencodeDelivery.image` comes from the merge-opencode values block
of the release that built it — the two coordinates (upstream version, delivery
digest) move together but are set in different places by design (design 0053
§5: independent rollback, cadence, auditability).

## 4. The base — CalVer procedure (design 0053 D5/S4)

The base is the dev-OS; its version axis is its own content. There is no
platform-train base tag and there will never be one again.

1. Change `runtimes/base/**`.
2. Bump the seed row `api/internal/imagefactory/catalog.seed.yaml` →
   `bases[bookworm].version` (+ `tag` — must equal `version`): month rollover
   if the calendar moved since the last content change, else patch +1.
3. Mirror the same value in `helm/values.yaml`
   `runtimeEnvironments.base.image.tag`.
4. One PR, all three. Merging fires `base-image.yml`, which reads the seed row
   and publishes exactly that tag (both arches, signed; idempotent).
5. Rollout (ops repo): bump `runtimeEnvironments.base.image.tag` to the new
   CalVer value and verify the tag resolves in ghcr before merge.

Never: a semver/`latest`/`sha-`/platform-VERSION tag in the base position. The
chart fails the render on an empty base tag and repolint fails non-CalVer or
seed-drifted values (§Guards), so the remaining mistake class is an explicit
wrong-CalVer tag — the ghcr pre-flight in §2 catches that.

## 5. Guards (what fails loudly, where)

| Guard | Enforces | Where it runs |
|---|---|---|
| repolint `version_scheme` check (`pkg/repolint/version_scheme.go`) | base tag is CalVer and equals the seed default row; seed rows are CalVer with `tag == version`; exactly one default row; `agentdDelivery`/`opencodeDelivery` image digests and per-arch binary pins never equal a platform component digest (`controller`/`api`/`frontend`/relay-router) and never equal each other; structural keys must exist (renames fail, not pass vacuously) | `make repolint` — pre-commit hook + CI Lint job + release.yml |
| chart render gate (`helm/templates/runtimeenvironment-base.yaml`) | an empty `runtimeEnvironments.base.image.tag` fails the render — the chart never substitutes the platform appVersion (the incident-3 mechanism) | every `helm template`/install/Flux render |
| `helm/appversion_drift_test.go` | default-rendered base RTE image == seed CalVer (and != appVersion); appVersion == latest CHANGELOG section | `go test ./helm/...` |
| CI fixture-freshness gate | OPENCODE_VERSION bump ⇒ golden fixtures touched in the same PR | `.github/workflows/ci.yml` |
| repolint `release_artifacts` check | every release image is signed/scanned/SBOM'd/tabled; merge jobs gate the release | `make repolint` |
| delivery-pin render gates (design 0053 §4.5) | empty `agentdDelivery.image` / `opencodeDelivery.image` fail the render; one-sided `binarySHA256*` pairs fail | `helm/delivery_pins_gate_test.go`, controller startup |

**Known limit:** the repolint guard reads this repo's committed files; it
cannot see the ops config repo's values. The ops-side classes are covered by
the §2 pre-flight (run against the candidate config PR) — the two layers are
complementary by design.
