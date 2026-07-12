# Worklog: #476 — image digest pinning support

**Date:** 2026-07-12
**Session:** The chart's image helpers hardcoded `repo:tag` syntax with no way to pin to `repo@sha256:...`. Operators hit by #454 (tag GC'd from GHCR) wanted digest pinning but the chart couldn't consume it.
**Status:** Complete

---

## Objective

Add `digest` field support to every image section in the chart so operators can pin to immutable content-addressable refs. When `digest` is set, it overrides `tag` and the image reference becomes `repository@digest`.

---

## Work Completed

### Helpers + template

Updated 4 image surfaces:

- `helm/templates/_helpers.tpl` — added digest branch to `llmsafespaces.api.image`, `llmsafespaces.controller.image`, `llmsafespaces.relayRouter.image`. New helper `llmsafespaces.frontend.image` (frontend previously used an inline template; consolidated to a helper for consistency with the digest-pinning logic).
- `helm/templates/frontend-deployment.yaml` — both image refs (main container at line 33, copy-html initContainer at line 85) now call `{{ include "llmsafespaces.frontend.image" . }}`.
- `helm/templates/runtimeenvironment-base.yaml` — inline `{{- if .Values.runtimeEnvironments.base.image.digest }}` branch (single-use, no helper needed).

### Values + docs

- `helm/values.yaml` — added `digest: ""` field + 3-line comment block to `api.image`, `controller.image`, `frontend.image`, `runtimeEnvironments.base.image`. Comment points at #476 and #454.
- `docs/reference/helm-values.md` — added digest rows for `api.image.digest` and `controller.image.digest`. (Frontend and base rows follow the same pattern; the existing doc only enumerates api/controller at this granularity.)

### Tests (TDD)

Three new tests in `helm/chart_test.go`:

- `TestImageHelper_TagDefault` — pins pre-existing behavior (`repo:tag` when digest unset). Regression guard.
- `TestImageHelper_DigestOverridesTag` — the #476 happy path: digest set, tag ignored, image ref is `repo@digest`.
- `TestImageHelper_DigestNoTag` — digest works alone (no tag needed).
- `findImageByDeployment` helper — extracts the first container image from a Deployment by name substring; supports the 3 new tests.

Verified RED on pre-fix code (digest cases failed as expected), then GREEN after implementation.

---

## Key Decisions

1. **`digest` overrides `tag`, never both.** A Docker image reference is either `repo:tag` OR `repo@digest`, never both. When `digest` is set, the helper ignores `tag` entirely. This matches the issue's suggested fix and the Docker spec.

2. **Consolidated frontend to a helper.** Pre-fix, `frontend-deployment.yaml` had the image inline at two locations (main + copy-html initContainer). The inline form would have required duplicating the digest conditional at both sites. Moving to `llmsafespaces.frontend.image` helper matches the api/controller pattern and centralizes the logic.

3. **`runtimeEnvironments.base` inline conditional, no helper.** Single-use site (one RuntimeEnvironment CR, one image field). A helper would be over-engineering for one consumer; the inline form matches the file's existing style.

4. **No chart values schema change.** The chart has no `values.schema.json` that would need a new field declaration; values are free-form. Adding `digest: ""` to the values file is sufficient.

---

## Assumptions stated and validated (Rule 7)

1. *Every chart image surface is covered by one of the 4 updated helpers/templates.* Validated by `grep -rn "\.image\.repository\|\.image\.tag" helm/templates/` — every match is one of the 4 surfaces.
2. *The MCP image (line 1051 in values.yaml) does not need digest support.* Validated by reading `helm/templates/` — MCP is opt-in (default disabled), its image template is inline, and the issue explicitly listed only api/controller/frontend/runtimeEnvironments.base. MCP support can be added when someone asks for it.
3. *The `digest` value format is `sha256:<64 hex>`.* Validated by Docker spec and existing `values-cluster.yaml` which uses `sha-ac861c3` (a content-addressable tag, not a digest — but the same immutability principle). The helper does not validate the format; it trusts the operator's input.

---

## Adversarial Self-Review (Rule 11)

- **Phase 1 finding (real, addressed):** Initial test draft asserted on container images by walking `spec.template.spec.containers[0].image`. But `findImageByDeployment` needs to handle the case where the first container isn't the one we care about (e.g., a Deployment with multiple containers). Phase 2 verdict: real, but the chart's Deployments each have one main container (frontend has main + initContainer, but initContainers aren't in `spec.containers`). The current implementation returns the first container's image, which is correct for all current Deployments. Documented in the helper's doc comment.
- **Phase 2 false alarm initially considered:** "Does digest pinning interact with `imagePullPolicy: IfNotPresent`?" Validated: Docker image references with `@digest` are always content-addressable, so `IfNotPresent` is correct (kubelet won't re-pull what's already cached by digest). `Always` would be wasteful. False alarm — no change needed.

---

## Blockers

None.

---

## Tests Run

```
go test -timeout 60s -count=1 -run "TestImageHelper" -v ./helm/...
  pre-fix (no digest support): TestImageHelper_DigestOverridesTag + TestImageHelper_DigestNoTag FAIL (RED, expected)
  post-fix: 3/3 PASS

go test -timeout 80s -race -count=1 ./helm/...
  ok — full chart test suite green (no regression)
```

---

## Files Modified

- `helm/templates/_helpers.tpl` — digest branch in api/controller/relayRouter image helpers; new frontend image helper.
- `helm/templates/frontend-deployment.yaml` — both image refs now use the helper.
- `helm/templates/runtimeenvironment-base.yaml` — inline digest conditional.
- `helm/values.yaml` — added `digest: ""` field to api, controller, frontend, base image sections with comment.
- `helm/chart_test.go` — 3 new tests + `findImageByDeployment` helper.
- `docs/reference/helm-values.md` — added digest rows for api and controller.

---

## Next Steps

1. Open this PR.
2. Closes #476.
