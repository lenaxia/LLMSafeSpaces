# pkg/board engine complexity audit — state columns, incident-hardening layers, seams

**Task:** tsk-sappy-nickel-shines (read-only complexity assessment of pkg/board: the 22+ task state columns and the ladder arms they feed, the #2042-#2045/#2061/wrap-up hardening stack as a SUM, and the reconciler/stage/audit/config seams)
**Date:** 2026-11-18. **Base:** origin/main `9f74d3020` (0.44.17 release head). **Worktree:** `audit/board-engine-complexity`.
**Method:** read-only — full-file reads of the non-test surface (~15.4 kLOC across 37 files; 21.2 kLOC of pins excluded but counted), migration archaeology (`api/migrations/000036-000059`, 13 board migrations, 10 adding task state columns), provenance-grounded in the code's own incident cites and the two spec records (`reports/specs/claim-lifecycle-design.md` — the #2060 port; `reports/specs/seamless-lease-design.md`), CHANGELOG 0.44.13-0.44.17 for the layer origins, `helm/values.yaml` for the deployed knob surface. **No code changes** (this document is the only artifact). Sibling audits: `reports/audits/api-app-complexity.md` (the adapter seam, tsk-tidal-wolfhound-restricts) and `reports/audits/ci-release-complexity.md` (PR #2094).

---

## 1. Executive summary

- pkg/board is **~15.4 kLOC of non-test Go over 37 files** (reconciler 1,781; service 1,388; config 1,039; stage 987; wrapup 930; reads 864; engine 800 are the seven largest), carrying **~26 task-table state columns**, a **21-seam Service** (19 `With*` options + 2 `Set*` setters), **13 engine knobs + 4 hard constants**, **48 `UPDATE tasks` write sites across 15 files**, and a **~30-term audit-action vocabulary** written through one owner (`events.go`). The test corpus is 58% of the package (21.2 kLOC) and is the reason the sprawl is survivable.
- The dominant cost is NOT any single column or arm — it is the **episode-reset discipline maintained by hand at 11+ write sites**: the ~10-column "clear the claim, reset the evidence slots" set is re-typed at every exit (wedge, reclaim, persona lapse ×2, drift re-entry ×2, bounce, exit-pending-reclaim, depose, rearm, review-settle, drift-done). This one fact generates the package's worst observed bug class (three real incidents of the "one site missed a column" shape — §4.4), and it is exactly the cost #2060's 22→8 collapse was drafted to remove. **A SQL-fragment consolidation removes the same bug class at ~1/50th the risk of the collapse.**
- The **22→8 ClaimEvidence collapse: NO-GO now, narrowed GO later** (§5). The columns are not redundant bookkeeping — each feeds a distinct single-statement SQL predicate, and that guarded-UPDATE shape IS the resolved replica-arbitration design (#2060 open-question 1). The full blob/struct rewrite re-opens every one of those writes, loses the CHECK-enforced invariants (`tasks_pending_origin_set`) and the indexable hot predicates, and the 2026-11-15 audit ruling already fences it as owner-gated phase 4. Three scoped collapses survive scrutiny: derive `last_attempt_failed` (provably ≡ `failure_count > 0` at every write site — verified), treat the sentinel triple as one conceptual slot, and **stop adding future evidence as new columns** (jsonb slots; `salvage` is the precedent).
- The incident-hardening stack (#2042 allowlist → #2043 two clocks → #2044 heartbeat-orbit budget → #2045 canary → #2046 directives → #2047 alerts → #2061/#2064/#2065 event entry → wrap-up engine → suspend freeze) is **semantically proportionate** — every layer traces to a real incident and they encode genuinely different questions — but it has begun to **overlap at the edges**: the #2043 lapse arm subsumes the busy/reaction rungs when the progress clock is stale, the #2044 budget demotion subsumes ALL observation evidence at cap, four independent caps share the value 3, and the 90m lapse threshold is *documented* as 2× lease but *stored* as an independent knob. The ladder's effective 11-arm precedence order is correct but exists only as scattered inline comments.
- Top 5 candidates (§8) are all **refactor-or-documentation class with one owner-gated schema note**: the episode-reset fragment, the ladder decision extraction, the narrowed collapse, the event-id minting helper, and the knob-lattice derivation. No code changed in this audit.

---

## 2. Inventory (what was audited)

### 2.1 File families and their roles (non-test LOC)

| File | LOC | Role | Origin |
|---|---|---|---|
| `reconciler.go` | 1,781 | RC1 worker-claim ladder + persona lapse/re-arm + pending coupling + dispatch drive + RC2 drift + RC3 deps + RC4 net + watermark | design 0067 §4.5; seamless-lease 000052; #2043/#2044/#2063/#2065 amendments |
| `service.go` | 1,388 | the action layer: guards, idempotency (eventWriter replay), 21 seams, pressure cache | design 0067 §4.2 |
| `config.go` | 1,039 | tiered board config (user defaults + project overrides, read-time merge, source annotations) | #1832 item 6; owner rulings 2026-10-24/10-29 |
| `stage.go` | 987 | MoveStage machine, done guard + cascade, bounce (#1959), exit-pending, **Heartbeat** (the #2044 response arm), throttle | design 0067 §4.1; 000050/000052; #1847/#1959/#2044 |
| `wrapup.go` | 930 | wrap-up engine seam: one interception point (`wrapUpAwareEpicCompletion`), mint, watermark delta, verdicts, ack | epic wrap-up review (000059, D1-D9) |
| `reads.go` | 864 | read surface + decorate passes (busy/quiet/phrase/deprecations) | design 0067 §4.8; #1837 P2a; #2047 |
| `engine.go` | 800 | claim CTE, DRR fairness, TaskStart, spawner/arbitration | design 0067 §4.4 |
| `rows.go` | 581 | the row shapes (Task carries 22 state columns on the wire) | — |
| `store.go` | 510 | scans, resolve helpers, transitions | — |
| `persona.go`, `phrase_read.go`, `pause.go`, `textguard.go`, `plan.go`, `ids.go`, `depose.go`, `board.go` | 368-450 each | personas; #1922 phrase reads; pause levers; #2030 argument guard; import_plan; #1831 phrase ids; S5 depose; errors/vocab/config consts | per-cited issues |
| `verdict.go`, `stamp.go`, `events.go`, `review.go` | 199-293 each | #1832 verdict channel; stamp write w/ retry+back-fill; the audit owner; review automation | — |
| `liveness.go`, `suspend.go`, `quiet.go`, `busy.go`, `due_mark.go`, `lease_events.go`, `ladder_metrics.go`, `rearm.go`, `criteria_met.go`, `depose.go` (shared), `builtin_roles.go`, `worker_compat.go`, `stillborn.go`, `seed_validation.go`, `criteria_check.go` | 31-199 each | the seams and one-file-per-incident remediations | each header carries its incident cite |

### 2.2 The task state columns (26), by the arm that feeds them

| Group | Columns | Migration | Incident/requirement of origin |
|---|---|---|---|
| Machine core | `stage`, `pending_origin`, `assignee_session`, `claim_token`, `lease_expires_at`, `last_progress_at`, `attempts`, `claim_incremented_attempts`, `cooldown_until`, `stage_entered_at`, `is_blocked` | 000036 | design 0067 (§4.4 Postgres-arbitrated dispatch); `stage_entered_at` later re-used as the #2044 epoch mint |
| Scheduling lever | `paused` | 000039 | #1747 (pause-must-pause) |
| Failed overlay | `last_attempt_failed`, `failure_count`, `last_failure_at` | 000041 | #1813 (failure visibility + doubling backoff) |
| Claim identity/provenance | `claim_persona` (000050), `implementer_session` (000051) | 000050/51 | #1941/#1946 reviewer claims; #1959 bounce |
| Ladder slots | `pending_input` (flag), `nudge_id`, `nudged_at`, `last_heartbeat_at` | 000052 | seamless-lease (000050/#1832 flag-not-stage) |
| Budgets/clocks | `busy_renews` (000053), `last_renewed_at` (000055), `no_advance_renews` (000057) | 000053/55/57 | busy-cap review r1; #2043 two clocks; #2044 heartbeat-orbit |
| Salvage | `salvage` (jsonb) | 000054 | S5 depose (2026-10-30 incident) |
| Row class | `kind` | 000059 | wrap-up engine (§1b) |

Plus 5 epic state columns (`wrap_up_review`, `last_wrapped_at`, `wrapup_count`, `wrapup_task_id`, `wrapup_ack_pending`, 000059) — the wrap-up engine's explicit-columns ruling (D2: never derive from row existence).

### 2.3 The write surface

- **48 `UPDATE tasks` statements** across 15 files (reconciler 15, stage 9, service 4, engine 4, wrapup 2, stamp 2, review 2, pause 2, depose 2, +7 singles).
- **11 sites clear the full episode set** (`claim_token=NULL` + sentinel slots + provenance): reconciler ×5 (wedge, reclaim, persona lapse, persona config-cleared, escalation), stage ×7 including `updateStageTx`'s baked-in reset (`stage.go:546`) and `bounceTx`, plus depose, rearm, review-settle, drift-done, exit-pending-reclaim.
- **32 `recordEventTx` sites** (reconciler 12, wrapup 6, engine 4, stage 3, review 2, events 2, delete 2, persona 1) over a ~30-term action vocabulary.

### 2.4 The knob surface

13 `EngineConfig` knobs (11 exposed in `helm/values.yaml` at defaults: `attemptsCap 3`, `leaseSeconds 2700`, `heartbeatLapseSeconds 5400`, `noAdvanceHeartbeatCap 3`, `throttleBackoffSeconds 300`, `failureBackoffSeconds 60`, `drrQuantum 1`, `sweepSeconds 30`, `userClaimCap 8`, `globalClaimCap 64`, `maxEdgesPerNode 16`, `maxNodesPerEpic 64` — `WrapUpConsecutiveCap` and `PressureSampleTTL` are code-default-only, not deploy-tunable today) + 4 hard constants (`boardBusyRenewCap 3`, `heartbeatFutureTolerance 2m`, `boardNudgeDeliveryConcurrency 8`, `maxClaimsPerSweep 64`) + 5 user/project config keys (`config.go`). No evidence in the repo of any deployment tuning the engine knobs off-default (single-instance posture; the values block is documentation-shaped).

---

## 3. Deep-dive: the ladder and the columns that feed it

### 3.1 The decision plane (what the columns buy)

`rc1Reclaims` (reconciler.go:306-905) is the package's center of gravity: a `FOR UPDATE SKIP LOCKED` victim scan computing **server-side, in one clock domain**, per victim: `progressLapsed` (`last_progress_at` vs `HeartbeatLapseThreshold`), `noEpochMotion` (`last_progress_at <= stage_entered_at` — the #2044 mint comparison), plus `busy_renews`, `no_advance_renews`, sentinel slots, and the project's workspace. The 11-arm precedence, in the order the code actually evaluates it:

1. suspension freeze (renew in place, pre-ladder);
2. `noAdvanceAtCap` (#2044 at-cap demotion — zeroes ALL observation evidence);
3. `hbAfterNudge` (response channel wins — checked BEFORE the lapse override, skipped at cap);
4. #2043 lapse arm (stale progress clock zeroes movement/reaction/busy evidence);
5. unobserved → nudge-or-wedge floor (heartbeat is the only detector at this rung);
6. movement / post-nudge reaction / busy-honored (busy capped by `boardBusyRenewCap`) → renew;
7. dead-evidence (`!Exists`) → reclaim;
8. observed-quiet: no nudge seam → lapse reclaim; no sentinel → send; unknown mark → re-nudge; no events after sentinel → wedge; else reaction renew.

Every one of these predicates is a **single-statement guarded UPDATE over named columns** — the shape that resolved #2060 open-question 1 (replica arbitration: predicates re-checked at write time, `RowsAffected==0` = converged-by-them; the Postgres transaction-clock ordinal proof for the send→stamp race is documented at the nudge write, reconciler.go:763-782). **This is what the 26 columns buy**: per-arm partial predicates (`WHERE nudge_id IS NOT NULL AND lease_expires_at > now()`, the due-mark's 9-predicate guard), CHECK-enforced invariants (`tasks_pending_origin_set`), partial indexes over hot predicates (`idx_tasks_reclaim ON lease_expires_at WHERE in_progress`), and column-level CASE arithmetic in one statement (the Heartbeat response write's 5-column budget CASE, stage.go:796-809).

### 3.2 What the sprawl costs (measured, not asserted)

1. **Write-path audit surface.** 48 UPDATE sites; a new evidence column must be threaded to every site whose episode it belongs to. Three real bugs of exactly this class are in the record: (a) review round 2 finding 4 — a `drift_review` park landing closed-unmerged violated `tasks_pending_origin_set` (the paired-form flag resolution now carried as inline SQL comments at three sites); (b) the #1947 corpse-assignee — the persona lapse pass originally kept `assignee_session`, poisoning the next claimant's stillborn guard until 03:49Z; (c) the escalate/nudge event-id collisions (an `ON CONFLICT DO NOTHING` silently dropping audit rows) — the id-minting cousin of the same threading discipline. The class is real, recurring, and expensive per instance.
2. **Migration churn.** 10 of 13 board migrations add task columns; each needs up+down, the helm mirror, and (per house discipline) a populated-database migration test (`migration_000052/55/58_populated_test.go` exist; 000052 — the pending_input stage→flag rehome under live data — is the cautionary tale the collapse cites). The churn is bounded but monotone: #2043 and #2044 each required a column within three weeks of each other.
3. **Onboarding/reading cost.** The Task row struct is 119 lines of field-comment provenance (rows.go:95-214); `rc1Reclaims` is ~600 lines; the precedence order above exists only as scattered inline comments; four internal-only columns (`last_heartbeat_at`, `busy_renews`, `no_advance_renewes`, `last_renewed_at`) don't even appear in the Task struct — the reconciler scans them inline, so no single place lists the full column set.

---

## 4. Deep-dive: the incident-hardening stack as a SUM

### 4.1 The layers, each with its incident

| Layer | Landed | The incident it answers | Where it lives |
|---|---|---|---|
| #2042 movement allowlist | PR #2052 | #2040 — five divergent alive-derivations; a settle loop renewed dead claims | hub-side class split (`framesWorker` vs opaque); engine sees only worker-activity deltas + `MovedOpaque` telemetry |
| #2043 two clocks + lapse arm | PR #2067 | 2026-11-15 — renewal stamped `last_progress_at`, deferring wedge detection by a window, indefinitely | `last_progress_at` (worker-anchored) / `last_renewed_at` (renewal) split + the lapse override |
| #2044 heartbeat-orbit budget | 0.44.16 | tsk-polished-keystone-waits — presence-only worker renewed forever via heartbeat responses carrying its own declared stamp | `no_advance_renews` + the mint comparison (`last_progress_at > stage_entered_at`) + the response-arm CASE decline |
| #2045 admission canary | 0.44.16 | wedged sessions stuck on prompt-path admission failures | `cmd/workspace-agentd/admission_watch.go` — **outside pkg/board** (correctly: it is a harness-delivery concern) |
| #2046 directive discipline | 0.44.16 | directives lost in flight; no audit | sentinel-composed halt/resume/deposed + delivery events |
| #2047 worker_silent + quiet | 0.44.16 | the 1h47m-class detection gap on the ALERT surface | api-side alert arm over pkg/board's quiet/busy read edges |
| #2061/#2064/#2065 event entry | 0.44.15 | the 1h47m detection latency itself | turn-end event → `ConsumeTurnEnd` due-mark (due_mark.go — 128 LOC, deliberately ONE guarded write) |
| Wrap-up engine | 0.44.16-17 | unclosed epics; reviewer recursion | wrapup.go + 5 epic columns + `kind` |
| Suspend freeze | 2026-11-13/14 | rate-limit suspension burned attempts and froze the board | suspend.go freeze arm (renewal, not skip) |
| Wait-lifecycle teaching | in-flight (docs-only) | workers misreading the wait contract | pattern docs; zero pkg/board delta |

### 4.2 The SUM assessment (interactions, overlaps, no-behavior states)

**Semantics: proportionate.** Each layer encodes a different question (frame-class truth; who wrote the progress clock; presence-vs-reaction; delivery-path health; detection latency; epic closure; suspension posture). The record's own strongest evidence: the three #2040 orbit channels were closed by three *different* mechanisms (allowlist, lapse arm, budget) because they were three *different* failures — a single merged "is this worker alive" arm would have re-opened any one of them.

**Edges that now overlap (the honest sum):**

1. **The lapse arm subsumes the renewal rungs when stale.** Post-#2043, a stale progress clock zeroes movement/reaction/busy before the rungs consult them (reconciler.go:556-565). The busy gate and reaction arm are therefore *conditional* arms — live only while the progress clock is fresh. Correct, but it means the rung list in any prose description is now two-dimensional (evidence × clock state), and neither the code nor the specs state the full matrix in one place.
2. **The #2044 demotion subsumes everything at cap** (reconciler.go:573-577) — by design (presence-not-reaction), but it is the third sequential evidence-zeroing block in the victim loop; a reader must hold three different "when does evidence die" rules to predict the ladder.
3. **Four caps, one value.** `boardBusyRenewCap` (hardcoded 3), `NoAdvanceHeartbeatCap` (3), `AttemptsCap` (3), `WrapUpConsecutiveCap` (3) — each independently justified at "epoch-scale ≈ 2¼h", but the *family* is undocumented: nothing states these are deliberately the same scale, and one of them is a const while its sibling is a knob.
4. **The 2× coupling is documentation, not code.** `HeartbeatLapseThreshold` is documented as "2 lease windows" (board.go:65-66, values.yaml) but stored independently — tuning `leaseSeconds` without `heartbeatLapseSeconds` silently breaks the documented measurement+grace structure. This is the one config interaction that can produce a *wrong* posture rather than merely a different one.
5. **Lexicon-vs-machine (the #2060 ruling) holds.** The lifecycle states (`active/productive/stale/challenged/orbit-bound/wedge/re-armed`) are predicates over the columns, never stored — the 2026-11-15 audit ruling ("vocabulary for alert surfaces, not an implementable machine") is respected throughout: no states column exists, `pending_input` remains flag-not-stage (000050), and the alert surfaces (the lexicon's consumers) read edges, not new state. **No no-behavior state was found.** The one candidate class — `pending_origin='dependency_abandoned'` (historical only, #1747 eliminated its writer) — is a *value* kept for old rows, not a behavior.
6. **Arms that subsume each other, found:** the freeze arm's persona twin (persona lapse pass re-implements the freeze's per-victim transaction rather than sharing rc1's batch shape — deliberate: different stage guard, but the second implementation of the same renewal idea); `dispatchDrive`'s EXISTS re-encodes the candidate predicate family (engine.go's `candidateSQL`, `eligibleProjectsSQL`, the drive's EXISTS, and the due-mark guard are four hand-maintained WHEREs over the same "claimable/due candidate" concept — each with a different purpose, but the predicate *family* has no shared definition).

### 4.3 The seam inventory (Service's 21 seams)

19 `With*` options + `SetEventPublisher`/`SetStampSink`: lives, nudger, spawner, aborter, messenger, redispatcher, drift, pending, turnEvidence, suspended, busySrc, quietSrc, leaseEvents, stampEvents, roles, failpoint, publisher, driveQueue, pressure. Every one is nil-inert with a pinned inert contract — the house's best pattern (the sibling audit's verdict, re-confirmed here: no seam should become constructor injection). The cost is purely enumerative: the constructor surface is where a new engineer must read ~18 doc-comment blocks to know what the engine can consult. The Service struct's field comments are the de-facto seam registry and are good; nothing to change but nothing to collapse either.

---

## 5. The 22→8 ClaimEvidence collapse — honest evaluation

**The proposal (#2060 Part 5, owner-gated phase 4):** collapse the evidence columns into `ClaimEvidence{Epoch, LastAdvance, LastTurnEnd, TurnNoAdvance, LastWorkerFrame, ChallengeCount}` + the kept trio (`stage`, `claim_token`, `lease_expires_at`).

### 5.1 What it would buy, checked against current code

- **One episode-reset site instead of 11+.** Real and the strongest argument: the threading bug class (§3.2.1) becomes unwritable — a blob reset is one assignment. But the same class dies to a SQL fragment (candidate #1) without touching the schema.
- **Less migration churn for future evidence.** Real: #2043/#2044 each needed a column migration within three weeks. But the churn is already mitigated by the slot pattern (000053/55/57 were clean ADDs; only the one rehome — 000052 — was hard, and rehomes are rare by construction).
- **A clearer mental model.** Partially real: eight fields beat 26 columns for teaching. But three of the eight (`LastTurnEnd`, `TurnNoAdvance`, `LastWorkerFrame`) are **not derivable from anything stored today** — `LastWorkerFrame` is deliberately hub-side in-memory (zero row-side authority, #2042), and per-row turn-end storage would be *new* state, not less. The struct describes a different design (the event-driven epoch model), not a compression of the shipped one.

### 5.2 What it would cost

1. **The guarded-UPDATE arbitration itself.** Every ladder arm's correctness rests on single-statement writes with column predicates (the replica-arbitration resolution; the Postgres-clock ordinal proof). A blob makes each arm's predicate a jsonb expression — expressible, but the physical invariants that make drift LOUD today (`tasks_pending_origin_set`, the `attempts >= 0` belt, the stage CHECK) have no equivalent without jsonb CHECK scaffolding, and the hot partial index (`idx_tasks_reclaim`) becomes a functional index over an extracted field.
2. **The wire and SDK surface.** 22 of the 26 columns ride the Task struct (openapi + Go/TS/Python/Java SDKs + frontend consumers of `last_attempt_failed`, `nudge_id`, `nudged_at` — verified in `sdks/openapi.yaml` and `frontend/src`). A collapse is a breaking wire change or a permanent translation shim.
3. **The provenance record.** The column comments ARE the incident record (each column's header cites its incident). A blob's keys lose the per-column CHECK/index/comment anchoring that makes drift fail loudly — the property three separate review rounds relied on (finding 4 was caught by a CHECK, not by review).
4. **Migration risk now vs. then.** When #2060 was drafted, the evidence columns were ~6 and untested-in-depth. Today they are 26, feed 48 write sites, and are pinned by 21.2 kLOC of tests including the kill-test fault seams. The rewrite's blast radius doubled since the proposal.

### 5.3 Verdict

**NO-GO on the full 22→8 collapse now.** Rationale: (a) the 2026-11-15 audit ruling fences it as owner-gated phase 4 and nothing in this audit found the fence wrong; (b) the columns are load-bearing SQL predicates, not bookkeeping — the collapse trades a documented, test-pinned threading discipline for an arbitration rewrite; (c) the strongest benefit is achievable at fragment-level risk; (d) three of the eight target fields are new state, not derived state, so "collapse" is partially a re-design in disguise; (e) the wire cost is real and permanent.

**Narrowed GO (on the next schema touch, behind the same owner gate):**
1. **Derive `last_attempt_failed` from `failure_count > 0`** — verified equivalent at every write site (all 5 setters write `true, failure_count+1` together; all 6 resetters write `false, 0` together; no divergent site exists). Fold into the next migration window; read-edge derivation until then.
2. **Treat `nudge_id`/`nudged_at`/`last_heartbeat_at` as one sentinel slot** — they move only in episode lockstep (arm sets both, response clears both + stamps, every exit clears all three). Columns may stay (the ordinal comparison and the CASE write are column-friendly); the *discipline* becomes one fragment.
3. **Stop adding evidence as columns.** Future ladder evidence rides jsonb slots on the existing pattern — `salvage` is the precedent (latest-only jsonb pointer, no CHECK scaffolding, added with zero churn in 000054). This caps the sprawl at today's 26 without a rewrite.

---

## 6. Deep-dive: the seams

### 6.1 reconciler.go (1,781 LOC)

The Sweep is a clean phase list (10 failpoint seams, each phase its own transaction, idempotent by deterministic event ids — the convergence contract is stated in the header and honored). The mass is `rc1Reclaims` (~600 LOC: scan → freeze → an 11-arm inline decision with 8 companion maps (`decisions`, `wedgeEvidence`, `nudgeIDs`, `renewSource`, `reclaimReason`, `busyOnlyRenew`, `hbNoMotionRenew`, `outbound`) → concurrent nudge batch → four write arms) and `rc1PersonaClaims` (~330 LOC: a lapse pass that is rc1's in_review twin at a different transaction granularity — one tx per victim vs rc1's batch — plus the re-arm pass). The decision logic is *correct* and *pinned*, but it is a function in disguise: 8 maps keyed by victim id, threaded through 300 lines. Extracting `decideLadder(victim, obs, cfg) → decision` + a small evidence struct makes the precedence list (§3.1) a readable ordered switch and the companion maps locals of a pure function — pinned byte-for-byte by the existing suites.

### 6.2 stage.go write sites (987 LOC)

`updateStageTx` already centralizes the episode reset *for stage moves* (`stage.go:546` appends the paired-form reset to every move — the finding-4 fix generalized) and `reset_overlay` (stage.go:35) is the #1813 triple's fragment. The mass that remains: `bounceTx`, `exitPendingReclaim`, and the reconcile-side exits hand-maintain their variants; and `Heartbeat` is the package's densest single statement (5 CASE columns over 3 budget inputs, stage.go:796-809) — correct, review-hardened twice, and a good candidate for a companion truth-table comment when next touched.

### 6.3 The audit/event surface

Single-owned and disciplined: `recordEventTx` + the `eventWriter` replay path (idempotency by caller requestId, 200-with-original on replay) + `writeAudit` for log-only actions. ~30 action terms over 32 sites. The one genuine roughness: **three event-id minting schemes coexist** — episode-keyed (`reconciler:reclaim:<id>:<episode>`), sentinel-keyed (`reconciler:nudge/wedge:<id>:<nudgeID>`), and nanos-keyed (the persona escalate fix, reconciler.go:1202) — each born from a distinct collision incident, each correct, but a new reconciler write must *choose* correctly with only prose to guide it. `engineEventID` exists (stage.go) and is used by some sites; standardizing the reconciler on it (with the scheme documented at the helper) is a comment-sized fix.

### 6.4 The config surface

13 engine knobs + 5 tiered config keys + 4 constants (§2.4). The tiered config system (config.go, 1,039 LOC) earns its size: read-time merge, source annotation, REPLACE discipline, advisory-locked writers, drift-as-500 — it is the best-documented file in the package. The engine knobs' issues are the two couplings of §4.2(3)(4): the undocumented cap-family (4× "3") and the 2×-lapse coupling that is documentation-only. One derivation (lapse default = 2× lease, overridable) + one family comment closes both.

---

## 7. Findings matrix

*Class: **P** = proportionate (keep, cost is earned), **C** = collapsible (consolidate/derive; functional equivalence arguable), **D** = dead (remove/derive at next window). "Monthly cost" is the recurring maintenance/bug-surface/onboarding tax, argued not measured.*

| ID | Finding | ORIGIN | Monthly cost | Class | Proposal | Functional-equivalence pin | Risk |
|----|---------|--------|--------------|-------|----------|---------------------------|------|
| B1 | Episode-reset set (~10 columns) hand-maintained at 11+ exit sites | Accretion, no single cause — each exit copied the set; the class #2060 was chartered to close | The package's worst bug class (3 real incidents: r2-finding-4 CHECK violation, #1947 corpse-assignee, escalate id-collision cousin); every new evidence column multiplies it. Already drifting: the nudge self-fail (stage.go:363) and rearm clear the sentinel pair but NOT `last_heartbeat_at` (benign today — the ordinal comparison is sentinel-gated — but inert-drift between reset sets is this class's early stage) | **C** | One `episodeResetSQL` fragment (extend `reset_overlay`'s pattern) used by every exit/mint site; the mint sites' `stage_entered_at`+`last_progress_at` same-statement invariant rides it | Byte-identical SQL at expansion; `lease_pins_test.go` (1,182 LOC), the kill suites, and the 000052/000055 populated-migration pins | low |
| B2 | `last_attempt_failed` ≡ `failure_count > 0` at every site (verified: 5 setters, 6 resetters, zero divergence) | #1813 overlay designed pre-derivation | one column in every scan/reset; a fourth term in the overlay triple that F1 must thread | **D** (at next schema window) | derive at read edge now; fold the column out in the next migration | wire keeps rendering it (read-edge derivation); frontend/SDK consumers unchanged | low (wire field stays; storage drops) |
| B3 | Sentinel triple (`nudge_id`,`nudged_at`,`last_heartbeat_at`) moves only in lockstep | 000052 seamless-lease | 3 columns in the reset set; ordinal logic spread over 2 files | **C** (discipline only; keep columns) | treat as one slot via B1's fragment; columns stay (CASE write + ordinal comparison are column-friendly) | same SQL; `heartbeat_lapse_2043` + `heartbeat_orbit_2044` suites | low |
| B4 | Ladder precedence (11 arms, 3 evidence-zeroing blocks) exists only as scattered inline comments | #2042/#2043/#2044 sequential incidents | every ladder change re-derives the order from 300 LOC; the 8 companion maps are a function in disguise | **C** (structure) | extract `decideLadder` pure function + write the §3.1 precedence table into its doc comment | pure refactor inside rc1's transaction; kill suites + `settle_no_authority_2042`/`heartbeat_lapse_2043`/`heartbeat_orbit_2044` pin every arm | low-med (mechanical, large diff) |
| B5 | The 2× lapse coupling is documentation, not code; cap family (4× "3", one const three knobs) undocumented | #2043/#2044 as separate PRs | tuning `leaseSeconds` alone silently breaks the documented measurement+grace structure | **C** (one derivation + one comment) | default `HeartbeatLapseThreshold = 2×LeaseDuration` (explicit override kept); EngineConfig doc block gains the cap-family table | defaults byte-identical (`2×45m = 90m`); `config_test.go` pins | low |
| B6 | 22→8 ClaimEvidence collapse | #2060 phase 4 (owner-gated) | — (not incurred: not scheduled) | **P** (the fence is right) / narrowed C per §5.3 | NO-GO now; B2 + B3 + "jsonb-not-columns for future evidence" is the surviving 20% | §5 | high if forced; ~zero as documented posture |
| B7 | Four hand-maintained "candidate/due" predicate family (candidateSQL, eligibleProjectsSQL, dispatchDrive EXISTS, due-mark guard) | design 0067 + #2065 + freeze, each with different purpose | a stage-semantics change (e.g. a future flag) must find all four | **P** (bounded) + note | shared WHERE-fragment for the stable core predicates when next touched; purposes stay distinct | same SQL | low |
| B8 | rc1 vs persona-lapse: two transaction granularities for the same renewal idea (batch vs per-victim) | deliberate (stage guards differ; fault isolation differs) | a reader must decide which pattern a NEW pass copies | **P** | leave; note the rule (batch for in_progress, per-victim where the stage guard lives in the write) | — | — |
| B9 | Three event-id minting schemes (episode / sentinel / nanos) | three distinct collision incidents, each fixed locally | new reconciler writes choose a scheme by prose only | **C** | standardize on `engineEventID` + document the three discriminators at the helper | ids unchanged where sites already conform; collision pins exist per scheme | low |
| B10 | 4 engine-internal columns absent from the Task struct (no single place lists all 26) | accretion | inventory archaeology per audit/reviewer | **C** (doc) | one column-inventory comment block at the Task struct (or store.go scan) listing internal-only columns | none (comment) | ~zero |
| B11 | Wrap-up engine: 5 epic columns + kind + one interception point | wrap-up design D1/D2 (explicit columns, never derive) | real but earned: the recursion cap/ack loop is pinned end-to-end | **P** | none | — | — |
| B12 | Suspend freeze re-implemented in the persona pass (vs shared batch shape) | the 2026-11-13/14 incident fix, persona twin added same PR | small; both sites cite each other | **P** | leave | — | — |
| B13 | `Heartbeat`'s 5-column CASE budget write | #2044 (the decline semantics) | densest single statement in the package; needed two review rounds | **P** + note | truth-table comment when next touched | — | — |
| B14 | 21 nil-inert seams on Service | one per incident-driven source | enumerative only; each seam's inert contract pinned | **P** | none — the pattern is the house's best | — | — |
| B15 | `pending_origin='dependency_abandoned'` — historical-only value (writer eliminated by #1747) | #1747 delete-replaces-cancel | one CHECK value + one consumer arm that can no longer mint | **D** (vocabulary, not code) | note in the origin consts (already marked "historical only"); removal only with a wire-window | the const comment already carries it | ~zero |

---

## 8. Top 5 simplification candidates (ranked value/risk)

*No code changes in this audit — proposals for follow-up stories. Each carries its functional-equivalence argument.*

### #1 — The episode-reset fragment (B1+B3+B10)
- **Value:** structurally kills the package's worst bug class (the threading incidents) at every existing and future exit site; the mint invariant (`stage_entered_at` written with `last_progress_at=now()`, the #2044 epoch anchor) becomes part of one auditable string; the column inventory comment rides along. This is the 20% of the 22→8 collapse that pays 80%.
- **Functional equivalence:** a SQL text fragment expanded at the same sites — the emitted SQL is byte-identical (modulo column order, which Postgres does not observe); every exit path is pinned (`lease_pins_test.go`, `1813_kill_test.go`, `1747_kill_test.go`, `bounce_1959_integration_test.go`, `depose_integration_test.go`, `rearm_integration_test.go`, `pending_exit_manager_integration_test.go`).
- **Risk:** low. One file-family diff, zero wire/schema delta. The trap is a site whose reset is deliberately PARTIAL (the persona config-cleared convergence keeps the identity columns) — the fragment needs strict/partial variants, named for their episode semantics.

### #2 — Ladder decision extraction (B4)
- **Value:** the 11-arm precedence becomes one readable ordered switch with the three evidence-zeroing blocks contiguous and cited; the 8 companion maps become locals; the next incident arm (there will be one) gets a 60-line pure function to extend instead of a 600-line transaction body.
- **Functional equivalence:** the decision function consumes the already-scanned victim fields + the observation and returns the arm + write inputs; the transaction body, the SQL, the ordering (freeze → scan → decide → deliver → write), and the post-commit projections are untouched. Every arm has a named incident pin (`settle_no_authority_2042`, `heartbeat_lapse_2043`, `heartbeat_orbit_2044`, `fairness_kill_test`, `reconciler_kill_test`).
- **Risk:** low-medium. Mechanical but large; the concurrency of the nudge batch (locks held during delivery — the documented invariant) must stay outside the extracted function.

### #3 — The narrowed collapse: derive the overlay, cap the sprawl (B2+B6)
- **Value:** one provably-dead column leaves storage; the "future evidence rides jsonb" rule (the `salvage` precedent) caps the migration treadmill without re-opening arbitration; the 22→8 question gets a closed, evidence-based answer for the owner gate (this audit's §5).
- **Functional equivalence:** `last_attempt_failed` derivation is verified at every write site; the wire field persists via read-edge decoration until a window removes it; the jsonb rule is forward-looking (no existing column moves).
- **Risk:** low (derivation) / owner-gated (any storage drop). This is deliberately NOT the #2060 rewrite.

### #4 — Event-id minting standardization (B9)
- **Value:** removes a silent-failure footgun (an `ON CONFLICT DO NOTHING` dropping audit rows on a mis-chosen discriminator) for every future reconciler write; the three schemes' collision incidents become one documented decision table.
- **Functional equivalence:** `engineEventID` already exists and renders the same id grammar; sites already conforming emit identical ids; the collision pins (persona escalate nanos, nudge/wedge sentinel determinism) hold.
- **Risk:** ~zero.

### #5 — The knob-lattice derivation + cap-family table (B5)
- **Value:** closes the only config interaction that can produce a wrong posture (lapse tuned off the 2× relationship); makes the four "3"s a stated design scale rather than coincidence; costs one default expression and one doc block.
- **Functional equivalence:** `2×2700s = 5400s` — the default is byte-identical; the explicit override knob survives for the documented escape hatch; `config_test.go` + `values.yaml` pin the default.
- **Risk:** ~zero.

**Honorable mentions:** B7's shared candidate-predicate fragment (when next touched); B13's truth-table comment; B15's vocabulary note. Explicitly NOT proposed: merging the busy/no-advance budget counters (their resets differ on heartbeat-vs-busy evidence — merging trades a pinned asymmetry for one column); collapsing the 21 seams (the nil-inert pattern is the package's most reusable asset); any lexicon-to-machine promotion (the 2026-11-15 ruling governs).

---

## 9. Explicit non-goals

No code changes were made — this document is the audit's only artifact. No opinion is restated here on the api/internal adapter seam (owned by `reports/audits/api-app-complexity.md`) or CI/release (PR #2094). The §5 verdict is an input to the owner's phase-4 gate, not an exercise of it. The #2045 canary and #2047 alert arms were assessed only as layer-sum members; their own packages were not audited. Nothing here authorizes a state-machine rewrite, a states column, or the 22→8 collapse — the 2026-11-15 audit ruling stands.
