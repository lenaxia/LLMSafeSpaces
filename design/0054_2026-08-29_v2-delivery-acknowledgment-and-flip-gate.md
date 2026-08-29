# 0054 — V2 delivery: acknowledgment contract, wire-contract authority, and the flip gate

**Status:** Proposed (partially implemented — the seam shipped in 0.25.3; the event bridge and pin contract gate the re-flip)
**Date:** 2026-08-29
**Depends on:** design 0052 (V2 inboard delivery), #1119 (incident record), #1121/#1123 (seam + follow-ups)
**Corrects:** 0052's rollout assumptions — its own empirical table predicted both production failures

## Problem

0052 shipped the V2 delivery flag. Two production flips (2026-08-28/29)
failed for causes that were **knowable in advance** — one was in 0052's own
findings table — because the rollout was gated on mechanics, not on product
requirements. This design states the requirements, the invariants they
imply, the acceptance criteria that gate the flip, and the three mechanisms
that satisfy them: the promotion-await acknowledgment seam, wire-contract
authority (pin + probe, never guess), and the frontend event bridge.

## What the incident taught (evidence: #1119, 6 comments + this gist trail)

1. **Admission ≠ delivery.** The user text persists at *promotion*; a
   defect-class death (model-resolve failure, park race) consumes or
   strands the admitted row with no signal. Completing outbox entries at
   admission silently drops messages (`sent` fired, turn never ran).
2. **V2 turns emit only `session.next.*`** — 0052's table said so; the
   frontend's busy/new-message indicators ride `session.status` and
   `message.part.delta`, which never fire in V2 mode.
3. **Wire shapes drift across patch releases** (`{modelID}` → `{id}` by
   1.18.15) and a probe that cannot distinguish "legacy" from
   "indeterminate" guesses — and a guessed shape is silently dropped.
4. Transient upstream dependencies (a LiteLLM config roll churning the
   model catalog) can kill a turn defect-class — the platform must assume
   best-effort execution regardless of upstream reliability work.

## Requirements, invariants, acceptance criteria

### Product requirements

| # | Requirement |
|---|---|
| R1 | Send anytime — idle, busy, mid-turn; the send never blocks the UI |
| R2 | Exactly-once effect — every accepted message produces exactly one turn |
| R3 | Outcome visibility — streaming response or actionable failure with reason; never a silent spinner |
| R4 | Model fidelity — the picked model is the model that runs |
| R5 | Legible conversation — busy/idle, new-message, queue indicators work |
| R6 | History coherence — the user can always read their transcript |
| R7 | Crash tolerance — messages survive agent restarts, pod churn, API rollouts |
| R8 | Maintainability — opencode bumps ship without production incidents |

### Invariants

- **I1 Durable intent**: no 202 before durable persistence.
- **I2 No silent loss**: `sent` requires the agent persisted the user text;
  every non-delivery path terminates in a visible state.
