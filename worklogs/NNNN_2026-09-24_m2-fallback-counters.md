# Worklog: M2 — migration-mode fail-open fallback + both counters (design 0061 §4, implementation PR3)

**Date:** 2026-09-24
**Session:** The recorded order's third item, after M1 (#1553, merged 301bdebc). The seam contract with the M4 lane (wt-1453, #1557 merged as 1530cc7e) is honored: the fallback reason is non-nil-classified and joined IsRelayDegrade.
**Status:** Complete

---

## Objective

"Not ready at batch time" (the handoff Secret absent, or its token for a bound provider expired) delivers the PRE-FLIP raw-key entry in migration mode + `relay_fallback_deliveries_total{workspace,provider_slug}` (the stall detector); strict mode preserves the fail-closed class-mute + `relay_degraded_batches_total{workspace,reason}` (the mode-independent detector); the knob threads chart → env → config → the install seam; both alerts render; the runbook carries the strict-flip criteria.

## Work Completed

- **The builder** (`pkg/secrets/relay_batch.go` + `injection.go`):
  - `DegradeRelayFallbackDelivery = "relay_fallback_delivery"` — non-nil on migration-mode not-ready (the M4 seam contract: CredentialsStaged=False/relay_fallback_delivery) — and the reason JOINS `IsRelayDegrade` (one place; the builder owns the vocabulary).
  - `SetRelayDeliveryFallback` + the mode field (zero value STRICT — the fail-closed default; the API's install arms migration explicitly) + `RelayFallbackForTest` (the observer precedent).
  - `relayBatchDegrade` mode-aware: migration → the fallback degrade + the class-level `relay_fallback_delivery` audit (the batch DELIVERS); strict → the existing not-ready degrade + audit + `relayDegradedBatches` increment.
  - `buildCredentialEntries`: the class-mute lifts under migration (`muteClass` gains `!s.relayFallback`); each whole-handoff raw emission counts (`relayFallbackDeliveries{workspace,slug}.Inc()`) + audits per provider.
  - `applyRelayHandoff(pd, h, fallbackAllowed)`: the EXPIRED outcome (RFC3339 past) — migration returns the outcome (the caller delivers raw + counts); STRICT delivers the token UNCHANGED (the renewal path owns expiry — no behavior change).
- **The counters**: plain `prometheus.NewCounterVec` (NOT promauto — agentd links the package and must not carry the series); the API MustRegisters them ONCE at relay-install (`registerRelayMetricsOnce`).
- **The knob**: `relayOnlyKeyDelivery.fallbackMode` (chart default `"migration"` — the merge-time default; the design's ruling) → `LLMSAFESPACES_RELAYONLYKEYDELIVERY_FALLBACK_MODE` (flag-on only) → config `BindEnv` + `SetDefault(migration)` → `installRelayTokenSource(svc, enabled, fallbackMode, ...)` arms the builder.
- **The alerts** (`prometheus-rules.yaml`, the `llmsafespaces.llm-relay`-adjacent group): `RelayFallbackDeliveryActive` (WARNING — any firing = the migration is live; the runbook named) + `RelayDegradedBatchesActive` (CRITICAL — the mode-independent fail-closed detector; the #1548 harm named).
- **The runbook** (`docs/runbooks/relay-only-flip.md`): the strict-flip paragraph — both criteria (7 consecutive zero-days + the gate green), the post-flip detection transfer to the degraded series, the one-value rollback.

## Tests (red-first: the table ran RED against the unmodified builder — the fixture pass was nil-source, fixed to the fake)

- `pkg/secrets/relay_fallback_test.go`: MIGRATION delivers raw + non-nil fallback degrade + per-slug counter + audit + NO degraded counter; EXPIRED per-provider falls back raw + counts while the valid provider keeps its token and NO class degrade (per-provider granularity); STRICT preserves the class mute + the not-ready degrade + the degraded counter + zero fallback; PRESENT handoff unchanged in both modes; the IsRelayDegrade join (the seam contract pinned).
- `api/internal/config`: the env threads + the migration default.
- `api/internal/app`: the install-seam table (""/migration arm; strict doesn't; flag-off wires nothing).
- `helm`: the env renders under the default (migration), explicit strict threads, flag-off renders nothing, both alert expressions render (the PrometheusRule extraction under monitoring.enabled — discovered: `monitoring.enabled` gates the rules resource, not prometheusRules alone).
- Counter isolation: `resetRelayCounters` per test (package-global collectors; the absolute zero-assertions need it — caught by test-order contamination).

## Key Decisions

1. The counters live in `pkg/secrets` but register ONLY in the API process (agentd links the package — the series belong to the batch builder's process).
2. STRICT + expired token: UNCHANGED delivery (the renewal path owns expiry) — the fallback is the not-ready AVAILABILITY trade, not an expiry enforcement change; migration + expired = raw (an expired token delivers broken anyway — agentd's local degrade — the raw key is the working credential).
3. The per-provider raw path for NOT-STAGED slugs under a PRESENT handoff (the #1529 mixed-fleet class, audited relay_raw_emission) is NOT counted as fallback — staging is READY; only absent-handoff and expired-token emissions count. (The whole-handoff-absent case counts every emitted slug.)

## Assumptions → validation record

- The M4 classifier picks up the fallback reason unchanged — verified against 1530cc7e's IsRelayDegrade (the case join is the one-line obligation; the pin asserts it).
- Flag-off byte-identical: the zero-value mode (strict) + the nil source reproduce the pre-M2 behavior exactly — the full prior relay_batch suite passes unmodified.

## Blockers

None.

## Tests Run

- `go test ./pkg/secrets/ ./api/...` — 43 packages green (the builder table + config + app seams + helm).
- Full `./helm/` with real helm green (the 4 new render pins).

## Next Steps

1. Review; then the recorded order continues: the M2/M4 e2e migration story (owns #1548's AC1/AC4) + the posture gate.

## Files Modified

- `pkg/secrets/relay_batch.go` (the reason const, IsRelayDegrade join, the counters, the mode setter/observer, applyRelayHandoff's expired outcome), `injection.go` (the mode-aware mute + the entry-site counting + the expired caller logic), `secret_service.go` (the field)
- `api/internal/config/config.go` (the knob + BindEnv + default), `api/internal/app/relay_handoff.go` (the install signature + the mode + the metric registration), `app.go` (the call), `relay_handoff_test.go`, `relay_only_config_test.go`
- `helm/values.yaml` (fallbackMode + the strict-flip criteria comment), `helm/templates/api-deployment.yaml` (the env), `helm/templates/prometheus-rules.yaml` (both alerts), `helm/relay_fallback_mode_test.go` (new, 4 pins)
- `docs/runbooks/relay-only-flip.md` (the strict-flip paragraph)

## r0-fix — the CI-red my local run couldn't see (no promtool here)

The alerts insert landed INSIDE LlmRelayRouterRejectSpike's multi-line expr block (my python anchor was the CONTINUATION line of a folded expression, not a rule boundary — the inserted `- alert:` lines became expression text, and promtool caught it: "could not parse expression: unexpected identifier RelayFallbackDeliveryActive"). The local run couldn't catch it — TestPromtoolRules SKIPS without the promtool binary, the exact silent-skip shape the orchestrator flagged on #1534. Fixed: the two alerts re-inserted as complete rules at the group head, before the first alert. Structural validation added locally (a rendered-YAML walk asserting every rule in the group has a clean alert name and an uncontaminated expr — the class closed without the binary).

## r1 — the four unit-level gaps closed; the structural walk in-tree

- **Promtool fire scenarios**: both alerts now have input_series + alert_rule_test cases in alerts_promtool_test.yaml (the #906-never-fire class; the design's own "fires in the unit harness" bar).
- **The registration observability**: TestInstallRelay_CountersRegistered asserts BOTH counters are gatherable from the default registry after the install seam — with the CounterVec-emits-nothing-until-a-child probe documented (the reviewer's deletion mutation now goes red).
- **Strict+expired**: TestRelayFallback_StrictExpiredDeliversToken pins the token-delivers-unchanged promise; the unreachable else arm REMOVED (applyRelayHandoff returns relayExpired only under migration — the comment now states the actual strict mechanism: the expiry check is skipped THERE).
- **Key Decision 3 pinned**: the present-handoff test now asserts fallbackDelta(aws-bedrock) == 0 (the not-staged class is NOT fallback — staging is ready).
- **The r0-fix "structural walk" claim**: now a real test (TestRelayFallbackMode_RulesStructurallyClean — the parsed-rules walk catching the folded-expr insertion class the Contains pins cannot).
- The e2e gap stands per the RECORDED ORDER — the M2/M4 e2e migration story is the next queue item (design 0061 §10's assignment; not evaporating — stated here and in the PR).

## r2 — the red-on-arrival promtool expectations; the enum; the stale comments

- The r1 promtool scenarios were RED (the reviewer reproduced with the pinned v3.4.1): promtool compares the FULL label set — increase() preserves the input labels, so exp_labels needed workspace+provider_slug / workspace+reason, and exp_annotations the complete descriptions. Fixed (CI-red closed).
- The fallbackMode enum is FAIL-LOUD at load (validateRelayFallbackMode — "srtict" refuses boot, never silently arming the fail-open path; the repo's config convention). Pinned.
- The four stale "never a raw fallback" comments corrected to the two-mode truth (relay_batch.go ×2, injection.go, relay_handoff.go ×2) — each now names strict-mute vs migration-counted.
- The e2e gate: STANDING per the recorded order (the M2/M4 story owns it — next in my queue, not deferred into nothing; the disposition stated in the PR body since r0).

## r3 — the e2e gate ABSORBED (the reviewer's explicit alternative)

The sole r2-blocking finding was the e2e gate (both delivery modes, zero cluster coverage). Rather than sequencing a separate story PR behind this one, this PR absorbs the M2/M4 e2e migration scenario (the design §10 row that was my next recorded item anyway):
- `local/us72-m2-fallback-e2e.sh` — R1 the migration scenario (the first workspace-relay-* handoff appears = #1548's AC1 final clause; CredentialsStaged=True; the batch carries the TOKEN not the canary; zero relay_staging_not_ready degrades — AC4's superseded-literal-must-still-hold-when-healthy) and R2 the fallback arm (handoff torn → the RAW canary delivers — the migration trade asserted in BOTH directions; relay_fallback_deliveries_total{workspace,provider_slug} read from the API's /metrics with BOTH labels pinned; CredentialsStaged=False/relay_fallback_delivery — the wt-1453 seam contract live).
- The resync trigger: the direct pod :4097 /v1/resync-secrets (the drill's precedent) with the suspend/activate fallback.
- 5 shape pins (rows-in-order with all seven verdict markers; token-AND-raw both asserted — one-direction checks can pass both rows on a stale config; counter labels; isolation; syntax).
- First recorded execution: the standing disposition (reviewer runner / #1456).

## r4 — the dead-on-arrival scenario fixed; the smoke landed

- The one-character fix attempt: WS_BASE's first segment was 9 chars (e2e72m2e0). [r5 correction: the r4 "fix" e2e072m2- was ALSO invalid — non-hex 'm'; PostgreSQL rejects the derived id at the seed INSERT before any row. Fixed r5: e2e07250- (all-hex 8), the TestIssue1342-canonical-pin precedent ADDED (the repo had the exact pin for this class; I didn't look).]
- The ExecuteSmoke LANDED — [r5 correction: the r4 record claimed it "catches the invalid-UUID death in ~9s" — FALSE, the reviewer ran the corpse BOTH ways and the smoke passes on both: the psql shim answers rc-0 without inspecting SQL, so DB-side deaths are structurally invisible to it. Its true depth: generic two-row traversal, no runtime abort — the test comment now says exactly that, and the canonical-UUID pin carries the DB-side class.]
- R1 now asserts BOTH degrade reasons absent (the design §10 clause: relay_staging_not_ready AND relay_fallback_delivery — the latter via the PRE_METRICS counter baseline, which R2 then reuses for its delta).
- The counter assertion is a DELTA (pre-tear baseline vs post-resync) — a stale series from a prior run can no longer satisfy it.
- R1's token read and R2's raw read POLL (the boot-batch-vs-resync-apply race the reviewer named).

## r5 — the second corpse; the canonical pin; the smoke's depth honest

- The r4 base was STILL invalid (non-hex 'm' in segment 1 — a runtime-only death, invisible to every needle AND to the smoke's rc-0 psql shim). Fixed: e2e07250- (all-hex). The TestUS72M2E2E_WorkspaceIDCanonical pin added — the repo's own precedent (TestIssue1342E2EScript_WorkspaceIDCanonical) caught exactly this class in milliseconds; I hadn't looked for it. Both prior corpses now fail this pin instantly.
- The smoke's depth corrected — [r6 correction of THIS r5 entry: the worklog WAS corrected in place (above), but the TEST COMMENT was not — the r5 "both corrected in place" and the commit message's "corrected in comment + worklog" were false at push (the edit had been drafted, not applied — the recurring class). The comment correction actually lands in r6.]

## r6 — the two-line record fix (actually applied, read back); the 1452 follow-up

- The smoke comment's false parenthetical ACTUALLY replaced this round (the r5 edit had been drafted-not-applied — the class again; this entry written after reading the file back).
- The r5 worklog sentence corrected: only the worklog was corrected in r5; the comment correction lands HERE.
- The issue1452 follow-up (r6 finding 2, the last class instance in local/): its default-form WS_BASE carried the same 9-char corpse. [r7 correction: the r6 entry's "the :- default IS the live literal" was FALSE — the lib shadows WS_BASE at source time, so the default form was DEAD and the r6 pin guarded a corpse that could never run; the reviewer traced the death chain as unreachable under the documented semantics. The assignment is now UNCONDITIONAL (the 1342/1455 precedent), the pin matches the live literal, and the r6 pin-comment's parallel claim is corrected.]
