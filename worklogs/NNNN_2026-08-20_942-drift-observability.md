# Worklog: #942 — drift observability, literal lint, bump gate, env relocation, runbook

**Date:** 2026-08-20
**Session:** Implement issue #942 end-to-end: the drift-observability layer that converts silent wire drift (the #739 failure class) into counters, warns, and a gated runbook; the event-name-literal lint; the opencode-bump→fixture-refresh gate; and the env-flag relocation out of the controller.
**Status:** Complete

---

## Objective

Close the exposure identified when deferring was discussed: the next opencode bump could ship with CI green and production silently broken (gauge frozen, billing zeroed). Five scopes, one PR.

---

## Work Completed

### 1. Drift observability (#739 Gap 2)

- **`wire.IsKnownEventType`** — pinned taxonomy (fixture types + legacy mixed-fleet names; numeric-suffix tolerance matching `isSuffixed`). `TestIsKnownEventType_CoversBothFixtures` forces every fixture type into the taxonomy: a fixture refresh that introduces a type must extend the set in the same change.
- **`Adapter.IsKnownEventType`** on the seam interface; opencode impl via wire.
- **`metrics.RecordAgentEvent`** — `llmsafespaces_agent_events_total{event_type}`; unknown types bucket under the single `unknown` label (cardinality bounded by taxonomy, documented on the `AgentEventMetricsRecorder` interface — the #947 log-before-err lesson applied: contracts live on the interface, not just the impl).
- **Tracker `observeAgentEvent`** in the single dispatch funnel (validator-verified: flat + nested + public DispatchProperties all route through dispatchProperties): known → counted by name; unknown → counted + **warned exactly once per distinct type** (`unknownSeen` capped at 64 — counting never stops, log flooding impossible; cap pinned by test). Classifier + metrics recorder injected (nil ⇒ observability off, never misclassification); classifier wired at tracker construction (`newSSETracker`), recorder in app.go next to `SetSessionMetrics`. Both wiring points pinned behaviorally (`TestNewSSETracker_DriftObservabilityWiredAtConstruction`).

**Drift signature this makes visible:** a taxonomy rename upstream = known series flatlines + `unknown` grows + a named warn — instead of events silently vanishing (the billing-silent-zero scenario).

### 2. Event-name-literal lint (`pkg/repolint/event_literal.go`)

- Flags comparison-context string matches on agent event literals (`==`/`!=`, `case` incl. comma-lists, map-key `m["lit"]`, `strings.Contains/HasPrefix/…`) in non-test .go outside the seam (pkg/agent/opencode/, cmd/workspace-agentd/, pkg/repolint/). Struct-literal **emissions** are the platform's own event names — deliberately not flagged.
- `session.status` deliberately excluded: the platform's SSE broker emits an event of the same name, making lexical discrimination between the broker stream and the agent wire impossible (documented in the rule). The real-repo test caught `pkg/mcp/client.go` matching the broker's `session.status` — correctly exempt once the collision was understood.
- knownLeaks: one live entry (proxy_events.go title persistence — folds into the adapter with #940). Two dead entries removed after validator's dead-key finding (rationale preserved as comments). Registered in the CLI (`runEventLiteral`); real-repo test fails on any NEW match.

### 3. opencode-bump → fixture-refresh gate

- **Pre-commit**: staged OPENCODE_VERSION change in runtimes/*/Dockerfile without a staged pkg/agent/opencode/testdata/ change → block.
- **CI**: same check against origin/main...HEAD on PRs. Validator found the first version hard-failed EVERY PR (depth-1 checkout grafts HEAD parentless → no merge-base); fixed with `fetch-depth: 0` on the lint job.

### 4. `OPENCODE_EXPERIMENTAL_EVENT_SYSTEM` relocation

- Controller pod env removed (an opencode env name is runtime knowledge, not platform knowledge); the runtime entrypoint exports it before `exec agentd --supervise` (env inheritance verified by validator: agentd builds child env from os.Environ). Pins: controller test now asserts ABSENCE in pod env; repolint test asserts PRESENCE in the entrypoint (the guardian after relocation). Rollout-coupling documented in the runbook (image + controller deploy together — the regression window validator #3 flagged).

### 5. Runbook (testdata/REFRESH.md)

Upgrade procedure (pre-merge capture steps, extend-don't-loosen taxonomy rules, pinned-count discipline), post-deploy checks (drift counters, `context_used` non-NULL — the #739 DoD item, billing sanity), recommended alert (`increase(...{event_type="unknown"}[15m]) > 0`), rollout-coupling checklist.

### Validator loop

Independent validator: 1 HIGH (CI gate merge-base — fixed via fetch-depth), plus dead knownLeaks (removed), rollout-coupling gap (documented), lint false-negative contexts (patterns added + pinned), taxonomy comment accuracy, interface-contract omission (fixed), missing worklog (this file), 2 nits (fixed). Warn-cap semantics pinned by test per finding 9.

---

## Key Decisions

| Decision | Rationale |
|---|---|
| Unknown types under one `unknown` label, not per-type labels | Unbounded upstream creativity vs bounded taxonomy; the per-type identity lives in the once-per-type warn (greppable), volume lives in the counter. |
| Counting only when classifier AND recorder both injected | A missing injection is a wiring gap, not drift — never misclassify as unknown. Both wiring points behaviorally pinned. |
| `session.status` excluded from the literal rule | Platform broker emits the same name; lexical discrimination impossible. The seam-side and agentd uses are allowlisted anyway. |
| Emissions not flagged | The platform's own SSE taxonomy legitimately reuses names; only matching contexts are drift-prone. |
| Warn-once cap at 64 distinct types | A taxonomy-rewrite upstream cannot flood the log; counting continues regardless. |

---

## Blockers

None.

---

## Tests Run

- `go test ./pkg/repolint/ ./pkg/agent/opencode/wire/ ./api/internal/services/sse/ ./api/internal/services/metrics/ ./controller/internal/workspace/` — ok
- `go test ./api/internal/handlers/` — ok (85s)
- `go build ./...`, `go run ./cmd/repolint -repo .` — all checks passed (agent event literals: 1 known leak tolerated)

---

## Next Steps

1. Post-merge: verify the CI fixture gate runs green on this PR (it's the gate's own first live exercise).
2. Live-cluster validation after deploy per the runbook (counters + context_used + billing sanity) — then close #942 with evidence.
3. Remaining roadmap: #941 (usage-authority cutover), #940 (Window wiring), US-65.8 frontend (the user-visible last mile).

---

## Files Modified

- `pkg/agent/opencode/wire/wire.go`, `wire_test.go` — taxonomy + tests
- `pkg/agent/adapter.go`, `adapter_test.go` — interface method + fake
- `pkg/agent/opencode/adapter.go` — impl
- `api/internal/services/metrics/metrics.go`, `metrics_test.go` — counter + test
- `api/internal/services/sse/tracker.go`, `tracker_regression_test.go` — observability funnel + tests
- `api/internal/handlers/proxy_lifecycle.go`, `context_usage_adapter_e2e_test.go` — wiring + pin
- `api/internal/handlers/mock_adapter_test.go` — mock
- `api/internal/app/app.go` — recorder wiring
- `pkg/repolint/event_literal.go`, `event_literal_test.go` — NEW rule + tests
- `pkg/repolint/entrypoint_event_flag_test.go` — NEW entrypoint pin
- `cmd/repolint/main.go` — check registration
- `.githooks/pre-commit` — bump gate
- `.github/workflows/ci.yml` — CI bump gate + full-history checkout
- `controller/internal/workspace/pod_builder.go`, `pod_builder_test.go` — env removal + absence pin
- `runtimes/base/tools/entrypoints/entrypoint-opencode.sh` — flag export
- `pkg/agent/opencode/testdata/REFRESH.md` — runbook
