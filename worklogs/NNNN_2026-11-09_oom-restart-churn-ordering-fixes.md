# Worklog: OOM Incident Response — Restart Churn, Queue Ordering, Log Storms

**Date:** 2026-11-09
**Session:** Production incident triage on the home cluster (llmsafespaces ns) + fixes for the four validated issue classes
**Status:** Complete (review round 1 addressed)

---

## Objective

Diagnose why chat sessions were dead in the frontend and the LLM unresponsive, then fix every validated contributing cause except the API OOM memory ceiling itself (fixed by raising api resources 1Gi→2Gi / 128Mi→512Mi).

---

## Work Completed

### 1. Incident triage (live cluster: `admin@home-kubernetes`, ns `llmsafespaces`)

- Both API replicas sat at ~1Gi of their 1Gi limit; `wvb5n` OOMKilled **8x in 4.5d**; on 2026-11-09 ~03:19 UTC both replicas + a workspace container OOM-died simultaneously.
- Failure chain: API dead → frontend blank; workspace pod restart → secrets re-materialize → back-to-back `opencode restart scheduled (env_secrets_changed)` ~46s apart → queued chat messages stuck `delivering` with `transient delivery failure: context deadline exceeded`.
- The 04:56:46 `daring-honeycomb-blooms` halt: opencode step-16 stream stalled 67s on the relay then self-cancelled (`error=Aborted`); no `session.next.step.failed` event emitted — external agent gap, surfacing path (`EVENT_TYPE_ERROR` → frontend ⚠️) already exists end-to-end.
- Validated as NOT defects: postgres-init job re-runs (helm `pre-install,pre-upgrade` hook with `before-hook-creation`, by design); workspace pod OOM root cause indeterminate (no node memory pressure, OOM events expired, 460MB/8Gi at check — needs live telemetry to chase).

### 2. Fix A — restart suppression on unchanged restart-relevant payload (`cmd/workspace-agentd`)

- TDD: `restart_payload_test.go` written first (4 tests), red, then implementation.
- New `restart_payload.go`: `restartPayloadHash` (sha256 over sorted `type|name|value` for `env-secret`/`api-key` entries; `""` sentinel for non-relevant batches) + tmpfs sibling file `<secrets-env>.restart-payload` (same lifecycle as the rev anchor: pod death wipes both; partial failure never anchors).
- `applySecretsBatch` restart block (`secrets.go`): gate the marker/metric/decision block on payload identity — byte-identical re-apply schedules NO restart; unreadable hash fails open; partial failure never persists the hash. The `hasFileClassEntries` refresh path is untouched (still runs when suppressed — `restarted=false`).

### 3. Fix C — reconcile log-storm aggregation (`api/internal/handlers`)

- TDD: `proxy_session_reconcile_logstorm_test.go` (3 tests; new `reconcileWarnCapture` logger), red, then implementation.
- `proxy_session_index.go`: per-row `kept_for_operator` and `count_skip_not_present` Warns aggregated to ONE Warn per pass (`count` + `sampleIDs` capped at new `reconcileWarnIDSamples=10`). Per-row outcome metrics unchanged (they carry the diagnosis).

### 4. Fix B — landed-user reconcile, queued-message ordering (`frontend`)

