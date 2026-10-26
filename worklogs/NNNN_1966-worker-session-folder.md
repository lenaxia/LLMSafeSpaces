# NNNN — #1966 Workers folder in the session list (presentation-only grouping for board/engine-spawned + subagent sessions)

Worker lane (drafty-echo-reviews), dispatched off issue #1966 (filed by novel-nutmeg-verifies). Branch `feat/1966-worker-session-folder`.

## Live evidence (validate-first)
- `session_list` on this very workspace mid-census: 100+ worker sessions (board implementers, reviewer personas, triage lanes, backfill one-shots) interleaved with ~4 user-facing sessions — the exact flood the issue describes, reproduced before any code was written.
- Classification-signal audit: persistent `session_origins` rows (kind='session') were written ONLY by `recordRoutineSessionArtifacts` (routine trigger sessions); board workers spawn via the workflow-agent seam (`board:<project>` dispatch, `session: "new"`) with NO origin row and NO parentId — i.e. the frontend had NO signal to classify them. This is the one place the listing needed a class hint (charter: "add it minimally").
- The sidebar already merges `GET /workspaces/:id/session-origins` into each `SessionListItem` (`WorkspaceSessionList` queryFn) — the worker mark rides that existing pipe; zero API-shape change.

## Change
- `pkg/types/workflows.go`: `SessionOriginWorker = "worker"` joins the origin vocabulary.
- `api/internal/app/board_spawner.go`: `newBoardSpawner` gains a third param (`spawnOriginRecorder` — the #1915 two-verb seam, satisfied by the workflows store); after a successful spawn it records `origin='worker', kind='session'` (title = task short id), best-effort: a failed write logs (slog) and the spawn still succeeds; nil recorder skips. A failed spawn records nothing.
- `api/internal/app/app.go`: board wiring passes `workflows.NewStore(boardPool)` (same cfg.Database; own pool, mirroring the secrets-pool discipline).
- `frontend/src/lib/sessionWorker.ts`: `isWorkerSession` — `origin === 'worker' || !!parentId`. NOT workers: manual/routine/workflow-origin sessions (badged automation and DAG agent nodes keep their main-list treatment; documented in-file).
- `frontend/src/components/layout/Sidebar.tsx`: live set splits archived → (existing) Archived group; then `isWorkerSession` → new **WorkersGroup** (mirrors the #1627/#1735 Archived-group component: collapsed by default, count badge, Bot icon, `data-testid="workers-group"`, rows fully usable when expanded); the main tree builds from user sessions only. Selecting a worker auto-expands the folder (tree's auto-expand-the-active-chain precedent; deliberately NOT Archived's never-expand friction — a live worker can be the session being read, pinned). `worker` origin badge tint amber.
- Removed: the `OrphansGroup` component + its render (structurally dead post-split — orphans require a parentId, and every parented session now folders); the pre-#1966 pending-indicator bubble-up from subagent children (children no longer walk the tree — pinned honestly, see below).
- No #1964 territory touched (board Archived drawer is PR #1965's lane).

## Superseded UX (deliberate, per the issue's classification section)
- Subtask nesting under parents ("parented Task-tool sessions … same bucket"): children always folder. 11 stale pins updated to the new contract (hierarchy ×6 incl. orphan → Workers; auto-expand setting ×2; spinner/pulse ×2; force-stop ×1) — each rewrite pins the NEW behavior, not absence.
- e2e `sidebar-hierarchy.spec.ts` rewritten for the folder (4 specs, incl. origins-endpoint mock and no-folder absence).

## Pins (red-first)
- `frontend/src/lib/sessionWorker.test.ts` — classifier: worker-origin → folder; parentId → folder; manual/routine/workflow/user → main list.
- `frontend/src/components/layout/Sidebar.workers.test.tsx` — folder renders collapsed w/ count; worker-origin AND parented sessions folder; user/routine/manual stay main-list; expand on click; auto-expand on worker selection; archived worker → Archived group (archive outranks); query keeps `{ includeArchived: true }` (presentation-only; server-side `q` search composes over the unchanged listing).
- `api/internal/app/board_spawner_origin_test.go` — happy-path mark (origin/kind/session/ws/title stamps), mark-failure-still-spawns, nil-recorder skip, failed-spawn-records-nothing, retention-trigger (prune fires once post-spawn with the spawn's podIP + fresh id as keep).
- All were RED before implementation (missing module / missing group / signature mismatch), then green.

## Owner-ruling addition: worker-session hard cap (folded pre-approval)
Ruling (issue #1966 comment 6429460456): past a cap of 50 worker sessions, the OLDEST are hard-deleted oldest-first. Design decisions + justification:
- **On-create prune, not a sweep**: churn is create-driven (each board claim spawns); the prune runs exactly at the growth boundary, needs no scheduler surface/cadence config, and the spawn's own EnsureActive means the pod-side delete leg hits a warm pod. Limitation accepted: a lowered cap trims on the next create.
- **Two-level misclassification refusal**: (1) the candidate listing carries RAW signals (origin-worker / parented booleans) and the retainer re-applies the predicate — a user session in the list is refused (pinned: it is never deleted, the next true worker goes instead); (2) every candidate is re-verified by a fresh single-row `IsWorkerSession` read immediately before the delete — a classification that flipped/vanished since the listing refuses (pinned).
- **Grace floor**: rows updated <1h ago (mid-lease workers) never deleted; the prune never chases past the floor (pinned: over-cap set with only fresh workers → zero deletes).
- **Hard delete class**: `apiwf.DeleteSessionAuthorized` (exported from the engine — the caller-neutral #1882 core shared with the routine scheduler + DAG reconciler): pod-side delete + platform teardown (tombstone, index row removal, SSE) + origin-mark clean — evicted workers leave no ghost in the folder count or the cap count.
- **Config**: `LLMSAFESPACES_BOARD_WORKERSESSIONCAP` (default 50), `LLMSAFESPACES_BOARD_WORKERSESSIONGRACESECONDS` (default 3600); ≤0 → ruling defaults. Platform deploy knobs on the Board config section (helm values → env → config chain).
- Store seam: `pkg/workflows` `ListWorkerSessions` (session_index LEFT JOIN session_origins, deleted_at-filtered, oldest-first) + `IsWorkerSession` (single-row re-verification) — integration test in StoreIntegrationSuite (compile-gated without PG).
- Retainer pins (born RED): over-cap-create prunes oldest first (exactly to cap); misclassified user session refused; re-verification refusal; grace floor; never-the-just-created; at/under-cap noop; delete-failure continues; listing failure swallowed; defaults 50/1h.

## Gates
- frontend: `npm test` 2503/2503 · `tsc --noEmit` clean · `eslint` 0 errors (7 pre-existing warnings: generated protobufs + BoardPage) · `test:e2e` 247 passed / 13 skipped (pre-existing) / 1 first-run flake on the new count assertion (cold dev-server window; hardened with explicit 15s timeouts; 3× clean focused reruns; passes on suite retry). Re-run after the ruling addition: unit 2503/2503, typecheck clean, sidebar e2e focused green.
- backend: `go vet` + `go test` green on api/internal/app, api/internal/config, api/internal/workflows, pkg/types, pkg/workflows (+exprlang, scriptwrap) — re-run after the ruling addition.
- CI workflows 281913422 / 286282352 on the branch (see PR checks).

## Rounds
(logged as review lands)

## Blast radius
Frontend sidebar only for rendering; backend adds one best-effort DB write per board spawn (no API shape change, no session behavior change). LLM-facing surface unchanged.
