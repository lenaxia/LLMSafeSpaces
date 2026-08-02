# Worklog NNNN — Image Factory S8: GH Actions image-build workflow

**Date:** 2026-08-02
**Scope:** S8 — the GitHub Actions workflow that builds custom workspace images.

## Summary

Created `.github/workflows/image-build.yml` triggered by `workflow_dispatch`
from the API's `POST /configs` handler. The API pre-renders the Dockerfile
using the tested pure `RenderDockerfile` function and passes it as a
workflow input — zero duplicated rendering logic. The workflow writes the
Dockerfile to a minimal build context, builds multi-arch via `docker buildx`,
pushes to ghcr.io, and reports the result via authenticated callback.

## Key design decisions

1. **API renders Dockerfile, not the workflow.** Eliminates the entire
   class of divergence bugs between a Go renderer and any workflow-side
   renderer. The `RenderDockerfile` function (S1, 15+ tests) is the single
   source of truth.
2. **Raw `docker buildx build` with `tee` for log capture.** Instead of
   `docker/build-push-action` (which doesn't expose build output), the
   workflow uses a raw buildx command piped through `tee /tmp/build.log`
   with `set -eo pipefail`. This captures actual error output for the
   failure callback's LLM explainer.
3. **Minimal build context.** Only `/tmp/build-context` (containing just
   the Dockerfile) is uploaded — no repo checkout needed since the
   Dockerfile contains only FROM/RUN/USER/WORKDIR/ENTRYPOINT.
4. **Retry deferred.** Design #12 assigns retry to the workflow level,
   but the current implementation reports the first result without
   internal retry. Documented in the header comment.

## Review-driven fixes

- Quote-stripping: `${{ inputs.dockerfile }}` interpolation strips JSON
  quotes → env var `DOCKERFILE_CONTENT` preserves exact bytes.
- pipefail: `shell: bash` + `set -eo pipefail` so buildx failure propagates
  through `tee` (tee's exit code is 0 without pipefail).
- Callback URL doubling: default is `/internal/image-factory` (not
  `/internal/image-factory/builds`), since the workflow appends
  `/builds/${BUILD_ID}/callback`.
- Removed dead `base_image_ref` input and `actions/checkout` step.
