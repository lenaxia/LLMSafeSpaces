# Worklog: Consistent build version flags across all components

**Date:** 2026-08-14
**PR:** #855

---

## Context

Every Go binary (api, controller, workspace-agentd, relay-router,
relay-proxy) had its own version plumbing, and production images
reported `"dev"` regardless of tag:

- `controller/main.go` and `cmd/workspace-agentd/main.go` carried their
  own `"dev"` fallback vars (`version/commitSHA/buildTime` and
  `buildVersion` respectively), decoupled from `pkg/version`.
- `api/Makefile` injected into linker symbols (`main.version`,
  `main.buildTime`) that do not exist in `api/cmd/api/main.go` — a
  silent no-op (historical, see worklog 0612).
- `runtimes/base/Dockerfile`, `cmd/relay-router/Dockerfile`,
  `cmd/relay-proxy/Dockerfile` had no version ldflags at all, so the
  agentd healthz version (`/v1/healthz`) always reported `"dev"` even
  for tagged releases.
- `release.yml` / `ci.yml` never passed `VERSION` as a build-arg to any
  image build, so every image built with the Dockerfile default (`dev`).

Goal: one source of truth (`pkg/version`), stamped via `-ldflags` in
every build path, so the platform-admin versions tab and every healthz
surface report the real semver for tagged releases.

## Assumptions

1. **`pkg/version` is the canonical ldflags target for all binaries.**
   Verified: `go build -ldflags "-X
   github.com/lenaxia/llmsafespaces/pkg/version.Version=9.9.9"` on a
   test main importing the package reports `9.9.9`. The API already
   read `version.Version` (router.go:750, platform_info.go:86), so
   wiring the other four binaries to the same package is consistent.
2. **`-X` on an un-imported package is a silent no-op** (verified: a
   scratch build with `-X` targeting a non-imported package succeeds and
   just drops the flag) — it does NOT fail the build. That silent no-op
   is exactly why the old `api/Makefile` flags were dead. The fix is
   therefore to make every binary that receives version ldflags import
   `pkg/version`. relay-router/relay-proxy now import it and surface the
   version in a healthz response header so the injection binds and is
   observable. (cmd/redact does NOT import pkg/version, so the base
   Dockerfile injects ldflags only into the agentd build, not redact.)
3. **The relay `/healthz` contract is 200 + empty body** (relay-router
   probes only `StatusCode == http.StatusOK`, verified in
   `cmd/relay-router/health.go`; `cmd/relay-proxy/README.md` documents
   "200 OK (no body)"). Therefore version is carried in an
   `X-Llmsafespaces-Version` response header, set BEFORE `WriteHeader`
   (a Set after WriteHeader is a silent no-op — guarded by test).
4. **Un-stamped local builds report `"unknown"`** (not `"dev"`), so a
   missing injection is obvious and no code path claims a `"dev"`
   version. No Go or frontend consumer parses/compares the default
   (grep-verified).
5. **Image tag vs binary version convention**: release image tags are
   semver without leading `v` (`0.15.5`, from `verify-changelog`'s
   v-strip; the GitOps repo's `helm-release.yaml` in
   talos-ops-prod/kubernetes/apps/llmsafespaces pins the same tag
   format). The `VERSION` build-arg is the
   v-stripped semver so binary version == image tag.
6. **CI `prepare` version resolution**: `github.ref_name` is `vX.Y.Z`
   on tag pushes (ci.yml triggers on `tags: ['v*.*.*']`) and
   `<N>/merge` on PRs; regex `^v[0-9]+\.[0-9]+\.[0-9]+$` therefore
   selects the semver path only on real tags, else falls back to
   `sha-<12>`. Env-var passthrough (not interpolation) avoids shell
   injection.

## Changes

### Go

- `pkg/version/version.go`: added `CommitSHA` and `BuildTime` vars;
  defaults `"unknown"`.
- `controller/main.go`: removed local `version/commitSHA/buildTime`
  vars; startup log reads `version.Version/.CommitSHA/.BuildTime`.
- `cmd/workspace-agentd/main.go`: `buildVersion = version.Version`
  (healthz now reports the injected version).
