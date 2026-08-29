# Worklog: agentd session state authority — design 0055

**Date:** 2026-08-29
**Session:** Architecture investigation → design doc. User questioned why the V2 migration is painful, whether the outbox/promotion-oracle machinery compensates for wrong-domain placement, and why agentd cannot act as the pod-local "human proxy" (state authority + snapshot/stream service). Investigated the opencode TUI architecture, the full worklog/design history (Epics 15/18/21/22/28/41/44/63/65, designs 0049–0054), and wrote `design/0055_2026-08-29_agentd-session-state-authority.md`.
**Status:** Complete (design; no code)

---

## Objective

Determine (a) why the platform cannot reach TUI-level integration with opencode, (b) whether the API-side state machinery (outbox promotion oracle, SSE tracker, event bridge) is compensating for wrong-failure-domain placement, (c) why agentd was never made the session state authority, and (d) capture the agentd-as-state-authority architecture as a design doc if warranted.

---

## Work Completed

### Investigation findings (opencode side, local checkout @v1.18.10)

- The TUI runs the full core in a worker thread; default mode dispatches the SDK's fetch into `Server.Default().app.fetch()` in-memory (`packages/opencode/src/cli/tui/worker.ts:42`); `--port` mode attaches the same server to a socket. Events arrive via GlobalBus→RPC forwarding or the same global SSE. The TUI is an ordinary SDK client — same routes, same events as the platform uses.
- The TUI's control comes from deployment topology, not transport: same-release generated SDK (no version skew), same process (no transport ambiguity), human verifier (no delivery-proof requirement).
- The TUI uses the same admit-and-schedule V2 prompt route (`/api/session/{sessionID}/prompt`) and derives all UI state from `session.next.*` events in one reducer (`packages/tui/src/context/data.tsx:124`).

### Investigation findings (platform side)

- Why the current architecture exists (evidence-cited in the design doc): agentd was never in the traffic path (proxy routes directly to `podIP:4096`, `proxy.go:482-519`); Epic 18 S18.3 designed route-through-agentd but the epic stalled at `Status: Planning`; 0050 D3's axiom ("a networked API cannot share fate") was over-applied from the accept path to the observation path; 0818's earned principle (stored derived state fails when the writer misses events) was read as "no pod-side state" rather than "no non-replayable state"; and the version-coupling precondition (agentd+opencode pinned together) only landed 2026-08-28 with 0053.
- agentd already has half the primitive: `sessionStatusTracker` (commit `01f3997c`, 2026-05-29) subscribes to opencode SSE but serves only statusz and session-aware restart.
- Verdict recorded in design 0055: the proposal was never rejected on merits; it was structurally unreachable until 0053, and the two genuine historical objections (in-pod starvation, dual authority) are answerable — M3 and the authority-boundary table answer them explicitly.

### Design 0055 written

`design/0055_2026-08-29_agentd-session-state-authority.md` — agentd as pod-local session state authority ("headless TUI"): seq-numbered PVC-backed event log with Epic-65 contract projection, snapshot-then-stream endpoints on AgentdPort 4097, entryID-keyed idempotent delivery ledger (retiring the text-matching oracle to a localhost fallback), authority boundary table (API keeps durable accept, cross-workspace events, all business logic), starvation answers, mode-coherence/flip procedure, rejected alternatives (status quo, per-session processes, V2-queue trust, bridge-only), corner-case matrix, validated assumption table, 4-story rollout.

---

## Key Decisions

