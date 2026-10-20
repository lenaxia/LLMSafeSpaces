# Worklog: #1757 — workspace phrase IDs (2 words + 1 digit, fleet-unique)

**Date:** 2026-10-20
**Session:** continuation of the #1691/#1756 worker (same lane). Branch `feat/1757-workspace-phrase-ids-stacked` (stacked on #1775 head 114fbb76 — #1757 consumes its frozen wordlists; union-merge of main planned per orchestrator). Worktree `wt-1757`.
**Status:** Complete — PR open, orchestrator merges on APPROVED

---

## Objective

#1757 per the dispatched rulings: every workspace gains a minted 2-word + 1-digit phrase (`amber-river-7`) as its display/address identifier; the UUID stays the internal key. Fleet-unique via check-and-retry; inputs accept phrase, name (deprecation window), or UUID; `targetWorkspaceId` (routine triggers) accepts the phrase.

---

## Work Completed

### Mint + classification (pkg/shortid, stacked on #1756)

`MintWorkspacePhrase` — 2 words from the #1756 frozen adjective+noun lists + one digit (0-9), under the same mint lock (thread-safe after #1775's race fix). `IsWorkspacePhraseForm` = exactly `word-word-digit`, case-insensitive. **Shared-list rule PINNED:** workspace phrases share the session wordlists (already frozen, harness-slug-excluding); disjointness is BY SHAPE — digit slot vs the harness's no-digit 2-word slugs; 2-vs-3 word count vs session phrases. `TestWorkspacePhrase_NamespaceDisjointness` pins all three pairwise. The mint pin allows birthday duplicates (307k space) — uniqueness is the index's job, verified ≥280 distinct/300 draws.

### Persistence (migration 000038, api+helm synced)

`workspaces.phrase_id text` + global partial unique index (`WHERE phrase_id IS NOT NULL AND deleted_at IS NULL` — fleet-unique among live workspaces). `EnsureWorkspacePhraseRegistered`: NULL-guarded UPDATE + read-back, 23505 → fresh draw (no counter suffix — the digit IS the disambiguator slot). `FindWorkspaceIDByPhrase` (normalized, live-only), `FindWorkspaceIDByName` (owner-scoped, unambiguous-only — names are caller-owned). sqlmock pins for all three incl. collision retry and ambiguity miss.

### Mint points + emission

Creation mints best-effort after the metadata insert (backfill converges); `GetWorkspace` lazy-backfills NULL rows on read; `GetWorkspace`/`ListWorkspaces` surface `phraseId` on the metadata (pinned).

### Addressing

`WorkspaceRefResolution` middleware on the `:id` group BEFORE the access gate: phrase → UUID rewrite (miss = structured 404); UUID passes byte-identical (shape guard, no probe); unambiguous NAME rewrites (userID from the auth context — the access middleware runs after and needs the rewritten UUID). Router pins: phrase-resolves-before-gate, miss 404, UUID passthrough, name rewrite. Routine triggers' `targetWorkspaceId` accepts the phrase (resolved to UUID before the #1729 ownership gate; stored key stays UUID) — wired in app.go for both user/org handlers.

---

## Key Decisions

- **Stacked branch**: #1757 consumes #1756's wordlists — building off main was impossible until #1775 merges; per orchestrator, union-merge main (never rebase).
- **No counter suffix for workspace collisions** — the digit slot already provides 10-way disambiguation; a fleet collision re-draws (workspaces are fleet-scarce).
- **Name acceptance is unambiguous-only and owner-scoped** — names are caller-owned and mutable; ambiguity is a miss, never a guess.

---

## Blockers

None (review availability depends on the trusted-app-bot fix reaching this PR's branch — same class as #1775).

---

## Tests Run

- `go test ./api/... ./pkg/...` — full sweep, zero FAILs.
- `go test ./api/internal/services/database/` — green incl. the four new #1757 sqlmock pins.
- `go test ./api/internal/server/ -run "TestWorkspaceRef|..."` — the four router pins green.
- `golangci-lint run` on all touched packages — 0 issues (one staticcheck comment-form fix).

---

## Next Steps

- Review rounds; the {workspace-phrase, session-phrase} cross-workspace SSE/SDK surfaces ride the #1756 §5 SDK-major follow-up.

---

## Files Modified

- pkg/shortid/{phrase.go, phrase_test.go}
- api/migrations/000038_workspace_phrase_id.{up,down}.sql (+ helm sync)
- api/internal/services/database/{database.go, short_session_ids.go, workspace_phrase_test.go, database_test.go}
- api/internal/interfaces/interfaces.go, api/internal/mocks/database.go
- api/internal/middleware/session_ref.go
- api/internal/server/{router.go, router_workspace_ref_test.go, router_session_ref_test.go}
- api/internal/handlers/triggers.go
- api/internal/services/workspace/{workspace_service.go, workspace_service_test.go, default_image_read_guard_test.go}
- api/internal/app/{app.go, e2e_suspend_test.go}, api/internal/services/auth/{auth_e2e_all_test.go, auth_sessionid_test.go}
- pkg/types/workspace.go
- worklogs/NNNN_... (this file), CHANGELOG.md
