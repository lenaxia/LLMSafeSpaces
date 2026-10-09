# 1104 — #1601 mechanical disk-space recovery (API + agentd + frontend)

Day 1 open. Charter: out-of-band "free up disk space" lever that works when
opencode is wedged at 91-99% disk — the exact condition where the agent-run
`go clean` remedy fails.

## Issue + ruling read in full

- Issue #1601: three surfaces — (1) API endpoint removing KNOWN-REPRODUCIBLE
  artifacts only (build caches per runtime, test residue, regenerable tooling
  output), never user data/logs/sensitive; DENY-BY-DEFAULT explicit allowlist;
  (2) "free ENOUGH" semantics — reclaim until below ~85% target, idempotent,
  report (path classes + bytes); (3) frontend button auto-surfaced >95%, on
  demand below, dry-run first render.
- Owner ruling (issue comment, 2026-09-29): **agentd hosts the API endpoint.**
  Deletion is a metadata op — works at 100% full. agentd's request/response
  path is write-free on the full volume (memory-backed emptyDir state, stdout
  logs). What dies at disk-full is opencode (PVC DB) — the thing being
  rescued. agentd-down is an availability problem, out of scope.
  Design implication honored throughout: **no temp files, no report
  persistence, response-only.**

## Architecture findings (verified against source, not assumed)

1. **PVC topology** (controller/internal/workspace/pod_builder.go:209-221,
   platform_env.go:50-64): one PVC "workspace" mounted at
   - `/workspace` (subpath `workspace`)
   - `/home/sandbox` (subpath `home`) → `$HOME` (`useradd -u 1000 -m sandbox`,
     runAsUser 1000) — home IS PVC
   - `/tmp` (subpath `tmp`) — /tmp IS PVC
   Platform env: `GOPATH=/workspace/.local/share/go`,
   `CARGO_HOME=/workspace/.local/share/cargo`,
   `MISE_DATA_DIR=/workspace/.local/share/mise`, `NPM_CONFIG_PREFIX=/workspace/.local`,
   `PYTHONUSERBASE=/workspace/.local`. GOCACHE/GOMODCACHE unset → Go defaults
   (`$HOME/.cache/go-build`, `$GOPATH/pkg/mod`). npm cache → `$HOME/.npm`;
   pip → `$HOME/.cache/pip`; pnpm store → `$HOME/.local/share/pnpm/store`.

2. **Sidecar mode split** (agentd_sidecar.go:202-221): the agentd sidecar
   (uid 2000) mounts `/workspace` **READ-ONLY** and does NOT mount
   `/home/sandbox` or `/tmp`. The supervisor (`supervise-opencode`, PID 1 of
   the workspace container, uid 1000) has the full RW filesystem view. Same
   conclusion as the legacy-key scrub before me: execution belongs where the
   filesystem view is.

3. **Existing plumbing to reuse**:
   - User mux (port 4097) handlers gated by `checkBasicAuthAny(agentdPassword,
     workspacePassword)` — the §D1 pair (user_timezone.go pattern).
   - Control socket v1 (control_socket.go): closed method enum, one JSON
     req/resp per TCP conn, supervisor-side server, sidecar-side client.
     Disk metrics already cross it (`metrics`).
   - API facade pattern (agent_reload.go): owner authz via
     `workspaceSvc.GetWorkspace(ctx, userID, workspaceID)` (userID-scoped =
     owner-only), phase gate, pod resolver, workspace password, dispatch to
     `http://<podIP>:4097/v1/...`, route on idGroup (AuthMiddleware +
     WorkspaceAccessMiddleware), openapi contract test + sdks/openapi.yaml.
   - Disk usage source for the frontend: statusz `disk` → controller deep-status
     poll → Workspace CRD status → GET /workspaces/:id/status →
     ChatPage `status.diskUsedBytes` → DiskUsageBar. Threshold single source:
     pkg/agent/systemnotices (warn 0.90 / crit 0.95) — the nudge this feature
     mechanically rescues; after a successful recovery drops below 0.90 the
     nudge stops injecting with zero changes (it reads live usage).

## Design (settled)

**Manifest — compiled-in, per-runtime-base, deny-by-default**
(pkg/diskrecovery/manifest.go). Keyed by runtime base name (today: one base,
`opencode` — runtimes/base → runtimes/opencode; the platform owns the images,
adding a base = editing the manifest in-repo, versioned with the executor).
NO runtime-loadable manifest: any PVC- or env-resident allowlist would let
user-space code add paths — compiled-in is the security property, not an
inconvenience. Env (`LLMSAFESPACE_RUNTIME_BASE`) may only SELECT among
compiled-in bases; unknown base → empty manifest → fail-safe no-op report.

