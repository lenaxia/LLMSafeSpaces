# Worklog: #1918 — npm publish E404 (dead NPM_TOKEN), SDK red decoupled from the release conclusion, create-release made re-run idempotent

**Date:** 2026-10-24
**Session:** Lane 1918 (worker tsk-tropical-uniform-plumbs)
**Status:** Complete

---

## Objective

Issue #1918: the `Publish TypeScript SDK to npm` job failed on release run 40046673282 (v0.41.0) and turned the whole release run `failure` even though every container image, the Helm chart, cosign signing, and the GitHub Release had shipped. The issue's diagnosis documents a SECOND failed leg on the same run: `Create GitHub Release` 422'd (`tag_name already exists`) on the re-run, leaving v0.41.0 with zero release assets. Fix in-repo: fail-fast auth preflight + decouple SDK red from the release conclusion with a distinct outcome-surfacing job + make create-release re-run idempotent (view/edit/clobber).

## Root cause (evidence)

- Run 40046673282, step "Publish to npm", 2026-10-24T01:45:08Z: `npm error code E404`, `PUT https://registry.npmjs.org/@llmsafespaces/sdk -> 404 Not Found`; exit 1.
- NOT a version collision: `@llmsafespaces/sdk@0.40.1` published 2026-10-21T19:31Z is the registry's latest; 0.41.0 never reached the registry.
- NOT a workflow regression: `git diff v0.40.1 v0.41.0 -- .github/workflows/release.yml` shows only unrelated go-version bumps (1.26.9 → 1.26.10); the npm job is byte-identical.
- Wiring: job yaml has `environment: npm` + `NODE_AUTH_TOKEN: ${{ secrets.NPM_TOKEN }}` + setup-node registry-url; the `npm` GitHub environment has ZERO environment-scoped secrets, so NPM_TOKEN resolves from repo-level secrets. `package.json` has no `publishConfig` — the registry comes from setup-node's `.npmrc`; nothing repo-side can produce a 404.
- Classification: npm deliberately answers a publish PUT that the token cannot see with **404, not 401/403** — an expired/revoked/mis-scoped NPM_TOKEN (granular token whose scope allowlist lost `@llmsafespaces/sdk`) masquerades as "package not found". Token worked 10-21, dead by 10-23. The token itself is an ops-owned secret — rotation is documented in the PR body, not done in-repo.

## Fix (release.yml + pins)

1. **Preflight auth check** (publish-typescript-sdk, before the publish PUT): `npm access ls-packages --json` — chosen over `npm whoami` because whoami cannot authenticate granular access tokens; ls-packages works for classic AND granular tokens and additionally proves `@llmsafespaces/sdk` is on the token's allowlist with `read-write`. All three failure branches (auth error / package invisible / read-only) emit `::error::` annotations naming the NPM_TOKEN rotation procedure. Branch logic simulated against a stubbed npm: all four cases exit correctly.
2. **Fail-soft SDK legs**: job-level `continue-on-error: true` on BOTH `publish-typescript-sdk` and `publish-python-sdk` (the issue ask covers "SDK legs" plural; a PyPI-side failure would equally redden container releases). The run conclusion now reflects the container-artifact jobs only.
3. **Distinct outcome-surfacing** (never-a-silent-skip, run-watchdog.yml convention): new `sdk-outcome` job with `if: !cancelled()`, `needs: [publish-python-sdk, publish-typescript-sdk]`, reading each leg's RAW `result` (under continue-on-error, `outcome` lies "success"). Converts red legs into `::warning::` run annotations (with the E404→rotate-NPM_TOKEN diagnosis inline) + a `$GITHUB_STEP_SUMMARY` table, and always exits 0 — surfacing, not gating.
4. **Pins** (`pkg/repolint/release_sdk_legs_test.go`, structural yaml.v3 — spelling-proof, `needs` parses as string or list): every `publish-*-sdk` job must be continue-on-error (future SDK legs auto-covered); `sdk-outcome` must exist, run on `!cancelled()`/`always()`, need both legs, and the workflow must read `needs.<leg>.result`; the npm leg must carry the ls-packages preflight (before the publish step, env'd with the same `NODE_AUTH_TOKEN` secret) naming NPM_TOKEN; `create-release` must NOT be gated on SDK legs. Red-first verified: all pins fail against unfixed release.yml, pass against the fix.
5. **create-release re-run idempotent (finding 2 of the issue's diagnosis)**: the v0.41.0 re-run hit `HTTP 422: Release.tag_name already exists` on the bare `gh release create`, and the attach step after it never ran — v0.41.0 shipped with ZERO release assets. Fix: `gh release view "v${VERSION}"` probe → `gh release edit` (notes updated in place) when the release exists, `gh release create --verify-tag` only on the fresh path; asset upload now `gh release upload --clobber` so re-attaching overwrites instead of failing on existing filenames. Pinned by `TestReleaseWorkflow_CreateReleaseReRunIdempotent` (red-first verified: probe/edit/clobber findings each fire against the bare create).

## Validation

- `go test ./pkg/repolint/` — ok (includes the workflow shell-syntax guard's `bash -n` over both new run blocks).
- `go vet ./pkg/repolint/`, `gofmt` — clean.
- `go run ./cmd/repolint .` — all checks passed (release-artifact parser unaffected).
- SDK build: `cd sdks/typescript && npm ci && npm run build` — ESM/CJS/DTS all green locally.
- Preflight + outcome scripts simulated against stubbed npm/env: all branches behave as designed (fail-fast messages, exit codes, summary table).

## Ops follow-up (outside the repo)

Re-runs execute the workflow snapshot at the ORIGINAL triggering ref (the tag's commit) — GitHub re-uses the original `GITHUB_SHA`/`GITHUB_REF` and workflow file, so NO change in this PR applies to run 40046673282; it protects future releases and re-runs of them. Remediation for the current incident:

1. Rotate the repo-level `NPM_TOKEN` secret (npmjs.com → Read-and-write, `@llmsafespaces` scope or classic Automation).
2. Publish the SDK: `gh run rerun 40046673282 --failed` (or scope to the job via `--job <publish-job-id>`). The tag-ref snapshot's plain `npm publish` succeeds with a live token and ships `@llmsafespaces/sdk@0.41.0`. Caveat: a `--failed` re-run also re-runs `create-release`, which still 422s against the existing v0.41.0 release (the snapshot predates this PR) and keeps that run's conclusion `failure`.
3. Attach the missing assets (v0.41.0 currently has zero): `gh release upload v0.41.0 <files> --clobber` from the run artifacts — the same idempotency this PR bakes into the workflow — or delete + recreate the release.

Longer-term: consider npm OIDC trusted publishing once npm supports it for CI (the PyPI leg already went OIDC).
