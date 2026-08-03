# Changelog

All notable changes to LLMSafeSpaces are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.5.1...HEAD
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
