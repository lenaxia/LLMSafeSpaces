# Lease/nudge evidence core — high-level design (independent audit + the boring optimum)

**Status:** Design (2026-11-18) — produced by the independent adversarial audit chartered in
`/workspace/reports/charters/lease-architecture-audit-charter.md`. Every Part A verdict below
was reached against code and the incident record, not rubber-stamped; the design that follows
is for WHAT SURVIVED. The companion implementation contract is
`lease-evidence-core-low-level-design.md` (phases, SQL, born-red pins).
**Companions (compose, never contradict):** `claim-wait-lifecycle-{high,low}-level-design.md`
(PR #2101 — TYPED WAITS sit ABOVE this core; this doc does not redesign them and preserves
their §6 ladder-governs-death boundary verbatim), `seamless-lease-design.md` (owns the ladder
semantics — this doc amends it in exactly two places: the challenge measurement window §4 and
the clock consolidation §5), `claim-lifecycle-design.md` (#2060's port — the lexicon ruling and
the phase-4 owner gate stand), and GitHub issues **#2100** (due-marking dead/masked in prod),
#2040/#2042/#2043/#2044 (the incident chain this design closes).
**In-flight composition:** PR #2104 (typed waits), PR #2103 (salvage-prefer terminal), the
failed wait-event-broker story (`tsk-pristine-henna-loses`, stage=failed — needs re-arm,
flagged to the orchestrator, out of scope here), and the **un-filed** #2100 fix story (the
charter named `tsk-stirred-harbor-hums`; no such story exists on the board — Phase 1 below is
its scope and should be filed as such).
**Seam verification:** anchors verified against `0aedbaf0a` (origin/main, 2026-11-18).

---

## 0. Part A — findings verdict table (the audit of record)