- **agentd owns session-scoped state projection + in-pod delivery truth; the API keeps durable accept (Valkey outbox), cross-workspace events, and all business logic.** One authority per fact; the outward agentd surface is frozen at four operations.
- **The API outbox is not replaced.** Suspended/unreachable workspaces force an API-side durable buffer regardless; only the delivery terminus and verification source move.
- **Not pursued: per-session opencode processes.** opencode's isolation unit is the directory, not the session; N processes yield N identical views at N× cost with no ownership semantics.
- **Design cites Epic 18 S18.3 as prior art** and answers the starvation (Epic 21/22/0050) and dual-authority (0054 I5/G1) objections in-document, per the adversarial-review discipline.
- **Placement: agentd, as a module-sealed subsystem with a reversible process boundary.** The dispositive reason is the generation signal (agentd is opencode's parent; `cmd.Wait()`/`onChildStarted` are the authoritative boundary — A9). Alternatives (sessiond sidecar, out-of-pod gateway, upstream ownership, client-side derivation) rejected in-document. The supervisor/observer-coupling objection is answered structurally: `cmd/workspace-agentd/sessionstate/`, recover walls, sole owner of the 4097 listener; the frozen 4-op surface makes moving to a dedicated container a zero-consumer-change operation if ever needed.
- **Stress-test round 1 (user-requested) folded in — replay deleted.** Ring/replay machinery found unnecessary (it fixed consumer-miss windows, not ingestion-miss windows) plus two BLOCKERs: (1) ingestion-window loss — projection rebuildable from opencode's store, reseed on every agentd boot/generation change; (2) #1119 stranding — ledger tracks `ledgered → admitted → promoted → turn-ended`. Replaced with stamped-snapshot-on-connect + ordered events + client discard-≤S (the TUI's proven pattern), per-session single-flight, caller-supplied-admission-ID spike, ledger-only PVC persistence.
- **Stress-test round 2 (user-requested) — all findings mitigated in-spec, none architectural.** B1 snapshot/attach gap → mandatory subscribe-before-snapshot connection ordering (I2); B2 missing state mapping → M2 state-mapping table with `(entryID, attempt)` dedupe scope and wake-only stranded recovery (I5/I6/I10); M1 wrong interrupt semantics → `superseded-by-interrupt` deleted, interrupt purity invariant (I7); M2 queue-depth source → ledger-derived, `v2Shadow`/`v2Pending` retire to agentd; M3 reseed race → ordered reseed procedure with synthetic `projection.reseeded` event (I3); M4 → flag/capability matrix. Minors folded: snapshot completeness incl. pending questions (I12), 4097 auth + rate limits (I8), fsync group-commit note (I9), Custom valve, cursor carve-out. Integrity unchanged: 4-op surface, no replay, single authority, placement, reversibility all hold.
- **Populated R/I/AC per user request:** 9 product requirements (R1–R9, with R2 restated honestly as at-most-once-per-attempt + no *silent* loss), 12 testable invariants (I1–I12) each with a named enforcement point, and per-story acceptance criteria (S1–S4) with fault-injection, crash-injection, and rollback-drill gates replacing prose exit criteria.
- **Round 3 (user-prompted): typed actions + harness ABI.** User observed 0055 creates the harness-ABI placement 0049 lacked (pi/claude-code pod-side adapters; claude-code's process-driven model fits agentd natively). Amendments: (1) op 5 — `POST .../actions` typed union (interrupt/switch_model/switch_agent/answer_question/compact) with capability negotiation; closes the round-2 control-op dual-writer gap by making agentd the sole writer of session mutations; (2) "Harness ABI" section — proto-shaped JSON now, proto/Connect IDL at the second-harness trigger (Rule 12) or S2 by choice; Epic 65 co-owns the type source-of-truth; history op defined in the ABI, opencode implementation stays deferred to S5 (upstream pagination); capabilities are data, never API-side branches; (3) surface count updated 4→5 across R6/R7/M1/AC. gRPC verdict recorded: the value is the IDL, not the wire; transport decided at IDL time.
- **Round 4 (user-prompted): D-decision review.** D1 confirmed externally neutral (no API/SDK/MCP contract changes; inline-first admission improves send latency; on-demand mirrors today's proxy attach behavior — reversible internal choice). D4 context restated (0053 = overlay delivery; S3 mandatory-pins is what yields single-regime). **D5 revised after owner challenge**: original "IDL at S2" rested on a weak premise (S1 endpoints persist — only the comparator is disposable; schema has 3 consumers from day one). New recommendation: IDL toolchain at **S1 start**, schema evolves during shadow, **freezes at S2 entrance**. Epic 65 source-of-truth coordination moved to S1 start. Package now D1-B / D2-C / D3-B / D4-C / D5-revised, recorded pending confirmation.
- **Round 5 (owner decision): D4 decided.** 0053-S3 (mandatory pins) is a hard prerequisite for 0055-S2 (single-regime fleet for the delivery cutover); 0055-S1 shadow is NOT gated and runs in parallel with 0053's remaining stories (needs only an opt-in pinned staging pool, shipped in 0053-S1). 0053 is in flight and prioritized per owner. Updated: decision header, D4 item, M4 flip order (capability → S1 shadow ∥ 0053 → 0053-S3 → V2 flag → authority flag), S2 gate note, unpinned-row demoted to boot-time guard. Remaining pending: D1-B, D2-C, D3-B, D5-revised.
- **Round 6 (owner decisions): ALL D-ITEMS DECIDED — D1-B, D2-C, D3-A, D4, D5-revised.** D1: on-demand streams + inline-first admission. D2: frontend cutover merges into S3; rationale corrected — a test env IS deployed but carries no compatibility burden (hard cutover acceptable). D3: owner chose **A over the recommendation** — parts-capable deliveries schema from day one; consequences recorded (capability report gates file parts to `NotSupported` on opencode until Epic 68/upstream; multi-text-part join rule defined now). D5: IDL toolchain at S1 start, schema evolves during shadow, freezes at S2 entrance; transport decided with the toolchain. Design updated: decision header, all five D-items, Harness ABI IDL bullet (supersedes "second harness" Rule 12 framing), deliveries-op surface comment, S1 AC (IDL + parts-schema items added; proto-shaped-audit item superseded).

---

## Blockers

None. Decision to proceed to S1 (or shelve) is the user's.

---

## Tests Run

None — design session. All load-bearing claims validated by inspection and cited in the design's assumption table (A1–A8): tracker existence, port reservations (`pkg/agentd/types.go:76-79`), `pkg/session` contract, PVC subPath machinery (`pod_builder.go:785`), 0053 pin coupling, proxy IP re-resolution, suspend/resume volume semantics.

- **Round 7 (owner-requested): test plans everywhere.** Every Epic 69 story issue (#1135–#1148) now carries a named Test plan section (unit/golden/property/fuzz/fault-injection/envtest/e2e/soak/benchmark per test); epic #1134 gained a cross-cutting test strategy (invariants-are-test-names, regression-first replay tests for the 2026-08-15/#1119/#755/0818 classes, harnesses-as-deliverables, `-race` suite, suites-gate-stories); design 0055 gained §Test plan — the consolidated suite→invariant→gate inventory. Fixed a self-inflicted edit that briefly dropped the §Non-goals header (restored; count wording aligned to five ops).

---

## Files Modified

- `design/0055_2026-08-29_agentd-session-state-authority.md` (new; status Accepted)
- `worklogs/0858_2026-08-29_agentd-state-authority-design.md` (new, this file)

## Issue filing

- Epic 69: #1134
- US-69.1–.14: #1135–#1148 (S1 shadow ungated, parallel with 0053; S2 gated on 0053-S3 per D4; S3 includes the frontend hard cutover per D2; US-69.14 upstream-blocked)

## Next Steps

1. Start US-69.1 (#1135, IDL) and US-69.2 (#1136, sessionstate module) — both ungated; run alongside 0053's remaining stories.
2. US-69.6 (#1140) spikes parallelizable immediately; the admission-ID outcome decides the oracle's fate in US-69.8.
3. File the three upstream asks (messageID-echo receipts; V2 history pagination — unblocks US-69.14; resume exposure) if not already filed under 0054's ask.
