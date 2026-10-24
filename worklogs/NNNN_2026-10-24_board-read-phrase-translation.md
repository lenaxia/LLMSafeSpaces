# Worklog — #1922: board read-edge session-phrase translation (raw-ID leak, Part 2 of #1919)

Lane: fix/1922-board-read-translation (worktree wt-1922). Base: origin/main @ 1c46aae7.
Ruling: orchestrator seam ruling (issue #1922 comment 6379589693, rich-sunrise-pearls) — BINDING,
all four points honored. Fixes #1922.

## What leaked (reproduced on pristine main before any code — scratch demo, /tmp/opencode/repro1922/)

The board stored and served the CANONICAL harness session id (`ses_…`) on every task-bearing
public surface, even with the session_index row warm (phrase minted):

- `Task.assignee_session` — scanTask copies verbatim (pkg/board/store.go:79-80): GetTask,
  ListTasks, SearchTasks, BoardHealth/BoardHealthInWorkspace leases (reads.go:380,560),
  ListTasksInWorkspace/GetTaskInWorkspace (agent surfaces), stage-move/verdict echoes; rendered
  verbatim by the drawer (frontend/src/components/board/TaskDrawer.tsx:203-204) and the epic
  delete dialog; passed through VERBATIM to manager MCP consumers (pkg/mcp/client.go:1226-1321
  hits /api/v1/me/*).
- `board_events.actor.session_id` — TaskEvents serves the actor JSON verbatim (reads.go:159).
- ADJACENT (swept, in scope): the SSE `board.event` frame fans `evt.Actor` raw to the user +
  workspace streams (api/internal/handlers/board_sse.go) — same actor JSON; the frontend does
  not render it today, but the stream is user/agent-readable.

Positive control: the repro method CAN see a phrase where one exists (the platform session
listing read returned `amber-falcon-glides` for the seeded row); all eight board surfaces still
served raw. Structural cause: zero session_index consults in pkg/board.

## The fix (minimal, per the ruling)

Translation at the Service public-read edge ONLY — `pkg/board/phrase_read.go`:

- `translateTaskReads` — one project→workspace query per batch + one bounded
  `LookupSessionPhrases` consult per DISTINCT target workspace (`session_index`, short_id
  non-NULL; no cross-workspace fan-out). Warm → phrase; cold/consult-failure → raw stays +
  `assignee_session_deprecated` flag (new additive field on Task + HealthLease). Idempotence
  guard: `shortid.IsShortIDForm` skips already-phrase values (the #1817 piece-3
  double-translation discipline).
- `translateEventActors` — TaskEvents: the actor JSON's session_id → phrase, or raw +
  `session_id_deprecated:"true"` (STRING — `markDeprecatedSessionID`'s exact #1821 shape,
  api/internal/handlers/proxy_usagestream.go:548). Stored rows NEVER touched (pinned).
- Apply sites: GetTask, GetTaskInWorkspace, ListTasks (covers ListTasksInWorkspace),
  SearchTasks, TaskEvents, BoardHealth/BoardHealthInWorkspace leases, and the write-echo tails
  MoveStage / ReportThrottle / UpdateTask / PauseTask (incl. idempotent early return) /
  ResumeTask / RecordVerdict (both tails).
- SSE leg: `boardEventDeliver` translates `evt.Actor` after its existing workspace resolve
  (bounded single-session consult; failure degrades raw, never breaks the projection).

Internal seams untouched — canonical forever: scanTask, bounce aborter (stage.go:198),
reconciler, bounce reclaim, stillborn guard, halt/resume messenger (pause.go:177), spawn stamp
(engine.go:487), IsWorkerSession, mustTask. Plugin files untouched (#1920). No CRD/Helm/
API-surface changes (the Task/HealthLease/actor flag fields are additive omitempty).

## Exclusions (swept, documented — not leaks)

- `TaskStart`/`Claim` echo: assignee is NULL by construction at claim mint (engine.go:233/380);
  the production spawn is async (app.go:1603 WithAsyncSpawn) and stamps `claim.Assignee` on its
  own goroutine AFTER the response returns — reading/translating it at TaskStart's return would
  DATA-RACE the spawn goroutine. Left canonical; the echo never carries a live assignee in
  production wiring.
- `CreateTask`/`RearmTask` echoes: assignee is always NULL on their rows (create is backlog;
  rearm NULLs the column).
- `board_transitions.actor`: write-only today — no read path serves it.
- Verdict payloads: `reviewer` is caller-supplied identity text, not a session id.

## Red pins (born-RED on pristine main @ 1c46aae7, commit e4106511)

Assertion failures, not build failures — the pin files compile against main (deprecation-flag
assertions probe the marshaled JSON, no new symbols):

- `pkg/board/phrase_read_integration_test.go` (runs in the secrets-integration workflow's
  "kill-tests for pkg/board" step): `TestReads_AssigneePhraseTranslation` 9 failures (every
  surface served raw), `TestReads_AssigneePhraseColdDegrade` 3 failures (no flag),
  `TestReads_ActorSessionPhrase` (raw actor, no flag) — also pins storage immutability
  (stored rows keep canonical ids after translated reads) and cold→warm convergence.
- `api/internal/handlers/board_sse_phrase_integration_test.go` — rides the existing
  `TestIntegrationBoardSSELiveProjection` -run filter of the SSE live-path CI step (function
  name prefix match; file path matches the workflow's `^api/internal/handlers/board` filter):
  warm actor raw instead of phrase; cold actor missing `session_id_deprecated`.

### Mutation recipe (per-pin verify that each fails when the fix is reverted)

`git revert --no-commit <fix-commit>` (or checkout the red commit's source files) restores the
pristine translation-free service/handler code while keeping the pins → all four pin groups
fail exactly as in the born-red run above. Verified at the red commit before writing the fix;
re-verified post-fix via source revert (see PR body for the run).

## Gates (at the fix head)

- `go test -tags=integration ./pkg/board/...` — ok (full suite, 34s).
- handlers integration board seams (`TestIntegrationBoardWorkerSemantics|BoardConfig|BoardManagerSemantics|BoardSSELiveProjection|UsageStream|SessionRegistry`) + server `TestIntegrationRouterBoardConfig` — ok.
- `go test -short ./pkg/board/... ./api/internal/handlers/` — ok.
- `go build ./...` — clean. Pre-commit (repolint, gofmt, goimports, golangci-lint) — clean.
- Known flake classes cited, not fixed: TestUploadApply_ConcurrentWallTime pool race; frontend
  live-update-1687.spec.ts e2e (CI-covered; not re-run locally per CI-first policy).

## Frontend

Zero behavior change (the phrase renders where the raw id rendered). Contract mirrors only:
additive optional `assignee_session_deprecated` on BoardTask and `session_id_deprecated` on the
audit-event actor (frontend/src/api/board.ts).
