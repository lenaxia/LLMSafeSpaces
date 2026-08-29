# Worklog: platform overlay delivery design (#1116)

**Date:** 2026-08-28
**Session:** User identified that workspace images tied to the platform version don't scale and asked whether platform artifacts could move to an agentd-style injection so the only user-facing version axis is the base OS. Design session → decision (two digest-pinned overlay artifacts) → `design/0053` + issue #1116.
**Status:** Complete (design; no code)

---

## Objective

Decouple platform-contract artifacts from `runtimes/base` so the base version axis becomes pure OS content (bookworm), extending the #863 agentd image-volume mechanism to cover everything the platform versions.

## Work completed

- Verified the coupling: base bakes agentd/redact/entrypoints/opencode and is tagged with the platform `VERSION` (`release.yml:1324`); `MinBaseVersion` (`dockerfile.go:25`) exists only because baked agentd can drift from the API contract (2026-08-25 incident, #871).
- Verified the existing injection seams: #863 overlay delivery (digest-pinned image volume, per-arch sha256 OCI annotations, ConfigMap pin cache), design 0051 sidecar mode (baked entrypoint already bypassed, supervisor owns spawn), agentd subcommand dispatch (`main.go:82-112`), `redact` consumers are platform-owned paths only.
- Decision with user: **two separate digest-pinned artifacts** (existing `agentdDelivery` + new `opencodeDelivery`), not one bundle — independent rollback/cadence, auditable validation coordinate, Epic 65 alignment. **No legacy mode** (no customers): pins mandatory, baked fallbacks deleted, `MinBaseVersion` + factory `ENTRYPOINT` removed, base re-versioned as dev-OS content.
- `redact` folds into agentd as a subcommand (preserves the one-file-one-hash contract); supervisor provides the PATH wrapper.
- Wrote `design/0053_2026-08-28_platform-overlay-delivery.md` (10 decisions, 5-story rollout, risks incl. resume-path pull cost + gVisor image-volume gap, assumption table with evidence).
- Filed #1116 with the proposal + work breakdown.

## Assumptions validated

- "redact has no external consumers" — grep across repo: entrypoint, PATH wrappers, smoke-test only.
- "base version == platform version" — release.yml tags `base:${VERSION}`; catalog rows 0.8.0/0.15.7 match platform tags.

## Not done / next

- No branch or commit made (design doc + worklog left in working tree on `feat/task-model-docs`; files are uncommitted for the user to review).
- Implementation stories S1–S5 (design/0053 §7) unstarted; `gh` could not authenticate earlier in the session but the token was provided and the issue was filed as #1116.
