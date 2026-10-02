# Worklog: Relay staging catch-up investigation + M2 per-provider stall-detector fix

**Date:** 2026-10-01
**Session:** Lane w9 (charter: why relay credential staging never happened for existing workspaces — 5-day permanent M2 fallback on workspace d8bed486). Root-cause adjudication, catch-up pins, and the per-provider fallback-classification fix.
**Status:** Complete (pending review)

---

## Objective

Determine why an armed controller (since v0.34.8, Sep 25) never staged relay handoffs for existing workspaces (prime hypothesis: #1597-class — staging tied to an event existing workspaces never re-fire), fix if it is the catch-up class, pin red-first, and answer what the fleet needs for the strict-mode flip.

---

## Work Completed

### Root cause adjudication (the prime hypothesis is DISPROVEN — two-class split)

- **Class 1 — the 5-day stall itself, root-caused by the sibling lane (#1611, merged as v0.34.13 mid-charter):** the API's SSL-redirect answered the controller's plain-HTTP `GET /api/v1/internal/workspaces/:id/llm-providers` with a 301 to TLS-on-plaintext-port. Every staging pass failed (`http: server gave HTTP response to HTTPS client` — the exact error worklog 1074 had recorded and mis-attributed to agentd) → `CredentialsStaged=False/StageFailed` forever → handoff never existed → M2 fallback fired every batch. **Fleet-wide, not existing-only** — zero workspaces ever staged, new or existing; the charter's framing was wrong on that axis.
- **Code reading (my first read, reported before building):** `reconcileRelayStaging` is called level-triggered from `handleCreating` (phase_creating.go:47, before pod build) and `handleActive` (phase_active.go:71, before the lifecycle branches). No creation-only trigger, no one-shot marker, no keypair-bootstrap condition. `requeueActive`=15s. The #1597 shape does not exist at this seam.
- **Live verification on d8bed486 (this pod):** confirmed the raw 25-char `sk-` key at `https://ai.thekao.cloud/v1` in `/agentd-config/agent-config.json` (the S1 exposure, live); confirmed the internal endpoint answers 401 (not 301) post-v0.34.13; a poller caught the catch-up flip at **2026-10-01 22:52:23 UTC** — `thekaocloud` now carries an `lrt_` token at the llm-relay router path — minutes after the API fix, with **no workspace event**, purely on the Active reconcile cadence. The 5-day permanent fallback ended; convergence is level-triggered, empirically.
- **Class 2 — the remaining hole (this lane's fix, issue #1614):** design 0061 §4 defines not-ready **per-provider**, but the builder implemented whole-handoff-absence plus per-provider expiry only. A **frontable** provider absent from a *present* handoff (all-mints-failed writes exactly that handoff; staging lag; torn handoff) delivered raw via `relayNotStaged` → `relay_raw_emission`: **uncounted** in migration (invisible to the 7-day-zero strict-flip criterion) and **fail-open in strict mode**. Post-#1611 this is the most likely residual failure shape — the controller→router mint path has never once succeeded in production (nothing ever staged before today), so a mint outage would sit in exactly this class while the counter reads zero.

### Implementation (TDD, red-first where a defect exists)

- **RED:** `TestRelayFallback_StageableAbsentFromPresentHandoff_MigrationCounts` (counter 0, audit says `relay_raw_emission`) and `..._StrictMutes` (raw delivered under strict) both failed on main; `TestStaging_TokensFailedMessageIsHonest` failed (message claimed "1 provider(s) staged" with zero tokens staged).
- **GREEN:** `secrets.RelayFrontableProvider(pd)` (pkg/secrets/provider_endpoints.go) — stageable kind ∧ resolvable upstream; the ONE predicate `relayDesiredSet` (controller) and the builder's classification share. Migration: frontable-absent counts (`relay_fallback_deliveries_total` + `relay_fallback_delivery` audit, reason `absent_from_handoff`), raw still delivers. Strict: frontable-absent **mutes** (audit `credential_skipped_relay_not_ready`, once-per-batch `relay_degraded_batches_total`). Non-frontable kinds: D5 raw carve unchanged in both modes. `CredentialsStaged=True` message counts the actual handoff token set + names mint shortfalls.
- **Catch-up pins (green-on-main by design — the hypothesis's disproof, guarded):** `TestRelayStaging_CatchUp_ConvergesPreArmingActiveWorkspace` (pre-arming Active workspace + pod converges through `r.Reconcile`/handleActive: envelope + handoff + True condition, steady-state retention on the second pass) and `TestRelayStaging_TriggerIsLevelTriggeredInBothPhaseHandlers` (source-shape pin: both handlers call the pass before their lifecycle branches).

### Respecting pinned rulings (recorded explicitly)

- **Key Decision 3 (M2 r1 finding 4):** bedrock under a present handoff stays uncounted — my change is gated on frontability, so the pin's letter and example survive untouched (extended: now pinned in migration mode too).
- **The expired-arm Nil-degrade ruling:** per-provider arms remain counter/audit-only, not class degrades — `degrade` stays nil when the handoff is present. The M4-visibility residual for per-provider fallbacks stands as designed (counter alerts; condition explains).

---

## Key Decisions

1. **Fix the counter classification, not the M4 degrade shape** — the strict-flip criterion is the counter; the two per-provider arms (expired, absent-frontable) now count identically while the class-degrade semantics stay exactly as the M2 lane pinned them. Minimal diff, two rulings preserved.
2. **Strict-mode mute for frontable-absent is a deliberate fail-closed alignment, not availability regression:** the only steady-state producer of this shape is a mint/staging fault; a ≤15s window on a freshly-bound provider is strict's meaning ("no raw under strict"), consistent with the whole-handoff mute.
3. **Shared predicate over duplicated logic:** the #1529 "builder cannot distinguish" ruling held for the MUTE decision on kind alone; `RelayFrontableProvider` is the controller's own stageability decision (kind + endpoint), so the builder counting on it cannot misclassify the D5 class. Parity is by construction, not by test.
4. **The catch-up pin is green-on-main and that is the finding** — reported honestly in the PR: no controller-side catch-up bug exists; the pin's value is guarding the level-triggered trigger against future "optimization" (e.g. moving the call behind an early return).
5. **Assumptions stated and validated (Rule 7):** the handoff-absent class was the counted one during the outage (validated: the orchestrator's counter evidence + `relayBatchDegrade` code path); this pod's workspace resolves providers by CRD owner for both builder and controller (validated: raw delivery proves the DEK path works, so `ResolveLLMProviders` returns the provider); production runs the chart-coupled flags (validated: controller-deployment renders `--relay-only-key-delivery=true` and the API env from one values key; the split-brain shape is not reachable by values).

---

## Blockers

None. (Note: GitHub pull_request webhook delivery was flapping per the charter — if the PR opens with no CI/review trigger, report and hold, no churn.)

---

## Tests Run

- `go test ./pkg/secrets/ -run 'TestRelay'` — ok (new: 2 StageableAbsent migration/strict + RelayFrontableProvider matrix; all existing pins green incl. Key Decision 3, expired-arm Nil-degrade, FlagOnRawEmissionIsAudited).
- `go test ./pkg/secrets/` (full package) — ok, 12.2s.
- `go test ./controller/internal/workspace/ -run 'TestStaging_'` — ok (new: TokensFailedMessageIsHonest; all staging/dr suites green).
- `go test ./controller/internal/workspace/` (full, 68s) — ok.
- `go test ./api/internal/services/workspace/ ./api/internal/handlers/ -run 'Relay|CredentialsStaged|Fallback'` — ok.
- `go test ./local/ -run 'TestUS72M2|TestUS61'` — ok.
- `gofmt -l` clean; `go vet` clean on touched packages. Envtest suite not runnable on this pod (no KUBEBUILDER_ASSETS; CI's envtest workflow covers it — the ConditionsMatrix asserts reasons/statuses, not messages).
- Red-first evidence captured in the session log (three tests failing on main pre-fix, verbatim output).

---

## Next Steps

- PR from `fix/relay-staging-catchup` → iterate to APPROVED → notify orchestrator (ses_f499ee9e6ffe52BJ8jxc2TEQQJ).
- Fleet watch (owner-side): `relay_fallback_deliveries_total` should fall to zero as v0.34.13 + this fix roll out; any residual ticks now name the provider.
- The strict-flip checklist is in issue #1614 (counter 7-day zero, frontable `relay_raw_emission` zero, CredentialsStaged fleet-true with no mint shortfalls, first green M2/M4 nightly under #1605's two-node config, posture gate green).
- Follow-up candidates (not this lane): US-72.5 coverage decision for non-frontable kinds; the per-provider-fallback M4-condition visibility residual (deliberate, documented).

---

## Files Modified

- pkg/secrets/provider_endpoints.go — `RelayFrontableProvider`
- pkg/secrets/injection.go — per-provider fallback classification (migration counts, strict mutes)
- pkg/secrets/relay_fallback_test.go — 3 new tests
- controller/internal/workspace/staging.go — relayDesiredSet onto the shared predicate; honest staged message
- controller/internal/workspace/staging_test.go — TokensFailedMessageIsHonest
- controller/internal/workspace/relay_catchup_test.go — new: catch-up pin + trigger source-pin
- worklogs/1083_2026-10-01_relay-staging-catchup-pins.md — this file
