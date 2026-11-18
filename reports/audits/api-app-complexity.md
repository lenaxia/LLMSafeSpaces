# api/internal/app + handlers board/observability surfaces — complexity audit

**Task:** tsk-tidal-wolfhound-restricts (AUDIT: api/internal/app + handlers' board/observability surfaces — inventory, cost, projection re-derivation, top-5 simplification candidates)
**Date:** 2026-11-18. **Base:** origin/main `9f74d3020` (0.44.17 release head; identical to the pre-release tree on `bd56d8464` for every audited path — verified by empty `git diff bd56d8464..HEAD -- api/internal pkg/board`).
**Method:** read-only — full-file reads of the audited surfaces, wiring-call archaeology in `app.go`, design-doc grounding (`reports/specs/claim-lifecycle-design.md`, `reports/specs/seamless-lease-design.md` via its in-code cites, designs 0055/0067/0072), incident cites as they appear in the code's own provenance comments. **No code changes** (this document is the only artifact). Sibling audit: `reports/audits/ci-release-complexity.md` (tsk-hazel-mint-waits, PR #2094) — its §4 owns the AI-review-lane facts; not re-derived here.

---

## 1. Executive summary

- The audited surface is **~3.3 kLOC of board adapters in `api/internal/app`** (13 `board_*.go` files beside the 2,676-line `app.go`) plus **~2.7 kLOC of alert/outbox/terminus in `api/internal/handlers`** (`proxy_lifecycle.go` 739, `board.go` 917, `proxy_worker_silent.go` 355, `outbox_terminus.go` 349, `board_sse.go` 302). Against the "poor facts, rich semantics" yardstick (`claim-lifecycle-design.md` principle 5: *few facts at the seam; complexity lives in one decision plane*) the **engine seam is in good shape** — the hub answers four poor questions, the REST board adapter is genuinely thin ("no business rules live here", `handlers/board.go:15`), and the #2047 proxy seam is dependency-narrow by construction.
- The complexity that IS here concentrates in **three re-derivations of already-owned semantics**: (a) the hub's three public read paths (`ObserveAssignees` / `BusyAssignees` / `AssigneeQuiet`) each re-compose the same legs — the max-of-two-identity-keys stamp idiom appears 5×, the derived-busy distrust guard 3× verbatim (F1); (b) `quietBoardLookup` re-encodes in raw SQL the claim/lease/project-scope semantics the board engine already owns (`board_quiet_lookup.go:55-84`, F2) — the exact "rich facts, poor semantics" inversion: every task fact is in the database, and the seam copies the engine's predicates instead of asking the decision plane; (c) the two alert arms (`session_hung`, `worker_silent`) carry parallel cooldown/publish/resolve machinery — 4+6 near-twin methods across two maps+mutexes (F3).
- `app.go`'s composition root is **one `New()` of ~1,894 lines (185–2078) with 128 `Set*(` injection call sites over 81 distinct setter names**. The `SetFrameObserver`-style one-function seam is the repo's **best** wiring pattern (narrow, late-bound, nil-inert — used consistently for the board's five seams); the file's cost is purely that everything is inline. The board block alone (`app.go:1600-1922`) is a coherent sub-root of ~320 lines (F5).
- One **dead-in-fleet** leg found, alive-by-design: the outbox deliverer's V2 promotion-wait branch (`proxy_lifecycle.go:328-378`) rides the opencode V2 prompt route deleted on the pinned harness (#755) — kept for design 0055 M4's flag matrix, but the legal `v2Delivery && !terminus` combination currently points a 30 s promotion wait at a route that 404s deterministically, with only a comment to say so (F4).
- Top 5 candidates (§8) are all **documentation-or-refactor class, no behavior change argued**: hub read-edge consolidation, engine-owned quiet facts, board-block extraction from `New()`, cooldown unification, and a V2-dead-route runtime warn.

---

## 2. Inventory (what was audited, and its role)

### 2.1 `api/internal/app` — board adapter files (non-test LOC)

| File | LOC | Role | Origin |
|---|---|---|---|
| `board_bus.go` | 746 | design 0072 S6 bus publisher + per-replica SSE join; bus-first with verbatim v1 fallback | S6, #1699 gap 4 |
| `board_liveness.go` | 615 | the seamless-lease adapter: hub (two-class frame index, nudge marks, cursors), `ObserveAssignees`, nudger | seamless-lease 2026-10-27; #2040/#2042/#2047 amendments |
| `board_worker_comms.go` | 417 | `boardV1Sender` (phrase-resolving sender), `deliverBoardTextV1`, aborter/messenger/redispatcher constructors, directive texts | #1747, #1959c, #2046 |
| `board_spawner.go` | 311 | claim-then-spawn worker factory (agentd node-executor class) | design 0067 §4.4, #1966 |
| `board_role_directory.go` | 226 | role-only personas: config validation + spawn-time canonicalization | owner ruling 2026-10-29 |
| `board_due_mark.go` | 216 | turn-end(no-advance) consumer → `Service.ConsumeTurnEnd`; epoch watermark; zero liveness authority | #2061/#2065 story 2 |
| `board_lease_events.go` | 167 | first eventbus producer: ladder decisions → workspace stream | design 0072 S4, 2026-10-30 incident |
| `board_worker_retention.go` | 158 | worker-session hard cap prune (authorized delete) | #1966/#1882 |
| `board_quiet_source.go` | 130 | `AssigneeQuiet` + `QuietFor` — quiet/busy-seconds read edge | #2047 |
| `board_quiet_lookup.go` | 107 | `QuietBoardLookup` impl: in_progress task facts + quiet join | #2047 R4 |
| `board_busy_source.go` | 104 | `BusyAssignees` — badge busy read edge (pure) | RC-C S2 |
| `board_suspend_source.go` | 67 | watcher phase cache → `SuspendSource` | 2026-11-13/14 incident |
| `board_turn_evidence.go` | 43 | session_index `message_count` → stillborn guard evidence | #1832 WS4 |
| *(app package total)* | *7,408* | incl. `app.go` 2,676, `secrets_adapters.go` 1,075 (out of audit scope; §5 notes it as the extraction precedent) | |

### 2.2 `api/internal/handlers` — observability/board surfaces (non-test LOC)

| File | LOC | Role | Origin |
|---|---|---|---|
| `proxy_lifecycle.go` | 739 | ProxyHandler Start/Stop; outbox deliver/verify hooks (3 regimes); D6 `escalateHungs` (`session_hung`) + cooldown map | D3/#987, #1119, design 0052, D6/#998/#1685 |
| `board.go` | 917 | REST projection over `boardManager` (thin adapter; parity with the agent seam) | design 0067 §4.3a, #1699 Lane 2 |
| `proxy_worker_silent.go` | 355 | #2047 `worker_silent` arm: evidence gates, sweep, publish/resolve, session-scoped cooldown | #2047 (parent #2040) |
| `outbox_terminus.go` | 349 | agentd delivery-ledger terminus: prior-attempt resolution, POST/poll, I10 state map, hand-rolled Connect-JSON transport | design 0055 M2/M4, #1313/#1316 |
| `board_sse.go` | 302 | §4.6 board.event publisher (persist-then-project, fail-open) | #1699 gap 4 |

---

## 3. Deep-dive: the board_liveness hub, the busy source, and the read edges

### 3.1 What the hub is (and why its shape is right)

`boardLivenessHub` (`board_liveness.go:102-140`) is the engine's ONLY opencode-knowledge holder: one global monotonic counter stamping a two-class frame index (`framesWorker` — MESSAGE_*/PART_* renewal evidence; `framesOpaque` — everything else, zero renewal authority), the #2047 wall-clock companion (`framesWorkerAt`), nudge send-marks, per-ref same-source-delta cursors, and a late-bound `busyFn`. The engine asks four poor questions (moved? exists? after-sentinel? busy?) and this is the only place that knows how opencode answers (`board_liveness.go:12-14`). This is the principle *implemented*: the #2040 allowlist (`workerActivityStampsEvidence`, :190-201) is the single definition of "worker activity" on the surface, and the due-mark consumer is pinned to never touch the hub (`board_due_mark.go` header) — one progress contract, consumed as projections.

Costs that are real but bounded: the prune path (:237-291) hand-maintains the `framesWorkerAt` lockstep invariant in two places (write-stroke :222-229, eviction :273-279); the dual-identity stamping (raw + phrase per frame, #1817) is the tax of a heterogeneous ref world (#1850 durable-phrase storage) and is paid idiomatically — see F1.

### 3.2 The busy source chain (four hops, each documented)

`agentd` statusz → usagestream gate's consult-maintained `derived` map (`consumer.go:384-409`, keyed by internal harness id per #1817) → hub `busyFn` (wired `app.go:1678-1680` as a late-bound closure over the `UsageStream()` singleton) → engine `Busy` fact → badge (`BusyAssignees`) and the `worker_silent` veto. Every hop restates its staleness bound in place (`consumer.go:390-394`, `board_liveness.go:507-521`). This chain is the claim-lifecycle design's "one contract consumed as projections" realized end-to-end; no consolidation warranted — but note the read-edge duplication riding on top of it (F1) and the THIRD busy notation on the snapshot surface (`proxy_busy_snapshot.go:214`, `DerivedBusy: raw.Status != "idle"`) which is a one-way projection OF the authority's own status field (design 0055's "API proxies, never re-derives" — compliant, since the fact is computed from the authority's answer, not from side-facts; flagged as a documentation note, F10).

### 3.3 The three read paths re-composing the same legs (F1 — the audit's core finding)

Three public hub methods each rebuild "the facts for a set of refs" from the same three legs (identity resolve via `resolveRefs`, worker stamp, `busyFn`):

- `ObserveAssignees` (`board_liveness.go:414-531`) — the sweep's delta consumer (advances cursors; the ONLY impure read);
- `BusyAssignees` (`board_busy_source.go:51-104`) — the badge (pure; absent = quiet);
- `AssigneeQuiet` (`board_quiet_source.go:57-130`) — the quiet/busy-seconds read (pure; membership-is-an-answer contract).

Duplicated idioms, by count: the **max-of-two-identity-keys stamp read** (`s := framesWorker[raw]; if p := framesWorker[phrase]; p > s {…}`) appears 5× (`board_liveness.go:458-461` worker + `:462-465` opaque, `board_busy_source.go:89-92`, `board_quiet_source.go:101-103` on the `At` map with its own inverted-comparison variant `:116-119` on the ordinal map); the **derived-busy distrust guard** (`busyFn != nil && stamp > 0 && busyFn(ws, raw) → known && busy`) appears 3× verbatim (`board_liveness.go:522-526`, `board_busy_source.go:96-101`, `board_quiet_source.go:115-125`); the **resolveRefs + budget + degrade-loudly scaffold** appears 3×. The busy-answer semantics in `BusyAssignees` and `AssigneeQuiet` MUST agree (the badge and the alert evidence quote the same truth) — today they do, by discipline and by three test suites, not by construction. This is the #2040 drift class ("five divergent alive-derivations") nested one level down from where the incident fixed it.

**Consolidation verdict (collapsible):** one unexported `hubFacts` resolver — `facts(ctx, refs) []hubFact` where `hubFact{present, deleted, workerStamp, workerAt, opaqueStamp, workspaceID, rawID, phrase}` — plus one `derivedBusy(f hubFact) bool` guard method; the three public methods keep their distinct contracts (impure sweep vs two pure reads) and become loops over the shared resolver. Functional equivalence: same maps read under the same mutex, same absent/unknown/degraded semantics per surface, byte-identical answers; pinned by `board_liveness_test.go`, `board_busy_source_test.go`, `board_quiet_source_test.go` (the purity pins — "no cursors advance" — are behavioral and survive a pure refactor).

---

## 4. Deep-dive: the alert evaluator (session_hung + worker_silent)

### 4.1 Two arms, two evidence vocabularies (deliberate), one duplicated machinery (not)

`escalateHungs` (`proxy_lifecycle.go:561-629`) asks statusz: busy-age ≥ 15 min AND message-silence ≥ 30 min (`hungEvidence`, :642-650, the #1685 two-window gate). `escalateSilentWorkers` (`proxy_worker_silent.go:184-229`) asks the board seam: worker-frame quiet ≥ 30 min AND task progress-clock staleness ≥ 30 min, busy-vetoed (`workerSilentEvidence`, :135-149). The evidence classes are deliberately different — `session_hung`'s silence clock is message-boundary (LLM-completed or user-sent; statusz `LastProgressSeconds`), `worker_silent`'s is frame-level with the #2040 allowlist (`framesWorkerAt`) plus the task's own #2043 progress clock. Both arms fail toward silence on absent evidence. **The semantics are proportionate** — they encode two different questions about two different populations.

The **machinery around them is duplicated**: cooldown map + mutex + cooling/alerted/mark/clear (`proxy_lifecycle.go:699-729`, workspace-keyed) vs cooldown map + mutex + cooling/alerted/mark/clear/enumerate-by-prefix (`proxy_worker_silent.go:296-355`, composite-key `\x00`-joined); publish/resolve event pairs with the same envelope shape (`workspace.alert`/`workspace.alert_resolved`, policy `notify_only`); persistence through the same drainer; leave-Active resolution hooks (`resolveHungs` :656-671 vs `resolveDepartedSilentWorkers` :325-329). **Consolidation verdict (collapsible):** extract one `alertCooldown` value type (new/mark/check/clear/keys-with-prefix over a keyed map+mutex); keep the two evidence gates and the two event vocabularies as the semantics they are. Functional equivalence: identical map behavior (the silent arm's prefix enumeration becomes the helper's `Keys(prefix)`; cooldown floors stay per-arm via constructor TTL).

### 4.2 The quiet seam's SQL (F2 — the "rich facts, poor semantics" headline)

`quietBoardLookup.InProgressAssignees` (`board_quiet_lookup.go:51-107`) is the proxy evaluator's ONLY board input, and the INTERFACE is exemplary (#2047 R4: one method, never the service, rides the existing alert pass). The IMPLEMENTATION re-derives engine-owned semantics in raw SQL:

- **claim-eligibility predicates** — `stage='in_progress' AND assignee_session IS NOT NULL AND NOT pending_input` (:60-62) re-encodes the engine's claim-set definition, including the 000050 flag-not-stage discipline;
- **lease posture** — `live/lapsed/none` from `lease_expires_at` vs `now` (:77-84) re-derives what the engine's lease ladder computes authoritatively (and stringly: a 3-value projection of structured engine truth);
- **workspace scoping** — the `projects.target_workspace_id` join (:58-59) re-derives the engine's project-scope resolution.

The engine already exposes workspace-scoped read projections of exactly this class (`boardManager.ListTasksInWorkspace` / `BoardHealthInWorkspace`, #1837 P2a — `handlers/board.go:63-65`). If the engine's stage machine or lease semantics drift, this SQL diverges silently and the alert evaluator fires on a different world than the ladder acts on — the #2040 lesson verbatim. **Consolidation verdict (collapsible, engine-side):** add one read-only engine method (`InProgressAssigneesForWorkspace`-shaped: the fact rows with lease posture and progress clock); implement `quietBoardLookup` as a thin call; the `handlers.QuietBoardLookup` interface and the proxy's dependency-narrowness are untouched (dependency narrowness and derivation ownership are orthogonal). The nil-vs-empty honesty contract (nil = degraded, :45-50) maps 1:1 to the engine returning `(nil, err)`. Functional equivalence: the fact set is the engine's own sweep candidate set — by construction the same rows the quiet arms act on, which is the property the current SQL approximates by predicate-copying; `board_quiet_lookup_test.go` + `proxy_worker_silent_test.go` pin the wire behavior.

---

## 5. Composition root: app.go size and the wiring patterns

- **`New()` spans ~1,894 lines** (185→2078) inside a 2,676-line file; `Run()` (2080+) is the janitor-start list. 128 `Set*(` injection call sites over 81 distinct setter names (17× `SetLogger`, 7× `SetAudit`, …). The file's own header concedes the trade (`//nolint:funlen,gocyclo` at :184 — "decomposition would require a 20-field return struct"), and two extractors already exist as precedent (`newEmailMailer` :167, the 1,075-line `secrets_adapters.go`).
- **The one-function seam pattern is the repo's best wiring idiom and should be kept, not replaced** by service injection: `SetFrameObserver` (`app.go:1665`, re-installed as a composite at :1912 via `chainFrameObservers` — hub stamps first, due-mark consumer second, byte-identical for frames the consumer ignores), `SetQuietBoardLookup` (:1671), `SetBusySource` (:1678), `SetAgentdTerminus`/`SetV2Delivery` (proxy lifecycle), `SetEventPublisher` (:1892). Each is narrow, late-bound (construction-order-free), nil-inert by contract, and independently fake-able — the #2047 R4 / #1959 discipline. The audit found NO instance where a `Set*` seam should become a constructor injection or vice versa; the finding is purely locational (F5): the board block (`app.go:1600-1922`, ~320 lines — pool, hub, five seams, lease bus, three role-seed calls, an 18-option `spawnOpts` literal :1733-1828, bus publisher + SSE join, drive channel + goroutine, service + reconciler, turn-end consumer, REST handler) is a self-contained sub-root whose extraction is mechanical. **Consolidation verdict (collapsible):** `wireBoard(cfg, deps…) (*boardDeps, error)` returning the four fields `App` holds plus the handler refs; the same constructors in the same order (the block's internal ordering is load-bearing and commented — the extractor must take the comments verbatim). Functional equivalence: pure code movement at the single call site; `board_wiring_test.go`, `board_builtin_roles_seed_test.go`, `board_spawner_*_test.go` pin assembled behavior at App level.
- **Worker comms/messenger surfaces (proportionate):** four sibling closures (`newBoardNudger` `board_liveness.go:593-615`, `newBoardAborter`/`newBoardMessenger`/`newBoardRedispatcher` `board_worker_comms.go:353-417`) share the scope-resolver + resolving-sender core; the shared parts are ALREADY single-owned (`deliverBoardTextV1` :220-260 with its fast-fail windows, `boardScopeFor` :330, the #2046 single sentinel composer `agentMessageSentinelLine` :553). The four arms differ in delivery contract (bus-first-with-fallback for the nudger; no text for the aborter; 3 s vs 10 s fast-fail) — collapsing them into a type would save ~30 lines and cost the engine-seam naming. Leave.
- **Outbox/terminus/ABI serve paths:** `outboxDeliver` (`proxy_lifecycle.go:287-387`) is a three-regime branch tree — terminus (ledger POST/poll with prior-attempt resolution, `outbox_terminus.go:92-149`), adapter-fresh (gate-then-send), adapter-prior-attempt (verify-then-gate) — × V1/V2. The terminus regime is the proportionate end-state (the ledger IS the oracle; the I10 state map `outbox_terminus.go:192-208` is the only consumer-side mapping; the #1316 attempt-minting discipline is enforced at every return). The hand-rolled Connect-JSON transport (:230-339) is a documented dependency-lightness trade, pinned against the real generated handler — proportionate, with a note that it is the one place the ABI contract is hand-maintained as `map[string]any`. The V2 leg is F4 (below).
- **SSE projections are clean:** `board_sse.go` renders committed rows post-commit, fail-open, with the bus-first/v1-fallback unification in `board_bus.go` byte-parity-pinned; `handlers/board.go` is the thin-adapter archetype (its 107-line interface is the engine's own verb set; parity with the agent seam is test-enforced). No re-derivation findings on either.

---

## 6. Where projections re-derive engine-known state (consolidation verdicts)

The yardstick: `claim-lifecycle-design.md` principle 1 — *"one progress contract… consumed as projections; never re-derived"* — and design 0055's *"sole derivation point; API proxies, never re-derives."*

| # | Surface | What is re-derived | The engine already owns | Verdict |
|---|---|---|---|---|
| R1 | `board_quiet_lookup.go:55-84` | claim-set predicates, lease `live/lapsed/none`, project scope — in SQL | the sweep's own candidate set; the lease ladder; #1837 P2a workspace reads | **Consolidate** (engine-side fact method; keep the narrow interface) — F2 |
| R2 | hub read-edge trio (§3.3) | the same three legs re-composed per read path, incl. the busy distrust guard 3× | nothing external — the hub IS the fact owner; the duplication is internal | **Consolidate internally** (one `hubFacts` resolver + one guard) — F1 |
| R3 | `proxy_busy_snapshot.go:214` | `DerivedBusy = status != "idle"` (a 3rd busy notation) | agentd statusz status field | **Keep** — a one-way projection OF the authority's answer (0055-compliant); add the one-comment mapping note — F10 |
| R4 | two silence clocks (`hungEvidence` statusz message-silence vs `workerSilentEvidence` frame-quiet + task clock) | "how long silent" twice, different evidence classes | agentd (message boundaries); hub + task row (frames + #2043 clock) | **Keep** — deliberately different populations/questions; the claim-lifecycle LEXICON names both; cross-link the two gates' comments so the difference is stated once — F3 note |
| R5 | `QuietFor` single-key read (hub, `board_quiet_source.go:39-50`) | a per-key quiet-clock selection parallel to `AssigneeQuiet`'s | `AssigneeQuiet` itself — and NO production caller exists: `QuietFor` is referenced only by its own test file (repo-wide grep, this base) | **Dead-in-production** (F9) — remove (with its tests), or keep as the documented single-key primitive with a caller-note; do NOT let a future consumer reach it silently past the batched read |
| R6 | `resolveRefs` raw/phrase + `boardPhraseLookupFor` (`board_worker_comms.go:102-113`) | the phrase→raw registry query, twice board-side (plus middleware + pod-side replicas, by design) | session_index is the authority; board reads it twice with different miss semantics (merge-on-collision vs `""`-miss) | **Tolerable** — the two miss semantics serve different contracts; share only the SQL string if touched — F6 |

Engine-owned and NOT re-derived (verified clean): board REST/SSE projections (`handlers/board.go`, `board_sse.go` — render verbatim); `is_blocked` materialization (design 0067 R3/R4, engine-transactional); the due-mark epoch watermark (consumer-side by design, zero liveness authority); suspension phases (watcher cache → seam, nil-safe).

---

## 7. Findings matrix (common format)

*Class: **P** = proportionate (keep, cost is earned), **C** = collapsible (consolidate/refactor, functional equivalence arguable), **D** = dead (dead-in-fleet or dead-in-principle).*

| ID | Finding | Evidence | Cost | Class | Recommendation | Risk of change |
|----|---------|----------|------|-------|----------------|----------------|
| F1 | Hub read-edge trio re-composes the same legs: max-of-two-keys idiom 5×, busy distrust guard 3× verbatim, resolve-scaffold 3× | `board_liveness.go:458-465,522-526`; `board_busy_source.go:89-101`; `board_quiet_source.go:101-125` | ~150 LOC of parallel scaffold; the #2040 drift class nested one level down (badge vs alert evidence must agree by discipline, not construction) | C | one `hubFacts` resolver + one `derivedBusy` guard; three public methods become loops (§3.3) | low — pinned by three suites; purity pins are behavioral |
| F2 | Quiet seam re-derives engine semantics in SQL (claim predicates, lease posture, project scope) | `board_quiet_lookup.go:55-84` vs `handlers/board.go:63-65` (#1837 P2a reads exist) | semantic drift risk: the evaluator can fire on a different world than the ladder acts on; stringly `LeaseState` | C | engine-side read-only fact method; thin `quietBoardLookup`; interface untouched (§4.2) | med-low — touches engine read surface; honesty contract maps 1:1 |
| F3 | Two alert arms carry duplicated cooldown/publish/resolve machinery (4+6 near-twin methods, two maps+mutexes) | `proxy_lifecycle.go:699-729` vs `proxy_worker_silent.go:296-355`; event envelopes :602-618 / :254-267 | ~120 LOC duplication; a third alert arm would copy it again | C | one `alertCooldown` type; keep the two evidence gates separate (they ARE the semantics) (§4.1) | low |
| F4 | V2 promotion-wait leg dead on pinned harness (route deleted, #755), kept by design 0055 M4; the legal `v2Delivery && !terminus` combo points a 30 s wait at a 404 route | `proxy_lifecycle.go:328-378` (branch), :401-409 (flag), `outbox_terminus.go:344-349` (matrix); #755 cite in `board_worker_comms.go:27-37` | ~50 LOC + 2 tunables inert in the terminus regime; silent-wedge surface when V2 armed without terminus (ambiguous → verify against a store the harness never wrote — the #987 class) | D (dead-in-fleet, alive-by-design) | keep the code (0055 posture); add a boot-time warn when `v2Delivery` armed without terminus on the pinned harness; cross-link the #755 cite into the branch comment | ~zero (a warn) |
| F5 | `New()` ≈ 1,894 lines; board sub-root ~320 lines inline; 128 `Set*(` sites / 81 setter names | `app.go:185-2078`, board block :1600-1922 | navigation/orientation cost per feature touch; the nolint concession at :184 | C (structure) / P (content) | extract `wireBoard` (+ sibling extractors as touched); one-function seams KEPT (§5) | low-med — large mechanical diff; wiring tests pin assembly |
| F6 | Phrase→raw registry consulted twice board-side with different miss semantics; dual-key stamping tax everywhere | `board_liveness.go:341-406`; `board_worker_comms.go:102-161` | the max-of-two-keys idiom is F1's fuel; two query builders to update in tandem | P (bounded) | share the SQL string only if touched; the heterogeneous-ref world (#1850) earns the dual-key tax today | low |
| F7 | `framesWorkerAt`/`framesWorker` dual-map lockstep invariant hand-maintained in 2 places | `board_liveness.go:222-229,273-279` | a future edit that touches one map and not the other corrupts quiet answers silently | C (representation) | merge to `map[string]struct{stamp uint64; at time.Time}` — the invariant becomes unwritable-wrong; +24 B/entry | low — hot mutex path unchanged in shape |
| F8 | Four worker-comms closures re-do scope+resolve+deliver with per-arm knobs | `board_worker_comms.go:353-417`, `board_liveness.go:593-615` | mild; the shared core is already single-owned (deliverBoardTextV1, sentinel composer, scope resolver) | P | leave; collapsing costs the engine-seam naming for ~30 LOC | — |
| F9 | `QuietFor` single-key read is dead-in-production: no caller outside its own test file (the #2047 "hub read" superseded by the batched `AssigneeQuiet`) | `board_quiet_source.go:39-50`; repo-wide `\.QuietFor(` grep = test hits only | 11 LOC + a second quiet-clock selection to keep agreeing with the batched path | D (dead-in-fleet) | remove with its pins, or annotate as test-only primitive | ~zero — no callers |
| F10 | Snapshot `DerivedBusy = status != "idle"` — a third busy notation, unmapped in prose | `proxy_busy_snapshot.go:214` | vocabulary tracking cost only | P + note | one comment stating the one-way authority→bool mapping | ~zero |
| F11 | Hand-rolled Connect-JSON ABI transport (`map[string]any` payloads) | `outbox_terminus.go:230-339` | the one hand-maintained ABI contract site (pinned against the generated handler by test) | P (documented trade) | keep; consider typed payload structs next time the file is touched | low |
| F12 | Two-class frame index + cursors + nudge marks + busyFn in one hub | `board_liveness.go:102-140` | — | P | none — this IS the poor-facts seam done right | — |

---

## 8. Top 5 simplification candidates (ranked value/risk)

*No code changes in this audit — these are proposals for follow-up stories. Each carries its functional-equivalence argument.*

### #1 — Hub read-edge consolidation (F1+F7; F9 rides along)
- **Value:** kills the #2040 drift class where it is most dangerous — the badge (`BusyAssignees`) and the alert evidence (`AssigneeQuiet`) quoting the same busy truth by discipline instead of construction; −5 duplications of the two-key idiom, −2 of the guard; the quiet-clock selection stated once (R5); the `framesWorkerAt` lockstep becomes unwritable-wrong (F7).
- **Functional equivalence:** pure refactor inside one file family — same maps under the same mutex, same per-surface absent/unknown/degraded contracts, same purity (the sweep stays the only cursor-advancing path). The three suites (`board_liveness_test.go`, `board_busy_source_test.go`, `board_quiet_source_test.go`) pin behavior including the purity pins; the #2040/#2042/#2047 incident-pin tests ride them.
- **Risk:** low. Mechanical, test-pinned, no wire or schema change.

### #2 — Engine-owned quiet facts (F2 / R1)
- **Value:** restores "the engine is the decision plane" for the #2047 alert evaluator's world model — the evaluator's facts become the sweep's own candidate set by construction instead of by predicate-copying; the lease posture stops being a stringly re-derivation; future stage-machine changes (the 000050 flag/stage kind) cannot fork the alert world from the ladder world.
- **Functional equivalence:** the `handlers.QuietBoardLookup` interface, the proxy's dependency-narrowness, the nil-vs-empty honesty contract, and the alert wire format are all unchanged; only the fact source moves from a hand-written join to an engine read of the same rows with the same predicates (which the engine already evaluates for the sweep). Pinned by `board_quiet_lookup_test.go` (rewritten against the engine fixture) + `proxy_worker_silent_test.go` (unaffected — it fakes the interface).
- **Risk:** medium-low — one new engine read method; sequencing: land with the engine's own tests for the workspace-scoped reads (#1837 P2a precedent).

### #3 — Board-block extraction from `New()` (F5)
- **Value:** the composition root's board story becomes one readable call (`wireBoard`); the ~320-line block's load-bearing ordering comments travel verbatim into a file where they are the whole context; follow-up board wiring (the pattern is still growing — #2061/#2065 landed two new seams in it this month) gets an obvious home.
- **Functional equivalence:** pure code movement — the same constructors, in the same order, at a single call site; `App`'s four board fields plus the handler refs cross the boundary. The existing App-level wiring tests (`board_wiring_test.go`, `board_builtin_roles_seed_test.go`, `board_spawner_*_test.go`, `board_bus_discovery_integration_test.go`) pin the assembled behavior.
- **Risk:** low-medium — large diff, zero semantic delta; the one trap is moving the `SetFrameObserver` re-install at `app.go:1912` (the composite) without its ordering comment.

### #4 — Alert cooldown unification (F3)
- **Value:** −~120 LOC of twin machinery; the next alert arm (the lexicon invites more: `evidence-degraded`, `orbit-bound` are nameable shapes) inherits the cooldown/resolution discipline instead of copying it a third time.
- **Functional equivalence:** identical map semantics — keying (workspace vs workspace`\x00`session) parameterized, TTL floors stay per-arm; the silent arm's prefix enumeration becomes `Keys(prefix)`. Pinned by `proxy_worker_silent_test.go` (cooldown/resolve coverage) + the D6 tests.
- **Risk:** low.

### #5 — V2-dead-route boot warn (F4)
- **Value:** the `OPENCODE_V2_DELIVERY`-without-terminus combination currently degrades to the #987 duplicate-verify class with only a code comment as warning; a boot-time warn (and a one-line runbook note) makes the dead-leg posture operational knowledge instead of archaeology.
- **Functional equivalence:** unchanged delivery outcomes — a log line at wiring time; the flag matrix (`ValidateDeliveryFlags`) is untouched.
- **Risk:** ~zero.

**Honorable mentions:** F9's dead-`QuietFor` removal rides #1's file family (delete-or-annotate, ~zero risk); F10's comment and F4's cross-link are one-liners; F11's typed payload structs next-touch only.

---

## 9. Explicit non-goals

No code changes were made — this document is the audit's only artifact. No opinion is expressed here on pkg/board's internal complexity (out of scope; only its SEAM with api/internal was audited), on `secrets_adapters.go` beyond its role as extraction precedent, or on the CI/release surfaces (owned by the sibling audit, PR #2094). The "rich facts, poor semantics" verdicts in §6 are consolidation assessments, not owner rulings — engine-interface changes (candidate #2) carry design weight and need their own story.
