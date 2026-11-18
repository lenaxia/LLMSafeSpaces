# cmd/workspace-agentd + pkg/agentd supervision stack — complexity audit

**Task:** tsk-superb-hammer-pricks (AUDIT: agentd supervision stack — sessionstate, watchdogs, canaries, delivery FSM)
**Date:** 2026-11-18. **Base:** origin/main `9f74d3020` (0.44.17 release head). **Sibling audits:** `reports/audits/api-app-complexity.md` (tsk-tidal-wolfhound-restricts — the api/internal SEAM over this code, incl. the hub read-edges and the quiet seam), `ci-release-complexity.md`, `emission-defenses-complexity.md`. Same matrix format, same yardstick: *few facts at the seam; complexity lives in one decision plane* (claim-lifecycle-design principle 5).
**Method:** read-only — full-file reads of the supervision/sessionstate/harness-seam families, wiring archaeology in `main.go` / `server.go` / `sidecar_mode.go`, dead-code greps for every suspicious helper (callers listed below), incident cites as the code's own provenance comments carry them. **No code changes** (this document is the only artifact).

**Scope:** the tree is 44,119 non-test LOC (cmd) + 2,131 (pkg/agentd); the three focus areas (supervision layers / state machines / harness seams) concentrate in ~19 kLOC across the four families inventoried in §2. Out of scope beyond seam-level notes: mcp_tools/mcp_server (tool surface), secrets.go store machinery, uploads, workflow engine, board payload schema — those are other audits' lanes.

---

## 1. Executive summary

