# Worklog: AgentOriginBadge — wrap the origin session ID, don't truncate it — Refs #1465

**Date:** 2026-09-19
**Session:** Owner-reported frontend lane on branch `fix/agent-origin-badge-wrap` (worktree wt-1453): the live UI truncated the AgentOriginBadge origin session ID behind an ellipsis instead of wrapping.
**Status:** Complete

---

## Objective

Full origin session ID must be visible and wrap cleanly at the container edge; `title` attr carries the full ID for hover-copy; the "· self-declared origin" suffix behavior untouched; vitest pin asserting the fix (a removed-overflow regression must fail).

---

## Work Completed

- **Root cause**: the label span carried Tailwind `truncate` (nowrap + hidden + ellipsis) — a 31-char `ses_` ID inside `inline-flex max-w-full` ellipsized.
- **Fix** (`AgentOriginBadge.tsx`): the ID moved into its own `data-testid="agent-origin-session-id"` span with `break-all` (house convention for monospace IDs — TriggersPage/ApiKeysTab/SecretsTab) + `title={origin.fromSession}`; the outer span drops `truncate` for `min-w-0` natural wrapping. Label text, workspace clause, and self-declared suffix byte-identical.
- **Pin** (TDD — written first, RED on the missing element): realistic 31-char `ses_f4990c383ffe6Jr3rKx1nyKwtx`; asserts `textContent === fullId`, `title === fullId`, class contains `break-all`, class does NOT contain `truncate` (the regression surface), and the badge text contains the full ID.
- **Noted for PR, deliberately not implemented**: resolving the badge to the sender's human title (client-side from the loaded session list) for local sessions — owner didn't ask.

### Assumptions stated and validated

| # | Assumption | Validation |
|---|---|---|
| 1 | `break-all` is the house wrap convention | grep: TriggersPage ×4, SecretsTab ×2, ApiKeysTab, MessageBubble use break-all/break-words for IDs/monospace |
| 2 | textContent pins can't see CSS truncation | true — hence the class-presence/absence assertions alongside the full-string assertion |

---

## Tests Run

- `npx vitest run src/components/chat/AgentOriginBadge.test.tsx` — RED pre-fix (1 failed: missing ID element); 6/6 PASS post-fix.
- `npm test` (full frontend suite) — 172 files / 1897 tests PASS.
- `npx tsc --noEmit` — clean; eslint on the two touched files — clean (6 pre-existing warnings elsewhere, 0 errors).
- Frontend deps installed fresh (`npm ci`) — no worktree had node_modules; disk watched (91% after).

---

## Next Steps

- Review loop to APPROVED; rides next train (orchestrator merges).

---

## Files Modified

- `frontend/src/components/chat/AgentOriginBadge.tsx` — fix
- `frontend/src/components/chat/AgentOriginBadge.test.tsx` — pin
- `worklogs/NNNN_2026-09-19_agent-origin-badge-wrap.md` — this worklog

## Review Round 1 (regression-surface gap + no layout observation — both taken)

Findings: (1) my pin scanned only the inner ID span — re-adding `truncate` on the OUTER label span (the historical bug location) passed every assertion because nowrap is inherited and the outer span ellipsizes; the pin now scans the ENTIRE badge subtree (badge + all descendants) for `truncate`/`whitespace-nowrap`, and the test moved inside the describe block (finding 3). Mutation-verified: outer-span truncate reintroduction fails vitest. (2) No assertion anywhere observed LAYOUT — the reason this bug shipped. Added a Playwright row (`agent-origin.spec.ts`): at a 375px viewport the badge's boundingBox height must exceed one text line (>26px; derivation in the spec comment) and the ID span must not overflow horizontally (scrollWidth <= clientWidth). Mutation-verified: outer-span truncate reintroduction fails the row (box stays one line). Both mutations re-verified green on restore.
