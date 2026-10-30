# 1233 — Trigger API delete/update 500 on a live-listed trigger: the dead-watcher litter class (2026-10-30 live-fire, tsk-swarthy-bloom-triumphs)

Branch: `trigger-delete-update-500` (worktree `/workspace/trigger-del-upd-500`, off `origin/main` @ `38528b640`).

## 1. The incident (what the orchestrator saw)

Trigger `7df2f7d4-e6df-437b-9311-70892481e997` (`release-v0449-watch`, workspace `7eaa3f1b`, cron `*/3`, the v0.44.9 release watcher with an inline gate script whose filter never matched):

- `trigger_delete` → 500 `{"error":"failed to delete trigger"}` (3 attempts, 04:41–04:49)
- `trigger_update {enabled:false}` → 500 `{"error":"failed to fetch trigger"}` (2 attempts)
- `trigger_list` → the row, perfectly healthy: enabled, `nextFireAt` advancing

Net effect: an immortal every-3-minutes tick — `skipped` fires don't count toward `autoDisableAfter` (failures only), so a healthy-but-useless watcher burns a slot forever while being unkillable through its own verbs.

## 2. Reproduction (live evidence, 07:21–07:24 UTC)

- `trigger_fires` on the live row: 50 visible fires, all `status:"skipped"`, `actionResult.gate.proceed=false` — the gate machinery behaved correctly the whole time.
- **The fire cadence is the forensic record.** Visible fires 04:54–05:21 all ran seconds OFF-schedule (`received_at` :04–:15 past the minute — e.g. 05:00:04, 05:15:04), with drift shrinking until steady :01.25 cadence resumes at 05:27. That is the scheduler CATCHING UP on ticks it missed while the API was down/restarting — the classic backlog signature. The 500 window (04:41–04:49) sits at the FRONT of that degraded window, i.e. inside the v0.44.9 release rollout (the trigger was created 04:16 to watch that very release run).
- **Both verbs succeeded on first try AFTER the window**: my `trigger_update {enabled:false}` at 07:23:43 returned 200 (the live tick is now stopped; the row itself was preserved — it is still the repro row, disabled, with its fire history intact).
- The failure is therefore **condition-dependent (transient store fault), not row-shape-dependent**: no fresh-throwaway repro is possible post-window because the condition left with the rollout. A throwaway trigger with a never-proceed gate script was NOT creatable as a deterministic 500 repro — and the code audit below explains why no row shape could ever produce one deterministically.

## 3. Root cause

### 3.1 Where the 500s live (code)

Both error strings exist only in the platform API's trigger handlers — the pod MCP tools are thin proxies (`PUT/DELETE /api/v1/me/triggers/:id` via `pkg/mcp/client.go`, and the pod-automation delegation `/internal/v1/automation/triggers` funnels into the same handlers):

