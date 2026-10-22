# Worklog: #1837 stillborn spawns — RCA (live cluster) + platform-side durability design

## Objective

Investigate the 8 stillborn board sessions from the lane-#1832 dogfood batch
(overnight + throttle window) with live kubectl/DB access, find the real
cause, and design durability **without changing opencode** (owner ruling:
"we will not fix opencode. make us durable to this").

No code changes in this pass — evidence + design only.

## Evidence (live cluster, workspace b150f355, pod …-d4cfa965)

Stillborn cohort (opencode.db `message` count = 0 AND `session_index.message_count` = 0):

| phrase | session_id | created (UTC) | msgs |
|---|---|---|---|
| lean-ritual-hauls | ses_e9e6a8e4affe4RFY5VsJfwHjXK | 2026-10-22 03:09:06 | 0 |
| olive-shanty-refrains | ses_e9e4b2a0fffeVP9i7CybSGWFep | 03:43:23 | 0 |
| blunt-ironwood-picnics | ses_e9e48dd37ffemAvlBgp7u2yTQh | 03:45:54 | 0 |
| jasper-pamphlet-folds | ses_e9e2b625bffeUN291YrnzdFXF9 | 04:18:06 | 0 |
| ebony-carousel-puzzles | ses_e9dc96c37ffe93e73dxPc356XD | 06:05:06 | 0 |
| wooden-persimmon-picnics | ses_e9babb4a6ffedJ9TQh0aRO1MhS | 15:56:48 | 0 |
| usable-tabby-waits | ses_e9babb48dffe2i6W2xut6N7XMj | 15:56:48 | 0 |
| petite-shingle-tramps | ses_f33645b43ffesnej2Vf1ce2ueh | 2026-09-23 04:52 | 0 |

### Root cause — two stacked defects, BOTH on our side

**Defect A (opencode 1.18.15, pinned, WILL NOT FIX):** the first prompt to a
freshly created session intermittently (~most first attempts in the observed
window) dies inside opencode with

```
ERROR message=failed error="Error: No user message found in stream. This should never happen."
  at SessionPrompt.run … at SessionHttpApi.prompt
```

- It fails **within 10–100 ms of `created`, with NO `stream`/provider line
  before it** — no provider request is ever made. The draft comment's
  provider-throttle correlation is a red herring (relay-router logs show no
  429s; these sessions call `thekaocloud` directly, not the relay).
- Repro signature in `/workspace/.local/opencode/log/opencode.log`: every
  stillborn has exactly `created` → `loop step=0` → the error, then nothing.
- The failing POST is the one the agent-node seam makes with a client
  `msg_`-prefixed `messageID` (#1327 dedupe key,
  `cmd/workspace-agentd/workflow_execute.go` `workflowAgentMessageKey`). The
  prompt stream races the keyed user-message store write.
- Interactive/peer messages (no `messageID` — `llmsafespaces_send_message`,
  MCP) never hit it: **every "revived" worker was saved by an orchestrator
  relay** (`lsp:agent-message-v1` part in opencode.db) landing 3–12 min after
  spawn (tidal 125 msgs, sunny-circuit 82, playful-river 48, tidy-mountain 9 —
  all four show the SAME first-prompt failure at creation). A stillborn =
  a spawn where no relay arrived inside the lease window.

**Defect B (ours — the reason Defect A costs 15 min instead of seconds):**

1. `api/internal/app/board_spawner.go:69-76` — the ONLY failure predicate is
   `resp.SessionID == ""`. `resp.ErrorCode` is ignored. But agentd's
   `failAgentNode` (`workflow_execute.go` `execAgentNode`) deliberately
   returns HTTP 200 + `errorCode` + the SURVIVING session id (#1470 existence
   contract: board workers are `session:"new"` = preserved, so the failed
   session is never torn down). Result: a failed charter dispatch returns
   `SessionID != ""` → the spawner reports **success**.
2. `pkg/board/engine.go:459-475` `spawnSync` — on "success" it stamps
   `assignee_session` and re-arms the lease. A 0-message corpse now holds a
   full 15-min claim (`LeaseDuration`, `pkg/board/board.go:49`).
