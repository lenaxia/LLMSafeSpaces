# Worklog: Chart image-tag release workflow + fix dead values-cluster pins (#454)

**Date:** 2026-06-30
**Session:** Resolve #454 — the values-cluster.yaml dead-tag pin + the chart's missing release-workflow documentation. Per maintainer direction: chart should pin to semver and/or sha, while keeping intermediate builds (ts/sha/dev) for fast deploys.
**Status:** Complete

---

## Objective

`values-cluster.yaml` pinned all 4 images to `ts-1781285219`, which was pruned from GHCR. The chart's own default (`appVersion: "0.1.0"`) was also broken — no `v0.1.0` git tag was ever cut, so the image doesn't exist. Fix the dead pins and document the release/intermediate-build workflow so the tag strategy is clear and durable.

---

## Work Completed

- **`values-cluster.yaml`**: repointed all 4 pins from the dead `ts-1781285219` to `sha-ac861c3` (the immutable content-addressable tag for commit `ac861c3d`, the same manifest as the confirmed-working `ts-1782762331`). Updated the header comment to explain why `sha-` is preferred over `ts-` for pinned references.
- **`charts/llmsafespaces/README.md`**: added an "Image tags" section documenting:
  - The 4 tag types CI publishes (`sha-`, `ts-`, `dev`, semver) and their purposes.
  - How to pin for production (sha-/semver).
  - How to deploy intermediate builds for fast iteration (dev/ts-/sha-).
  - How to cut a release (`git tag v0.1.0 && git push`) so the chart default resolves without overrides.
  - A GHCR retention note explaining that `sha-`/`ts-` are pruned together (the key insight from the investigation) and that semver releases + retention config are the durable fix.
- Added a quick-install callout noting image tags must be supplied until `v0.1.0` is released.

---

## Key Decisions

1. **`sha-` not `ts-` for the values-cluster pin.** Both are the same manifest version; the investigation found pinning to `sha-` alone does NOT prevent GHCR pruning. However, `sha-` is self-documenting (identifies the commit) and is the convention the chart already recommends. The durable prevention is the release workflow + retention config (documented), not the tag format.
2. **No code change to `_helpers.tpl` or CI.** CI already emits all 4 tag types correctly. The chart default (`default .Chart.AppVersion .Values.*.image.tag`) is correct — it just needs a release to exist. The gap is operational (no release cut yet), not code.
3. **Scope: fix + document, don't cut the release.** Cutting `v0.1.0` is a maintainer git-tag action, not a code change. The documentation makes the process clear.

---

## Blockers

Cutting the `v0.1.0` release (the durable chart-default fix) requires a maintainer git-tag push — documented but not performed in this PR.

---

## Tests Run

- `python3 -c yaml.safe_load(values-cluster.yaml)` — valid YAML.
- `misspell` on changed files — clean.
- No Go code changed; no build/test needed.

---

## Next Steps

- Maintainer cuts `v0.1.0` per the documented process → chart default resolves without overrides.
- Configure GHCR retention to keep the latest semver-tagged version.

---

## Files Modified

- `values-cluster.yaml` — 4 pins ts-1781285219 → sha-ac861c3; updated header comment.
- `charts/llmsafespaces/README.md` — added "Image tags" section + quick-install callout.
- `worklogs/NNNN_2026-06-30_chart-image-tag-release-workflow.md` — this worklog.
