# Worklog: 0.30.1 bundle — create_session guidance contract + mid-turn context usage (zero-stamp scan)

**Date:** 2026-09-14
**Session:** opencode (main dev box), live on the 0.30.0 pod after the rollout.
**PR:** #1379. Status: iterating on review.

## Objective

Two agentd-side fixes found while exercising the new tools live on the
rolled-out 0.30.0 fleet:

1. **`create_session` guidance** — the tool's description must encode the
   owner's decision rule: use ONLY for (a) independent fire-and-forget
   work the caller will NOT depend on, or (b) tasks requiring HUMAN
   input / intended for human consumption (the session lands in the
   workspace list for a person to steer). The result NEVER returns to
   the caller. Fully autonomous work whose outcome is needed (user
   story, investigation) → the task tool (blocking, returns result).
2. **`session_metadata` context usage mid-turn** — busy sessions
   returned no `context_tokens`/`context_limit`/`context_fill`
   (omitempty hid the zeros).

## Root cause (live-proven, Rule 7)

Mid-turn, opencode's in-flight assistant message carries a ZEROED token
stamp (real usage lands at step completion). Observed directly against
this pod's own busy session: newest assistant-with-tokens-block =
`input:0, cache_read:0, cache_write:0`; the previous completed step
carried `455 + 559168 = 559,623` (≈56% of the 1M window — the honest
live number). Both prompt-token scans (the seam's `SessionPromptTokens`
AND agentd's `fetchSessionPromptTokens`) stopped at the newest
assistant-with-a-tokens-block and returned 0.

Fix in BOTH scans (review finding 1 — the twin feeds fillGaps →
statusz `ContextUsed`, and post-#1342 interrupt-first restarts can
abandon in-flight messages with PERMANENTLY zeroed stamps, blinding the
operator view indefinitely): skip zero-total stamps; a real completion
always carries `input > 0`. Busy sessions now report the last COMPLETED
step — "as of last step" semantics, documented on both functions.

## Assumptions validated

1. A zeroed stamp is an in-flight/abandoned placeholder, never a real
   completion: every real completion records prompt `input > 0` (a
   non-empty prompt always costs tokens). The only-assistant-is-zero
   case (first turn mid-flight) honestly reports 0 — pinned.
2. The reviewer's traced consumers hold: all other token consumers are
   event-driven (SSE) and never observe the zeroed stamp; the twin is
   the only other list-scanner (verified in review).

## Tests (red/green)

- `TestSeam_SessionPromptTokens_SkipsInFlightZeroStamp` (fails pre-fix),
  `..._OnlyZeroStamps` (first-turn edge).
- `TestFetchSessionPromptTokens_SkipsZeroedInFlightStamp` (the twin,
  fails pre-fix), `..._OnlyZeroStamps_ReturnsZero`.
- Guidance pins expanded to 11 assertions + a NEW schema-description
  harness (`schemaDescs["tool/property"]`) pinning the prompt
  InputSchema's no-return line (review finding 3).
- Full suites: `pkg/agent/opencode` 12.3s ✅, `cmd/workspace-agentd`
  259s ✅.

## Known bound (noted, not fixed)

The scans page at `limit=20`: if >19 messages land after the last
completed stamp while one is in flight, the completed stamp falls
outside the page and the false-0 resurfaces until the turn completes.
Self-corrects; revisit only if it bites (review's non-blocking minor).

## Rollout plan

Bundle → 0.30.1 patch release → chart appVersion + CHANGELOG in the
same commit (the 0.30.0 lesson) → talos-ops-prod: GitRepository ref +
four semver image tags + the agentd/opencode overlay digests (0.30.1
index digests fetched from ghcr) → recycle workspaces.