Entries (each cache-by-construction, literals matching the controller-injected
platform env):
- `go-build-cache` `/home/sandbox/.cache/go-build`
- `go-module-cache` `/workspace/.local/share/go/pkg/mod` (= `go clean -modcache`)
- `npm-cache` `/home/sandbox/.npm`
- `pip-cache` `/home/sandbox/.cache/pip`
- `pnpm-store` `/home/sandbox/.local/share/pnpm/store`
- `cargo-download-cache` `/workspace/.local/share/cargo/registry/cache` (NOT
  CARGO_HOME itself — `bin/` is user-installed tooling)
- `mise-download-cache` `/workspace/.local/share/mise/downloads` (NOT
  MISE_DATA_DIR — `installs/` is the user's toolchain)
- `tmp-build-residue` `/tmp/go-build` PREFIX class, min-age 120s (corpse rule;
  pattern fixed in the manifest, never arbitrary /tmp)

**Boundary validator** (runs at execution time even though the manifest is
compiled-in — defense vs future manifest edits + symlinked cache dirs):
candidate must resolve (EvalSymlinks deepest existing ancestor) strictly
inside a declared cache root; must not be/contain a protected root
(`/`, `/workspace`, `/home`, `/home/sandbox`, `/tmp`, `/workspace/.local`,
`/workspace/.local/share`, the package-home roots). An allowlist entry that
would touch a user path (== or ancestor of a protected root, or outside the
cache roots, or symlink-escaping) is REFUSED per-class, reported with reason,
never silently skipped. These refusals get red-first pins.

**Engine** (pkg/diskrecovery/engine.go): statfs `/workspace` → if ratio
already ≤ target: idempotent no-op report. Else measure each class
(read-only walk; age filter for the /tmp class), sort desc, delete
whole-class (os.RemoveAll on the validated path / age-matched children),
re-statfs after each class, stop at ratio < target ("free ENOUGH", not
maximal). Dry-run = measure + report, zero mutation. All ops bounded by ctx;
writes = unlinks only (no temp files, no report persistence — owner ruling).

**Execution topology**: user-mux `POST /v1/disk-recover`
{"dryRun":bool,"targetRatio":float} (§D1 gate, bounded body).
- single-container mode: engine in-process (agentd is PID 1, full FS view).
- sidecar mode: forwarded over control socket (`disk_recover` method added to
  the closed v1 enum; supervisor executes as uid 1000 with the RW view).
  Conn deadline re-armed like upload_apply (10s blanket is too tight for a
  bounded sweep of a huge modcache).

**API facade**: `POST /api/v1/workspaces/:id/disk-recover?dryRun=true` —
owner authz (GetWorkspace userID-scope), Active-phase gate, dispatch to
agentd with workspace password Basic auth, typed report relayed (decode →
re-encode; no raw passthrough). openapi.yaml + contract test.

**Frontend**: `DiskRecoveryStrip` in ChatPage under DiskUsageBar.
- ≥95% (systemnotices critical threshold — the single source; frontend
  constant 95 with comment): warning banner, first render = dry-run report
  ("Review what can be freed"), then "Free now" executes and invalidates
  ["workspace-status", id].
- <95%: subtle on-demand action in the same metrics area.
- Report render: per-class rows (class, bytes, status) + before/after %.

## Plan

1. pkg/diskrecovery: types + manifest + validator + engine, red-first tests
   (refusal pins, symlink-escape pin, dry-run-no-mutation pin, free-enough
   stop pin, idempotency pin, age-filter pin). Verify failing first, then
   implement.
2. agentd: disk_recover.go handler + control-socket method + client + wiring
   (serverDeps seam mirroring sys metrics) + tests (auth gate, both modes).
3. API: disk_recover.go handler + router + openapi + tests (owner authz,
   phase gate, dispatch, report relay, dryRun param).
4. Frontend: workspaces.ts recoverDisk + DiskRecoveryStrip + ChatPage wiring
   + vitest (sessions/workspace-status queries route-mocked per convention).
5. CHANGELOG (Unreleased), worklog updates with run-measure counts.
6. Gates: go test (count-verified), vet, golangci-lint, goimports, helm
   chart test (PATH=/tmp/opencode/bin:$PATH — silent-skip hazard), frontend
   npm ci + vitest + build. Then push, poll AI review on cadence.

Status: architecture recon complete, no code yet. Next: step 1 red-first.