- **I3 No duplication**: never re-send what the agent may hold, verify-first (#987).
- **I4 Contract fidelity**: never send a guessed wire shape — verified pin
  or positive probe observation only.
- **I5 Mode coherence**: delivery, read, and verify agree on the same store.
- **I6 Bounded liveness**: every non-terminal entry has a bounded time to
  its next transition.
- **I7 Explainability**: every delivery transition observable in
  events/metrics/logs — no pod forensics.

### Acceptance criteria (THE flip gate — the old runbook checklist is superseded)

- R1: mid-turn send → <100 ms, queued state
- R2/I3: duplicate-contract suite green; crash-recovery verifies before re-send
- I2: defect-class injection (model-resolve death, agent-down, park race,
  instance dispose) → every case ends failed-with-reason in the queue UI
  within bounds; zero sent-without-persist across the suite
- R4: e2e — pick model M, send, metering proves M ran *(no AC existed; the
  2026-08-29 regression)*
- R5: e2e **in V2 mode** — busy within 2 s of turn start, deltas stream,
  new-message fires *(no AC existed; predicted by 0052's own table)*
- R6: dual-store reads, or the exception documented and user-visible at flip
- I4: bump gate green vs the pinned binary; probe-vs-pin mismatch alerts;
  no indeterminate→guess code path
- I6: outbox depth + oldest-age metrics; verify errors logged
- I7: steady-state taxonomy has zero `unknown`-classified events

## Mechanism 1 — the promotion-await seam (shipped, #1121/0.25.3)

`outboxDeliver` (V2 branch): after `SendAsync` admits, poll the promotion
oracle — `VerifyDelivery(text, since=admittedAt)` — for `V2PromotionWait`
(30 s; production: persist lands <1 s including dying promotions).
Promotion observed → complete (`queue.update/sent` at *real* delivery).
Window expired → `outbox.Ambiguous` → the #987 machinery: late promotion
completes; absent-after-window re-admits (bounded nudge). No new state, no
cross-replica coupling; rides the hardened verify path.

Rejected alternative: completion on the `session.next.prompted` event
(messageID correlation). Empirically fragile — `prompted` fires for
promotions that then die (consume-without-trace); the persisted text is
the only promotion proof. Rejected alternative: per-session event
subscriptions (the TUI model) — subscriptions are pure reads (disproved
live); redundant with the global bus the bridge already consumes.

Operational notes: the delivery budget must exceed the window (+20 s
margin, #1123); the oracle runs on a dedicated no-keep-alives bounded
client (#1123) — one wedged agent connection costs one bounded pass, never
the shared pool.

## Mechanism 2 — wire-contract authority (I4; #1123 + refinements)

Authority order: **positive probe observation > pinned contract > never a
guess.**

1. `PinnedRuntimeContract` — one constant stating what the platform's
   runtime images guarantee (Model.Ref key shape, V2 route presence).
2. **Continuous CI enforcement**: the bump gate validates candidate
   binaries, and CI re-validates the pin against the currently pinned
   binary on every change to either side. A pin that is only bump-checked
   is folklore with a constant's name.
3. Runtime behavior: use a positive probe classification when available
   (reality wins); the pin is the *only* indeterminate fallback; mismatch
   surfaces as an alert, never a silent switch.
4. **Provenance predicate**: platform-registry runtime image → pinned
   mode (assert); custom/BYO image → discovery mode (classify). "The pin"
   is not global — per-CR runtime overrides exist.

Rejected: probe-only authority (indeterminate ⇒ guess — the 2026-08-29
model regression); pin-only (denies reality on mismatch, and BYO runtimes
exist). Growth path: schema codegen at bump time if surfaces multiply.

## Mechanism 3 — the frontend event bridge (R5/I7; gates the re-flip)

Map `session.next.*` → the platform event contract in **one** derivation
point (the SSE tracker's classifier/deriver, where US-63.5's queue
derivations already live):

- busy: `session.next.step.started` → busy-on; turn-end (last
  `step.ended` with no pending step / prompt completion) → idle
- streaming: `session.next.text.delta` → the platform's delta event shape
- new-message: message-completion events → the new-message indicator

Steps, in order: (1) enumerate the real V2 taxonomy — the 14k+ `unknown`-
classified events from first traffic have never been inventoried;
(2) map + implement; (3) V2-mode e2e asserting R5's ACs; (4) I7 requires
steady-state zero `unknown`.

## Amendments (2026-08-29, post-audit)

- **Model semantics under V2 are SESSION-scoped, not per-message.** The
  V2 endpoint strips per-prompt overrides, so a message's model selection
  applies to the session and sticks for subsequent no-override messages
  (matching the SPA's own UX). Two consequences accepted for test mode:
  (a) this is a product behavior change vs V1 — surfaced here rather
  than implicit; (b) mid-conversation model switching races: a turn
  admitted before a later SetSessionModel may execute after it and run
  on the later model. Rare, bounded to rapid-switch usage; reopen if it
  bites. Upstream ask (per-prompt model in the V2 schema) remains the
  clean fix.
- **G2 closed** (busy-reaper): bridge-derived busy states carry a
  deadline (10 min, delivery-scale); expired ones publish idle with a
  warning. Lost terminal events decay in bounded time.
- **Turn-lifecycle has two sources today** (V1: tracker activity
  machinery; V2: event derivation in onV2RawEvent). They can disagree
  (tracker idle-timeout vs bridge idle). Unification — one lifecycle
  derivation fed by both dialects — is an open item before the
  production gate; until then the bridge is authoritative for V2 turns.
- **Composition-testing discipline**: every handler-side adapter seam
  gets a pass-through assertion on the COMPOSED production stack
  (proxy_verify_wiring_test.go) — the #1127 outage passed every unit
  test because tests see the raw adapter. New seams extend that test.
- **Cross-uid coverage**: #1131 closed the single-container materialize
  gap (auth store 0640). A systematic audit of every uid-2000-writer →
  uid-1000-reader path is an open hardening item — three incidents in
  two days came from per-path patches.

## Corner-case matrix (2026-08-29 adversarial review)

Verified covered:

- **API crash mid-promotion-wait** — `Recover` restores staged entries as
  `StatusVerifying` (outbox.go:738): verify-first, never a blind re-admit.
  The exactly-once property holds across crashes by construction.
- **Agent restart mid-window** (the MHcs1C case) — late promotion lands;
  window expiry → ambiguous; verify backoff outlives the restart; oracle
  confirms delivered. Observed live, resolved correctly.
- **Turn longer than the window** — completion is at promotion, not turn
  end; no interaction with turn duration.
- **Two replicas** — shared Redis state + per-session lock; oracle client
  is adapter-scoped.
- **Identical legitimate texts in one session** — the `since` floor +
  clock-skew margin separates windows; only sub-skew interleavings can
  cross-match (bounded by `verifyClockSkew`).
- **Re-admit nudge safety** — re-admission only after a *definitive
  absence* verdict; residual TOCTOU (promotion lands after the absence
  check) can duplicate one turn in a rare race — accepted residual,
  bounded, surfaced by attempts counters.

Gaps found (this review — must close before re-flip):

- **G1 Rollback mode-coherence (I5 violation on flag-off).** Flipping
  V2→V1 with in-flight V2-verifying entries switches the verifier to the
  V1 store: V2-window text reads absent → definitive absence → V1
  re-send → duplicate turn. The 2026-08-29 rollback avoided it only
  because the single stuck entry had been manually reaped minutes
  earlier. **Flip procedure addition: drain before mode change** — pause
  accepts, let verifying entries resolve or park them with an explicit
  `mode_transition` reason (visible + dismissable, never auto-re-sent),
  then flip. Mechanical, not runbook-prose.
- **G2 Bridge busy-reaper.** The event bridge must not create a
  busy-forever class when a turn-end event is lost (agent restart
  mid-turn): busy state needs a timeout/reaper derived from the same
  events that set it. Bridge-design requirement, not implementation
  detail.

Documented limitations (explicit, not silent):

- **Custom pre-V2 runtimes under the global flag** — a BYO 1.18.14 image
  (admit-without-drain) chronically fails the window and parks as
  failed-with-reason: safe (I2 holds) but permanently non-functional.
  The provenance predicate should surface a per-workspace warning.
- **Verify cost on very long sessions** — `MessagesV2` is a single
  unbounded fetch (64 MB bound); pagination is a scale follow-up.

## Known accepted exception — R6

The global history read switch hides pre-flip transcripts while V2 is on
(operator decision 2026-08-28). Until dual-store reads exist, the flip
gate includes R6 as **documented exception, user-visible at flip time**.
Dual-store reads are a separately funded decision, not a flip blocker.

## Flip gate procedure

1. Every AC green (including R4/R5 e2e in V2 mode).
2. Flip via `api.extraEnv` in the config repo (durable), never `kubectl set env`.
3. Watch: sent-at-promotion timing, verifying-entry rate, taxonomy
   `unknown`=0, model-fidelity spot check.
4. Rollback = revert the flag commit (V1 restores indicators, model path, reads).

## Open items

- Event bridge implementation + taxonomy inventory (this design's only
  unbuilt mechanism).
- Pinned-contract refinements (constant + CI enforcement + predicate).
- Upstream asks (opencode): terminal delivery events with messageID echo;
  wake-on-admit; stable Model.Ref schema. The platform must not depend on
  them (defense in depth) but should push for them.
- Outbox depth/oldest-age metrics (I6 observability gap).