3. The spawner calls `executor.Execute` **directly** — it does not ride
   `executeWithRetry` (`api/internal/workflows/engine.go:153`), which is
   exactly how the rest of the platform already survives Defect A:
   `script_failed` wrapping "opencode returned 5xx" is retry-class
   (`retryableAgentdFailure`, engine.go:204), 3 attempts, fresh session each.

**Proof the retry path works:** the "1832 lease keeper" cron (`*/9`, trigger
`4c0629c5`) rides the routine path (`routine-agent` node →
`executeWithRetry`). The opencode log shows the Defect-A error in **triples
on the exact */9 grid** (03:09:23/25/29, 03:18:23, 03:27:23/25/29 …) — the 3
retry attempts — while `trigger_fires` shows **29 delivered / 2 failed** and
`lease-keeper.log` has real per-fire lines at ~:36–:50s of each slot. The
routine path burns 3 spawns to land one and occasionally loses the fire; the
board path burns one spawn, stamps the corpse, and waits out the lease.

Also confirmed along the way: the keeper's renew-at-04:21:02 on jasper
(the corpse renewal #1847 describes) is real (`lease-keeper.log`:
`04:21:02Z renew:tsk-3do6n458,…` while jasper had 0 messages) — its
liveness check still renewed a 0-message assignee. Cross-ref #1847; the
keeper is now disabled (decommission ruling).

## Durability design (platform-side only; opencode stays unfixed)

Layered — D1 removes most stillbirths, D2/D3 make the remainder cheap, D4/D5
harden semantics and the wider blast radius.

### D1 — Spawner rides the retry class + honest success predicate (root fix)

In `api/internal/app/board_spawner.go`:
- Success = `resp.SessionID != "" && resp.ErrorCode == ""`. An errored
  dispatch with a surviving session is a failure (and the survivor is a
  corpse — abort it via the existing aborter seam, same as spawnSync's
  lost-race leg, engine.go:480-486).
