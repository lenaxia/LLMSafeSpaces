# Worklog: floating-tag default image — root cause and durable fix

**Date:** 2026-08-15
**PR:** TBD

---

## Context

Incident: workspace `af572165` (chat session `ses_ffcba854…`) reported the
agentd MCP server unavailable. Investigation chain:

1. Live pod's `/sandbox-runtime/agent-config.json` injected
   `mcp.llmsafespaces.url = http://127.0.0.1:4098/v1/mcp` (admin mux) —
   `mcpHandler` only listens on the user mux `:4097`. Live probe: `:4097`
   → 200, `:4098` → 404. This is the pre-existing bug fixed upstream in
   `b3a72720` (PR #725, "MCP injection port fix 4098→4097 — pre-existing
   bug affecting all MCP tools"), first shipped in v0.14.0.
2. The pod ran `base` image v0.13.0 (digest `c5c6ca89…`, built Aug 10)
   despite `imagePullPolicy: Always` and pods created Aug 14 17:58–20:39,
   after upstream `latest` had moved to v0.15.5 (`b780b1c9…`, Aug 14
   ~16:00 UTC).
3. Reproduced live: `kubectl run --image=ghcr.io/lenaxia/llmsafespaces/base:latest`
   still resolved to the stale v0.13.0 digest ("1.27 GB pulled in 204 ms").
4. Why: the cluster (talos-ops-prod) runs spegel; containerd's
   `_default/hosts.toml` routes **every** registry through spegel peers
   only (`29999` + NodePort `30021`, `resolve+pull`, no direct-ghcr
   fallback). Spegel resolves floating tags from peer node stores —
   frozen at whatever digest the nodes cached.
5. Why workspaces used `latest` at all: the settings schema seeded
   `workspace.defaultImage = ghcr.io/lenaxia/llmsafespaces/base:latest`
   (tier-3 platform default in the launch hierarchy), bypassing the
   Helm chart's pinned `runtimeEnvironments.base.image.tag`
   (→ Chart.AppVersion).

Root cause (platform side): **a floating tag as a code-level default**.
The registry mirror is not a bug — floating tags resolving differently
per puller is expected behavior; depending on them for correctness is not.

## Assumptions stated and validated

1. `resolveDefaultRuntime` tier 4 falls back to `"base"` when
   `workspace.defaultImage` is empty — validated in
   `api/internal/services/workspace/workspace_service.go`
   (`TestCreateWorkspace_EmptyRuntime_StoredRTEDefaultImage_StillUsed`).
2. The chart seeds a `base` RuntimeEnvironment pinned to
   `.Chart.AppVersion` — validated in
   `helm/templates/runtimeenvironment-base.yaml`.
3. Seed only inserts missing keys (`InsertInstanceSettingIfMissing`) —
   validated in `pkg/settings/seed.go`; therefore existing deployments
   keep the stale row until migration `000023` removes it.
4. The admin UX "clear" writes `""` — validated by allowing empty in the
   new validator (empty is the new default).
5. No other setting/chart default ships a floating image tag —
   validated by grep: only `schema.go:91` (fixed). Chart
   `namespace.podSecurityVersion: "latest"` is the Pod Security
   Standards API-version keyword, not an image tag — left as-is.

## Changes

1. `pkg/settings/schema.go` — `workspace.defaultImage` Default
   `""` (fall through to the chart-pinned base RTE); new declarative
   `SettingDef.RejectMutableTags` field; `SchemaVersion` 10 → 11
   (default change + schema-response shape change, documented in the
   version comment).
2. `pkg/settings/image_ref.go` (new) — `validateImageRefPinned` +
   exported `ValidateImageRefPinned`: rejects known-mutable tags
   (`latest`, `main`, `master`, `dev`, `edge`, `nightly`, case-folded),
   untagged image refs (implicit `:latest`), malformed digests, embedded
   whitespace; accepts digest pins, explicit non-mutable tags
   (semver/`sha-`/`ts-`), RuntimeEnvironment names (no `/`), and empty.
3. `pkg/settings/validate.go` — TypeString branch enforces
   `RejectMutableTags` after pattern.
4. `pkg/settings/normalize.go` — trim whitespace for
   `workspace.defaultImage` (same near-miss policy as the "8gi" fix).
5. `api/.../workspace_service.go` — tier-3 read guard: a stored value
   that fails pin validation is skipped with a `Warn` and the hierarchy
   falls through to the base RTE. Covers rows written before validation
   existed that migration 000023 deliberately preserves (admin-customized).
6. Migration `000023_workspace_default_image_no_float` (canonical +
   chart copy via `make chart-sync-migrations`): deletes
   `instance_settings` rows still equal to the exact seeded default;
   admin-customized values preserved.
7. Frontend: `SettingDef.rejectMutableTags?` typed; `settingsNormalize`
   trims `workspace.defaultImage` (mirrors backend). Enforcement stays
   server-authoritative — no third copy of the tag policy client-side.
8. Tests: `pkg/settings/image_ref_test.go` (22 cases),
   `pkg/settings/default_image_boundary_test.go` (Set accepts/rejects ×
   default fallthrough), `api/.../default_image_read_guard_test.go`
   (stored floating/untagged values not launched; pinned/RTE still used).
   Existing `:latest` fixture values in `workspace_defaults_test.go`
   updated to pinned refs.
9. `docs/reference/crds.md` RTE example repinned; canary N3 comment
   updated to describe the full hierarchy; CHANGELOG entry.

## Adversarial review

- **False positive risk — registries with ports:** handled; tag parsing
  mirrors `parseImageTag` (last `/`, then last `:`), tested
  (`registry.local:5000/team/img:v1.2.3` passes, `:latest` variant
  rejected).
- **False positive risk — bare repo named `latest`:**
  `registry:5000/latest` has no `/` → treated as RTE name → RTE lookup
  fails loudly at create. Acceptable (loud failure, not silent staleness).
- **Digest refs:** `@sha256:` requires hex, correct length prefix check;
  `registry:5000/img@sha256:…` covered.
- **Case sensitivity:** registry tags are case-sensitive, but mutable
  conventionally-lowercase tags are matched case-folded (`:Latest`
  rejected) — a tag that *could* be pushed over is treated as mutable.
  Deliberate.
- **Breaking-change surface:** admins using `:dev`-style tags for the
  platform default now get a validation error. Intentional — this is the
  bug class. Immutable CI tags (`sha-`, `ts-`) remain valid. The
  Workspace-CRD webhook was deliberately NOT extended to `spec.runtime`:
  blocking floating tags there would break UPDATEs of pre-existing CRs;
  that's an operator policy decision, not a platform invariant.
- **Migration is not reversible in general:** `down` re-inserts the old
  floating default only when the row is missing; admin-customized rows
  were never touched, so nothing of theirs is lost. Rollback restores
  the pre-fix behavior for unmodified deployments — as a rollback must.
- **Why not fix spegel instead?** Mirror-tag behavior is expected
  distributed-cache semantics. Removing the dependency on floating tags
  is the platform-side fix. Infra follow-up (out of repo, filed below):
  talos-ops-prod may add a direct-ghcr fallback for digest-pinned pulls.

## Verification

- `go test ./pkg/settings/...` — pass (incl. 22-case validator table).
- `go test ./api/internal/services/workspace/ -run TestCreateWorkspace` — pass.
- `make chart-sync-migrations` + `./bin/repolint` — pass (migration
  numbering, chart parity).
- `go build ./...` — clean.
- Frontend: `tsc` + vitest for touched files — pass.
- Cluster validation (this deployment): `workspace.defaultImage` cleared
  in UX; new-workspace creation path returns `spec.runtime: base` →
  resolves to the chart-pinned RTE (0.15.5).

## Follow-ups

- Patch the three active Workspace CRs still carrying
  `spec.runtime: ghcr.io/lenaxia/llmsafespaces/base:latest` (operator
  action on this cluster — they predate the fix).
- talos-ops-prod (infra repo): consider a per-registry `ghcr.io`
  hosts.toml entry bypassing the spegel `_default` mirror for
  digest/semver-pinned pulls, keeping spegel for layers.