- `api/Makefile`: ldflags now target `pkg/version.Version/CommitSHA/
  BuildTime` (was dead `main.version`/`main.buildTime`).
- `cmd/relay-router/main.go`, `cmd/relay-proxy/main.go`: import
  `pkg/version`; `/healthz` sets `X-Llmsafespaces-Version` header.

### Dockerfiles (all 5 Go image builds)

`api/Dockerfile`, `controller/Dockerfile`, `runtimes/base/Dockerfile`
(agentd builder), `cmd/relay-router/Dockerfile`,
`cmd/relay-proxy/Dockerfile`: added `ARG VERSION=unknown`,
`ARG COMMIT_SHA=unknown`, `ARG BUILD_TIME=unknown` and inject all three
via `-ldflags`. Multi-line `-ldflags` continuation collapses to a valid
single flag string (verified by review).

### Workflows

- `release.yml`: all 5 image builds pass
  `VERSION=${{ needs.verify-changelog.outputs.version }}`,
  `COMMIT_SHA=${{ github.sha }}`,
  `BUILD_TIME=${{ needs.prepare.outputs.timestamp }}`; controller
  smoke-test also passes VERSION; **`build-relay-binaries` job now
  stamps the relay-proxy binaries** (was `-s -w` only) with the same
  semver — this is the authoritative tag-triggered relay distribution
  channel attached to the GitHub Release and consumed by relay VMs via
  cloud-init.
- `ci.yml`: `prepare` job resolves `version` (tag → v-stripped semver,
  else `sha-<12>`); all 5 image builds pass VERSION/COMMIT_SHA/
  BUILD_TIME.
- `publish-relay-binaries.yml`: version resolution moved BEFORE the
  build; binaries are stamped from the resolved tag name
  (`${RELAY_VERSION#v}` strips the leading `v`) rather than
  `GITHUB_REF_NAME` (which is the branch name on workflow_dispatch,
  e.g. `main`). Commit sha + build time stamped too.

### Tests

- `cmd/relay-proxy/proxy_test.go`: `TestHealthzHandler_Returns200`
  extended to assert `X-Llmsafespaces-Version == version.Version`;
  new `TestHealthz_VersionHeaderWiredThroughMux` asserts the header on
  the token-exempt wired route and that `/` stays token-gated.

### Docs

- `CHANGELOG.md`: entry under `[Unreleased]`.

## Verification

- `go build` / `go vet` clean: workspace-agentd, controller, api,
  relay-router, relay-proxy, pkg/version.
- agentd healthz handler tests pass (incl. version contract).
- relay-proxy tests pass: `TestHealthzHandler_Returns200`,
  `TestHealthz_VersionHeaderWiredThroughMux`,
  `TestRequireToken_HealthzAndMetricsExempt`.
- `-ldflags` injection verified end-to-end: scratch builds with
  injected Version/CommitSHA/BuildTime report the injected values
  (agentd healthz, relay binaries, api).
- Workflow YAML parses (release.yml 26 jobs, ci.yml 19 jobs).
- CI on the PR: Lint, gitleaks, Trivy, govulncheck, pkg/secrets
  integration, review, Test (full suite, race detector), Frontend
  (unit + typecheck + e2e) all pass.

## Follow-up filed

- The earlier stale sentence ("e2e-nightly.yml images remain un-stamped")
  was removed; e2e-nightly.yml kind-cluster image builds ARE stamped
  (VERSION + COMMIT_SHA, c91718b). Remaining un-stamped image paths:
  `e2e-pr.yml:87` (docker compose `--build` with no VERSION arg) and
  `local/bootstrap.sh` (below).
- `BUILD_TIME` format differs across call sites (epoch seconds in
  release.yml/ci.yml vs `%Y-%m-%d_%H:%M:%S` in api/Makefile and
  publish-relay-binaries.yml) — cosmetic, no consumer parses it.
- `local/bootstrap.sh:130-142` builds images un-stamped → local kind
  clusters report `unknown` (previously `dev`) — acceptable dev path,
  could add `VERSION=local` for clarity.
- `publish-relay-binaries.yml:37` interpolates `${{ inputs.tag }}` into
  shell (release.yml uses env passthrough) — pre-existing, worth aligning
  in a follow-up.
