# Worklog: #1767 — platform prompt documents the three-mount-point topology

**Date:** 2026-10-20
**Session:** Re-point of the prompt-side deliverable chartered during #1759 (rode the closed PR #1764; the owner invited re-pointing). Fresh lane off current main: the DefaultPlatformPrompt persistence block replacement + content pins, cherry-picked from 4633dcb6 and verified against the landed base.
**Status:** Complete

---

## Objective

Replace DefaultPlatformPrompt's Persistence block with the owner-approved text documenting the three PVC mount points (one shared volume, all three persist — including /tmp), the tmpfs contrast with the never-delete-the-symlinks contract, and the inspect-all-three cleanup directive with live-state guardrails.

---

## Work Completed

- Red-first: the content pins (pkg/types/agent_prompt_test.go) applied to main's prompt → 2 tests RED (main still carried the pre-change text, including the backwards "/tmp … do not rely on its contents surviving" line — verified count=1 on origin/main before starting).
- Applied the approved 6→6 bullet persistence block from 4633dcb6 (verified byte-clean against main's untouched agent_prompt.go) → green.
- The prompt service + handlers identity/length pins (DefaultPlatformPrompt equality, ≤ MaxPromptPerLevel) pass unchanged.

## Key Decisions

- **DB-row-override semantics (restated per charter)**: DefaultPlatformPrompt is the fresh-install fallback only — api/internal/services/prompt/service.go falls back to it when no sys_prompt_platform row exists; a saved row, including an explicitly empty one, always wins (the admin's deliberate choice). Deployments that customized the platform prompt will NOT receive the new persistence block until they clear their stored row; the new text reaches default-tier workspaces only. Correct behavior, stated so nobody mistakes the merge for a rollout.
- Straight cherry-pick of the owner-approved text — no rewording (the text was approved verbatim during #1759; #1767's Requirements reproduce it).

## Blockers

None.

## Tests Run

- `go test -count=1 -run TestDefaultPlatformPrompt ./pkg/types/` → RED pre-change (2 FAIL), GREEN post-change.
- `go test -count=1 ./api/internal/services/prompt/ ./api/internal/handlers/ -run Prompt` → ok.

## Next Steps

- PR review loop to APPROVED; packet to the orchestrator. Then salvage PRs A (harness + legs) and B (grammar note) against the #1762-landed base.

## Files Modified

- pkg/types/agent_prompt.go — persistence block (approved text)
- pkg/types/agent_prompt_test.go — content + contrast pins
- worklogs/NNNN_2026-10-20_platform-prompt-mount-topology.md — this worklog
