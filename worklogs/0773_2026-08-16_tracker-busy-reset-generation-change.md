# Worklog: orphaned busy-flag reset at opencode generation change (#892 D2)

**Date:** 2026-08-16
**Session:** Implement design 0050 D2 — phantom-busy sessions from the 2026-08-15/16 halt incident (workspaces 946a442f / 843a55c2). PR #893, tracking issue #892.
**Status:** Complete

---

## Objective

When opencode dies mid-turn (watchdog kill, crash, OOM, operator restart), the SSE tracker's busy flag is orphaned forever: no idle event will ever arrive, and the #795/#803 prune only removes entries for sessions that no longer exist in `/session` — which returns DB records, not busyness (documented at `session_aware_restart_test.go:227`). Users saw "busy, no progress" for 20–30+ min (8 tools stuck `status:"running"` across two workspaces, each starting seconds before a generation change), forcing a Stop-before-send ritual that killed live turns.

---

## Work Completed

### agentd (cmd/workspace-agentd)

- `session_tracker.go`: `resetBusyFlags()` — busy→idle only; entries and `promptTokens` (context meters, display state) survive; returns cleared IDs. `onOpencodeGenerationStart()` — the supervisor hook body; counts via `RecordTrackerBusyReset`, logs cleared session IDs.
- `managed_process.go`: `onChildStarted func()` callback field, invoked by the supervisor after each successful child `Start()` and before `close(upCh)` — so the reset lands before the new generation can serve any request. Not fired when `Start()` fails (no generation began). `pid()` accessor for the vitals gatherer (D1).
- `main.go`: tracker constructed before `startManagedProcess`; hook wired through the new `startManagedProcess(supervise, sseTracker)` signature. statusz (`server.go`) and restart gating (`secrets.go` `hasAnyBusy`) both read the healed state.
- `ops_metrics.go`: `workspace_tracker_busy_resets_total{workspace_id}` — sessions healed per reset.

### API server (api/internal)

- `proxy_events.go`: `reconcileSessionState` statusz decode cap 16 KB → 1 MB, mirroring #801 (`proxy_connections.go`). With >~55 sessions the old LimitReader truncated the body, the JSON decode failed, and the reconcile silently no-op'd — stale `activeSess` entries persisted client-side as phantom-busy, exactly where D2 was supposed to end them.
- `proxy_v2_test.go`: `TestReconcileSessionState_LargeStatuszDecodes` — verified red against the 16 KB cap, green at 1 MB.
- `pod_bootstrap_e2e_test.go`: fixed the pre-existing `buildAgentd` data race (goroutine `cmd.Run()` vs parent `cmd.Process.Kill()` on timeout — flagged by CI's `-race`). Now `exec.CommandContext` with all Process access inside the stdlib's synchronized watcher.

### Tests (agentd)

- `tracker_generation_reset_test.go`: reset semantics (busy cleared, idle untouched, tokens/entries survive, cleared IDs returned), empty/all-idle no-ops, orphan heal through the hook, 500-write concurrency smoke, metric assertion (Add semantics + empty-ID normalization).
- `managed_process_generation_test.go`: hook fires per generation — first boot, operator `restart()`, and crash recovery (SIGKILL the child; the incident's actual trigger path) — orphaned busy heals at each boundary; nil hook safe.

---

## Key Decisions

- **Busy flags only, not the whole tracker.** `promptTokens`/contextUsed is display state and survives legitimately; `prune()` continues to own deletion.
- **Hook placement before `close(upCh)`.** `restart()` blocks on `upCh`, so restart-returning implies reset-ran; the reset lands before the new process serves traffic.
- **Accepted residual micro-race:** an SSE event buffered from the dying generation can be processed after the reset and re-mark a session busy. Mutex-protected (no corruption), strictly no worse than pre-fix, self-heals on the next generation change.
- **Decode cap at 1 MB, not unbounded:** mirrors the reviewed #801 precedent; statusz with thousands of sessions is a statusz problem, not a reconcile problem.

---

## Assumptions (stated + validated)

1. Busy state produced by a dead generation is orphaned by definition. **Validated:** all 8 incident orphans started seconds before a generation change; `/session` returns records (test comment + #803 history).
2. Fresh SSE events rebuild truth in seconds. **Validated by construction:** the tracker subscribes independently; boot emits status events.
3. `restart()` implies hook ran. **Validated:** `restart()` blocks on `upCh` (managed_process.go), which closes only after the hook.
4. CI race was pre-existing. **Validated:** race is in `buildAgentd` (api test helper), untouched by this branch's diff; failure signature references only pod_bootstrap_e2e_test.go.

---

## Blockers

None. G7 stress harness (design 0050) gates the merge order but not this PR's correctness.

---

## Tests Run

- `go test ./cmd/workspace-agentd/ -count=1` — full suite, green (337s)
- `go test ./cmd/workspace-agentd/ -run 'TestManagedProcess_OnChildStarted|TestResetBusyFlags|TestOnOpencodeGenerationStart|TestRecordTrackerBusyReset' -count=1 -race` — green
- `go test ./api/internal/handlers/ -run 'TestReconcileSessionState_LargeStatusz|TestV2StrandedRecovery' -count=1` — green; LargeStatusz verified red pre-fix
- `go build ./... && go vet ./... && gofmt -l` — clean

---

## Next Steps

- D1 watchdog demotion (PR #894, stacked on this)
- D4 probe truthfulness (readyz TCP semantics + probe budgets)
- D5 elapsed-time badges, D3 durable prompts after G4 audit, G7 stress harness as merge gate

---

## Files Modified

- cmd/workspace-agentd/session_tracker.go
- cmd/workspace-agentd/managed_process.go
- cmd/workspace-agentd/main.go
- cmd/workspace-agentd/ops_metrics.go
- cmd/workspace-agentd/tracker_generation_reset_test.go (new)
- cmd/workspace-agentd/managed_process_generation_test.go (new)
- api/internal/handlers/proxy_events.go
- api/internal/handlers/proxy_v2_test.go
- api/internal/handlers/pod_bootstrap_e2e_test.go
- design/0050_2026-08-16_starvation-proof-session-truthfulness.md (new)