- TDD: 2 tests in `ChatPage.sse.test.tsx` (drop-on-land + #447 keep case), red, then implementation.
- `ChatPage.tsx`: new `reconcileLandedUserMessage` useCallback — on harness `MESSAGE_START` (USER), refetch history and drop optimistic twins whose `messageIdentityKey` appears in the refetched history. A queued message delivered minutes late now takes its backend landing position immediately instead of the client-clock bubble staying pinned below live agent parts. Hookcount guard 77→78.
- Harness hardening: ChatView mock composer value moved to a module-level ref (`mockComposerValue`) — the uncontrolled textarea lost typed text on re-render, making identity assertions impossible; `userEvent.type` in this harness never reaches the mock's onChange (pre-existing; the existing send test asserted only call-count).

### 5. Environment repair (pre-existing breakage)

- `frontend/node_modules` was missing `mermaid` (in package.json) — 19 test suites failed to collect. `npm install` restored it; full suite green.

---

## Key Decisions

- **Pod-side content gate over server-side reconcile fix**: the ~46s restart cadence originates in `secretsreconcile` notify classification, but the pod-side suppression kills the visible churn regardless of which server path minted the redundant pull, and is contained to the agentd seam (Rule 12). Server-side classifier left for the in-flight US-70.3/design-0064 work (untracked WIP present in the tree — not mine, untouched).
- **Suppress on identity, not debounce**: a min-interval coalescer would delay genuinely-changed credentials; identity suppression never delays a real change.
- **MESSAGE_START as the persistence proof**: closing the loop at harness message-start rather than queue-pill clear avoids the #447 eventual-consistency window (drop still requires the refetched history to contain the identity).
- **Aggregated logs, per-row metrics**: operators lose no diagnosis signal (metrics are per-row); only log volume is bounded.
- **Not fixed — aborted-turn auto-retry**: auto-resuming a mid-turn abort risks duplicate tool side-effects; the correct surface (ERROR event → ⚠️ bubble) already exists and the missing opencode event is external.

---

## Blockers

- None for this work. (Note: App-identity git auth unavailable in this environment; the platform-provisioned `GH_TOKEN` mechanism is used per README-LLM §0b's credential-mechanism clause.)

---

## Review Round 1 (AI reviewer, 2026-11-10)

- **Finding 1 (blocking, accepted)**: restart-payload anchor read/compare/write sat OUTSIDE applyMu; a parked restart from apply A could land its anchor AFTER apply B's — a stale identity over the child's env, wrongly suppressing a needed restart. Fixed: gate read + anchor write moved inside the locked section (serialized in pipeline order); restart decision stays outside. Regression test `TestApplyBatch_ConcurrentAnchorsSerializeInPipelineOrder` pins the interleaving with a CAS-gated blocking proc.
- **Finding 2 (blocking, accepted)**: `reconcileLandedUserMessage` refetched all cached pages per MESSAGE_START (stale cursors, growing with depth). Fixed: mirrors reconcileOnIdle's page-1 slice before refetch (the landed message is the newest and lands in page 1).
- Minor (accepted): `kept_spawn_window` per-row Warn folded into the same per-pass aggregate. `reap_deferred_probe` intentionally stays per-row — each line carries a distinct probe error string that an aggregate would destroy.
- Missing tests added: concurrent-anchor race (above), unreadable-anchor fail-open (`TestApplyBatch_UnreadablePayloadHashFailsOpen`, skipped as root), sample-cap boundary (`TestSessionIndexReconcile_LogStormSampleCapBoundary`). Socket-topology composition pin deferred (suite green; ordering unpinned) — noted as follow-up.
- CI gates repaired en route: x/net 0.62.0 + klauspost/compress 1.20.2 (fresh vuln DB findings), frontend-test + sdk-contract jobs pinned to ubuntu-24.04 (26.04 runner image broke playwright install + hurl/libxml2), epic67 test flake fixed (call-local http client), gosec G703 function-level nolint on atomicWriteAny (repo convention), staticcheck QF1012.

## Tests Run

- `go test ./cmd/workspace-agentd/ -run 'TestApplyBatch_.*(Payload|ApiKey)|TestRestartPayloadHash_Canonicalization' -timeout 180s` — 4/4 pass (after red)
- `go test ./cmd/workspace-agentd/... -timeout 900s` — agentd + faultmatrix pass (pre-existing `sessionstate/busy_age_test.go` build break: design 0064 S1 TDD-red WIP authored before its derivation — deliberately untouched)
- `go test ./api/internal/handlers/ -timeout 900s` — all pass (95s)
- `go test ./api/internal/handlers/ -run TestSessionIndexReconcile` — all pass
- `go vet ./cmd/workspace-agentd/... ./api/internal/handlers/...` — clean
- `cd frontend && npx vitest run` — **2491/2491 pass** (218 files)
- `cd frontend && npx tsc --noEmit` — clean; `npm run lint` — 0 errors, 7 warnings (verified identical pre-change via stash)

---

## Next Steps

- talos-ops-prod PR: the `api.resources` override (512Mi/2Gi) in `kubernetes/apps/llmsafespaces/llmsafespaces/app/helm-release.yaml` — merge together with this PR so flux rolls the ceiling before the next boot-flood cycle.
- Consider a `restarts_suppressed` counter for the single-container topology (the socket topology already has one) so suppression is scrape-visible, not log-only.
- Revisit `secretsreconcile` server-side seq minting when US-70.3 lands; the classify-notify-apply loop still burns pulls on unchanged content server-side.
- Chase the workspace-container OOM (03:19, exit 137 at 8Gi with 460MB steady-state) if it recurs — needs retained node/cgroup telemetry.

---

## Files Modified

- `helm/values.yaml` — api resources 128Mi/1Gi → 512Mi/2Gi
- `cmd/workspace-agentd/secrets.go` — payload-identity gate around the secrets restart block
- `cmd/workspace-agentd/restart_payload.go` — NEW: payload hash + tmpfs anchor helpers
- `cmd/workspace-agentd/restart_payload_test.go` — NEW: 4 tests
- `api/internal/handlers/proxy_session_index.go` — warn aggregation (kept_for_operator, count_skip_not_present) + `reconcileWarnIDSamples`
- `api/internal/handlers/proxy_session_reconcile_logstorm_test.go` — NEW: 3 tests + capture logger
- `frontend/src/pages/ChatPage.tsx` — `reconcileLandedUserMessage` + MESSAGE_START wiring
- `frontend/src/pages/ChatPage.sse.test.tsx` — 2 tests + ChatView mock composer-value fix
- `frontend/src/pages/ChatPage.hookcount.test.tsx` — EXPECTED_HOOK_CALLS 77→78
