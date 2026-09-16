# Worklog: Release bump checklist + version-scheme guards

**Date:** 2026-09-16
**Session:** Issue #1237 — docs/release-bump-checklist.md mapping every versioned component to its scheme and bump procedure, wiring the opencode contract gates into the procedure, and a mechanical repolint guard for the two checkable incident classes; plus fixing the live chart defect the guard exposed (base tag appVersion fallback).
**Status:** Complete

---

## Objective

Three incidents in four days (one fleet-wide outage) came from coordinated bumps applying the platform VERSION to components not on the platform scheme. Deliver: (1) a checklist enumerating every versioned component → scheme → source of truth → bump rule → gate; (2) the goldens + `local/opencode-binary-contract.sh` wired in as mandatory pre-bump steps when the opencode pin changes; (3) a mechanical guard (repolint chosen, see Key Decisions) that fails loudly on the base-CalVer class and the agentd-digest-paste class; (4) this worklog.

---

## Work Completed

### Checklist (`docs/release-bump-checklist.md`)
- Enumerated the components from the tree: platform semver images (api/controller/frontend/relay-router/relay-proxy — verified against `release.yml`'s env + sign/scan loops; `mcp` deliberately NOT listed: no workflow builds that image), chart semver, GitRepository tag, base CalVer (seed row single source), the chart's base-tag mirror, agentd/opencode overlay digest pins, per-arch binary sha256 break-glass pins, the upstream OPENCODE_VERSION pin + `local/bootstrap.sh` mirror, relay-proxy binary artifacts + sha256s, image-factory extension seeds, third-party images.
- Wired the three opencode contract gates as mandatory pre-bump steps: the golden fixtures (`pkg/agent/opencode/testdata/` + REFRESH.md), the CI fixture-freshness gate (ci.yml "Opencode bump requires fixture refresh"), and `local/opencode-binary-contract.sh` B1–B5 (read in full and described per-check; pristine-tarball caveat preserved).
- Documented the base CalVer procedure per design 0053 D5/S4 + `base-image.yml` header ("the version SINGLE SOURCE is the catalog seed row"): one PR = runtimes/base change + seed row bump + values.yaml mirror; publish is automatic + idempotent.
- Documented the ops-config pre-flight (the issue's four checks) since the config repo is outside repolint's reach.

### Guard (TDD)
- `pkg/repolint/version_scheme_test.go` written FIRST (RED: `undefined: RunVersionSchemeCheck`), then `pkg/repolint/version_scheme.go` (GREEN). 22 test functions / 38 executed cases: happy path; base-tag CalVer violations (semver, one-digit month, month 13, latest, empty); base-tag drift vs seed; digest-pinned base skips tag checks; seed CalVer + tag==version + neither-tag-nor-digest + digest-pinned-row + exactly-one-default-row (zero and two) + empty-seed; agentd/opencode image-digest and per-arch-binary-pin equality vs controller/api/frontend/relay-router digests; agentd↔opencode identical pins; distinct pins pass; 10 missing-structural-key cases (block keys, the relay-router block, digest leaves, binarySHA256 leaves); missing files; untagged image refs.
- Wired into `cmd/repolint/main.go` (`runVersionScheme`) — runs in pre-commit, CI (`ci.yml` make repolint), and release.yml's lint job.

### Pre-flight script (the issue's criterion 2, added in review round 1)
- `local/release-preflight.sh` — run against a candidate config values file: P1 registry resolution of every tag/digest (ghcr v2 API, `GHCR_API` overridable), P2 index membership + no delivery pin reusing a platform digest + tag/digest coherence against the live index + `binarySHA256*` break-glass pins verified against the CI-stamped index annotations (`dev.llmsafespaces/<artifact>.sha256-{amd64,arm64}`; un-annotated index + pins set = documented break-glass posture, noted not failed — round-2 review finding: the header claimed this verification before it existed), P3 base CalVer + seed single-source equality, P4 opencode coordinate vs the repo pin + optional behavioral-contract execution via `--opencode-bin`. `--offline` is an explicitly loud degraded mode.
- `local/release_preflight_script_test.go` — 4 test functions / 17 subtests: bash syntax, structure pins (each detector row must exist), offline behaviors (happy, both incident classes, drift, pin mismatch, empty tag, usage error), and online behaviors against an in-process ghcr-v2 stub (resolve pass, incident-3 404, incident-2 foreign digest, stale tag+digest pair, annotation match / mismatch / un-annotated break-glass).

### Live defect the guard exposed (fixed)
- The guard failed on main's own `helm/values.yaml`: `runtimeEnvironments.base.image.tag: ""` falls back to `.Chart.AppVersion` (`helm/templates/runtimeenvironment-base.yaml:9`) → `base:0.30.1`, a tag that has not existed since design 0053 moved the base to CalVer — the incident-3 mechanism, live in chart defaults.
- Fix: values default `tag: "2026.09.0"` (mirror of the seed's default row, with rationale comment); template now FAILS on an empty tag instead of substituting appVersion; `helm/appversion_drift_test.go` rewritten — `TestChart_DefaultBaseRTE_TagIsSeedCalVer` asserts the default render mirrors the seed row (CalVer-regex-checked) and never equals appVersion. `TestChart_AppVersion_MatchesLatestRelease` unchanged (platform-train invariant). Render-gate failure path pinned by `helm/runtimeenvironment_base_test.go` (empty tag fails with the mandatory message; explicit tag renders verbatim; digest pin wins).
- Stale docs corrected: `docs/operator/runtime-environments.md` (was: "falls back to Chart.AppVersion"), `docs/reference/helm-values.md` row, `helm/README.md` image-tags note + pinned-deploy example (dropped the base `sha-` pin line).

---

## Key Decisions

1. **repolint, not a chart test, as the guard mechanism.** The repo's guard culture for incident-born invariants over committed files is repolint (`release_artifacts.go` ← v0.19.1; `agent_id_prefix` ← #1305; `spec_coupling_marker` ← #1305). The check reads the actual `helm/values.yaml` + `catalog.seed.yaml` (the task's "validate against the actual values/seed"), compares values keys a render cannot (delivery digests vs platform digests), and runs at commit time (pre-commit + CI), not just at render/test time.
2. **No CalVer regex fail in the Helm template.** `local/s5-overlay-validation.sh:215` legitimately sets `runtimeEnvironments.base.image.tag=ci` (locally built kind image); a format gate in the template would break the shipped S5 suite. The template only fails on EMPTY (the appVersion substitution — the actual incident mechanism); format policing stays in repolint over committed files.
3. **Fixing the appVersion fallback is in scope, not scope creep.** A guard that "fails when base.tag doesn't match CalVer" is incoherent while the chart silently substitutes a platform semver for the empty default — the committed default WAS empty, so the guard would fail on main forever or the fallback stays a live incident-class bug (Rule 5). The fix makes default installs reference an existing tag.
4. **Structural-key presence = loud failure, for every key the guard reads.** The check verifies the block keys AND the comparison leaves (`*.image.digest`, delivery `image`/`binarySHA256*`, base `tag`/`digest`) — including the relay-router image block (present in the committed values; its digest joins the platform-digest set) — so a values-key rename cannot degrade the guard into a vacuous pass. Review round 1 caught the initial set being narrower than the read set (router + leaves); the rule is now "read set == presence set".
5. **Empty-vs-empty digests skip, not fail** — chart defaults carry no pins; the equality comparisons are active exactly when a coordinated bump has both sides set.
6. **Digest normalization to bare hex** (`sha256:<hex>` fields vs bare `binarySHA256*` values vs `@sha256:<hex>` refs) so all three pin forms compare correctly.
7. **Seed rows must set tag or digest** — neither means the row renders an untagged image ref (always broken); tag set ⇒ CalVer + `tag == version` (base-image.yml publishes the version as the tag); digest-pinned rows are valid without a tag.
8. **Per-arch pin verification is fail-loud in the unsafe direction** (round-3 review findings A+B): a `binarySHA256*` pin that disagrees with the CI-stamped annotation fails; a SET pin whose arch annotation is absent ALSO fails (skip-must-not-pass — the controller uses explicit pins as-is); only a fully-unannotated index with pins set is the documented break-glass posture (noted). The terminal ok is flag-guarded so a reported mismatch is never followed by a "match" line.

---

## Blockers

None.

---

## Tests Run

- `go test ./pkg/repolint/` — PASS (full package incl. TestVersionScheme: 22 funcs / 38 cases, test-first RED→GREEN).
- `go test ./local/ -run TestReleasePreflight` — PASS (4 funcs / 17 subtests incl. in-process registry-stub legs: resolve, both incident classes online, stale pin, annotation match/mismatch/break-glass, un-annotated-arch fail-loud, fetch failure, opencode-side symmetry).
- `go test ./helm/...` — PASS (full package, helm on PATH — CI installs latest; validated locally across helm v3 and v4 lines), including `TestChart_DefaultBaseRTE_TagIsSeedCalVer`, `TestBaseTag_MandatoryRenderGate`, and the delivery-pin render gates.
- `go run ./cmd/repolint` — all checks passed (after the values.yaml fix; failed as designed before it).
- `go build ./...`, `make test`, `make lint` — green before the PR (see PR body).

---

## Next Steps

- Ops side (talos-ops-prod): wire `./local/release-preflight.sh <candidate values>` into the config-bump PR template so P1–P4 run on every coordinated bump against the config repo.

---

## Files Modified

- `docs/release-bump-checklist.md` (new)
- `local/release-preflight.sh` (new)
- `local/release_preflight_script_test.go` (new)
- `pkg/repolint/version_scheme.go` (new)
- `pkg/repolint/version_scheme_test.go` (new)
- `cmd/repolint/main.go` (wire `runVersionScheme`)
- `helm/values.yaml` (base tag default `2026.09.0` + comment)
- `helm/templates/runtimeenvironment-base.yaml` (fail on empty tag; drop appVersion fallback)
- `helm/appversion_drift_test.go` (default-base test asserts seed CalVer, not appVersion)
- `helm/runtimeenvironment_base_test.go` (new — render-gate failure paths)
- `docs/operator/runtime-environments.md`, `docs/reference/helm-values.md`, `helm/README.md` (stale fallback docs corrected)
- `api/internal/services/database/database.go` + 7 `*_integration_test.go` files (pre-existing lint failures fixed, Rule 5)
- `worklogs/NNNN_2026-09-16_release-bump-checklist.md` (this file)
