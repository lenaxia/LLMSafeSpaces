# 1238 — The SECOND undeletable-trigger live-fire: statement-cache rotation (SQLSTATE 26000) behind the per-id 500s (2026-10-30 22:00–22:40, tsk-crystal-flatboat-tramps)

Branch: `trigger-stmtcache-500` (worktree `/workspace/trigger-stmtcache-500`, off `origin/main` @ `a345b8101`, v0.44.11).

## 1. The incident

Trigger `5da42f34-869f-46d6-9975-2ea6570c835d` (`v04411-release-watch`, workspace `7eaa3f1b`, cron `*/5`, inline python gate script reading `/tmp/opencode/v04411-deploy-fired.marker`), **created 21:31:58Z — AFTER the first fix (PR #1995, v0.44.10 ~06:30Z)**:

- `trigger_fires` → 500 `{"error":"failed to fetch trigger"}`
- `trigger_update` → 500 `{"error":"failed to fetch trigger"}` (2 attempts)
- `trigger_delete` → 500 `{"error":"failed to delete trigger"}` (2 attempts, ~1h apart)
- `trigger_list` → the row, normal, the whole time; `trigger_create` for other triggers worked mid-window (`pr-merge-watch` created 22:34:24)
- ops fallback: a hand-created marker file inerted the gate (proceed=false) — the trigger still polled every 5 min and could not be silenced

The charter's framing ("a row created post-fix still fails — either the earlier fix covered only a subset of row shapes, or the creation path writes something the per-id read path chokes on") assumed row-shape determinism. It does not hold — and proving that is half of this worklog.

## 2. What the prior fix (PR #1995, commit f86e59563) covered — and what it did not

#1995 diagnosed the FIRST live-fire (`7df2f7d4`, v0.44.9 rollout 04:41–04:49) as a transient connection-class store fault and shipped: `SetLogger` + `respondStoreFault` on all 21 error-carrying 500 arms, `isTransientStoreErr` = **PG classes 08/53/57/58 + wrapped net.Error ONLY**, one 200 ms `retryTransientStoreCall` on fetch/update/delete, idempotent-complete delete, 499 for client cancels.

What it did NOT cover: **connection-lifecycle faults that wear deterministic-looking SQLSTATEs.** The classifier's comment explicitly prides itself on excluding "deterministic SQL" — but SQLSTATE 26000 (`invalid_sql_statement_name`) is not an application fault. It is what a client sees when a server-side prepared statement vanishes behind its statement cache — the signature of a statement-pooler recycle or a failover-side session reset during a rollout. #1995 never retried it.

## 3. Root cause, with evidence

### 3.1 Deterministic row-shape causes excluded (again, now on live probes)

- **Store symmetry**: `GetTrigger` / `GetTriggerByID` / `ListTriggers` share `triggerSelectColumns` + the same scanner; `DeleteTrigger` is a bare `DELETE ... WHERE id/owner_type/owner_id`. A poison column value CANNOT break per-id reads while list succeeds (pkg/workflows/store.go:379–521).
- **Live probes at 22:43–22:47** (through the same `/internal/v1/automation` surface via agentd): `trigger_fires` → 200; `trigger_update {description:...}` → 200 (note: a description patch skips the fetch arm entirely — `update()` only pre-fetches when the patch touches sourceConfig/enabled/target/memory-capture/script, triggers.go:981); `trigger_update {enabled:true}` (a no-op that DOES exercise the failing fetch arm) → 200. The row was readable and operable minutes after the window closed, with the identical row shape. **The failure is condition-dependent, not row-shape-dependent.**
- **The `tz` red herring**: the stuck row's `sourceConfig` is `{"expr":"*/5 * * * *"}` with NO `tz` key (the one visible difference from healthy siblings). Benign: `schedule.go` guards `cfg.TZ != ""` everywhere and defaults to UTC; the scheduler fired this row on schedule its whole life.
- **No code regression in the window**: `git diff 1c1a7b400..a345b8101` (v0.44.10→v0.44.11) touches no trigger handler/store/MCP path (only `pkg/mcp/board_tools.go`).
- **Lock contention excluded**: `ClaimDueCronTriggers` is a millisecond `FOR UPDATE SKIP LOCKED` transaction; the gate script runs after commit.

### 3.2 The mechanism, empirically pinned on real PG

The window brackets the v0.44.11 release run (41131334216: 21:28:44→21:58:33 success) plus its rollout. Fire-cadence forensics show the store/scheduler was degraded IN the window: every fire lands at `:17.127` except **22:40:27.031 — 10 s late**, with sibling `pr-merge-watch` claimed at the identical microsecond (one shared catch-up tick: the 22:40:17 scheduler tick failed and the next one recovered) — the same catch-up-lag signature as the first incident, at the exact moment of live delete attempt #2.

Repro (real PG 15.6, pgx v5.9.2 — the repo's exact version; the exact live row shape inserted): warm pgx's server-side statement cache with the `GetTrigger` statement, then rotate statements away server-side (`DEALLOCATE ALL` behind the client's back — what a pooler recycle / failover-side session reset does), then re-execute:

```
REPRO A (server-side statement rotation):
   PgError code=26000 class=26 transient-under-PR1995=false
   msg="prepared statement \"stmtcache_4de252cdc7eab90f680b2fed84d2e9b6b0eaa3a224b382ef\" does not exist"
REPRO A/B heal check (immediate second run): nil
```

The failure is **one-shot per (connection, statement)**; the immediate re-run re-prepares and succeeds. `DEALLOCATE`-driven no-op-DDL plan invalidation (ALTER TABLE) was also tested: pgx v5 handles it transparently (nil) — class 26 is the failure mode that leaks.

So: during the rollout, server-side statement rotation burned cached per-id statements one connection at a time. Each first post-rotation execution of `GetTrigger`/`DeleteTrigger` on an unburned connection returned 26000; #1995's classifier called it deterministic → no retry → the exact "one blip = hard 500" defect #1995 fixed for TCP-class faults, still live for statement-cache faults. LIST kept working because its statement had already been revalidated on the connections those calls landed on; the per-id ops on the one row being operated on kept landing on unburned ones (nobody attempted per-id ops on any OTHER row in the window — the "other rows work" evidence is from 07:23 and 22:43+, outside it). The row's creation time was a coincidence of WHEN the ops were attempted, not a property of the row.

## 4. The fix

`api/internal/handlers/triggers.go` — `isTransientStoreErr` adds class **26** (26000, the pgx statement-cache rotation signature) to the retryable set, with the doc comment carrying the live-fire citation and the one-shot rationale. Retry safety: for a genuinely invalid statement name the retry fails identically 200 ms later; the statement NEVER executed server-side (the prepare was gone before it started), so there is no uncertain-outcome hazard (strictly safer than the already-retried connection classes). One switch case + comments — no behavioral change anywhere else.

Known same-class leftover, deliberately out of scope (fix + regression pins only): `webhook_receiver.go`'s raw `GetTriggerByID` 500 arm has no retry — same treatment would apply if a webhook-delivery incident materializes.

## 5. Pins (all fail on the old classifier; mutation-verified by stashing the fix)

- `api/internal/handlers/triggers_stmtcache_test.go` (handler-level, CI-runnable): seeds the EXACT live row shape (cron `*/5`, `sourceConfig` with no `tz`, the verbatim marker-file gate script, `autoDisableAfter` 12). One-shot 26000 on the fetch → `PUT {enabled:false}` 200 with 2 store calls (old code: 500, 1 call); one-shot 26000 on delete → 200 `{"deleted":true}` (old: 500); one-shot on the fires pre-fetch → 200 (old: 500); persistent 26000 → 500 WITH the logged cause (`26000`, triggerId — the diagnosability pin); `TestTriggerV04411RoundTrip_StmtCacheBlips` — fires-audited → disabled → deleted → 404 through class-26 blips.
- `triggers_transient_test.go` `TestIsTransientStoreErr`: +`26000` and wrapped-26000 → true (the 23505/42601 not-retried contrast stays).
- `pkg/workflows/store_integration_test.go` `TestTriggerStatementCacheRotation_Class26IsOneShot` (real PG): creates the live row shape, warms the cached per-id statement on a dedicated session, rotates server-side (`DEALLOCATE ALL`), asserts the next execution is **PgError 26000 with "prepared statement"** (the incident's error class, verbatim, on real PostgreSQL), asserts the immediate re-run heals, and asserts the pool-level `GetTrigger` still works. The dedicated session is closed (never returned to the suite pool) — a pooled conn would poison the suite's client-side cache for every other statement name (learned the hard way: 7 alphabetically-later tests failed until the demo moved off-pool).

## 6. Verification

- `go build ./api/... ./pkg/...`, `go vet` on touched packages — clean.
- Targeted handler pins + `TestIsTransientStoreErr` — PASS; **mutation check**: with the classifier change stashed, `TestTriggerUpdate/Delete/ListFires_StmtCacheRotation_*`, `TestTriggerUpdate_StmtCacheRotation_Persists_500WithLoggedCause`, and `TestIsTransientStoreErr` all FAIL; restored, all PASS.
- Full suites: `go test ./api/internal/handlers/ -count=1` ok (104 s); `go test ./pkg/workflows/ -count=1` ok against real PG on :5433 (122 integration subtests); `go test ./... -count=1` — zero failures.
- `make fmt-check` green (after `make fmt` reformatted the integration test's script literal); `golangci-lint run` on touched packages — 0 issues.
- Live verification (2026-10-31 ~01:06Z): `trigger_update {enabled:false}` on `5da42f34` → 200 (the verb that 500ed twice); `trigger_delete` → `{"deleted":true}` (the verb that 500ed twice). The every-5-minute poll is silenced; the marker file at `/tmp/opencode/v04411-deploy-fired.marker` is inert litter (nothing reads it now) and can be removed at will. The live 200s are also evidence: the row itself was never the problem — the condition was.

## 7. Post-fix incident-class posture

The next rollout window will still burn cached statements — but each per-id op now retries once, and the one-shot class-26 blip is absorbed. A SUSTAINED rotation storm (more than one burned connection in a row on the same request) still 500s — deliberately: that is a real fault, and the arm now logs its class-26 cause with the trigger id, so the next incident window leaves a SQLSTATE trail instead of five bare 500s (the actual diagnosability gap #1995 closed for the message body but which this incident proved had never been exercised on a second event).
