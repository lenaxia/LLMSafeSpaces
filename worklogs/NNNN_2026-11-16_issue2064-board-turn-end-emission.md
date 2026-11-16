# Board turn-end emission — both harnesses, the attested fold only (issue #2064, epic #2061)

2026-11-16 · branch `feat/2064-turn-end-emission` · task tsk-electric-dinner-recants

## §1 The story and its rulings

Epic #2061 story 1: agentd emits a provenance-marked turn-end event
(`EVENT_TYPE_BOARD_TURN_END`, payload `BoardTurnEnd{advanced, epoch,
turn_duration_ms}`) for board claim-holder sessions, on BOTH in-tree
harnesses. The 2026-11-15 binding audit rulings narrow everything:

- **Emission-site restriction** (the anti-#2040 guard): only the
  harness's own turn-end observation path emits — the opencode leg's
  `applyHarnessEventLocked` (the Ingest/IngestForTest fold) and the pi
  leg's pump settle fold (`agent_settled`). Synthetic settles
  (reconcile store evidence, lease rederive), corroboration polls, and
  the reseed flush NEVER emit. Born-red pinned on both legs
  (`TestBoardTurnEnd_SyntheticSettleNeverEmits`,
  `TestPiBoardTurnEnd_EmissionSiteIsTheAttestedFold`).
- **Advance flag**: the narrow set only — `board_record_progress` |
  stage move | verdict. Presence reads (board_read/list/health) and
  heartbeats are deliberately NOT advance (the keystone lesson); no
  productivity detection exists beyond the set (pinned by
  `TestBoardAdvanceTools_SetPin`).
- **Claim-holders only**: the spawner-minted registry
  (boardWorkerSessions / the shared workers file), never user chats.
- **Zero liveness authority (#2042)**: the event stays opaque-class in
  the api's movement allowlist — pinned by name in
  `TestLeaseLivenessHub_ManagementEventsAreNotWorkerMovement`; a
  mutation naming it in `workerActivityStampsEvidence` reddens.

## §2 What landed

- Schema: enum 11 + message + field 14, regenerated (Go/TS), buf lint
  + breaking green against the frozen ref; sessiongen's Uint64Kind →
  uint64 fix; surface completeness pins amended.
- Opencode leg: `Config.BoardTurnEndProbe` seam consulted under the
  authority lock at the attested fold; epoch/duration/publish in the
  authority (`board_turn_end.go`, 12-test suite incl. degraded nil
  probe, panic containment, error turns, wire translation).
- Shared machinery (`board_turn_ledger.go`): the advance ledger —
  stamped at callMCPTool's head, keyed on `lsp_injected_session` ONLY
  (a model-declared sessionId copy never stamps; the #2040 poisoning
  guard stays closed, pinned) — memory for the serving process plus an
  append-only JSONL tail on the shared volume for the pi leg's
  cross-process consult. Lazy singletons so the test binary redirects
  the tail path in TestMain before first use.
- Pi leg (`pi_board_turnend.go`): the pump's ApplyEvent busy→settled
  transition hook journals the event to
  `/workspace/.opencode-board-turnends` (env-overridable,
  compaction-bounded); polls and agent_end never emit.
- Origin plugin: `board_move_stage` + `board_record_verdict` join
  `INJECTED_SESSION_TOOLS` (the stamp's attribution key), lockstep
  pinned both directions + a named baseline pin.
- Ops: `llmsafespaces_board_turn_ends_published_total` +
  `llmsafespaces_board_turn_end_probe_failures_total`, delta-bridged
  from the authority's cumulatives (the answerBudget convention).
- E2e: `TestBoardTurnEndWiring_RealChainEmits` drives the REAL
  production chain (registry → choke-point stamp → probe → authority
  publication) — advanced=true in-window, false stale-window, user
  sessions never emit.

## §3 The bug the test caught

`TestPiBoardTurnEnd_CompactionKeepsTail` failed on first run and
exposed a real production bug: `compactJournalTail` closed the journal
file BEFORE scanning it — the scanner reads an empty set, the rewrite
never triggers, and the journal grows unbounded past its 10k bound with
no error and no warn (silent). Fixed (close after the scan); the test
now pins it by name.

## §4 Process notes

- The session suffered recurring terminal display corruption
  (truncated/duplicated/phantom tool outputs, including fabricated
  CHANGELOG reads and a phantom worklog directory). Recovery protocol:
  every verification written to a file and read back; every edit
  confirmed by independent grep before being trusted. All final states
  in this worklog were re-verified that way.
- Full suites green at head: `cmd/workspace-agentd` (95s),
  `sessionstate` (27s), `pkg/abi`, `pkg/session`, and the api
  liveness pins; -race on every new test file.
