# Worklog: elapsed-time badges on running tools (#892 D5)

**Date:** 2026-08-16
**Session:** Implement design 0050 D5 (progress visibility) on `fix/892-d5-elapsed-badges`, stacked on #895. Tracking #892.
**Status:** Complete

---

## Objective

Make live-silent distinguishable from dead at a glance. During the incident, a `sleep 720` polling turn and an orphaned (dead-state) tool rendered identically — "busy, no progress" — which is what drove the Stop ritual. A running tool with a visible, honestly-growing elapsed time lets the user tell "working, 40s" from "state is stale, 3h" without any protocol change.

---

## Work Completed

- `api/types.ts`: `MessagePart.toolStartedAt?` (optional — absent on older payloads).
- `api/messages.ts`: `ContractMessage.parts[].tool.state.startedAt?` typed; `transformHistory` threads it into `toolStartedAt`. The API already ships it (`pkg/session/part.go:51`, json `startedAt` — verified in the 2026-08-15 incident responses).
- `components/chat/MessagePart.tsx`:
  - `formatElapsed` — coarse durations (`38s`, `1m 12s`, `3h 4m`); the badge distinguishes live from dead, not stopwatch precision.
  - `ToolElapsedBadge` — renders in both tool paths (plain and `ToolDetails` summary, via a new optional `badge` prop), right-aligned, `aria-label="elapsed time"`, tabular numerals.
  - Ticking: `useNow` at 1s only while a running tool with a start time is on screen; otherwise the interval is 1h (effectively off). Idle screens pay nothing.

## Tests

`MessagePart.test.tsx`: badge renders at ~42s (tolerant regex), formats hours coarsely, absent on completed tools, absent on running-without-startedAt (older-API compatibility). Existing 64 tests unaffected; `tsc --noEmit` clean; `messages.test.ts` green.

---

## Key Decisions

- **Coarse format, no milliseconds:** the design goal is live-vs-dead discrimination.
- **Badge absent rather than "unknown" when `startedAt` is missing:** old payloads degrade to today's UI.
- **1s tick scoped to visible running tools:** no global timer churn.

## Assumptions (validated)

1. API ships `startedAt` on tool state — `pkg/session/part.go:51` + incident response bodies.
2. `useNow(interval)` exists with per-component intervals — `frontend/src/hooks/useNow.ts:14`; Sidebar already uses the pattern.

## Blockers

None.

## Files Modified

- frontend/src/api/types.ts
- frontend/src/api/messages.ts
- frontend/src/components/chat/MessagePart.tsx
- frontend/src/components/chat/MessagePart.test.tsx
