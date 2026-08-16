# Starvation-proof session truthfulness: watchdog demotion, tracker reset, durable prompts

**Author:** ops (via investigation of the 2026-08-15/16 repeated-session-halt incident, workspaces 946a442f / 843a55c2)
**Status:** Draft — pending review
**Related:** #892 (tracking issue), #807 (hung-opencode watchdog), #795 / #803 / #805 (ground-truth session status), #852 (false auto-aborts), #810 (watchdog birth), branch `fix/watchdog-starvation-corroboration`

## Problem

Long LLM turns on build-capable workspaces were being aborted repeatedly.
Investigation across two live workspaces (API request logs, agentd logs,
opencode histories, cgroup `cpu.stat`, kubelet events) found not one cause
but a chain, each link turning CPU starvation into user-visible loss:

1. **Watchdog false fires.** A 2-CPU cgroup under build load (the pods' own
   `go`/`esbuild`/`tsc` children; 4,460–10,576 throttled CFS periods,
   397–1,307 s cumulative stall) starves opencode's single JS event loop
   past the 4 s health timeout. The watchdog reads timeout as "hung" and
   SIGTERMs a progressing process. 7+ kills observed, **zero true
   positives**. The max-defer force path killed busy sessions *by design*
   after ~5 min on the same ambiguous evidence. Restart churn amplifies
   starvation (each restart re-does boot work) and repeatedly killed
   containers via kubelet startup-probe timeouts.

2. **Phantom-busy sessions.** The "ground-truth" chain (#795/#803/#805) is
   circular at the last hop: `cachedState` builds statusz busyness from the
   SSE tracker mirror and only prunes entries for sessions that no longer
   *exist* in opencode's `/session` — but `/session` returns DB records,
   which survive death (the codebase itself documents this distinction in
   `session_aware_restart_test.go:227`). When opencode dies mid-turn, the
   busy flag is orphaned forever: no idle event will ever come. Observed:
   8 tools stuck `status:"running"` across two sessions, each starting
   seconds before a generation change. Users see "busy, no progress for
   20–30 min."

3. **Message loss.** Revive messages died client-side (`SendPromptAsync:
   context canceled` ×6 — iOS kills in-flight fetches) because the Redis
   queue never engaged: the client queues only on its own busy reading,
   which was stale-idle under SSE starvation. Zero `POST /queue` in 14 h.

4. **Human as reconciler.** With a phantom-busy session, Stop is the only
   control that works (`POST /abort` → opencode emits idle → tracker
   flushes). Users rationally pre-Stop before sending — and Stop cannot
   distinguish orphaned-busy (dead state, nothing to kill) from a live but
   silent turn (a real `sleep 720` was killed mid-wait at 03:46).

Constraint (ruling): **CPU limits are not raised.** The system must
disambiguate starvation from hang within the 2-CPU budget.

## Root-cause frame

Every actor that treats a **timeout as a verdict** kills or loses something:
the watchdog (timeout → kill), kubelet probes (timeout → pod kill), the
client fetch (cancel → dropped message), the client busy-gate (stale →
wrong route). The design goal is that under a CPU storm **nothing is
killed and nothing is lost**, and honest state recovers on its own.

Property taxonomy the fixes align to: **liveness** (process exists —
`cmd.Wait()`), **responsiveness** (loop serves HTTP — healthz), **progress**
(work advances — CPU ticks / tool output / generation). Conflation of the
last two is the incident.

## Design

### D1 — Watchdog demotion: detect and alert, (almost) never kill

Rulings (from incident review; supersede the corroboration branch's
original kill matrix):

| Evidence at would-fire moment | Action |
|---|---|
| TCP dial to opencode **refused** (no listener) | Kill — the only lethal path |
| Dial OK + CPU ticks advancing over ~3 s sample | Suppress (progressing; starved, not hung) |
| Dial OK + CPU ticks flat | **Suppress** — blocked-upstream-IO turns are alive and must not be killed |
| Evidence unavailable (probe bug, /proc unreadable, mid-restart) | **Suppress + alert** — fail closed to the no-kill policy |

- **Flat-CPU is not kill evidence.** A turn blocked on a timeout-less
  upstream LLM call has a live process and flat ticks; killing it is the
  same ambiguity class as starvation. Recovery for genuinely wedged turns
  is D2 (honest busy state) + user Stop, now informed (D5).
- **No stand-down.** Suppress forever; count
  `workspace_watchdog_suppressions_total{reason=starved|flat|unknown}` and
  alert on sustained growth (ops owning the alert rule). The previous
  60-suppression disarm would have re-run the incident under chronic load.
- **Consequence, accepted:** with flat-CPU off the kill list, the lethal
  set converges to dead-listener-only, which crash recovery nearly always
  reaches first. The watchdog survives as an **instrument** (detector,
  alerter, suppressor), not an executioner. Hung-and-alive is recovered by
  D2 + D5 + D6 escalation.
- **Generation-aware refused-dial (gap G2).** A refused dial during the
  respawn window (crash recovery mid-restart, socket not yet bound) must
  not kill the process `cmd.Wait()` just spawned — that loop is the
  6-restarts-in-11-min shape. If a (re)start is in flight, crash recovery
  owns lifecycle; the watchdog stands down.

### D2 — Tracker reset on opencode generation change

On every managed-process start hook: clear **busy flags only** for entries
in the SSE tracker. Rationale: busy state produced by a dead generation is
orphaned by definition (validated: all 8 observed orphans started seconds
before a generation change).

- Do **not** clear `promptTokens`/contextUsed data — it is display state,
  not liveness state (`server.go:69`), and survives legitimately.
- Fresh SSE events rebuild truth within seconds of the new process
  serving. `cachedState`'s tracker.get + prune are unchanged otherwise.
- Naming (gap G1): once D3 lands, the **durability layer owns busy
  truth**; the tracker is its cache. Until then the tracker is authoritative.

### D3 — Durable prompts: mimic the TUI's *property*, not its mechanism

> **G4 audit amendment (2026-08-16, #892):** the durable queue already
> exists — `EnqueueMessage` routes to opencode V2 `delivery:"queue"`,
> which persists SessionInput rows in opencode's SQLite (survive
> generation changes by construction) and drains on `execution.wake`;
> `wakeStrandedV2Sessions` recovers stranded inputs on reconnect. The
> incident's losses were all on the V1 direct path chosen by the client.
> D3 therefore reduces to: retire client-decides routing, let the API
> decide queue-vs-direct from authoritative busy, add clientMessageID
> idempotency and a per-session count cap. No new durability machinery.

The TUI never loses a compose because compose and execute share fate. A
networked API cannot share fate, so we copy the property — **accept-then-
process** — and pay its taxes explicitly:

- **Server-side accept.** `POST /prompt` validates, dedupes, persists to
  opencode's V2 `delivery:"queue"` (durable SessionInput rows in
  opencode's SQLite — per the G4 audit above; the Redis structure is a
  shadow tracker, not the queue), acks. The client can no longer lose a message
  by dying mid-flight. The direct-send client path is retired entirely;
  **the server decides queue-vs-deliver, always** (the client-decides path
  reintroduces both the FIFO race documented at `ChatPage.tsx:985` and the
  stale-busy mist routing).
