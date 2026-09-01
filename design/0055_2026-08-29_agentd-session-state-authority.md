# 0055 — agentd session state authority: the pod-local headless TUI

**Status:** Implemented (Epic 69: US-69.1–.13 merged; US-69.14 history terminus deferred upstream)
**Status (original):** Accepted (2026-08-29, owner) — Epic 69 filed: #1134 (US-69.1–.14 = #1135–#1148)
**Date:** 2026-08-29
**Depends on:** 0053 (overlay delivery — agentd+opencode version coupling), 0049/Epic 65 (session contract), Epic 22 (agentd health isolation), design 0051 (uid separation), 0052/0054 (V2 delivery)
**Revives:** Epic 18 S18.3 (route proxy traffic through agentd) — designed 2026-06, shelved with its parent epic (`Status: Planning`), never decided on merits
**Retroactively answers:** the two objections the incident history raises against in-pod state (starvation, dual authority) — see M3/M4

---

## Problem

Session truth is currently derived in the wrong failure domain. Every
state-bearing mechanism lives in the API server, one failure domain away
from the process that produces the truth:

| Mechanism | Location | Symptom of wrong-domain placement |
|---|---|---|
| SSE tracker (busy/streaming derivation) | API (`proxy_events.go`) | Reconnect reseed heuristics, idle-timeout vs event-derivation disagreement (0054 amendment: "two turn-lifecycle sources"), 14k+ `unknown`-classified events |
| Event taxonomy bridge (0054 Mechanism 3) | API — unbuilt | Must translate `session.next.*` cross-domain, cross-version |
| Verify oracle (`verifydelivery.go`) | API | Exact-text scan over paged history, 2-min clock-skew margin, bounded no-keep-alive client — all compensating for reading another domain's store over a network |
| Frontend reconnect state | API + browser | Snapshot reconstruction instead of replay |

