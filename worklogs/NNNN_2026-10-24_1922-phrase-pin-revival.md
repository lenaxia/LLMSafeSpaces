# NNNN — #1922 validate-first: phrase pins revived after the #1849 create-contract collision (test-only)

Issue: https://github.com/lenaxia/LLMSafeSpaces/issues/1922 (CLOSED fixed by #1930 — this lane's charter is the validate-first sweep of current main)

## What this lane was chartered to do, and what validation found

VALIDATE-FIRST ruling: reproduce on CURRENT origin/main; pin what STILL
leaks; items already fixed by landed work → report evidence instead of
coding. Sweep result on main @ 530718ce3:

- **The #1922 leak itself is FIXED by #1930** (11b6a489e, merged
  2026-10-24 09:35 UTC). All five phrase pins pass once fixtures satisfy
  the (later) create contract: warm/cold assignee translation, actor
  translation, replay-echo translation, TaskStart echo, SSE actor +
  payload translation. Full `-race -tags=integration ./pkg/board/...`
  green; handlers SSE + worker/manager semantics green.
- **No new raw-ID leaks** in the post-#1930 landings (#1904, #1931,
  #1933): #1904's diffs are create-contract teaching (MCP schema text,
  openapi requireds, TaskDialog client-side gating) with zero session-id
  emission; `board_transitions.actor` remains write-only (no read path
  serves it); the documented `claim.Assignee` probabilistic window is
  unchanged and was accepted in #1930's review.
- **But three of the five #1930 pins are DEAD on main** — a fixture
  collision, NOT a leak regression:

## The collision (reproduced RED on main before any code)

#1904 (feat/1849-seed-validation, merged AFTER #1930 the same morning)
made the create contract atomic: a task CREATE requires a non-empty
description AND ≥1 acceptance criterion (`pkg/board/seed_validation.go`,
guard `invalid_input`). Its commit swept "12 direct CreateTask sites" —
but #1930 had landed 09:35 with four more CreateTask fixtures inside the
NEW pin files, and the sweep missed them:

1. `pkg/board/phrase_read_integration_test.go:285` —
   `TestReads_ReplayEchoPhraseTranslation`'s create fixture (no
   Description) → the guard rejects at `CreateTask` (line 288 error:
   "task description is required at create") BEFORE any phrase
   assertion. **CI-RED on main**: Secrets Integration job at the
   #1933-merge push, run 40090453099, job 120861912867, exact test +
   line quoted from the CI log.
2. `api/internal/handlers/board_sse_phrase_integration_test.go:135,137`
   — `TestIntegrationBoardSSELiveProjectionActorPhrase`'s warm/cold
   fixtures (no Description, no criteria) → same guard death. Latent:
   the CI step rides the same job BEHIND the board kill-test step, so
   it was SKIPPED on the red run — dead pins CI had not yet reached.
3. Same file `:263` — `TestIntegrationBoardSSELiveProjectionPayloadPhrase`
   (table-driven create) → same.

Net effect on main: the raw-ID-leak guards this issue exists for were
not executing (1 CI-visible red + 2 latent), and main's Secrets
Integration job was red.

## The fix (minimal, test-only — 4 fixtures, zero assertion changes)

Each dead fixture gains a charter description + one acceptance criterion
(the #1849 create contract's own shape — the harness `seedTask` already
seeds exactly this):

- `pkg/board/phrase_read_integration_test.go` — the replay-create
  `TaskInput` gains `Description` (criteria were already present).
- `api/internal/handlers/board_sse_phrase_integration_test.go` — the
  warm/cold actor fixtures and the payload table's create gain
  `Description` + `AcceptanceCriteria: {{Text: "c1"}}`.

No production code touched. No assertion touched — the pins' phrase
expectations are byte-identical to #1930's.

## Evidence the revived pins still bite (mutation runs)

- Replay-echo seam (`service.go` `resolveEcho` tail): translation call
  disabled → `TestReads_ReplayEchoPhraseTranslation` RED on the phrase
  assertion (`expected: "amber-falcon-glides"`). Restored → green.
- SSE actor emission (`board_sse.go` `translateBoardEventIDs` call)
  disabled → `TestIntegrationBoardSSELiveProjectionActorPhrase` RED on
  the phrase assertion (`expected: "amber-heron-waits"`). Restored →
  green.

## Runs (branch head, local PG 15 + CI-mirrored migrations)

- `go test -race -tags=integration ./pkg/board/... -count=1` — ok
  (25.6s; one environmental scratch-DB flake on
  `TestMintUniqueOwnerWideLadder` cleared per #1930's disclosed
  fresh-DB-per-run note: fixed-uuid workspace bare-INSERT, deleted the
  row, green. CI runs a fresh DB — unaffected).
- `go test -race -tags=integration ./api/internal/handlers/ -run
  'TestIntegrationBoard' -count=1` — ok (2.6s).
- `go test -short ./pkg/board/ ./api/internal/handlers/` — ok.
- `go build ./...` clean; `gofmt -l` clean; `golangci-lint run
  pkg/board/... api/internal/handlers/...` — 0 issues.

## Reporting per the charter (evidence, not coding, for the fixed items)

- Issue #1922's named surfaces (Task.assignee_session across drawer/
  list/search/health/agent surfaces/write echoes; board_events.actor at
  TaskEvents + SSE emission) — FIXED by #1930; verified green on main
  HEAD with corrected fixtures (this PR's fixtures ARE the correction).
- #1904/#1931/#1933 post-fix landings — swept, no raw-ID emission added.
- Main CI Secrets Integration RED at 40090453099 — root-caused to the
  fixture collision; this PR is the repair.
