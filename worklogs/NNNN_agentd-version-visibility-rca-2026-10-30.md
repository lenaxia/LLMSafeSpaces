# NNNN — Fleet currency: workspace agentd version visibility + skew detection (RC-A)

Task `tsk-linear-vignette-dines` (epic: Liveness and observation, S0).
Branch `agentd-version-visibility-skew`.

## The problem (root cause RC-A, 2026-10-30 incident)

Workspace-resident agentds do NOT roll with fleet releases — by design
(#1746 owner ruling 2026-10-19: the user owns when their sessions are
interrupted; agentd updates apply at the next natural pod recreation).
The incident: the pod ran agentd v0.44.7 (commit `6ce1859f`, built
Oct 29 02:08) while #1986's spawn fix had shipped in v0.44.9 (merged
Oct 30 02:23) — the fix existed ~15 hours but never reached the
workspace, and discovering the skew required manually curling
`localhost:4098/v1/healthz` (admin port 4098), which already carried
commit_sha/build_time/uptime.

An N-1 fleet is the product posture; an INVISIBLE N-1 fleet is the
bug. Nothing API-side recorded WHAT the running agentd was (the #1746
fields are digests — pod-build-time identity, not the running
binary's self-report), and nothing compared it against the fleet
release.

## What was built

Rides the controller's EXISTING 15s liveness poll of that same healthz
endpoint (`checkAgentHealth`) — no new scrape, no agentd change
(healthz carried `version`/`commit_sha`/`build_time` since the
2026-08-15 traceability fix).

1. **Visibility** — `mirrorAgentdVersion`
   (`controller/internal/workspace/agentd_version_visibility.go`)
   mirrors the running agentd's self-reported build identity onto the
   Workspace CRD: `status.agentdVersion` / `agentdCommit` /
   `agentdBuildTime`, then through the API's
   `GET /workspaces/{id}/status` (`WorkspaceStatusResult`). Evidence
   rules copied from the #1760 supervisor slice: mirrored on ANY
   decoded response (identity is a property of the binary that
   answered, not of its health); cleared on unreachable/undecodable
   (a dead pod's agentd is unknown, not carried forward); an older
   runtime omitting a field keeps the prior value (mixed-fleet W15).

2. **Skew detection** — the reported version vs `FleetVersion`, a new
   reconciler field wired from `pkg/version.Version` in
   `SetupControllers` (the chart appVersion — the release pipeline
   stamps the controller and every agentd with the same version; the
   task's stated comparison source). `compareVersions` compares
   numeric dot segments left-to-right (the incident pair 0.44.7 vs
   0.44.11 — 7 < 11 numerically where lexical string compare says the
   opposite), tolerates a `v` prefix, ignores prerelease/build
   metadata, and returns incomparable (not a guess) for
   `unknown`/`dev` on either side. Older ⇒
   - condition `AgentdVersionStale=True` (reasons Stale / Current /
     Unknown; message carries both versions + the runbook pointer),
   - `AgentdVersionStale` Warning event exactly-once per
     (agentd, fleet) version pair — annotation-stamped
     (`llmsafespaces.dev/agentd-version-stale-reported`) via the
     mirrorLegacyScrub full-Update-on-fresh-fetch pattern so it
     survives controller restarts,
   - gauge `llmsafespaces_workspace_agentd_version_skew{workspace_id}`
     =1 while stale (latched: an incomparable response cannot flap it
     green) + counter `..._skew_detected_total` once per episode,
   - a controller log line on each new skew pair,
   - chart alert `LLMSafeSpacesAgentdVersionSkew` (warning, `for: 2h`:
     hours of N-1 is the OnRefresh posture working; 2h+ persistence
     means the runbook has not been run).
   Never gates health, liveness, or restarts. Agentd NEWER than the
   controller (a Renovate digest bump between releases) reads as
   current — ahead is not this failure class.

3. **Roll runbook** — `docs/operator/agentd-delivery.md` § "Version
   visibility & skew detection (RC-A)": detection is automatic; the
   roll is a choice, by #1746 policy — OnRefresh (default, no touch),
   Rolling (drain-aware polite roll, instance-wide or per-workspace
   one-liners), `forceRoll` (immediate, drain bypassed,
   security-critical). Post-roll convergence checks listed. Digest
   drift (`agentdPendingDigest`) stays the authoritative "pod not
   rebuilt" signal; the version surfaces make it human-readable and
   alertable.

## Files

- `pkg/apis/llmsafespaces/v1/workspace_types.go` — 3 status fields,
  `WorkspaceConditionAgentdVersionStale`, three reasons.
- `helm/crds/workspace.yaml` — schema for the new fields (repolint
  Go↔YAML drift gate green).
- `controller/internal/workspace/agentd_version_visibility.go` — the
  mirror + compare + flags.
- `controller/internal/workspace/health.go` — wiring (call after the
  supervisor mirror; clear on unreachable/undecodable).
- `controller/internal/workspace/reconciler.go` — `FleetVersion`.
- `controller/internal/controller/controller.go` — wire
  `versionpkg.Version`.
- `controller/internal/metrics/metrics.go` — gauge + counter,
  registered.
- `pkg/types/workspace.go` + `api/internal/services/workspace/workspace_service.go`
  — API surface.
- `helm/templates/prometheus-rules.yaml` — the alert.
- `helm/chart_test.go` — alert-name pin.
- Tests: `controller/internal/workspace/agentd_version_visibility_test.go`,
  `api/internal/services/workspace/agentd_version_visibility_rca_test.go`.
- Docs: `docs/operator/agentd-delivery.md`; CHANGELOG `[Unreleased]`.

## Evidence

- `go test ./controller/internal/workspace/ -run 'TestCompareVersions|TestMirrorAgentdVersion|TestCheckAgentHealth_MirrorsAgentdVersionAndSkew|TestCheckAgentHealth_UnreachableClearsAgentdVersion' -count=1` — ok.
- `go test ./controller/internal/workspace/ ./controller/internal/metrics/ ./pkg/apis/... ./pkg/repolint/ -count=1` — ok (full workspace package incl. envtest-free suites).
- `go test ./api/internal/services/workspace/ -run TestGetWorkspaceStatus -count=1` — ok.
- `make fmt-check` — green (see CI).

## Deliberate non-goals

- No automated fleet-wide roll: the #1746 ruling stands; this task's
  contract was DETECTION must be automatic, roll documented — met.
- No agentd-side change: healthz already reports the identity; the
  2026-08-15 traceability work did that half.
- No `board_health` MCP change: the workspace status API read is the
  operator surface; board_health is board-lease domain.
- Prerelease-vs-release compares equal (semver would order rc below
  release): the release pipeline never stamps prereleases into fleet
  deploys; documented in the doc section.