- **The supervision stack is a reef of small, incident-shaped healers, not a monolith.** Twelve independent liveness/healing loops run on an opencode pod (§3.1 inventory table), each born from a named incident (#892, #1632, #1311, #1732, #2045, 2026-10-08…), each with a narrow kill/heal set and loud suppression counters. The *shape* is defensible — this is defense in depth against genuinely different wedge classes — but the loops derive their world from **three different busy truths and two different health truths that must agree by discipline, not construction** (F1): the SSE tracker's own busy map gates kills (watchdog deferral, session-aware restart) while the sealed sessionstate authority owns the #1574 derived-busy definition that every *projection* surface serves.
- **The #2040 five-derivations lesson: the instance was fixed, the class was fixed at exactly one boundary.** The board turn-end emission is now structurally single-sited (`applyHarnessEventLocked` — the anti-#2040 call-site rule, `sessionstate/board_turn_end.go:13-25`), and #1574 made busy *inside the authority* one definition. But agentd-side, four parallel busy/liveness derivations remain (tracker, notifier mirror-fold, statusz registry floor, healthz cache — §3.2), each with a documented justification and none sharing a read edge. The class fix is documentation-and-ownership, not construction.
- **The delivery ledger/FSM (design 0055 M2) is proportionate and load-bearing** — six delivery states (+ an Unspecified zero member), monotone by construction, every duplicate-storm incident (#1288, #1292b, #1315) closed by a named guard inside it. One real defect: **`deliveryLedger.compact()` has zero production callers** (grep: none outside `ledger.go` + tests) — compaction never runs, so terminal rows and the WAL grow without bound on long-lived pods (F5). Dead code whose absence is an unbounded-growth liability.
- **The harness seams are uneven.** The sessionstate module's 7 injected seams (Parser/Store/Admitter/Actor/Wake/BoardTurnEndProbe/IDTranslator) are the real "third harness" contract — dialect-free, module-sealed, and *would* carry a third harness cleanly. The S5 status seam (`harnessStatusSource`) is a one-method interface with one non-default implementation, and the Appendix-A control-socket vocabulary (Restart/SetSpawnEnv/SpawnEnvState) is opencode-shaped: the pi adapter implements it with five honest no-ops (`pi_supervisor.go:325-366`) — the abstraction leaks pi through opencode's contract (F9). A third harness would also discover that the pi leg *bypasses* sessionstate entirely (aggregator + journal + S4 bridge instead of authority+ledger), so "a third harness" has two divergent precedent stacks to study.
- Top 5 candidates (§8): ledger compaction wiring, the busy-truth read-edge convergence, dead-letter removals (`admissionWatch.suspect()`, the ledger-less lease backstop arm audit), the stringly restart-reason inline forks, and the sidecar's twin control-socket pollers. All documentation-or-refactor class, each with a functional-equivalence argument.

---

## 2. Inventory (audited families, non-test LOC)

### 2.1 Supervision family (cmd/workspace-agentd, 5,435 LOC + sessionstate_metrics 497)

| File | LOC | Role | Origin |
|---|---|---|---|
| `managed_process.go` | 618 | the crash-respawn loop: sole `cmd.Wait()`, backoff, OOM class, restart/stop intent channels | worklog 0125 Bug 2; #904 |
| `supervise_opencode.go` | 642 | sidecar-topology PID-1 subcommand: self-verify, boot layers, adapter (spawn-env pull, crash attribution) | design 0051 US-1; #863, #1573 |
| `healthz.go` + `healthz_cache.go` | 849 | US-22.1 no-I/O healthz; readiness cache + episode/generation CAS + the health-watchdog decision tree | US-22.1; #807, #1632 r1-r3 |
| `watchdog_vitals.go` | 394 | vitals corroboration: dead-listener-only kill set (HUNG/STARVED/FLAT/RESPAWN/UNKNOWN) | #892, design 0050 D1 |
| `supervisor_restarts.go` + `supervisor_status.go` | 322 | #1760 restart log; sidecar's 15s control-socket status mirror | #1760/#1776; design 0057 I10/I13 |
| `turn_resume.go` | 278 | #1732 A2: roll-classified infra-loss → one bounded auto-resume | #1732 |
| `admission_watch.go` | 471 | #2045 prompt-path canary: strike ring → session-aware restart | #2045 (Nov 13-15 2026 incident) |
| `restart_reason.go` | 344 | marker file + reason taxonomy + boot-time read-back | US-44.7; #1801, #2045 |
| `oom_detection.go` / `orphan_reaper.go` / `memory_pressure.go` / `rearm_loop.go` / `relay_liveness.go` | 1,241 | OOM classify; PID-1 zombie reap; cgroup pressure observe; bounded re-arm; relay-path liveness | worklog 371; #1632; US-44.5; #910/US-72.0; US-72.4 |
| `supervise_selfverify.go` | 276 | the fail-closed binary/image self-verify gate both supervisor subcommands run before anything | design 0051 step-2; #863, #1573 |
| `sessionstate_metrics.go` | 497 | the reconcile/stall watchdog loop + metric bridge | US-69.12, #1311 |

### 2.2 sessionstate family (5,542 LOC)

| File | LOC | Role | Origin |
|---|---|---|---|
| `sessionstate/authority.go` | 1,152 | the sealed authority: seq, fold, fanout, replay ring, reseed, metrics | design 0055 Epic 69 US-69.2; 0069; #1291, #1576, #1584 |
| `sessionstate/ledger.go` | 893 | delivery WAL + 7-state FSM + driver (admission ladder, dedupe, promotion) | 0055 M2; #1288, #1292b, #1315, S2 |
| `sessionstate/reconcile.go` | 448 | store-evidence convergence sweep + busy clear (ordered publication) | #1311, #1722 |
| `sessionstate/lease.go` | 386 | pending-input leases; serve-path gather; status rederive backstop | #1310 slice B |
| `sessionstate/projection.go` | 456 | the contract fold; #1574 busy definition; #1576 layer 2 | #1574, #1576, #1578 |
| `sessionstate/{service,actions,replay,board_turn_end,cursor}.go` | 954 | ABI surface + rate limit; typed actions; replay ring; turn-end emission; durable cursor | 0055 I8; US-69.9; 0069; #2061/#2064 |
| `sessionstate_wiring.go` | 1,253 | the opencode seams: StoreReader (3 endpoints), Admitter (V1+model pre-set), Actor (9 verbs), converters | US-69.3/69.7/69.9; #1372 |

### 2.3 State machines beyond sessionstate (4,540 LOC)

| File | LOC | Role | Origin |
|---|---|---|---|
| `session_tracker.go` | 943 | SSE tracker: statuses/busySince/progress stamps; statusz busy legs; watchdog busy source | design 0050 D6; #998, #1685 |
| `session_notifier.go` + `_store` + `_delivery` (+ `_metrics`) | 2,417 (+77) | idle-subscriptions: sibling frame consumer, derived-busy mirror fold, debounce, durable outbox | design 0071; #1722 |
| `board_turn_ledger.go` | 383 | board turn-end emitter facts: claim-holder registry, advance ledger (JSONL tail), epochs | #2061/#2064 story 1 |
| `session_shortids.go` / `session_ref.go` | 797 | short-id replica (advisory, receive-only; 211) + warm worker (586) | design 0066; #1817 |

### 2.4 Harness seams (pi stack 3,195 + harness_driver 199 = 3,394 LOC)

`harness_driver.go` 199 (S5 poller/store/statusz render), plus the eight pi files: `pi_supervisor.go` 551 (S2 process model + health pass), `pi_state.go` 448 (aggregator), `pi_policy.go` 595 (per-session/single), `pi_rpc.go` 559 (JSONL pump), `pi_bridge.go` 431 (S4 transport on :4096), `pi_overlay.go`, `pi_board_turnend.go`, `sidecar_pi.go`. Origins: pi scoping report S2-S5; design 0053 §5.4; #2061.

---

## 3. Deep-dive I: the supervision layers

### 3.1 The loop inventory (acceptance criterion 2)

Twelve healing-class loops on an opencode pod (pi topology swaps #1-adjacent machinery, noted). "Truth" = the state each loop derives its world from.

| # | Loop | Trigger / cadence | Heals | Truth source | Topology |
|---|---|---|---|---|---|
| 1 | `managedProcess.supervise` (`managed_process.go:198`) | event: `cmd.Wait()` return | child death (crash/OOM respawn + backoff) | process table | single + supervisor |
| 2 | health-watchdog (`refreshIsHealthyLoop`, `healthz_cache.go:364`) | 5s ticker | hung/dead-listener opencode (kill set: verdictHung only; UNKNOWN-episode bound escalates) | healthz cache (own /global/health derivation) + tracker busy + vitals | both (sidecar via socket restarter) |
| 3 | sessionstate reconcile watchdog (`runSessionStateWatchdog`, `sessionstate_metrics.go:415`) | 15s (ReconcileCadence) | stranded ledger rows, wedged BUSY views, pending leases, declared-timeout parts; stall→wake(reseed) | harness store evidence (`StoreReader`) | single + sidecar |
| 4 | serve-path lease refresh (`refreshSessionLeaseOnServe`, `lease.go:273`) | per GetSnapshot, TTL-coalesced | stale pending-input sets | live ask registries | both |
| 5 | reseed ladder (`startStateAuthorityReseed`, wiring:361) | boot + generation change, backoff | projection divergence from store | store | both |
| 6 | turn-resume supervisor (`turn_resume.go`) | event-fed (finish "unknown") | infra-lost turns (one bounded re-prompt) | raw wire events | both |
| 7 | admission canary (`admission_watch.go`) | event-fed (POST timeout strikes) | per-session prompt-path wedge → session-aware restart | admission timeouts | both |
| 8 | session-aware restart deferral (`makeSessionAwareRestartDecision`, `secrets.go:288`) | per request, poll loop | restart *timing* (idle window; stall force via interrupt) | **tracker busy map** | both |
| 9 | agent-config watcher (`watchAgentConfigForChanges`) | file watch | late config ingestion (restartWithGrace) | config mtime | both |
| 10 | relay re-arm + liveness (`rearm_loop.go`, `relay_liveness.go`) | 30s + backoff | relay config degrade (bounded re-arm → restart) | relay router probe | single |
| 11 | orphan reaper (`orphan_reaper.go`) | 1s ticker | zombie accumulation (PID-1 duty) | process table | all |
| 12 | kubelet probes (healthz/agentz/readyz) | external | the final backstop (pod kill) | healthz cache snapshots | both |

Observation-class loops riding beside them (no healing authority, listed to bound the count): memory-pressure monitor, ops-metrics 60s, eventbus health ticker, supervisor-status 15s mirror, upload/staging sweepers, short-id warm worker, fillGaps backfill, notifier Run + facts ticker, pi-bridge SSE heartbeat. **Verdict: 12 healing loops is the honest count; none is a duplicate of another's wedge class** — every one traces to a distinct incident, and the kill sets are deliberately narrow (#892's dead-listener-only ruling is enforced in #2, #7's fire rides #8's decision, pi's D6 "escalate, never execute" is enforced in the pi twins). The cost is not redundancy of *purpose* but of *world-model*: see 3.2.

### 3.2 Overlap and the one-truth question (the #2040 lesson)

**Busy/liveness derivations alive agentd-side** (grep-verified):

1. **sessionstate projection** — `deriveBusyComponents`/`enrichBusyLocked` (`projection.go:331-377`): the #1574 single definition (streaming ‖ active parts ‖ queue), served to State(), snapshots, and the ABI. The authority.
2. **sessionStatusTracker** — own `statuses`/`busySince` maps folded from raw SSE (`session_tracker.go:67-130`). Feeds: the watchdog's busy-deferral (`busySessionChecker`), `makeSessionAwareRestartDecision` (loop #8), statusz busy ages, the #1685 progress stamps, and the session_hung evidence leg API-side. Pre-dates the authority; never converged.
3. **notifier mirror fold** — `notifierSession.derivedBusy()` (`session_notifier.go:424`) re-folds the authority's *output frames* by the same #1574 rule. Documented as deliberate (§2.2 header: a sibling consumer, "NOT a hook inside the sealed sessionstate module") — an accepted re-derivation with a stated reason.
4. **providerCache registry floor** — statusz composes busy from the cached live-turn registry + tracker (`session_tracker.go:41-93`), a fourth composition of "busy".
5. **healthz cache** — `Healthy` from /global/health probes (a different question: serving vs turning) — legitimately separate.
6. **pi aggregator** — `piStateAggregator` (`pi_state.go`) is the pi topology's parity-shaped derivation (no authority exists there).

**Verdicts.** (a) The *emission* side of the #2040 class is fixed structurally: board turn-ends can only be born at `applyHarnessEventLocked` (call-site rule, `board_turn_end.go:13-25`) — a class fix at that boundary. (b) The *busy read* side is fixed by instance: #1574 made one definition *inside* the authority and every authority surface serves it — but the two decision planes that gate **kills** (watchdog deferral, restart deferral) still read the tracker's independent derivation, and statusz reads a fourth composition. They agree today by discipline (three test suites + the #1312 ownership table: harness store is ground truth, projection is the derived view, tracker is the SSE fast path). This is the exact shape the sibling audit found one level up (its F1) — nested one level down here. (c) The health question is cleanly separated (serving-health vs turn-busy vs relay-path are different questions with different clocks; no consolidation warranted).

Not overlap, verified: the three busy-*clearing* paths inside sessionstate (event fold, evidence sweep, lease-window rederive) are an explicitly layered authoritative+backstop design (`lease.go:26-30` states the precedence); the two canaries (#6, #7) share one `pkgAdmissionWatch` instance; the restart *requesters* fan in to exactly one execution seam per topology (managedProcess / control-socket Restart).

### 3.3 The restart taxonomy

Four restart-reason classes exist as consts (`restart_reason.go:47-97`) plus three inline string values at four sites — "crash" (`managed_process.go:292,297`), "oom" (`oom_detection.go:94`), "credential_reload" (`xdg_config_layer.go:438`) — and the socket's closed enum (`control_socket.go`). Eight marker-write sites across seven files follow the documented every-class-writes-its-own-marker pattern. Minor fork: the three inline literals are the same taxonomy expressed three ways (F6).

---

## 4. Deep-dive II: the state machines

### 4.1 The delivery ledger/FSM (proportionate, with one dead limb)

Six delivery states (+ the `Unspecified` zero member): `ledgered → admitted → promoted → turn_ended`, plus `stalled` (admitted past deadline, wake-only recovery) and `failed` (per-attempt terminal, re-armable at attempt+1). Monotonicity is enforced by from-set maps (`ledger.go:283-284`); every duplicate-storm incident is closed *inside* the machine: cross-attempt dedupe (`admittedAnywhere`, #1288's five-identical-turns), evidence-based admission (#1315's FAILED-compatible-with-write), keyed upsert at the harness (S2). The surface (893 LOC) mixes four concerns — WAL persistence, the FSM, queue/depth queries, and the delivery driver — which is one file more than the concept count, but the seams are clean (driver joins the authority's session single-flight; the ledger never calls out). **The dead limb: `compact()` (`ledger.go:574-629`) has zero production callers** — the WAL and the in-memory `rows` map retain every terminal row forever. `InFlightDeliveries` scans only the three live states so correctness holds, but a long-lived pod's ledger.wal and rows map grow monotonically with delivery volume. The code is complete, tested, and unwired (F5).

### 4.2 The busy-view authority (the real state machine — surface vs behaviors)

`sessionRecord` carries 9 mutating fields whose interactions ARE the wedge history: `status/busy` (event truth), `busySince` (lease window), `lastBusySeq` (evidence-freshness gate), `pendingSince/resolvedSeq` (lease staleness gates), `toolDeadlines` (#1576 layer 2), `foldedDead` (the fold-dead hold, lifted only at the harness boundary). Write paths: the event fold, the evidence sweep (ordered publication via `applyLocked` — #1722), the lease rederive, the declared-timeout fold, the reseed. Each guard pair (seq-gates, LiveBusy settle gate, terminal veto #1578, orphan sweep #1342) traces to a named incident and is pinned. **Verdict: the surface is wide but the behaviors are distinct and individually earned — this is the best-documented state machine in the repo.** The one collapsible representation: `pendingSince`/`resolvedSeq` are two half-maps of the same "ask last-fold seq" fact (merge candidate, low value).

### 4.3 The session index surfaces (four pod-local registries, four storage idioms)

(a) `sessionShortIDStore` — atomic-rename JSON, no fsync, advisory, receive-only (0066). (b) `sessionSubscriptionStore` — durable file + latch + outbox watermarks (0071). (c) `boardWorkerSessions` + (d) `boardAdvanceLedger` — append-only JSONL tails with count-bounded compaction on the shared volume (#2061). Each idiom is individually justified (advisory vs durable vs cross-process); the cost is that "where does pod-local registry state live and how does it survive" has four answers. Tolerable — but the *fifth* registry (the ledger WAL) makes it five; a note for the next one, not a refactor.

### 4.4 healthz-cache episode machine

The generation-epoch CAS (`noteAgentGeneration`/`storeResolved`, `healthz_cache.go:181-212`) is subtle but earns it: the r3 reviewer's empirically-demonstrated lost-update race (a boundary landing mid-poll reinstating a stale episode clock). Proportionate.

---

## 5. Deep-dive III: the harness seams

- **The sessionstate seam set is the real third-harness contract.** Seven injected seams (`Config` fields, `authority.go:111-180`), module-sealed, dialect-free. `sessionstate_wiring.go` is the complete opencode tenant (~1.25 kLOC). A third HTTP+SSE harness would implement the seven seams and reuse the authority, ledger, reconcile, and replay machinery wholesale. **This abstraction pulls its weight** — with one caveat: it currently has ONE tenant. The pi leg does not implement it (no Admitter/StoreReader/Parser for pi): pi pods run the aggregator + S4 bridge + turn-end journal instead (`pi_supervisor.go`, `pi_bridge.go`, `pi_board_turnend.go`). So the "would a third harness be easy?" answer splits: *yes* if it is opencode-shaped (seams are waiting), *study two divergent precedents* if it is pi-shaped (two parallel stacks for board turn-end emission alone — the opencode authority path and the pi observer path, F10).
- **S5 (`harness_driver.go`) is thin and honest but barely an abstraction.** One method (`HarnessStatus`), one non-default implementation (pi); the opencode leg is the inline pre-S5 path selected by env-branch (`startBackgroundLoops`'s early return, `server.go:817-820`). That was the recorded mixed-fleet-compatible choice (absent env = byte-identical opencode). Cost: harness behavior forks live as if-branches in shared files (`sidecar_pi.go`, `server.go`) rather than behind the interface — a third harness adds another branch, not another implementation.
- **The Appendix-A control-socket contract leaks opencode.** `piSupervisorProc` (`pi_supervisor.go:325-366`) implements Restart→(false,false), SetSpawnEnv→swallowed, SpawnEnvState→zero, RefreshFiles→("","") — five honest no-ops so the *sidecar's* opencode-shaped poller keeps its contract on a pi pod. This is the seam's leak made concrete: the supervisor contract encodes single-child + spawn-env-push semantics that only opencode has. Deliberate (mixed-fleet honesty), documented, and cheap — but it is the first thing a third harness would trip on.
- **The legacy `SetSpawnEnv` push path is demolition-pending**, marked in-place ("superseded by the spawn-time pull; demolition lands with US-70.5", `supervise_opencode.go:429-434`). Alive-by-design; tracked, not forgotten.

---

## 6. Where loops/derivations re-derive known state (consolidation verdicts)

| # | Surface | What is re-derived | The owner already has | Verdict |
|---|---|---|---|---|
| R1 | tracker busy map (`session_tracker.go:67`) feeding loops #2/#8 | turn-busy per session, second derivation | sessionstate `SessionView.Busy` (#1574) | **Consolidate via read-edge** (F1) — needs a BusyOrUnknown + progress-clock read from the authority; the #1576 steer forbids widening the *decision* seam, not adding an authority read |
| R2 | notifier mirror fold (`session_notifier.go:424`) | derived busy, third derivation, from frames | the authority it subscribes to | **Keep** — the sealed-module ruling is explicit; the fold is the documented price of seal integrity |
| R3 | statusz busy floor (tracker + registry composition) | busy composition #4 | authority State() | **Consolidate opportunistically** when statusz next changes (P today) |
| R4 | pi aggregator vs sessionstate | the whole projection, pi topology | — | **Keep** — different harness, no authority exists; note for a future pi tenant of sessionstate |
| R5 | sidecar twin control-socket polls (harness_status 5s + supervisor status 15s) | supervisor state, two loops two stores | one socket | **Collapsible** (one poller, two stores) — small value, zero risk (F11) |
| R6 | `rederiveStatusLocked` idle/ERROR arm (lease.go:207-229) | busy-clear #3 (backstop) | the evidence sweep (authoritative in ledger-wired topology — always in production) | **Dead-in-production, alive-for-test-topologies** — keep with its comment (F7) |

---

## 7. Findings matrix

*Class: **P** = proportionate, **C** = collapsible, **D** = dead / dead-in-fleet.*

| ID | Finding | Evidence | Cost | Class | Recommendation | Risk |
|----|---------|----------|------|-------|----------------|------|
| F1 | Kill-gating decisions read the tracker's second busy derivation while the authority owns the #1574 definition | `session_tracker.go:67-130` → `healthz_cache.go:433`, `secrets.go:288+`; vs `projection.go:331` | the last read-side #2040-class divergence: watchdog may defer on tracker-busy while reconcile clears authority-busy (bounded by max-defer + UNKNOWN bounds, but by arithmetic, not shared truth) | C | add an authority read-edge (BusyOrUnknown, per-session progress clock); flip the two consumers; tracker demotes to SSE→Ingest shim + statusz legacy (R1) | med — boot-window semantics (tracker answers pre-reseed) and the #1576 steer scope it to a read-edge, not a seam change |
| F2 | 12 healing loops, 3 busy truths, 2 health truths — every one incident-earned; overlaps are world-model overlaps, not purpose duplicates | §3.1 table | cognitive/orientation cost per incident response; the inventory itself was not written down anywhere until this audit | P + doc | commit §3.1 as the loop map; cross-link from each loop's header comment | ~zero |
| F3 | Delivery FSM is sound; driver+ledger+queries in one 893-LOC file | `ledger.go` | navigational only; seams clean | P | split only next-touch (driver to its own file) | — |
| F4 | Notifier re-folds derived busy from frames (sealed-module price) | `session_notifier.go:424`, header :22-30 | a fold that must track #1574's rule by discipline | P | keep; add the one-comment cross-link to `deriveBusyComponents` | ~zero |
| F5 | `deliveryLedger.compact()` has zero production callers — WAL + rows grow unboundedly; terminal rows retained forever | grep: no non-test callers; `ledger.go:574-629` | unbounded file/memory growth on long-lived pods; the retention policy exists on paper only | **D (dead-in-fleet)** | wire it: call on the reconcile-watchdog cadence with a retention cutoff (e.g. 24h) — the method is complete and tested | low — pinned by ledger tests; compaction preserves in-retention rows by construction |
| F6 | Restart-reason taxonomy expressed three ways: 4 consts + 3 inline string values at 4 sites + socket closed enum | `managed_process.go:292,297` ("crash"); `oom_detection.go:94` ("oom"); `xdg_config_layer.go:438` ("credential_reload"); `restart_reason.go:47-97`; `control_socket.go`; 8 marker-write sites / 7 files | drift surface between marker/metric/enum | C | hoist "crash"/"oom"/"credential_reload" to consts used by all sites | ~zero |
| F7 | Lease-window idle/ERROR arm is dead in production (ledger always wired) | `newStateAuthority` always sets Admitter; `lease.go:194-229` | ~20 LOC + a third busy-clear to reason about | D (dead-in-production) | keep as the documented ledger-less backstop; add the caller-note | ~zero |
| F8 | `admissionWatch.suspect()` exported-ish helper: no production callers (strikeCount/suspectSessionsLocked carry the load) | grep: none | 17 LOC dead (`admission_watch.go:282-298`) | D | remove with its pins | ~zero |
| F9 | Appendix-A control-socket vocabulary is opencode-shaped; pi implements it with five honest no-ops | `pi_supervisor.go:325-366` | the seam leaks: a third harness repeats the no-op ceremony; the contract conflates "supervised process" with "opencode child + spawn-env push" | C (contract, doc-first) | document the v2 supervisor contract (restart semantics OPTIONAL, capability-typed like `harness_status`); no code change warranted now | ~zero for the doc; med if re-contracted |
| F10 | Two parallel #2061 board-turn-end emitter stacks (authority probe path; pi observer + journal path) | `board_turn_end.go` vs `pi_board_turnend.go` | the emission-site rule is honored at both attested folds; epochs/ledgers are two implementations | P (documented trade) | keep — pi has no authority; note that a future pi sessionstate tenant collapses these | — |
| F11 | Sidecar runs twin control-socket polls (harness_status 5s, supervisor status 15s) into two stores | `sidecar_pi.go:80`, `supervisor_status.go:140` | one redundant socket round trip per 5-15s; two freshness idioms | C | merge loops next-touch (one poller writing both stores) | low |
| F12 | Session-index registry storage has four idioms (§4.3) | §4.3 | vocabulary tracking | P + note | a paragraph in the next design doc that adds a fifth | ~zero |

---

## 8. Top 5 simplification candidates (ranked value/risk)

*No code changes in this audit — proposals for follow-up stories, each with its functional-equivalence argument.*

### #1 — Wire ledger compaction (F5)
- **Value:** converts an unbounded-growth liability (every terminal delivery row retained in WAL + memory for the pod's lifetime) into the designed bounded state; the code is already written and tested — this is a wiring gap, not a build.
- **Functional equivalence:** delivery outcomes unchanged — compaction drops only `turn_ended`/`failed` rows older than the retention cutoff, preserves the format header and in-retention rows, rewrites via tmp+rename under the ledger mutex (`ledger.go:574-629`). `admittedAnywhere` dedupe keys on live/outcome states that are never compacted while re-armable; the outbox's cross-attempt dedupe window (retry ladder ≤ 80s) is orders below any sane retention. Pinned by the existing ledger test suite.
- **Risk:** low — one call site on `runSessionStateWatchdog`'s cadence with a failure log; no schema change.

### #2 — Authority busy read-edge for the kill-gating consumers (F1 / R1)
- **Value:** closes the last read-side #2040-class divergence: the watchdog's busy-deferral and the session-aware restart decision would read the same derived truth the projections serve, instead of the tracker's parallel map. Divergence today is bounded (max-defer 60 polls, UNKNOWN-episode 15m) by arithmetic stacked on top of two truths — this removes the need for the arithmetic to matter.
- **Functional equivalence:** the authority gains one read (BusyOrUnknown with the tracker's conservative unknown-as-busy boot-window semantics — absent/uninitialized answers busy, matching `trackerHasBusyOrUnknown`; plus the per-session progress clock the stall-force path needs, which the authority can expose from `lastSeqAt`-per-session stamps it already tracks). The two consumers' decision trees, thresholds, and markers are untouched; the tracker keeps its remaining consumers (statusz, session_hung evidence) until R3. Pinned by the watchdog-deferral and restart-decision suites, re-pointed at the authority fixture.
- **Risk:** medium — the boot window (tracker live before first reseed) must answer conservatively, and the #1576 orchestrator steer explicitly refused *seam widening*; this is scoped as an authority read-edge + consumer flip, which the steer permits. Sequenced after R3's statusz leg to land one consumer at a time.

### #3 — Dead-letter removal batch (F7, F8, F6)
- **Value:** −~40 LOC of dead/unreachable paths and one taxonomy fork: `admissionWatch.suspect()` (no callers), the lease-window backstop arm's caller-note (or removal if the ledger-less topology is retired), and the three inline reason strings ("crash", "oom", "credential_reload") hoisted to the restart-reason consts.
- **Functional equivalence:** `suspect()` deletion removes an uncalled method (its pins move or die with it); the consts hoist is a rename at four sites with the marker/metric bytes unchanged (the strings are identical).
- **Risk:** ~zero.

### #4 — Sidecar poller merge (F11 / R5)
- **Value:** one control-socket loop instead of two on pi sidecars (fewer round trips, one freshness idiom); also the natural place to document R5's contract (optional restart semantics) while touching the socket's consumers.
- **Functional equivalence:** both stores keep their exact shapes and staleness contracts; the merged loop performs the same two fetches on the faster cadence (or per-store cadences with one ticker floor); failure semantics per store unchanged (failed poll keeps last snapshot).
- **Risk:** low.

### #5 — Commit the loop map + derivation map as living docs (F2, R2-R4)
- **Value:** the supervision inventory (§3.1) and the derivation verdict table (§6) become reviewable artifacts in-repo beside the code they describe — the orientation cost of twelve loops is paid once per reader today, once per repo after; future incident response starts from a map instead of archaeology. This is the same class of payoff the sibling audit's §3 delivered for the API seam.
- **Functional equivalence:** documentation only; no behavior, wire, or schema change.
- **Risk:** ~zero — staleness is the only maintenance cost, and each loop's header comment cross-links back.

**Honorable mentions:** F3's driver/ledger file split next-touch; F12's registry-idiom paragraph; R3's statusz busy-floor convergence riding any statusz change; the US-70.5 `SetSpawnEnv` demolition already scheduled.

---

## 9. Explicit non-goals

No code changes were made — this document is the audit's only artifact. No opinion is expressed on pkg/agentd's secrets/types beyond their role as shared contracts (2.1 kLOC mostly out of lane), on the MCP tool surface, or on the workflow engine. The §6 verdicts are consolidation assessments, not owner rulings: candidate #2 touches the #1576 steer's territory and needs its own story with that steer cited. The pi stack's internal quality (policy/RPC/pump) was audited at the seam level only — a pi-internal audit is a separate lane.
