# Worklog: docs site — MkDocs Material setup + full content restructure

**Date:** 2026-07-11
**Session:** Stand up the documentation site at https://lenaxia.github.io/LLMSafeSpaces/ using MkDocs Material. Full content restructure of existing README + design docs into a navigable hierarchy. New operator-facing content written for gaps.
**Status:** Complete

---

## Objective

Pre-this-PR the project had no navigable documentation entry point.
README.md is a dense operator+integrator+contributor hybrid. README-LLM.md
is 127 KB of contributor guidance. design/ has 40+ architecture docs with
no index. The threat model lives in a security-report subdirectory.
Operators trying to evaluate the project had to read everything to find
anything.

Goal: a public-facing docs site, organized by audience, that operators
can navigate to find what they need in <30 seconds.

---

## Work Completed

### MkDocs Material setup (`mkdocs.yml`)

- Material theme with light/dark palette toggle, search, navigation tabs,
  sections, breadcrumbs, copy-button code blocks, mermaid diagrams, tabs.
- Plugins: search, tags, git-revision-date-localized (shows last-modified
  per page).
- Nav structure: Home / Getting Started / Operator Guide / Architecture /
  API Reference / Contributing / Reference — seven top-level sections,
  32 pages total.
- `docs/requirements.txt` for reproducible builds.

### Pages written (32 total, ~16k words)

**Getting Started** (3 pages):
- `index.md` — overview, why use it, what a workspace is
- `quickstart.md` — 10-minute kind install, end-to-end
- `concepts.md` — data model (User, Org, Workspace, etc.)

**Operator Guide** (13 pages):
- installation, configuration, storage, networking, security,
  runtime-environments, multi-tenant, oidc-sso, inference-relay,
  monitoring, upgrading, troubleshooting, runbook

**Architecture** (5 pages):
- overview, components, lifecycle, secrets (crypto deep-dive),
  threat-model (full G1-G50 table from epic-17)

**API Reference** (4 pages):
- rest (every endpoint), authentication, mcp, sdks

**Contributing** (5 pages):
- overview, development workflow, engineering rules (Rules 0-12),
  testing, worklogs

**Reference** (4 pages):
- helm-values (every chart value), crds (3 CRDs with full spec),
  cli (10 binaries), changelog

### CI workflow (`.github/workflows/docs.yml`)

Push-to-main trigger on `docs/**`, `mkdocs.yml`, `README.md`,
`CHANGELOG.md` changes. Builds the site and syncs to `gh-pages`
preserving chart artifacts at the root (index.yaml, *.tgz).

Deploy shape: Option A per the design discussion — docs site and chart
registry both live at the gh-pages root. No change to operator `helm
repo add` URLs.

### Verified

- `mkdocs build --strict` produces 43 HTML pages, 6.4MB site, zero errors.
- All cross-page links resolve (broken anchors are non-fatal INFOs).
- All chart paths updated to post-rename `helm/` (sub-agents wrote
  against the pre-rename path; fixed before commit).

---

## Key Decisions

1. **MkDocs Material over Hugo/Docusaurus.** Material is the de-facto
   standard for infra/K8s docs (Cilium, Backstage). Excellent search,
   versioning hooks, API doc generation. Python in CI is the tradeoff;
   worth it for the DX.
2. **Full content restructure, not just docs-as-code.** Rewrote the
   README's dense paragraphs into scannable web pages with admonitions,
   tabs for alternatives, and mermaid diagrams. The README is no longer
   the primary operator doc — the site is.
3. **Design docs stay where they are.** `design/` is historical
   architecture reasoning, not user-facing docs. They're linked from the
   architecture pages but not served as primary content.
4. **`docs/requirements.txt`** pins MkDocs + plugin versions so the CI
   build is reproducible.
5. **Deploy workflow preserves chart artifacts.** When syncing the
   rendered site to gh-pages, the workflow explicitly skips
   `index.yaml`, `*.tgz`, `checksums.txt` so the chart registry isn't
   wiped by a docs rebuild.

---

## Assumptions stated and validated (Rule 7)

1. *`mkdocs build --strict` is the right gate.* Validated — it catches
   broken links, missing nav entries, and config errors. CI runs the
   same command.
2. *Material's unlicensed-mode watermark is acceptable for an open-source
   project under AGPL.* Material is MIT-licensed for the OSS edition;
   the "unlicensed" warning in the build output is a recent commercial
   pivot the project will need to revisit. For now, the OSS edition
   works fully.
3. *Sub-agents can write the bulk content in parallel.* Validated —
   three parallel delegations produced 32 pages in one pass, then I
   fixed chart-path references and verified the build.

---

## Tests

```
mkdocs build --strict   → 43 pages, 0 errors
go build ./...          → clean
```

No Go code changes; docs-only + new workflow YAML.

---

## Files Added

- `mkdocs.yml`
- `docs/requirements.txt`
- `docs/index.md` + 31 section pages
- `.github/workflows/docs.yml`

## Files Modified

- `.gitignore` (added `site/`)

---

## Next Steps

1. Open this PR for review.
2. After approval + merge: the docs workflow publishes to gh-pages
   automatically. First visit will show the rendered site at
   https://lenaxia.github.io/LLMSafeSpaces/.
3. Track follow-ups: versioned docs (mike plugin), interactive API
   explorer (mkdocs-swagger-ui-tag), search analytics.
