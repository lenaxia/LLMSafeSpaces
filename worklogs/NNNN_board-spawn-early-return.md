# Board-worker spawns return identity immediately; the charter delivery is detached

2026-10-30 · branch `board-spawn-early-return` · the 2026-10-29 post-0.44.8 live-fire

## §1 The incident

Two board workers claimed tasks on fresh 0.44.8 claims (22:19, 22:43 UTC)
and ran as ANONYMOUS ORPHANS: their rows never carried `assignee_session`
(40+ minutes with live claims — verified by board_read), the task cards
rendered no link and no busy indicator, the sidebar's Workers folder did
not classify them, and the lease ladder's liveness observation was blind
to their rows (refs are built from the assignee). The owner hit all
three surfaces from the UI within minutes.

## §2 Root cause

The spawn chain gated BOTH identity stamps on the worker's ENTIRE first
turn:

1. `spawnSync` (pkg/board/engine.go) calls the spawner; only a
   successful return stamps `assignee_session` (the guarded UPDATE) —
   and the API-side origin mark rides the same success path
   (board_spawner.go, post-Execute).
2. The spawner's `HTTPAgentExecutor` POSTs the agentd node-execute
   endpoint with a **15-minute client timeout**.
3. agentd's agent-node handler blocks on opencode's
   `/session/{id}/message` — which answers at **TURN COMPLETION**
   (writeWorkflowSuccessSession at node end).

A board worker's first turn (read the charter, set up a worktree, start
building) routinely exceeds 15 minutes → the client times out →
`spawnSync` logs "worker spawn failed — lease lapse heals" and returns →
both stamps are skipped → agentd keeps the session alive server-side,
the worker works its charter happily, unclaimed-by-nobody, invisible.

This was NEVER deterministic: first turns under 15 minutes stamped fine
(all of this morning's workers did), which is why every suite stayed
green — the engine and read-edge layers are provably correct in-repo
(verified: full pkg/board integration suite green on clean 0.44.8,
including the #1922/#1930 phrase-translation pins and the claim stamps).

## §3 The fix

agentd's node-execute handler gains an early-return arm for the
board-worker NodeID ONLY: after the session create (and the
boardWorkerSessions provenance record), the charter message is
dispatched in a DETACHED goroutine (`dispatchAgentMessageDetached`:
WithoutCancel-of-request — context.Background with a 2h bound that
outlives any real first turn; the same #1327 dedupe key as the blocking
path so a re-drive upserts the same transcript message; failures log
and the lease-lapse contract heals — identical failure semantics to
today) and the endpoint answers IMMEDIATELY with the session id.

Real DAG agent nodes keep the blocking contract untouched — their
results feed the graph.

## §4 Regression pins (the owner directive: every bug gets tests)

`cmd/workspace-agentd/workflow_board_spawn_early_return_test.go`, over
a fake opencode whose message leg PARKS until released (the
"first turn still running" shape):

- `TestBoardWorker_SpawnReturnsIdentityBeforeTurnCompletes` — THE pin:
  200 + sessionId + dispatched within 2s while the turn is parked;
  the dispatch fires and drains after release. Mutation-verified RED:
  with the early arm disabled the test deadlocks on the parked leg
  (the incident's exact manifestation — observed as a hang during the
  mutation run before restore).
- `TestAgentNode_NonBoardWorkerStillBlocks` — the counter-pin: a
  regular agent node does NOT answer before turn completion.
- `TestBoardWorker_DeliveryFailureKeepsIdentity` — a failing charter
  delivery never rewrites the spawn's outcome; the leg drains without
  panic.

## §5 Verification

`go build ./...`; the new pins under `-race`; the FULL agentd suite
(377s, green); golangci-lint 0. pkg/board untouched (its integration
suite re-run green on clean main during triage — the engine was never
at fault).

## §6 Notes

- The prod rows already orphaned (tsk-arctic-wardrobe-verifies,
  tsk-pallid-pivot-wiggles at claim time) stay orphaned by design —
  no backfill; their leases lapse and the fixed path re-claims with
  stamps. Their workers keep working; their PRs ride the review gate
  regardless.
- Side finding during triage: the main working tree carried 22
  untracked leftovers from the old unlanded subscription lane
  (including migration files with conflicting 000052/000053 numbering
  that broke the local build). Removed; the lane's real content landed
  long ago via PRs.
