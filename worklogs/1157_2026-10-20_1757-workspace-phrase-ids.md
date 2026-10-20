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

## r1 addendum — remediation of the CHANGES_REQUESTED verdict @ 23f01ecd

- **Triggers dead path (Critical 1):** the #1729 gate now resolves
  the RESOLVED UUID (never the raw phrase) and the clobber line is
  gone — the stored key is the UUID. Foreign-phrase denial folds into
  the byte-identical 404 (§4.3). 7 pins in
  triggers_workspace_phrase_1757_test.go.
- **5xx-never-suggests (Correctness 2):** degraded phrase/name
  lookups answer 503 in the middleware AND the trigger surface;
  FindWorkspaceIDByName checks rows.Err() on every cardinality path
  (sqlmock pin on the 2-row swallow).
- **Doc-comment truth (Correctness 4):** WorkspaceRefResolver's
  contract now states exactly what is wired (idGroup only; admin
  :workspaceId deferred); Deprecation/Sunset headers attach on the
  NAME rewrite; ambiguity passes through unrewritten (no suggestions
  on the workspace surface).
- **Backfill redesign (Robustness 1 + Correctness 5):** the DB read
  is PURE (no write, sqlmock pin); legacy rows converge on the first
  AUTHORIZED read — services/workspace GetWorkspace, post-verifyOwner
  (3 service-tier pins incl. denied-caller-never-writes);
  types.Workspace carries phraseId. The migration comment describes
  the real convergence (no sweep job existed to claim).
- **§4.3 masking (Security 1):** phrase-addressed Forbidden folds
  into the byte-identical miss 404 in the access middleware and the
  trigger gate (router pin asserts JSON equality; e2e covers both).
- **Style:** uuid shadow renamed; AssertNotCalled moved after
  ServeHTTP; MintWorkspacePhrase's dead error return removed (the
  bounded retry that can fail lives in EnsureWorkspacePhraseRegistered).
- **Tests added:** 7 trigger pins, 7 router pins, 3 service-tier
  backfill pins, 2 sqlmock pins, 2 e2e flows. Phantom-pin lesson
  applied: every "pinned" comment now names a test that exists.

## r2 addendum (verdict @ bb6310a4 — near-approve; all r1 fixes verified)

- List API: `WorkspaceListItem.PhraseID` + the `ListWorkspaces`
  mapping (the DB query already selected phrase_id; the conversion
  dropped it — same defect class as the #477 OrgID drop). Pinned:
  `TestListWorkspaces_PropagatesPhraseID`.
- SDKs: `phraseId` added to Python/Go/TS/Java Workspace models +
  list items + openapi.yaml. Root cause of the red canary: the
  Python dataclass hard-rejects unknown kwargs (`Workspace(**resp)`).
- `EnsureWorkspacePhraseRegistered`: `deleted_at IS NULL` guard on
  the UPDATE + early ("", nil) on a dead row (no pointless mints, no
  retry burn). Pinned: `TestEnsureWorkspacePhraseRegistered_SoftDeletedRow_NeverWrites`.
- Test hardenings per verdict: creation-mint ASSERTED
  (`TestCreateWorkspace_MintsPhraseAtCreation`), org trigger route
  pinned (`TestTriggerCreate_OrgRoute_PhraseTarget_ResolvesAndPersistsUUID`),
  §4.3 pairing in ONE test (`TestWorkspaceRef_UnknownVsForeignPhrase_ResponsesAreIdentical`),
  inert pure-read pin replaced with an error-armed UPDATE tripwire
  (`TestGetWorkspace_NullPhrase_PerformsNoWrite` — consumed ⇒
  ExpectationsWereMet==nil ⇒ fatal; unconsumed ⇒ the only unmet).
- Doc residues: phantom "backfill sweep" comment (creation path),
  session_ref.go admin-:workspaceId self-contradiction; PR body
  retitle: References #1757 + completion in #1792.
- Gates: `go vet ./api/... ./pkg/...` clean; full `./pkg/... ./api/...`
  sweep green (`-p 2` — the default parallelism exhausts the 15G disk
  at the link stage; environmental, not code). Six named r2 pins
  individually verified -v PASS.

## r3 addendum (single mechanical finding @ 4141330b)

- Dead-row read-back is now NULL-aware (`sql.NullString`, the
  GetWorkspace convention): the documented ("", nil) contract was
  unreachable — a plain-string scan errors on the NULL migration
  000038 leaves on legacy rows. Pin stubbed with the REAL state
  (AddRow(nil)). -v PASS with the fresh-mint and collision pins.
