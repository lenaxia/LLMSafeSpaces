# Worklog: #828 batch 2 — session cluster + prompt/queue adapter-only

**Date:** 2026-09-12
**Session:** epic-826 / #828 batch 2 (claim: [#828 comment](https://github.com/lenaxia/LLMSafeSpaces/issues/828#issuecomment-5641340986)). Agent: opencode.
**Status:** Complete

---

## Objective

Migrate the remaining 7 `proxy_handlers.go` dual-path sites to adapter-only, deleting their legacy tails (including the whole V2 queue surface and the bespoke history fetch path) in the same PR.

---

## Work Completed

### Handlers migrated (8 → 0 dual-path gates in proxy_handlers.go; 11 by batch-1 site count)

- **SendPromptAsync / EnqueueMessage**: nil-adapter guard after shared validation; outbox arm now keys on `h.outbox != nil` alone; both sync fallbacks unified into a new `syncSend` seam (session-limit → quota → model-policy → `adapter.Send` → enrichment → postAdapterSuccess). The #944 disk-full 507 classification is preserved inside `syncSend`'s error path.
- **GetHistory**: guard + adapter arm un-indented; bespoke legacy tail deleted (`fetchUpstreamHistory`, `doHistoryRequest`, `paginateOpencodeHistory`, `stripPaginationQuery`).
- **GetSession / AbortSession / DeleteSession**: guards; `proxyToWorkspace`/`abortV2` tails deleted. DeleteSession side effects (tombstone, index cleanup, SSE, active-session removal) still run only after a successful adapter delete — pinned by `TestDeleteSession_NilAdapter_Returns503_AndWritesNoTombstone` + `TestDeleteSession_AdapterPath_StillTombstones`.
- **RenameSessionInAgent**: guard returns a typed error; bespoke raw-PATCH tail deleted.

### V2 surface deleted whole

`proxy_v2.go` (enqueueV2, abortV2, v2Client, both factory setters), `proxy.go`'s factory fields, `app.go`'s wiring, and `pkg/agent`'s `V2SessionClient`/`V2ClientFactory` interface. The adapter uses its own `PromptV2WithModel` — unaffected. 7 V2-path tests deleted with the code; 2 semantic rows ported (validation-precedes-guard; no-409-guard); the 2 statusz-reconcile tests moved to their own file (`proxy_reconcile_state_test.go`); the V2 test harness retained (sans factory) as the guard-rows' red-state witness.

### Activity-recording parity (batch-1 gap found and fixed)

The legacy transport recorded workspace activity on every 2xx proxy; the adapter arms bypass the transport and batch 1 did not replace the recording for create/list. Added `recordActivityIfTracked` and wired it into CreateSession, ListSessions (batch-1 sites), GetSession, GetHistory. `TestProxy_ActivityRecordedOnSuccess` ported to the adapter path; the failure row rides the read seam.

### Test migration (53 failing rows → green)

