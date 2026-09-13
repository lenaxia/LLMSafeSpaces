# Worklog: #828 final batch — required adapter, transport deletion, repolint gate

**Date:** 2026-09-13
**Session:** epic-826 / #828 final batch (claim: [#828 comment](https://github.com/lenaxia/LLMSafeSpaces/issues/828#issuecomment-5641340986)). Agent: opencode.
**Status:** Complete

---

## Objective

Close #828: adapter as a required constructor parameter (every nil-check collapses), delete the raw-proxy transport and everything that existed to serve it, and add the repolint-style zero-site gate.

---

## Work Completed

### 1. ResolverHost (the construction-order enabler)

The chicken-and-egg — the adapter needs handler-owned resolvers (password cache, pod-IP lookup) while the handler ctor now needs the adapter — is broken by extracting `ResolverHost` (`resolver_host.go`): k8s client + logger + namespace + the state store, exposing `GetPassword` / `GetWorkspacePodIP` / `State` / `SetStateStore`. app.go builds the host first, builds the adapter over it, passes the adapter to the ctor, then adopts the host via `SetResolverHost` (pre-Start panic guard, same invariant as SetStateStore — which now forwards into the host, so the Redis swap reaches the adapter's resolver). The handler's `getPassword`/`state()`/resolver bridges delegate to the host; the old `proxyPodIPResolver` dies.

### 2. Required adapter

- `NewProxyHandler(k8s, log, ns, httpClient, adapter)` — nil adapter → construction error.
- `SetAdapter` deleted; out-of-package tests get `SetAdapterForTest` (in-package tests assign the field).
- **All 30 nil-checks deleted**: the 10 route guards + rename helper, the input cluster's 8, permissions, session-index ×3, parents, stream/user-events flight gates (now unconditional), lifecycle Start's outbox hooks (unconditional), the events phase-change sweep gate, askLivenessOf's unknown arm, tryLateAnswer's compound.
- **DeleteSession/AbortSession gain `resolveWorkspaceForAdapter`** — a pre-existing batch-2 omission surfaced when its coincidental guard-503 pin (the WorkspaceNotActive rows were passing via the nil-guard's 503, not a phase check) disappeared. Now cluster-consistent: 404/503+Retry-After/ceiling enforced, pinned by the repaired rows.

### 3. Transport deletion

`proxyToWorkspaceWithErrBody`, `doProxy`, both test seams, and everything orphaned with them:
- `proxy_request_buffer.go` + its config keys (`requestBufferSizePerWorkspace`/`TimeoutSeconds`) + `SetRequestBufferConfig` + app.go wiring + the five request-buffer metric families + their Record fns (the buffer's only consumer was the transport's bufferable arm).
- `proxy_upstream_observability.go` + the `upstream5xxTotal` family + `RecordUpstream5xx` + `sanitizePathForMetric` — the batch-2 r6 delta-3 ruling made formal: adapter-path failures surface via structured error logs and `api_requests_total{status}`, no synthetic counter label.
- The SSE terminal-event machinery (US-44/B2 agent-died rows), the error-body buffering arms, stale-IP retry, Upstream-401 invalidation, G34 header allowlist e2e — all transport-only contracts, tombstoned at their sites.
- ~50 seam-riding test rows deleted with per-row rationale; `stubAgentStateChecker` resurrected in its surviving consumer.

### 4. The gate

`adapter_required_gate_test.go`: `TestNoAdapterNilChecks` — zero `h.adapter == nil`/`!= nil` matches in non-test handler sources, or fail with file:line. The count is 0 today; any reintroduction of the fallback pattern goes red.

---

## Key Decisions

1. **ResolverHost over lazy adapter injection** — preserves the required-parameter semantics without inverting the resolver ownership; one shared password cache + invalidation (SetStateStore forwards).
2. **Lenient in-package mock** (`newLenientMockAdapter`) for env constructors — specific rows overwrite `env.handler.adapter` as before; out-of-package suites use `nullTestAdapter{agent.Adapter}` (panics if actually driven — they never drive it).
3. **Formal 5xx-counter retirement** per the standing r6 ruling, rather than inventing adapter-side labels.

---

## Assumptions (Rule 7 — stated and validated)

- A1: no remaining `requestBuffer`/`doProxy` consumers after transport deletion → validated by build (every reference died or was tombstoned).
- A2: `DeleteSession`/`AbortSession` resolve omission was accidental (batch-2 r1 called it "pre-existing... on main" without mandating a fix) → validated against the batch-2 r1 record; fixed here because its coincidental pin vanished.
- A3: config keys unused outside the deleted wiring → validated (config_test rows were the only other consumers; deleted with rationale).

---

## Blockers

None.

---

## Tests Run

- `go test ./api/... ./pkg/agent/...` — green; `go vet ./...` + `gofmt` clean
- The gate row green; mutation-checked by construction (any nil-check in a non-test file fails it)

---

## Next Steps

1. #828 CLOSES: all four batches + final landed; the claim comment records the completion.
2. 4a (#1302) continues on its own claim.
3. Follow-ups surfaced: `sdks/openapi.yaml` regen (4a scope); the S1/Act retirement of `AnswerQuestion`/`ReplyPermission` (4a scope).

---

## Files Modified

- api/internal/handlers/resolver_host.go (new)
- api/internal/handlers/proxy.go (ctor; guards; transport+buffer+observability deletion; SetResolverHost/SetAdapterForTest)
- api/internal/handlers/proxy_connections.go (host delegation; podIPResolver deleted)
- api/internal/handlers/proxy_handlers.go (guards gone; Delete/Abort resolve)
- api/internal/handlers/proxy_input.go, proxy_permissions.go, proxy_session_index.go, session_parents.go, proxy_stream.go, stream_user_events.go, proxy_events.go, proxy_lifecycle.go, proxy_inbox.go (guards/gates unconditional)
- api/internal/handlers/proxy_request_buffer.go, proxy_upstream_observability.go (+ their test files, terminal-events, chat-buffering, request-buffer, 5xx test files) — deleted
- api/internal/services/metrics/metrics.go (buffer + 5xx families deleted)
- api/internal/config/config.go (+ test) (buffer keys deleted)
- api/internal/app/app.go (host-first construction)
- api/internal/handlers/adapter_required_gate_test.go (new — the gate)
- ~40 test files (ctor sweep, guard-row deletions with rationale, harness ports)

---

## Review r1 remediation (PR #1362)

- **AbortSession's readiness contract pinned** (the unpinned half of the Delete/Abort resolve repair): NotActive-503+Retry-After, NotFound-404, Ceiling-429 — all with adapter-must-not-be-called assertions.
- **ResolverHost empty-password arm executed**: `TestResolverHost_GetPassword_EmptyPasswordKey` re-homes the deleted `TestProxy_EmptyPasswordKey` against the host (production-live on every adapter call).
- **Dead transport helpers deleted** with their suites: `stripVerboseQuery` (+7 rows), `copyResponseHeaders` (+4 rows in auth_cache, +2 in headers_allowlist), `isConnectionError` (+its table row), `blockedResponseHeaders`. `copyRequestHeaders` survives (dev_preview's legitimate consumer).
- **`SetAdapterForTest` deleted** — zero call sites repo-wide (the mcp harness now constructs with the adapter directly).
- **Stale refs swept**: adapter field doc (guards/SetAdapter text → ctor-required reality), modelPolicyChecker's invariant cite, proxy_input's mid-sentence, contract-stream/gauges `DeleteRequestBufferMetrics` cites, .golangci.yml's transport cite.
- **ResolverHost.logger dropped** (assigned-never-read; errors carry the failures).
- **Design-stories knock-on landed**: `design/stories/README.md` epic-65 row records US-65.4/65.5 completed-and-superseded by #828 with the PR trail.
- Cosmetic: redundant parens removed, bare brace blocks unwrapped.