- **Idempotency.** Client-supplied `clientMessageID`; dedupe on accept.
  Covers iOS reconnect retries and double-taps.
- **Delivery semantics: at-least-once, deduped at render.** A crash
  between "opencode persisted" and "API marked delivered" can redeliver;
  exactly-once is impossible without opencode-side idempotency. The UI
  collapses duplicates by clientMessageID. This is a written decision; do
  not revisit per-PR.
- **Backpressure.** Per-session queue cap (e.g. 25) with 429 + queue
  visibility; accept-during-outage must not become a billing surprise.
- **Prerequisite audit (gap G4):** the existing drain goroutine's restart
  semantics — what happens to in-flight drains, ordering, and the
  delivered-marking across an opencode generation change — must be mapped
  before implementation. Start from the queue that exists, not a blank page.

### D4 — Kubelet probes: detect death, never slowness

Under starvation, timeouts are the *expected symptom of a healthy pod*.

- **Liveness:** `/v1/healthz` only; verify the handler is lock-free (gap
  G5). periodSeconds 10, timeoutSeconds 10, failureThreshold 8 (~80 s
  grace). Kills only on agentd-process death.
- **Readiness:** agentd up + opencode process exists — not responsiveness.
  Slow-but-alive pods must keep traffic; unready buys no CPU and
  disconnects the wrong thing.
- **Startup:** periodSeconds 5, failureThreshold 36 (180 s incl. relay
  injection). The startup-probe kill of the 04:01 pod was churn, not
  recovery. Chart/operator config change only. *(Amended in the D4 PR:
  the original draft said ×30/150 s; the incident log showed boot
  exceeding 120 s under quota saturation — with a 3 s timeout, ×36/180 s
  is the number actually shipped and covered by the pod-builder test's
  ≥180 s budget assertion. Review round 1 on #895 flagged the
  deviation; recorded here rather than silently diverging.)*

### D5 — Informed Stop + progress visibility

- `POST /abort` semantics unchanged (it is the one reliable idle-synthesizer),
  but with D2 the *need* to pre-Stop disappears, and with honest busy flags
  Stop becomes an informed action rather than a ritual.
- **Elapsed-time badges** on running tool parts, rendered from
  `state.time.start` (already in payload). An orphaned tool honestly
  showing "3h" is the signal that state is stale — visible without any
  protocol change.

### D6 — Unattended escalation