- `api/internal/handlers/triggers.go` `update()` — the pre-fetch `store.GetTrigger` (runs for every `{enabled:...}` patch) → `failed to fetch trigger` (the update's own store call would have said `failed to update trigger`)
- `api/internal/handlers/triggers.go` `del()` — `store.DeleteTrigger` → `failed to delete trigger`

### 3.2 Deterministic causes excluded (the charter's suspect list, item by item)

- **Divergent read/write paths (cache vs store)**: none. `GetTrigger` (id+owner), `ListTriggers` (owner), `DeleteTrigger`, `UpdateTrigger` are plain parameterized SQL on the same `triggers` table through the same pgxpool; `get`/`list` share `triggerSelectColumns` and the same row scanner. No cache layer exists in front of the workflow store.
- **Soft-delete flag**: none — `DELETE FROM triggers WHERE id/owner_type/owner_id` is a hard delete; `RowsAffected()==0` maps to `ErrNotFound` (which would 404, not 500).
- **A constraint firing on delete (FK from fire rows)**: impossible by schema. Every FK referencing `triggers` is `ON DELETE CASCADE` (`trigger_fires`, `webhooks`) or `ON DELETE SET NULL` (`workflow_runs.trigger_id`, `session_origins.trigger_id`) — migrations 000016/000022. **Pinned on a real PG**: the new `TestTriggerDelete_WithFireHistory_CascadeRoundTrip` creates the exact repro shape (cron `*/3` + never-proceed inline gate script + 3 skipped fires), disables via UpdateTrigger, deletes via DeleteTrigger, and asserts the fires cascade away — delete-with-history is a plain round-trip, never an FK 500.
- **The update path re-fetching by a different key**: no — same `(owner_type, owner_id, id)` triple as list's owner scope.

Why LIST kept working while get/delete 500'd: under connection-class faults (server restarting, connections recycled mid-rollback-deploy), whether a GIVEN statement lands on a live connection or a dying one is per-statement luck; five attempts over eight minutes against a churning pool is exactly the shape you'd expect, and the scheduler's own backlog (§2) proves the API↔DB path was degraded in that window. The 0.44.8→0.44.9 diff touches none of the trigger handler/store paths (checked: only an MCP tool description string), so a mixed-version code bug is also excluded.

### 3.3 The actual defects (what made an infra blip an 8-minute incident)

1. **The 500 arms swallowed the underlying error.** `TriggersHandler` had NO logger, and the arms responded `c.JSON(500, gin.H{"error": "failed to ..."})` discarding `err` — five 500s, zero diagnostic trace. This is why the incident could only be root-caused by inference from fire-cadence forensics instead of one log line.
2. **Retry-blindness.** One transient connection fault was surfaced to the agent caller as a hard, unexplained 500 on BOTH verbs — and for `delete` specifically, a 500-after-uncertain-commit followed by a 404-on-retry is precisely the "undeletable" trap.
3. **Client-canceled contexts mapped to 500.** A pod-churn-canceled request (this fleet restarts pods routinely) returns `context.Canceled` from the store and landed in the same 500 arms — polluting 5xx and mislabeling client events as server faults.

## 4. The fix (`api/internal/handlers/triggers.go`, wiring in `app.go`)

- **`SetLogger` (deferred injection, nil-safe — pod-bootstrap #407 precedent)** wired for both user and org handlers in `app.go`; every 500 arm on the trigger surface now logs the underlying store error with verb + `triggerId`/`ownerType`/`ownerId` + the error's concrete type.
- **`isTransientStoreErr`** — connection-class classification only: PG classes 08 (connection_exception), 53 (insufficient_resources incl. 53300), 57 (operator_intervention — the 57P01/57P02 shutdown family a rolling deploy produces), 58 (system_error), plus wrapped `net.Error`. Deterministic SQL faults (23505, 42601, FK...) and `context.Canceled`/deadlines are deliberately NOT transient (retrying those is useless or load-doubling).
- **One bounded retry (200 ms, context-yielding)** on every trigger-surface store call: the shared `fetchTrigger` (update/get/listFires/rotate), `updateTriggerRow` (single atomic patch statement — re-applying the same patch is idempotent), and `deleteTriggerRow`.
- **Idempotent-complete delete**: if the first delete attempt dies transient and the retry finds the row gone, the delete committed on the dead connection (or another actor removed the row) — either way the caller's goal state holds → 200 `{"deleted":true}` with a Warn log of the rationale. Only a plain first-attempt miss stays 404.
- **499 `statusClientClosedRequest`** for `context.Canceled` with a done request context — a client event, not a server fault; the 500 message strings are unchanged for genuine server faults (wire-compat).
- Handler-level pins (`triggers_transient_test.go`, all CI-runnable): transient-once → retried to 200 on update-fetch/update-store/delete/get/listFires; persistent transient → 500 WITH the underlying error + fields captured in the log; deterministic fault (23505) → NOT retried, one store call; client-cancel → 499, no retry; delete transient-then-gone → 200 + Warn; plain miss → 404; and the full **round-trip over a trigger with fire history** (fires visible → disable 200 → delete 200 → get 404).
- Store-level pin on real PG (integration suite, ran green locally against localhost:5433): `TestTriggerDelete_WithFireHistory_CascadeRoundTrip` — the FK-cascade schema promise behind "fire history never blocks a delete".

## 5. The immortal-skipped-tick litter question — PROPOSED, not implemented (per charter)

The 500s are fixed; whether an all-`skipped`-for-N-days trigger should auto-park is a semantics change and the charter rules it an owner call. Proposal in the PR body, summary:

- `autoDisableAfter` counts consecutive FAILURES only — by design (a healthy gate filtering noise is the #1812 FEATURE, not a defect: the release watcher did its job for three hours). Making `skipped` count would break every legitimate poller.
- Options: (a) a separate opt-in `autoParkAfterSkips` (count consecutive skips, disable + tag when a threshold the OWNER sets is crossed — explicit, no surprise semantics); (b) zero-code: surface `consecutiveSkips` / `lastProceededAt` in `trigger_list` so litter is VISIBLE to the agent/owner who can then disable or delete; (c) status quo. Recommendation: (b) first (pure observability, no semantics change), (a) only if real litter accumulates.
- Interim state: the incident's litter itself is gone — the live trigger is disabled (its own intended lifecycle was self-delete after firing the relay message, which its broken filter never allowed).

## 6. Verification

- `go test ./... -count=1` — green (full repo).
- Integration suite against real PG (localhost:5433) — green, including the new cascade pin.
- `make fmt-check` — green. `golangci-lint run` on touched packages — 0 issues.
- Live-row before/after: BEFORE — enabled, firing every 3 min, both verbs 500 (04:41–04:49). AFTER (07:23) — update succeeds, `enabled:false`, tick stopped, row + fire history preserved as the repro. The live delete was deliberately NOT exercised (preserve the repro row); delete correctness is pinned by tests (handler + real-PG integration).
