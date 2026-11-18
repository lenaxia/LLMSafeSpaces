# The model-emission defense stack — complexity audit

**Task:** tsk-dual-tanker-ambles (AUDIT: emission-defense stack — total defense-in-depth vs diminishing returns, interaction/redundancy cases, replay-harness CI cost vs bug rate, residual risk model, rollback/flag surface)
**Date:** 2026-11-18 (r3 = measurement-correction round; see §Method). **Base:** origin/main `b1987029b` (rebased to `9f74d3020`, the 0.44.17 release head). **Method:** read-only — layer source reads (relay-router, workspace-agentd, pkg/board, pkg/agent), git archaeology for the #1849/#2024/#2030 lineage, the in-repo #2030c baseline (worklog 1278; the companion evidence file `reports/evidence/tsk-visioned-estate-stencils-emission-rate.md` is **pod-local** at `/workspace/reports/evidence/…`, not committed to this repo), and a post-hygiene emission measurement on this pod's harness DB (read-only `sqlite3 mode=ro`).
**r3 correction (binding — round-3 board review):** the r1/r2 measurement classified key-form parts against the **current** story-named stamp set; the 272 "true emissions" are **pre-rename plugin stamps on legacy task names** (all four `*_task` names sat in the plugin's `INJECTED_SESSION_TOOLS` before the #2005 story rename, `33a569efe~1`), and the r1 day-buckets carried a `llmsafespaces_`-prefix extraction bug. The #2030c baseline's 272 (worklog 1278 A2) shares the rename artifact. This round re-derives everything under the **corrected classifier**: record-time set membership (pre-10-30: task-named members; after: story names; session tools constant) and a **strict bleed rule** (param value is *exactly* a bare session id — substring matches are polluted by legitimate code content and identity params). **No code changes** (this document is the only artifact).

---

## 1. Executive summary

- The stack is **7 distinguishable defense layers**; five shipped in a four-day incident window (2026-11-13 → 11-16) atop two October foundations (the #1849/file-crud discipline and the 2026-10-29 busy-renew cap). Each layer has live incident evidence; **none was shipped speculatively**. Total dedicated CI cost is trivially small (**~3.6 s of `go test` + a 0.06 s in-CI probe per run**, measured); the two 1949 replay scripts and the liveprobe cost **zero CI** (operator-run, env-gated).
- The layers resolve into **three orthogonal threat classes**. Orthogonality here rests on **mechanism + independent evidence streams**, not on a single DB timeline: v3's megastorm cure is proven by fleet data and exact-context replays (60/60 → 0 at rollout, worklog 1261); hygiene's contribution is proven by the served-replay mechanism and the death of the *observable* emission traces. An earlier round claimed "120 emissions after v3 was live" from DB day-buckets — that number was a classification artifact (plugin stamps, not emissions) and is **retracted** (§7).
- The DB's honest bound is stronger than the r1 story: **key-form emissions on non-member surfaces are 0 ALL-TIME on this pod — every era, pre- AND post-hygiene**. The only directly observable model-emission traces (non-member key-form; strict semantic bleed) never fired after 10-31. The #2030c flip gate's distinguishable target class was **empty before hygiene ever shipped**.
- Exactly **one layer is redundant in its observed evidence classes**: the #2030c strict-board-args dispatch gate (`AGENTD_STRICT_BOARD_ARGS`) — shipped default-OFF with a flip decision explicitly gated on "the post-#2030a-hygiene residual emission rate". Under the corrected classifier the gate's class was empty in *every* era (0 all-time non-member emissions), so the flip decision resolves to NO on stronger grounds than the r1 delta. Its three-site lockstep (plugin stamp set ↔ 41-tool mirror ↔ parity test) is the largest ongoing maintenance tax in the stack.
- One further defense is **partially redundant** (destination-injection recovery's send_message arm — the fragile long-inline-body shape it recovers from is obviated at source by `messageFile` for compliant bodies) but retains residual value on the still-inline surfaces.
- The rollback surface is **two runtime env switches across two processes** (`BYO_AGED_ARG_RECENT_TOOL_CALLS` kill switch; the never-flipped strict flag) plus one untested tuning knob; hygiene, messageFile, the wire strip, and the placeholder guards are **flagless by design**. A meta-flag governing them all is technically feasible and the wrong move — see §6.
- **Residual, unmitigated classes remain real**: long escaped-JSON string emission on surfaces with **no file-mediated path** (`workflow_create`'s `specYaml`, trigger prompts — the exact #2030b shape, un-mitigated there), agedarg's verbatim-passthrough anomaly edge, and the fundamental unobservability of member-surface emissions (the plugin stamp overwrites any model copy — the DB can never attribute a member-surface key; only non-member key-form and bleed are measurable). The stack's residual risk is no longer in the classes it keeps stacking against — it is in the surfaces the pattern never reached.

---

## 2. The stack, layer by layer (origin-traced, costed)

| # | Layer | Origin / date | Mechanism (one line) | Incident evidence (live) | CI cost (measured) | Rollback |
|---|-------|---------------|----------------------|--------------------------|--------------------|----------|
| L1 | Board payload file-crud + #1849 placeholder scan + long-string-last schema discipline | design 0070, #1849 (Oct) | the model never *emits* long board JSON — payloads ride `bodyFile`/`planFile`; placeholder-token and empty-create refusal; schemas order long strings last | the #1929 12/12 runaway class (bulk-first key order); orchestrator corruption 2026-10-25 | bundled in agentd suite | none (flagless) |
| L2 | **aged-arg v3 restructure** — `cmd/relay-router/agedarg.go` (617 ln) | #1949/#1929, PR **#2024**, 2026-11-13 (`a87c7312d`) | rewrites ALL prior `tool_calls` in the request body as prose bullets — the tiling template leaves model context; lossless; deterministic (cache-stable) | megastorm 60/60 pre-rollout events → **0 post-rollout** (worklog 1261); myriad 1/1→3/3 clean, quick-panda 3/3 | 0.47 s (`-run AgedArg`, 25+7 test funcs) | env `BYO_AGED_ARG_RECENT_TOOL_CALLS` (-1 kill / 0 validated / >0 untested) |
| L3 | **Opaque renewal caps + lease ladder bounds** — `pkg/board/reconciler.go:266` `boardBusyRenewCap=3` (2026-10-29 `95e5450ab`), #2044 heartbeat-orbit budget (`noAdvanceAtCap`, 2026-11-15 `a9ad956b4`), opaque-delta zero-renewal-authority (`board_liveness.go`), depose-guard parity (`depose.go:287`), busy TTL (`busy.go`) | rc1 + #2044 | a wedged/emission-stuck worker cannot hold a lease forever: busy-only renewals cap at 3, opaque telemetry has zero renewal authority, motionless heartbeats demote every renewal arm | 2026-11-15 incident (settle stamped as movement); 2026-10-30 busy-badge spin | bundled in pkg/board suite | compile-time consts (not runtime-switchable) |
| L4 | **Destination-injection recovery** — `recoverInjectedDestination`, `mcp_tools.go` | #1530 vocab, generalized in PR **#2032** (`09428c81f`, 2026-11-14) | parseable emission-corrupt destinations recovered: split at first quote, re-validate leading ref, deliver with loud warning | 2 live parseable losses 2026-11-14 (04:59/06:05) | bundled in agentd suite | none (additive recovery path) |
| L5 | **messageFile bodies** — `send_message_file.go` (190 ln) | **#2030b** (`19f528065`, 2026-11-16) | long/complex message bodies are emitted as a SHORT file path; platform reads the body from /exchange — the fragile long-escaped-string shape leaves the emission surface | the #2030 unparseable-storm subclass (medium+ bodies at 3rd+ call position) | 2.74 s (with strict+placeholder batteries, 13 test funcs here) | none (additive schema; inline `message` stays first-class) |
| L6 | **Served-transcript identity hygiene** — `pkg/agent/transcripthygiene.go` + opencode/pi adapters | **#2030a**, PR **#2059** (`34580e5cf`, 2026-11-16) | strips `lsp_injected_session`/`sessionId`/`session_id` from the SERVED transcript view (stored record intact) — the replay-continuation *source* disappears; construction-fatal per harness (`TranscriptHygieneFor`) | the #2030 live-instance adjudication (stamp persistence in served replay); the #2030c baseline's gauntlet-session extras (method-caveated, §7) | 0.09 s across all 3 pkgs | **none — by design** (no-silent-gaps posture) |
| L7 | **Strict board args** — `strict_board_args.go` (313 ln) | **#2030c**, PR **#2070** (`a5936e340`, 2026-11-15/16) | accept-side teaching errors for emitted identity keys + unknown fields; schema-side `additionalProperties:false` when on | its motivating 272-emission baseline **shares the rename artifact** (§7) — the gate's distinguishable class was empty all-time | part of the 2.74 s battery (7 test funcs) | env `AGENTD_STRICT_BOARD_ARGS` (default OFF, never flipped) |

Standing seams that are *not* emission defenses but close gaps the above would otherwise leave: the accept-side **wire strip** (`boardWireBody`, `board_manager_tools.go:178` — the plugin's own stamp must never ride the platform wire) and the **send_message placeholder guard** (`mcp_tools.go:1884`, #1814). Both flagless.

---

## 3. Interaction analysis — what covers what

**The three threat classes and their cures:**

1. **Structural tiling / megastorm** (#1949/#1929): model pattern-continues its own tool-call JSON. Cured by **L2** (template removed from context). Dead since 2026-11-13; zero recurrence — fleet data and exact-context replays (worklog 1261), independent of the DB-classification question below.
2. **Identity-stamp continuation** (#2030 mainline): model replays `lsp_injected_session` it saw in served history into new calls; bleed condition replaces a semantic value. Source killed by **L6**; wire-side containment by the `boardWireBody` strip; would-be accept-side rejection by **L7**.
3. **Long escaped-JSON string fragility** (#2030 residual): emission breaks mid-string at 3rd+ position, injecting fragments into adjacent params. Shape removed by **L5** for message bodies; parseable subclass recovered by **L4**; **not addressed on workflow/trigger surfaces** (§5).

**Orthogonality — what the evidence actually supports (r3-corrected):** the two transforms remove *different features of model-visible history*: v3 rewrites the whole tool-call **structure** (any tool, any args — the tiling template), hygiene removes **3 identity keys** from the served view. Neither can subsume the other *by construction*: v3 renders request bodies and would carry a stamp onward as bullet *text* (it cannot know which keys are identity stamps); hygiene does not restructure tool-call JSON (a tiling template served clean of stamps is still a template). On the evidence side the streams are independent: the megastorm died at v3's rollout (fleet, 60/60→0) *before hygiene existed*; the observable emission traces on this pod (non-member key-form, strict bleed) are zero after 10-31 in **every** regime (§7) — so no DB timeline can or should be used to sequence the two cures. An earlier round's "120 emissions after v3 live" was a classification artifact (plugin stamps mis-bucketed by a name-prefix bug) — retracted; the orthogonality argument stands on mechanism + the two independent streams, not on day-buckets.

**Does strict + hygiene + file-bodies make earlier layers redundant?** No — with one exception (§7). Specifically tested:

- *Hygiene vs v3*: different features of history (3 keys vs the whole tool-call structure). Removing v3 would resurrect the megastorm class — the class hygiene's own mechanism cannot touch (no key-stripping removes a structural template).
- *messageFile vs recovery (L5 vs L4)*: partial overlap — see §7.4.
- *Wire strip vs hygiene*: the plugin still stamps every `INJECTED_SESSION_TOOLS` call at the boundary (**68 post-hygiene-boundary stamps** — the injection is alive; only the *served replay* is clean). The wire strip remains load-bearing for as long as the plugin exists.
- *Renewal caps (L3) vs everything*: different threat entirely (blast radius of a wedged worker, not emission prevention). No interaction; no redundancy.

---

## 4. The replay harness surface — CI cost vs current bug rate

**Inventory:** `scripts/1949-aged-arg-replay.py` (685 ln: 3 arms — neutral/poisoned/compacted; 7 scorer signatures — duplicate keys, degenerate fragment tiling, **key repetition ≥10x**, malformed args, native-markup leak, splice-artifacts-valid-JSON, size blowup; the script's own header still lists "multi-call", but its scorer treats multi-call turns as legal), `scripts/1949-build-exact-context.py` (exact-context builder), and `scripts/1530-corrupt-args-liveprobe.py` (live emission probe) — these three are **env-gated operator instruments** (`REPLAY_BASE_URL`/`REPLAY_API_KEY`/`REPLAY_MODEL`) with **no CI references**. The fourth, `scripts/1530-arg-mutation-probe.mjs`, **is wired into CI** (`ci.yml:654`, `node scripts/1530-arg-mutation-probe.mjs 2000` on every PR): an argv-driven offline falsification probe for the plugin arg-mutation race (the #1530 incident class), measured **0.06 s** — it guards against *reintroduction* of a live-code race, not a dead emission class, and is not part of the freeze below.

**What CI actually pays:** `TestAgedArgRenderMatchesReplayHarness` (runs the script's `--print-stub`, byte/semantic-parity pin) — **0.20 s**, runs in `-short`; the in-CI `1530-arg-mutation-probe.mjs` — **0.06 s** (live-race guard, kept). The Go battery protecting the layers: agedarg 0.47 s, agentd strict/send/placeholder 2.74 s, hygiene 0.09 s — **~3.6 s total per run** (0.20 + 0.06 + 0.47 + 2.74 + 0.09). The cost of the harness is therefore **not cycles but coupling**: any change to the v3 render forces the python mirror to move in lockstep (the parity test enforces it), keeping a #1949-class instrument alive against a class with **zero events in 5 days post-rollout and a dead mechanism** (v3 removed the template the arms poison).

**Verdict:** the harness has already paid for itself (it proved the cure pre-rollout and pinned v1/v2 failure modes). Its recurring value is now the render-parity pin, not the live arms. Freeze the arms; keep a slim pin (Top-5 #2).

---

## 5. Residual risk model — what remains unmitigated

| Residual class | Status | Evidence |
|----------------|--------|----------|
| **Long escaped-JSON strings on non-file-mediated surfaces** — `workflow_create.specYaml` (stringified DAG), trigger prompts, agent-node handler source | **OPEN — the #2030b shape, un-mitigated where it lives today** | #2030b's own adjudication: "long-prose emission inside a JSON string is the fragile shape"; the file-path pattern was applied to send_message and board payloads only |
| **Member-surface emissions are unobservable, forever** | structural | the plugin overwrites `lsp_injected_session` unconditionally on member tools — a model-emitted copy cannot survive to the DB. The measurable traces are only: non-member key-form (**0 all-time**) and strict semantic bleed (**1 all-time, 10-31**). Any future emission-rate claim must state which trace it measured (§7, F8) |
| Aged-arg verbatim-passthrough edge | accepted, documented | any span anomaly → forwarded with `tool_calls` intact (lossless-by-construction choice); the template survives for that span. Window-0 + clean spans is the validated posture; no observed event |
| De novo (non-replay-sourced) emissions | unobserved | 0 non-member key-form events in any era; with strict OFF, `additionalProperties` schemas would accept such fields silently until a semantic param is hit |
| Placeholder-class emissions off-guard surfaces | narrow | guarded on board CRUD + send_message; other free-text surfaces (workflow notes, piggy-bank notes…) accept silently |
| Direct-to-provider paths without the relay | bounded | tiling reproduces without relay (proven); only hygiene/agentd-side layers cover such paths — acceptable since history is served through hygiene-covered adapters |

**What we keep stacking against that is already dead:** stamp-continuation hardening beyond hygiene (strict-args flip, further accept-side teaching) — the only distinguishable class was empty even pre-hygiene; megastorm-class instrumentation — the mechanism (tool-call template in context) cannot recur while v3 runs; gauntlet-pod strict-mode enablement — moot (§7.5).

---

## 6. Rollback / flag surface — inventory and the meta-flag question

| Switch | Location | Default | Class |
|-------|----------|---------|-------|
| `BYO_AGED_ARG_RECENT_TOOL_CALLS` | relay-router per-process env (`byo_run.go:69`) | 0 | **the one genuine kill switch** (-1); the `>0` window is an untested knob |
| `AGENTD_STRICT_BOARD_ARGS` | workspace-agentd env (`strict_board_args.go:59`) | OFF | rollout flag, **never flipped**; decision gate now answerable (§7.1) |
| hygiene / messageFile / wire strip / placeholder guards / renewal caps | — | on | **flagless by design** (or compile-time consts) — cannot be misconfigured off; hygiene is construction-fatal per adapter |

**Could one meta-flag govern them?** Technically yes (one helm value plumbed to two processes) — and it would be a mistake: (a) the flagless layers are a *feature* (the #2030a no-silent-gaps posture and the additive-rollout byte-identity discipline both exist so there is no configuration state to get wrong); (b) the strict flag's correct destiny is **removal**, not governance; (c) an umbrella switch creates partial-coverage states (relay off, agentd on) that no test matrix covers — a new wedge class. The right meta-move is **shrinking the surface to one kill switch** (agedarg — the only layer that rewrites every request body, i.e. the only blast radius that warrants a runtime switch) by retiring the strict flag and collapsing the window knob (Top-5 #1, #4).

---

## 7. REQUIRED — defenses now redundant given later layers

**Post-hygiene emission data (the evidence, r3-corrected classifier):** measured on this pod's harness DB (`/workspace/.local/opencode/opencode.db`, read-only; **read at 2026-11-18T01:20:06Z**), boundary = 2026-11-17T20:00Z (agentd restart on the hygiene binary — `/agentd/.../workspace-agentd` built 2026-11-17 15:46, carries `pkg/agent/opencode.StripIdentityStamps`). Classifier: **record-time set membership** — a part's tool name is checked against the plugin set *as it stood when the part was recorded* (before the #2005 story rename `33a569efe`, 2026-10-30, the set carried the `*_task` names — verified against `33a569efe~1`'s plugin; after it, the story names; the session tools are unchanged). **Bleed rule: strict** — the param's value is *exactly* a bare session id (`^ses_…$`) in a non-identity param; substring matches are unusable (legitimate code content and identity params pollute them — this is why r1's "23 bleed" and worklog 1278's "13" are not reproducible: both used substring-class detectors).

**The DB's structural limit (stated first, because every number below sits under it):** on member surfaces the plugin's unconditional overwrite makes stamp-vs-emission **unattributable** — a model-emitted `lsp_injected_session` on a member tool cannot survive to storage. The DB therefore supports exactly two observable emission traces: key-form on **non-member** surfaces, and strict bleed. Both are reported.

| Class (corrected) | All-time pre-boundary | **Post-boundary (5.3 h, 1,449 tool parts)** |
|-------|---------------------------|-----------------------------|
| Key-form on **non-member** surfaces (the only direct emission bound) | **0 — every era, including pre-hygiene** | **0** |
| Key-form on legacy `*_task` names (= pre-rename plugin stamps; members then) | 272 on the four names the round-3 review enumerated (`board_update_task` 215, `board_create_task` 46, `board_resume_task` 6, `board_pause_task` 5; 274 across all seven pre-rename task-named members), 100% carriage, **time-boxed 2026-10-26 → 10-30, ending at the #2005 rename** | n/a — names retired with the alias window |
| Key-form on current members (= the plugin's own stamps; the unattributable zone) | 587 | 68 — **the injection is alive, as designed** |
| Strict semantic bleed (value = bare session id in a semantic param) | **1** (2026-10-31, `send_message` `message`) | **0** |
| Legacy `sessionId`/`session_id` extras on non-declaring tools | 19 (corrected extractor; r1's "83" was the prefix-bug artifact) | **0** |

**r1/r2 numbers retracted:** "272 true emissions → 0" (rename artifact), "120 emissions after v3 live" (prefix-bug day-buckets that matched *all* key-form parts, i.e. plugin stamps), "23 bleed" / "13 baseline bleed" (substring detectors), "83 legacy extras" (broken extractor). The #2030c baseline (worklog 1278 A2's "272 model emissions on the legacy alias names, which the plugin does not stamp") shares the rename artifact — the plugin *did* stamp those names at record time.

### 7.1 `AGENTD_STRICT_BOARD_ARGS` dispatch gate (#2030c) — REDUNDANT; the case is now *stronger* than r1 argued
Shipped default-OFF with the flip decision "gated on the post-#2030a-hygiene residual emission rate". Under the corrected classifier the gate's **distinguishable** target class — emissions the platform's own stamp cannot masquerade as, i.e. key-form on non-member surfaces — was **never observed on this pod in any era: 0 all-time, pre- AND post-hygiene**. The flip decision resolves to NO not because hygiene cured a live class, but because the class the gate could uniquely catch was empty before hygiene shipped (hygiene then also removes the served-replay *source*, closing the mechanism). The strict bleed trace — the one observable member-surface emission signature — is 1 all-time (10-31), 0 post-boundary. Residual coverage the gate would still offer — de novo, non-replay-sourced emission — is doubly unobserved (0 non-member events in any regime). Redundancy is evidence-backed on the corrected bound. (The *schema-side* half — unconditional `additionalProperties:false` advertisement — has independent teaching value; see Top-5 #1's carve-out.)

### 7.2 The 41-tool stamp-set mirror + `TestStrictBoardArgs_PluginSetParity` — subsumed by 7.1
A three-site lockstep (plugin `INJECTED_SESSION_TOOLS` ↔ `boardStampSetTools` ↔ parity test) maintained so strict mode can distinguish platform stamps from emissions. The set counted **38** at #2030c ship time (both mirror sites at `a5936e340`; worklog 1278's "37" is its authored-at figure, pre-merge) and has since grown to **41** across two changes — `board_ack_epic_wrapup` via `e1fe96678` (the wrap-up-verdict story) and `board_move_stage`/`board_record_verdict` via `#2061/#2064` story 1 (PR #2086) — the growth itself is the lockstep tax working. With the gate retired the mirror goes with it; the *admitted-args derivation* (`boardToolAdmittedArgs`) is catalog-driven and would survive only if the schema-side carve-out is kept.

### 7.3 The legacy-alias teaching arms (`sessionId`/`session_id` on non-declaring tools)
Corrected count: 19 pre-boundary extras, **0 post-boundary** — and the pre-boundary events ride the same era as the legacy names. Dead class, twice over.

### 7.4 Partial — `recoverInjectedDestination`'s send_message arm (#1530/#2030)
The corruption shape it recovers (`TARGET","message":"…"}` splice) is emitted *while writing a long inline body* — exactly the fragility `messageFile` removes at source for compliant sends. Residual inline surface (short `message` bodies, other long-string params elsewhere) keeps the recovery **non-fully-redundant**: keep it, but its expected fire rate drops toward zero as `messageFile` adoption completes — it is the stack's only *recovery* layer and costs ~nothing.

### 7.5 The "enable strict on gauntlet-class pods" recommendation — moot
The interim recommendation predates the corrected bound. The post-boundary window includes board-worker traffic (68 member stamps — the population whose calls the gate would screen); zero non-member or bleed events fired.

**Explicitly NOT redundant (re-checked):** L2 aged-arg v3 (different class; mechanism argument §3 — and the megastorm evidence stream is independent of the DB classification question); the `boardWireBody` wire strip (the plugin still stamps — 68 post-boundary stamps — and the wire must stay clean regardless of model behavior); L3 renewal caps (different threat axis); L1849/file-crud discipline (shape prevention, still the outermost layer); placeholder guards (different corruption mode: mid-write substitution, not continuation).

---

## 8. Findings matrix (common format — aligned to `reports/audits/api-app-complexity.md` §7)

*Class: **P** = proportionate (keep, cost is earned), **C** = collapsible (consolidate/refactor, functional equivalence arguable), **D** = dead (dead-in-fleet or dead-in-principle).*

| ID | Finding | Evidence | Cost | Class | Recommendation | Risk of change |
|----|---------|----------|------|-------|----------------|----------------|
| F1 | Strict-args gate is redundant in its observed classes; the flip decision resolves to NO on the corrected all-time bound | §7 table: **0 non-member key-form all-time** (every era), 1 strict bleed all-time (10-31), 0/0 post-boundary; §7.1 | 313 ln + 7 test funcs + the 41-tool three-site lockstep (grew 38→41 across two changes in its first week) — the largest standing maintenance tax in the stack | D | retire flag + dispatch gate; optionally keep unconditional `additionalProperties:false` advertisement (Top-5 #1) | low-med — loses an unobserved de-novo backstop |
| F2 | The replay harness arms target a dead class; the recurring cost is Go↔python render lockstep, not CI | §4: 0 events in 5 days post-rollout; parity test 0.20 s; no CI wiring for the 1949 arms/liveprobe (the #1530 probe IS in CI and guards a live-code race) | every v3 render change forces a script edit (the parity test enforces byte lockstep) | D (arms) / P (parity pin) | freeze/archive the arms; pin format constants only (Top-5 #2) | ~zero |
| F3 | The `>0` window knob of `BYO_AGED_ARG_RECENT_TOOL_CALLS` is an untested behavioral branch on the most safety-critical transform | agedarg.go:69 "untested tuning knob — the validated setting is 0" | dead config weight + a config state nobody has validated | C | collapse to binary kill switch (Top-5 #4) | low |
| F4 | The identity-key set is comment-locked in three sites (`IdentityStampKeys` "single source of truth" vs `boardWireBody` hardcode vs `boardIdentityInlineKeys`) | transcripthygiene.go:44-45 "All three sites must stay in lockstep" | drift risk on the one set the whole #2030a family keys on | C | derive all three from `agent.IdentityStampKeys` (Top-5 #3) | ~zero |
| F5 | The #2030b file-mediation pattern was never extended to the remaining inline long-string surfaces (`specYaml`, trigger prompts) | §5; tool descriptions confirm inline-only | the exact fragile shape persists on workflow/trigger authoring | P (gap, not waste) | a *future* story, consciously scoped — not another stacked guard on a dead class | med (UX change) |
| F6 | Renewal-cap exhaustion predicate is mirrored in two sites (`reconciler.go:584`, `depose.go:287`) | both `busyRenews < boardBusyRenewCap` | the S5 depose guard must not drift from the ladder | C | single authority function (Top-5 #5) | med — lease semantics, needs the pin tests |
| F7 | Emission-rate evidence on this pod is now method-bound: the #2030c baseline's 272 is a rename artifact, and member-surface emissions are *structurally unobservable* (plugin overwrite) | §7 correction record; `33a569efe~1` plugin set; 100% carriage on legacy names | any future flip/keep decision that re-uses the baseline numbers inherits the artifact | D (the artifact) / P (the lesson) | corrected classifier spec (record-time membership + strict bleed rule) recorded in F8's re-measure | ~zero (documentation) |
| F8 | Post-hygiene window is 5.3 h / 1,449 parts — adequate for the flag-retirement decision (0 events on both observable traces) but small in absolute hours | §7 measurement record (read 2026-11-18T01:20:06Z) | over-claiming risk if stated without the window | n/a | re-measure at +7 d **with the corrected classifier** — record-time set membership incl. legacy alias resolution per `board_story_alias.go:47-56`, strict bleed rule (exact session-id values only) — before *deleting* strict-args code; the retirement decision itself needs no wait | n/a |
| F9 | Exactly two runtime switches exist; hygiene & co. are flagless by design — a meta-flag would add a partial-coverage state nothing tests | §6 | a new wedge class, not a simplification | P (current shape) | shrink to one kill switch rather than govern many | n/a (process) |

---

## 9. Top 5 simplification candidates (ranked value/risk)

### #1 — Retire `AGENTD_STRICT_BOARD_ARGS` (never flip; remove flag + dispatch gate; carve out the schema-side advertisement) — highest value
- **Value:** removes the 3-site lockstep (every new board tool currently must update plugin + mirror + parity test — it grew twice in its first week), 313 ln + 7 test funcs, and one of the stack's two runtime switches; closes the #2030c pending_input with the corrected bound instead of a config flip.
- **Functional equivalence:** the class the gate can *distinguish* (non-member key-form emissions) measured **0 all-time — every era** (§7.1); the member-surface class was always unattributable at the gate too (the stamp overwrites before the check runs — the gate's own design concedes this by keying on the original wire name); hygiene is construction-fatal per harness (no silent-gap path); the wire strip + platform DTO validation remain the boundary. The schema-side `additionalProperties:false` advertisement can survive *unconditionally* (byte-safe, teaches the vocabulary up front) if the teaching value is wanted without the dispatch gate.
- **Risk:** low-med — the de-novo emission backstop is forgone (unobserved class); mitigation: one +7 d re-measure with the corrected classifier (F8) before deleting code; the retirement decision itself needs no wait.

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

**Honorable mentions:** document L4's expected fire-rate as declining (§7.4) rather than touching it; a consciously-scoped F5 story for `specYaml` file-mediation (the one place *adding* a defense is still justified); re-measure post-hygiene at +7 days **with the F8 corrected classifier** and bank the result beside the corrected baseline numbers in this doc.

---

## 10. Explicit non-goals

No code, flag, schema, workflow, or chart changes were made in this audit — every candidate above is a proposal for a follow-up story. The measurement was performed read-only (`sqlite3` URI `mode=ro`) against this pod's harness DB. **Correction provenance (r3):** the round-3 board review independently re-derived the emission classification and found the r1/r2 numbers (272 emissions, 120 post-v3, 23 bleed, 83 extras) to be artifacts of current-set classification, a name-prefix extraction bug, and substring bleed-matching; this round reproduces the corrected results under record-time membership and a strict bleed rule (the four-name 272 total, the 100% carriage, and the 0-all-time non-member bound all re-verified locally: `33a569efe~1` plugin set enumerated; legacy task-name parts time-boxed 10-26 → 10-30; strict bleed scan = 1 event, 10-31). Lineage commits cited: `95e5450ab` (busy-renew cap), `a87c7312d` (#2024), `09428c81f` (#2032), `19f528065` (#2030b), `a5936e340` (#2070), `34580e5cf` (#2059), `a9ad956b4` (#2044 budget), `33a569efe` (#2005 rename).