Suppress-forever is safe only when someone is watching. For sessions with
no interactive owner (workflows, triggers, background agents): sustained
suppression + busy-N-minutes → notify the session owner (API notification /
frontend banner). Policy: **notify, never execute.** New surface; needs an
API + frontend design pass (gap G3).

### D7 — Tool-children parallelism caps (within the CPU constraint)

The starvation severity was self-inflicted oversubscription: build tools
spun machine-sized thread pools (GOMAXPROCS-class defaults) inside a 2-CPU
quota. Cap them at the workspace layer via env. No limit raise; this
converts 400 s CFS stalls into ordinary queueing. Needs a measurement pass
to confirm the biggest levers (gap G6).

*(Amended in the D7 PR, review round 1 on #897:)*
- *The single shipped lever is `GOMAXPROCS` (set to the effective burst
  limit). `ESBUILD_WORKER_THREADS` from the draft was verified against
  esbuild's shipped source to have no numeric semantics — a `"0"`-disable
  flag for exactly one sync-API worker thread — so setting it to any
  number is a placebo; it is deliberately not set. esbuild is covered
  transitively: it is a Go binary, so its pool follows GOMAXPROCS.*
- *`npm config jobs` from the draft is dropped: npm has no effective
  parallelism env knob; it rides the quota like any other process.*
- *Order deviation recorded: the design ordered "G6 measurement → D7
  caps"; D7 shipped first with G6 deferred to post-deploy (pre/post
  throttle counters on a build-capable workspace). Rationale: the caps
  are strictly no-worse (pools bounded by the quota the cgroup already
  enforces), and the incident's usage-blocking severity justified not
  waiting. G6 remains open to validate the effect and tune the cap.*

## Assumptions and where they fail

| # | Assumption | Fails when | Mitigation in design |
|---|---|---|---|
| A1 | Busy state from a dead generation is orphaned | Tracker also holds non-liveness state (context meters); a second busy authority appears with D3 | Reset busy flags only (D2); name the durability layer as busy owner (G1) |
| A2 | Dial refused = safe kill | Crash-recovery respawn window: socket not yet bound | Generation-aware stand-down (D1/G2) |
| A3 | Suppress-forever is safe | Unattended sessions hang silently | D6 escalation |
| A4 | TUI behavior transfers | Network taxes: duplicates, redelivery, ordering, backpressure | D3 decisions; audit first (G4) |
| A5 | 2 CPUs fixed ⇒ starvation is weather | Severity amplified by oversubscribed tool children | D7 caps |
| A6 | `/proc/<pid>/stat` ticks advance under CFS throttle | (Kernel-solid; listed for completeness) | — |

## Gaps (each blocks the component named)

- **G1** Busy-authority unification (D2/D3 seam)
- **G2** Generation-aware refused-dial (blocks D1 as coded)
- **G3** Escalation path spec (D6)
- **G4** Drain-goroutine restart audit (blocks D3 implementation)
- **G5** healthz lock-freeness verification (blocks D4 liveness config)
- **G6** Tool-children cap measurement (D7)
- **G7** The stress harness (below) — gates all merges

## Stress test (gate — nothing merges before it passes)

Harness in a real workspace: CPU storm (a `go build -p16`-class load) +
live long turns + forced opencode restarts mid-queue + iOS-style client
disconnects mid-send.

**Acceptance:**
- Zero kills of a reachable opencode (watchdog AND kubelet)
- Suppressions counted and alerted, not acted on
- Every accepted message delivered at-least-once, rendered once
- Tracker busy flags clear on every generation change within seconds
- Elapsed badges render; no phantom-busy session survives a restart + 60 s
- The unifying assertion: every timeout-consuming actor (watchdog, kubelet,
  client fetch, client busy-gate) ends the storm having killed nothing and
  lost nothing.

## Implementation order

1. D2 tracker reset (small, independent, user-visible)
2. D1 amendments on `fix/watchdog-starvation-corroboration` (reclassify
   flat→suppress, unknown→suppress+alert, remove stand-down, generation-
   aware refused-dial G2) + marker-failure metric + full suite; PR
3. D4 probe config (after G5 verification)
4. D5 badges (frontend, independent)
5. G4 audit → D3 durability PR
6. G6 measurement → D7 caps PR
7. G7 harness runs against the stack; #807's original hung-opencode
   complaint is re-checked last (a listening-but-deadlocked loop is now
   recovered by D6 escalation + informed Stop — confirm that is acceptable
   to close the loop on #807, or revisit with data)

## Deferred decisions

- Watchdog drop-by-data: if suppression metrics show `hung` never fires
  while `starved` fires constantly, deleting the kill path entirely
  becomes justified — post-deploy, evidence first.
- Multi-workspace-class probe defaults (this doc covers build-capable
  workspaces; light workspaces may want tighter thresholds).
