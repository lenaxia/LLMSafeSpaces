# Worklog: #1685 — evidence-gate the 'busy 15 min may be hung' nudge (30-min message-level window)

**Date:** 2026-10-15/16
**Session:** w27 charter deliverable 3 of 3 (#1679 ✓ merged → #1686 (merge pending) → #1685). Owner ruling (issue comment 6202575356): evidence-gate APPROVED with amended parameters — 30-min no-progress window; progress is MESSAGE-LEVEL only; 15-min busy floor kept as semantic guard; the owner's bash case pinned.
**Status:** Complete

---

## Objective

Stop the hung-session nudge from firing on long-but-healthy turns (this fleet's turns regularly run 30-60+ min): alert only on message-level evidence — no new messages from either side (LLM-completed or user-sent) for 30 min, in addition to the 15-min busy floor. Align the banner copy to state the evidence, sync the openapi description, leave MCP/SDK surfaces untouched (audited).

---

## Root cause (verified in tree)

`escalateHungs` (api/internal/handlers/proxy_lifecycle.go) escalated on `oldest_busy_seconds ≥ 15min` alone — the only signal statusz carried (`OldestBusySeconds`, D6/#998). agentd's tracker had `lastEventAt` (ANY event — chunks/tools stamp it), which cannot discriminate a healthy long tool run from a wedge. No message-level signal existed anywhere.

## Work Completed

### (a) RED-first pins — `proxy_1685_test.go` (first commit)

5-arm `TestEscalateHungs_EvidenceGate`: both-windows alert (busy 900s + silence 1800s, payload carries `last_progress_seconds`); **the owner's bash case** (busy 1200s + silence 1200s → NO alert); busy-floor guard (busy 600s + silence 3600s → NO alert); 1799/1800 boundary; resumed-messages resolve a live alert. Includes the additive `LastProgressSeconds` field on `agentd.StatuszResponse` (type-only) so the pins compile. Verified TRUE RED against main @ a22dd37e: 4/5 arms fail against the single-window gate (compiles clean; the floor arm passes vacuously).

### (b) Wire-seam classifier — `pkg/agent/opencode/wire/wire.go`

`ParseMessageLevelProgress(eventType, props) (sessionID, ok)`: matches `message.updated` (suffix-tolerant, like every matcher there) and counts it as progress ONLY when `info.role == "user"` (user-sent) or `info.cost` is present (assistant completion — the same billing-completion discriminator the ABI translator uses for MESSAGE_END). In-flight assistant upserts (no cost), part deltas/updates, tool events, heartbeats → not progress.

### (c) Tracker — `session_tracker.go`

`lastMessageAt map[string]time.Time` + `noteMessage` (stamped ONLY via the classifier) + `lastMessageAges()` (RLock'd copy, the busyDurations pattern) + prune coverage. Stamped in BOTH the flat and nested (legacy) processEvent arms. `lastEventAt` (#1342 any-event progress) is untouched — the two clocks answer different questions ("has either side said anything new?" vs "is the harness alive?").

### (d) statusz — `server.go`

`LastProgressSeconds` = max seconds-since-last-message across the AUTHORITY-FILTERED busy set (the same filtered iteration as BusyAges — status and evidence from one view). 0 when no busy session or no stamp exists.

### (e) API gate — `proxy_lifecycle.go`

New tunable `busyAlertNoProgress = 30min` (vars for tests). `hungEvidence(sz)`: busy floor AND silence window both met; `LastProgressSeconds <= 0` = evidence-unavailable → NO alert (fails toward silence — the ruling's direction is fewer false alerts). Resolution fires when EITHER window clears. Payload + Warn log carry `last_progress_seconds`; guidance copy updated to the evidence phrasing.

### (f) Companion migration — existing D6 fixtures

The D6 fixtures' hung shapes (busy 20m) are exactly the owner's bash case under the new gate — they stopped alerting (verified: 8 tests RED), proving the gate bites. Migrated deliberately: `hungStatusz` now carries an independent satisfying silence value (`busyAlertNoProgress + 5min`); the resolution test's closure sets both clocks independently (hung = 20m busy + 35m silent; recovered = 10s/10s).

### (g) Frontend — `types.ts`, `ChatPage.tsx`, `ChatPage.queue.test.tsx`

`WorkspaceAlertEvent.data.last_progress_seconds?` (optional — older APIs). Banner: "No new messages for X min while this session kept running — it may be hung…" when the field is present; busy-minutes copy remains as the documented fallback when absent. New pin: evidence copy renders + busy-minutes copy absent + Dismiss works; the two pre-existing banner tests pin the fallback path unchanged.

### (h) openapi + CHANGELOG

Alerts-endpoint description updated to the two-window message-level contract. CHANGELOG [Unreleased] "Changed — hung-session alerting". No SDK/MCP surface changes (audited: only the openapi description mentions alerting; no SDK models the alert payload; no MCP tool).

---

## Key Decisions

- **Cost-bearing message.updated = LLM completion** — reuses the ABI translator's own MESSAGE_END discriminator instead of inventing a second definition of "completed"; one wire truth.
- **Fail toward silence** — evidence-unavailable (0) never alerts: a false negative (missed wedge) is recoverable by the human watching the session; the false positive was the dogfooded noise the owner filed.
- **Resolution on EITHER window clearing** — a message arriving mid-hang (even if the turn keeps running) is proof of life; the badge must drop.
- **Persisted alerts unchanged** — RecordAlert's shape (workspace, session, type, oldestBusy) is untouched; the evidence is advisory context in the SSE payload, not new DB schema (no migration for a copy fix).
- **Two independent clocks kept** — lastEventAt (#1342, harness-alive) and lastMessageAt (#1685, conversation-alive) serve different consumers (restart-worthiness vs hung-alerting); merging them would regress one or the other.

---

## Blockers

None. #1690 (#1686) merge still pending with the orchestrator — its CHANGELOG section will merge trivially beside this one.

---

## Tests Run (measured on this branch)

- RED: `go test ./api/internal/handlers/ -run TestEscalateHungs_EvidenceGate -v` → 4/5 arms FAIL against main (bash case alerted; 1799 alerted; payload missing field; no resolution) — compiles clean.
- GREEN: same suite 5/5 PASS. Full D6 family: `TestEscalateHungs|TestD6|TestGetWorkspaceAlerts` → 21 top-level PASS / 0 FAIL.
- `go test -count=1 ./api/internal/handlers/ ./cmd/workspace-agentd/ ./pkg/agent/opencode/wire/ ./pkg/agentd/ ./api/internal/services/usagestream/` → all ok (104s / 335s / …).
- agentd pins: `TestTrackerMessageProgress_*` (6) + `TestStatusz_LastProgressSeconds_*` (2) — 8/8 PASS.
- Frontend: `npx tsc --noEmit` clean; `npm run lint` 0 errors; ChatPage.queue.test.tsx 17/17; FULL vitest **191 files / 2111 tests all pass**.
- Incident disclosed: my first version of the evidence-copy test used a comma-expression assertion (`expect(...), "string"`) — a lint error that also failed 15 vitest FILES (0 failing tests — the transform choked on the shared file). Fixed to two statements; suite fully green after. The intermediate "15 failed files" run is recorded here, not hidden.

---

## Incident record — PR-open events lost (GitHub event-delivery)

PR #1704 opened 00:26Z; 45+ minutes later ZERO workflow runs (checks 0/0, no review job) while repo Actions were demonstrably alive (another branch's PR runs fired at 00:47Z). Matches the documented GitHub event-delivery-loss shape (the #1569/#1530-era outage; remedied then by re-firing PR events). This commit is the re-fire: the worklog record you are reading is real content whose push carries the synchronize event. If the pipeline still does not fire on synchronize, the remaining lever (PR close/reopen — prior fleet use was owner-approved) is queued behind the orchestrator's word. Also disclosed: one accidental "placeholder" message was sent to the orchestrator (wrong buffer, no content; corrected immediately).

## Next Steps

- PR → review loop → merge-readiness report (no self-merge).
- Charter close after all three PRs merged + release production-verified: #1679 ✓ merged (ff68dbb5), #1686 awaiting orchestrator merge (APPROVED), #1685 this PR.
