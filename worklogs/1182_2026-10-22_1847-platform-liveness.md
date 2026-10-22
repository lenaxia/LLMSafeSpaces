# Worklog: #1847 PR2 — platform-derived claim liveness (spec items 1-4)

**Date:** 2026-10-22
**Session:** lane-1847 (platform-derived lease liveness), PR2 of the slicing.
**Status:** Complete

---

## Objective

Issue #1847 spec items 1-4: retire worker-declared timestamps as the
PRIMARY liveness input. Platform-derived liveness: an open turn is
alive (holds leases across long silent tool calls), started+recent
activity is alive within a grace, stillborn spawns are never renewed,
and a wedged turn releases within grace + ONE pre-reclaim appeal ping
delivered at the turn boundary.

## Work Completed

### Storage (migration 000044_board_liveness, api + helm mirrors)

- `board_session_liveness` (PK workspace+session): busy, started,
  last_activity_at, pushed_at — the agentd push projection.
- `tasks.appeal_until` (the open appeal window) and
  `tasks.lapse_reason` (why the claim lapsed — the item-6 input).

### Classification (pkg/board/liveness.go, unit-pinned)

`ClassifyLiveness(now, sample, grace, staleBound)` — pure, table-tested:
- no row / never started → **none** (stillborn: never renewed).
- busy with fresh pushes → **alive** (open turn holds, any activity age).
- started + activity inside grace → **alive**.
- else → **dead** (the appeal class).
- Every stamp clamps to its own PUSH ANCHOR (`effectiveActivity`):
  information cannot be newer than when it was pushed — a garbage/
  future stamp is exactly as fresh as the row's honest push time,
  never more (acceptance: "no such input path").

### RC1 rework (pkg/board/reconciler.go)

Victims (lapsed in_progress) LEFT JOIN the liveness projection:
- alive → guarded lease re-arm + watermark adopts the (clamped)
  observed activity + appeal cleared; attempts untouched;
  `liveness_renew` event (deterministic id per lapse episode).
- dead + started, no appeal open → ONE appeal: window stamped on the
  row (lease extended to it), reason classified
  (silent_beyond_grace vs stale_liveness), `lease_appeal` event,
  post-commit ping via the messenger seam (DirectiveLeaseAppeal —
  halt/resume machinery, turn boundary, never mid-generation).
- open appeal (window unelapsed) → skipped: no second ping, no reclaim.
- appeal lapsed / none-class → the #1813 reclaim with lapse_reason
  recorded (no_assignee / session_unknown / appeal_ignored / ...).

### Observability push (agentd → API)

- agentd: `boardLivenessLoop` (30s, the pressure-loop pattern) pushes
  `sessionStatusTracker.livenessSnapshot()` — busy (statuses), started
  (messages>0 OR busy — busyPartitions' evidence rule), activity =
  max(lastEventAt, lastMessageAt, busySince fallback). Unit-pinned
  (`board_liveness_test.go`) incl. the empty-pod case (pushes nothing;
  stale rows age out server-side).
- API: `POST /internal/v1/automation/board/session-liveness` (pod
  SA-token seam), `Service.RecordSessionLiveness` upserts with
  server-stamped pushed_at + write-side future clamp.
- Wire: `WorkerSeamWireV2` (agentd v0.41.0) appended to the #1746 skew
  matrix — the four frozen tool routes unchanged + the new push; both
  compat pins (client emits-exactly-newest, API accepts-every-set)
  updated; the route joins the agent-seam parity table.

### Heartbeat demotion (spec item 2)

Tool description rewritten (wire form unchanged): OPTIONAL manual
override for deliberate quiet; liveness is platform-derived;
server-clock authority named. The guidance pin gains "PLATFORM-DERIVED"
and swaps "new progress evidence" → "progress watermark".

### Wiring

- EngineConfig: LeaseGrace / LeaseAppealWindow / LivenessStaleBound
  (+ defaults; grace 0 → 2x lease). Chart knobs (values.yaml) + env
  overrides + config defaults.
- app.go: knobs feed the engine; the messenger renders the appeal
  text with the configured window (leaseAppealDirectiveText).
- Task read model carries appeal_until / lapse_reason (REST + MCP
  surfaces see the appeal state).

## Red-first arc

- Red commit b1d63bc0: 4/5 classification unit pins + all four RC1
  flow pins failing (real output quoted in the commit message:
  reclaims=1 / stage backlog against "an open turn is alive", NULL
  lapse_reason, no appeal machinery).
- Implementation turned all red pins green; the full pkg/board
  integration suite green on a fresh PG.

## Verification

- `go test -tags integration ./pkg/board/` — green on fresh PG (the
  only DB-reuse failure is the ROUTED pre-existing flake
  TestMintUniqueOwnerWideLadder, reproduced on main).
- `go test ./api/internal/handlers/ ./pkg/agent/opencode/
  ./cmd/workspace-agentd/ ./pkg/board/` — green.
- `make repolint` — green.

## Acceptance mapping (issue's sketch)

- 20-min silent bash holds its lease → TestKill_RC1Liveness_
  OpenTurnHoldsExpiredLease + TestClassifyLiveness_OpenTurnHolds.
- Garbage/future timestamp cannot affect a lease → future stamps
  clamp to the push anchor at BOTH the write and the classifier
  (TestClassifyLiveness_FutureStampClampsToPushAnchor: identical
  verdict to the honest row).
- Wedged turn releases within grace + one appeal ping →
  TestKill_RC1Liveness_WedgedTurnAppealsThenReclaims (one ping, no
  second inside the window, reclaim after).
- Stillborn never renewed → TestClassifyLiveness_StillbornNeverRenewed
  + TestKill_RC1Liveness_StillbornReclaimed.
- Lapsed-then-alive claimant, attempts unchanged →
  TestKill_RC1Liveness_AppealWindowRecoveryReArms (same token, same
  attempts, appeal cleared).

## Deliberate scope lines

- board_reclaim (spec item 5's tool) + claim history + escalation
  reasons (item 6) + the decommission note (item 7) are PR3 — the
  appeal text intentionally says "make any board write or keep
  working", not the tool name yet.
- The recovery arm re-arms the ORIGINAL claim (token never left the
  worker); PR3's board_reclaim covers the post-repool recovery.
