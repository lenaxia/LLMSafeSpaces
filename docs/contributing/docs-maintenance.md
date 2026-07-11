# Documentation maintenance runbook

The docs site is largely hand-written. This page documents what content
exists, where it comes from, and how to keep it from drifting as the
codebase evolves. **Anyone editing source-of-truth files should consult
this page to find out whether the docs need to follow.**

---

## The drift problem in one paragraph

Most docs pages describe behaviour that lives in source code. When the
source changes, the docs don't auto-update. The AI code reviewer catches
some drift (it caught two real bugs on PR #527 alone: controller flag
renames, helm-values selector key abbreviations), but not all. Drift
accumulates until somebody notices an operator following stale docs and
filing a bug. This page exists to make the maintenance loop explicit
and cheap.

---

## Content inventory

### Reference pages — highest drift risk

These pages mirror source files 1:1 (or close to it). They are the most
likely to drift and should be the first place to look when changing the
source.

| Doc page | Source of truth | Drift trigger |
|---|---|---|
| `docs/reference/helm-values.md` | `helm/values.yaml` | Any new/changed chart value |
| `docs/reference/crds.md` | `pkg/apis/llmsafespaces/v1/*_types.go` (Go struct + kubebuilder tags) + `helm/crds/*.yaml` | Any CRD field change |
| `docs/reference/cli.md` | `cmd/*/main.go` flag definitions | Any flag add/rename/remove |
| `docs/reference/changelog.md` | `CHANGELOG.md` (root) | Every release |
| `docs/api/rest.md` | `api/internal/server/router.go` (route table) + handler files | Any new/changed/removed endpoint |
| `docs/api/authentication.md` | `api/internal/services/auth/auth.go` + `api/internal/middleware/auth.go` | JWT claim changes, auth flow changes |
| `docs/architecture/lifecycle.md` | `controller/internal/workspace/phase_*.go` + `controller/internal/workspace/reconciler.go` | Any phase transition change |

### Operator pages — medium drift risk

These are hand-written prose that references current behaviour. They
need updates when the *behaviour* changes, not when the source comment
changes.

| Doc page | Watch these source paths |
|---|---|
| `docs/operator/installation.md` | `helm/values.yaml`, `helm/templates/NOTES.txt`, `helm/Chart.yaml` (version) |
| `docs/operator/configuration.md` | `api/internal/config/config.go`, `api/config/config.yaml` |
| `docs/operator/security.md` | `design/stories/epic-17-security-review/THREAT-MODEL.md`, `api/internal/middleware/security.go`, `api/internal/config/config.go` (security validations) |
| `docs/operator/storage.md` | `controller/internal/workspace/pod_builder.go` (volume mounts), `helm/values.yaml` (storage section) |
| `docs/operator/networking.md` | `controller/internal/workspace/network_policy.go`, `helm/templates/workspace-network-policy.yaml` |
| `docs/operator/runtime-environments.md` | `runtimes/base/Dockerfile`, `controller/internal/webhooks/workspace_webhook.go` (registry allowlist) |
| `docs/operator/multi-tenant.md` | `controller/internal/webhooks/pod_tenant_quota_webhook.go`, `helm/values.yaml` (gvisor section) |
| `docs/operator/oidc-sso.md` | `api/internal/services/sso/sso.go`, `api/migrations/000038_org_sso_configs.up.sql` |
| `docs/operator/inference-relay.md` | `controller/internal/relay/`, `cmd/relay-router/`, `cmd/relay-proxy/` |
| `docs/operator/monitoring.md` | `helm/dashboards/`, `helm/templates/podmonitor-*.yaml` |
| `docs/operator/upgrading.md` | `CHANGELOG.md`, `api/migrations/` (new migrations = upgrade consideration) |
| `docs/operator/troubleshooting.md` | `docs/operator/runbook.md` (keep these two in sync — troubleshooting is symptom→cause, runbook is procedure) |
| `docs/operator/runbook.md` | `cmd/rotate-kek/main.go`, `api/internal/services/auth/auth.go` (JWT rotation) |

### Architecture pages — low drift risk

These describe system shape. They change rarely but should be audited
when the architecture itself changes (new component, new data store,
etc.).

| Doc page | Audit trigger |
|---|---|
| `docs/architecture/index.md` | New top-level component added or removed |
| `docs/architecture/components.md` | Same |
| `docs/architecture/secrets.md` | Crypto changes in `pkg/secrets/`, new encryption tier, KEK delivery change |
| `docs/architecture/threat-model.md` | New finding in `design/stories/epic-17-security-review/THREAT-MODEL.md`; gap status change |

### Evergreen pages — minimal drift risk

| Doc page | Notes |
|---|---|
| `docs/index.md` | Update only for major positioning changes |
| `docs/getting-started/index.md`, `concepts.md` | Update when the data model changes |
| `docs/getting-started/quickstart.md` | Update if the install flow changes (e.g. new bootstrap script) |
| `docs/contributing/*` | Update when the dev workflow changes |

---

## Maintenance triggers

Whenever you do any of the following, check this table for doc
maintenance obligations:

| Source change | Docs that need updating | Who notices |
|---|---|---|
| New chart value in `helm/values.yaml` | `docs/reference/helm-values.md` | Code reviewer + AI reviewer |
| New/changed/removed flag in any `cmd/*/main.go` | `docs/reference/cli.md` | Code reviewer + AI reviewer |
| New/changed/removed API route in `router.go` | `docs/api/rest.md` | Code reviewer + AI reviewer |
| CRD field add/rename in `pkg/apis/llmsafespaces/v1/*_types.go` | `docs/reference/crds.md` | Code reviewer |
| New security gap closed/opened in `THREAT-MODEL.md` | `docs/architecture/threat-model.md` + `docs/operator/security.md` | Worklog should mention |
| New migration in `api/migrations/` | `docs/operator/upgrading.md` (if the migration has upgrade considerations) | PR author |
| Crypto change in `pkg/secrets/` | `docs/architecture/secrets.md` + `docs/operator/security.md` + `docs/operator/runbook.md` | PR author |
| Bootstrap script change in `local/*.sh` | `docs/getting-started/quickstart.md` | PR author |
| Release tag cut | `docs/reference/changelog.md` (copy `CHANGELOG.md` entry) | Release process |

---

## Maintenance procedures

### Procedure A: Update the Helm values reference

When you add or change a chart value in `helm/values.yaml`:

1. Open `helm/values.yaml` and find the value you changed.
2. Open `docs/reference/helm-values.md` and find the section
   corresponding to the value's parent block.
3. Update the table row: type, default, description.
4. If the value has security implications, add a note in
   `docs/operator/security.md` linking to the helm-values entry.
5. Run `mkdocs build --strict` locally to catch broken anchors.

**Time budget:** 5–10 minutes per value change.

### Procedure B: Update the CLI reference

When you add, remove, or rename a flag in any `cmd/*/main.go`:

1. Read the flag definition and its `flag.{Type}Var` call.
2. Open `docs/reference/cli.md`, find the section for that binary.
3. Add/modify/remove the row in the flags table.
4. If the flag replaces an old one, note the rename in the description
   (operators may have scripts referencing the old name).
5. Run `mkdocs build --strict` locally.

**Time budget:** 2–5 minutes per flag.

### Procedure C: Update the REST API reference

When you add or change a route in `api/internal/server/router.go`:

1. Open `router.go` and find the route registration.
2. Open `docs/api/rest.md`, find the category (Auth, Workspaces, etc.).
3. Update the route table.
4. If the route has new auth requirements or returns a new shape, update
   the surrounding prose.
5. Run `mkdocs build --strict` locally.

**Time budget:** 5–15 minutes per route, depending on whether the
semantics changed.

### Procedure D: Update the threat model

When the security gap table in
`design/stories/epic-17-security-review/THREAT-MODEL.md` changes:

1. Open the threat model design doc and find the changed rows.
2. Open `docs/architecture/threat-model.md`.
3. Update the gap register table (mirror the status: Fixed / Open / Accepted).
4. Update the summary counts ("21 Fixed / 22 Open / 7 Accepted").
5. If a previously-Open gap is now Fixed, add a callout to
   `docs/operator/security.md` describing the operator-visible change.
6. Run `mkdocs build --strict` locally.

**Time budget:** 10–20 minutes per gap status change.

### Procedure E: Update the changelog page

When cutting a new release:

1. Append the new release's section to `CHANGELOG.md` (root).
2. Mirror the same content into `docs/reference/changelog.md`.
3. Verify the GitHub Release notes match.

**Time budget:** 5 minutes per release.

### Procedure F: Rebuild the local site

Before pushing docs changes:

```bash
# Install deps if missing
pip install -r docs/requirements.txt

# Build with strict mode (catches broken links + missing nav entries)
mkdocs build --strict

# Serve locally to eyeball the rendering
mkdocs serve
# → open http://127.0.0.1:8000
```

`mkdocs build --strict` is the CI gate. Run it before every push.

---

## CI enforcement

The `.github/workflows/docs.yml` workflow builds the site on every
push that touches `docs/**`, `mkdocs.yml`, `README.md`, or
`CHANGELOG.md`. It runs `mkdocs build --strict` (configurable). A
broken link or missing nav entry fails the build.

What CI does **not** catch today (track as future work):

- Helm values that exist in `values.yaml` but not in
  `docs/reference/helm-values.md`.
- CLI flags that exist in `cmd/*/main.go` but not in
  `docs/reference/cli.md`.
- Routes that exist in `router.go` but not in `docs/api/rest.md`.

These would need custom linters. They are the highest-value follow-up
for drift prevention. Track them in a single issue when you have time.

---

## When in doubt

- **If you change source and don't know which docs to update**, read
  the Content Inventory tables above and follow the source-of-truth
  pointers.
- **If you change docs and don't know if you got it right**, run
  `mkdocs build --strict` locally and ask the AI reviewer on the PR.
- **If the docs and the source disagree, the source is right** — open
  a docs PR to catch up. Do not "fix" the source to match stale docs.

---

## Audit cadence

- **Per PR:** check the Maintenance Triggers table; update the docs in
  the same PR when source-of-truth files change.
- **Per release:** run through Procedures A–E end-to-end. ~1 hour.
- **Quarterly:** audit the architecture pages for shape changes that
  per-PR updates may have missed. ~2 hours.

The audit cost is small if the per-PR discipline holds. The audit cost
is large if it doesn't — that's how the previous docs state happened
(README at 524 lines, README-LLM at 1905 lines, no navigable entry
point).
