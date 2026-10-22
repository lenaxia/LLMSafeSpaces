# Worklog: #1831 — board entity phrase-ids (session-ID playbook), takeover r1

**Date:** 2026-10-22
**Branch:** `feat/1831-board-entity-phrase-ids` (off `557dd9db`, union-merged to `538caeef` = origin/main at push)
**Status:** PR opening (r1)
**Worker:** takeover from parked worker shiny-moon (implementation ~90% inherited uncommitted; this lane finished docs/red-evidence/verification/merge/push per the parked plan)

---

## Objective

Issue #1831: board project/epic/task short ids migrate from random
opaque forms (`tsk-vlfuvcil`) to word-phrase ids
(`tsk-amber-falcon-glides`) following the validated session-ID
playbook (#1756/#1790): additive dual-accept window → emission flip →
later deprecation removal; reverse-resolve case-insensitively;
did-you-mean keyed to edit distance over WORDS; MCP tool schemas/docs
updated. Internal UUID PKs stay (addressing/vocabulary change only).

## What shipped (all inherited-uncommitted work verified + completed)

### pkg/board

- `ids.go`: phrase mint (`shortid.MintPhrase`, prefix kept, closed
  kind map prj-/epc-/tsk-), shape classifiers `IsPhraseIDForm` /
  `IsLegacyBoardIDForm` / `IsDeprecatedBoardID`, word-token bounded
  Levenshtein (`wordEditDistanceBounded`), vocabulary-dispatching
  `suggestCandidateScore` (phrase candidates: min(word, char)
  distance; legacy + titles: char ranker only).
- `store.go`: `mintUnique` collision ladder (base, base-2 … base-5,
  re-draw bounded ×3) + `taskMintOwnerScope` (owner-wide uniqueness
  for epc/tsk — per-project UNIQUEs remain DB backstops); scan-time
  `ShortIDDeprecated` marking.
- `rows.go`/`reads.go`: `ShortIDDeprecated` field on Project/Epic/Task,
  set at scan time so every read path (REST/MCP/agentd render rows
  verbatim) carries it; `ListEpics` marks too.
- `service.go`/`plan.go`: epic/task mints (create + import-plan) use
  the owner-wide scope.
- Migration `000043_board_phrase_ids.{up,down}.sql`: CHECK widens to
  accept both forms (down restores random-only, fails if phrase rows
  exist — best-effort by design).

### pkg/mcp + agentd + frontend + docs

- `pkg/mcp/board_tools.go`: all tool descriptions phrase-first,
  legacy noted deprecated-but-resolving.
- `cmd/workspace-agentd/mcp_server.go`: worker + manager tool
  descriptions phrase-first (takeover fixed the `tk-X` typo →
  `tsk-XXXXXXXX`, 4 sites).
- `frontend/src/lib/boardRules.ts`: `wordDistanceBounded` +
  `isBoardPhraseId` mirror of the server ranker; `boardRules.test.ts`
  4 new cases; TaskDialog dependency placeholder → phrase example
  (takeover).
- `api/internal/server/router.go` board-routes comment,
  `docs/api/mcp.md` `board_get` row, `design/0067` §4.1 supersession
  note + header summary (takeover).

## Red-first evidence (captured this lane, pre-implementation trees)

1. **Go behavioral red** (impl files stashed, tests kept —
   `go test ./pkg/board -run 'TestSuggestWordDistance|...'`):
   ```
   --- FAIL: TestSuggestWordDistance (0.00s)
       ids_phrase_suggest_unit_test.go:24: one-word drift must suggest the phrase id (char distance 4 > bound 2), got []
       ids_phrase_suggest_unit_test.go:33: two-word drift (bound 2) should still suggest
   --- FAIL: TestSuggestMixedFleet (0.00s)
       ids_phrase_suggest_unit_test.go:78: phrase needle must suggest only the phrase candidate, got []
   ```
2. **Go compile red** (unit + `go vet -tags=integration`):
   `undefined: IsPhraseIDForm / mintBoardID / IsLegacyBoardIDForm / IsDeprecatedBoardID` —
   `FAIL github.com/lenaxia/llmsafespaces/pkg/board [build failed]`.
3. **Frontend red** (boardRules.ts stashed, vitest):
   ```
   × suggestBoardItems over phrase ids (#1831) > suggests a one-WORD drift (character distance out of bound)
   AssertionError: expected [] to include 'tsk-meek-lantern-ambles'
   Tests  1 failed | 63 passed (64)
   ```

## Verification (this lane)

- `go build ./...` clean.
- `go test ./pkg/board/... ./pkg/mcp/...` — ok (0.004s / 2.5s).
- `go test ./cmd/workspace-agentd/...` — ok (372s; subpackages cached).
- `go vet -tags=integration ./pkg/board ./pkg/mcp` — clean.
- `npx tsc --noEmit` (frontend) — clean.
- `npx vitest run --maxWorkers=8 --minWorkers=2` — **210 files / 2383
  tests all green**. NOTE: the default-worker full run (64-way) fails
  en masse with `document is not defined` in this pod — a
  concurrency/environment artifact, NOT a code regression: single-file
  runs pass with and without this lane's changes, and the capped-worker
  full run is green. CI (its own runners) runs plain `npx vitest run`.
- DB-backed legs (integration tests, migration) run in the
  secrets-integration CI job (no local Postgres in this pod); both
  files compile under `-tags=integration` and skip cleanly without a
  DB.

## Follow-ups (disclosed, out of lane)

- Cross-table owner-wide uniqueness is a mint-side guard only (epc vs
  tsk share the phrase space per-prefix… actually disjoint prefixes —
  the gap is prj/(epc,tsk) owner-retarget edge on `prj` scope);
  a DB-level cross-table constraint + manager project-retarget edge
  disclosed on the issue plan comment as follow-up.
- Window exit migration (narrow the CHECK, drop legacy resolution)
  ships after one release with no legacy rows remaining.

## Takeover notes

- Foreign stash `stash@{0}` (feat/1747) verified untouched throughout
  (checked after every stash dance: list shows only the foreign entry,
  porcelain count back to expected).
- Main moved `557dd9db → 538caeef` during the lane; union-merged
  before push (no overlap: main's delta = pr-review prompts +
  ghaworkflows tests + worklog 1173; CHANGELOG untouched by main so no
  union needed). Main's pr-review lane claimed worklog number 1173 —
  this worklog ships under the `NNNN_` sentinel (the post-merge bot
  assigns the final number; uniqueness check re-run empty before push).

## r2 addendum — migration renumber 000042 → 000043

CI caught a same-cycle migration-version race: main's #1853 (merged
34da5751, after this lane's fetch of 538caeef and concurrently with
the push) healed its duplicate-41 as `000042_board_config` — colliding
with this lane's `000042_board_phrase_ids` (repolint migrations
sequence + TestLive_Migrations_NoCollisionsOrGaps reddened on the PR).
Fix: union-merge of 34da5751 + renumber this lane's migration to
`000043_board_phrase_ids` (api/ + helm/ mirrors + doc references).
Also: main's board-config lane claimed worklog number 1174 — the
NNNN_ sentinel discipline absorbed both collisions without a renumber
on my side.

