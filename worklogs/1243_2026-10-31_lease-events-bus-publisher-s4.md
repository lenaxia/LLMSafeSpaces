# Worklog 1243 — Liveness events publish onto the bus: nudge, renewal, wedge, reclaim (design 0072 S4)

- **Story**: tsk-redolent-hawthorn-leaves ("S4 — Liveness events publish onto the bus … first API-side producer", epic "Liveness and observation", the 2026-10-30 incident remediation S0–S7)
- **Branch/worktree**: `lease-events-publisher` at the isolated worktree `/tmp/opencode/lease-events-wt` (forked at `9c17803b9`, the sprint-board-story-machinery integration line). The shared checkout moved mid-flight to the S2 lane's uncommitted edits (`pkg/board/busy.go` referencing a `busySrc` field not yet landed — build-breaking for every other lane); per the repo's lane convention (S3's precedent, COORDINATE.md) this lane extracted to its own worktree and restored the shared tree to exactly the S2 lane's files (`rows.go`, `reads.go` modified, `busy.go` untracked). One early service.go copy carried the S2 lane's in-flight `busySrc` hunk; it was removed from THIS lane's tree before the first commit (their work is untouched in the shared checkout).
- **Date**: 2026-10-31

## 1. The finding this closes

The nudge ladder was WIRED in prod (`api/internal/app/app.go` WithLivenessSource + WithNudgeSender) but its actions were invisible: nudges sent, renewals, lapse verdicts, and reclaims left no event anywhere — and agentd's lease consumer group already waited on `lease.*` topics that NOTHING published (design 0072 built the consumer "ready and pinned" against a producer that did not exist). The API had zero eventbus usage. During the incident, reconstructing what the ladder had decided forced transcript forensics.

Depends on S3 (bus health, tsk-misty-keystone-rambles — DONE): publishing onto a bus whose health is invisible just moves the blind spot. S3's branch is unmerged at the time of writing; this lane therefore composes with it at the STREAM (producer-agnostic by construction — S3's stream-level gauges and the lease group's lag/deliveries move for ANY publisher), and carries its own API-side expvar counters so the producer's outcomes are visible without S3's code.

## 2. What was built

1. **pkg/eventbus — the vocabulary formalized**: `LeaseRenewedTopic` / `LeaseReclaimedTopic` join the launch set, and the lease family's payload contract becomes constructors (`LeaseNudgePayload`, `LeaseRenewPayload`, `LeaseWedgePayload`, `LeaseReclaimPayload`) shared by the API publisher and the agentd consumer/tests — the producer↔consumer agreement is code, not copy-paste (the sentinel body still cannot cross the api/agentd split — pkg/board drags pgx — and stays byte-parity-pinned by test, the existing convention).
2. **pkg/board — the engine's observation-plane seam** (`lease_events.go`, NEW): `LeaseEvent` (kind, task ids, assignee, nudge id, source/evidence/reason, failure count), `LeaseEventSink`, `board.WithLeaseEventSink`. Emissions, all POST-COMMIT and fail-open:
   - `rc1Reclaims`: renewals (with the rung that answered alive: `heartbeat_response` / `movement` / `post_nudge_reaction` / `busy_gate`), wedge verdicts (evidence summary + the sentinel measured), reclaims (reason: `lease_lapse` / `dead_evidence` / `nudge_delivery_failed` / `lapse_no_assignee`). The NUDGE rung is deliberately engine-silent — the app nudger is the nudged publisher (double-reporting would make the stream's nudged count a lie).
   - `rc1PersonaClaims`: the in_review persona lapse pass emits reclaims (`persona_lapse`, `persona_config_cleared`); its SELECT grew `short_id`/`assignee_session` so the payload matches the in_progress shape.
   - `Heartbeat` (stage.go): the response arm emits `renewed`/`heartbeat_response` at its own committed write (the sweep never sees a heartbeat-renewed row — the lease is live again).
3. **api/internal/app — the API's FIRST bus producer** (`board_lease_events.go`, NEW): `boardLeaseEventbus` — one shared redis client, one `RedisBus` per workspace stream (lazily wired, evicted on failure), a 2s publish budget, and a breaker that pauses the publisher one sweep window after a failure (a dead Redis costs ONE budget, then fail-fast — never a per-sentinel tax). `newBoardNudger` gained the bus leg AFTER `markNudge` (the mark-before-sentinel contract holds on both paths): publish `nudged` → nil (the group is the delivery leg); any failure → the v1 direct send VERBATIM (the phantom-nudge contract). `LeaseEventSink` maps kinds → topics with the shared builders. Knob: `board.leaseEventbus.{addr,password}` / `LLMSAFESPACES_BOARD_LEASEEVENTBUS_ADDR`/`_PASSWORD`; empty addr (default) = nil publisher = pre-S4 behavior byte-identical. API-side visibility: `/debug/vars` `board_lease_events_{published,failed}_total`.
4. **cmd/workspace-agentd — the consumer closes the loop**: the lease group's `renewed` (Info) and `reclaimed` (WARN — the incident's invisible transition, now a loud line on the pod the claim lived in) arms join `nudged` (delivery) and `wedge_verdict` (log); lifecycle arms message NOBODY. The file-top topology comment now records the producer as LANDED.

## 3. The arming posture (the one operator-facing rule)

The API knob and the controller's `--workspace-eventbus-addr` are ONE posture: arm both or neither, against the SAME Redis. A published nudge whose workspace runs an unarmed agentd has no consumer — the lost-wake-up shape design 0072 names as the failure that matters, now visible on S3's gauges (publishes rising; the lease group absent or lagging). Documented in the config comments, the design amendment, and the publisher's file header.

## 4. Tests (all green, serially — see §6)

- `pkg/eventbus`: the two topics parse; the four payload builders' exact maps + reserved-field safety.
- `pkg/board` (integration, real PG): `lease_events_test.go` — renewal carries its rung (movement, busy-gate); the wedge carries evidence + nudge id + a stage snapshot AT EMISSION proving post-commit (the sink sees the row already failed); every reclaim path names its reason (dead-evidence, nudge-delivery-failed, no-assignee); the heartbeat response projects at its own write; nil sink = the ladder byte-identical (spot pin).
- `api/internal/app` (miniredis): bus-first nudging with the shared-builder payload and NO v1 send; the mark recorded on the bus leg; mid-flight bus death falls back to v1 verbatim; the breaker's fail-fast window + re-arm against a fresh transport; every sink kind's topic/payload on `workspace:{id}:events`; the OFF posture's nil-receiver safety; the two address spellings (URL credentials win over the password env; malformed addr refuses without echoing it).
- `cmd/workspace-agentd`: the lifecycle arms log-not-message (hooked-logger pins, a panicking seam proves no delivery); `TestBusE2E_LeaseLifecycleEventsAckQuietly` — renewed+reclaimed published with the SHARED builders ack through the real consumer group with zero session POSTs; the existing `TestBusE2E_LeaseConsumerDeliversNudge` unchanged (it already published "exactly as the API's nudge ladder will" — now it does).
- The full existing lease-pin battery (eleven born-precise pins) runs unchanged — the ladder's decisions, clocks, and guards are untouched; the projection is additive.

## 5. What was deliberately NOT done

- **No helm/controller wiring for the API env** — the knob is config+env; the helm values bump belongs with the arming change (the S3 lane's talos-ops-prod values bump is likewise a follow-up; the two land together when the operator arms the posture).
- **No `SetPublishObserver` use** — that seam lives on S3's unmerged branch; this lane's expvar counters carry the API-side visibility today, and S3's prometheus surface picks the publisher up at the stream level the moment both merge.
- **No renew/wedge/reclaim SessionSend deliveries** — lifecycle events are observation-plane; agentd logs them. Messaging a wedged worker is noise (design 0072 §3); a renewed worker is mid-work; a reclaimed row's next claimant is the engine's business.

## 6. Verification record (this lane's runs)

- `go build ./...` clean; `go vet` over all five touched packages clean; `gofmt -l` clean; `make fmt-check` green (run before every push, per the charter).
- Suites, run SERIALLY (shared PG between concurrently-active lanes flaked the board suite when two suites hit it at once — diagnosed via `pg_stat_activity` during a passing window: foreign test backends present exactly when the bursts fired; a pristine-HEAD worktree passed 3/3 alone, and this branch passed alone likewise):
  - `pkg/eventbus` ok; `pkg/board -tags integration` ok (47–52s); `api/...` ok (app+config+handlers); `cmd/workspace-agentd` ok (372s, full battery incl. the bus e2e family).

## 7. Coordination

- S2 lane (busy TTL/badge): active in the shared checkout on `pkg/board/{busy.go,reads.go,rows.go}` and (briefly, mid-edit) `service.go`. Zero file overlap with this lane's committed set; the service.go adjacency was resolved by this lane's worktree extraction. Merge note for the orchestrator: this lane adds a field to `Service` (`leaseEvents`) directly below `nudger` — S2's `busySrc` hunk sits in the same struct; trivial union.
- S5 (DEPOSED, committed `9c17803b9` before this lane forked) and S1 (stamp hardening) are IN this branch's base — their suites pass with the new seam installed.

## 8. Rework round (PR #2007 r2, the orchestrator's findings)

1. **Main moved under the lane** — #2005 (the story-machinery rename) merged INTO this branch per the orchestrator's direction. Every conflict was S1/S5-vs-rename (this lane's own S4 files merged clean): the schemas union S1's `sessionId` self-identification with main's `storyId` spellings; the allowlists take main's story forms.
2. **The S5 depose tool ported to canonical story form everywhere it rides this branch** (it shipped task-named on the integration branch and never met the rename): `board_depose_story` + `story_id`/`storyId` keys on both catalogs (registration, dispatch, allowlists, the origin plugin's INJECTED_SESSION_TOOLS, the parity grid), `BoardDeposeStory` on pkg/mcp's integrator surface (the agentd loopback and REST wire keep their task spelling — the rename's own wire decision); `board_depose_task` joins BOTH one-release alias windows (agentd's call-time map + pkg/mcp's registered DEPRECATED-ALIAS tools), because the task name is what v0.44.x prod served.
3. **TestDocs_MCPTools_Parity (the canary finding)**: docs/api/mcp.md gains the canonical `board_depose_story` row (required args + guards) and the `board_depose_task` deprecated-alias row — the parity test reads table rows as the documented contract, and the alias is a REGISTERED tool on the external surface, so both rows are demanded.
4. **Lint contextcheck (board_lease_events.go)**: `busFor` now takes the caller's ctx and derives the wire timeout from it (no fresh Background window inside a request-bounded call path).
5. **Worklog renumber**: main's rename lane had already taken 1241 — this lane's worklog is 1242 (repolint's duplicate check caught the collision; the gap list is the tolerated historical class).
6. Gates rerun serially on the merged tree, all green: make lint (golangci-lint, contextcheck included) 0 issues; make repolint + pkg/repolint (both parity canaries); make fmt-check; pkg/eventbus, pkg/mcp, pkg/board -tags integration (40s), api/internal/{app,config,handlers} (handlers 84s full), cmd/workspace-agentd full (376s).

## 9. Rework round 3 (PR #2007 r3 — the S6 unification landed under the lane)

#2008 (S6: board events + stamps ride the bus, SSE joins the stream) merged into
sprint-board-story-machinery at 16:25Z carrying THIS LANE'S r1 work (the S6 lane
had forked off it), S3's health surface, and S6's own legs. The merge bounce was
the overlap; the reconciliation:

- **The transport belongs to S6's shared shape**: `board_bus.go`'s `boardBusClient`
  (one transport, many legs — lease/board-events/stamps, per-leg expvar counters
  injected at publish) supersedes this lane's private client/breaker/buses
  machinery. Resolution: TAKE THEIRS for `board_lease_events.go`(+test) and
  `pkg/eventbus/eventbus.go` — the r2 contextcheck fix is architecturally
  subsumed (their `busFor(ctx, …)` takes the caller's ctx), and every r2 test
  (breaker fail-fast, address spellings) rides their file.
- **The unions**: service.go gains S2's `busySrc` + S6's `stampEvents` beside
  this lane's `leaseEvents`; app.go gains S2's `WithBusySource` + S6's
  boardBusPublisher/SSE-join wiring beside this lane's `boardLeaseBus`; config
  gains S6's `board.eventbus.*` with field-by-field fallback to this lane's
  `board.leaseEventbus.*` (one Redis, one posture); design 0072's §2 table
  gains S6's landed rows; CHANGELOG/COORDINATE additive.
- **This lane's r2 (depose story-form, docs canary) survives untouched** — S6's
  merge never touched those files; the docs/api/mcp.md hunk resolves to the
  story-form rows (main's rename + the depose row).
- **Worklog number**: the S6 lane's review round had renumbered this worklog
  1241→1243 on their branch; this lane's 1242 (the r2 rename) consolidated INTO
  their 1243 — one file, the r1+r2+r3 record.
- Gates rerun on the merged tree (serial): build, golangci-lint, fmt-check,
  repolint, pkg/eventbus, pkg/board -tags integration, api/internal/{app,config,
  handlers}, cmd/workspace-agentd full.
