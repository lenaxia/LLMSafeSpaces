# Changelog

All notable changes to LLMSafeSpaces are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.16.1] - 2026-08-20

### Fixed

- **Session load took 10–15s on large transcripts (#971, #973)**: the
  first history page now uses opencode's native `?limit=N` (measured
  live: 26ms vs 1.8s for the full-transcript fetch+decode the handler
  previously performed per page — ~95% of the hot path was decoding
  messages it then discarded). Back-pagination keeps the full-fetch
  path (opencode 1.18.10's cursor params are unusable — verified live:
  `before=` 400s every shape, `cursor=` is ignored). The
  `X-Next-Cursor` contract is unchanged.
- **D3 follow-ups (#960→#964)**: queue retry wired server-side
  (POST /queue/:id/retry re-arms the same entry) and a pre-redelivery
  transcript check that collapses the dominant duplicate-turn source.

## [0.16.0] - 2026-08-20

### Added

- **D3 durable prompts (#907, #943)**: Valkey-backed outbox with
  accept-then-202 semantics and detached delivery — prompt submissions
  survive API restarts and deliver when the consumer is ready (api +
  frontend).
- **repolint ForbiddenPathsCheck (#854, #959)**: live guard against
  resurrection of deleted filesystem trees (the c9c68684-class stale
  merge), wired into pre-commit + CI.

### Fixed

- **Dev preview served stale HTML through the CDN/browser chain (#957,
  epic-66 P0-1)**: the API edge now forces `Cache-Control: no-store` on
  `text/html` responses — the chain had been observed serving stale
  previews across dev-server changes, sending developers debugging code
  that never reached the browser. Non-HTML responses keep app-set caching
  so hashed bundles still cache.
- **Dev preview stripped WebSocket upgrades — HMR broken end-to-end
  (#958, epic-66 P0-2)**: the G34 header wipe in the dev-preview proxy
  discarded `Connection`/`Upgrade` (set by ReverseProxy before Rewrite
  runs) and the allowlist omitted the `Sec-WebSocket-*` handshake family,
  degrading WS handshakes to plain GETs at dev servers. Upgrade headers
  are now re-established after the wipe; 101 responses skip the
  response-size body wrapper (upgrade streams are bidirectional and
  unbounded). Full WS echo round-trips pinned by tests on both the API
  edge and the agentd hop.
- **agentd fail-closed boot (#934, D5.2/D5.3)**: an admin token is now
  required at boot (no silent tokenless mode) and an empty workspace
  password is rejected; the boot-resolved token threads through to the
  muxes.
- **Distinct admin-mux token (#933)**: agentd's admin mux authenticates
  with its own file-only-delivered token (env scrubbed of the old
  secret), with a mixed-fleet bearer fallback for upgrade ordering.

### Changed

- **Legacy per-language runtime images deleted (#854, #959)**:
  `runtimes/{go,nodejs,python}` were dead (built by no pipeline since
  Epic 7; accidentally resurrected by a stale-branch merge). Tenant
  toolchains are managed by mise (the tenant runtime manager) in the
  base image. Docs now describe the one-image reality.

## [0.15.12] - 2026-08-19

### Fixed

- **Dev preview could not be enabled on any stock deployment (#946, #948)**:
  the three Epic 66 instance settings
  (`devPreview.enabled` / `maxResponseBytes` / `maxConnsPerWorkspace`) were
  registered in `KnownKeys` only — unreachable through every consumer: no
  admin-UI section (the settings page is schema-driven), a boot-time
  `GetBool` index-miss whose error was swallowed (hard-disable regardless
  of DB state), and admin PUT rejected as unknown key. The keys are now
  registered in `InstanceSettings()` (SchemaVersion 11→12, canary twins in
  lockstep), the app.go read is extracted into
  `devPreviewConfigFromSettings` with warn + typed-default fallback, and
  the wiring is pinned by schema/service/app-wiring/HTTP-level tests
  (red→green verified).
- **lib/pq dropped (#945)**: GO-2026-6166..6173 have no fixed upstream
  release; the API no longer depends on lib/pq (pgarray migration).
- **Relay `/provider` hang (#927, #911)**: the response-header phase is
  now bounded on both relay-router and relay-proxy — an unresponsive
  upstream provider no longer hangs the endpoint indefinitely.
- **agentd live-validation findings (#926)**: overlay verification is
  pod-gated and startup logs are visible.
- **image-factory 409 name collisions + one-call default move (#950,
  #936)**: creating a config with a colliding name returns a typed 409
  instead of an unhandled error, default moves in one call, and seeding
  preserves the runtime default. Migration 000025 (single default).
- **CI flake (#920)**: agentd watchdog/healthz tests no longer time out
  under `-race -coverpkg`.

### Added

- **Epic 65 wire seam + ContextUsage contract (#938)**: adapter-mediated
  usage persistence — the platform's context/usage reading now flows
  through the `pkg/agent` adapter seam instead of opencode-shaped parsing
  in platform code.
- **Usage + metering decode migrated onto the wire seam (#949, #939)**:
  the SSE tracker's metering decode consumes the adapter contract;
  `MeteringFromEvent` log-before-err obligation documented on the
  interface.
- **Image-factory base-update (#928 v1+v2: #930, #931; ruling #929)**:
  read-side staleness signal (base-update pill in the launch picker) and
  the refresh flow; the base image is the version axis.
- **G7 stress harness (#924, #907)**: watchdog/restart/turn assertions
  under CPU storm.

## [0.15.11] - 2026-08-17

### Fixed

- **All-sessions 502 on chat (hotfix, #917)**: the per-prompt model
  selector was sent to opencode's POST /session/:id/message as the
  "providerID/modelID" STRING, but opencode 1.18.10's schema requires the
  OBJECT `{"modelID","providerID"}` — every prompt 400'd and surfaced as
  a 502 (reported as a CORS failure in the browser, since the error page
  carries no CORS headers). Regression from #909, where mocked tests
  asserted the string form with no real-schema validation. The adapter
  (V1 Send and V2 prompt) now sends the object; the test fake validates
  the body against opencode's schema so a string-form revert fails the
  suite structurally, and a full-pipeline e2e pins the object through
  handler → adapter → backend.

## [0.15.10] - 2026-08-17

### Fixed

- **MCP surface** (#880/#905): `credential_list` decoded the API's
  `{"secrets":[...]}` wrapper incorrectly — every MCP call failed; the
  `workspace_create` tool schema advertised `name` as optional while the
  API 422s on empty names.
- **SSE observability** (#901/#906): per-workspace tracker state
  (`connected` — initialized at arm time, deleted on stop —
  `reconnects_total`, `last_event_age_seconds`, `pod_ip_unavailable_total`),
  drop-accurate delivered-events counter, admin-auth-gated pprof with a
  self-contained mux, open/close lifecycle logs on both SSE endpoints
  (heartbeat frames excluded from event counts), `relay_free_models` in
  statusz with all terminal paths driving it, seven alert rules —
  including the incident-shaped `SSETrackerWatchesZero` and the
  replica-aware (`min by`) `SSETrackerWatchFailing` — every expression
  executed by promtool rule tests in CI. Dead SSE-subscription backoff
  reset fixed (healthy long-lived connections kept the maxed 30s retry).
- **SDK canary**: tools contract now a stable named subset + ≥24 floor
  with a cross-module parity guard, canary-module unit tests in CI, and
  `CANARY_NO_CONTROLLER=1` environment truth replacing phase inference.

### Added

- Observability for the event-delivery pipeline (#901, #902 follow-ups):
  per-workspace `llmsafespaces_sse_tracker_connected` /
  `..._reconnects_total` / `..._last_event_age_seconds` /
  `..._pod_ip_unavailable_total` gauges and counters;
  `llmsafespaces_sse_broker_delivered_events_total` (drop-accurate);
  admin-auth-gated `/api/v1/admin/debug/pprof/*` (self-contained mux);
  open/close lifecycle logs on both SSE endpoints;
  `relay_free_models` in agentd statusz (free-tier routing degradation
  visible); seven alert rules incl. SSETrackerWatchesZero (critical) and
  SSETrackerWatchFailing (armed-but-not-connected paging).
- Fixed dead SSE-subscription backoff reset: `connectAndRead` always
  returns non-nil, so healthy long-lived connections that ended kept the
  maxed 30s retry backoff forever.
- Fixed MCP `credential_list` for all clients (decoded the API's
  `{"secrets":[...]}` wrapper incorrectly → every call failed) and made
  the `workspace_create` tool schema require `name` (#880).
- SDK canary: tools contract now a stable named subset + ≥24 floor with
  cross-module parity guard (pkg/repolint) and canary-module unit tests
  in CI; `CANARY_NO_CONTROLLER=1` env truth replaces phase inference.

## [0.15.9] - 2026-08-16

### Fixed

- **SSE tracker watches never re-armed (#902)** — the halting-sessions
  bug: sends succeeded and turns ran to completion, but client streams
  received zero agent events whenever no API replica held an event
  watch for the workspace. Watches were armed only on phase
  *transitions* into Active; prior-phase state is Redis-backed and
  survives API restarts, so post-restart seeds skipped arming and
  watches that died later (workspace pod churn) had no re-arm path.
  Now: every Active event arms the tracker (idempotent; transitions
  still force a fresh connection), and a 60s reconciler re-arms watches
  for all Active workspaces on every replica — permanently converting
  event-blindness into at most one interval.
- Observability that was missing during the incident: tracker arm/stop
  at Info, tracker disconnects at Warn (was Debug — invisible),
  `llmsafespaces_sse_tracker_watched_workspaces` gauge per API replica
  (0 = that replica's user streams are event-blind). Full gap backlog:
  #901.

### Changed

- `buildAgentd` e2e test timeout 120s → 240s (cold-cache go build under
  `-race` on loaded CI runners exceeded it; spurious suite failures).

## [0.15.8] - 2026-08-16

### Fixed

- **Starvation-proof session truthfulness (#892, design 0050)** — the
  2026-08-15/16 repeated-session-halt incident chain, end to end:
  - **Watchdog demotion (D1)**: the health-watchdog's kill set is
    dead-listener-only — TCP dial refused AND supervised pid alive AND
    past the 180s boot grace. Starved (CPU advancing), flat (blocked on
    upstream I/O — alive), respawn-window (crash recovery owns it), and
    unknown evidence (probe degraded) all suppress forever, counted in
    `workspace_watchdog_suppressions_total{reason}`. No stand-down;
    unknown no longer fails open to a kill. The max-defer force path
    fires only on corroborated dead-listener evidence.
  - **Orphaned busy-flag reset (D2)**: sessions whose busy state was
    produced by a dead opencode generation — the phantom-busy
    "busy with no progress for 20-30 min" class — clear at every
    generation change; context meters survive. API-side reconcile fixed
    for >55-session workspaces (statusz decode cap 16 KB → 1 MB).
  - **Probe truthfulness (D4)**: `/v1/readyz` answers from agentd
    liveness + a kernel-level TCP check on opencode's port — never
    opencode responsiveness, and lock-free (atomic provider snapshot; a
    concurrent statusz fetch can no longer block it past probe
    timeouts). Probe budgets: readiness 5s/5s/×12, startup 5s/3s/×36
    (180s — covers starved boot-to-listen), liveness 10s/10s/×8 on the
    lock-free healthz.
  - **Elapsed-time badges (D5)**: running tools show a live, coarse
    elapsed time (history and live SSE paths, both opencode wire
    shapes) — live-silent ("42s") vs dead state ("3h") at a glance.
  - **Tool-parallelism caps (D7)**: `GOMAXPROCS` on workspace pods,
    derived from the effective CPU burst limit — build-tool children no
    longer oversubscribe the 2-CPU quota (the incident's CFS-stall
    source). No quota change.
  - Supervisor `stop()` race (hang on pod termination when a child
    crashed during backoff) fixed; restart-reason marker write failures
    now counted in `workspace_restart_marker_write_failures_total`
    (incident: 9 attempted writes, 0 landed, stdout-only).

### Changed

- healthz reports `commit_sha`/`build_time` (build identity; "unknown"
  when unstamped).

## [0.15.7] - 2026-08-15

### Changed

- **`AgentConfigInput` fully describes the agent config sources**. The
  admin system prompt and allowed external directories are now
  first-class `Apply` inputs (`AdminPrompt`, `AllowedDirs`) with the
  same pointer semantics as providers/model/relay/MCP — previously
  they were writer-construction options invisible to the seam contract,
  requiring a writer rebuild to revise. Construction still seeds them
  from the bootstrap side-car files; Apply updates thereafter; replace
  and clear are authoritative over prior renders.

### Fixed

- **Binding any MCP server crash-looped workspace boot.** The injection
  pipeline writes mcp-server metadata per MATERIALIZE-CONTRACT.md with
  native JSON types (`"args": [...]`, `"timeoutMs": 5000`), but the
  materializer's `Secret.Metadata` required `map[string]string` — the
  whole secrets.json parse aborted on the first bound server, taking
  every other secret down with it (Init:Error loop). `Secret` now
  decodes both shapes (complex values carried JSON-encoded as strings),
  and malformed metadata is reported per-entry instead of failing the
  file. Found on v0.15.6 the moment the platform opengist server got an
  auto-apply rule — the first MCP binding this cluster ever had.

## [0.15.6] - 2026-08-15

### Changed

- **Consistent build version injection across every component**. All Go
  binaries (api, controller, workspace-agentd, relay-router, relay-proxy)
  now read their build identity from `pkg/version` — the single source of
  truth — stamped via `-ldflags` in every image build (VERSION/COMMIT_SHA/
  BUILD_TIME). Previously the controller and workspace-agentd used their own
  `"dev"` fallbacks, the API Makefile targeted non-existent linker symbols,
  and release/CI never passed `VERSION`, so production images reported
  `"dev"` regardless of tag. Un-stamped local builds now report
  `"unknown"`. The base runtime healthz, the controller startup log, the
  API `/livez`/`/v1/admin/platform-info`, and the relay healthz
  endpoints all surface the injected semver for tagged releases.

### Fixed

- **Workspace pods could boot with no platform MCP server, system
  prompt, or `/tmp` external-dir approval**. Those blocks are rendered
  only by the agent-config writer's marshal path, and every write
  trigger at boot was conditional (pre-boot relay needs a free-models
  catalog; the relay injector needs a successful fetch; credential
  reload needs user action). When all skipped, opencode read the
  materialize base config `{$schema, provider, model}` and ran without
  the built-in `llmsafespaces` MCP server until the first credential
  reload. agentd now performs one unconditional empty-input `Apply`
  before starting opencode (`ensureBootAgentConfig`), stamping the MCP
  entry, admin prompt, and allowed dirs on every boot. Found while
  verifying the 2026-08-15 stale-image incident on v0.15.5.
- **Writer rebuilds no longer drop user-staged MCP servers** (found in
  review of the above): the config writer captured only
  provider/model/agent/mode from the existing config, so any rebuild
  from a writer with no staged MCP source (boot normalize, pre-boot
  relay, relay injector) silently deleted workspace-bound MCP servers
  written by materialize (Epic 53) until the next credential reload.
  The on-disk `mcp` section is now captured and re-emitted unless a
  staged source supersedes it.
- **Floating-tag default workspace image (`workspace.defaultImage`)**. The
  schema seeded `ghcr.io/lenaxia/llmsafespaces/base:latest` as the platform
  default. Floating tags resolve per-puller — registry mirrors (e.g. spegel)
  serve stale per-node digests while upstream has already moved the tag —
  so new workspaces silently launched week-old images (incident 2026-08-14:
  a v0.13.0 agentd missing the MCP 4098→4097 port fix was served for
  `latest` while upstream had published v0.15.5). Fixes, in depth:
  - Schema default is now empty — new workspaces fall through to the
    `base` RuntimeEnvironment, whose image tag is pinned by the Helm chart
    (`runtimeEnvironments.base.image.tag`, defaulting to the chart
    AppVersion).
  - `workspace.defaultImage` writes are validated: image refs must be
    pinned to an explicit non-mutable tag (`latest`, `main`, `master`,
    `dev`, `edge`, `nightly`, `stable`, `prod`, `current`, `release`
    rejected) or a digest; untagged refs are
    rejected (implicit `:latest`). RuntimeEnvironment names still pass.
  - The workspace-service read path skips stored floating-tag values with
    a warning, so values written before validation existed cannot launch.
  - Migration `000024` (renumbered from 000023 after #734 took that slot) removes the seeded `base:latest` row from existing
    deployments (admin-customized values are preserved).

## [0.15.5] - 2026-08-14

### Fixed

- **Swallowed adapter errors on prompt/delete paths (#817, #851)**.
  `SendMessage`, `SendPromptAsync`, and `DeleteSession` returned bare 502
  responses without logging the underlying adapter error — production
  failures (including the 125-second hangs under investigation in #817)
  were undiagnosable. All three paths now log `adapter failed` with the
  underlying error, matching every other adapter call site. Each branch
  has a regression test.

- **Five nil-request panic sites surfaced by Go 1.26 (#853)**. Go 1.26's
  stricter `net/url` parsing exposed a latent panic class: call sites that
  discarded the `http.NewRequestWithContext` error and called `Do(nil)`.
  Most severe: the workflows engine's PreserveOnFailure cleanup embedded a
  session ID from workspace-agent **output** into a URL — a control
  character would crash the API process (the routine runs in the scheduler,
  outside gin's recovery middleware; tenant-triggerable). Also fixed in
  bulk agent-reload, MCP session list/read, and agentd session cleanup.
  Dedicated regression tests pin every new error branch.

### Changed

- **Go toolchain 1.25.12 → 1.26.6 (#853)**. Fixes 7 Go standard-library
  CVEs (GO-2026-6218, -6091, -6090, -6089, -6088, -5972, -5026) that began
  failing govulncheck on every PR after the OSV database refresh. Verified
  red→green locally and on CI (race detector and short suites green on
  1.26.6). Pins updated across go.mod, 14 workflow pins, 6 Dockerfile base
  images; the runtime base's mise `go@latest` is now pinned. The
  CVE-2026-46600 `.trivyignore` entry is removed — obsolete under 1.26.6.

### Known issues

- #817 root cause (the 125-second prompt hangs) remains open — this
  release ships the logging needed to diagnose it on the next occurrence.
- #854: the tenant Go runtime image still pins Go 1.20.5 (follow-up).

## [0.15.4] - 2026-08-14

### Fixed

- **Snapshot bloat root cause — `git init` in workspace boot (#810, #807)**.
  Every workspace pod ran `git init` + `git config` during initialization,
  creating a `.git` directory on the PVC that grew unbounded. Combined with
  `filediff.Producer` (dead code that was never wired), this caused PVC
  snapshot bloat and Longhorn volume-attach timeouts. The `git init` is
  removed entirely; `buildWorkspaceDirsInit` now only runs `mkdir -p`.

- **Health watchdog for opencode hangs (#807)**. A previously-healthy
  opencode process that later hangs (deadlock, CPU starvation) was
  undetectable — the pod stayed 1/1 Running forever with no restart. The
  new health watchdog fires after 3 consecutive `/global/health` failures
  (post-boot), triggers a restart via the managed-process supervisor, and
  is rate-limited to 3 restarts per 10 minutes. Session-aware deferral
  avoids killing in-flight LLM turns (checks busy state before restarting,
  with a 5-minute max-defer cap). Restart reasons are recorded as markers,
  Prometheus metrics, and structured logs.

- **Structured 503 error responses (#810)**. Both `proxy.go` and
  `proxy_handlers.go` now return `code`/`reason`/`message` fields on 503
  responses. The frontend's `ChatHistoryErrorBanner` surfaces a yellow
  "recovering" state when `reason` is present. All three SDKs (Go, Python,
  TypeScript) raise `ServiceUnavailableError` with structured fields.

- **SSE `agent_died` message consumed (#810)**. The `message` field in
  `agent_died` SSE events was written to the wire but never read by the
  frontend. `ChatPage.tsx` now reads `event.data.message` into
  `agentDiedMessage` state and renders it in the banner.

### Changed

- CI workflows now configure git identity for the review bot (fixes
  "Author identity unknown" errors in pr-review, ai-comment, issue-opened,
  and renovate-analysis workflows).
- `runtimes/base/Dockerfile` pins Python to 3.12.13 (floating `python@3.12`
  resolved to 3.12.14 which has no precompiled binary in mise, breaking
  the from-source build).
- Makefile `release-tag` accepts semver pre-release suffixes (e.g.
  `0.15.4-rc.1`).

## [0.15.4-rc.1] - 2026-08-13

### Fixed

- **SSE tracker billing silent-zero on incomplete events (#751 F1c)**.
  `handleSessionUpdated` silently dropped events with empty model ID,
  zero output tokens, or empty session ID. All failure paths now emit
  warn logs so operators can detect billing drift.

- **SSE tracker cost-as-object silently zero (#751 F1a)**. When cost
  arrived as a JSON object instead of a plain number, the value was
  never extracted — billing stayed at zero. Now extracts the `cost`
  field from object shapes with a plain-number fallback.

- **SSE tracker reconnect race (#751 F3)**. `StopWatching` didn't wait
  for the subscribe goroutine to exit, allowing stale events to
  resurrect cleared billing state. Added `sync.WaitGroup` per workspace;
  `StopWatching` and `Stop` now block until goroutines drain.

- **`sessionStartTime` keying mismatch (#751 F2)**. The start-time map
  was keyed by bare session ID while cleanup used `workspaceID:` prefix
  matching — entries leaked forever. Re-keyed to composite
  `workspaceID:sessionID`.

- **Frontend cold-start busy detection (#752 F4)**. `seedBusy` checked
  `status === "active"` but the backend enum has no `"active"` value
  (`idle/busy/unknown/error/compacting/archived`). Now accepts `"busy"`.

- **Unknown SSE event types silently dropped (#752 F6)**. Both SSE
  handlers (ChatPage + SessionActivityProvider) now log unknown event
  types via `console.debug`, making version drift visible.

- **Wrong workspace evicted on max-active limit (#770)**.
  `enforceMaxActiveWorkspaces` sorted by DB `UpdatedAt` (bumped by any
  row mutation) instead of `LastActivityAt` (CRD annotation written by
  ActivityTracker on real user interaction). An actively-used workspace
  with old `UpdatedAt` could be auto-suspended while a stale one stayed
  running. Now sorts by `LastActivityAt` with `UpdatedAt` fallback for
  pre-US-23.3 workspaces.

- **window.confirm fail-open data loss (#775)**. Two call sites wrapped
  `window.confirm()` in try/catch where the catch block proceeded with
  deletion. In sandboxed iframes, `window.confirm` throws → session or
  workspace deleted without confirmation.

- **All confirm dialogs migrated to accessible ConfirmDialog (#814)**.
  All 14 `window.confirm` call sites replaced with the Radix Dialog-
  based `ConfirmDialog` component via the new `useConfirmDialog` hook.
  Dialogs now render in the DOM, working in sandboxed iframe contexts
  where `window.confirm` is blocked entirely.

- **CI review bot git identity (#813)**. The pr-review workflow failed
  with "Author identity unknown" because `persist-credentials: false`
  left no git identity. Added `git config` step to all 4 opencode
  workflows.

### Changed

- `fetchUserWorkspacePhases` refactored to `fetchUserWorkspaceStates`
  returning both phase and `LastActivityAt` from a single K8s API call
  (zero additional API calls).

## [0.15.3] - 2026-08-13

### Fixed

- **Sessions stuck "busy" after message — TRUE root cause (#805, #806)**.
  The SSE event scanners in both agentd and the API had a 64KB per-line
  buffer. opencode 1.18.10 emits `message.part.updated` events that
  exceed 300KB (patch parts listing thousands of changed files; observed
  370KB in production). When the scanner hit such a line, it failed
  silently and dropped the SSE connection. The tracker then missed the
  subsequent `session.status:idle` event and the session stayed "busy"
  forever. Both scanners raised to 16MB. Regression tests verified to
  fail on revert.

## [0.15.2] - 2026-08-12

### Fixed

- **Frontend crash on empty response bodies (#782)** — the frontend
  `client.ts` unconditionally called `res.json()` for any non-204 success.
  A 202/200 with empty body (e.g., the old prompt endpoint behavior)
  threw "Unexpected end of JSON input". Now reads `text()` first and
  returns `undefined` for empty bodies, matching the TypeScript SDK's
  defensive pattern.

- **Stale sessions accumulate in agentd tracker (#792 Pattern 2 S8)** —
  the `sessionStatusTracker` never pruned sessions from its in-memory
  maps during the 30s `fillGaps` cycle. Deleted sessions stayed forever,
  causing phantom busy counts and incorrect restart gating. The cycle
  now calls `prune(activeIDs)` at the top of each run.

## [0.15.1] - 2026-08-12

### Fixed

- **AbortSession silently broken on opencode 1.18.10 (SEV1)** — the V2
  interrupt endpoint (`POST /api/session/:id/interrupt`) was removed in
  opencode 1.18.10 (the entire `v2/` route group was deleted). On 1.18.10
  the V2 path returns 204 from a catch-all stub but does nothing —
  clicking "Stop" had no effect. Switched to V1 `POST /session/:id/abort`
  which actually stops the in-flight turn.

- **V1 Send response truncated at 4 MB** — same class of silent-
  truncation as #737. A single assistant turn with verbose tool output
  (>4 MB) would truncate, the JSON parse would fail, and the user's
  message would appear to vanish even though the LLM completed.
  Raised to 64 MB.

- **statusz 16 KB decode cap too small** — a workspace with 55+ sessions
  produces a statusz body >16 KB. The decode silently failed and the
  stuck-busy self-heal stopped working for heavy users. Raised to 1 MB.

### Root cause

The entire `v2/` route group directory was deleted from opencode 1.18.10
(`packages/opencode/src/server/routes/instance/httpapi/groups/v2/`).
Both V2 endpoints (`/api/session/:id/prompt` and `/api/session/:id/interrupt`)
are gone — they return 200/204 from a catch-all stub but execute nothing.
Confirmed via source diff of anomalyco/opencode v1.15.12 vs v1.18.10.

## [0.15.0] - 2026-08-12

### Summary

First production-ready release since v0.13.1. Bundles all v0.14.x hotfixes
(Epic 65 adapter migration fallout) plus the stuck-busy ground-truth fix.
Production was intentionally held at v0.13.1 through v0.14.x; this release
is the culmination of the full investigation, root-cause analysis, and fix
verification marathon documented in worklog 0744.

The minor version bump (0.14 → 0.15) is justified by the breaking
`SendPromptAsync` response change (202 → 200 with full assistant message
body) and the switch from V2 queue to V1 synchronous send.

### Fixed

- **Sessions stuck "busy" forever (#792, #795)** — the in-memory
  `activeSess` map retained stale "active" entries when the SSE stream
  dropped or the API pod restarted mid-turn, making sessions appear
  stuck busy indefinitely. `GetAuthoritativeActiveSessions` now queries
  the workspace pod's `/v1/statusz` for ground-truth busy/idle status
  and self-heals stale entries on every session-list request.

- **Dev-preview URL used relative origin (#793, #797)** — dev-preview
  links now use the absolute API origin instead of a relative path that
  broke when the frontend was served from a different host.

### Changed

- **`SendPromptAsync` now returns 200 with the assistant message body**
  (#755) — previously returned 202 (accepted) via V2 queue, which is
  admitted but never drained on opencode 1.18.10. Now uses synchronous
  V1 `POST /session/:id/message` and returns the completed assistant
  message as JSON.

### Testing

- **E2e regression coverage closed for three hotfix gaps (#800)** —
  worklog 0744's audit found that three of the four Epic 65 symptom
  fixes were marked "Code path verified" (non-evidence per Rule 0).
  Added three integration tests that exercise the full gin → handler →
  adapter → HTTP path and are verified to FAIL when the corresponding
  fix is reverted:
  - `TestE2E_Adapter_GetHistory_LargeBodyOver16MiB_No502` (#737)
  - `TestE2E_Adapter_SendPromptAsync_UsesV1SendNotV2Queue` (#755)
  - `TestE2E_Adapter_GetHistory_EmptySession_ReturnsArrayNotNull`

### Included from v0.14.x (previously unreleased to production)

All fixes from v0.14.3 through v0.14.6 are included. See the individual
changelog entries below for details. Key user-facing fixes:

- Messages disappear on send (#755) — V1 synchronous send
- GetHistory 502 on large sessions (#737) — streaming decoder
- Null history crash on new sessions — nil→[] guard
- Sessions stuck busy after LLM finishes — SSE watch on read paths
- opencode 1.18.10 wire-shape drift (#740, #743, #744, #748) — providerID,
  Agent, Status, flat tool parts, summary object, cost tokens
- SSE tracker billing drift + memory leak (#751)
- Secrets rotation restart mid-turn (#753)
- Reasoning content silently dropped (#750)

## [0.14.6] - 2026-08-12

### Fixed

- **GetHistory returns `null` for empty sessions — frontend crash** — when
  opencode returns no messages (new session), the adapter returned a nil
  slice which Go serializes as `null`. The frontend called `.filter()` on
  the response, crashing with "Cannot read properties of null". Fixed to
  return `[]`.

## [0.14.5] - 2026-08-12

### Fixed

- **Sessions stuck "busy" after LLM finishes (#755)** — adapter read
  paths (GetHistory, GetSession, ListSessions) never called
  `adapterEnsureSSEWatch`, so opening a busy session without sending a
  message never started the SSE tracker. The `session.status=idle`
  event was never received and the session appeared stuck busy forever.
  Fixed by adding SSE watch to all three read-path handlers.

## [0.14.4] - 2026-08-12

### Fixed

- **Messages disappear on send — Sev1 (#755, #757)** — `SendPromptAsync` always routed through V2 queue (`delivery:"queue"`) which is admitted but never drained on opencode 1.18.10. Every message from every workspace vanished. Switched to synchronous `adapter.Send` (V1 `POST /session/:id/message`) which works on all versions.

- **GetHistory 502 on large sessions — streaming decoder (#737, #738)** — the 16 MiB `readBody` cap truncated history bodies >16 MiB (94 MB sessions), causing JSON parse failure → 502. Replaced with streaming `json.Decoder` that has no body-size cap and returns partial results on truncation.

- **Reasoning content silently dropped (#750, #757)** — `translate.go` read `p.Reasoning` but opencode 1.18.10 puts reasoning text in `p.Text`. Added fallback.

- **Adapter 10s HTTP timeout broke sync Send (#746, #757)** — the hard `http.Client.Timeout: 10s` covered the entire exchange including body read. Removed; context deadline is the correct boundary.

- **CapDiff falsely advertised (#745, #757)** — `Capabilities()` unconditionally returned `CapDiff` even though filediff is never wired. Made conditional on `a.differ != nil`.

- **SSE tracker billing drift + memory leak (#751, #757)** — `handleSessionUpdated` parsed cost as `float64` (breaks on object shape), provider key only accepted `providerID` not legacy `provider`, and per-session billing maps never cleaned up on `StopWatching`. Fixed all three + data race on `sessionStartTime` mutex.

- **Secrets rotation restart mid-turn (#753, #757)** — agentd session tracker only stored `busy`/`idle`, treating `retry`/`error`/`compacting` as neither (stale idle). Restart decision then fired mid-turn. Fixed to treat all non-idle statuses as busy.

- **agentd parser drift (#747, #757)** — `fetchSessionPromptTokens` had silent zero-return paths with no logging and unbounded body read. Added debug logs + 16 MB `io.LimitReader` cap.

- **MCP server OOM risk (#749, #757)** — `mcpSessionList`/`mcpSessionRead` used unbounded `io.ReadAll` with no status check. Added 4 MB/16 MB caps + status-code validation. Fixed MCP functions to use `getAgentAddr()` for testability.

- **session_index channel drop (#754, #757)** — `RecordMessage` silently dropped events when channel full. Added nil-guarded warn log.

- **opencode 1.18.10 wire-shape drift (#743, #748)** — `providerID` dropped (custom UnmarshalJSON), `Agent` field missing, `Status` latent 502 (polymorphic decoder).

- **V2 bridge: spurious wake + TTL leak + nil-guard (#744, #748)** — busy-session guard in `wakeStrandedV2Sessions`, TTL pruning in `v2PendingSessions`, symmetric nil-guard.

- **ParseSessionWire/ParseSessionListWire 502 on 1.18.10 (#740, #741)** — `Summary` string→object, `Cost` object→bare number, `Time` field name drift. All fixed with `json.RawMessage` + custom unmarshalers.

- **Context usage backfill from CRD (#739, #741)** — `context_used` NULL after API pod restarts. Backfill from CRD status + observability.

- **Adapter cross-cutting regressions (#740, #741)** — all adapter handler paths now enforce workspace readiness, connection limits, session limits, metering, quota, activity tracking.

## [0.14.3] - 2026-08-11

### Fixed

- **Per-session context usage broken after API pod restarts (#739, #741)** —
  `session_index.context_used` stayed NULL for sessions whose last LLM step
  completed during an API pod restart. The SSE tracker reconnects but does
  not replay missed events. `ListWorkspaceSessions` now backfills NULL
  values from the workspace CRD status (which carries fresh per-session
  values from agentd) and persists them to the DB (self-healing). Also adds
  warn logs to `persistContextFromEvent`'s previously-silent no-op paths.

- **Epic 65 adapter cross-cutting regressions (#740, #741)** — every
  `if h.adapter != nil` handler branch bypassed `proxyToWorkspaceWithErrBody`,
  skipping workspace-readiness checks, connection limits, session limits,
  metering, quota enforcement, and activity tracking. Extracted 5
  cross-cutting helpers (`resolveWorkspaceForAdapter`,
  `checkAdapterSessionLimit`, `checkAdapterQuota`, `postAdapterSuccess`,
  `adapterEnsureSSEWatch`) and wired them into SendMessage, SendPromptAsync,
  GetHistory, GetSession, ListSessions, and CreateSession.

- **ParseSessionWire/ParseSessionListWire deterministic 502 on opencode
  1.18.10 (Finding O, #741)** — `ocSession.Summary` was typed as `string`
  but 1.18.10 returns an object; `ocSession.Cost` was `*ocCost` but 1.18.10
  returns a bare number; `ocTime` used `startedAt`/`completedAt` but 1.18.10
  uses `created`/`updated` (epoch ms). All three fixed with `json.RawMessage`
  and custom unmarshalers. Added `ocTokens` struct to extract session-level
  token data.

- **Read-path adapter error paths swallowed errors with no logging (Finding
  N, #741)** — CreateSession, ListSessions, GetSession now log via
  `h.logger.Error` before returning 502, matching the existing GetHistory
  pattern.

- **opencode 1.18.10 wire-shape drift: providerID, Agent, Status (#743,
  #748)** — (1) `ocModelRef.Provider` used JSON key `"provider"` but 1.18.10
  sends `"providerID"` — provider silently dropped for every session.
  Fixed with custom `UnmarshalJSON` accepting both keys. (2) `ocSession`
  had no `Agent` field — `session.AgentID` always empty. Added field +
  mapping. (3) `ocSession.Status` was a required value type — latent 502
  if opencode sends status as a bare string. Changed to `json.RawMessage`
  with polymorphic decoder.

- **V2 session-queue bridge: spurious wake + TTL leak + nil-guard (#744,
  #748)** — (1) `wakeStrandedV2Sessions` had no busy-session guard,
  creating concurrent turns on SSE reconnect. Now skips non-idle sessions.
  (2) In-memory `v2PendingSessions` had no TTL — lost events left entries
  forever. Added `lastAdded` timestamp + prune-on-read matching Redis's
  10-min TTL. (3) Legacy `enqueueV2` path lacked nil-guard on `v2Pending`.

- **Epoch-millis timestamp parsing in translator (#735)** — message
  timestamps from opencode were parsed incorrectly, causing incorrect
  message ordering in the frontend.

- **Dec 31 timestamps — `CreatedAt` JSON tag missing omitempty (#736)** —
  `session.Message.CreatedAt` serialized as `"0001-01-01T00:00:00Z"` when
  unset, causing frontend to show epoch-zero timestamps.

- **Nightly e2e: RBAC scope + Workspace CRD schema + session routes (#742)** —
  fixed e2e test infrastructure for controller RBAC, CRD schema validation,
  and session route mapping.

## [0.14.2] - 2026-08-11

### Changed

- **Session queue is now inboard/durable (US-63.7)** — the external Redis-
  backed message queue (`api/internal/services/msgqueue/`) and its incident-
  scarred workarounds (message-id hack, 409-requeue, destructive abort,
  stranded-queue sweep) have been deleted. The proxy now uses opencode's V2
  session API exclusively (`delivery:"queue"` prompt + non-destructive
  `interrupt`). The `LLMSAFESPACE_V2_SESSION_QUEUE` feature flag is removed;
  V2 is the only path.
  - **Abort is non-destructive:** queued messages survive an abort and drain
    on the next turn.
  - **Dismiss/revoke is removed:** the V2 API has no revoke on admitted-but-
    unpromoted rows. `DELETE /sessions/:sid/queue/:messageId` now removes
    from the shadow marker only (best-effort).
  - **`ForceAbortSession`** (Epic 44 admin escape hatch) is retained.

## [0.14.1] - 2026-08-11

### Fixed

- **Session history 502 on opencode 1.18.10 — flat-string tool shape (#730,
  #731)** — the 0.14.0 GetHistory typed parser declared `ocPart.tool` as a
  JSON object (`*ocTool`) but opencode 1.18.10 emits `"tool"` as a bare
  string (the tool name) with `callID`/`state`/`input`/`output` hoisted to
  the part level. Every `GET /api/v1/workspaces/{wid}/sessions/{sid}/message`
  returned **502 Bad Gateway** for any session containing a tool call —
  effectively every session in every workspace within minutes of creation
  (Sev1 outage, ~24 min, mitigated by rolling the API back to 0.13.1).

  **Root cause:** silent opencode wire-shape assumption drift (README §7).
  The parser was "validated from opencode 1.15.12 binary"; production pods
  run **1.18.10**. The Epic 65 translator tests built `ocPart` values with
  the legacy nested-object shape, so every test passed while production
  502'd. Same failure class as #707 and #486.

  **Fix 1 (parser):** a custom `UnmarshalJSON` on `ocPart` normalizes both
  wire shapes — flat-string (1.18.10+: `tool:"bash"` + part-level
  `callID`/`state.time.{start,end}`) and legacy nested (≤1.15.x) — into the
  canonical `ocTool`. The `session.ToolPart` contract is unchanged.

  **Fix 2 (resilience, README §12 containment):** `ParseHistoryWire` now
  decodes in two stages — `[]json.RawMessage` first (only fails on non-array
  bodies), then per-message. A future opencode schema change in one part
  degrades that single message to a `session.MessageSystem` notice instead
  of 502-ing the whole history. Operators get a signal via the adapter WARN
  log (`downgraded` count + session/workspace context).

  **Regression tests (TDD, golden fixtures as schema pins):**
  `testdata/history_1_18_10_flat_tool.json` (verbatim captured payload) and
  `testdata/history_1_15_12_nested_tool.json` guard both shapes. Six tests
  cover the verbatim shape, legacy non-regression, mixed shapes in one
  history, one-malformed-part no longer killing the page, totally-garbage
  still erroring, and all 9 tool names observed in production. Plus adapter
  integration and two handler e2e tests.

- **WorkspaceImagesTab re-fetch loop (#731)** — `load` had `baseName` in
  its `useCallback` deps but `load` itself sets `baseName` on first run,
  creating a re-fetch loop (`load → setBaseName → load recreated → useEffect
  → load → setLoading(true)`). The component flickered back to the spinner
  after initial load, racing the scope-routing test. Fixed with a `useRef`
  guard so `load` has a stable identity and `useEffect` fires once.

- **Playwright e2e mock shape drift (#731)** — after Epic 65 the GetHistory
  endpoint returns the contract shape (`type`/`createdAt`/`parts`), not
  opencode's raw shape (`info.role`/`time.created`). The e2e mock helpers
  in `composer.spec.ts` and `session-activity.spec.ts` were never updated,
  so `transformHistory` filtered out every message and 7 Playwright tests
  failed on main. Updated mocks to emit contract shape.

- **TypeScript SDK — `baseUrl` visibility (TS2341, #731)** —
  `WorkspacesAPI.devPreviewUrl()` accesses `this.client.baseUrl`, but
  `baseUrl` was declared `private`. The SDK Contract Tests CI job was
  masked by the failing frontend-test dependency; fixing the frontend
  unblocked it and exposed the bug. Changed to `readonly` (package-visible).

- **SDK canary `expectedSchemaVersion` drift (8/4 → 10, #731)** —
  `pkg/settings/schema.go` bumped `SchemaVersion` to 10 but the Go/TS/Python
  SDK canary tests expected 8/4 respectively. The schema-version assertion is
  a SCHEMA DRIFT DETECTOR by design — it caught the gap. Aligned all three
  SDK canaries with the current version.

- **SDK canary login rate-limit exhaustion (#731)** — the `/auth/login`
  endpoint has a per-route rate limit of 10/min (burst 10). Each SDK canary
  suite calls `jwtLogin` per scenario, exhausting the bucket and causing
  spurious 429 failures in cred-crud and other login-bearing scenarios. The
  Go section already had a `sleep 65` before S-RATE-LIMIT; added matching
  waits before the Python and TypeScript sections and a mid-Go wait before
  S-CRED-CRUD.

## [0.14.0] - 2026-08-11

### Added

- **Workspace dev preview — authenticated HTTP/WS tunnel to in-workspace dev
  servers (#725)** — users can now view web applications (Vite, Next.js,
  webpack-dev-server, Playwright, ad-hoc HTTP servers) running inside their
  workspace pod from a browser, with full hot-module-replacement (HMR)
  support. The tunnel is authenticated to the workspace owner only (via the
  existing `lsp_session` cookie or Bearer header) and reuses the existing
  API→pod:4097 NetworkPolicy allowance — zero new ingress rules, no
  per-workspace Service or Ingress objects, no publicly shareable URL.

  **Architecture:** browser → API (`AuthMiddleware` +
  `WorkspaceAccessMiddleware`) → `DevPreviewHandler`
  (`httputil.ReverseProxy`, G34 header allowlist, response size cap,
  connection cap) → agentd:4097 (`devPreviewHandler`, port denylist, Host
  rewrite for CVE-2025-30208) → `localhost:<port>`.

  **Opt-in:** toggle via `PUT /workspaces/:id/dev-preview` or the workspace
  settings drawer. Operator kill-switch via `devPreview.enabled` instance
  setting. Configurable response size cap (default 50 MiB) and per-workspace
  connection cap (default 50).

  **MCP tool:** the in-workspace agent can call `dev_preview_url` to
  construct a preview link for the user, with enable instructions included.
  Fixed a pre-existing bug where the MCP server was injected on the wrong
  port (4098 admin → 4097 user mux), which had prevented all in-workspace
  MCP tools from being reachable.

  **SDKs:** `setDevPreview` + `devPreviewUrl` helpers in all four SDKs
  (TypeScript, Python, Go, Java). OpenAPI spec updated with both routes.

  **Security:** G34 header allowlist applied (caller Cookie/Origin/Referer
  stripped before forwarding to pod). Agentd strips Basic auth before
  forwarding to the dev server. Port denylist (4096/4097/4098 + privileged
  ports <1024) enforced at both API and agentd layers.

### Fixed

- **MCP injection port mismatch (pre-existing)** —
  `injectAgentdMCPServer` injected the MCP server URL as
  `http://127.0.0.1:4098/v1/mcp` (admin port), but `mcpHandler` is
  registered on the user mux (4097). Fixed to `AgentdPort` (4097). This
  had prevented `session_list`, `session_read`, and the new
  `dev_preview_url` tools from being reachable by the in-workspace
  opencode agent.

## [0.13.1] - 2026-08-10

### Fixed

- **Session origin recording for routine triggers (#706)** — the
  `session_origins` table, `RecordSessionOrigin` store method, and sidebar
  badge UI were built during Epic 64 but never wired into the engine.
  `executeRoutine` now calls `RecordSessionOrigin` after success, so
  routine-created sessions show the purple "routine" badge in the sidebar
  and link back to their trigger. Session deletion for `PreserveOnFailure`
  now only suppresses origin recording when the DELETE actually succeeds.
- **Create forms moved to detail pane (#705)** — trigger and workflow
  create forms now open in the detail pane instead of a modal, matching
  the edit flow.

## [0.13.0] - 2026-08-10

### Added

- **Session contract + V2 session queue (#695)** — platform-owned session
  contract (US-65.2/65.6) and V2 session queue (US-63.1→63.4). Decouples
  the platform from opencode via a typed session model behind a single
  adapter seam.
- **SSE bridge + stranded-input recovery (#698)** — US-63.5 SSE bridge
  for reliable streaming and US-63.9 stranded-input recovery for sessions
  interrupted during queue drain.
- **Fresh-load queue visibility (#701)** — Redis-backed shadow marker for
  V2 queue pills on fresh page load.

### Fixed

- **Trigger concurrent-fire race (#700)** — `ListDueCronTriggers` used a
  plain SELECT with no row locking; every API replica's scheduler tick
  fired the same due trigger concurrently, inflating the failure counter
  past `auto_disable_after`. Replaced with `ClaimDueCronTriggers` using
  `FOR UPDATE SKIP LOCKED` + atomic timestamp advance.
- **Trigger counter not reset on re-enable (#700)** — `UpdateTrigger`
  now resets `consecutive_failures` to 0 on the `enabled=false→true`
  transition via a CASE clause.
- **Workspace re-suspend during activation (#696)** — `K8sWorkspaceActivator`
  now refreshes `last-activity-at` when patching `spec.suspend=false`.
- **Stuck-Creating escape hatch + FailedMount auto-recovery (#702)** —
  `spec.suspend=true` now honored in Pending + Creating phases.

## [0.12.1] - 2026-08-09

### Fixed

- **Mobile-responsive drill-down navigation** — workflows and triggers pages
  now show one pane at a time on mobile (list → tap → detail with back button).
  Desktop layout unchanged.
- **Trigger editor routine parity** — full editing support for routine-mode
  triggers (prompt, memory, capture, preserve session).
- **Cron fire/run IDs must be UUIDs** — cron trigger fires were generating
  non-UUID string IDs, causing silent DB insert failures. All IDs now use
  `uuid.New().String()`.

## [0.12.0] - 2026-08-09

### Fixed — Completing items 1-7

- **PreserveOnFailure type comment corrected** — now accurately describes
  the engine calling `DELETE /v1/workflow/session/delete` on success.
- **Session origin API endpoint** — `GET /workspaces/:id/session-origins`
  returns origin mappings for sidebar enrichment. `SessionListItem.Origin`
  field added to the session list response type.
- **agentd MCP server tests** — 10 tests covering initialize, tools/list,
  tools/call (unknown tool, missing args), injection (empty + existing MCP).
- **Active runs indicator on chat page** — shows a pulsing badge with count
  of in-flight workflow runs when the workspace has active runs.
- **Wake-up latency in run detail** — shows the time between run creation
  and run start (wake-up cost) as a yellow indicator.
- **Integration tests run in normal CI** — removed `//go:build integration`
  tag from `store_integration_test.go`. Tests compile in every CI run
  (catching SQL drift) and skip gracefully when PG is unavailable.

## [0.11.0] - 2026-08-09

### Added — Remaining Epic 64 items

- **agentd built-in MCP server** — serves `session_list` and `session_read`
  tools via MCP JSON-RPC protocol at `/v1/mcp`. Injected as a "remote" MCP
  entry in agent-config.json so the in-workspace opencode agent discovers
  it natively. Tools call opencode's local API — no platform credentials
  needed. Enables the agent to read previous session history for
  cross-run continuity (Flow 3 roll-forward).

- **`PreserveOnFailure` session deletion** — the engine now calls
  `DELETE /v1/workflow/session/delete` on agentd when a routine succeeds
  with `preserveSession: "on_failure"`. The session is preserved on
  failure (for investigation) and deleted on success (clean workspace).
  New agentd endpoint: `DELETE /v1/workflow/session/delete?sessionId=X`.

- **Full cron expression support** — replaced the hand-rolled 4-pattern
  parser with `github.com/robfig/cron/v3`. All standard cron expressions
  now work: `30 9,14 * * *`, `0 0 * * 0`, `*/15 8-18 * * 1-5`, etc.

- **Session origin tracking** — migration 000022 adds `session_origins`
  table linking opencode sessions to their creating trigger/workflow.
  Store methods: `RecordSessionOrigin`, `ListSessionOrigins`. (API
  enrichment + frontend icon rendering: next step.)

- **Active runs by workspace** — `GET /api/v1/workspaces/:id/runs/active`
  returns non-terminal runs for a workspace. For the run-active-on-
  workspace indicator.

- **`ListPendingRoutineFires`** integration test — verifies the SQL-level
  webhook-routine flow (fire created → pending → processed → delivered).

- **`GetLastRoutineResult`** integration tests — verifies the memory query
  returns results from 'delivered' fires (not 'fired').

- **Design docs** — `ENGINE-MIGRATION-DESIGN.md` (API→controller migration
  plan, 3-5 day estimate) and `SESSION-OWNERSHIP-DESIGN.md` (unified
  session ownership, 2-3 week estimate).

### Fixed

- **`computeNextFire_Hourly` test corrected** — the old parser was wrong
  (`0 * * * *` at 12:30 returned 13:30 instead of 13:00). robfix/cron
  gives the correct result.

## [0.10.0] - 2026-08-09

### Changed — Breaking: Triggers as Routines

This release redesigns the trigger model. Triggers are no longer limited
to firing DAG workflows or scripts — they directly embody agent routines
(scheduled agent turns with memory, capture, and session lifecycle).

**Breaking changes:**
- `target_type` and `target_config` columns dropped from `triggers` table
  (migration 000020). Replaced with explicit routine fields: `workspace_id`,
  `prompt`, `agent`, `script_path/args/env`, `memory_mode`, `capture_mode`,
  `preserve_session`, `workflow_id`.
- Trigger API: `POST /me/triggers` no longer accepts `targetType` or
  `targetConfig`. Use `workflowId` for DAG mode, or `workspaceId + prompt`
  for routine mode.
- `run_script` target type eliminated. Use a routine trigger with
  `scriptPath` + `prompt` instead.
- Synthetic workflow rows for `run_script` triggers eliminated.
  `BuildRunScriptSpec` and `GetOrCreateScriptWorkflow` deleted.
- SDK trigger create/update signatures changed (all 4 SDKs).
- MCP `trigger_create` tool signature changed.

### Added — Routine execution

- **Routine executor** in the scheduler: activates workspace, renders prompt
  with `{{.prevResult}}` memory, executes optional script, sends prompt to
  opencode agent, captures result per capture policy, deletes or preserves
  session per preserve policy.
- **Memory modes**: `none` (every run independent) or `last_result` (injects
  previous successful result into prompt template). Enables roll-forward
  state for hourly/daily routines without requiring session continuity.
- **Capture modes**: `errors_only` (default — only stores on failure) or
  `full` (always stores the agent response).
- **Session preservation**: `never` (delete after capture), `always` (keep
  session in sidebar), `on_failure` (keep only on error).
- **Migration 000020**: trigger routine fields + backfill from old
  target_type/target_config.
- **Migration 000021**: `trigger_fires.result` column for memory +
  observability. `session_origins` table for session origin tracking.
- **Scheduler dependencies**: scheduler now holds `WorkspaceActivator` and
  `AgentdExecutor` for routine execution.
- **Frontend**: trigger create form rebuilt with routine/workflow mode
  toggle, workspace picker, prompt editor, memory/capture/preserve controls.
- **Design doc**: `design/stories/epic-64-triggers-workflows/ROUTINES-REDESIGN.md`.

### Fixed

- **`run_script` now actually executes.** Previously it just logged
  "delivered" and did nothing. Replaced entirely by the routine executor.
- **Ephemeral session deletion** in agentd `execAgentNode`: sessions marked
  ephemeral are now deleted after response capture (`DELETE /session/:id`).
- **Full transcript capture** in agentd `execAgentNode`: all parts (text +
  tool-use) are captured, not just concatenated text.

## [0.9.1] - 2026-08-08

### Added — Epic 64: Triggers & Workflows UX v2

- **Visual DAG editor** — `@xyflow/react` canvas with typed node palette
  (script/agent/http/condition), drag-to-connect edges, condition-branch
  handles, minimap. Per-node typed edit panels replace raw JSON editing.
  Visual/JSON toggle.

- **Run detail page** — per-node timeline with status icons, attempt counts,
  branch labels, durations. Expandable input/output/error per node. Cancel
  button. Auto-polls while running. Route: `/workflows/:wfId/runs/:runId`.

- **`onMissingWorkspace` policy** — migration `000019`. When a workflow's
  target workspace is gone: `abort` (default, fails run fast) or `create`
  (auto-provisions a new workspace for the owner, pins it as target, waits
  for Active). Includes `WorkspaceCreator` engine interface + app.go wiring.

- **Trigger UX overhaul** — cron frequency builder (every N min/hours, daily,
  weekdays, timezone) with raw-expression toggle. Webhook create flow with
  one-time secret reveal, URL copy, signing example, IP allowlist, idempotency
  mode. Circuit-breaker progress bar + auto-disabled badge + re-enable.

- **Trigger editor** — editable schedule (inline edit with friendly/raw
  toggle), swap target workflow, edit input template. Full update support.

- **`run_script` target** — trigger create form supports both `run_workflow`
  and `run_script` targets (workspace + path + args + env).

- **Workspace picker** — workflow editor has a workspace dropdown and an
  "If missing: abort/create" toggle. Run dialog includes workspace picker
  when no default target set.

- **inputSchema + defaults editors** — Advanced section in workflow editor
  for JSON Schema input validation and node defaults block.

- **Delivery log** — `GET /me/triggers/:id/fires` endpoint. Live-polling
  delivery log panel with expandable rows (envelope + action result).

- **Webhook secret rotation** — `POST /me/triggers/:id/rotate-secret`
  endpoint. One-time reveal of new secret with copy button.

### Fixed

- **Webhook rate limiting wired** — the handler comment claimed rate limiting
  but the code didn't do it. Now calls `RateLimiterService.Allow` per-webhook
  with 429+Retry-After.

- **Hash idempotency implemented** — `computeHashDedupKey(body, ts)` derives
  dedup key from `sha256(body + 5min-window)`. Was a stub returning empty.

## [0.9.0] - 2026-08-08

### Added — Epic 64: Triggers & Workflows

- **Workflow engine** (#655–#686). Deterministic DAG-structured pipelines
  running inside workspace pods, fired by cron/webhook triggers. The engine
  runs in the API server as background goroutines (reconciler + scheduler),
  not the controller — preserving the architectural boundary (controller →
  K8s only, API → PostgreSQL + K8s + HTTP).

- **4 node types**: `script` (Python/Node inline handlers via mise),
  `agent` (named opencode agent with structured output), `http` (outbound
  HTTP through workspace egress), `condition` (expr-lang branch routing).

- **Trigger sources**: `cron` (5-field expression with timezone support)
  and `webhook` (HMAC-SHA256 verified, IP allowlist, idempotency dedup).

- **Single-in-flight enforcement**: partial unique index on
  `workflow_runs(workflow_id) WHERE status IN ('queued','running')` —
  atomically prevents concurrent runs without leader election.

- **Circuit breaker**: triggers auto-disable after N consecutive failures
  (default 10). Missed cron fires are logged as 'skipped', not silent.

- **DAG validator**: 9-pass validation at create/update — cycles, dangling
  edges, unreachable nodes, condition branch coverage, expr-lang type-check.

- **Migration 000016**: 7 tables (workflows, triggers, webhooks,
  webhook_deliveries, workflow_runs, workflow_node_runs, trigger_fires).

- **11 MCP tools**: workflow/trigger CRUD for external agents (Claude
  Desktop, Cursor). Registered on the platform MCP server.

- **4 SDKs**: Go, TypeScript, Python, Java — all have workflow + trigger
  methods with wire-format tests.

- **Frontend**: sidebar entries (Workflow/Zap icons) + two-column layout
  pages for workflow and trigger management. YAML editor with server-side
  validation.

- **7 Prometheus metrics**: run duration, node duration, concurrent runs,
  trigger fires, webhook deliveries, scheduler tick.

### Fixed

- **agentd: loadAllowedDirs append bug (#687).** `loadAllowedDirs` appended
  to `allowedDirs` instead of replacing, causing 2 tests to fail in any real
  workspace. Fixed: reset before each load.

- **agentd port mismatch (#686).** Controller's `HTTPAgentdExecutor` used
  port 4098 (admin) instead of 4097 (user). Fixed by moving the engine to
  the API server.

## [0.8.13] - 2026-08-07

### Fixed

- **Image factory: consistent scope sections + member configs read-only
  (#680).** Org and platform admin tabs now show consistent section
  headings ("Org Images" / "Platform Images" / "Member Images"). Member
  configs are always read-only from org/platform tabs. Cross-scope
  configs shown in separate sections with no edit buttons.
- **Image factory: audit fixes (#683).** Delete confirmation now shows
  the config name instead of the hash. Base selector correctly tracks
  and sends the version to the create API. Backend doc comment corrected
  for `canMutateScope` cross-org limitation.

## [0.8.12] - 2026-08-07

### Added

- **Image factory: build row billing attribution (#679).** Added `scope`
  and `org_id` columns to `image_factory_builds` (migration 000018).
  Org-scoped builds are attributed to the org; platform-scoped builds to
  the platform owner. Backward compatible (nullable + backfill from
  config scope/org_id). Two partial indexes for billing queries.

## [0.8.11] - 2026-08-07

### Added

- **Image factory: org/platform-scoped config creation (#664, #667).** Org
  admins and platform admins can now pre-build images at their scope.
  New routes: `POST /orgs/:id/image-factory/configs` (OrgAdminGuard),
  `POST /admin/image-factory/configs` (AdminGuard). Cross-scope coalescing
  (same selection = one shared build). Extended delete/rename ownership
  (org admin status check, platform admin bypass). Org admin Images tab +
  platform admin Image Factory tab in the frontend.
- **Image factory: `allowed_image_configs` restriction policy (#668).** Org
  admins can restrict which org/platform images members can launch. Empty
  = unrestricted (default). Member configs always exempt. Enforcement at
  API (`resolveImageFactoryConfig`) as a backstop. Migration 000017.
- **Epic 64: Triggers & Workflows (#655, #656, #657, #663, #665, #666).**
  Data model, storage layer, DAG spec validator, workflow CRUD handlers,
  trigger CRUD + webhook secret crypto, and design definition.

### Fixed

- **Mobile settings tab horizontal scroll (#658).** Tab items lacked
  `shrink-0` and `overflow-x-auto` was on the wrong element, making
  "Workspace Images" unreachable without rotating to landscape.

## [0.8.10] - 2026-08-05

### Added

- **Org admin Settings tab — surface 6 backend-only policies (#653).** New
  `/orgs/:id/settings` tab with three cards: Workspace Limits
  (`max_workspaces_per_member`, `max_active_workspaces_per_member`), Model &
  Provider Restrictions (`allowed_models`, `allowed_providers`), and MCP &
  Image Defaults (`max_mcp_servers_per_workspace`, `default_runtime`).
  Previously these 6 policies were backend-only since Epic 43 with no UI.
- **E2e coverage for org-admin portal (#654).** 9 Playwright tests covering
  deep-linking, sidebar navigation, role gating, save flow, and unhappy
  paths (org load 404, save 403). Closes the codebase-wide e2e gap for the
  org-admin portal.

## [0.8.9] - 2026-08-05

### Fixed

- **Image-list dropdown: viewport-aware positioning (#652).** The
  `NewWorkspaceSplitButton` popup was hardcoded to `absolute right-0
  top-full` and overflowed the viewport edge when the button sat near the
  right or bottom of the screen. Now portals to `document.body` via
  `createPortal` and positions via `computeMenuPosition` (reused from
  `KebabMenu`): flips above when no room below, clamps horizontally, caps
  height with scroll. Repositions on scroll/resize.

## [0.8.8] - 2026-08-05

### Fixed

- **MCP org-tab: routing, list-envelope crash, member-policy toggle (#650).**
  Three org-admin MCP bugs: (1) org-level add was rejected with "org admin
  has disabled member MCP servers" because the router mounted the org tab
  without passing `orgId`, causing every org-scope request to fall through
  to the user endpoint; (2) `n.map is not a function` crash because the API
  client typed the `{servers:[...]}` envelope as a bare array; (3) no UI for
  org admins to manage member MCP servers (`allow_user_mcp_servers` policy
  was backend-wired but had no frontend toggle). Also fixes a latent
  `api.put`/`api.post` falsy-body drop (silently dropped `false`/`0`).
- **MCP deferred-wiring regression gaps (#651).** Closed two gaps from the
  v0.7.1 production 500 fix: admin audit events were silently dropped
  (nil-wired `SetAudit`) with no regression test; `resolveWorkspaceQuota`
  lacked the same nil-orgChecker guard `UserCreate` got. Both now have
  red→green regression tests.

### Added

- **Image Factory: delete + rename config API and UI (#649).** Member-scope
  `DELETE /configs/:hash` (rejects building status) and
  `PATCH /configs/:hash` (rename with collision detection). Shared
  `resolveConfigByHash` (member → org → platform scope loop).

## [0.8.7] - 2026-08-04

### Fixed

- **PWA: autoUpdate (#648).** Switched service worker from `prompt` to
  `autoUpdate` to prevent stale-chunk 404 errors after deploys (the admin
  portal `PlatformAdminLayout` 404).
- **Dark mode dropdowns (#648).** Native `<option>` elements now inherit
  dark-mode background/foreground via global CSS.
- **Image factory UI pills (#648).** Split-button popup shows Ready/Building
  status pills. Workspace Images tab shows scope pills (Platform/Org/Personal)
  and an expandable drawer with extension chips. Status pills are dark-mode-safe.

### Added

- **Catalog: R + Julia (#648).** Added `r-base`, `r-devtools` (CRAN build
  dependencies), and Julia LTS to the image factory catalog.
- **Preferences: preferredRuntime dropdown (#648).** The Default Image user
  setting now renders as a dropdown of Ready configs instead of freeform text.

## [0.8.6] - 2026-08-04

### Added

- **Workspace: split-button launch + default-image hierarchy (#642).** The
  new-workspace button is now a segmented control: `[+]` launches the default
  image in one click, `[▼]` opens a popup menu of Ready image-factory configs.
  Default image is resolved via a 4-tier hierarchy: user preference
  (`preferredRuntime`) → org policy (`default_runtime`) → platform setting
  (`workspace.defaultImage`) → `"base"`. Org admins set the default via the
  existing policy API (`PUT /orgs/:id/policies/default_runtime`).

## [0.8.5] - 2026-08-03

### Added

- **Image Factory: workspace launch integration (#641).** Workspace creation
  now supports selecting an image-factory config. The new-workspace dialog
  shows a config picker with status pills (Ready=selectable, Building/Rejected=
  disabled). The API resolves the selected config to its built image ref and
  sets it as the workspace runtime — leveraging the controller's existing
  image-ref passthrough, so no controller or CRD change is needed.

## [0.8.4] - 2026-08-03

### Fixed

- **Image Factory: accept 204 from GitHub workflow_dispatch (#640).** GitHub's
  `workflow_dispatch` endpoint returns 204 No Content on success, not 201 Created.
  The dispatcher only accepted 201, so every successful dispatch was treated as a
  failure → 503 → DB rollback. This was the second of two compounding bugs behind
  the 2026-08-03 outage (the first was the missing `Actions: Write` permission,
  fixed by granting it; #639's logging made this one visible). Accept both 201
  and 204 as success.

## [0.8.3] - 2026-08-03

### Fixed

- **Image Factory: dispatch error logging (#639).** `POST /image-factory/configs`
  returned a generic `503 "failed to dispatch build"` with the underlying error
  discarded, making the 2026-08-03 outage (GitHub App missing `Actions: Write`
  permission → `403 "Resource not accessible by integration"`) look like a
  wiring/version problem for hours. The dispatch-failure path now logs the
  dispatcher's wrapped error via the handler logger before returning 503.
  Adds `SetLogger`/`HasLogger` on `ImageFactoryHandler` (mirrors the
  `pod_bootstrap.go` precedent for swallowed-error observability gaps) plus a
  handler-level regression test and an app-level wiring guard.

## [0.8.2] - 2026-08-03

### Fixed

- **Image Factory: null knownFailures crash (#638).** Go serializes nil
  slices as JSON null; the catalog endpoint returned "knownFailures":null
  which crashed the frontend (catalog.knownFailures.some on null). Fixed:
  API returns [] not null; frontend has a defensive guard.

### Added

- **Image Factory: redesigned catalog — 30 extensions in 3 groups (#638).**
  Language Packs (Python/Node/Go/Rust/Java/.NET/Ruby/PHP via mise), System
  Packages (Playwright, Chromium, TeX Live, Tesseract, Graphviz, etc via apt),
  Files (MOTD). Frontend groups extensions by type.


## [0.8.1] - 2026-08-03

### Changed

- **Image Factory: GitHub App authentication (#637).** Switches from PAT
  to GitHub App (`llmsafespaces-builder`, App ID 4470040). The dispatcher
  mints a JWT from the App's private key, exchanges it for an installation
  token (cached 50min, mutex-protected), and uses it for workflow_dispatch.
  The workflow uses `actions/create-github-app-token` to push packages to
  `ghcr.io/lenaxia/llmsafespaces-images/ws`. Config: `appId` + `privateKey`
  replace `apiToken`. Helm chart: `appCredentials.secretName` replaces
  `secretKeyRef`. 7 dispatcher tests (happy + error paths + caching).

## [0.8.0] - 2026-08-02

### Added

- **Image Factory — custom workspace images (#616, #619, #624, #628, #629,
  #631, #634).** Talos-style image factory enabling users to self-serve
  workspace images with system-level dependencies that mise/pip/npm/go
  cannot supply. Users select extensions from an operator-curated catalog;
  the API renders a deterministic Dockerfile and dispatches a GitHub Actions
  build; the workflow builds multi-arch, pushes to ghcr.io, and calls back
  with the result.

  **Design:** `design/0046` (28 decisions) + `design/0047` (contracts).

  **Architecture:**
  - **Immutable extensions** (design #7): catalog entries are publish-new +
    retire, never edited in place. This makes content-addressing and the
    failure blocklist simple by construction.
  - **Eager build on save** (design #12): configs are only launchable once
    Ready → the workspace controller stays genuinely untouched.
  - **Dispatch-before-commit** (design #17): GH Actions dispatch happens
    before the config row commits; on dispatch failure the row is never
    created.
  - **Build coalescing** (design #16): duplicate in-flight/succeeded builds
    are linked, not re-dispatched.
  - **Per-build callback token** (design #18): `subtle.ConstantTimeCompare`
    on the callback endpoint — the only path an external runner can mutate
    build state.
  - **Atomic transitions** (design): `TransitionBuildSucceeded`/`Failed`
    do all writes in a single DB transaction.

  **Components:**
  - Migration `000013`: 6 tables (platform_config, bases, extensions,
    known_failures, configs, builds).
  - `api/internal/imagefactory/`: pure logic — `HashSelection` (content-
    addressed `s-<sha256[:16]>`), `ResolveSelection`, `RenderDockerfile`
    (deterministic), `ValidateResolved`, `LoadSeed`/`SeedCatalog` (embedded
    YAML with 9 initial extensions).
  - `api/internal/handlers/imagefactory*.go`: consumer endpoints (catalog,
    configs, callback), admin endpoints (bases/extensions/known-failures
    CRUD), `ghActionsDispatcher` (production GitHub Actions API client),
    `llmExplainer` (OpenAI-compatible failure explanation with degradation).
  - `.github/workflows/image-build.yml`: the build workflow — receives a
    pre-rendered Dockerfile, runs `docker buildx build`, pushes to ghcr.io,
    POSTs the result via authenticated callback.
  - Frontend: `WorkspaceImagesTab.tsx` settings page with status pills
    (building/ready/rejected), extension checkboxes, base selector,
    create-and-build form. API client `imageFactory.ts`.
  - Helm chart: `imageFactory.*` values (imageRepo, callbackURL,
    ghDispatcher with secretKeyRef for PAT, llmExplainer, architectures).
  - ConfigMap + deployment templates for secret-based credential delivery.

  **Tests:** 60+ Go tests (unit, sqlmock store, postgres integration,
  handler-level e2e round-trips, dispatcher, seed, callback security,
  AdminGuard), 9 vitest frontend tests. All `-race` clean.

  **6 design docs** (`design/0046` + `design/0047`) capture the full
  decision history including 3 stress-test passes.

### Added

- **Image Factory S6 — LLM failure explainer (#624).** Wires the platform's
  in-cluster LLM (LiteLLM/vLLM) into the image-build failure-explainer
  interface defined in S5, producing human-readable explanations of build
  failures with graceful degradation when the LLM is unavailable.
- **Image Factory S7 — Admin portal endpoints (#628).** Platform-owner admin
  CRUD endpoints for the image factory catalog: bases, extensions,
  known-failures, and platform config (architectures).
- **Image Factory S8 — GH Actions image-build workflow (#629).** The
  `.github/workflows/image-build.yml` triggered by `workflow_dispatch` from
  the API's `POST /configs` handler; the API pre-renders the Dockerfile and
  the workflow builds and pushes the custom workspace image.

### Changed

- **Repolint gate: gin.SetMode under t.Parallel (#630).** Adds a repo lint
  check that fails if a `*_test.go` file calls `gin.SetMode` from a function
  reachable by a `t.Parallel()` test body. Prevents recurrence of the
  data race that blocked the v0.7.1 release gate. Detection is per-function
  transitive reachability; serial-only calls remain allowed.

## [0.7.1] - 2026-08-02

### Fixed

- **Nil orgChecker panic on POST /me/mcp-servers (#622).** The user MCP
  handler was constructed with `pgOrgStore` before `pgOrgStore` was
  initialized (init ordering in `app.go`). `UserCreate` called
  `h.orgChecker.GetUserOrgID()` on a nil interface → nil pointer dereference
  → generic 500 on every user-scope MCP server creation. Fixed: pass nil at
  construction, wire via `SetOrgChecker` after `pgOrgStore` is created.
  Added nil-guard (503 instead of panic). Same nil-wiring bug fixed on
  `adminMcpHandler.SetAudit` (was silently dropping all platform-admin MCP
  audit events).

## [0.7.0] - 2026-08-02

### Added

- **MCP Server Integration — Epic 53 (#613, #615, #617).** Platform admins,
  org admins, and individual users can now register external MCP servers
  whose tools workspace agents gain at startup. Three scopes (platform/org/user),
  three transports (http, sse, stdio), and full governance:

  - Three-tier encrypted storage mirroring the Epic 30 credential model:
    admin/org via master KEK, user via session DEK (zero-knowledge).
  - Injection pipeline: `mcp-server` entries in `secrets.json` → materializer
    stages → `AgentConfigWriter` renders opencode `mcp` config section.
  - Live reload via `agentpush.Service` after bind/unbind/token rotation.
  - Org policy `allow_user_mcp_servers` (default locked) gates member access.
  - Plan-tier quota `MaxPersonalMcpServers` (free=5, team+=unlimited).
  - Org policy `max_mcp_servers_per_workspace` (default 5) enforced at bind.
  - Instance setting `mcp.allowOrgAdminServers` (default true, fail-closed)
    kill-switch for all org-admin MCP mutations.
  - Secret references via `{env:VAR}` — resolves from existing `env-secret`
    entries at opencode runtime.
  - SSRF validation: IP range blocking (RFC1918, loopback, link-local, CGNAT,
    Unspecified) + DNS resolution.
  - Env var name validation reuses `validation.ValidateEnvVarName` (blocks
    `LD_PRELOAD` etc). Header CRLF injection prevention.
  - OpenAPI spec updated with full MCP CRUD + bindings + auto-apply.
  - Frontend: shared `McpServersTab.tsx` for all three admin surfaces with
    secret-reference picker.
  - E2E verified against real opencode 1.15.12 binary — all 3 transports
    connect successfully.

  Migration `000012`: `mcp_servers`, `mcp_server_bindings`, `mcp_server_auto_apply`.
  Schema version bumped to 8. No CRD — MCP servers are API-owned relational data.

  Design: `design/stories/epic-53-mcp-server-integration/`. PRs: #613, #615, #617.

- **README-LLM.md MCP Server Integration section (#615).** Documents the
  as-built system per DoD item 15.

### Fixed

- **Passkey login redirect (#613).** `redirectAfterAuth` only redirected when
  `return_to` was set. Passkey login without `return_to` stayed on `/login`.
  Now falls back to `/chat`.

- **Passkey e2e cookie auth (#613).** The Playwright mock for `/auth/me`
  accepted only Bearer headers; now also accepts `lsp_session` cookies,
  matching production HttpOnly cookie behavior.

## [0.6.0] - 2026-07-30

### Added

- **Instance-default `/tmp/*` auto-approval for agent permissions (#602).** New
  instance setting `workspace.allowedExternalDirectories` (default `["/tmp/*"]`)
  injects `mode.permissions.external_directory` allow-rules into every
  workspace's `agent-config.json` at boot. Agents no longer prompt for `/tmp/*`
  on every session. `/sandbox-cfg/*` is deliberately excluded (plaintext
  credential path). Configurable via admin UI; schema v7.

- **Go SDK `Secrets.CreateWithMetadata` method (#603).** Non-breaking addition
  alongside existing `Create` (which delegates with nil metadata). Required for
  `env-secret` type secrets that need `metadata.var_name`.

### Fixed

- **Remember-me sessions last 30 days, not 24 hours (#602).** The Helm chart's
  ConfigMap never rendered `auth.rememberMeDuration`, so the Go auth service
  silently fell back to `tokenDuration` (24h) for every Helm-deployed install.
  Users checking "remember me" were bounced to `/login` daily.

- **API-layer workspace storage validation (#603).** The API had no storage size
  format/magnitude validation (only the controller webhook did, which can be
  disabled). Added format regex (`^[1-9][0-9]*(Gi|Mi)$`) + 1024Gi max cap.

- **GetWorkspace returns 404 after deletion (#603).** Previously returned the
  DB row with `Phase=""` when the CRD was garbage-collected — clients couldn't
  distinguish "deleted" from "creating". Now returns 404 NotFound.

- **SDK `BindingItem.ID` JSON tag corrected (#603).** Changed from `json:"id"`
  to `json:"secretId"` to match the API's actual response shape.

- **10+ SDK canary assertion fixes (#603).** Fixed cascading pre-existing
  failures across S-WS-CRUD, S-WS-STATUS, S-SECRET-CRUD, S-SECRET-REVEAL,
  S-SECRET-AUDIT, S-SECRET-BINDINGS, S-ENV-VARS, S-OWNERSHIP, S-USER-SETTINGS,
  S-RATE-LIMIT scenarios. Root causes: env-secret metadata requirement, DEK
  requires JWT auth (not API key), binding JSON tag mismatch, schema version
  bump, rate-limit burst threshold.

## [0.5.5] - 2026-07-25

### Fixed

- **API-key-only admin workspaces now reach Active (#593, #595).** Three
  root-cause fixes: (1) `agentHealth.providersConfigured` was always `0` for
  healthy workspaces because the controller's healthy condition message
  omitted the `configured=N` token the API regex parses; (2) `POST
  /provider-credentials` and `GET /provider-credentials/:id/models` returned
  an opaque `503 "encryption unavailable"` for API-key callers — now returns
  `403` with actionable guidance pointing at `decryptAccess=true` or password
  auth; (3) admin credentials never reached admin-owned workspaces without a
  manual second API call — `SeedWorkspaceCredentials` now cascades all admin
  credentials when the workspace owner has `users.role='admin'`.

- **Deleted API keys are now immediately rejected (#597).** The 15-minute
  validation cache was not evicted on delete, so a deleted key kept
  authenticating until the TTL expired. `DeleteAPIKey` now evicts the
  `apikey:<hash>` cache entry after the DB delete.

- **`motd` field always present in `/auth/config` response (#596).**
  `AuthConfig.MOTD` had `json:"motd,omitempty"` which dropped the key when
  the value was empty (the default). All three SDK canaries assert field
  presence.

- **returnTo redirect race under react-router v7 (#600).** v7's
  `startTransition` default caused `navigate(returnTo)` to lose a race with
  `GuestOnly`'s `<Navigate to="/chat">`. Login and Register pages now use
  `window.location.href` for the returnTo redirect.

### Changed

- **Frontend major-version bumps: vite 5→8, vitest 2→3, react-router-dom
  6→7 (#600).** Clears all moderate+ npm CVEs (esbuild dev-server CORS,
  vite server.fs.deny, react-router open-redirect ×3). Zero config changes;
  the app already used `createBrowserRouter` (v6.4+ data-router API). The
  returnTo fix above was the only code change required.

- **SDK canary CI runs a real kind cluster (#599).** The `sdk-canary` job
  previously used a stub kubeconfig pointing at a non-existent server, which
  broke workspace-creating scenarios (`S-WS-CRUD`, `S-WS-STATUS`,
  `S-WS-QUOTA`, `S-OWNERSHIP`). Now creates a kind cluster + installs CRDs
  so workspace CRUD operations succeed.

- **SDK versions synced to 0.5.4 (#598).** Python `pyproject.toml`,
  TypeScript `package.json`/`package-lock.json`, Java `pom.xml`, and
  `openapi.yaml` were stale at `0.4.5` on main (the release pipeline
  overwrites at publish time and never bumped them back).

- **postcss bumped to 8.5.18+ via npm overrides (#597).** Clears
  GHSA-r28c-9q8g-f849 (path traversal, HIGH).

## [0.5.4] - 2026-07-23

### Fixed

- **SDK publish pipeline: PyPI packages-dir and TypeScript build path.**
  The PyPI publish job looked for `dist/` at the workspace root instead of
  `sdks/python/dist/`. The TypeScript npm job failed because v0.5.2 was
  manually published without build output. Both fixed.

## [0.5.2] - 2026-07-23

### Added

- **SDK refresh, parity, and publishing (Epic 62, #584 #586 #588).** The four
  hand-written SDKs (Go, Python, TypeScript, Java) are now at typed-surface
  parity with the current API server. The OpenAPI spec went from 45 → 84
  paths. Python and TypeScript SDKs publish to PyPI and npm respectively on
  platform release tags. SDK versions track the platform version. The Java
  SDK was rewritten from a generic HTTP wrapper into a typed facade with
  unchecked exception hierarchy. A live contract test validates the
  `/message` blocking + JSON assumption against a real workspace.

## [0.5.1] - 2026-07-23

### Fixed

- **Swipe-to-open-sidebar no longer triggers browser back-nav (#590).**
  In mobile mode, ~50% of swipe-to-open-sidebar gestures triggered the
  browser's back navigation instead. Root cause: `touchstart` was
  registered `{ passive: true }`, making `preventDefault()` impossible at
  the moment of contact — the browser/OS committed to the back-nav
  gesture during touchstart / first-touchmove before the JS `touchmove`
  handler could claim it. Fix: make `touchstart` non-passive and call
  `preventDefault()` for edge touches (`clientX < EDGE_ZONE`) at contact,
  claiming the gesture before the OS can engage back-nav. Applies to both
  `AppShell` (chat) and `PortalLayout` (admin/orgs portals).

## [0.5.0] - 2026-07-23

### Added

- **Platform versions display in the admin portal (#587).** A new
  "Versions" tab (`/admin/versions`) shows the running version of every
  platform component (API, Controller, Frontend, Relay Router, Base
  Runtime) in a table. Versions are read from the deployed Deployment
  image tags via a new `GET /api/v1/admin/platform-info` endpoint
  (admin-gated) — the most truthful "what is running" signal — rather
  than each component self-reporting. The handler discovers components
  by the `app.kubernetes.io/name=llmsafespaces` label (release-name
  independent) and degrades gracefully (200 with partial data) if the
  K8s API or settings are temporarily unavailable.

- **SDK refresh — OpenAPI parity + Python/Java/TypeScript SDKs (#584,
  #586, #588).** Closes the drift between the four hand-written SDKs and
  the current API server:
  - **OpenAPI spec refresh (US-62.1):** 45 → 84 paths covering the full
    in-scope router surface; version 1.0.0 → 1.1.0.
  - **Python SDK parity (US-62.2):** rewritten for parity with the
    refreshed spec; contract tests (US-62.9).
  - **Java SDK typed-facade rewrite (US-62.5, #586):** builder-constructed
    facade with 9 service groups + typed model classes, replacing the
    generic HTTP wrapper.
  - **Python/TypeScript SDKs published to PyPI + npm (US-62.8, #588):**
    SDK versions now track the platform version; trusted-publishing
    release pipelines (OIDC, no stored tokens) run on `v*.*.*` tags.

## [0.4.5] - 2026-07-22

### Fixed

- **Kebab menus now viewport-aware (#583).** The kebab (three-dot)
  menu always opened directly below its trigger; the bottom-most
  workspace/session in the left nav overflowed past the viewport
  bottom — partially unreadable/unusable. Since `KebabMenu` is the
  shared component backing all three usages (sidebar workspace, sidebar
  session, chat header) and is the only custom portal in the codebase,
  the fix applies everywhere. Positioning now: flips above when there
  isn't room below; clamps horizontally to the viewport edges; caps
  height and scrolls (`overflow-y-auto`) when the menu is taller than
  the viewport; repositions on scroll/resize. The geometry is extracted
  into a pure `computeMenuPosition()` (unit-tested) and measured via
  `scrollHeight` (not `offsetHeight`) so the `maxHeight` cap stays
  stable across re-measures — measuring the capped height would drop
  the cap and re-expand the menu.

## [0.4.4] - 2026-07-22

### Fixed

- **Chat links open in a new tab (#581).** Markdown links rendered by
  `ReactMarkdown` in chat messages (both assistant text and
  thinking/reasoning parts) opened in the same tab, navigating the
  user away from the chat. Added a shared `MarkdownLink` component
  that renders every link with `target="_blank"` and
  `rel="noopener noreferrer"` (the `rel` is required for security —
  `target="_blank"` without it lets the opened page access
  `window.opener`). Both `ReactMarkdown` instances in `MessagePart.tsx`
  now wire the override; `rehype-sanitize` strips `target`/`rel` from
  the hast tree by default, but the component override adds them at
  render time so they always reach the DOM.

## [0.4.3] - 2026-07-22

### Fixed

- **Frontend image — patch system-library CVEs (openssl, libpng,
  libxml2, musl, nghttp2, zlib).** The frontend image builds on
  `nginxinc/nginx-unprivileged:1.27-alpine`, whose packaged system
  libraries lagged Alpine's security advisories. The release Trivy
  gate found HIGH/CRITICAL CVEs against `libssl3` (CVE-2026-31789
  CRITICAL, CVE-2025-15467 HIGH), `libpng`, `libxml2`, `musl`,
  `nghttp2-libs`, and `zlib`. Added `apk upgrade --no-cache` to the
  nginx stage of `frontend/Dockerfile`, which pulls the fixed
  versions from Alpine's repository (e.g. openssl 3.3.7-r0, libpng
  1.6.55-r0). This is a real package upgrade, not a `.trivyignore`
  accept — the vulnerable bytes no longer ship. The other
  control-plane images (api/controller/relay-router/relay-proxy) are
  `gcr.io/distroless/static` and were already clean; the base runtime
  image is warn-only by design (Debian bookworm CVE backlog).

- **Release workflow — two gate bugs that blocked the v0.4.2 release
  (#577, #578).** (1) The `wait-for-ci` job's `ALL_DONE` jq computation
  counted its own in-progress check run (self-referential — the same
  class of bug #574 had set out to fix), making the success branch
  unreachable and forcing a 40-minute timeout. (2) After (1) was fixed,
  nightly `gremlins` (mutation tests) check runs on the tag SHA — which
  are expected mutation-testing signal, not a release gate — were being
  treated as release blockers. Both fixes add the appropriate name
  exclusion to the gate's jq filter.

## [0.4.2] - 2026-07-22

### Fixed

- **Streaming scroll-follow — user no longer yanked to the bottom (#575).**
  During streaming, scrolling up to read earlier content while waiting
  for a response immediately pulled the viewport back to the bottom,
  trapping the user at the tail. Root cause: in
  `frontend/src/components/chat/MessageList.tsx`, the `scroll` handler
  deferred the `stickToBottom.current` intent update into a
  `requestAnimationFrame`. The streaming `MutationObserver` auto-scrolls
  to the bottom on every token while `stickToBottom` is `true`; because
  the intent update was deferred one frame, a token whose observer rAF
  was registered before the user's scroll rAF ran first and re-scrolled
  to the bottom, so the deferred handler read an at-bottom position and
  kept `stickToBottom=true`. With tokens arriving every few ms this
  happened on essentially every scroll attempt. Fix: update
  `stickToBottom.current` synchronously in the scroll handler; the very
  next token's observer now sees the user's intent and leaves the
  viewport alone. Follow mode and the "Resume tailing" button are
  unchanged.

- **Release workflow — self-referential CI-gate failure (#574).** Both
  the v0.4.0 and v0.4.1 releases were blocked by a self-referential
  failure in `lewagon/wait-on-check-action`: when the `Wait for CI`
  job failed (transient API error) and was re-run, the prior failed
  check run persisted on the ref, so every retry immediately saw the
  old failure and re-failed — making the release unrecoverable without
  manual `gh release create`. Replaced the action with a custom polling
  script in `release.yml` that filters out Release workflow job names
  explicitly (in-progress or failed), so it never observes its own
  check. This is what makes the v0.4.2 release cuttable cleanly.

## [0.4.1] - 2026-07-21

### Added

- **zstd in runtime base image (#569).** Added the `zstd` package to
  the workspace runtime image. Required by modern package managers
  (apt .deb contents, conda .conda format, npm v10+ tarballs), git
  pack compression (2.22+), and container image layers. Pre-fix, agents
  hitting any of these got a confusing "command not found".

- **Synthetic monitor for CORS expose-headers (#571).** New script at
  `hack/monitor-cors-expose-headers.sh` that catches the class of bug
  where an ingress-controller middleware overrides the app's
  `Access-Control-Expose-Headers` — silently stripping `X-Next-Cursor`
  from the browser's view. The app's own tests cannot catch this
  because they only cover the app; the override happens at the edge.
  Schedule via UptimeKuma, cron, or GitHub Actions.

### Fixed

- **EnqueueMessage body cap — DoS hardening (#568).** The canonical
  `EnqueueMessage` handler at `proxy_handlers.go` read the request body
  via unbounded `c.ShouldBindJSON`. A client could POST a multi-gigabyte
  body that the API would buffer in full before the 100KB text limit
  rejected it. Applied `http.MaxBytesReader` cap (101KB) matching the
  pattern already used in `redirectPromptToQueue` and `proxy.go:275`.

- **Stable sort tiebreaker in message history (#570).** The
  `selectChronological` sort used `id.localeCompare` as a tiebreaker
  when two messages shared the same `createdAt` millisecond — the
  documented root cause of issue #387. Replaced with stable-sort by
  original array index (backend delivery order). Immune to future
  opencode ID format changes; the lex tiebreaker assumed IDs are
  sortable by creation time (they're not).

- **Test isolation from host's reload-secrets cache (#572).**
  `TestE2E_PasswordReset_FullPurgeThenBoot_NoProviders` was failing on
  hosts where `/sandbox-runtime/last-reload-secrets.json` exists
  (including the opencode sandbox). The materialize subcommand reads
  this file by default; the test forgot to override
  `LLMSAFESPACES_RELOAD_CACHE_PATH` to a tempdir. Fixed in all three
  materialize call sites in `pod_bootstrap_e2e_test.go`.

- **Trivy scan split — control-plane blocks, base warns (#567).** The
  v0.4.0 release was blocked because Trivy found HIGH-severity CVEs in
  the base runtime image (Debian bookworm packages, mostly
  `fix_deferred` upstream). Split the scan loop: control-plane images
  (api/controller/frontend/relay-router/relay-proxy) still gate the
  release; the base runtime image surfaces findings as a warning
  annotation without blocking. The base inherits Debian's CVE backlog;
  failing on every bookworm CVE would prevent any release from shipping.

## [0.4.0] - 2026-07-19

### Added

- **Redis TLS support (#465, #557).** The API's Redis client config now
  exposes `tls` and `insecureSkipVerify` fields, wired through the chart
  values and configmap. Required for AWS ElastiCache
  (`TransitEncryptionEnabled`), GCP Memorystore with TLS, and any
  self-hosted Redis with TLS. Production should leave
  `insecureSkipVerify: false` and use a CA-signed cert; the field is a
  dev/test escape hatch for self-signed certs. Chart test renders the
  configmap fields; cache test asserts TLS connect + cert-verification
  paths.

- **Image digest pinning (#476, #556).** Every image section in the chart
  (`api`, `controller`, `frontend`, `runtimeEnvironments.base`,
  `relay-router`) now accepts an optional `digest` field. When set, it
  overrides `tag` and the image reference becomes `repository@digest`.
  Operators hit by #454 (tag GC'd from GHCR) wanted immutable pins; both
  `sha-<commit>` and `sha256:<digest>` forms are now first-class. Three
  new chart tests pin the helper behavior.

- **GCP KMS provider for master KEK (US-57.3, #528).** The KEK provider
  abstraction now has a GCP KMS implementation alongside AWS KMS. The
  master KEK can be hosted in either cloud; switching is a configmap
  change. Closes the multi-cloud KEK story for operators who already
  run GCP.

- **`migrate-kek` CLI for cross-provider KEK migration (US-57.2, #532).**
  New binary at `cmd/migrate-kek` that re-wraps the existing master KEK
  under a new provider (e.g. AWS KMS → GCP KMS, or local → cloud) with
  zero downtime. The KEK is decrypted in-memory under the old provider,
  re-encrypted under the new, and the result is atomically written.
  Audit-logged via the standard `secret_audit_log`.

- **Invitation accept page + settings deep-linking + `return_to`
  redirect (#533).** Org invite emails now land on a dedicated accept
  page that handles logged-in vs. logged-out flows. Settings tabs are
  deep-linkable (`/settings/billing`, `/settings/security`, etc.) with
  a `return_to` query param preserved across the auth gate. New e2e
  specs cover invitation accept, return_to redirect, and settings
  messaging.

- **gVisor overhead benchmark harness (Epic 51, #549).** Operator-run
  script at `helm/scripts/gvisor-benchmark.sh` that measures gVisor
  overhead on a representative LLM-coding workload. Documented
  methodology in `docs/operator/gvisor-benchmark.md` — accept/reject
  decision per the <30% overhead target.

- **Supply-chain hardening: cosign signing + Trivy scanning + Renovate
  digest pinning (#534).** All release images are signed with cosign
  keyless OIDC; `cosign verify` is documented in the install runbook.
  Trivy scans run in CI on every PR and on the release tag. Renovate
  is configured to open digest-pinning PRs for base images.

- **Traefik CORS guidance in operator docs (#560).** The chart
  previously documented only the nginx-ingress security-headers path.
  Added a "CORS at the edge" subsection to `docs/operator/networking.md`
  with a complete Traefik Middleware example, the 5-header
  expose-list pinned to `security.go:64`, the drift-hazard warning, and
  a verification snippet. Closes the documentation gap that caused a
  production CORS bug (queued-message button never rendered).

### Fixed

- **FIFO race in message queue — drained messages rendered out-of-order
  after reload (#563, #564).** When a session transitioned busy→idle,
  a direct POST /prompt could race ahead of the still-draining queue
  goroutine. opencode assigned the direct send an earlier
  `info.time.created` than the queued message, so on reload
  `selectChronological` placed the queued message AFTER the direct
  send. Two-layer fix: (1) frontend `handleSend` now also checks
  `queue.queuedMessages.length > 0` before routing to direct send,
  closing the common case; (2) backend `SendPromptAsync` checks
  `queueSvc.Len()` and redirects to `Enqueue` when non-empty, closing
  the residual race window where the client's view is stale (the
  `refreshQueue` poll hasn't landed yet). Regression tests at both
  layers with FIFO-order assertions.

- **Free-models refresher ClusterRole configmaps grant (#469, #558).**
  The controller's ClusterRole was missing `get/list/watch` on
  `configmaps`, so `freeModelsRefresher` could not read the cached
  free-models list. The chart now renders the grant conditionally on
  `controller.inferenceRelay.enabled`.

- **Relay `scrapeRouterMetrics` silent error swallowing (#475, #555).**
  The metrics scraper logged at Debug level on HTTP errors, masking
  real failures (router not yet running, wrong port, wrong path).
  Elevated to Warn with the status code and URL in the log fields.

- **arm64 image variants contained x86-64 binaries (#462, #554).** The
  multi-arch build matrix produced arm64 image manifests whose contents
  were x86-64 binaries — `docker pull ...arm64` followed by
  `uname -m` returned `x86_64`. Fixed the Dockerfiles and the build
  matrix's platform handling.

- **copy-html initContainer PSA restricted compatibility (#468, #551).**
  The frontend pod's initContainer ran `cp -a` into a read-only path
  under PSA `restricted`. The chart now mounts an `emptyDir` at
  `/usr/share/nginx/html` and the initContainer copies into it.

- **KEK post-migration audit gate + nil-fallback guard (US-57.2, #548).**
  After a `migrate-kek` run, the API now refuses to start if the new
  KEK provider returns nil — previously it would silently fall back to
  the un-encrypted path. Audit log entries are also gated: a missing
  audit logger no longer silently drops entries.

- **Materialize test isolation from workspace reload cache (#559).**
  The materialize test suite shared state with the workspace reload
  cache, producing flaky failures when run in parallel. Tests now
  isolate their cache fixture.

- **Production CORS expose-headers override by Traefik Middleware
  (talos-ops-prod #2053).** The API correctly emitted
  `Access-Control-Expose-Headers` with 5 entries
  (`X-Request-ID, X-RateLimit-Limit, X-RateLimit-Remaining,
  X-RateLimit-Reset, X-Next-Cursor`), but a Traefik Middleware at the
  cluster edge overwrote the value with only `X-Request-Id`. The
  browser hid `X-Next-Cursor` from JS, breaking the "Load earlier
  messages" button. Fix: updated the cluster Middleware to mirror the
  app's list. Documentation: see "CORS at the edge" above.

### Removed

- **Cloudflare Worker inference relay (Epic 60, #553).** Zen
  (`opencode.ai/zen/v1`) now blocks all Cloudflare Worker egress IPs,
  making the Worker relay path unreachable. Removed: the
  `workers/inference-relay/` directory, the chart's
  `inferenceRelayURL` / `inferenceRelaySecret` / top-level
  `cloudflare:` block, the `--inference-relay-secret` controller flag,
  the `INFERENCE_RELAY_SECRET` env var, and the `relay-secret-sync`
  Helm Hook Job. Operators should switch to direct-to-Zen (the new
  default) or the self-hosted InferenceRelay fleet (Epic 42).
  **Upgrade note:** existing chart values with
  `inferenceRelayURL: https://relay.safespaces.dev` will break — clear
  it to empty.

### Security

- **G13 — Account lockout now keys on email + IP (Medium).** The
  lockout counter was keyed on email only
  (`lockout:<email>`). An attacker who knew a victim's email could
  submit bad passwords from any IP and lock the victim's account — a
  DoS amplification vector. The lockout key now includes the client IP
  (`lockout:<email>:<ip>`), so an attacker from a different IP cannot
  trigger the victim's lockout. A new `WithClientIP(ctx, ip)` context
  helper propagates the IP from the gin router through `Login`. Callers
  that don't set it fall back to email-only keying (backward compat).
  Regression: `TestLogin_G13_AttackerFromDifferentIPCannotLockVictim`,
  `TestLogin_G13_SameIPLockoutStillWorks`,
  `TestLogin_G13_NoIPContextFallsBackToEmailOnly`.

- **G38 — ChangePassword now revokes all sessions (High).** The handler
  at `POST /api/v1/account/change-password` previously re-wrapped the
  DEK with the new password and updated the bcrypt hash but left every
  outstanding JWT — including the caller's — valid until natural expiry.
  A token stolen before the change kept reading secrets (the cached DEK
  in Redis was already evicted by `KeyService.ChangePassword`, but the
  JWT signature remained valid and a re-login re-cached the DEK under
  the new password, which the stolen token could then use). The handler
  now calls `auth.Service.RevokeAllUserSessions` after both the DEK
  re-wrap and the bcrypt update commit, writing the per-jti and per-hash
  revocation markers and clearing durable `jwt_sessions` rows — the same
  primitive `password-reset/confirm` already used (OWASP ASVS V2.5.2).
  Best-effort: a revocation failure is logged and the password change
  still reports success (the cryptographic change is irreversible).

- **G37 — Workspace env-var name blocklist (High).** The handler at
  `PUT /api/v1/workspaces/:id/env` accepted any POSIX-shaped env-var
  name, including `LD_PRELOAD`, `PATH`, `PYTHONPATH`, `BASH_ENV`,
  `DYLD_INSERT_LIBRARIES`, etc. Setting one of these via the env-secret
  mechanism would let a workspace owner compromise every process
  spawned in the pod (agentd, opencode, mise-installed interpreters) —
  a container-escape-equivalent in practice because the pod's single
  UID shares the same trust boundary. The new
  `pkg/validation.ValidateEnvVarName` enforces three rules at the API
  layer: POSIX shape (`[A-Za-z_][A-Za-z0-9_]*`), length ≤ 256, and not
  on a curated blocklist of ~30 dangerous names sourced from ld.so(8),
  bash(1), Python, Node, Ruby, Perl, Java, and glibc docs. The same
  validator is now used by agentd's materialize-time check as defense-
  in-depth. Locale vars (`LANG`, `LC_ALL`, `TZ`) are intentionally NOT
  blocked — they don't execute code and users legitimately set them.

- **G35 — `/account/recover` per-route rate limit (High).** The
  recovery endpoint at `POST /api/v1/account/recover` was mounted on
  the root router behind only the global 100/min/IP rate limiter. While
  the recovery key is 128-bit random (brute-force is mathematically
  infeasible), the endpoint does Argon2id work to re-derive the DEK
  under the new password, making it a CPU-exhaustion DoS target. The
  new `PerRouteRateLimitMiddleware` (separate from the global
  `RateLimitMiddleware`) applies a stricter per-route limit (default
  20 tokens/burst 5, from the previously-dead-code `authRatePerMinute`
  /`authRateBurst` constants) using per-route bucket isolation
  (`<path>:<identity>` key) so a user hitting `/recover` cannot deplete
  the budget for other routes. The middleware is generic — future
  endpoints (e.g. G41 `/secrets/:id/reveal`) can be added to the same
  routes map.

- **G25 — Secret `value` field no longer logged (High).** The request
  logging middleware (`api/internal/middleware/logging.go`) masked
  sensitive JSON fields by name (`password`, `token`, `secret`, `key`,
  `apiKey`, `credit_card`) but NOT `value` — the field name used by
  the secrets API to carry the plaintext credential on
  `POST /api/v1/secrets` and `PUT /api/v1/secrets/:id`. A request to
  create a secret logged the plaintext API key in the application log,
  visible to anyone with log access. Two-layer fix: (1) added `value`
  to the default `SensitiveFields` list (defense in depth — catches
  any logged JSON with a `value` field, even on paths not in the skip
  list); (2) added `SkipPathPrefixes` to `LoggingConfig` and configured
  the default with the credential-bearing paths (`/api/v1/secrets`,
  `/api/v1/account`, `/api/v1/auth`, `/api/v1/admin/provider-credentials`)
  so bodies on those paths are never logged at all. Either layer alone
  prevents the leak.

- **G36 — Workspace secrets cleaned on deletion (High).** The
  workspace controller's `handleTerminating`
  (`controller/internal/workspace/phase_terminating.go`) deleted the
  pod, PVC, and `workspace-pw-*` Secret but NOT `workspace-creds-*`,
  which persisted indefinitely after workspace deletion. The existing
  `cleanupFailedWorkspaceSecrets` primitive (already used for the
  Failed-phase path in `recovery.go`) knows how to delete both
  `workspace-creds-*` and `workspace-pw-*`; this PR wires it into the
  graceful-termination path too. Best-effort (failures logged, not
  propagated — the workspace is already being torn down and the
  finalizer must still release). `handleDeletion` (the CRD-deletion
  entry point) inherits the fix automatically because it calls
  `handleTerminating`.

- **G28 — Workspace bind handler reclassified as Accepted (was High/Open).**
  The threat-model row originally flagged "PUT /workspaces/:id/bindings
  returns 204 but K8s Secret is never created." Epic 35 (secretless
  injection) removed the durable K8s Secret path entirely; the
  architecture now persists bindings to PostgreSQL and the init
  container fetches them via `/internal/v1/pod-bootstrap` at boot. The
  live HTTP push to running pods is best-effort; `ErrNoRunningPod` is
  an accepted, documented transient state. The "no-op for first-time
  delivery" is the intended behavior in the new architecture. Added
  `TestSecretService_G28_BindingsSurviveNoPodState` to lock the
  persistence invariant — bindings survive the no-pod window and are
  visible to the bootstrap read path (`GetBindings`) when the pod
  eventually boots.

### Threat model reconciliation

A fresh audit of the 50 gaps in
`design/stories/epic-17-security-review/THREAT-MODEL.md` against the
current code found 6 rows whose status no longer matched reality. This
entry reconciles the threat model without changing any production code.

- **G29 — Path-traversal `mount_path` accepted by API → 🟢 Fixed (was Medium/Open).**
  The threat-model row claimed `POST /api/v1/secrets` accepts
  `mount_path = "../../etc/passwd"`. `validateMountPath` was added at
  `pkg/secrets/secret_service.go:582` (Bug 13 in worklog 0085); it is
  called from line 563 BEFORE secret creation. Stale row corrected.
- **G45 — Legacy `source /sandbox-cfg/env` in entrypoint → 🟢 Fixed (was Low/Open).**
  US-35.7 moved the env-secret source path to `/sandbox-runtime/secrets-env`.
  The legacy `/sandbox-cfg/env` source no longer exists in
  `entrypoint-opencode.sh`. Stale row corrected.
- **G50 — Decrypt operations not audited → 🟢 Fixed (was Medium/Open).**
  The threat-model row claimed `NewAuditedProvider` had zero call sites.
  US-50.12 wired it at three production sites in `api/internal/app/app.go`:
  `app.go:408` (providerCredsProv), `app.go:409` (orgCredsProv),
  `app.go:624` (apiKeyProv). Every Decrypt on those providers now logs
  to `secret_audit_log`. Stale row corrected.
- **G4 — No mTLS between API and sandbox pods → 🟡 Accepted (was Medium/Open).**
  Real gap, but the fix requires either a service mesh (Linkerd/Istio)
  or per-workspace certificate provisioning — both substantial
  infrastructure additions outside the scope of threat-model-gap fixes.
  Compensating controls: NetworkPolicy default-deny, RFC1918/CGNAT
  egress filter, explicit header allowlist (G34 fix), per-request
  basic-auth rotation. Operator runbook: deploy Linkerd or Istio in
  `inject` mode on the LLMSafeSpaces namespace to close this gap
  without code changes.
- **G30 — Egress NetPol allows external DNS resolvers → 🟡 Accepted (was Medium/Open).**
  Real gap; standard Kubernetes `NetworkPolicy` cannot restrict DNS by
  destination domain. Closing requires Cilium FQDN policies, Calico
  `GlobalNetworkPolicy`, or a custom filtering resolver — operator
  infrastructure decisions. Compensating controls: workspace pods use
  cluster DNS by default; egress allowlist blocks RFC1918/CGNAT; DNS
  exfil bandwidth is naturally limited.
- **G40 — Agentd user port (4097) has no application-layer auth → 🟡 Accepted (was Medium/Open).**
  Real defense-in-depth gap; the trust boundary is the NetworkPolicy
  (`workspace-network-policy.yaml`) — only API server pods can reach
  workspace pods on port 4097. Adding `requireBearerToken` at the
  application layer is defense-in-depth that the existing controls
  make redundant for the documented deployment topologies.

Threat model counts: 26 Fixed / 16 Open / 8 Accepted →
**38 Fixed / 0 Open / 12 Accepted** (50 total). All gaps resolved.

All 50 gaps are resolved: 38 Fixed, 12 Accepted, 0 Open.

- **G6/G41 — `/secrets/:id/reveal` per-route rate limit (Medium).**
  The reveal endpoint at `POST /api/v1/secrets/:id/reveal` accepts
  the user's password as input to re-authenticate before decrypting.
  Without a per-endpoint cap, a single IP could attempt 100 password
  guesses per minute against the global limiter. The route is now in
  `DefaultRouterConfig.PerRouteRateLimitConfig.Routes` at 5/min + burst
  5 — matches the legitimate-user pattern (re-reveal several secrets in
  quick succession) while making brute-force impractical. Closes both
  G6 and G41 (duplicate rows for the same gap). Regression:
  `TestRouter_G41_RevealSecretRateLimited`.

- **G21 — `/sandbox-cfg/password` mode 0600 (Medium).** The init
  container's `cp /mnt/secrets/password/password /sandbox-cfg/password`
  preserved the K8s Secret's `defaultMode: 420` (0644), leaving the
  opencode basic-auth password world-readable in the pod filesystem.
  Replaced with `install -m 0600` so the mode is set atomically with
  the copy. Regression: the existing `TestE2E_Reconcile_*` test now
  asserts the `install -m 0600` line in the rendered credScript.

- **G42 — SSE connection tracking prunes stale entries (Medium).** The
  `sseConnCounts` global map grew monotonically — every distinct
  client IP that ever attempted a connection left a permanent entry.
  `sseConnAllowed` now opportunistically prunes expired entries on
  every call (O(N) sweep, N bounded by per-IP rate limit). Regression:
  `TestSSEConnAllowed_G42_PrunesStaleEntries`.

- **G44 — Pod-level RunAsNonRoot (Low).** `buildPodSecurityContext`
  set RunAsUser/RunAsGroup/FSGroup/SeccompProfile but not
  RunAsNonRoot. Every container today sets it explicitly at the
  container level, but a future sidecar without its own
  SecurityContext would inherit the pod default (nil) and could run
  as root. Added `RunAsNonRoot: &true` at the pod level — the kubelet
  enforces it by refusing to start any container resolving to UID 0.
  Regression: `TestG44_PodSecurityContextHasRunAsNonRoot`.

- **G46 — Silent password file read failure now fatal (Low).**
  `readAgentPassword` previously logged a Warn on file-read error and
  returned an empty string. The workspace would start silently non-
  functional — opencode without auth, every proxy request fails basic-
  auth. Now logs at Error and calls `os.Exit(1)` so the pod enters
  CrashLoopBackOff, surfacing the failure as a pod-level signal.
  Regression: `TestReadAgentPassword_HappyPath` (error path uses
  os.Exit, documented as not unit-testable without subprocess exec).

- **G47 — Inference relay secret no longer exposed as CLI arg (Low).**
  The Helm chart's plaintext fallback
  `--inference-relay-secret={{ .Values.inferenceRelaySecret }}`
  rendered the secret into the controller's container args, visible
  in `kubectl get pod -o yaml` and audit logs. Removed the fallback;
  operators who set `inferenceRelaySecret` without configuring
  `externalSecret.create` or `externalSecret.existingSecret` now get
  a `helm template`-time error with an actionable remediation message.
  Regression: `TestControllerArgs_G47_NoPlaintextRelaySecretFallback`
  (fail-fast) and `TestControllerArgs_G47_EnvVarPathStillWorks`
  (legitimate path).

## [0.3.0] - 2026-07-11

Network hardening sweep + KMS-backed master KEK foundation + Go security bump.

### Security

- **G34 — proxy header allowlist (Critical).** The workspace reverse proxy
  previously forwarded every client request header (Cookie, Origin, Referer,
  X-Forwarded-*, arbitrary custom headers) verbatim into the tenant pod. Now
  uses an explicit allowlist (`Content-Type`, `Accept`, `X-Request-ID`) and
  strips RFC 7230 hop-by-hop headers in both directions. `Accept-Encoding` is
  deliberately not forwarded — Go's http.Transport handles gzip transparently.
  ([#513](https://github.com/lenaxia/LLMSafeSpaces/pull/513))

- **G39 — terminal WebSocket Origin check (High).** The terminal WebSocket
  upgrader accepted any Origin (`CheckOrigin: return true`), enabling
  cross-site WebSocket hijacking from a malicious page in a browser holding
  the user's session ticket. Now defaults to same-origin only, with an
  operator-controlled allowlist (`terminal.allowedOrigins` Helm value).
  Removed the dead `WebSocketSecurityMiddleware` and
  `RouterConfig.AllowedWebSocketOrigins` plumbing — the gorilla `Upgrader`
  is now the single enforcement point.
  ([#515](https://github.com/lenaxia/LLMSafeSpaces/pull/515))

- **CORS hardening.** `security.allowedOrigins=["*"]` combined with
  `security.allowCredentials=true` is now rejected at config load with an
  actionable error. The combo is forbidden by the CORS spec (Fetch §3.2.1)
  because it would let any website read authenticated responses from this API
  in a victim's browser. Browsers reject the combo client-side; we now also
  fail closed at boot.
  ([#516](https://github.com/lenaxia/LLMSafeSpaces/pull/516))

- **NetworkPolicy CGNAT drift (chart/controller parity).** The chart-side
  default `blockedEgressCIDRs` was missing `100.64.0.0/10` (CGNAT),
  `127.0.0.0/8` (loopback), and `224.0.0.0/4` (multicast) that the
  controller-side list already had. The CGNAT gap specifically affected
  managed Kubernetes offerings (AKS default VNet, some EKS configs, k3s
  default flannel) where `100.64/10` is the pod CIDR — workspace pods on
  such clusters could reach internal pods/services in the CGNAT range,
  defeating cross-tenant isolation.
  ([#517](https://github.com/lenaxia/LLMSafeSpaces/pull/517))

- **runtimeClass webhook admin-gate (S51.2 closure).** The Workspace CRD's
  `spec.runtimeClass` field (Epic 51 S51.1, used for per-workspace gVisor
  opt-out) was documented as "admin-gated, not tenant-selectable" but the
  webhook enforcement was deferred to S51.2. Without it, any user with
  workspace create/update RBAC could set `spec.runtimeClass="runc"` to
  escape gVisor via direct kubectl. The workspace validating webhook now
  rejects `spec.runtimeClass` unless the object carries the annotation
  `llmsafespaces.dev/allow-runtime-class-override: "true"`, applied via
  cluster-admin RBAC.
  ([#518](https://github.com/lenaxia/LLMSafeSpaces/pull/518))

- **JWT iss/aud claims.** JWTs now carry explicit `iss` and `aud` claims,
  minted from `auth.jwtIssuer` / `auth.jwtAudience` (default
  `"llmsafespaces"`), and validated on every parse. Pre-fix tokens carried
  only `sub/jti/exp/iat`, so any service sharing the same HMAC secret could
  mint accepted tokens. Pre-fix tokens are rejected after this change; tokens
  are short-lived (24h default) so rotation is fast.
  ([#519](https://github.com/lenaxia/LLMSafeSpaces/pull/519))

- **Epic 57 US-57.1 — CompositeProvider + prefix-aware local providers
  (foundation for KMS-backed master KEK).** The first of three PRs closing
  the largest unbuilt gap in the threat model: API-pod RCE → permanent KEK
  exfiltration for offline DB decrypt. This PR lands the dispatch mechanism
  (`CompositeProvider` with prefix-sniffing ciphertext routing) and the
  prefix-aware local providers (`lkms:v1:` prefix on new writes, legacy
  un-prefixed blobs still decrypt via fallback). Subsequent PRs add AWS KMS
  and GCP KMS providers and the `migrate-kek` CLI.
  ([#510](https://github.com/lenaxia/LLMSafeSpaces/pull/510) design,
  [#511](https://github.com/lenaxia/LLMSafeSpaces/pull/511) composite,
  [#512](https://github.com/lenaxia/LLMSafeSpaces/pull/512) AWS KMS provider + Go 1.25.12 bump)

### Infrastructure / Tooling

- **Go 1.25.12.** Bumped from 1.25.11 to fix `GO-2026-5856`
  (Encrypted Client Hello privacy leak in `crypto/tls`).
  ([#512](https://github.com/lenaxia/LLMSafeSpaces/pull/512))

- **Supply chain hardening.** Release images are now cosign-signed
  (keyless OIDC, Rekor transparency log). Trivy image scanning runs
  on every built OCI image. Renovate `docker:pinDigests` opens PRs to
  pin Dockerfile FROM lines to immutable digests.
  ([#534](https://github.com/lenaxia/LLMSafeSpaces/pull/534))

- **Documentation site.** New MkDocs Material site at
  https://lenaxia.github.io/LLMSafeSpaces/ — 32 pages across 7
  sections (Getting Started, Operator Guide, Architecture, API
  Reference, Contributing, Reference). Docs-maintenance runbook
  documents content inventory, drift triggers, and procedures.
  ([#527](https://github.com/lenaxia/LLMSafeSpaces/pull/527),
  [#529](https://github.com/lenaxia/LLMSafeSpaces/pull/529),
  [#530](https://github.com/lenaxia/LLMSafeSpaces/pull/530))

- **Chart path cleanup.** `charts/llmsafespaces/` → top-level `/helm`.
  Zero impact on consumers (chart registry URL unchanged).
  ([#526](https://github.com/lenaxia/LLMSafeSpaces/pull/526))

- **New chart values:** `terminal.allowedOrigins`, `auth.jwtIssuer`,
  `auth.jwtAudience`. See the security entries above for usage.

- **Helm chart NetworkPolicy default** now includes the full
  controller-side private-or-internal CIDR set (CGNAT/loopback/multicast),
  not just RFC1918 + link-local.

## [0.2.2] - 2026-07-07

## [0.2.1] - 2026-07-06

## [0.2.0] - 2026-07-06

## [0.1.0] - 2026-07-04

[Unreleased]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.14.2...HEAD
[0.14.2]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.14.1...v0.14.2
[0.14.1]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.14.0...v0.14.1
[0.5.1]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.4.5...v0.5.0
[0.4.5]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.4.4...v0.4.5
[0.4.4]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.4.3...v0.4.4
[0.4.3]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.4.2...v0.4.3
[0.4.2]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.4.1...v0.4.2
[0.4.1]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.2.2...v0.3.0
[0.2.2]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.2.1...v0.2.2
[0.2.1]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/lenaxia/LLMSafeSpaces/releases/tag/v0.1.0
## Placeholder