| # | Orchestrator finding | Verdict | The evidence (adversarial standard: what would make it wrong) |
|---|---|---|---|
| F1 | Transport fragility: evidence rides side-channels the platform doesn't own (files on mixed-rw/ro volumes, in-band frames); that single weakness explains #2040, #2100-dead-path, AND the masking class | **PARTIAL** | The fragility is real and causal for two of three. **#2100-A CONFIRMED as transport:** the claim-holder registry persists at `/workspace/.opencode-board-workers` (`board_workers.go:36`), written by `record()` (`:81-101`) whose file append fails SILENTLY on the sidecar's ReadOnly `/workspace` mount (design 0070 §3a; `pod_builder.go:224-241` — `/exchange` is the ONE dual-RW subtree), while the emitting half hydrates once at process start (`:58`, `:115`) from the frozen file — the cross-process bridge was EROFS-dead on its only writer. The advance tail (`board_turn_ledger.go:63`) and the pi journal ride the same class. **#2040 PARTIAL:** in-band synthesized frames with no provenance were necessary (`board_liveness.go:207` stamped any event; usagestream `OnFrame` fires for every frame) but NOT sufficient — the incident also needed agentd's synthesis loop AND the renewal write stamping `last_progress_at` (a logic lie, fixed by #2043). **#2100-B REFUTED as transport:** the masking is the hub's first-observation zero-baseline plus the parking turn's own frames reading as `MovedSinceLastSweep` (`board_liveness.go:466-485`) — pure cursor semantics; it would mask over a perfect channel. What would make F1 wrong: a transport-unified explanation of masking — none exists. |
| F2 | Inverted primacy: lease is a timeout-watchdog over an event-shaped reality; events bolted on as accelerators; #2060's inversion ruled but under-implemented | **CONFIRMED** | #2060 principle 3 rules "event-driven entry, sweep as backstop" (`claim-lifecycle-design.md` Part 2 §3). Exactly ONE event entry class landed (#2065 turn-end due-mark) and it is (a) dead in prod (#2100-A — `board_turn_end.go:97-99` gates on a registry that is empty in the emitting half), (b) masked when it fires (#2100-B), (c) covering one entry class only. The measured 90m35s park→nudge (issue #2100's timeline) is pure watchdog latency — two lease windows consumed by timeout arithmetic an event should have collapsed. What would make F2 wrong: a working event path — the live evidence says there isn't one. Nuance kept: "events primary" does NOT mean removing the watchdog; the sweep+lease remains the convergence backstop BY DESIGN (#2065 R5). The finding's substance — the accelerator is unreliable, so the watchdog's latency is the real system — is exactly the live record. |
| F3 | Quantum mismatch: 45m leases vs minute-scale quanta make every failure cost ~an hour; post-#2042 trusted free worker-activity evidence enables a 5–15m lease with cheap observed renewals, collapsing detection to minutes with NO nudge engagement; prerequisite an airtight busy-gate | **PARTIAL** | The arithmetic CONFIRMED: defaults `board.go:132` (LeaseDuration 45m) × the ladder's shape — nudge rung extends by LeaseDuration (`reconciler.go:783-787`), wedge check at next expiry — give park→verdict ≈ 2 windows; the incident measured 90m35s exactly. Observed renewals are free post-#2042 (two-map allowlist, `board_liveness.go:118-119`, `:207-231`). **Refuted in two parts:** (1) "NO nudge engagement" — the nudge is not an accelerator to eliminate; it is challenge-before-kill due process (F6 keep-list; the owner's wedge ruling: even a thinking model responds). A shorter lease shortens the challenge's measurement window; it never removes the challenge. (2) "5–15m" — the busy-gate CANNOT be made airtight: mid-turn silent tool calls renew only via Busy, which is deliberately capped (`boardBusyRenewCap=3`, `reconciler.go:266`) because its failure mode — an authority latched busy-true over a dead harness — is real and live-fired (#2040's secondary; #2045's canary covers only the prompt path). Mid-turn coverage = cap × lease; at 5m that is 15m — below real build/test durations, re-opening the "sleep N && poll killed as wedges" incident class the busy-gate was built to close. Coverage arithmetic (§6) puts the honest floor at cap×lease ≥ ~60m ⇒ lease ≥ 20m at cap 3. Also: post-#2100-fix, park→nudge is event-driven (~seconds) regardless of lease length — the quantum governs only the wedge-verdict latency and the events-dead backstop, so the binding fix is the measurement-window decouple (§4), not a short lease. |
| F4 | Over-specified heartbeat: demanding a specific tool call is model-dependent; presence provable by any observed response; advance-detection + #2044's bound handle response-without-progress | **CONFIRMED, with a load-bearing dependency to rewire** | Model-dependence is real at the floor: at the no-evidence rung the heartbeat is the ONLY wedge detector (`reconciler.go:594-611`), so a grammar-ignorant worker that replies in prose is wedged there (saved at observed rungs by the worker-map ordinal, `board_liveness.go:491-505` — post-#2042 the ordinal IS presence-by-any-response). `observedProgressAt` IS trusted load-bearing TODAY: the response write stores it into `last_progress_at` (`stage.go:795-810`), which feeds #2043's lapse arm (victims-query predicate, `reconciler.go:319-320`) and #2044's motion detector (`$2 > stage_entered_at` in the same response write). So the shrink is viable ONLY if the motion signal migrates to platform-observed advance evidence — which is strictly more honest, since a worker-declared watermark is exactly the orbit's fuel (a declared-fresh stamp reads as motion and resets the budget; the lying class is open today by design). The design (§5) performs that migration; the heartbeat tool survives as the explicit portable fast-path, its watermark duty retires. |
| F5 | State sprawl: two clocks + five counters + nudge-slot sprawl is where the incidents hid; the #2060 ClaimEvidence collapse is right and keeps being deferred | **PARTIAL** | Sprawl CONFIRMED by write-site enumeration: the claim-evidence column set on `tasks` is now `lease_expires_at, last_progress_at, last_renewed_at, nudge_id, nudged_at, last_heartbeat_at, busy_renews, no_advance_renews` (+ `pending_input/pending_origin`, `paused`, `salvage`, `stage_entered_at` anchors; migrations 000052/53/55/57), written across ~30 guarded UPDATE sites in 15 files (reconciler 15, stage 9, engine 4, plus depose/pause/persona/rearm/review/service/stamp/suspend/wrapup); the episode-reset discipline is hand-replicated at every mint — PR #2104's wait-clearing had to enumerate the same sites again. The incidents lived exactly in column semantics (#2043's conflation, #2044's missing counter, #2100-B's cursor baseline). The COLLAPSE AS A JSONB STRUCT is NOT validated: the 2026-11-15 audit ruling keeps phase 4 behind its owner go/no-go gate (`claim-lifecycle-design.md` Part 5), and the columns exist as columns for a real reason — the victims scan computes lapse/motion predicates SERVER-SIDE in one clock domain under `FOR UPDATE SKIP LOCKED` (`reconciler.go:314-329`), a discipline a blob would erode. The honest answer is semantic consolidation now (§5: two clocks → one platform-owned advance clock, declared watermark retired, three file artifacts deleted) and the struct collapse explicitly sequenced OUT with reasons (§5.3) — one answer, not two. |
| F6 | Keep-list: challenge-before-kill, epoch guards, bounded trust per weak channel, salvage-preference, ladder-governs-death survive any redesign | **CONFIRMED** | All five are load-bearing in code and preserved verbatim here. Challenge-before-kill: the wedge requires a positioned sentinel plus zero post-sentinel evidence on both channels (`reconciler.go:648-664`). Epoch guards: mint-time slot resets (engine claim paths) and the due-mark's dual-side epoch/mint guards (`due_mark.go:44-53`). Bounded trust: busy cap, opaque zero-authority (the capped variant was rejected round-1), heartbeat budget. Salvage-preference: S5 depose machinery + in-flight PR #2103. Ladder-governs-death: PR #2101 §6 — waits never carry renewal authority. This design's §4 exercises the keep-list actively: shortening the measurement window is legal precisely BECAUSE the challenge survives it; F3's "no nudge engagement" is vetoed BY this keep. |

