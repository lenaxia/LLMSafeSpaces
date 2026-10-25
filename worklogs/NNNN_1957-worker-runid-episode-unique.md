# NNNN — #1957 BUG A: episode-unique workflow RunIDs for worker claims (stillborn charter delivery)

Orchestrator-executed fix (novel-nutmeg-verifies), reviewed through the standard bot channel.

## Live evidence chain (2026-10-25)
- 16:47:36 / 17:06:38 / 17:29:52 — bounce-minted and cooldown-re-armed worker sessions (hazel-brochure-squeals, golden-sailboat-generates, level-mosaic-shines) spawn with ZERO tokens on BOTH the 10-21 agentd and the 0.44.0 agentd (e86bd7c4) — version skew ruled out.
- 17:50:49 — discriminator probe (tsk-chilled-musket-slogs): a FRESH task (fresh UUID ⇒ fresh RunID) claimed, spawned, turned, wrote its file in 68s on the same pod/engine.
- Root cause: agentd's workflow-agent mux keys charter delivery on (workflow, run); the spawner's RunID for plain worker claims was the bare task id (board_spawner.go:126 pre-fix) — every re-claim re-used the key and the charter was silently dropped.
- Reviewer confirmation (r2/r4 verdicts): all seven spawn-bearing paths mint fresh claim tokens; the agentd dedupe hashes the full raw identity (workflow_execute.go:704-717); nothing persists the dispatch RunID — uniqueness by construction is safe.

## Change
- `boardRunID(c)`: task + stage tag ('w' for worker arms) + claim token (fresh per mint; tokenless fallback UnixNano).
- `boardSpawnRequest(c)`: builds the dispatch; RunID derived internally.
- Seams `spawnActivator`/`spawnDispatcher` interfaces (concretes + callers unchanged) so the dispatch closure is pinnable.

## Pins
- TestBoardRunID_EpisodeUnique (same-task claims never share a RunID; shapes).
- TestBoardSpawnRequest_WiresTheRunID (builder carries RunID/prompt/fresh-session).
- TestBoardSpawner_DispatchCarriesEpisodeRunID — the real closure over fakes; the reviewer's bypass mutation (re-inline bare task id in the closure) verified RED locally and independently by the reviewer; second-episode divergence pinned.
- TestBoardRunID_TokenlessFallbackShape.

## Rounds
- r1: SA4000 dead branch removed. r2: builder extraction (rejected — pin didn't force the closure). r3: seam-sealing claim (rejected — bypass mutation still green; claim false). r4: interfaces + closure pin (accepted; blocking finding resolved). r5: comments trued, tokenless pin, worklog.

## Blast radius
api/internal/app only; engine/agentd untouched; BUG B (reviewer toolset) was the stale agentd digest — fixed in talos-ops-prod#2854.
