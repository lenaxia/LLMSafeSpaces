# Worklog: #1332/#1333 — dev-preview public FQDN + bare-port 308 redirect

**Date:** 2026-09-11
**Session:** opencode (main dev box). Incident-driven: the 2026-09-10/11
ses_f72a8bb48ffe / ws 1f4e68af "dev tunnel not passing CSS" report.
**Status:** Complete (unit/integration; e2e legs itemized under Validation)

---

## Objective

Two production defects from the incident, per the user's direction ("the
dev preview URL should always be the true fqdn that is publicly facing"):

1. **#1332** — agentd's `dev_preview_url` MCP tool emitted
   `http://llmsafespaces-api.llmsafespaces.svc:8080/api/v1/workspaces/1f4e68af-…/dev-preview/3000/`:
   an in-cluster svc URL handed to a browser user. The user hand-rewrote
   the host, dropped the invisible trailing slash, and the page rendered
   unstyled (see 2). Root cause: in sidecar mode the tool runs in the
   agentd sidecar, whose env has `LLMSAFESPACE_API_URL` = in-cluster
   (load-bearing for the sidecar boot phase) and **no**
   `PREVIEW_ORIGIN_BASE_DOMAIN` — the controller wired both only to the
   main container (`pod_builder.go`), so the sidecar tool fell to
   path-mode with the svc origin. Single-container path-mode deployments
   with an internal `APIServiceURL` had the same bug.
2. **#1333** — the path tunnel served the document at
   `/dev-preview/3000` (no trailing slash) without correction; the
   browser resolved the app's RELATIVE assets (`style.css?v=3`,
   `src/main.js?v=2` — unchanged pre/post app refactor, verified against
   git `adecae6`/`7c25ab6`) one directory up, stripping the port segment;
   the resulting `/dev-preview/style.css` request failed port parsing
   (`400 port must be numeric`). Gin's RedirectTrailingSlash cannot fix
   this: the `/dev-preview/*portPath` catch-all matches the slashless
   form.

## Assumptions (stated, then validated — Rule 7)

1. The in-cluster `LLMSAFESPACE_API_URL` on the sidecar is load-bearing
   for its boot phase (bootstrap+materialize) and must not change.
   **Validated:** `agentd_sidecar.go` comment + boot flow design 0051
   step 1; the fix adds a separate env rather than repointing it.
2. No consumer depends on the tool emitting cluster-internal URLs.
   **Validated:** the only consumer of the output is the chat UI's
   `LSP_DEV_PREVIEW_V1` button parser (requires absolute URLs — #977
   regression test) and the human reading the markdown link.
3. Redirecting only the BARE port form is safe; deeper slashless paths
   (`/…:port/app.js`) are legitimate asset requests.
   **Validated:** existing test `TestDevPreviewHandler_WSUnreachablePort_502`
   exercises `/5173/ws` (slashless subpath) and initially broke under a
   suffix-based redirect — narrowed to the exact `/<port>` form (test
   failure → fix, TDD doing its job).
4. Public-origin derivation `https://api.<baseDomain>` matches the
   preview handler's convention. **Validated:** same convention already
   in `pod_builder.go` (Epic 68 prerequisite) and `mcp_server.go`'s old
   derivation branch.

## Work Completed (TDD — tests written first, red, then implemented)

### Fix 1 — public FQDN, always (#1332)

