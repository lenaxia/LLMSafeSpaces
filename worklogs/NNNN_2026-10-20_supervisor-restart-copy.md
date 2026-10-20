# Worklog: #1796 — restart copy correction: "restarted by the supervisor" (mechanism rides the reason field)

**Date:** 2026-10-20
**Session:** Copy lane fixing the AgentRestarted condition/event/banner copy that attributed crash/OOM restarts to the health watchdog. Rides #1776's deferral (Amendment 2 conditional ruling: 7 independent literals → defer; now owner-funded).
**Status:** Complete

---

## Objective

User-facing copy says "restarted by the supervisor" for all restarts; the mechanism (health_watchdog/crash/oom) is carried by the reason field. No literal claims the watchdog performed a crash/oom restart.

---

## Work Completed

- Red-first: the 4 test-pin assertions updated FIRST (AgentRestartBanner.test.tsx:21; ChatPage.agentRestart.test.tsx:155,183; controller health_agent_supervisor_test.go:68) — RED against the unchanged sources (3 frontend tests + the controller pin fail), GREEN after the copy fix.
- The 8 literal sites fixed (the issue's 7 + 1 hunk-approved addition):
  1. controller/internal/workspace/health.go:337 (condition message template)
  2. controller/internal/workspace/health.go:367 (event template)
  3. pkg/mcp/server.go:89 (workspace_status tool description)
  4. frontend AgentRestartBanner.tsx (rendered banner copy)
  5. api fixture workspace_status_supervisor_test.go:47 (message mirror)
  6.-8. Test pins (banner, ChatPage ×2, controller).
- Coherence comments updated to match: AgentRestartBanner.tsx doc comment, ChatPage.tsx ×2, frontend/src/api/types.ts, sdks/typescript/src/types.ts (the 8th site — #1699-declared set, hunk-mapped through the orchestrator and APPROVED; lane merged, no open PR), pkg/apis workspace_types.go ReasonWatchdogRestarted doc comment (the constant is set for ALL restarts incl. crash/oom — comment corrected in place with a name-predates-attribution note; the constant VALUE untouched, API surface).
- sidecar_mode.go's "health watchdog" references deliberately KEPT — they name the actual subsystem, not a restart attribution.

## Key Decisions

- Discriminating pins chosen so the new assertions fail against the old copy ("restarted by the supervisor at" / "restarted by the supervisor (reason:") — copy changes proven, not assumed.
- The ReasonWatchdogRestarted constant rename was considered and rejected: CRD condition Reasons are API surface consumers match on; the comment carries the correction instead.

## Blockers

None.

## Tests Run

- Frontend: `npx vitest run AgentRestartBanner.test.tsx ChatPage.agentRestart.test.tsx` → RED pre-fix (3 failed / 8 passed) → GREEN post-fix (11/11).
- Go: `go test ./controller/internal/workspace/ ./api/internal/services/workspace/ ./pkg/mcp/ ./pkg/apis/...` → all ok (controller pin red pre-fix demonstrated).

## Next Steps

- PR review loop to APPROVED → rebase-current → final packet.

## Files Modified

- controller/internal/workspace/health.go, health_agent_supervisor_test.go
- pkg/mcp/server.go
- pkg/apis/llmsafespaces/v1/workspace_types.go
- frontend/src/components/chat/AgentRestartBanner.tsx (+test), frontend/src/pages/ChatPage.tsx, ChatPage.agentRestart.test.tsx, frontend/src/api/types.ts
- api/internal/services/workspace/workspace_status_supervisor_test.go
- sdks/typescript/src/types.ts (hunk-approved)
- worklogs/NNNN_2026-10-20_supervisor-restart-copy.md — this worklog
