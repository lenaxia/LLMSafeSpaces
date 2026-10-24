# Worklog: #1755 — archive/unarchive cascades to child sessions

**Date:** 2026-10-24
**Branch:** `fix/1755-archive-cascade`
**Status:** PR opened; iterating to LATEST FORMAL APPROVED

---

## Objective

Issue #1755 (owner request, 2026-10-19): archiving a session must
cascade over the parentID subtree (worker sessions archive as one unit
with their spawned subagent children); unarchive cascades the release.
Open question (sticky vs mirror) resolved in-lane per the owner's
recommendation: **sticky** — children archived INDIVIDUALLY keep that
state across the parent's round-trip.

## Validate-first (the cascade gap still reproduces on origin/main)

- The 2026-10-19 bot comment claiming implementation (commit `1ea9fddc`,
  branch `opencode/issue1755-20261019174205`) was **vaporware**:
  `git cat-file -t 1ea9fddc` → not a valid object; the branch does not
  exist on the remote; no PR was ever opened (`gh pr list --search 1755`
  → empty). The 2026-10-23 triage ("verified not implemented") stands.
- Landed adjacent work does NOT cover the gap:
  - #1882/PR1887 + PR1897 (worklogs 1202/1213): DELETE propagation —
    `DeleteSessionTree` and `MarkSessionIndexDeleted` already walk the
    recursive parentID tree. Delete cascades; **archive does not**.
  - #1879/PR1914 (worklog 1214): archive read-path durability
    (stale-set markers + post-write verification) — single-session
    write semantics unchanged.
- Pinned on origin/main @ 530718ce3: `SetSessionArchivedStatus`
  (api/internal/services/database/database.go:1377) is a single-row
  `UPDATE session_index SET archived = $3 WHERE workspace_id = $1 AND
  session_id = $2` — no tree walk, no marker. **Partial-solution
  residual precisely pinned: delete cascades, archive doesn't.**

## Fix design

**One recursive-CTE UPDATE per direction** inside
`SetSessionArchivedStatus` — the single choke point both surfaces
(REST `PUT /workspaces/:id/sessions/:sessionId/archived` and pod
`POST /internal/v1/session-archive`, the agentd `session_archive` MCP
tool) already funnel through (`workspace.SetSessionArchived` →
`sessionindex.SetArchived` → `database.SetSessionArchivedStatus`).
Zero interface churn; atomic (single statement, one round-trip).

- **Sticky marker** (migration 000048, additive `boolean NOT NULL
  DEFAULT false` on `session_index`): pre-existing archived rows read
  as individually held — a cascade never releases state it did not set.
- **archive(target):** target → `archived=true, marker=false`
  (individual); unarchived descendants → `archived=true, marker=true`.
  Already-archived descendants skipped, not re-stamped — the skip
  preserves individual intent.
- **unarchive(target):** target released; descendants released only
  when `archived=true AND marker=true`.
- **CTE walks with UNION (dedupe)** — parentID is a tree by
  construction, but a corrupted parentID cycle terminates instead of
  hanging (pinned by integration test). Workspace scoping enforced at
  the CTE anchor, recursive member, and outer UPDATE (pinned).
- **404 contract preserved:** absent anchor → empty descendants set →
  0 rows affected → `NotFoundError`; no phantom row (pinned).

Documented granularity of the single-marker design (pinned by test):
in P→C→G, archiving C individually stamps G cascade-held; a parent
round-trip then releases G while C stays archived. Individual intent
survives; cascade-of-cascade attribution does not — per-holder
tracking judged complexity for the edge (matches the issue's KISS
steer and the owner's sticky recommendation).

Composes with landed work: the #1879 agentd post-write read-back
checks the TARGET only (`confirmed[sessionID] == archived`) — still
correct; pods converge on descendant state via the existing
archived-set fetch (unchanged surface). Listing behavior untouched
(#1735 filter reads the same `archived` column).

**Residual (flagged, out of scope):** `PublishSessionArchived`
announces only the named session on SSE; open tabs converge
descendants on the next list refetch, not instantly. Per-descendant
SSE would widen the DB interface for affected IDs — left for a
decision, as in-issue scoping was state + listings.

## TDD evidence

RED commit `9c2e98d31`: integration suite
(`TestIntegration_SessionArchiveCascade_*` — subtree archive + marker
semantics, reverse cascade, sticky individual child, re-sweep,
leaf-only, absent-row 404 no-phantom, cycle-corruption termination,
granularity edge, workspace scoping) + rewritten sqlmock call pins +
`TestSetSessionArchivedStatus_SQLStructure` — all failing against the
single-row impl (`could not match actual sql: "UPDATE session_index
SET archived = $3..."`), verified locally. GREEN: migration 000048 +
the cascade UPDATE; unit suites green
(database/sessionindex/workspace/handlers/server), `gofmt`/`go vet`
clean, `go run ./cmd/repolint` clean (helm mirror synced via
`make chart-sync-migrations`).

## Files

- `api/internal/services/database/database.go` — the cascade UPDATE
- `api/migrations/000048_session_archive_cascade.{up,down}.sql`
- `helm/migrations/` — byte-synced mirror
- `api/internal/services/database/session_archived_test.go` — pins
- `api/internal/services/database/session_archive_cascade_integration_test.go`

Not security-sensitive: no `pkg/redact`, RBAC, CRD schema, or secrets
handling touched (plain additive column on an API-owned index table).