- Bound the dispatch like the routine path: reuse `executeWithRetry` (or an
  equivalent ≤3-attempt loop with fresh `session:"new"` per attempt — a
  failed attempt's session has 0 messages, so there is nothing to
  double-execute; same safety argument as #1457's failed-create retry).
- Keep dispatch idempotence across re-drives: attempts create FRESH
  sessions, so the #1327 messageID stays harmless; the dedupe key is not
  load-bearing for board charters (one charter per claim, R9 fresh-per-task).

Expected effect: per-attempt failure observed p≈⅔ → 3 attempts → ~1-2% of
claims still fail dispatch; those now return ERROR (no stamp) and heal by
ordinary lease-lapse semantics with NO assignee ever recorded.

### D2 — First-token receipt (spawn-ack, #1847 addendum)

"Dispatch returned 200" ≠ "generation started" (200-empty is a possible
shape; acceptance ≠ first token). The platform already owns the receipt
signal: `session_index.message_count` is maintained by the API's own message
proxy (`api/internal/services/database/database.go:1218` upsert,
`:1251` absolute) and short-id registration (`short_session_ids.go:49`);
agentd's sessionstate exposes messages/busy (`llmsafespaces_session_metadata`).
Spawn-ack = after stamping, require the assignee to reach message_count ≥ 1
within spawn-grace (#1847 starting defaults G=15/A=10/spawn-grace=5).

### D3 — Fast eviction of zero-message claims (started-vs-spawned guard)

RC1 (`pkg/board/reconciler.go` `rc1Reclaims`) reclaims only on
`lease_expires_at < now()`. Add a stillborn predicate:
`stage='in_progress' AND assignee_session IS NOT NULL AND spawn age >
spawn-grace AND session_index.message_count = 0` (join on
(workspace_id, session_id) — platform-owned tables only).
- Evict → re-arm FIFO with a fresh claim_token immediately (worst-case burn
  drops 15 min → ~5 min even if D1 regresses).
- Emit a distinct board_event action (`stillborn_evict`) for the PR-D
  timeline and for a stillborn-rate metric.

### D4 — Attempts semantics (already ruled, enforce here)

Claim events NEVER touch attempts (#1837 sharp-edges §23 taxonomy; also the
A1 proof-by-replay: 3 attempts, 0 failures, all claim events). The
stillborn-evict path must not increment attempts; `claim_incremented_attempts`
already isolates the grant increment — keep the evict path off it.

### D5 — Same-seam hardening for routines/triggers

Every LLM routine (trigger prompts, workflow agent nodes) rides the same
agent-node seam and currently pays ~3 spawns per fire for Defect A (keeper:
29/31 delivered, 2 burned fires against the auto-disable budget). Options,
in preference order:
1. agentd `execAgentNode`: on a message-leg failure where the session is
   FRESH and has 0 stored messages, re-POST the message once in the SAME
   session before reporting failure (no double-execution: a 0-message failed
   turn never started). This collapses the retry cost from 3 sessions to 1.
2. Keep `executeWithRetry` as-is (it works) and accept the spawn overhead
   until (1) lands.
Also: surface `stillborn_evict` + retry-attempt counts as metrics so a
regression in Defect A's failure rate is visible without log spelunking.

## Key Decisions

- **opencode stays pinned and unfixed** (owner ruling). All durability is in
  board_spawner / engine / reconciler / agentd.
- The #1327 messageID is retained (not the removed lever): board charters are
  fresh-per-claim, and the routine path needs it; D5(1) addresses the cost.
- Fast eviction uses platform-owned observability (session_index) — no new
  instrumentation, no opencode reads on the hot path.
- Stillborn is a claim event, not an attempt event (D4), consistent with the
  #1847 attempts-as-context-loss-ledger ruling.

## Blockers / Open Questions

- Exact per-attempt failure rate beyond the observed window (Sep instance
  suggests this predates the lane; log rotation hides older evidence) — the
  D3 metric answers this prospectively.
- Whether opencode ever returns 200-empty (vs 5xx) on Defect A: observed
  envelopes were 5xx-class (`script_failed` wrapping "opencode returned 5xx"
  is what the retry classifier matched); D2 covers the 200-empty shape
  regardless.
- Keeper corpse-renewal at 04:21:02 despite its `messages == 0` guard —
  needs its own look (session_metadata accuracy); folded into #1847.

## Tests Run (evidence reproduction)

- `session_index` rows for the 8 phrases (psql, llmsafespacespg cluster).
- opencode.db `sqlite3` message counts (0 for all 8; 125/82/48/9 for the
  relay-revived workers).
- `grep -A8 "created id=<ses>" opencode.log` for each: created → loop step=0
  → "No user message found in stream" — no provider `stream` line.
- Error-timestamp histogram: */9-grid triples = keeper retry attempts;
  trigger_fires 29 delivered / 2 failed; lease-keeper.log per-fire lines.
- relay-router logs: no 429/throttle events in window.
- Code reads: `board_spawner.go:64-92`, `engine.go:125-215,1415-1470`,
  `workflow_execute.go` (`execAgentNode`/`failAgentNode`/`createOpencodeSession`),
  `reconciler.go` Sweep/RC1, `database.go:1218-1290`.

## Next Steps

1. Implement D1 (predicate + bounded attempts) with kill-test pins in
   `pkg/board/engine_kill_test.go` style (spawn returns 200+errorCode+session
   → NO stamp, claim re-arms; 2nd attempt success → stamped).
2. Implement D3+D4 (RC1 stillborn predicate, `stillborn_evict` event, no
   attempts increment) + sweep kill-test.
3. D2 receipt poll behind the spawn-grace config key (#1847 PR-A config map).
4. D5(1) agentd same-session re-POST for fresh 0-message failures.
5. Post the consolidated #1837 comment with corrected causality (not
   provider throttling; spawner ignores ErrorCode + no retry) — draft lives
   in this worklog's Evidence section.

## Files Modified

None (investigation + design only; this worklog).
