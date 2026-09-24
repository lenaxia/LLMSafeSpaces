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