**Stop-and-ask determination (charter Part C.4):** no verdict REFUTES a direction. The two
material qualifications — F3's busy-gate floor (lease ≥ ~20m at cap 3; measurement-window
decouple replaces the short lease as the latency lever) and F5's sequencing (semantic
consolidation now, struct collapse stays behind its owner gate) — are tuning and scoping
answers reached BY the audit arithmetic, not doubts designed around. They are surfaced here
and in the phase plan; the orchestrator reviews them at the PR.

## 1. The problem, in half a page

The live incident (2026-11-17, issue #2100): a worker parked at 16:56:20Z and the platform's
first question arrived at 18:26:55Z — 90m35s, exactly two 45m lease windows, because every
event-driven accelerator was either dead (the turn-end emission never fired: its claim-holder
gate reads a registry file the writing half cannot write) or self-defeating (when the due-mark
does fire, the observation plane's own zero-baseline renews the row one more window — the
machinery's detection logic masked its own accelerator). Underneath both defects sits one
shape: claim-relevant FACTS — who holds claims, what advanced, when turns ended — are derived
in one process, persisted on side-channels the platform does not own end-to-end (append-only
files on a volume that is read-only to their writer; worker-declared watermarks), and consumed
in another process that cannot tell the channel is dead. Meanwhile the challenge's measurement
window is fused to the lease quantum (45m), so even a healthy nudge→wedge cycle costs a full
window, and the ladder's honesty machinery has accreted two clocks and five counters whose
write sites must be kept in lockstep by hand.

The design goal, per the charter: claims cheap to hold honestly, fast to detect when dead,
impossible to wedge silently — maintained by a human in an hour. The optimum is boring: the
platform's own store is the evidence channel; the challenge window decouples from the lease;
presence becomes response-observed; the two clocks become one platform-owned advance clock.

## 2. Evidence transport v2 — the store is the channel (store-native; fixes #2100's class)

The claim-relevant event set is small: turn-end (with its turn window), advance, session
death. For each, the platform ALREADY owns an end-to-end path or the fact itself:

- **Session death** is already store-native (`session_index.deleted_at`, consumed by
  `resolveRefs` → dead-evidence reclaim). No change.
- **Advance** is already store-native in its truth: every advance-class board call
  (`board_record_progress`, stage moves, verdicts — the `boardAdvanceTools` set,
  `board_turn_ledger.go:57-61`) is an API-mediated write that lands timestamped and
  actor-attributed in `board_transitions`/`board_events`. The agentd-side choke-point ledger +
  append-only tail (`board_turn_ledger.go:63`) exist ONLY to move this fact across agentd
  halves — over a file that is EROFS-dead on its writer. Transport v2 DELETES that machinery:
  the turn-end consumer derives `advanced` API-side with one bounded store query (did an
  advance-class write by this assignee land inside the turn window?), §2 of the low-level doc.
  A worker-declared nothing ever enters the path.
- **Turn-end** is the one raw harness fact that must cross a process boundary, and the
  platform-owned path exists and works: the main agentd's authority fold emits
  `EVENT_TYPE_BOARD_TURN_END` on the ABI stream (`sessionstate/board_turn_end.go`), the API's
  usagestream folds it (`board_due_mark.go`, wired at `app.go:1910-1912`). What kills it today
  is the claim-holder GATE in front of emission, fed by the dead registry. Transport v2 removes
  the gate: the event is emitted for every harness turn-end (payload: session, epoch, turn
  window, duration — no content), and the API consumer's EXISTING assignee-match guard
  (`due_mark.go:112`) is the claim filter. A user chat's turn-end matches no claim and is
  inert. The `boardWorkerSessions` registry's emission role dies with it (its remaining
  consumer — the self-certification refusal — is same-half MCP state; the low-level doc pins
  its blast radius and notes its file's separate disposition).

**Silent failure made impossible, honestly:** (a) no claim-relevant byte rides a mixed-mount
file — the EROFS class is deleted, not relocated; (b) the channel is monitored end-to-end by
its own heartbeat: the emitter exports `boardTurnEndsPublished` (exists), the API gains a
`board_turn_end_events_folds_total` counter and a board_health line `event_channel` (age of
last folded turn-end vs claims-active) so a dead fold leg reads DEGRADED on the surface a human
actually reads — the #2100 class was invisible because no surface compared claims-active
against events-seen; (c) every remaining failure degrades to the sweep backstop (#2065 R5
posture, unchanged).

**What is explicitly NOT built:** no new bus (the Redis lease-eventbus stays disarmed), no new
ABI event types for derived facts, no cross-half file protocols. The boring answer wins
because the facts were already platform-owned — they were just being re-derived and shipped
over channels nobody owned.

## 3. The challenge — due process kept, window decoupled (F3's real lever)

The nudge survives as challenge-before-kill (F6). What changes is its cost. Today the nudge
rung extends the lease by `LeaseDuration` (`reconciler.go:783-787`) and the ordinal question
is answered at the NEXT EXPIRY — so a non-responding worker costs a full 45m window after the
nudge, and every proposal to shorten that has collided with the lease's OTHER duties
(mid-turn coverage, backstop detection). The design splits the fused knob:

- **`NudgeMeasureWindow`** (new engine knob, default 10m, floor 5m): the nudge rung extends
  the lease by THIS, not by LeaseDuration. The row re-enters the victims scan at +10m and the
  unchanged ladder answers the ordinal question.
- Why 10m is safe post-#2042: the sentinel arms ONLY on a row that is quiet AND not busy
  (`reconciler.go:612-631`); a responding worker answers in seconds (live data: 20ms, 4.4s,
  19s); a worker that starts ANY new turn post-sentinel emits worker-activity frames that
  postdate the mark (`EventsAfterNudge`, worker-map only) and renews; a heartbeat arriving
  before the window closes renews IMMEDIATELY through the response arm (no expiry wait). The
  wedge therefore requires total silence on both channels from an idle session for the whole
  window — 10m is ≥3× the worst observed legitimate response latency (provider cold start),
  and the knob is tunable per deployment.
- The full cycle compresses: park → (event due-mark) → nudge within ~1 sweep; nudge → verdict
  ≤ NudgeMeasureWindow. **The live 90.5-minute shape becomes ≤ ~11 minutes** — without
  touching LeaseDuration, without one byte of busy-gate exposure. This is THE pin.

## 4. Presence — response-observed; the watermark retires (F4's answer)

The nudge prose stops demanding a specific tool call as the ONLY proof of life. Presence is
any observed response: post-sentinel worker-activity frames (the ordinal, already
worker-map-only), a post-nudge `board_heartbeat` arrival, or the session_index message_count
delta — the three legs the ordinal query already composes (`board_liveness.go:491-505`). The
heartbeat tool SURVIVES as the explicit, portable fast-path (and remains the only detector at
the no-evidence rung — the harness-agnostic floor keeps its pin), but its
`observedProgressAt` parameter becomes optional and stops writing the progress clock:
presence is not progress, and a worker-declared watermark is the orbit's fuel (a declared-fresh
stamp reads as motion today and resets the #2044 budget — the lying class is open by design;
retiring the watermark closes it). Motion becomes platform-observed only: §5's advance clock.
An honest transcript-working worker is unaffected in practice: its frames are movement
renewals, which already reset every budget counter; the shapes that lose credit are exactly
the shapes that never emitted anything.

## 5. The clocks — two become one, platform-owned (the F5 answer, sequenced)

`last_progress_at` (worker-anchored, heartbeat-written) and `last_renewed_at`
(ladder-written) both die in favor of **`tasks.last_advance_at`** — written ONLY at
platform-observed advance sites (claim mints, where it equals `stage_entered_at`; stage moves;
verdict writes; the #1946 criteria grant; the future `board_record_progress`) — plus the
existing `lease_expires_at` backstop. The #2043 lapse arm re-keys to advance staleness (no
ADVANCE for `HeartbeatLapseThreshold` ⇒ renewal evidence suppressed); the #2044 budget's
motion detector re-keys to `last_advance_at > stage_entered_at` (same comparison, honest
input). One clock, one writer class, zero worker input. The declared-watermark plumbing (the
response write's progress CASEs, the future-clamp tolerance on a worker stamp) is deleted.

The **`ClaimEvidence` JSONB collapse is explicitly NOT shipped here** (§5.3 of the low-level
doc carries the full reasoning for the record): the owner gate stands, and the server-side
single-clock-domain predicates under SKIP LOCKED are why these facts are columns. What this
design ships instead is the collapse's SEMANTIC v1 — one advance clock, no declared evidence,
three file artifacts deleted, the registry's cross-process role deleted — which shrinks the
column set by two and makes every remaining column platform-written. If the owner later opens
phase 4, it starts from a strictly smaller, fully-anchored surface.

## 6. The lease quantum — arithmetic, not vibes (F3's number, gated)

With §2-§5 landed, `LeaseDuration`'s remaining duties are: (a) events-dead backstop detection
(≤ 2 windows + sweep), (b) mid-turn silent coverage accounting (`boardBusyRenewCap × lease`),
(c) spawn-to-first-activity grace. The arithmetic:

- Coverage floor: real silent tool calls run 10–60m (builds, suites, browser installs). At
  cap 3: 45m lease ⇒ 135m coverage (generous); 20m ⇒ 60m (adequate + margin); 15m ⇒ 45m
  (zero margin); 5m ⇒ 15m (inadequate — false wedges, the busy-gate's live-fire class).
- Backstop: 45m ⇒ ≤90m worst detection; 20m ⇒ ≤50m.
- Renewal write volume scales 45/lease (≈2.2× at 20m): single-row guarded UPDATEs, no audit
  events — noise-level.

**Proposal: retune 45m → 20m as a SEPARATE, GATED phase** (kill switch = the existing config
knob, no migration), admitted only after one release cycle with
`board_due_marks_total{trigger=event}` share ≥ 90% and zero false-wedge escalations — staged
45 → 30 → 20 with a week's counter review between. The measurement-window decouple (§3)
delivers the latency win FIRST and independently; the lease retune is a follow-up tightening
of a degraded mode, not a prerequisite. `HeartbeatLapseThreshold` stays 90m (it bounds ADVANCE
staleness post-§5, an epoch-scale question, decoupled from the lease window — stated in the
low-level doc so nobody "harmonizes" it later).

## 7. Boundaries and sequencing (what absorbs what)

1. **PR #2104 (typed waits) and PR #2103 (salvage-prefer)** merge first — untouched by this
   design; Phase 3's `last_advance_at` writers are exactly #2104's wait-clear sites plus one
   term (additive, sequenced after).
2. **The un-filed #2100 fix story** is Phase 1 + Phase 2 of this design (file them as the
   story's scope; the issue's own fix directions — registry relocation, loud degradation,
   cursor rebaseline — are subsumed: Phase 1 lands the rebaseline + a minimal registry
   remediation so prod goes honest THIS release; Phase 2 deletes the remediation along with
   the class).
3. **The failed wait-event-broker story** (`tsk-pristine-henna-loses`) is unrelated to this
   core (the broker sits ABOVE, resolving event-waits by trigger) — it needs a re-arm and a
   fresh attempt; flagged to the orchestrator, not designed here.
4. **PR #2101's wait layer** composes unchanged: user-waits freeze the ladder, event-waits
   stay citizens, the broker's resume signal "races the ladder, never a lease extension" —
   and this design makes the race honest (the ladder it races now detects death in minutes,
   which is exactly why the broker must never carry renewal authority — the boundary holds).
5. **The seamless-lease doc** is amended in exactly two places (§3's window decouple; §5's
   clock consolidation); its Part-1 loop, pins 1-11, and the #2043/#2044 amendments otherwise
   stand as the ladder's record.

## 8. Non-goals (stated, per the charter)

No new event buses (the disarmed lease-eventbus stays disarmed; counters carry observability).
No distributed consensus, leader election, or fencing tokens — SKIP LOCKED + guarded UPDATEs
remain the only arbitration. No per-harness special cases (the store-side derivation is
harness-uniform by construction — the filter keys on assignee identity, not harness shape).
No functionality loss: user-waits, event-waits, salvage, suspension freeze, busy gate, and the
no-evidence floor all keep their pins green. No phase-4 JSONB collapse. No driver continuation
on turn-end (rejected in #2060, still rejected). No attestation machinery on the sentinel
(owner ruling: forgery is not the end of the world).