agentd already subscribes to opencode's SSE stream
(`sessionStatusTracker`, commit `01f3997c`, 2026-05-29) — but consumes it
only for statusz and session-aware restart (#852). The delivery path
bypasses agentd entirely (`proxy.go:482-519` routes directly to
`podIP:4096`).

### Why this was unreachable before (and is reachable now)

1. **agentd was never in the traffic path.** Epic 18 S18.3 designed
   route-through-agentd (`AgentdPort = 4097 // ... future proxy`,
   `pkg/agentd/types.go:77`) but hot migration stalled; the idea was
   shelved, not rejected.
2. **0050 D3's axiom over-applied.** "A networked API cannot share fate"
   is true for the accept/202 path. It was extended to delivery
   *observation*, where agentd — the supervisor, the parent of opencode,
   the same-pod same-clock process — can share fate, which is exactly
   what verification wants.
3. **The earned distrust of stored state (0818)** — three generations of
   stored-busy failed *because the writer could miss events*. A
   store-reseeded projection with stamped snapshots is the first
   mechanism that satisfies the principle rather than violating it:
   derived state is never trusted across a writer gap — it is rebuilt
   from the store, and consumers re-snapshot.
4. **The version-coupling precondition did not exist.** Until 0053
   (2026-08-28), agentd and opencode were independently versioned; an
   agentd that deeply knew opencode's wire contracts would have recreated
   version skew inside the pod. 0053 pins both artifacts together under
   platform control.

---

## Product requirements

- **R1 — Send anytime.** Prompts are accepted while a session is idle,
  busy, or mid-turn; acceptance never blocks on turn completion
  (preserves 0050 R1).
- **R2 — Delivery integrity, honestly stated.** Every accepted prompt
  is admitted to the agent **at most once per attempt and at least once
  overall**; loss or duplication never occurs *silently* — every
  ambiguity resolves to bounded, visible ledger state.
- **R3 — Crash tolerance.** Accepted prompts survive API crash, agentd
  crash, opencode crash, and pod suspend/resume; recovery is automatic,
  ID-keyed, and wake-first (never re-admission).
- **R4 — State truthfulness.** Any client reconnecting receives correct
  state in one snapshot; busy/streaming/queue state is never orphaned
  by a dead generation (the 2026-08-15 phantom-busy class is
  structurally eliminated).
- **R5 — Observability.** Seq advance, ledger states, promotion stalls,
  and snapshot costs are first-class metrics; in-pod starvation is
  visible from outside rather than guessed from timeouts (0050 D1's
  progress taxonomy, realized).
- **R6 — Containment.** All opencode *event*, *delivery*, and
  *session-action* wire knowledge lives inside the pod (the
  `sessionstate` module); the outward surface is frozen at five
  operations in platform schema. The API retains adapter-translated
  store CRUD for now — paginated history reads, credentials staging,
  session CRUD, model list (see "History reads" in M1) — so the
  containment boundary is events+delivery+actions, not yet every
  opencode surface.
- **R7 — Reversibility.** The state authority can move to a dedicated
  container with zero consumer changes (the 5-op contract is stable).
- **R8 — Rollback.** `AGENTD_STATE_AUTHORITY=off` returns to 0052 paths
  with no user-visible loss; the ledger remains readable and back-drains.
- **R9 — Bounded cost.** One API stream per pod; agentd overhead is
  bounded and independent of transcript length; snapshot cost is
  O(in-flight state), never O(history); no per-session processes.

---

## Authority boundary (the dual-authority answer, stated first)

One authority per fact. No fact has two owners.

| Fact | Authority | Notes |
|---|---|---|
| Durable accept of user intent (202 semantics) | **API outbox** (Valkey) — unchanged | Must survive pod death/suspension and serve any replica. I1 keeps its current owner. |
| Sessions/busy/streaming/queue state of a *reachable* pod | **agentd** | Sole derivation point; API proxies, never re-derives |
| Session-scoped event stream | **agentd** (stamped-snapshot + ordered events) | Replaces the API tracker + 0054 Mechanism 3 |
| In-pod delivery outcome (admitted/failed, per entry) | **agentd ledger** | ID-keyed; replaces the text-matching oracle |
| Cross-workspace, user-scoped events (`workspace.phase` etc.) | **API** (Epic 28) | Can never come from a single pod; unchanged |
| Auth, tenancy, quotas, catalog, billing | **API** | agentd gains no business logic |

The API outbox remains the accept seam. What changes is its delivery
terminus (opencode → agentd) and its verification source (text-scan →
ledger lookup). The I5 discipline ("delivery, read, and verify agree on
the same store") becomes structural: delivery, read, and verify all
terminate at agentd, in the pod, next to the store.

---

## Placement: why agentd (and the reversibility condition)

The load-bearing reason is the **generation signal**. Every mechanism in
this design — projection resets, ledger replay, `generation.changed`
markers — depends on knowing exactly when opencode died and restarted.
agentd is opencode's parent: `cmd.Wait()` and the `onChildStarted` hook
are the authoritative generation boundary. The 2026-08-15 phantom-busy
incident (0050 D2) happened because generation truth was inferred from
outside; any non-parent placement reintroduces that inference.

The honest case against, and its answers:

- **Supervisor/observer coupling** — a projection panic must not become
  a pod restart killing a running turn. Answered structurally: the
  authority is a **module-sealed subsystem** (`cmd/workspace-agentd/sessionstate/`),
  own package, own goroutines, recover walls around all externally-shaped
  parsing, sole owner of the 4097 listener.
- **Charter bloat** — answered by the frozen 4-op outward surface (M1);
  the subsystem's charter is exactly "session state authority," nothing
  else migrates into it.
- **Cgroup sharing** — bounded (M3); and every in-pod alternative shares
  the same cgroup, so this differentiates in-pod vs API-side, not agentd
  vs sidecar.

**Reversibility condition:** because everything outside the pod sees
only the 4-op contract, the process boundary is deliberately reversible
— if the coupling ever manifests, `sessionstate` moves to its own
container (the 0053 overlay machinery already supports per-artifact
delivery) with zero consumer changes. Alternatives considered and
rejected: a new sessiond sidecar (not the parent — generation signal
becomes inference; +1 container per pod under per-tenant quotas); an
out-of-pod per-workspace gateway (recreates cross-domain observation
plus new stateful infra — dominated); upstream opencode ownership
(retires M2's receipt fallback but will never own the Epic-65
projection); client-side derivation (multiplies version-skew across N
SDKs — what Epic 65 exists to prevent).

---

## Design

### M1 — Stamped snapshots + ordered events (no replay)

Extend agentd's existing SSE subscription into a durable projection:

- Every inbound opencode event is assigned a **monotonic seq** under the
  same lock as the projection update, then translated to the Epic 65
  contract (`pkg/session` part types) — the sole `session.next.*` →
  contract derivation point. The opencode dialect becomes pod-internal.
  opencode's own ascending event IDs are recorded as cross-checks only.
- **Sync protocol — snapshot-on-connect, no replay buffer:**

  ```
  connect → snapshot@S → event@S+1 → event@S+2 → ...
  client rule: apply in order; discard seq ≤ S
  ```

  Soundness rests on two invariants the single-process design provides
  by construction: (a) the snapshot and its stamp are captured under the
  projection lock — state is exactly the fold of events 1..S (atomic
  cut); (b) the stream is ordered by the single seq assigner. Deltas are
  anchored by `started` events and the snapshot's partials already
  reflect everything ≤ S, so post-snapshot deltas append exactly. The
  TUI itself syncs this way (fetch current state, then live tail — no
  replay); this design adopts the proven pattern. There is no ring
  buffer, no `since=` cursor, no replay machinery; reconnect mid-turn
  re-fetches in-flight parts (bounded per turn, not history).

  **Connection ordering (mandatory — closes the gap direction):** the
  atomic stamp solves overlap (duplicate application); the fatal
  direction is the *gap* — events emitted between snapshot capture and
  live attach. Every stream connection MUST therefore: (1) register a
  per-connection buffer with the fanout, (2) capture snapshot@S under
  the projection lock, (3) flush buffered events with seq > S, (4) go
  live. The buffer is transient and per-connection (seconds, not a
  replay log); without this ordering the protocol silently loses
  `S+1..S+k` and the loss is invisible.

  **Snapshot content completeness:** busy/streaming state, in-flight
  parts (with partials), ledger-derived queue depth (M2), and **pending
  question/permission requests** — a reconnecting client must be able to
  act (answer a standing question) without any opencode call. Unknown
  event types pass through the contract's Custom valve (the 14k-unknown
  lesson relocated, not repeated).

- **Projection rebuildability (the load-bearing invariant):** the event
  log is a delta accelerator, never the source of truth. Events can be
  lost in the opencode-emit→agentd-ingest window (agentd crash, or
  agentd rollout while opencode lives — routine in sidecar mode). On
  every agentd boot and every `generation.changed`, the projection is
  **reseeded from opencode's persisted store**, then the seq counter
  resumes from the PVC ledger. First-enable on workspaces with existing
  history seeds the same way. Consequence: snapshots are trustworthy
  after any restart because they are store-derived, not log-derived.
- **Reseed procedure (ordered, race-free):** on generation change or
  boot: (1) quiesce the projection (pause fanout), (2) buffer inbound
  events, (3) reseed state from the store, (4) emit a synthetic
  `projection.reseeded` event consuming the next seq, (5) flush buffered
  events, (6) resume live. Live clients treat `projection.reseeded` as
  a mandatory re-snapshot trigger (reconnect); the synthetic event
  guarantees seq monotonicity across the rebuild and gives clients an
  explicit invalidation signal. Residual: pre-enable stranded V2 rows
  are invisible to the reseed (documented bootstrap blindness — M2's
  wake covers them only after first observation).
- **Generation changes** (opencode restart): seq continues (counter is
  ledger-persisted); the procedure above runs. agentd survives
  opencode's death by construction (it is the parent).

New endpoints on `AgentdPort` (4097), consumed by the API proxy:

```
GET  /v1/events                      # snapshot frame, then live (SSE)
GET  /v1/sessions/:id/snapshot       # contract-shaped: busy, last parts, queue depth
GET  /v1/sessions/:id/deliveries/:entryID   # ledger outcome lookup
POST /v1/sessions/:id/deliveries     # idempotent delivery, parts-capable
                                      # schema; file parts capability-gated (D3)
POST /v1/sessions/:id/actions        # typed action union (below)
```

That is the entire outward surface — five operations, platform schema.
The second-brain guardrail: if a change wants more than these, it
belongs in the API.

**Typed actions (op 5) — one verb slot, capability-negotiated.**
`POST .../actions` takes a discriminated union — `interrupt`,
`switch_model`, `switch_agent`, `answer_question`, `compact` — and
returns a typed result or `NotSupported` per the capability report
(below). This does two jobs:

1. **Closes the control-op dual-writer gap** (round-2 finding M4/W4):
   control operations stop being API→opencode side-channels; agentd
   becomes the sole writer for session mutations, serialized against
   delivery by the same single-flight machinery. The S1 concurrency
   matrix now decides *exceptions* (if any), not the default.
2. **Seeds the harness ABI** (see that section): harness-divergent
   verbs (compact exists in opencode V2 and claude-code; pi may differ)
   are typed, capability-flagged differences — not adapter-shaped
   branches in the API.

Session create/CRUD, credentials staging, and model catalog remain
API-side adapter surface (reads and lifecycle, not turn mutations);
history stays adapter-side per the pagination note below until S5.
The capability report (provenance predicate today) extends to carry
supported actions and surface versions.

**History reads (pagination) — deliberately not a sixth op yet.**
History is not the snapshot: snapshots are O(in-flight state) (R9);
transcript loads are **paginated store reads** and stay on the existing
adapter path — V1 `GET /session/:id/message?limit=&before=` cursor
passthrough; the V2 store's full-list wart (no pagination on 1.18.15)
is absorbed there as today (0052's `WithV2Store` translation). The
stitch rule that makes the dual path exact: **history page ∪
snapshot@S ∪ events>S reconcile by entity (message/part) ID** — the
projection preserves store IDs through contract translation (S1
verifies), so no timestamp grace windows are needed. Upstream ask
(file with the 0054 receipts ask): `limit`/`before` on
`GET /api/session/:id/message`. If it lands, an optional S5 folds
history into this surface as a thin paginated fifth op with stamp
correlation for free — until then, moving it buys containment purity at
  real cost (V2 wart handling, big-payload proxying in a starved pod) and
  is not taken.

**Harness ABI (multi-harness trajectory — pi, claude-code).** This
surface is the placement design 0049 lacked: each harness gets a
pod-side adapter module (`sessionstate-<harness>`) implementing the
same five ops against its native integration — HTTP for opencode,
process-driving for claude-code (agentd's existing competency),
Go-native for pi — with version coupling per pod (0053 machinery per
artifact). Rules:

- **IDL at S1 start, freeze at S2 (D5 decision).** The schema (proto
  IDL; Connect vs gRPC transport chosen with the toolchain at S1
  start) generates Go server/client stubs, frontend TS types, and the
  contract tests that drive the S1 comparator. It evolves freely
  during shadow and freezes when the outbox binds (S2 entrance).
  Epic 65 co-owns the type source-of-truth migration
  (`pkg/session` → schema) — that conversation happens at S1 start.
- **History is defined in the ABI** (cursor-typed) but implemented for
  opencode only at S5 (upstream pagination); new harnesses are
  ABI-native for history from day one.
- **Capabilities are load-bearing, not cosmetic:** every typed action
  and surface version is declared in the capability report;
  undeclared actions return `NotSupported` — harness differences are
  data, never API-side branches.

### M2 — Delivery ledger and idempotent in-pod delivery

- The outbox's `outboxDeliver` changes terminus: `POST .../deliveries`
  with the outbox's own **entryID** and **attempt** (outbox-controlled,
  incremented on outbox-directed retry), text, model override.
  202-from-agentd means "durably ledgered," not "admitted to opencode."
  Dedupe is scoped to `(entryID, attempt)` — **at-most-once admission
  per attempt**, so a terminally-`failed` attempt is re-armable while an
  entryID's admissions remain auditable forever. agentd drives admission
  locally with retry (**at-least-once effort**). **Per-session
  single-flight** — at most one delivery in flight per session — bounds
  every ambiguity window (crash-after-admit, identical texts, ordering
  across the API-outbox/agentd-ledger boundary during suspend/resume)
  to exactly one entry.
- **Ledger state machine:** `ledgered → admitted → promoted →
  turn-ended`, with terminal `failed` (per attempt). Stopping at
  `admitted` as *the whole story* would reintroduce the #1119 stranding
  class; the promotion deadline/reaper semantics of 0054's
  promotion-await move into this machine: `admitted` past the promotion
  deadline becomes **`stalled` — recovered by WAKE ONLY** (US-63.9
  drain-trigger semantics). Re-admitting an admitted entry is forbidden:
  a second prompt creates a second durable row and the eventual drain
  runs both turns. agentd — as the ledger owner — inherits US-63.9's
  wake trigger (drain-on-resume) and US-63.10's queue visibility
  (`v2Shadow` retires; see below).
- **Interrupt semantics (V2 non-destructive):** interrupt affects turn
  projection state only; it NEVER mutates entry states. Admitted-
  not-promoted rows survive an interrupt by design (F8, Epic 63's
  accepted UX) and run after it — marking them failed would manufacture
  phantom failures and invite duplicate re-sends. There is no
  `superseded-by-interrupt` entry state; the only interrupt/delivery
  race is an admission POST completing during an interrupt, where the
  landed row is preserved queued input — correct by construction.
- **State-mapping table (load-bearing for delivery integrity):**

  | Ledger state | Outbox entry | User queue UI | Recovery on stall |
  |---|---|---|---|
  | `ledgered` | delivering | queued | agentd retry loop |
  | `admitted` | **delivered** (terminal; 0052 semantics) | sent | **wake only** — never re-admit |
  | `stalled` (admitted > deadline) | delivered + alert | sent (stalled badge) | wake; alert if wake fails |
  | `promoted` | observability only | running | — |
  | `turn-ended` | observability only | done | — |
  | `failed` (attempt) | retry/backoff per outbox policy | error with reason | re-arm `(entryID, attempt+1)` |

- Admission correlation is ID-keyed against agentd's own ledger — the
  `since`-floor, clock-skew margin, and page-budget machinery of
  `verifydelivery.go` retire. **S1 spike (cheap, high payoff):** F17
  forbids caller-supplied prompt `id` only on *collision* (409); if
  opencode accepts a fresh unique caller ID, entryID→messageID
  correlation becomes exact and the residual fallback vanishes. Until
  settled, the admit-succeeded-response-lost case is resolved
  *localhost* (same clock, synchronous store read, single-flight bounds
  it to one entry) — behind the adapter seam, invisible to the API.
- **Queue depth is ledger-derived** (`ledgered ∪ admitted-unpromoted`) —
  not store-derived (V2 has no list endpoint; US-63.10's finding) and
  not shadow-derived (Redis `v2Shadow` retires in S3). This works
  because all deliveries flow through agentd; it survives restarts and
  needs no second authority. Documented residual: entries stranded
  before the feature enabled are invisible until first observation.
- **Pod death mid-delivery:** the ledger is PVC-backed; on resume agentd
  replays unresolved entries (and fires drain wakes per the stalled
  rule) without API involvement. The API's recover path asks the
  ledger, gets a definitive answer.
- **Storage:** a fourth PVC subPath (`platform/`), created by the
  workspace-dirs init alongside `workspace/`, `home/`, `tmp/`
  (`pod_builder.go:785`). Owned by uid 2000 (sidecar mode, design 0051;
  the mode dependency is explicit — single-container mode weakens the
  file-perm claim to crash-ambiguity protection only) mode 0640. Only
  the **ledger** is PVC-persistent (WAL append, fsync-before-ack); the
  in-memory projection and its seq counter resume via ledger-cursor +
  store reseed (M1), so there is no event log to persist. Ledger
  compaction is an independent outcome-retention policy (terminal
  outcomes kept N days for outbox resolution), never coupled to any
  consumer cursor.

### M3 — Starvation answers (the Epic 21/22/0050 objection, answered)

Objection: 2026-08-15 showed everything in a 2-CPU pod starving together;
Epic 22 existed to decouple agentd from opencode's responsiveness.

1. **agentd never makes a synchronous opencode call on any hot path.**
   Delivery admission is asynchronous against agentd's own queue; a
   starved opencode delays delivery, never a 202 or a snapshot read
   (snapshot serves from the projection, not from opencode).
2. **The starved actors of 2026-08-15 were opencode's JS event loop and
   every caller making blocking HTTP into it** (Epic 22 A6). agentd is a
   small Go process doing JSON parse + append + fanout. Epic 22's
   healthz decoupling is preserved unchanged.
3. **Starvation becomes observable instead of guessed.** Seq advance is
   the progress signal 0050 D1 wanted but could only approximate with
   CPU ticks: `seq stalled Ns while pod running` is a first-class,
   externally-visible fact. The watchdog demotion matrix keeps its
   no-kill policy and gains better evidence.
4. **Backpressure is bounded:** slow consumer → drop connection;
  consumer resyncs via a fresh stamped snapshot (the protocol makes
  reconnect cheap and always-correct by construction). The API holds at
  most one stream per pod; browser fan-out stays at the API.

### M4 — Mode coherence, flip, rollback (the I5 objection, answered)

- **Flag/capability matrix (I5 discipline as a table):**

  | Surface present (per-pod capability) | `AGENTD_STATE_AUTHORITY` | `OPENCODE_V2_DELIVERY` | Path |
  |---|---|---|---|
  | ✗ (BYO / unpinned / pre-V2 runtime) | forced off | off | V1 direct (today) |
  | ✓ | off | off | V1 direct; API diff-consumes agentd stream (S1 shadow) |
  | ✓ | off | on | 0052 world (adapter V2 store reads) |
  | ✓ | on | **must be on** — wiring rejects `on/off` | outbox→agentd ledger; API reads via agentd only |

  Surface presence is a *capability* (pinned opencode + agentd ≥ this
  design), always additive and harmless; authority is the *flag*. The
  illegal combination is rejected at wiring time, not runtime. **D4
  decision:** once 0053-S3 (mandatory pins) lands, the unpinned row is
  a boot-time guard against misconfiguration, not a supported fleet
  mode — a dual delivery regime is not maintained. Flip
  order: capability ships → S1 shadow (parallel with 0053; zero-
  divergence exit) → 0053-S3 completes → V2 flag → authority flag.
  0054 G1's drain-before-flip becomes trivial: the flip
  procedure asks agentd for in-flight entry count (same domain), waits
  for zero or parks with `mode_transition` reason, flips. No cross-store
  verify. Rollback = authority off; the ledger remains readable and
  back-drains via the 0052 path.

- **4097 enforcement:** every route — including `/v1/events` — requires
  the 0051 `agentdPassword` (the snapshot stream is a content-egress
  path and deliveries an injection path from uid-1000 localhost);
  deliveries are rate-limited per session; unknown event types surface
  via the contract Custom valve rather than silently dropping.
- BYO/pre-V2 runtimes: the provenance predicate (0054) is unchanged and
  moves into agentd's capability report; a pre-V2 image simply yields a
  projection without V2 queue semantics — surfaced, not silently
  degraded.

---

## Invariants

Properties that must hold at all times; each is testable and has a
named enforcement point. A PR that weakens any of these requires a
design amendment, not a code comment.

- **I1 — Single seq authority.** Seqs are assigned only by agentd,
  monotonically, under the same lock as the projection update (atomic
  stamp). No other component assigns ordering.
- **I2 — Subscribe-before-snapshot.** No connection can miss events
  between its snapshot cut and live attach (buffered-attach ordering,
  M1). Fault-injection tested.
- **I3 — Rebuildability.** Projection state is a pure function of
  (store snapshot at last reseed + events since). Reseed runs on every
  agentd boot and every generation change, and emits
  `projection.reseeded` (mandatory client re-snapshot).
- **I4 — Store is truth.** Events never contradict the store after a
  reseed; on conflict, the store wins.
- **I5 — Delivery idempotency.** At-most-once admission per
  `(entryID, attempt)`; at-least-once effort; **per-session
  single-flight** at all times, across API replicas and suspend/resume.
- **I6 — Wake-only stranding recovery.** An `admitted` entry is never
  re-admitted (second row = duplicate turn on drain); stall recovery is
  the US-63.9 wake.
- **I7 — Interrupt purity.** Interrupt mutates turn projection state
  only; never entry states (V2 non-destructive semantics).
- **I8 — Surface authentication.** Every 4097 route requires
  `agentdPassword`; deliveries are rate-limited per session; no
  unauthenticated route exists, including health and events.
- **I9 — Ledger durability.** A delivery 202 implies the entry is
  fsync-persisted on PVC (group-commit allowed; the implication is not).
- **I10 — Fixed completion mapping.** Outbox completion ⟺ ledger
  `admitted` (terminal, 0052 semantics); `promoted`/`turn-ended` are
  observability only. No consumer invents an alternate mapping.
- **I11 — Dialect containment.** No consumer outside the pod sees
  opencode event taxonomy; the outward stream is contract-only.
- **I12 — Snapshot completeness.** A snapshot alone is sufficient to
  render session state and act on pending question/permission requests,
  with zero opencode calls. Stitch rule: history (paginated, adapter
  path) ∪ snapshot ∪ live events reconcile by entity ID — store IDs are
  preserved through contract translation; timestamps are never used for
  stitching.

---

## Test plan (inventory of tests that must exist)

Per-story test plans live in the Epic 69 issues (#1135–#1148); this is
the cross-cutting inventory. Naming: every test maps to an invariant or
a historical incident class — a test that proves neither is noise.

| Suite | Proves | Type | Gate |
|---|---|---|---|
| `stamp_atomicity_race`, `seq_monotonic_across_kill9` | I1 (seq authority, atomic stamp) | `-race`, fault-injection | S1 |
| `connect_race_no_event_loss`, `discard_rule_property_fuzz` | I2 (subscribe-before-snapshot; client fold) | fault-injection, property | S1 |
| `reseed_under_active_streaming`, `reseed_agentd_restart_opencode_alive`, `orphaned_busy_impossible` | I3/I4 (rebuildability; store is truth; the 2026-08-15 class dead) | integration, regression | S1 |
| `translation_golden_fixtures` (per pinned version, wired into the bump gate), `id_preservation_stitch`, `unknown_custom_valve_passthrough` | I11/I12 (dialect containment; ID stitch) | golden | S1 |
| Scenario suite ×6 + soak (streaming, kill -9, agentd restart, suspend/resume, starvation, reseed-under-load) | S1 exit: zero divergence | e2e + soak | S1 |
| Crash matrix (agentd-after-ack, opencode-after-admission, suspend-in-window), `durability_202_survives_kill`, `dedupe_entryid_attempt`, `single_flight_per_session` | I5/I9 (exactly-once per attempt; 202 survives kill) | fault-injection | S2 |
| `stranded_1119_replay`, `interrupt_purity_suite` | I6 (wake-only) / I7 (interrupt purity) — the #1119 and abort-UX classes | regression | S2 |
| `duplicate_contract_suite`, `crash_matrix_via_real_outbox`, `rollback_drill_under_load` | R2/R8 through the real outbox | e2e, drill | S2 |
| `illegal_flag_combo_boot_rejected`, `state_mapping_guard`, `flip_drain_and_park` | I10 + M4 matrix | unit/config, drill | S2 |
| `action_vs_delivery_serialization`, `interrupt_admission_race`, `not_supported_typed` | op-5 serialization + I7 | golden, e2e | S2 |
| `client_discard_rule_unit_property`, `midturn_reconnect_e2e`, `standing_question_reconnect_e2e`, `api_rolling_deploy_e2e`, `old_dialect_dead_code` | I12 client rule; hard cutover clean | unit/property, Playwright | S3 |
| `streams_scale_to_zero`, `two_replicas_single_owner`, `rolling_deploy_no_fanin_storm` | D1-B consumption model | e2e, metrics | S3 |
| Alert rehearsals (starvation, stranded, latency) + `metrics_scrape_completeness` | R5 | drill | S4 |
| `resume_budget_final_p95`, `flipgate_park_with_reason`, epic-exit review | Exit criteria | benchmark, drill | S4 |
| `history_cursor_pages_budget`, `stitch_rule_e2e_post_move`, `adapter_history_paths_dead_code` | US-69.14 | instrumented, e2e | S5 |

Harness rules: the crash-window driver, fixture replay runner, and probe
scripts are committed artifacts; concurrency tests run under `-race` in
CI; fixture goldens regenerate per pinned opencode version through the
existing bump-gate pattern (`agent_config_writer_schema_test.go`
precedent, REFRESH.md provenance).

---

## Non-goals

- No cross-workspace events in agentd (Epic 28 stays API-owned).
- No business logic in agentd: quotas, permissions, catalog, billing
  stay in the API. The outward surface is frozen at the five
  operations of M1/op-5.
- The API outbox is not replaced — accept durability for
  suspended/unreachable workspaces remains Valkey's job.
- No new trust in opencode's own V2 queue beyond 0052/0054 assumptions;
  the 0054 upstream asks (terminal delivery events with messageID echo)
  remain desirable and would retire M2's localhost fallback.

---

## Assumptions (validated)

| # | Assumption | Evidence |
|---|---|---|
| A1 | agentd already subscribes to opencode SSE | `sessionStatusTracker`, commit `01f3997c` (2026-05-29); consumed by statusz, #852 restart, ops_metrics |
| A2 | 4097 is reserved as agentd's user-facing port, "future proxy" | `pkg/agentd/types.go:77` — Epic 18 S18.3's slot, never taken |
| A3 | The Epic 65 contract types exist and are the outward schema | `pkg/session/{part,message,event}.go` |
| A4 | A PVC subPath can be added mechanically | `pod_builder.go:785` — workspace-dirs init already creates three subPath roots |
| A5 | agentd and opencode are version-coupled as of 0053 | design 0053, S1 shipped (worklog 0855) |
| A6 | opencode events carry ascending IDs (usable as cross-check) | opencode `bus/global.ts:21` |
| A7 | The API proxy re-resolves pod IP per request (resume-safe routing) | `proxy.go:482-519` |
| A8 | Suspend keeps PVC, wipes tmpfs | volume layout, README-LLM Relay Config Subsystem |
| A9 | agentd holds the authoritative generation signal (parent of opencode) | `main.go:260-266` — `startManagedProcess` wires `proc.onChildStarted = sseTracker.onOpencodeGenerationStart`; `cmd.Wait()` underlies `managedProcess` |

---

## Rejected alternatives

- **Status quo (0054 Mechanisms 2+3):** keeps the text oracle and an
  API-side bridge; the two-lifecycle-derivations disagreement and the
  unknown-taxonomy inventory remain open items forever.
- **Per-session opencode processes:** opencode's isolation unit is the
  directory (`InstanceRuntime.load({directory})`), not the session — N
  processes give N identical views plus N×(config readers, MCP sets,
  boot cost inside the 22s resume budget) and no ownership semantics.
- **Trusting opencode's V2 queue end-to-end:** #755 (admits-without-drain
  on 1.18.10) and #1119 (defect-class deaths strand admitted rows) —
  the platform must assume best-effort execution regardless.
- **API-side bridge only (0054 M3 without relocation):** fixes display
  but keeps delivery verification cross-domain — the oracle, skew
  margins, and bounded verify clients all survive.

---

## Corner-case matrix

- **agentd restarts (not opencode — routine during platform rollouts in
  sidecar mode):** events in the restart gap are unrecoverable by
  design; the projection reseeds from opencode's store on boot (M1's
  load-bearing invariant), the seq counter resumes from the ledger
  cursor, reconnecting clients receive a fresh stamped snapshot. No
  client ever observes the gap as state — only as a snapshot jump.
- **Suspend/resume:** PVC survives, IP changes; API re-resolves (A7);
  agentd boots, reseeds, replays unresolved ledger entries, emits
  `generation.changed`.
- **API SSE flap mid-turn:** consumer reconnects; new snapshot@S then
  live; discard-≤S makes the overlap exact. No replay, no cursors.
- **opencode wedges mid-turn:** seq stalls (visible, M3.3); snapshot busy
  state carries the 0054-G2 deadline reaper logic, now pod-local.
- **Admitted-but-unpromoted (the #1119 class):** ledger state machine
  (M2) surfaces it — `admitted` past promotion deadline → `failed` with
  reason, entry outcome visible to the outbox. Never silent.
- **Identical texts in one session:** entryID correlation; per-session
  single-flight bounds ambiguity to one entry regardless.
- **Two API replicas:** both proxy to the same agentd; delivery remains
  single-owner via the ledger's per-entryID dedupe; outbox
  consumer-group claiming unchanged.
- **User tampering with ledger:** `platform/` subPath is uid-2000 0640;
  not mounted read-write (or at all) in the workspace container's user
  space — same mount-topology integrity as US-4b. (Both this design and
  the API-side oracle trust the pod's honesty against *malice*; the
  ledger's integrity value is against *crash ambiguity*.)
- **Control operations (interrupt, model/agent switch, question/
  permission answers, compact):** routed through the typed action op
  (M1 op 5) — agentd is the sole writer of session mutations,
  serialized against delivery by single-flight. Session create remains
  API-side (adapter CRUD) with the created session's first events
  entering the projection normally. S1's concurrency matrix documents
  any exception routing and the interrupt/admission-in-flight race
  (I7: an admission completing during an interrupt lands a preserved
  queued row — correct by V2 semantics).

---

## Rollout & acceptance criteria

### S1 — projection + surface (shadow; no cutover)

Build: stamped-snapshot stream, contract translation, snapshot/
delivery-status endpoints, reseed path incl. first-enable seeding,
4097 auth. Spikes: caller-supplied admission ID; control-op concurrency
matrix (interrupt vs admission-in-flight vs admitted-queued vs turn;
session-create routing).

- [x] I2 fault-injection test: events emitted mid-connect are never
      lost (subscribe-before-snapshot ordering proven, not asserted)
- [x] I3 proven: after an agentd restart *with opencode alive* (sidecar
      rollout), projection converges to store truth; live clients
      re-snapshot on `projection.reseeded`
- [ ] Zero-divergence: agentd projection vs the API's own tracker agree
      on busy/streaming/queue across the scenario suite — streaming
      turn, opencode `kill -9` mid-turn, agentd restart mid-turn,
      suspend/resume, CPU-starvation soak under build load (pool — the
      ≥7-day soak rides the staged pool; harness committed, worklog 0866)
- [x] Reseed-under-active-streaming scenario in the suite (M3's race)
- [ ] Admission-ID spike result documented against every pinned
      version; fallback decision recorded per I5 (pool — the per-version
      matrix rides the staged pool; harness committed, worklog 0867)
- [x] IDL (D5): schema exists from S1 start with generated contract
      tests driving the comparator; transport choice (Connect vs gRPC)
      recorded; `pkg/session` source-of-truth plan agreed with Epic 65
- [x] Parts-capable deliveries schema (D3): file parts →
      `NotSupported` per capability report on the opencode adapter;
      multi-text-part join rule contract-tested
- [x] Typed action op: every union member exercised; `NotSupported`
      returned per capability report for undeclared actions; actions
      serialize against in-flight delivery (single-flight) with no
      lost interrupts
- [x] Concurrency matrix reviewed; no duplicate admissions, no phantom
      failures in golden tests
- [x] Snapshot latency p99 < 250ms on a 2-CPU pod under load (served
      from projection, zero opencode calls — M3.1)

### S2 — delivery terminus (`AGENTD_STATE_AUTHORITY=on`, flagged, staged pool)

**Gated on 0053-S3 (mandatory pins) — D4 decision; single-regime fleet.**

- [x] Flag matrix (M4) implemented; illegal `on/off` combination
      rejected at wiring; capability predicate gates the surface per pod
- [x] Crash-injection suite, each green with exactly-once admission per
      attempt and zero duplicate turns: kill agentd after ledger-ack
      before admission; kill opencode after admission before promotion;
      suspend pod in each window; API crash mid-forward
- [x] Stranded-admitted recovery fires wake only (I6) — verified by a
      #1119 replay scenario (defect-class death consuming the row)
- [x] Interrupt suite (I7): entries survive interrupt unchanged; no
      `superseded` states exist; queued rows drain after interrupt
- [x] I9 verified: 202-then-agentd-kill-then-resume loses no ledgered
      entry; fsync path measured on target storage (Longhorn/EFS) and
      group-commit adopted if send-path p99 exceeds budget
- [x] State-mapping table (M2) enforced in code: outbox completion
      wired to `admitted` only; `stalled` alerts fire and wake
- [x] US-63.9 `v2Pending` wake and US-63.10 `v2Shadow` retired from
      the API (ledger-derived queue depth live)
- [x] Text-scan oracle demoted to agentd-internal fallback — deleted
      outright if the S1 spike landed exact ID correlation

### S3 — API tracker retirement (cutover; sequenced behind Epic 65's frontend dialect work)

- [x] Frontend consumes the contract stream via the API; reconnect
      suite: mid-turn reconnect, standing question at reconnect
      (answerable from snapshot — I12), API rolling deploy mid-turn
      (snapshot fan-in bounded, measured)
- [x] API-side session-state derivation and the 0054 M3 bridge plan
      deleted; unknown-taxonomy classifier and drift metric removed
- [x] Two API replicas concurrently consuming one pod: delivery stays
      single-owner (I5); no stream interference

### S4 — cleanup & operability

- [x] Dead paths deleted (API-side derive/verify unless fallback
      retained); flip-gate rewritten to the agentd in-flight-count
      procedure — verify retained behind the adapter seam as the
      documented rollback fallback (spike pool runs open)
- [x] Metrics live: `seq_stall_seconds`, `ledger_depth`,
      `promotion_stall`, `snapshot_size_bytes`, `delivery_202_latency`;
      alerts wired (seq stall while pod running; stalled entries;
      wake failures)
- [ ] Resume budget: reseed + ledger replay within the ~22s target at
      p95, including long sessions (V2 store full-list cost measured)
      (pool — harness + runbook committed; evidence rides the delivery
      pool workflow)
- [ ] Rollback drill under load: authority flag off returns to 0052
      paths with no user-visible loss; ledger back-drains (R8)
      (pool — harness + runbook committed; evidence rides the delivery
      pool workflow)
- [x] Ledger outcome-retention and the seq-cursor meta row proven
      uncompactable; compaction test included

---

## Open items

### Decisions owed (block epic filing or early stories)

> **IDL + transport decided 2026-08-30 (US-69.1):** protobuf IDL managed by
> buf; transport = **Connect RPC** (connect-go); `pkg/session` ↔ schema
> source-of-truth agreement recorded with the Epic 65 flip at S2 freeze.
> See [`design/0056_2026-08-30_harness-abi-idl-transport.md`](0056_2026-08-30_harness-abi-idl-transport.md).

> **ALL DECIDED 2026-08-29 (owner): D1-B, D2-C, D3-A, D4, D5-revised.**
> D1: on-demand streams + inline-first admission. D2: frontend cutover
> merges into S3 — only a test env is deployed, so a hard cutover that
> breaks it is acceptable. D3: parts-capable deliveries schema from day
> one; file parts capability-gated to `NotSupported` on opencode until
> Epic 68/upstream; multi-text-part join rule defined now. D4: 0053-S3
> mandatory pins gates S2; S1 shadow parallel. D5: IDL toolchain at S1
> start, schema evolves during shadow, freezes at S2 entrance.

- **D1 — API consumption model:** ✅ **DECIDED: B** — on-demand streams
  + inline-first admission. The deliveries POST admits inline
  (ms-scale, short timeout) so the outbox never needs the stream; the
  stream is purely display, subscribed only while browsers watch a
  workspace. Delivery = POST/poll, display = stream: one mechanism per
  concern. Externally neutral for API/SDK/MCP; sends get faster, reads
  become truthful; the A/B choice stays reversible internally.
- **D2 — Frontend dialect cutover ownership:** ✅ **DECIDED: C** —
  merge the remaining frontend-consumption milestone into 0055 S3.
  Rationale: a hard Epic 65 dependency repeats the 0054 limbo; a
  reverse translation bridge is discarded work. Deployment is test-env
  only — a hard cutover that breaks it is acceptable (no compatibility
  constraint exists).
- **D3 — Attachments in the deliveries op:** ✅ **DECIDED: A** —
  parts-capable deliveries schema from day one (owner preference for
  ABI completeness; Epic 68 unblocked whenever funded). Consequences
  recorded: opencode V2 prompt is text-only on 1.18.x, so the schema
  accepts parts while the **capability report gates file parts to
  `NotSupported`** on the opencode adapter until harness support lands
  — harness limits are data, not code branches. The multi-text-part
  join rule (order-preserving newline join into the V2 `text` field)
  is defined now and contract-tested.
- **D4 — Epic ordering vs 0053:** ✅ **DECIDED:** 0053-S3 (base strip +
  mandatory pins) is a hard prerequisite for 0055-S2 — the delivery
  cutover ships only into a single-regime fleet. 0055-S1 (shadow) is
  *not* gated: it runs in parallel with 0053's remaining stories,
  requiring only an opt-in pinned staging pool (available since
  0053-S1). 0053 is in flight and prioritized.
- **D5 — IDL early-payment:** ✅ **DECIDED (revised):** IDL toolchain
  lands at **S1 start** — the schema has three consumers from day one
  (agentd server, API client, S1 comparator); the S1 endpoints persist
  into S2/S3. The schema **evolves freely during S1** and **freezes at
  S2 entrance** (when the outbox binds — the freeze, not the start, is
  the irreversible commitment). The IDL-vs-`pkg/session`
  source-of-truth decision with Epic 65 happens at S1 start.

### Spikes & measurements (S1-gated)

> **US-69.6 findings recorded 2026-08-30** (harnesses committed;
> in-repo numbers below, pool-bound numbers marked POOL):

- **Caller-supplied admission ID** — probe harness committed:
  `local/spike-admission-id.sh` (baseline / fresh-unique / duplicate-reuse
  matrix per pinned version). POOL: run on the staged pool for every
  pinned opencode version. Disposition rule recorded: fresh-unique accept
  + duplicate 409 ⇒ delete the localhost text-match fallback outright in
  US-69.7; anything else ⇒ the fallback stays and the matrix documents why.
- **fsync-through-gVisor on Longhorn/EFS** — baseline harness committed:
  `BenchmarkCursorFsyncLatency` + `TestSpikeNumbers/fsync_baseline_local`.
  Local (container overlayfs): **~7–10 ms/op**. Decision rule: pool
  numbers ≤10 ms/op ⇒ fsync-per-event stands (I9 as written); >10 ms/op ⇒
  group-commit enters US-69.7's ledger design. POOL: measure under
  gVisor (runsc) on Longhorn AND EFS storage classes.
- **Resume budget** — in-process harness committed:
  `BenchmarkResumeBudget` (reopen + durable-cursor load + boot reseed at
  50 sessions, cursor@500 events): **~0.5 ms** — the authority's share of
  the ~22 s budget is negligible; PVC re-attach + opencode boot dominate
  (unchanged conclusion). POOL: the V2 store full-list cost on a
  ~20k-message session (no pagination on 1.18.15) is measured with the
  same harness shape on the pool; feeds US-69.13 verification.
- **Snapshot size at N sessions** — `BenchmarkSnapshotSize100/500` +
  `TestSpikeNumbers`: 100 sessions × (4 in-flight parts + 2 pending
  inputs) = **33 KB / ~0.8 ms**; 500 sessions = **169 KB / ~4 ms**.
  **Decision: no cap or pagination at S1/S2** — three orders of magnitude
  under any wire concern, single-consumer-per-pod (D1-B). Revisit only if
  a future harness produces ≥10 KB/session snapshots.
- **Reseed under active streaming** — settled in US-69.2/69.5 suites
  (deterministic green; CI starvation finding documented on #1139 for the
  pool soak).
- **Question/permission event semantics** — confirmed from the pinned
  fixtures + existing adapter surface: the events carry request/response
  payload only (id, sessionID, question/options, permission/patterns);
  no control-plane semantics hide in them. Answer routing via the
  `answer_question` action is the complete path (US-69.9); the V1 REST
  reply endpoints the proxy uses today are the same store mutations the
  action will wrap. No divergence found.
- **statusz ↔ snapshot dedupe** — **Decision: keep both, distinct
  charters.** statusz is the controller's deep-introspection poll
  (no-latency-bound; sessions list with tokens/context); the ABI snapshot
  is the I12 contract view (bounded, zero harness calls). Merging them
  would put a no-upper-bound endpoint's semantics into the frozen
  surface. US-69.11 retires the API-side DERIVATION, not statusz; the
  controller's scrape may later source cheaper data from the snapshot,
  decided there.
- **Control-op concurrency matrix** — **Decision: no exceptions.** All
  five action verbs serialize against delivery via the authority's
  single-flight (US-69.9); session-create stays API-side (adapter CRUD)
  per M1's explicit carve-out. The interrupt/admission-in-flight race is
  settled by construction (I7: an admission completing during an
  interrupt lands a preserved queued row).

### Upstream dependencies

- Terminal delivery events with messageID echo (retires the localhost
  fallback); `limit`/`before` pagination on `GET /api/session/:id/message`
  (unblocks S5); `resume` exposure — the wake remains a prompt-nudge
  (transcript-polluting) until then; the weakest mechanism in this
  design.

### Spec paragraphs owed (writing, low risk)

- Ledger on-disk format, versioning, outcome-retention policy.
- Billing/usage sourcing from the contract stream (Epic 33 consumes
  `step.ended` cost/tokens — confirm the projection carries them).
- Action-result ↔ event causal linkage (does an action 200 carry the
  seq of its effect event?).
- Session-create routing exception; projection eviction for idle
  sessions; opencode-bump cadence now that agentd embeds the dialect
  (upstream fixes wait for platform release trains?).
