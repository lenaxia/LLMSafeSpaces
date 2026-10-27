# 0069 — Authority replay ring + `Events.since` resume: amending design 0055 M1's no-replay ruling

**Status:** Design + implementation (2026-10-26) — the amendment is owner-approved (the P0+P1 verdict of the SSE delivery-guarantees investigation, `/workspace/reports/sse-delivery-guarantees-investigation.md` §L2/§L3); this document records the ruling and the mechanism. Design 0055 stays as history; its M1 no-replay clause is amended only to the extent written here.
**Date:** 2026-10-26
**Amends:** `design/0055_2026-08-29_agentd-session-state-authority.md` M1 ("There is no ring buffer, no `since=` cursor, no replay machinery", :179-181) and the matching abi.proto header note.
**References:** design 0056 (harness ABI IDL/transport — the additive-evolution process this change follows); the investigation report above (the per-leg delivery matrix and the verdict's P1 row); design 0049 §4.1 (contract types the ring carries).
**Affects:** `pkg/abi/llmsafespaces/abi/v1/abi.proto` (+ regenerated `pkg/abi/v1`, `pkg/abi/v1/abiconnect`), `cmd/workspace-agentd/sessionstate/` (new `replay.go`; `authority.go`, `service.go`), this repo's test suite (`replay_test.go`).

---

## 1. The problem, in half a page

Design 0055 chose snapshot-reconstruction over replay for the agentd session-state authority's event stream, and that choice was *earned*: every state the stream feeds is derivable from the stamped snapshot (I2's atomic cut), so a reconnecting client converges by re-snapshotting. The audit in the SSE investigation found exactly one consumer class for which that is not true: **per-event side effects with no correction path** — concretely, billing. `MESSAGE_END` frames carry token/cost data that no snapshot reproduces (`SessionSnapshot` has status/parts/inputs/busy — no usage), the API's usagestream consumer fires billing on the event, and an event dropped in the agentd fanout (slow-consumer eviction, transport flap, reseed redial) is never billed. The DB write is idempotent by construction (deterministic key `(workspace, message, seq)`), so *redelivery is always safe* — the missing half was a way to redeliver.

The verdict: keep every other at-most-once leg as documented; upgrade only this one (L2 ring + L3 resume as one protocol change). The op set stays five; the change is an additive optional field on an existing op's request, exactly the versioned-wire-change path design 0055's freeze anticipated ("design 0055:225-234 freezes the op count, not the request schema").

## 2. The ruling

**Design 0055 M1's sentence "There is no ring buffer, no `since=` cursor, no replay machinery" is amended to:**

> The authority retains a **bounded replay ring** of published stream frames (count-capped and byte-budgeted, evict-oldest) and the Events op accepts an **optional `since`** — the caller's last applied seq. A served cursor is resumed gaplessly from the ring; an unservable cursor degrades to the snapshot-first protocol that was 0055's only mode. The ABI op SET stays five; no new ops, no new frame types, no change to the snapshot-first contract.

**What is NOT amended:**

- The stamped snapshot remains the authority for soundness. `since` resume is *parasitic* on I2: any cursor the ring can serve exactly is defined by the ring's contiguity with the seq space, and any cursor it cannot serve falls back to the snapshot. Resume never fabricates state the snapshot machinery doesn't already guarantee.
- I2's connection ordering (register buffer → stamp under the lock → flush → live) is unchanged for snapshot-mode connections, and is *generalized* (not replaced) for resume connections (§3).
- The browser-facing legs (L4a/b/c) are untouched: they keep their own rings/signals and their snapshot-reconciliation model. Only the ABI Events leg (op 1) grows a resume path.
- M3.4 backpressure is unchanged: a slow live subscriber is still dropped; the difference is that its reconnect can now present a cursor instead of always paying a full snapshot.

## 3. Mechanism

### 3.1 The ring

