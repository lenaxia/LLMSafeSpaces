# Worklog: prompt triplication — cross-attempt admission dedup (#1288 fix 1)

**Date:** 2026-09-05
**Session:** Live production incident on the V2 flip: one user send executed five turns. Root-caused via the API logs (one admission), the Valkey outbox (one entry), and opencode's sqlite on the PVC (five persisted user messages at +11s/+20s/+40s/+80s — the API outbox's doubling retry ladder). Fix 1 of 3 from #1288.
**Status:** In Progress

---

## Objective

Stop the outbox retry ladder from manufacturing duplicate agent turns.

## Work Completed

### Root cause (evidence, not theory)

- ONE `POST /prompt`, ONE outbox entry, ONE dedupe key — the API admitted once.
- opencode's `session_message` table: five identical user rows, timestamps matching the API's retry backoff constants (10s base, doubling) exactly.
- The terminus protocol: on a retryable poll outcome the deliverer re-POSTs at attempt+1; agentd's `attemptAdmission` keyed its dedup by (entryID, attempt) — every new attempt was a fresh admission, and opencode's prompt API carries no idempotency key. The original test even pinned the bug: "attempt+1 is a NEW admission — allowed".
- Trigger: the workspace's opencode restarted (env-secret change) at the send moment, so attempt 1's admission was slow to become visible; the ladder fired.

### Fix 1: cross-attempt admission dedup

`attemptAdmission` now consults `admittedAnywhere(entryID)` — any prior attempt at ADMITTED or later makes the new attempt admit idempotently with that attempt's messageID; the opencode POST never happens. The outbox entry ID is stable across the ladder, making it the correct dedup key.

### Fix 1b: admission uses the TUI's delivery semantics (steer, not queue)

`opencodeAdmitter` shipped with `delivery:"queue"` (0052 semantics: drains on idle/wake) — but the pinned opencode 1.18.10 **never drains that queue** (#755, "messages vanished"; the API's adapter path abandoned queue for exactly this reason long ago). The incident ran on queue-mode admission racing opencode restarts. Steer is what the TUI sends: admit-and-run-now, a synchronous messageID, turn events on the session stream — the taxonomy the promotion correlation was built for (contract goldens pin steer's prompt.admitted/prompted events). Trade-off, disclosed: with dedup holding and steer's run-now semantics, a message opencode LOSES to a restart between admit and turn completion is never re-POSTed — its ONLY recovery surface is stall escalation (admitted → stalled, fsync'd, wake fired once, stalled-entries gauge). That surface is now pinned (`TestSteerDedup_RestartDestroyedAdmitted_EscalatesViaStall`).

## Key Decisions

1. **Dedup key = outbox entry ID, not text.** A text fingerprint would collapse genuine duplicate user sends ("ok" twice). The entry ID is stable across the machine-driven ladder and unique per user intent.
2. **Dedup at the agentd boundary** (not API-side): the API's prior-attempt lookup already completes on ADMITTED; the gap was agentd's per-attempt scoping after a re-POST. Fixing at the boundary also covers replay/resume paths.
3. **The contradicting pin was updated, not deleted**: `TestDeliver_ExactlyOncePerAttempt`'s tail now asserts the dedup (1 POST, prior messageID carried) — the old assertion WAS the incident.

## Blockers

None.

## Tests Run

- `go test ./cmd/workspace-agentd/...` — green (218s + 23s).
- New: `TestAttemptAdmission_CrossAttemptDedup` (ladder attempt+1 after admitted → 1 POST, messageID carried).

## Next Steps

- #1288 fix 2: transcript durability verification — opencode.db IS PVC-backed and the 12 messages persist; the remaining gap is the HTTP list/serving path during restarts, plus the event-stream attach latency that hid live updates.
- #1288 fix 3: env-secret restart deferral under an active/queued turn.
- Ship as 0.27.1 + production roll.

## Files Modified

- cmd/workspace-agentd/sessionstate/ledger.go — admittedAnywhere + cross-attempt dedup in attemptAdmission.
- cmd/workspace-agentd/sessionstate_wiring.go — admission delivery mode queue → steer (the TUI semantics).
- cmd/workspace-agentd/sessionstate_wiring_test.go — TestOpencodeAdmitter_UsesSteerDelivery (red-checked).
- cmd/workspace-agentd/sessionstate/ledger_test.go — cross-attempt pin; corrected exactly-once tail; seeded-state guard; stall-escalation companion pin.