- `cmd/workspace-agentd/mcp_server.go`:
  - `mcpDevPreviewURL` now returns `(string, error)`; origin resolution
    moved to `mcpPublicAPIOrigin()`: `LLMSAFESPACE_API_PUBLIC_URL` →
    `LLMSAFESPACE_API_URL` → `https://api.<PREVIEW_ORIGIN_BASE_DOMAIN>`
    (derivation LAST — preserves the pre-existing API_URL-wins order the
    old tests pin).
  - `assertPublicAPIOrigin` REFUSES cluster-internal origins — `.svc`,
    `.svc.cluster.local`, `.cluster.local`, `localhost`, and
    loopback/RFC1918/link-local/unspecified IPs — with an error naming
    the fix (`--api-public-url` / Helm `api.publicUrl`). The tool's
    contract is a browser-usable URL; misconfiguration fails loudly
    instead of handing users dead links. Empty-everywhere also errors
    (kills the relative-link regression class from #977 for good).
- `controller/internal/workspace/reconciler.go`: new `APIPublicURL`
  field + `publicAPIURL()` helper (explicit → base-domain derivation →
  empty) — single derivation point.
- `controller/internal/workspace/agentd_sidecar.go`: sidecar env gains
  `LLMSAFESPACE_API_PUBLIC_URL` + `PREVIEW_ORIGIN_BASE_DOMAIN` (when
  configured); `LLMSAFESPACE_API_URL` unchanged (boot invariant).
- `controller/internal/workspace/pod_builder.go`: main container gains
  `LLMSAFESPACE_API_PUBLIC_URL`; existing Epic-68 `LLMSAFESPACE_API_URL`
  override untouched.
- Flag/chart plumbing: `controller/main.go` `--api-public-url`;
  `controller.go` `SetupControllers` signature; `helm/values.yaml`
  `controller.apiPublicURL`; `controller-deployment.yaml` renders the
  flag when set.
- Tests: tool resolution matrix (public override path+origin mode,
  override-beats-derivation, svc/cluster.local/localhost/RFC1918
  refusal incl. a misconfigured public var, no-origin error);
  sidecar/main env tests (explicit, derived, unset; boot-coordinate
  preserved); pre-existing port-policy test pinned with a public origin
  (it previously relied on the buggy relative-link emission for its
  boundary case).

### Fix 2 — bare-port 308 (#1333)

- `api/internal/handlers/dev_preview.go`: after port validation (invalid
  / denied ports stay 400 — normalization never masks validation), a
  request whose `portPath` is exactly `/<port>` gets
  `308 Permanent Redirect` to `<path>/?<query>`. Pure URL rewrite before
  any workspace lookup (asserted: zero getter calls).
- Tests: bare port → 308 + Location; query preserved; invalid/denied
  bare forms still 400; real subpath (`/5173/index.html`) not
  redirected; `/5173/ws` (slashless subpath) NOT redirected (the
  regression my first, too-broad suffix check introduced — caught by
  the existing 502 test, narrowed).

## Validation

- `go test ./cmd/workspace-agentd/ -count=1` — 260s, full suite green.
- `go test ./controller/... -count=1` — green.
- `go test ./api/internal/handlers/ -count=1` — green.
- `go test ./helm/... -count=1` and `go test ./local/ -run TestDevPreviewScript` — green.
- `go build ./...`, `go vet`, `golangci-lint run` (0 issues), `make fmt-check` — clean.
- Kind e2e: `local/dev-preview-tunnel-e2e.sh` (wired into `e2e-nightly.yml`
  after the us-70 revisions rows) covers both issues' acceptance legs:
  308+query preservation, redirect-follow HTML, relative CSS through the
  tunnel, 400-outranks-redirect, tool fail-loud, and the full
  flag→controller→pod-env→tool wiring chain (controller patched with
  `--api-public-url`, pod recreated). Not executed against a live kind
  cluster in this session — first nightly run post-merge is the proof.

## Review iteration 2 (PR #1334, review bot findings)

1. **Resolution order (#1332 spec compliance)** — implemented
   `PUBLIC_URL → API_URL → derived` with fail-fast; the distinguishing
   topology (PUBLIC unset × internal API_URL × base domain set) errored
   where the spec derives. Fixed: an internal/unparseable `API_URL` is
   now SKIPPED (fall-through to the derivation) — the legitimate sidecar
   topology yields a working URL. An explicitly-set-but-internal
   `PUBLIC_URL` still hard-errors (a misconfigured dedicated knob must
   fail loud; pinned by test). New tests: internal-API_URL fall-through,
   unparseable-API_URL fall-through.
2. **Error text named a nonexistent Helm value** (`api.publicUrl`) —
   three literals had already drifted from the chart's
   `controller.apiPublicURL`. Fixed via one shared constant
   (`apiPublicOriginHint`) + a test asserting the correct value appears
   and the old one is gone.
3. **Dotless in-cluster hostnames** (`http://api-name:8080` — the
   same-namespace K8s DNS form) passed the suffix/IP checks.
   Fixed: a dotless non-IP host is refused (never publicly resolvable).
   Refusal table extended: dotless host, IPv6 loopback `[::1]`, IPv6
   link-local `[fe80::1]`.
4. **E2E legs (hard gate)** — added `local/dev-preview-tunnel-e2e.sh`
   (kind, us-70 harness conventions) + nightly wiring + structure pin
   tests (`local/dev_preview_script_test.go`: bash syntax, per-leg
   assertion fragments, UUID workspace contract, restore-the-controller
   patch, workflow wiring).
5. **Helm render test gap** — `helm/controller_api_public_url_test.go`
   pins `--api-public-url` absent by default, exact-value render when
   `controller.apiPublicURL` is set.
6. **Docs contract** — `docs/reference/cli.md` + `docs/reference/helm-values.md`
   gained the `--api-public-url` ↔ `controller.apiPublicURL` pair.
7. **Worklog numbering** — renamed `0913_` → `NNNN_` sentinel (the
   post-merge bot assigns the real number; pre-commit hook enforces).
8. **Claimed pre-existing failure not reproduced** — the review cited
   `TestOutboxDeliver_V2NoPromotionNeverFalselyCompletes`
   (`api/internal/handlers/proxy_outbox_verify_test.go:547`) failing
   deterministically 5/5. Could not reproduce on this head:
   standalone `-count=5` green, `-race -count=3` green, full
   `./api/internal/handlers/` suite green twice (~113–116s), and the
   branch's CI "Test (full suite, race detector)" job is green.
   Documented here rather than "fixed" — there is no failure to fix in
   this environment; if the reviewer sandbox has a reproducible seed,
   that's a separate issue with the steps to trigger it.

## Deployment verification (post-merge)

1. Helm: set `controller.apiPublicURL=https://api.safespaces.dev`
   (or rely on the previewOrigin derivation), upgrade, then recreate a
   workspace pod and confirm sidecar env carries
   `LLMSAFESPACE_API_PUBLIC_URL` + `PREVIEW_ORIGIN_BASE_DOMAIN`.
2. In-session agent calls `llmsafespaces_dev_preview_url` → output
   starts `https://api.safespaces.dev/…`, no `.svc`.
3. Load the emitted URL with and without the trailing slash — both
   render styled (the redirect covers the slashless form).