## r3 addendum — review findings remediated + integration seed legs green-up

PR Review (run 39822809981, against pre-merge commit) findings and
disposition:

1. "Integration tests fail CI (non-uuid ids)" — fixed across
   4146f987/ab1f76e1/acc1575a/2cfcb7e3 (uuid literals, explicit
   param casts, valid legacy-form seed short_ids, one epic per
   project for the tasks FK).
2. "arbitration test calls mintUnique (random) while asserting a
   forced base" — REAL: the deterministic pin now calls
   `arbitrateMint` directly (base→base-3 owner-wide), plus a
   p1-scoped sanity pin proving the per-project scope would hand out
   the sibling-held base-2 (the invisible-collision regression the
   owner-wide scope exists to prevent).
3. "Public contract stale: sdks/openapi.yaml + SDK canary/contract
   suites still require the 8-char form" — fixed: 7 openapi patterns
   dual-accept (phrase | legacy | uuid per surface), response schemas
   carry `short_id_deprecated`, descriptions/examples phrase-first;
   hurl contract asserts the PHRASE form on fresh creates (emission
   pin at the SDK tier); go/python/typescript canary shape regexes
   flipped to the phrase form. `make -s openapi-validate` green.
4. "Lint red: duplicate migration 41" — already absorbed by the r2
   union-merge + renumber to 000043 (review ran pre-merge).
5. "Slug aliases unaddressed" — disclosed as follow-up: the board's
   dual-accept window already provides the session-playbook alias
   equivalent (legacy form keeps resolving, case-insensitively);
   title-slug aliases are an additive UX layer, not a window blocker.
