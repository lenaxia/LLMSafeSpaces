# Worklog — SSE event-delivery guarantees (design 0069 + the P0 row): replay ring, Events.since resume, billing closure

Lane: sse-guarantees (P0 reviewer's lane landed in-tree; P1a landed
in-tree unreported; P1b built by worker session copper-rodeo-recants,
orchestrator merry-finch-loses). Source of record: the per-leg
delivery-guarantee audit at
`/workspace/reports/sse-delivery-guarantees-investigation.md` (§L2/L3 +
verdict table), rendered into repo conventions as
`design/0069_2026-10-26_authority-replay-ring-events-since.md`. PR #1970.

## 1. What was built

Three pieces against the audit's verdict table:

- **P0 (L4a)** — the user-stream replay's client-ahead gap: `lastID ≥
  nextID` (a cursor from an id space this replica never wrote — the
  ring is in-process, so an API roll restarts it at id 1) was a SILENT
  empty replay. Second gap arm in `sinceLocked` + (review r1) the
  no-ring arms (`Replay`'s nil-return, `sinceLocked`'s empty-count
  return) — every presented cursor against state this replica cannot
  honor now emits `resync`.
- **P1a (L2, design 0069)** — the authority replay ring: every seq'd
  published frame retained, evict-oldest under a 1024-frame cap AND a
  16 MiB byte budget (newest always retained), appended at the
  `fanoutLocked` seam; `EventsRequest.since` (optional field — the op
  set stays five) resumes a served cursor gaplessly (replay-tail copy +
  live registration under ONE lock hold), degrades honestly below
  low-water or (review r1 guard) beyond the current seq.
- **P1b (L3, the consumer half)** — `abiclient` persists its fold on
  the `Client` and presents `since = fold.Seq` on every reconnect;
  first-frame disambiguation makes pre-0069 agentds and below-low-water
  cursors fall back to snapshot-first with zero version negotiation;
  reseed-in-replay redials snapshot-first without looping; the
  usagestream gate reuses one client per endpoint so run-loop retries
  resume too; `WithResumed` suppresses the #1884 seed machinery on
  resumed connections (nothing lost → nothing reconciles).

## 2. Assumptions, and how each was validated

- *"Redelivery is safe for billing by construction"* — the
  deterministic `(workspace, message, seq)` idempotency keys collide on
  redelivery. Validated: `TestRecordStepUsage_DeterministicKeys` (pre-
  existing) + `TestBillingDuplicateRedelivery_IdempotencyKeyAbsorbs`
  (byte-identical `Usage` on duplicate delivery, one row per key).
- *"The ring's contiguity makes any `lowWater ≤ since ≤ seq` cursor
  exact"* — one seq per published frame, appended under the authority
  lock. Validated: `TestReplayResumeGaplessProperty` (60 randomized
  cycles), `TestReplayResumeUnderConcurrentFloodRace` (`-race`),
  `TestStreamResumePropertyFuzz` (25 cycles at the client tier, count-
  exact applied-seq multiset).
- *"Mixed-version rolls are safe"* — an old agentd ignores the unknown
  `since` field and opens snapshot-first; the client's first-frame
  disambiguation handles it. Validated by construction (proto3 unknown-
  field tolerance) and pinned at the client tier
  (`TestStreamResumeFallbackBelowLowWater` treats a snapshot-first
  open as fallback without error).
- *"A lost client cursor degrades, never loses"* — the cursor is
  in-memory per Client/gate; a fresh client snapshots. The durable
  ws_state variant was explicitly declined (tradeoff documented at the
  `Client` type) — validated by the fallback pins.
- *"[Review r1] The fsync'd cursor makes ahead-of-authority cursors
  impossible"* — this assumption was FALSE as stated: a lost/reset
  cursor file under a stable endpoint renumbers the seq space. Fixed
  with the `since > a.seq` guard (honest fallback), pinned by
  `TestReplayCursorBoundsCaughtUpAndAhead`. This is the exact drift
  mode README §Worklog warns about — the review round caught an
  assumption the design doc stated as fact.
- *"[Review r1] P0's 'never a silent empty replay' held in all
  windows"* — also false: the nil/empty-ring early returns kept the
  no-state window silent. Fixed + pinned
  (`TestUserBroker_ReplayGapRuleCompleteTable` case 4).

## 3. Proof that the billing windows close

`api/internal/services/usagestream/billing_replay_test.go`, kill-window
fault injection against the REAL authority + REAL abiclient + REAL
consumer: window 1 (slow-consumer drop — fold stalled mid-billing, the
eviction pinned deterministically by an armed wrapper reproducing the
drop's exact wire shape; no kernel-buffer dependence after a flood-based
attempt proved CI-buffer-dependent), window 2 (transport gap — run-loop
redial on the reused client resumes and bills exactly once, ZERO
`StreamResynced` firings — the no-double-fire pin). Window 3
(gate-closed) is measured-but-unaddressed by design per the audit's
recommendation (measure the sub-tick miss rate before any gate-poke
coupling).

## 4. The #1884 reconciliation (behavior change, deliberate)

Every snapshot seed that remains (initial connect, reseed redial,
below-low-water/client-ahead fallback) fires `StreamResynced` → the
authority flight; a served resume fires nothing. The two #1884 e2e rows
were re-based onto the honest fallback via a tiny-ring fixture
(`newKillingAuthorityRing`, `newReconcileConsumerRetry`) so the notify
path stays pinned without depending on the loss it used to ride.

## 5. Verification

`go build ./...`; `go test ./pkg/abi/... ./cmd/workspace-agentd/sessionstate/... ./api/...`
(incl. `-race` on the abiclient + usagestream suites);
`golangci-lint run` — green. `make abi-generate` adopted for the TS
artifact (the CI codegen-freshness gate caught the first push — commit
`2af7e852`). Review r1 findings (ahead-of-authority guard, P0 no-ring
window, worklog, CHANGELOG dedup, stale design sentence, ring belt)
addressed on the loop; the duplicated CHANGELOG section was an
extraction bug in the PR-prep script, caught by the reviewer and by the
merge-conflict resolution.

## 6. Known residuals (documented, not built)

- A resumed connection skips the `derived`-baseline rebuild; exposure
  requires a stale-true consult lease surviving the dead window AND
  persistent consult failure on every replayed leg (fail-open re-ORs).
  Belt noted in design 0069 §6 (age-out across resumes) if ever
  observed.
- A wedged resumed connect on an idle pod waits for ctx/transport
  instead of the 2s first-frame budget (silence is legitimate for a
  caught-up cursor) — the documented steady-state posture.
- Window 3 (gate-closed) — measured-but-unaddressed.