- Transport generics re-pointed from GetSession/GetHistory to a new **`registerLegacyReadTransport` seam** (Any-method, the exact pre-batch-2 GetSession transport call) — batch-1's `legacy-message` seam gains a read sibling.
- DeleteSession rows (13) ported to `mockAdapter`; the 404-passthrough row became the adapter-error-502 row (typed error contract; no tombstone on failure).
- GetHistory contract rows (8) ported to the `newE2EEnv` adapter harness; `extractIDs` reads the contract shape (top-level id).
- **Contract deltas surfaced and pinned** (not silently absorbed):
  1. *Slice-then-translate vs filter-then-slice* — the adapter page is the newest N RAW messages; step-marker-only messages surface as empty-parts contract messages; system roles are contract data. Production behavior since #971; pinned with rationale in `TestGetHistory_PageIsRawSlice_TranslateAfterwards (renamed r2)`.
  2. *Optimistic cursor* (#971) — a full native page emits a cursor even when no older messages exist; the exact-boundary row now pins this documented contract.
  3. *History upstream-5xx Prometheus counter dies* with `doHistoryRequest` — adapter path has structured error logs, no counter. **Flagged for reviewer** (inventing a synthetic status label would corrupt the metric); the counter survives for the still-legacy transport routes until the final batch.
- Deleted with rationale: history query-forwarding row (upstream query is now the adapter's; `before` never forwarded — asserted in the FullWalk port), history fetch-error row (adapter 502 equivalent exists), transport-retry row (generic retry pinned on seam), history 5xx observability ×2, `TestGetHistory_4xx_NoEnrichment` (bespoke-path enrichment passthrough).
- `contract_auth`: question/permission routes are now the only backend-reaching raw-proxy surface (dialect wired in the fixture); migrated routes marked guarded.

---

## Key Decisions

1. `syncSend` extraction — EnqueueMessage's body is consumed by binding, so it cannot delegate to the SendPromptAsync handler; one seam serves both non-outbox fallbacks (dev/test only in production — the outbox is the accept path).
2. Adapter arms record activity explicitly (`recordActivityIfTracked`) rather than pulling `postAdapterSuccess` (which also meters and indexes — wrong for reads).
3. The three contract deltas above are pinned with comments + surfaced in the PR body, not normalized away.

---

## Assumptions (Rule 7 — stated and validated)

- A1: production always sets adapter + outbox (app.go) — the `syncSend`/outbox-less paths are dev/test surfaces → validated in app.go wiring.
- A2: `V2SessionClient` interface had no consumers beyond proxy_v2.go → validated by repo-wide grep (adapter uses `PromptV2WithModel`).
- A3: history pagination contract on the adapter path is unchanged from #971 production behavior → validated by porting the FullWalk/FirstMessage rows green against the real adapter.
- A4: `TestRollbackDrill_UnderLoad` failure in one local run was load flake (outbox stream, not touched here) → passed on the clean full run; CI will adjudicate.

---

## Blockers

None.

---

## Tests Run

- `go test ./api/internal/handlers/` — ok (96.8s; 53 previously failing rows green)
- `go test ./api/... ./pkg/agent/...` — ok
- `go vet` + `gofmt` — clean
- New guard rows red-first: 7 rows failed against working legacy tails (V2 server / proxy backend) before migration.

---

## Next Steps

1. Batch 3: input/permissions cluster (`proxy_input.go` ×6, `proxy_permissions.go`) — the epic-71 wave-4a gate (#1302).
2. Batch 4: session-index/parents (fetchAndPersistTitle, runParentBackfill, fetchSessionParent).
3. Final batch: adapter as required ctor param; delete `proxyToWorkspace*`/`doProxy` + both test seams + their suites; repolint zero-site gate; upstream-5xx counter decision.

---

## Files Modified

- api/internal/handlers/proxy_handlers.go (7 handlers migrated; history helpers deleted; syncSend added)
- api/internal/handlers/proxy.go (V2 factory fields deleted)
- api/internal/handlers/proxy_v2.go (deleted)
- api/internal/app/app.go (V2 wiring deleted)
- pkg/agent/agent.go (V2SessionClient/V2ClientFactory deleted)
- api/internal/handlers/proxy_adapter_crosscutting.go (recordActivityIfTracked)
- api/internal/handlers/proxy_batch2_migration_test.go (new — 12 rows after r1)
- api/internal/handlers/proxy_test_helpers_test.go (read seam; V2 harness retained)
- api/internal/handlers/proxy_reconcile_state_test.go (new — moved rows)
- api/internal/handlers/proxy_test.go, proxy_history_pagination_test.go, proxy_history_e2e_test.go, proxy_chat_buffering_test.go, proxy_upstream_5xx_observability_test.go, proxy_request_buffer_test.go, proxy_auth_cache_test.go, proxy_terminal_events_test.go, proxy_send_logging_test.go, contract_auth_test.go (re-points/ports/deletions with rationale)

---

## Review r1 + r2 remediation (PR #1349)

- **r1:** EnqueueMessage→syncSend coverage added (sync-send happy path + empty-text 400-precedes-guard); DeleteSession failure row's negative assertions restored (live SSE subscriber + index-not-called + no-tombstone); `agent.IsSessionNotFound` deleted; always-true outbox guard dropped; history rows renamed to match their pins; comment sweep (partial — completed in r2).
- **r2:** GetHistory doc tail rewritten to the adapter contract; renamed tests' doc comments rewritten; dangling refs swept (FullFlow NOTE, paginateContractHistory doc, V2Delivery doc, metrics tail, e2e fragment); `helloText` inlined.
- Reviewer verified the r1 coverage fixes fail-without-fix (the `msg_sync_1` assertion is only producible via adapter.Send). Delta-3 (5xx counter) call: accepted — no synthetic label.
- `TestOutboxDeliver_V2NoPromotionNeverFalselyCompletes`: load-sensitive flake (failed once in one full-suite run; green x3 isolated on branch AND main) — timing-robustness follow-up noted for the outbox owner.
- CI: Build Frontend (arm64) failed once with buildx `error writing layer blob: not_found` (registry infra); green on rerun.

## Tests Run (r2)

- `go test ./api/internal/handlers/ ./pkg/agent/` — green; vet/gofmt clean

---

## Review r3 + r4 remediation + outbox deflake (PR #1349)

- **r3 (comment-only):** metrics counter-emission "both" claim, DeleteSession legacy-path refs, observability doProxy route list, worklog count qualifier — all fixed.
- **r4 (comment-only):** contract_auth analogy scoped to the enqueue row; "outbox unconditionally" → "on the production cache-service path"; outbox comment's async clause deleted (deliverDetached is synchronous context-detachment).
- **Outbox deflake (in-PR, test-only):** `TestOutboxDeliver_V2NoPromotionNeverFalselyCompletes` pass-3 raced the shrunk backoff window (instant `DeliverOutboxOnceForTest` returned false ~1/5 under load) — now polls for due-ness + `assert.Eventually` the re-admit counter; 20x green with `-race`. Flagged on #1314 for the 0b owner.
- Reviewer's r4 live-check note (repolint `TestLive_Worklogs_NoDuplicates` red on then-main via an unnumbered #1334 sentinel) no longer applies: the sentinel was numbered when #1334 merged; the live check is green on current main.

---

## Review r5 remediation + ops (PR #1349)

- **r5 (comment-only):** faultmatrix dedupe provenance corrected (#1334 re-introduced the old body; #1346's dedupe never merged — the kept copy is #1344's instrumented one); contract_auth BasicAuth pin repointed to `pkg/agent/opencode/adapter_test.go` (rejecting fake) + e2e presence check; stale repolint live-check note updated (the #1334 sentinel was numbered at merge).
- **Main-red #2 this cycle:** the faultmatrix duplicate returned via #1334-on-#1344 stacking; deduped again on this branch (kept #1344's copy — the soak rows consume its counters) + flagged the recurrence class on #1314.
- **Ops:** workflow-event drop recurred on ref `f365db08`→`c709a9f8` (zero runs on the pushed head); re-armed with this append. Outbox no-promotion row deflaked in-PR (poll due-pass + Eventually counter; 20x -race green).

- Event-pipeline recovery note: PR #1354 opened during the repo-wide pull_request-event outage; re-armed via this commit once issue_comment events confirmed the pipeline alive.

---

## Review r6 remediation (PR #1354)

- **Activity parity completed for write routes:** AbortSession and DeleteSession now `recordActivityIfTracked` on their 2xx paths (the deleted transport recorded every 2xx; the r1 parity fix had covered reads only).
- **Guard-row harness header corrected:** the three V2-harness rows' red state is a 500 (no V2 client post-deletion), distinct from the typed 503 by body; rows were captured red-first against the real tails pre-deletion.
- **Sweep:** ListQueue/DeleteQueueMessage V2-shadow comments rewritten (no queue without the outbox); `proxy.go` adapter field doc updated to the post-batch-2 reality (fail-closed guards; remaining nil-checks are batch-3/4 files).
- Reviewer accepted both surfaced contract deltas (outbox-less queue 200-sync; counter delta-3) and confirmed zero missing test cases.

---

## Review r7-r9 remediation (PR #1354)

- **r7:** write-route activity parity lines pinned (AbortSession/DeleteSession tracker rows, mutation-verified by both reviewer and me); helper/SetAdapter/AbortSession docs corrected.
- **r8:** SetAdapter doc re-scoped to the migrated cluster (queue-view + rename exceptions); per-phase tracker isolation in the pins.
- **r9:** the adapter FIELD doc re-scoped identically (r8 fixed the sibling 75 lines below and missed this one); guard-row header's red-state mechanism corrected (nil-deref panic under bare gin.New(), not a 500 — mutation-verified by the reviewer); helpers "red-state witness" wording aligned; stale admin_session.go DeleteSession cite fixed (non-blocking, pre-existing); worklog rounds completed (this section).

- **r10:** field doc's nil-check enumeration completed (lifecycle Start() hooks + events sweep gate); guard-row panic claim scoped to the three guard rows and attributed to the nil adapter; "typed error" -> "its own error" for the rename helper.