A `replayRing` on the authority (`a.mu`-guarded) holds `*abiv1.StreamFrame` values — **every frame that consumes a seq and reaches `fanoutLocked`**: harness events, reseed notices, orphan-sweep aborts, platform-local action events. Ringing at the *fanout seam* (rather than only inside `applyLocked`) is load-bearing: the reseed notice consumes a seq; a ring that skipped it would let a resumed client apply post-reseed deltas onto a pre-rebuild fold without ever seeing the mandatory re-snapshot signal — the exact silent-divergence class this amendment exists to close.

Eviction is evict-oldest under **two independent budgets**:

- count cap, default **1024 frames** (a busy turn emits tens-to-hundreds of frames; 1024 covers a full turn with margin);
- byte budget, default **16 MiB**, mirroring the SSE line-buffer precedent (`session_tracker.go:452-468`: a 100K-file patch event is ~10 MB — the ring must absorb bursts of large frames without becoming the pod's largest allocation). Frame size is `proto.Size(frame)`.

One edge rule: **the newest frame is always retained**, even when it alone exceeds the byte budget. A ring that dropped it would advance the low-water mark past a seq nothing was evicted for — incoherent water marks for a one-frame memory saving.

Frames are shared pointers, not copies — the same immutability discipline fanout already relies on (the projection clones everything it retains, so frames are never mutated after publish). Worst-case ring memory is the byte budget plus one oversized frame.

Water marks: `lowWater = oldest retained seq − 1` (empty ring ⇒ `current seq` — trivially servable, nothing before it left to miss); `highWater = current seq`. Published seqs are contiguous (one seq per published frame; cursor-persist failures drop the event *and* the seq together), so **any `since ≥ lowWater` is served exactly** — the property the handler's resume decision rests on.

### 3.2 Resume connections (`StreamSince`)

`EventsRequest.since` present means: *"I hold exactly the fold of events 1..since."* The authority's decision, under one `a.mu` hold:

- `since ≥ lowWater` → **resume**. The stream opens with the retained frames `seq > since` in publish order — **no snapshot frame** — then continues live. Gapless and duplicate-free by construction: subscriber registration and the replay-tail copy happen inside the *same* lock hold as ring appends and fanout sends, so every frame published before the hold is in the copy (and can never enter the channel), and every frame published after is in the channel (and was never in the copy). This is I2's atomic-cut discipline, generalized from "snapshot + buffer" to "ring tail + buffer".
- `since < lowWater` → **honest fallback**: byte-identical today's behavior (snapshot frame first, then live). Not an error — a cursor older than the ring is the normal state after a process restart (the ring is memory; only the seq counter is durable) or a slow reconnect.

**Client-side disambiguation needs no wire change beyond the field itself:** the first frame tells the story — a `SnapshotFrame` means "resume not honored; apply the stamp's discard rule" (identical to today's sync protocol), anything else means "replaying from your cursor". A `ReseedNotice` inside the replayed range is delivered in seq position; the mandatory re-snapshot rule applies to it as on any connection.

A cursor exactly AT the current seq resumes with an empty replay and goes live — the legitimate caught-up case. A cursor BEYOND the current seq is a client-ahead cursor and **also falls back to the snapshot** (review r1 guard): the durable cursor makes ahead-of-authority cursors practically impossible from a healthy authority, but "practically impossible" is not a code guarantee — a lost or reset cursor file under a stable endpoint renumbers the seq space under a client that still holds the old fold, and serving that cursor as an empty resume would make the client's discard rule silently drop the entire renumbered stream: the exact silent-gap class this design exists to close (and the sibling L4a P0 fix closes on the browser leg). The first-frame disambiguation needs no change — the fallback's SnapshotFrame re-seeds the fold.

### 3.3 Wire and freeze-gate compliance

`optional uint64 since = 1` on `EventsRequest` — the first field of a previously empty message, additive, explicit-presence so `since = 0` is expressible ("replay everything the ring holds"). The op SET stays five: `TestSchemaSurfaceCompleteness` (method set, closed unions) and `buf breaking` against the S2 freeze baseline both admit additive fields; no gate is loosened. Consumers that never send `since` see byte-identical behavior — the API's contract-stream manager and the S1 comparator are unaffected until the follow-up consumer work opts in.

### 3.4 Observability

`Metrics()` grows the ring's shape: `ReplayRingFrames`, `ReplayRingBytes`, `ReplayLowWater`, `ReplayHighWater`, and `ReplayFallbacks` (resume attempts that degraded to the snapshot — any unservable cursor: below low-water OR beyond the current seq, the client-ahead guard; a persistently high low-water rate means consumers reconnect slower than the ring turns over or restarts keep eating it, a client-ahead rate means cursor-file resets or endpoint reuse across authority generations). `ReplayWindow()` exposes the water marks for tests and diagnostics.

## 4. Test plan (landed with this change)

`cmd/workspace-agentd/sessionstate/replay_test.go`, built on the discard-rule property-fuzz and kill-window fault-injection precedents:

- **`TestReplayResumeGaplessProperty`** — 60 randomized disconnect/resume cycles (random pre-connect prefix, random loss window, random live-during-replay tail): every resumed stream is exactly `since+1 … final`, contiguous, snapshot-free. The no-gap/no-duplicate property across the reconnect AND replay→live boundaries.
- **`TestReplayResumeUnderConcurrentFloodRace`** — the same seam under an ingest flood, chained resumes, `-race`.
- **`TestRingEvictionBoundary`** / **`TestRingByteBudgetEviction`** / **`TestRingSingleOversizedFrameAlwaysRetained`** — evict-oldest order, both budgets independently, water-mark exactness at the eviction frontier, resume at exactly low-water, and the oversized-frame edge.
- **`TestReplayFallbackBelowLowWater`** / **`TestReplayAfterKill9FallsBackHonest`** — the fallback is today's protocol (snapshot first, stamp rule), including across a hard process kill (ring dies, cursor survives → old cursor honestly refused).
- **`TestReplayAcrossReseedNotice`** — a resume spanning a reseed delivers the notice in seq position.
- **`TestEventsSinceOnWire`** — the connect handler end-to-end: absent `since` pins today's snapshot-first behavior; served `since` opens at `since+1`; below-low-water `since` opens with a snapshot.

## 5. Follow-up (out of scope here)

The consumers — `abiclient` resume from its last applied seq, the API usagestream/billing rider persisting that cursor, and the regenerated TS ABI artifact — land in the P1b follow-up task; this change is deliberately server-side only, and every client that ignores `since` behaves exactly as before.

---

## 6. The consumer half, landed (P1b addendum, 2026-10-27)

The follow-up landed: `pkg/abi/abiclient/client.go` (resume), `api/internal/services/usagestream/consumer.go` (gate-level client reuse + seed suppression), and the fault-injection proof in `pkg/abi/abiclient/client_resume_test.go` + `api/internal/services/usagestream/billing_replay_test.go`. The rulings this half adds to the record:

**The fold lives on the `Client`, not the `Stream` call.** A cursor is only truthful if the fold it summarizes survives to present it — so `Stream` folds into client-held state, and every reconnect (the internal redials AND later `Stream` calls) presents `since = fold.Seq`. The usagestream gate reuses one client per workspace while the resolved endpoint is stable (a changed pod or credential rebuilds — never present one authority's cursor to another). The cursor is in-memory: it dies with the gate/client and the next connection takes a snapshot — always sound. The durable ws_state variant (`api/internal/services/wsstate/store.go` precedent) was considered and declined: it would survive API rolls, but a lost cursor degrades to one snapshot, never to loss — not worth the coupling.

**Reseed inside a replay range pays the snapshot.** A `ReseedNotice` delivered mid-replay marks the fold invalid and the redial reconnects WITHOUT `since`: replaying post-reseed deltas onto a pre-reseed fold would diverge it, and replaying the notice again would loop the redial (the fold's cursor never advances past a notice it must not apply). Frames replayed BEFORE the notice stay applied — that is exactly the billing window the resume exists to close; the residual window is only [notice seen → fresh snapshot stamp], subsumed state-wise with cost data lost — far smaller than today's every-gap loss, and inherent to the reseed contract itself.

**Client disambiguation needs no negotiation.** The first frame tells the story (`SnapshotFrame` ⇒ resume not honored — initial connect, below-low-water fallback, or a pre-0069 agentd that ignores the field; anything else ⇒ replaying from the cursor), which makes mixed-version rolls safe with zero version probing. One posture change: the 2s first-frame wedge budget applies only while a snapshot is owed; a resumed, caught-up, idle connection legitimately hears nothing (steady-state semantics extend to the resumed connection's first frame — silence was already acceptable post-snapshot).

**A resumed connection is not a seed.** #1884's snapshot-seed machinery (derived-baseline rebuild, #1666 busy corrections, the `StreamResynced` → authority flight) exists to reconcile SILENT LOSS; a served resume loses nothing, so the consumer suppresses it via the new `WithResynced` counterpart — `WithResumed` — and the seed machinery runs exactly when a snapshot actually lands: the initial connect, the reseed redial, and the honest fallback. The #1884 e2e rows were re-based onto that fallback (a tiny-ring fixture evicts the cursor past low-water during the dead window); every seed that remains fires the flight, and no resume ever does.

**Proof that the billing windows close** (kill-window fault injection, the `authority_test.go` precedent): window 1 (agentd-side slow-consumer drop — fold stalled mid-billing, subscriber evicted; the drop pinned deterministically by an armed wrapper reproducing the eviction's exact wire shape, clean stream end, since kernel buffer sizes would make a flood-based drop flaky) and window 2 (transport gap — stream death with billables publishing while dead, recovery through the run-loop redial on the reused client) each recover every dropped MESSAGE_END via the ring replay and bill it exactly once; duplicate redelivery produces byte-identical `Usage`, so the deterministic `(workspace, message, seq)` idempotency keys absorb it. **Window 3 (gate-closed: a turn starting entirely inside the pod arms no gate) is measured-but-unaddressed by design** — the investigation's recommendation stands: measure the sub-tick miss rate before building any agentd→API gate-poke coupling. The regenerated TS ABI artifact ships in the same PR (`make abi-generate` — the codegen-freshness gate demands it), ahead of any frontend consumer of `since`.

**Review r1 hardening (the AI review round):** (1) the client-ahead guard above (`since > a.seq` → honest fallback, pinned by `TestReplayCursorBoundsCaughtUpAndAhead`); (2) the P0 no-ring window — `Replay`/`sinceLocked` previously returned `(nil, false)` for a user with NO ring state, silently reproducing the exact client-ahead gap the P0 arm fixes for the populated ring; both arms now gap on any presented cursor (`lastID > 0`), pinned in `TestUserBroker_ReplayGapRuleCompleteTable`; (3) a `replayRing.append` belt refuses to ring a seq-less frame (unreachable today — both `fanoutLocked` call sites publish seq'd frames — but a future frame kind would otherwise corrupt `lowWater` by underflow); (4) documented residual: a resumed connection skips the `derived`-baseline rebuild that snapshot seeds perform (`WithResumed` suppresses the seed branch) — validated narrow because every busy mutation flows through `applyLocked` and is therefore ringed, so replayed legs re-consult and reconstruct `derived`; the residual requires a stale-true consult lease surviving the dead window AND every replayed leg's consult persistently failing (fail-open re-ORs the stale true). A belt (age-out of derived entries with no successful consult across a resume) is noted as the follow-up should that compound case ever be observed.
