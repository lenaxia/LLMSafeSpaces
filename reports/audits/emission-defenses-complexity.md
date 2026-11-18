# The model-emission defense stack — complexity audit

**Task:** tsk-dual-tanker-ambles (AUDIT: emission-defense stack — total defense-in-depth vs diminishing returns, interaction/redundancy cases, replay-harness CI cost vs bug rate, residual risk model, rollback/flag surface)
**Date:** 2026-11-18. **Base:** origin/main `b1987029b` (rebased to `9f74d3020`, the 0.44.17 release head). **Method:** read-only — layer source reads (relay-router, workspace-agentd, pkg/board, pkg/agent), git archaeology for the #1849/#2024/#2030 lineage, the in-repo #2030c baseline (worklog 1278; the companion evidence file `reports/evidence/tsk-visioned-estate-stencils-emission-rate.md` is **pod-local** at `/workspace/reports/evidence/…`, not committed to this repo), and a fresh **post-hygiene emission measurement** on this pod's harness DB (read-only `sqlite3 mode=ro`; methodology identical to the baseline's). **No code changes** (this document is the only artifact).

---

## 1. Executive summary

- The stack is **7 distinguishable defense layers**; five shipped in a four-day incident window (2026-11-13 → 11-16) atop two October foundations (the #1849/file-crud discipline and the 2026-10-29 busy-renew cap). Each layer has live incident evidence; **none was shipped speculatively**. Total dedicated CI cost is trivially small (**~3.5 s of `go test` + a 0.06 s in-CI probe per run**, measured); the two 1949 replay scripts and the liveprobe cost **zero CI** (operator-run, env-gated).
- The layers resolve into **three orthogonal threat classes**, and the orthogonality is *proven on live data*, not argued: the 272 identity-stamp emissions include **120 that occurred after the aged-arg v3 cure was live** (my day-bucketed re-measure; v3 kills structural tiling, not key-continuation), and the megastorm class died under v3 (60/60 → 0) while key-continuation died only under hygiene (272 → **0 post-hygiene, 0 semantic bleed, 0 legacy extras over 1,015 post-deploy tool parts**).
- Exactly **one layer is now redundant in its observed evidence classes**: the #2030c strict-board-args dispatch gate (`AGENTD_STRICT_BOARD_ARGS`) — shipped default-OFF with a flip decision explicitly gated on "the post-#2030a-hygiene residual emission rate"; that measurement now exists and reads **zero**. Its three-site lockstep (plugin stamp set ↔ 41-tool mirror ↔ parity test) is the largest ongoing maintenance tax in the stack for a class that is dead at the source.
- One further defense is **partially redundant** (destination-injection recovery's send_message arm — the fragile long-inline-body shape it recovers from is obviated at source by `messageFile` for compliant bodies) but retains residual value on the still-inline surfaces.
- The rollback surface is **two runtime env switches across two processes** (`BYO_AGED_ARG_RECENT_TOOL_CALLS` kill switch; the never-flipped strict flag) plus one untested tuning knob; hygiene, messageFile, the wire strip, and the placeholder guards are **flagless by design**. A meta-flag governing them all is technically feasible and the wrong move — see §6.
- **Residual, unmitigated classes remain real**: long escaped-JSON string emission on surfaces with **no file-mediated path** (`workflow_create`'s `specYaml`, trigger prompts — the exact #2030b shape, un-mitigated there), agedarg's verbatim-passthrough anomaly edge, and non-replay-sourced de novo emissions. The stack's residual risk is no longer in the classes it keeps stacking against — it is in the surfaces the pattern never reached.

---

## 2. The stack, layer by layer (origin-traced, costed)

| # | Layer | Origin / date | Mechanism (one line) | Incident evidence (live) | CI cost (measured) | Rollback |
|---|-------|---------------|----------------------|--------------------------|--------------------|----------|
| L1 | Board payload file-crud + #1849 placeholder scan + long-string-last schema discipline | design 0070, #1849 (Oct) | the model never *emits* long board JSON — payloads ride `bodyFile`/`planFile`; placeholder-token and empty-create refusal; schemas order long strings last | the #1929 12/12 runaway class (bulk-first key order); orchestrator corruption 2026-10-25 | bundled in agentd suite | none (flagless) |
| L2 | **aged-arg v3 restructure** — `cmd/relay-router/agedarg.go` (617 ln) | #1949/#1929, PR **#2024**, 2026-11-13 (`a87c7312d`) | rewrites ALL prior `tool_calls` in the request body as prose bullets — the tiling template leaves model context; lossless; deterministic (cache-stable) | megastorm 60/60 pre-rollout events → **0 post-rollout** (worklog 1261); myriad 1/1→3/3 clean, quick-panda 3/3 | 0.47 s (`-run AgedArg`, 25+7 test funcs) | env `BYO_AGED_ARG_RECENT_TOOL_CALLS` (-1 kill / 0 validated / >0 untested) |
| L3 | **Opaque renewal caps + lease ladder bounds** — `pkg/board/reconciler.go:266` `boardBusyRenewCap=3` (2026-10-29 `95e5450ab`), #2044 heartbeat-orbit budget (`noAdvanceAtCap`, 2026-11-15 `a9ad956b4`), opaque-delta zero-renewal-authority (`board_liveness.go`), depose-guard parity (`depose.go:287`), busy TTL (`busy.go`) | rc1 + #2044 | a wedged/emission-stuck worker cannot hold a lease forever: busy-only renewals cap at 3, opaque telemetry has zero renewal authority, motionless heartbeats demote every renewal arm | 2026-11-15 incident (settle stamped as movement); 2026-10-30 busy-badge spin | bundled in pkg/board suite | compile-time consts (not runtime-switchable) |
| L4 | **Destination-injection recovery** — `recoverInjectedDestination`, `mcp_tools.go` | #1530 vocab, generalized in PR **#2032** (`09428c81f`, 2026-11-14) | parseable emission-corrupt destinations recovered: split at first quote, re-validate leading ref, deliver with loud warning | 2 live parseable losses 2026-11-14 (04:59/06:05) | bundled in agentd suite | none (additive recovery path) |
| L5 | **messageFile bodies** — `send_message_file.go` (190 ln) | **#2030b** (`19f528065`, 2026-11-16) | long/complex message bodies are emitted as a SHORT file path; platform reads the body from /exchange — the fragile long-escaped-string shape leaves the emission surface | the #2030 unparseable-storm subclass (medium+ bodies at 3rd+ call position) | 2.74 s (with strict+placeholder batteries, 13 test funcs here) | none (additive schema; inline `message` stays first-class) |
| L6 | **Served-transcript identity hygiene** — `pkg/agent/transcripthygiene.go` + opencode/pi adapters | **#2030a**, PR **#2059** (`34580e5cf`, 2026-11-16) | strips `lsp_injected_session`/`sessionId`/`session_id` from the SERVED transcript view (stored record intact) — the replay-continuation *source* disappears; construction-fatal per harness (`TranscriptHygieneFor`) | 272 true emissions / 13 semantic bleeds pre-hygiene (worklog 1278); gauntlet session 88/400 extras | 0.09 s across all 3 pkgs | **none — by design** (no-silent-gaps posture) |
| L7 | **Strict board args** — `strict_board_args.go` (313 ln) | **#2030c**, PR **#2070** (`a5936e340`, 2026-11-15/16) | accept-side teaching errors for emitted identity keys + unknown fields; schema-side `additionalProperties:false` when on | the 272/13 pre-hygiene baseline (its own motivation) | part of the 2.74 s battery (7 test funcs) | env `AGENTD_STRICT_BOARD_ARGS` (default OFF, never flipped) |

Standing seams that are *not* emission defenses but close gaps the above would otherwise leave: the accept-side **wire strip** (`boardWireBody`, `board_manager_tools.go:178` — the plugin's own stamp must never ride the platform wire) and the **send_message placeholder guard** (`mcp_tools.go:1884`, #1814). Both flagless.

---

## 3. Interaction analysis — what covers what

**The three threat classes and their cures:**

1. **Structural tiling / megastorm** (#1949/#1929): model pattern-continues its own tool-call JSON. Cured by **L2** (template removed from context). Dead since 2026-11-13; zero recurrence.
2. **Identity-stamp continuation** (#2030 mainline): model replays `lsp_injected_session` it saw in served history into new calls; bleed condition replaces a semantic value. Source killed by **L6**; wire-side containment by the `boardWireBody` strip; would-be accept-side rejection by **L7**.
3. **Long escaped-JSON string fragility** (#2030 residual): emission breaks mid-string at 3rd+ position, injecting fragments into adjacent params. Shape removed by **L5** for message bodies; parseable subclass recovered by **L4**; **not addressed on workflow/trigger surfaces** (§5).

**Orthogonality, on live data (not argument):** v3 (L2) shipped 2026-11-13; my day-bucketed DB scan shows stamp emissions on 11-13/14/15/16 of 5+9+32+74 = **120 events post-v3** — v3 renders args as `**k**: v` bullets and the *text-form* stamp does not trigger key-continuation, but neither does it prevent it. Hygiene (L6) deployed 2026-11-17 ~20:00Z → **0 events since**. Conversely, the megastorm class was already dead under v3 alone. Each cure is evidence-bound to exactly one class.

**Does strict + hygiene + file-bodies make earlier layers redundant?** No — with one exception (§7). Specifically tested:

- *Hygiene vs v3*: different features of history (3 keys vs the whole tool-call structure). Removing v3 would resurrect the megastorm class — the class hygiene demonstrably does not touch (120 events coexisted with v3's restructured-but-still-textual stamps).
- *messageFile vs recovery (L5 vs L4)*: partial overlap — see §7.4.
- *Wire strip vs hygiene*: the plugin still stamps every `INJECTED_SESSION_TOOLS` call at the boundary (**47 plugin stamps in the post-hygiene window** — the injection is alive; only the *served replay* is clean). The wire strip remains load-bearing for as long as the plugin exists.
- *Renewal caps (L3) vs everything*: different threat entirely (blast radius of a wedged worker, not emission prevention). No interaction; no redundancy.

---

## 4. The replay harness surface — CI cost vs current bug rate

**Inventory:** `scripts/1949-aged-arg-replay.py` (685 ln: 3 arms — neutral/poisoned/compacted; 7 scorer signatures — duplicate keys, degenerate fragment tiling, **key repetition ≥10x**, malformed args, native-markup leak, splice-artifacts-valid-JSON, size blowup; the script's own header still lists "multi-call", but its scorer treats multi-call turns as legal), `scripts/1949-build-exact-context.py` (exact-context builder), and `scripts/1530-corrupt-args-liveprobe.py` (live emission probe) — these three are **env-gated operator instruments** (`REPLAY_BASE_URL`/`REPLAY_API_KEY`/`REPLAY_MODEL`) with **no CI references**. The fourth, `scripts/1530-arg-mutation-probe.mjs`, **is wired into CI** (`ci.yml:654`, `node scripts/1530-arg-mutation-probe.mjs 2000` on every PR): an argv-driven offline falsification probe for the plugin arg-mutation race (the #1530 incident class), measured **0.06 s** — it guards against *reintroduction* of a live-code race, not a dead emission class, and is not part of the freeze below.

**What CI actually pays:** `TestAgedArgRenderMatchesReplayHarness` (runs the script's `--print-stub`, byte/semantic-parity pin) — **0.20 s**, runs in `-short`; the in-CI `1530-arg-mutation-probe.mjs` — **0.06 s** (live-race guard, kept). The Go battery protecting the layers: agedarg 0.47 s, agentd strict/send/placeholder 2.74 s, hygiene 0.09 s — **~3.8 s total per run**. The cost of the harness is therefore **not cycles but coupling**: any change to the v3 render forces the python mirror to move in lockstep (the parity test enforces it), keeping a #1949-class instrument alive against a class with **zero events in 5 days post-rollout and a dead mechanism** (v3 removed the template the arms poison).

**Verdict:** the harness has already paid for itself (it proved the cure pre-rollout and pinned v1/v2 failure modes). Its recurring value is now the render-parity pin, not the live arms. Freeze the arms; keep a slim pin (Top-5 #2).

---

## 5. Residual risk model — what remains unmitigated

| Residual class | Status | Evidence |
|----------------|--------|----------|
| **Long escaped-JSON strings on non-file-mediated surfaces** — `workflow_create.specYaml` (stringified DAG), trigger prompts, agent-node handler source | **OPEN — the #2030b shape, un-mitigated where it lives today** | #2030b's own adjudication: "long-prose emission inside a JSON string is the fragile shape"; the file-path pattern was applied to send_message and board payloads only |
| Aged-arg verbatim-passthrough edge | accepted, documented | any span anomaly → forwarded with `tool_calls` intact (lossless-by-construction choice); the template survives for that span. Window-0 + clean spans is the validated posture; no observed event |
| De novo (non-replay-sourced) emissions | unobserved | 0 events in fresh sessions pre-hygiene; 0 post. With strict OFF, `additionalProperties` schemas would accept such fields silently until a semantic param is hit — the residual the flip decision was guarding |
| Placeholder-class emissions off-guard surfaces | narrow | guarded on board CRUD + send_message; other free-text surfaces (workflow notes, piggy-bank notes…) accept silently |
| Direct-to-provider paths without the relay | bounded | tiling reproduces without relay (proven); only hygiene/agentd-side layers cover such paths — acceptable since history is served through hygiene-covered adapters |

**What we keep stacking against that is already dead:** stamp-continuation hardening beyond hygiene (strict-args flip, further accept-side teaching) — the source is gone; megastorm-class instrumentation — the mechanism (tool-call template in context) cannot recur while v3 runs; gauntlet-pod strict-mode enablement — moot (§7.5).

---

## 6. Rollback / flag surface — inventory and the meta-flag question

| Switch | Location | Default | Class |
|-------|----------|---------|-------|
| `BYO_AGED_ARG_RECENT_TOOL_CALLS` | relay-router per-process env (`byo_run.go:69`) | 0 | **the one genuine kill switch** (-1); the `>0` window is an untested knob |
| `AGENTD_STRICT_BOARD_ARGS` | workspace-agentd env (`strict_board_args.go:59`) | OFF | rollout flag, **never flipped**; decision gate now answerable (§7) |
| hygiene / messageFile / wire strip / placeholder guards / renewal caps | — | on | **flagless by design** (or compile-time consts) — cannot be misconfigured off; hygiene is construction-fatal per adapter |

**Could one meta-flag govern them?** Technically yes (one helm value plumbed to two processes) — and it would be a mistake: (a) the flagless layers are a *feature* (the #2030a no-silent-gaps posture and the additive-rollout byte-identity discipline both exist so there is no configuration state to get wrong); (b) the strict flag's correct destiny is **removal**, not governance; (c) an umbrella switch creates partial-coverage states (relay off, agentd on) that no test matrix covers — a new wedge class. The right meta-move is **shrinking the surface to one kill switch** (agedarg — the only layer that rewrites every request body, i.e. the only blast radius that warrants a runtime switch) by retiring the strict flag and collapsing the window knob (Top-5 #1, #4).

---

## 7. REQUIRED — defenses now redundant given later layers

**Post-hygiene emission data (the evidence):** measured 2026-11-18T00:15Z (4.25 h elapsed from the boundary) on this pod's harness DB (`/workspace/.local/opencode/opencode.db`, read-only), boundary = 2026-11-17T20:00Z (agentd restart on the hygiene binary — `/agentd/.../workspace-agentd` built 2026-11-17 15:46, carries `pkg/agent/opencode.StripIdentityStamps`; process up since 19:5xZ). Methodology identical to the #2030c baseline (worklog 1278; the companion evidence file is pod-local, §Method): tool parts with `state.input` maps, classified by stamp-set membership on the pre-alias tool name.

| Class | Pre-hygiene (all-time → boundary) | **Post-hygiene (4.25 h)** |
|-------|-----------------------------------|---------------------------|
| Plugin's own stamps (stamp-set tools) | 532 in scan (baseline: 834 all-time incl. prior pod) | 47 — **injection alive, as designed** |
| True model emissions (`lsp_injected_session` on non-stamp-set surfaces) | **272** (matches the baseline's 272 exactly — method parity confirmed); 120 of them *after* v3 was live | **0** of 1,015 tool parts (95% CI upper bound ≈ 0.30% vs pre-rate 1.53%) |
| Semantic value-bleed (`ses_…` value in another param) | 23 events, all pre-boundary | **0** |
| Legacy `sessionId`/`session_id` extras on non-declaring tools | 83 | **0** |

### 7.1 `AGENTD_STRICT_BOARD_ARGS` dispatch gate (#2030c) — REDUNDANT for its observed classes
Shipped default-OFF with the flip decision *"gated on the post-#2030a-hygiene residual emission rate"* (its own charter; worklog 1278 filed the pending_input rather than guess). The gate's answer is now in: **0/1,015 residual emissions, 0 bleed** — on the very pod class that produced the 272/13 baseline. Both rejection classes the gate teaches (identity-stamp emissions, unknown-field emissions *as a stamp consequence*) landed at zero with hygiene alone. The residual it would still catch — de novo, non-replay-sourced emission — was **never observed** even pre-hygiene in fresh sessions. Redundancy is evidence-backed, not inferred. (The *schema-side* half — unconditional `additionalProperties:false` advertisement — has independent teaching value; see Top-5 #1's carve-out.)

### 7.2 The 41-tool stamp-set mirror + `TestStrictBoardArgs_PluginSetParity` — subsumed by 7.1
A three-site lockstep (plugin `INJECTED_SESSION_TOOLS` ↔ `boardStampSetTools` ↔ parity test) maintained so strict mode can distinguish platform stamps from emissions. The set counted 37 at #2030c ship time (worklog 1278) and has already grown to **41** (`#2061/#2064` story 1 added `board_move_stage`, `board_record_verdict`, and siblings — the growth itself is the lockstep tax working). With the gate retired the mirror goes with it; the *admitted-args derivation* (`boardToolAdmittedArgs`) is catalog-driven and would survive only if the schema-side carve-out is kept.

### 7.3 The legacy-alias teaching arms (`sessionId`/`session_id` on non-declaring tools)
Target class measured **0 pre-hygiene in the current-pod window and 0 post** — dead twice over (the baseline itself recorded 0).

### 7.4 Partial — `recoverInjectedDestination`'s send_message arm (#1530/#2030)
The corruption shape it recovers (`TARGET","message":"…"}` splice) is emitted *while writing a long inline body* — exactly the fragility `messageFile` removes at source for compliant sends. Residual inline surface (short `message` bodies, other long-string params elsewhere) keeps the recovery **non-fully-redundant**: keep it, but its expected fire rate drops toward zero as `messageFile` adoption completes — it is the stack's only *recovery* layer and costs ~nothing.

### 7.5 The "enable strict on gauntlet-class pods" recommendation — moot
The baseline's interim recommendation predates the post-hygiene arm. The post window includes board-worker traffic (47 stamp-set calls — the population that historically emitted); zero fired.

**Explicitly NOT redundant (re-checked):** L2 aged-arg v3 (different class; 120 stamp emissions coexisted with it — it cannot cover them, they cannot cover it); the `boardWireBody` wire strip (the plugin still stamps — 47 post-window stamps — and the wire must stay clean regardless of model behavior); L3 renewal caps (different threat axis); #1849/file-crud discipline (shape prevention, still the outermost layer); placeholder guards (different corruption mode: mid-write substitution, not continuation).

---

## 8. Findings matrix (common format)

| ID | Finding | Evidence | Impact | Recommendation | Risk of change |
|----|---------|----------|--------|----------------|----------------|
| F1 | Strict-args gate is redundant in its observed classes; its flip-decision gate resolves to NO | §7.1 table: 272→0, 13/23→0, 0/83→0 post-hygiene (1,015 parts, 4.25 h) | 313 ln + 7 test funcs + a 3-site lockstep (41 tools and growing) maintained against a dead class | retire flag + dispatch gate; optionally keep unconditional `additionalProperties:false` advertisement (Top-5 #1) | low-med — loses an unobserved de-novo backstop |
| F2 | The replay harness arms target a dead class; the recurring cost is Go↔python render lockstep, not CI | §4: 0 events in 5 days post-rollout; parity test 0.20 s; no CI wiring for the 1949 arms/liveprobe (the #1530 probe IS in CI and guards a live-code race) | every v3 render change forces a script edit | freeze/archive the arms; pin format constants only (Top-5 #2) | ~zero |
| F3 | The `>0` window knob of `BYO_AGED_ARG_RECENT_TOOL_CALLS` is an untested behavioral branch on the most safety-critical transform | agedarg.go:69 "untested tuning knob — the validated setting is 0" | dead config weight + a config state nobody has validated | collapse to binary kill switch (Top-5 #4) | low |
| F4 | The identity-key set is comment-locked in three sites (`IdentityStampKeys` "single source of truth" vs `boardWireBody` hardcode vs `boardIdentityInlineKeys`) | transcripthygiene.go:44-45 "All three sites must stay in lockstep" | drift risk on the one set the whole #2030a family keys on | derive all three from `agent.IdentityStampKeys` (Top-5 #3) | ~zero |
| F5 | The #2030b file-mediation pattern was never extended to the remaining inline long-string surfaces (`specYaml`, trigger prompts) | §5; tool descriptions confirm inline-only | the exact fragile shape persists on workflow/trigger authoring | a *future* story, consciously scoped — not another stacked guard on a dead class | med (UX change) |
| F6 | Renewal-cap exhaustion predicate is mirrored in two sites (`reconciler.go:584`, `depose.go:287`) | both `busyRenews < boardBusyRenewCap` | the S5 depose guard must not drift from the ladder | single authority function (Top-5 #5) | med — lease semantics, needs the pin tests |
| F7 | Exactly two runtime switches exist; hygiene & co. are flagless by design — the meta-flag would add a partial-coverage state nothing tests | §6 | a new wedge class, not a simplification | shrink to one kill switch rather than govern many | n/a (process) |
| F8 | Post-hygiene measurement window is 4.25 h / 1,015 parts — adequate for the flag-retirement decision (0.30% CI upper bound vs 1.53% prior, parts-based) but small in absolute hours | §7 measurement record | over-claiming risk | re-measure at +7 d before *deleting* strict-args code (retiring the flag decision does not need the wait) | n/a |

---

## 9. Top 5 simplification candidates (ranked value/risk)

### #1 — Retire `AGENTD_STRICT_BOARD_ARGS` (never flip; remove flag + dispatch gate; carve out the schema-side advertisement) — highest value
- **Value:** removes the 3-site lockstep (every new board tool currently must update plugin + mirror + parity test), 313 ln + 7 test funcs, and one of the stack's two runtime switches; closes the #2030c pending_input with data instead of a config flip.
- **Functional equivalence:** the classes it rejected are measured dead at the source (§7.1); hygiene is construction-fatal per harness (no silent-gap path the gate would backstop); the wire strip + platform DTO validation remain the boundary. The schema-side `additionalProperties:false` advertisement can survive *unconditionally* (byte-safe, teaches the vocabulary up front) if the teaching value is wanted without the dispatch gate.
- **Risk:** low-med — the de-novo emission backstop is forgone (unobserved class); mitigation: one re-measure at +7 d (F8) before deleting code; the retirement decision itself needs no wait.

### #2 — Freeze the aged-arg replay harness as incident evidence; slim the parity pin
- **Value:** ends the Go↔python byte-lockstep maintenance on a dead class while keeping the instrument reproducible.
- **Functional equivalence:** the Go battery (32 test funcs) pins the transform; the pin reduces to the bullet-format constants (`- Called \``, `  - Result: `) the tests already assert; the archived script remains runnable for exact-context live replay if the class ever twitches.
- **Risk:** ~zero.

### #3 — Derive the three identity-key sites from `agent.IdentityStampKeys`
- **Value:** removes comment-enforced lockstep on the key set the whole #2030a/#2030c family keys on (F4).
- **Functional equivalence:** the same three strings, one definition, behavioral switch-cases untouched.
- **Risk:** ~zero (compile-time).

### #4 — Collapse the agedarg window knob to a pure kill switch
- **Value:** deletes the only untested behavioral branch (protected-window scan + per-span protection interactions) of the request-rewriting layer; makes the rollback surface binary.
- **Functional equivalence:** validated config (0) and kill (-1) unchanged; the removed path never ran in production.
- **Risk:** low — forecloses a tuning option its own comment distrusts.

### #5 — Single renewal-exhaustion authority for ladder + depose guard
- **Value:** `depose.go`'s refusal predicate cannot drift from the reconciler's honoring predicate (F6) — the S5 salvage lever's correctness currently rests on a mirrored expression.
- **Functional equivalence:** one function computing exhaustion, consumed at both sites; same inputs, same cap constant.
- **Risk:** medium — lease semantics; ride the existing pin tests and the #2044 budget suite.

**Honorable mentions:** document L4's expected fire-rate as declining (7.4) rather than touching it; a consciously-scoped F5 story for `specYaml` file-mediation (the one place *adding* a defense is still justified); re-measure post-hygiene at +7 days and bank the result beside the 2030c baseline.

---

## 10. Explicit non-goals

No code, flag, schema, workflow, or chart changes were made in this audit — every candidate above is a proposal for a follow-up story. The post-hygiene measurement was performed read-only (`sqlite3` URI `mode=ro`) against this pod's harness DB using the #2030c baseline's own methodology; the pre-boundary arm reproduces the baseline's published numbers (272 true emissions; bleed events of the same order) which is the method-parity check that makes the post-boundary zero comparable. Lineage commits cited: `95e5450ab` (busy-renew cap), `a87c7312d` (#2024), `09428c81f` (#2032), `19f528065` (#2030b), `a5936e340` (#2070), `34580e5cf` (#2059), plus `a9ad956b4` (#2044 budget).
