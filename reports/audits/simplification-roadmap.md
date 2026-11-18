# Cross-surface simplification roadmap — synthesis of the six complexity audits

**Task:** tsk-chilled-hawthorn-notices (SYNTHESIS: dedupe the six surface audits, resolve conflicts, produce the ranked cross-surface simplification roadmap with sequencing and a do-NOT-touch list; read-only — no code changes).
**Date:** 2026-11-18. **Base:** origin/main `0aedbaf0a` (0.44.17 release head + the wait-lifecycle design docs, PR #2101). **Worktree:** `docs/simplification-roadmap-synthesis`.

**Inputs (the six audits, all read in full):**

| Surface | Doc | Task | PR (open at this head) |
|---|---|---|---|
| board-engine | `reports/audits/board-engine-complexity.md` | tsk-sappy-nickel-shines | #2098 |
| api-app | `reports/audits/api-app-complexity.md` | tsk-tidal-wolfhound-restricts | #2096 |
| agentd | `reports/audits/agentd-complexity.md` | tsk-superb-hammer-pricks | #2097 |
| emission-defenses | `reports/audits/emission-defenses-complexity.md` | tsk-dual-tanker-ambles | #2095 |
| frontend | `reports/audits/frontend-complexity.md` | tsk-pallid-lavender-supports | #2099 |
| ci-release | `reports/audits/ci-release-complexity.md` | tsk-hazel-mint-waits | #2094 |

**Method:** read-only synthesis — every finding from all six matrices was cross-indexed; duplicates merged where two audits taxed the same accretion; conflicts adjudicated with the audits' own evidence (§4); nothing re-measured, nothing invented. Finding IDs below are the audits' stable IDs (B*, F*, FC*, plus the ci-release matrix verdicts). **No code changes** (this document is the only new artifact besides its worklog/CHANGELOG/COORDINATE rows).

---

## 0. Owner summary (the epic comment)

> Six read-only audits of ~45 kLOC of Go + 8 kLOC of frontend hotspots + the CI/release train converge on one picture: **the complexity that hurts is not any single subsystem — it is four repeating classes**, and each class has a cheap, test-pinned cure already argued by its audit.
>
> 1. **Re-derivation of owned truth** (the #2040 lesson, still alive at three seams): the api hub re-composes its read legs 3×; the #2047 alert evaluator re-encodes engine claim/lease semantics in raw SQL; agentd's kill-gating loops read a second busy derivation while the sessionstate authority owns the first. → initiatives 5, 7, 8.
> 2. **Hand-maintained reset/lockstep disciplines**: pkg/board's ~10-column episode-reset re-typed at 11+ exit sites (its worst bug class, 3 real incidents); the emission stack's three comment-enforced lockstep sets; ChatPage's dual reset effects (2 shipped bug rounds). → initiatives 2, 4, 5.
> 3. **Copy-paste machinery**: the credential trio (~60% dup, 4 shipped user-facing divergences incl. an unconfirmed org-tier delete), twin alert-cooldown maps, a whole duplicated CI frontend job. → initiatives 1, 6, 14.
> 4. **Dead or unvalued mass**: a never-flipped defense flag with a 3-site lockstep tax that grew 38→41 tools in its first week; an unwired compaction method (unbounded ledger growth); ~30 runner-minutes of per-PR CI spend with zero gate value (the duplicated e2e job, the informational-only coverage-delta comment, the `continue-on-error` canary) — plus ~73 more recoverable through the build-matrix conditionalization, which is the one initiative that honestly shifts a detection window and is presented as a stated trade for the owner to accept or decline (§4.6). → initiatives 1, 3, 4, 11.
>
> **The one urgent call:** the wait-lifecycle implementation (designs merged, PR #2101) is about to add state columns and board tools — land the **episode-reset fragment (initiative 2) and the strict-args lockstep retirement (initiative 4's decision half) before it**, so its columns join a fragment instead of becoming three more hand-typed reset sites, and its tools skip the stamp-mirror tax. The full 22→8 column collapse stays **NO-GO** (owner-gated phase 4 fence affirmed).
>
> **The armor list (§8) is as important as the roadmap**: aged-arg v3, transcript hygiene, the wire strip, the renewal caps, the 12 healing loops, the ledger FSM, the nil-inert seams — every one is incident-proven with no functional-equivalent simplification; cutting any re-opens a named incident class.
>
> The 15 initiatives are ranked and sequenced in §5/§6. Roughly: ~30 zero-gate runner-min per PR recovered outright plus ~73 more via the stated build-matrix trade (CI); 4 user-facing divergence bugs closed (frontend trio); 3 recurring bug classes made structurally unwritable (episode resets, hub read legs, lockstep sets); 1 unbounded-growth liability wired; 1 cross-repo transcription-error class automated away.

---

## 1. Charter scope

Synthesis only: dedupe, adjudicate, rank, sequence, fence. Every initiative below is a *proposal for follow-up stories* — none is exercised here. Findings whose simplification would reduce functionality are flagged OUT of scope in §9 per the charter; the do-NOT-touch armor list (§8) carries incident provenance and the no-equivalent-simplification argument for each entry.

## 2. Dedupe rules applied

- Same mechanism, same surface, two audits → merged (one initiative, both IDs cited).
- Same *class*, different surface → one initiative family with per-surface legs (sequenced separately), because the fixes are in different files with different risk profiles.
- Same surface, genuinely different findings → kept separate.
- A finding one audit calls **P (proportionate)** that another's wording invites collapsing → escalated to §4 adjudication, never silently merged.

## 3. Cross-surface dedupe map

| Theme (the same accretion, as it showed in multiple audits) | Findings merged | → Initiative |
|---|---|---|
| #2040 re-derivation class, read-side (busy/liveness truth kept in agreement by discipline, not construction) | api-app F1+F7+F9; agentd F1/R1/R3; (board-engine's internal twin: B7) | 5 (leg 1), 8 (leg 2); B7 rides 13 |
| Engine semantics re-derived outside the decision plane (alert world vs ladder world) | api-app F2/R1; emission F6 (predicate mirror reconciler↔depose) | 7 (+ the F6 half folds into 13's single-authority work) |
| Hand-maintained reset-by-enumeration | board-engine B1+B3; frontend FC-2 (and FC-6's implicit transitions) | 2 (board), 12 (frontend) |
| Comment-enforced lockstep sets | emission F4 (identity keys ×3), emission §7.2 (41-tool stamp mirror ×3); api-app F7 (dual-map lockstep); board-engine B9 (3 event-id schemes) | 4 (stamp+identity), 5 (dual-map), 13 (event-ids) |
| Copy-paste machinery with shipped divergence | frontend FC-5 (credential trio), FC-7 (paged-list scaffold); api-app F3 (twin cooldown maps); agentd F11 (twin pollers); ci-release F1 (duplicated CI job) | 6, 14, 1 |
| Dead-in-fleet / dead-in-principle mass | ci-release F1 (dead `if: false` job), F4 (coverage-delta zero gate), F3 (canary non-blocking); api-app F9 (QuietFor), F4 (V2 dead route — keep+warn); agentd F8 (`suspect()`), F7 (lease backstop — keep+note); emission F1 (strict-args); board-engine B2 (`last_attempt_failed` derivable), B15 (historical vocab value) | 1, 11, 14, 4, 13/2 |
| Config-surface hygiene (untested knobs, undocumented families) | board-engine B5 (2×-lapse coupling, 4×"3" family); emission F3 (untested agedarg window) | 13 (lattice), 4 (knob collapse) |
| Documentation-as-code (maps that were archaeology until the audits) | agentd F2/R2-R4; board-engine B4 doc half, B10; api-app F10, R4 cross-links | 14 (doc riders on the touching initiatives) |

## 4. Conflict adjudications (evidence-cited)

1. **"Converge all busy derivations" vs the sealed-module ruling.** An aggressive read of agentd F1/R1 + R2 + R3 would fold *every* busy read onto the authority. The audit's own evidence forbids it: the notifier mirror fold is an explicitly ruled sibling-consumer (sealed-module price, `session_notifier.go` header); healthz "serving vs turning" is a different question; the #1576 orchestrator steer refused seam widening. **Adjudicated: converge only the two decision-gating reads** (watchdog deferral, session-aware restart) plus opportunistic statusz — initiative 8, scoped as an authority *read-edge*, not a seam change. The fold and healthz stay.
2. **"Unify the two alert arms" vs two deliberately different evidence classes.** api-app R4 rules the silence clocks proportionate (message-boundary silence vs frame-quiet + task clock — different populations, both in the claim-lifecycle lexicon); F3 rules their *machinery* duplicated. **Adjudicated: unify the cooldown/publish/resolve machinery (initiative 14); the evidence gates and vocabularies stay two.**
3. **22→8 ClaimEvidence collapse.** board-engine §5: NO-GO (columns are the resolved single-statement guarded-UPDATE arbitration; 3 of 8 target fields are new state; CHECK/index/wire costs). No sibling audit dissents; emission F6's predicate-mirror finding *reinforces* the fence (more shared-authority work, not less). **Adjudicated: fence affirmed; the narrowed survivors ride the wait-lifecycle migration window (initiative 2 + §6).** This answers the charter's explicit sequencing question: the full collapse is not "before or after" wait-lifecycle — it is *never*, until the owner gate re-opens it; only the fragment-before / derivation-within / extraction-after split is scheduled.
4. **Retire vs keep the strict-args gate (#2030c).** Emission §7.1: redundant in every observed class (0 non-member key-form emissions all-time, corrected classifier; 1 strict bleed all-time, pre-hygiene). The defense-in-depth instinct says keep; the evidence says the distinguishable class was empty *before* the later layers shipped, and the flip gate will never flip. **Adjudicated: retire (decision now, code deletion after the +7 d corrected-classifier re-measure, ~2026-11-24); the schema-side `additionalProperties:false` advertisement survives unconditionally as the teaching carve-out.** The de-novo backstop forgone is unobserved in every regime — recorded as accepted residual risk, not silently dropped.
5. **Posture-gate breadth (ci-release F11).** TUNE per the audit, but the breadth was a deliberate design-0061 §11 ruling-3 choice. **Adjudicated: proposal-only in this roadmap (§9 flags it owner-gated); no initiative scheduled unilaterally.**
6. **Build-matrix conditionalization (ci-release #2).** The one initiative that honestly *shifts a detection window* (cross-arch Dockerfile breaks surface at merge+minutes instead of pre-merge). **Adjudicated: kept, with the trade stated in the initiative body; the mitigations are the untouched main-push full matrix, the self-contained release.yml rebuild, and the verified absence of required-status-checks on build rows. The owner may decline this one and lose only its savings.**
7. **Release-path `test-full` drop (ci-release honorable mention).** Reduces tag-path verification; main already ran it. **Adjudicated: OUT of scope (§9) — verification-reducing; if ever taken, it is an owner ruling with the .15 linter-drift catch as the counter-evidence.**
8. **sdk-canary disposition.** Two variants in the audit: make-blocking (adds a gate) vs move-to-nightly (removes PR-path coverage). **Adjudicated: fix-then-block is the primary (initiative 11); the nightly move is the fallback only if the 429 class proves incurable — it is flagged in §9 as the functionality-reducing variant.**
9. **Board-engine B8/B12 "twins" (batch vs per-victim renewal; freeze re-implementation in the persona pass).** Both ruled proportionate-with-note by their own audit; the merge instinct (one shared pass) would trade pinned stage-guard differences. **Adjudicated: leave, documented.**

## 5. The roadmap — 15 initiatives, ranked

*Rank = recommended execution order under the sequencing constraints of §6 (quick wins and the wait-lifecycle-critical items first; gated items last). Value/risk/effort (S/M/L) per initiative; every initiative carries its audits' functional-equivalence anchor. Classes per the audits' own verdicts.*

### 1. CI zero-risk quick wins — delete the duplicated e2e job, single-source the linter pin
- **Source:** ci-release F1, F6 (+ F8/F9 process/comment notes riding).
- **Value:** ~11 runner-min + 2 check rows per frontend PR for a *functional duplicate* (e2e-pr re-runs ci.yml's frontend step set, already drifted: step order + `ubuntu-latest` vs the pinned `ubuntu-24.04`; its `e2e-fullstack` sibling is `if: false` dead code since #24). The golangci pin drift between ci.yml/release.yml stalled the v0.44.15 train (2 of its 3 dead runs) — one Makefile var closes the class.
- **Risk:** ~zero (the same steps run in ci.yml; the pin default is byte-identical). **Effort:** S.
- **Equivalence:** ci-release §6 #1/#honorable — same step set, newer runner pin, same green signal; `make release-tag` validations unchanged.
- **Sequencing:** none — land first; proves the CI lane for initiative 11. Riders: the `.trivyignore`-with-expiry discipline (F8) and the stale `test-full` header comment (F9) belong in the same touch.

### 2. pkg/board episode-reset SQL fragment — **before wait-lifecycle implementation**
- **Source:** board-engine #1 (B1+B3+B10); the B2 derivation folds into the same migration window.
- **Value:** structurally kills the package's worst observed bug class (the ~10-column clear-the-claim set hand-maintained at 11+ exit sites; 3 real incidents: the r2-finding-4 CHECK violation, the #1947 corpse-assignee, the escalate id-collision cousin; early drift already visible — nudge self-fail and rearm clear the sentinel pair but not `last_heartbeat_at`). This is the 20% of the 22→8 collapse that pays 80%, at ~1/50th its risk. The sentinel triple becomes one conceptual slot; the column-inventory comment (B10) rides along.
- **Risk:** low (one file family; strict/partial fragment variants named for episode semantics — the persona config-cleared site keeps identity columns deliberately). **Effort:** M.
- **Equivalence:** byte-identical SQL at expansion; pinned by `lease_pins_test.go`, the kill suites, the 000052/000055 populated-migration pins.
- **Sequencing (the charter's question, answered):** **land BEFORE the wait-lifecycle implementation stories** (designs merged at PR #2101) — the typed-wait columns then join the fragment instead of becoming three more hand-typed reset sites. Fold B2 (`last_attempt_failed` ≡ `failure_count > 0`, verified at every write site) into the wait-lifecycle migration window. If wait-lifecycle lands first, retrofit is small (its exits meet the fragment on merge).

### 3. Wire delivery-ledger compaction — close the unbounded-growth liability
- **Source:** agentd #1 (F5).
- **Value:** `deliveryLedger.compact()` is complete and tested with **zero production callers** — terminal rows and the WAL grow monotonically for the pod's lifetime on long-lived pods; the retention policy exists on paper only. This is a wiring gap, not a build.
- **Risk:** low — one call on the reconcile-watchdog cadence with a retention cutoff; compaction preserves in-retention rows by construction; the outbox dedupe window (retry ladder ≤ 80 s) is orders below any sane retention. **Effort:** S.
- **Equivalence:** agentd §8 #1 — delivery outcomes unchanged; pinned by the ledger suite.
- **Sequencing:** independent; soon (it is a live liability, not a style issue). Riders: the §3.1 loop map committed as living docs (agentd #5's half that documents this family).

### 4. Emission-stack maintenance-surface shrink — retire strict-args, single-source the lockstep sets
- **Source:** emission #1-#4 (F1-F4 + §7.2); interacts with board-engine (new-tool friction).
- **Value:** (a) the #2030c dispatch gate never flips (its distinguishable class was empty all-time — corrected classifier), and its three-site lockstep (plugin stamp set ↔ 41-tool mirror ↔ parity test) grew 38→41 in its first week — every new board tool pays it; (b) the identity-key set is comment-locked in three sites — one definition; (c) the agedarg `>0` window knob is an untested branch on the most safety-critical transform — collapse to the binary kill switch; (d) the 1949 replay-harness arms target a dead class (0 events post-rollout) — freeze as incident evidence, keep the slim parity pin.
- **Risk:** low-med (the de-novo backstop is forgone — unobserved; the +7 d re-measure gates *code deletion*, not the decision). **Effort:** S-M.
- **Equivalence:** emission §9 — hygiene is construction-fatal per harness; the wire strip + DTO validation remain the boundary; schema-side `additionalProperties:false` survives unconditionally (teaching carve-out); the kill switch's validated config and kill positions are unchanged.
- **Sequencing:** the retirement *decision* is now; deletion after the re-measure (~2026-11-24, corrected classifier per emission F8); **landing before wait-lifecycle's new board tools** skips them the mirror tax.

### 5. Busy-truth convergence, leg 1 — api hub read-edge consolidation
- **Source:** api-app #1 (F1+F7+F9).
- **Value:** the badge (`BusyAssignees`) and the alert evidence (`AssigneeQuiet`) must quote the same busy truth — today they agree by discipline and three suites, not construction (the max-of-two-identity-keys idiom ×5, the derived-busy distrust guard ×3 verbatim). One `hubFacts` resolver + one `derivedBusy` guard; the `framesWorkerAt`/`framesWorker` dual-map lockstep becomes unwritable-wrong (merge to one map); dead `QuietFor` rides out.
- **Risk:** low — pure refactor inside one file family; purity pins are behavioral. **Effort:** M.
- **Equivalence:** api-app §8 #1 — same maps under the same mutex, same per-surface contracts, byte-identical answers; the #2040/#2042/#2047 incident-pin tests ride.
- **Sequencing:** independent; also the pattern proof for initiative 8 (same class, one plane down).

### 6. Frontend credential-trio unification — close shipped user-facing divergences
- **Source:** frontend #3 (FC-5; FC-7 rides as the same pattern).
- **Value:** ~900-1,000 duplicated lines deleted and **four shipped divergences closed** — the org-tier delete with NO confirmation dialog (the `64507dc42` confirm migration skipped the trio), `parseInt` vs `Number.isFinite` validation drift on the same field, missing key-reveal toggle, and a tier with no update path (key rotation = delete+recreate). The in-repo exemplar (`McpServersTab`, one component × three scopes) proves the shape.
- **Risk:** medium — tier-specific blocks stay as slot props; per-tier specs keep rendering routed instances. **Effort:** M.
- **Equivalence:** frontend §7 #3 — scope-injected clients render identical markup for the shared 60%; the #432 commit is the done-right precedent.
- **Sequencing:** independent of all backend work. The unconfirmed org-tier delete is user-facing — the strongest urgency argument in the frontend set.

### 7. Engine-owned quiet facts — the #2047 alert world stops re-deriving the ladder's
- **Source:** api-app #2 (F2/R1); emission F6's predicate-mirror cure is the same principle board-side.
- **Value:** `quietBoardLookup`'s raw SQL re-encodes claim-set predicates, lease posture (stringly live/lapsed/none), and project scope — if the engine's semantics drift, the alert evaluator fires on a different world than the ladder acts on (the #2040 lesson verbatim). One read-only engine method; the narrow `handlers.QuietBoardLookup` interface is untouched.
- **Risk:** medium-low — one new engine read; land with the engine's own tests (#1837 P2a precedent). **Effort:** M.
- **Equivalence:** api-app §8 #2 — the fact set becomes the sweep's own candidate set by construction; nil-vs-empty honesty maps 1:1; `proxy_worker_silent_test.go` unaffected (it fakes the interface).
- **Sequencing:** after or with initiative 5 (both touch the #2047 family; either order works — 5 is hub-side, 7 is engine-side).

### 8. Busy-truth convergence, leg 2 — agentd kill-gating reads the authority
- **Source:** agentd #2 (F1/R1, R3 opportunistic).
- **Value:** the last read-side #2040-class divergence: the watchdog's busy-deferral and the session-aware restart decision gate *kills* on the tracker's second derivation while the sessionstate authority owns the #1574 definition. Divergence today is bounded by arithmetic (max-defer, UNKNOWN bounds), not shared truth. One authority read-edge (BusyOrUnknown with the tracker's conservative boot-window semantics + the per-session progress clock), then flip the two consumers.
- **Risk:** medium — boot-window conservatism (tracker answers pre-reseed); the #1576 steer scopes this to a read-edge, not a seam change. **Effort:** M-L.
- **Equivalence:** agentd §8 #2 — decision trees, thresholds, markers untouched; pinned suites re-pointed at the authority fixture.
- **Sequencing:** internal order per the audit — statusz floor opportunistically first, then watchdog-deferral, then restart-decision (one consumer at a time). After initiative 5 lands as the same-class precedent.

### 9. Frontend BoardPage extraction — move the freshest bug cluster out of the 2,629-line page
- **Source:** frontend #1 (FC-4) + #5's boardRules half (FC-8).
- **Value:** StageRail/StageCell + 4 inline dialogs (~950 ln) host the freshest bug cluster (orientation round-trip ×2, arrow-key hijack, snap drift — 3 fix commits in 5 days); extraction shrinks spec families from page-mounts to component-mounts. `boardRules.ts` completion makes the lifecycle allowed-map and search predicates unit-testable; the kebab `cardOverrides` staleness class (r2 `8e2f65255`) gets a documented `useReducer` transition.
- **Risk:** low (pure move, the 22-prop contract preserved verbatim, no renaming in the same change). **Effort:** M.
- **Equivalence:** frontend §7 #1/#5 — same JSX/props/DOM; rail/focus specs drive the same surface.
- **Sequencing:** independent; before initiative 10/12 only in the sense that it is the cheapest frontend win (any order works).

### 10. Frontend SessionActivityProvider split — end the re-render storm, make transitions explicit
- **Source:** frontend #2 (FC-6).
- **Value:** one ~1,050-line event-handler closure over ~11 state maps + ~17 ref maps; fresh context identity per render → every busy delta for any session re-renders every consumer (ChatPage consumes 12 selector hooks). The #1646 r3-r5 millisecond-ordering bugs were review-discoverable *because* transitions were implicit. A `useSyncExternalStore` busy-store + module-scope reducers makes the lease/flight transitions explicit and per-slice selective.
- **Risk:** medium — must preserve EventID-ordering semantics exactly; guarded by the strongest wall (9 spec families, 6,457 ln, asserting outcomes). **Effort:** L.
- **Equivalence:** frontend §7 #2 — same-signature selectors, same values for the same inputs.
- **Sequencing:** independent; ideally before further ChatPage extractions (12) so they land against the new store edges — but the audits support either order; sequence 10 → 12 if staffing allows one lane.

### 11. CI matrix phase 2 — builds, coverage-delta, canary
- **Source:** ci-release #2, #4, #5.
- **Value:** up to ~73 runner-min/PR from path-filtered per-component builds (49% of ci.yml spend today for never-pushed PR artifacts, plus the QEMU-arm64 flake class that reddened a docs-only head); ~11 min from demoting coverage-delta (informational comment, gates nothing); ~8.5 min recovered as a *real gate* by fixing the canary's 429 class (env-config kills both the 4×`sleep 65` and the flake) then flipping `continue-on-error` off.
- **Risk:** low-med — the build change shifts cross-arch break detection to merge+minutes (main-push + release.yml rebuild everything; no required-status-checks depend on PR build rows — stated trade, §4.6); canary blocking rides ~a week of clean nightly data. **Effort:** M.
- **Equivalence:** ci-release §6 #2/#4/#5 — per-component Dockerfile-break detection preserved; the 50% coverage floor stays in `test`; the canary keeps scenarios/assertions.
- **Sequencing:** after initiative 1 (the lane is proven); the canary flip last.

### 12. Frontend ChatPage — keyed session-state reducer, then staged machine extractions
- **Source:** frontend #4 (FC-1/2/3).
- **Value:** the r1/r3 reset-leak class (#1760: a workspace-A toast shadowing workspace B — one of nine parallel one-bit states missed its reset, twice) becomes structurally unreachable via a reducer keyed `(workspaceId, sessionId)`; then `useStuckSessionAutoAbort` (self-contained) and `useOptimisticTranscript` (the subtle 70-line gate with 3 shipped bugs) move byte-identical with their pins; the `hookcount` meta-test (a component needing a source-scanning test for its own shape — the shape IS the finding) retires with the early-return move.
- **Risk:** medium-high (the reconcile gate is the frontend's subtlest code — move-only, no semantic edits in the same change). **Effort:** L (one PR per step).
- **Equivalence:** frontend §3.2/§7 #4 — specs render ChatPage, not hooks; the 21-family wall (6,962 ln) enforces it.
- **Sequencing:** internal order fixed: reset-reducer → auto-abort → reconcile.

### 13. pkg/board ladder extraction + knob lattice + event-id standardization — **after wait-lifecycle lands**
- **Source:** board-engine #2, #5, #4 (B4+B5+B9; B7's shared candidate-predicate fragment rides when next touched; emission F6's exhaustion-predicate single-authority is the same class).
- **Value:** the 11-arm precedence becomes one readable ordered switch (8 companion maps keyed by victim id are a function in disguise); the next incident arm extends a 60-line pure function instead of a 600-line transaction body. The knob lattice closes the only config interaction that can produce a *wrong* posture (lapse tuned off the documented 2× relationship; default becomes derived, override kept) and states the 4×"3" cap family. Event-id minting standardizes on `engineEventID` — a mis-chosen discriminator today silently drops audit rows (`ON CONFLICT DO NOTHING`).
- **Risk:** low-med (mechanical, large diff; the nudge batch's locks-held-during-delivery invariant stays outside the extraction). **Effort:** M-L.
- **Equivalence:** board-engine §8 #2/#5/#4 — transaction body/SQL/ordering untouched; defaults byte-identical (`2×2700s=5400s`); every arm incident-pinned.
- **Sequencing:** **after wait-lifecycle engine work lands** — both are large diffs in `rc1Reclaims`; avoid running them concurrently. The exhaustion-predicate authority (emission F6) can land anytime as a small standalone (it touches reconciler + depose only).

### 14. Cross-surface small-machinery batch + documentation riders
- **Source:** api-app #4/#5 (F3, F4); agentd #3/#4 (F6, F8, F7-note, F11); api-app F9 rides 5.
- **Value:** one `alertCooldown` type replaces 4+6 near-twin methods across two maps+mutexes (the next alert arm inherits the discipline instead of copying it a third time); a boot-time warn makes the V2-dead-route posture (`v2Delivery` armed without terminus on the pinned harness → a 30 s wait at a 404) operational knowledge instead of archaeology; `admissionWatch.suspect()` (no callers) deleted; the restart-reason taxonomy's three inline strings hoisted to the consts; the sidecar's twin control-socket pollers merged (one loop, two stores).
- **Risk:** ~zero to low across the batch. **Effort:** S each.
- **Equivalence:** per audits — identical map semantics/cooldown floors; a log line at wiring time; an uncalled method; identical marker/metric bytes; same per-store staleness contracts.
- **Sequencing:** anytime; natural filler between the larger initiatives. Doc riders: agentd's loop/derivation maps, api-app's R4/F10 cross-links and notes land with their touching initiatives.

### 15. Release-train one-command cut + gitops-patch generation — **gated on #2092**
- **Source:** ci-release #3 (F5).
- **Value:** the ~10-step manual cut across two repos (manual CHANGELOG heading, Chart.yaml bumps, hand-copied semver tags + 3 digest pins transcribed from job logs) is the transcription-error class — an #1237 incident was exactly a wrong paste, and the CRD-never-upgrades trap bit twice (#2088 outage, #2092 class closure). `make release-cut VERSION=X` generates the cut commit + tag; a post-release step harvests the `merge-*` prints into a machine-readable gitops patch with digest-GET verification built in (reusing `release-preflight.sh` P1/P2). Humans move from transcribing to reviewing.
- **Risk:** medium — two repos, ops-repo credentials; mitigation is generation-not-application (the command never merges anything; human + AI review stay on both commits). **Effort:** L.
- **Equivalence:** ci-release §6 #3 — byte-identical artifacts, same gates in the same order.
- **Sequencing:** **HARD-gated on #2092's guard set merging first** (`deploy/flux/crds` mirror + `FluxCRDDriftCheck` + `make flux-sync-crds` — PR #2093 still open at this head); the automation consumes the guard rather than duplicating it.

## 6. Sequencing graph (what unblocks what)

```
now ──> 1 (CI quick wins) ──────────────> 11 (CI phase 2; canary flip last)
     ──> 2 (episode-reset fragment) ──┐
     ──> 4 (strict-args decision +    │   BEFORE wait-lifecycle implementation
          lockstep removals)          │   (designs merged PR #2101; impl stories next)
                                     └────────────┐
wait-lifecycle migration window: fold B2 (derive last_attempt_failed)
wait-lifecycle engine work lands ────────────────┴─> 13 (ladder extraction + lattice + event-ids)
+7 d corrected-classifier re-measure (~2026-11-24) ──> 4's code-deletion half
3, 5, 6, 7, 9, 12, 14: independent lanes (parallelizable)
5 (hub leg) ──pattern-proof──> 8 (agentd leg; internal: statusz → watchdog → restart)
10 before 12 preferred (store edges first), not required
#2092/#2093 merges ──HARD GATE──> 15 (release automation)
```

The charter's posed example — "a board-engine state collapse before or after wait-lifecycle lands" — resolves as: **fragment before, column-derivation within the same migration window, ladder extraction after, full collapse never (fenced, owner-gated phase 4).**

## 7. What the roadmap deliberately does NOT rank (and why)

- **The freshness seam (frontend FC-9):** the audits' own exclusion — it *adds* a domain-staleness signal and needs a product decision + backend cooperation. Recorded, not scheduled (§9).
- **Anything in the do-NOT-touch list (§8).**
- **Board-engine B7 beyond a when-touched fragment, B8/B12 twins, B13's comment-only, B14's seams:** proportionate per the audit; ride their next natural touch.

## 8. Do-NOT-touch list (armor with live incident provenance)

*Entry rule: live incident provenance AND no functional-equivalent simplification — the listed property is what the incident taught, and no consolidation preserves it.*

| # | Armor | Incident provenance (live) | Why no equivalent simplification exists |
|---|---|---|---|
| A1 | aged-arg v3 restructure (L2) | #1949/#1929 megastorm: 60/60 events pre-rollout → 0 post (fleet + exact-context replays, worklog 1261) | Mechanism-orthogonal to hygiene (rewrites tool-call *structure*; cannot know which keys are identity); removal resurrects the class hygiene cannot touch |
| A2 | Served-transcript identity hygiene (L6) | #2030 live adjudication (stamp persistence in served replay) | Flagless/no-silent-gaps is the design; any toggle creates silent-gap states nothing tests; construction-fatal per adapter is the posture |
| A3 | `boardWireBody` wire strip | the plugin still stamps every member call (68 post-boundary stamps measured) | Needed for as long as the plugin exists, regardless of model behavior |
| A4 | Renewal caps + #2044 heartbeat-orbit budget + the Heartbeat CASE decline | 2026-11-15 settle-stamped-as-movement; tsk-polished-keystone-waits presence-only-orbit | Merging the busy/no-advance counters trades a pinned asymmetry (resets differ on evidence class); the CASE decline semantics needed two review rounds — comment it, don't touch it |
| A5 | The 26-column state surface / the 22→8 fence | r2-finding-4 caught by a CHECK; the #2060 arbitration resolution rests on guarded single-statement UPDATEs | Columns ARE the replica-arbitration design; a blob trades loud drift for jsonb scaffolding + permanent wire shims (owner-gated phase 4 fence affirmed) |
| A6 | The 21 nil-inert Service seams + the one-function `Set*` wiring idiom | one seam per incident-driven source; #2047 R4 / #1959 discipline | Both audits' explicit best-pattern verdict; constructor injection was considered and refused |
| A7 | The 12 agentd healing loops | each traces to a named incident (#892, #1632, #1311, #1732, #2045…) | No purpose duplicates; kill sets deliberately narrow (#892's dead-listener-only ruling; pi's escalate-never-execute); the overlaps are world-model, fixed by initiative 8's read-edge, not by deleting loops |
| A8 | The notifier sealed-module fold | design 0071's explicit sibling-consumer ruling | A hook inside the sealed sessionstate module breaks the seal the ruling protects |
| A9 | The two silence clocks (`session_hung` vs `worker_silent`) | #1685 two-window gate; #2040/#2047 frame evidence | Different populations, different evidence classes; only the surrounding machinery unifies (initiative 14) |
| A10 | Wrap-up engine explicit columns + recursion cap + ack loop | unclosed-epics/reviewer-recursion class (design 000059 D1/D2) | D2 ruling: never derive from row existence; pinned end-to-end |
| A11 | The lexicon-not-machine boundary (no states column; `pending_input` stays flag) | 2026-11-15 audit ruling | Vocabulary for alert surfaces; a stored machine re-opens every arbitration question A5 closes |
| A12 | The delivery-ledger FSM + its duplicate-storm guards | #1288 five-identical-turns, #1292b, #1315 — each closed *inside* the machine | Monotone by construction; the guards are the incidents' resolutions (compaction — initiative 3 — is wiring, not semantics) |
| A13 | Populated-migration discipline + CHECK constraints (`tasks_pending_origin_set` etc.) | 000052's live-data rehome cautionary tale; r2-finding-4 | The physical invariants that make drift LOUD; a review net caught one, the CHECKs catch the rest |
| A14 | The AI-review REVIEW-LEDGER merge gate | process-critical (the merge gate itself); #80's 8/8 rejection burn documented | Parser *tolerance* additions only (cross-repo, ai-workflows); removing the gate removes the process |
| A15 | `recoverInjectedDestination` (L4) | 2 live parseable losses 2026-11-14 | The stack's only *recovery* layer; partially redundant with messageFile but the inline residual remains; costs ~nothing |
| A16 | The #2042 two-class frame allowlist + hub's single opencode-knowledge holding | #2040 five divergent alive-derivations (the founding incident) | The hub IS the poor-facts seam done right; consolidation happens *inside* it (initiative 5), never around it |

## 9. Functionality-reducing findings — OUT of scope by charter (flagged)

| Finding | Why out |
|---|---|
| frontend FC-9 (freshness/domain-staleness seam) | Adds functionality and needs a product decision + backend cooperation — the audit itself excluded it from candidates |
| ci-release honorable: drop `test-full` from the release path | Reduces tag-path verification; owner ruling territory (the .15 linter-drift catch is counter-evidence) |
| ci-release F11: posture-gate path narrowing | Reduces gate breadth chosen by design-0061 §11 ruling-3 — proposal recorded, owner sign-off required, NOT scheduled |
| ci-release #5 fallback: canary moved to nightly *instead of* fixed+blocking | The variant reduces PR-path coverage; the primary (fix 429 → block) is scheduled instead |
| api-app F4: deleting the V2 promotion-wait leg | Alive-by-design (design 0055 M4 flag matrix); only the boot-warn is scheduled |
| agentd F7: removing the lease-window idle/ERROR backstop arm | Dead-in-production but alive for test topologies; annotate, don't delete |
| board-engine B15: removing `pending_origin='dependency_abandoned'` | Wire-window territory; the const comment already carries "historical only" |
| Initiative 11's build-matrix change (stated trade) | Not functionality loss but a detection-window shift — called out in §4.6 so the owner can decline with eyes open |

## 10. Explicit non-goals

No code, workflow, Makefile, chart, flag, or schema changes were made — this document, its worklog, the CHANGELOG entry, and the COORDINATE row are the only artifacts. Nothing here exercises the owner's phase-4 gate, the #1576 steer, or any audit's own verdicts: where this synthesis ranks, it ranks the audits' proposals; where it adjudicates, it cites their evidence. The six sibling audit PRs (#2094-#2099) were open at this head; their findings docs are cited by path and will land with them.
