# Worklog NNNN — Image Factory: Production readiness

**Date:** 2026-08-02
**Scope:** Closes the gap between "feature-complete" and "production-ready":
catalog seed, real BuildDispatcher, Helm chart wiring.

## Summary

Added the three integration pieces that were blocking production use:

1. **catalog.seed.yaml + seed loader** — embedded YAML with initial bases
   (bookworm) + 9 extensions (ffmpeg, libgl1, python313, bun, etc.).
   Loaded at API boot via `imagefactory.SeedCatalog()`. Idempotent:
   existing extensions are NOT overwritten (runtime admin changes win).

2. **ghActionsDispatcher** — production `buildDispatcher` implementation
   calling the GitHub Actions `workflow_dispatch` API. Wired in app.go
   when `imageFactory.ghDispatcher.apiToken` is non-empty. Without the
   token, builds return 503 (graceful degradation).

3. **Helm chart wiring** — `imageFactory.*` values in values.yaml +
   configmap-api.yaml template. Operators configure the dispatcher token,
   repo owner/name, and LLM explainer endpoint via Helm values.

## Bug fixed

Base struct had JSON tags but no YAML tags. yaml.v3 lowercases Go field
names (`IsDefault` → `isdefault`) but the seed YAML uses camelCase
(`isDefault`). Added `yaml:"isDefault"` (and all other fields) to Base.

## What's now possible

With `ghDispatcher.apiToken` set in Helm values:
- POST /configs dispatches a real GH Actions build
- The workflow builds, pushes, and calls back
- The callback transitions the config to ready/rejected
- Users see status pills in the settings page
