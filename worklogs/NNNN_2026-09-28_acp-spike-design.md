# Worklog: ACP spike + design-watch (issue #1595, design 0063)

**Date:** 2026-09-28
**Session:** ses_f4990c442ffei8kVvWzcN9qMHU
**Status:** Design doc drafted on branch `design/acp-spike`; PR up; awaiting review rounds.

## Objective

Per issue #1595 (owner-chartered): run the pinned opencode 1.18.15's `opencode acp` server, enumerate its session_update shapes and the permission round-trip, map them onto the session contract's five part types, and settle the staged path / upgrade-resilience accounting / spec-maturity risk in a design/NNNN document. Spike first, doc second — evidence before prose.

## Work Completed

- Verified the pinned binary: `/opencode/usr/local/bin/opencode` = 1.18.15; `opencode acp` subcommand present (JSON-RPC over stdio; `--pure`, `--cwd` flags used).
- Built four node stdio drivers (`acp_drive.mjs`, `acp_silentignore.mjs`, `acp_lifecycle.mjs`, `fork.mjs`) in /tmp, committed under `design/evidence/acp-spike/` with seven verbatim transcripts (ping2, tools2, perm3, editperm, silentignore, lifecycle, fork) — every message both directions, full payloads, timestamped — plus the playground ask-configs and a binary-zod-extract.md making [zod] citations auditable.
- Enumerated the surface live: initialize/session new/prompt/close/list/set_config_option/set_model/fork/load; **seven of eleven** `sessionUpdate` variants observed on the wire (five during prompts; `user_message_chunk` replay-only via fork/load); tool kinds edit/execute/other with full payload anatomy.
- Captured the permission round-trip end-to-end for both kinds (edit/diff in editperm; execute in perm3): `session/request_permission` (options allow_once/allow_always/reject_once) and the working response shape `{outcome:{outcome:"selected",optionId}}` — singular optionId, contra the draft spec.
- Captured the failure mode committed: wrong-shape consent response → silent 24s+ wedge → notification-only cancel recovers → correct shape proceeds (transcript-silentignore). Also: session/list ignores `limit` and spans the store cross-cwd.
- Cross-checked against the zod schemas bundled in the binary (strings extraction, committed as binary-zod-extract.md) — the full method tables, the 11-variant update union, tool kinds, stopReasons, dispatch tables (cancel is notification-only).
- Wrote `design/0063_2026-09-28_acp-spike.md`: mapping table (zero part-type changes; plan/todowrite → Tool per 0049 discipline), staged path (A: ACP-shaped adapter vocabulary now; B: transport migration assessed-only; C: triggers named), retire/remain accounting vs the #730 golden fixtures + dialect.go + translate.go, spec-maturity risk register.

## Key Decisions

- Stage A only requests a charter; transport migration (Stage B) stays assessed-only behind named triggers (second agent runtime funded; editor-direct access chartered).
- The §5 divergence set (permission shape singular optionId + silent-ignore/wedge; fork-requires-cwd; session/list cross-cwd AND limit-ignored; cancel notification-only; emission gaps) is proposed as the ACP golden-fixture v0.
- The silent-ignore finding (malformed consent response → turn wedged, no error) is recorded as safety-relevant fail-closed behavior requiring a pipe watchdog + cancel-recovery drill in any Stage B.

## Blockers

None. (Driver iteration notes: first permission-response attempts used the draft-spec `optionIds` array and were silently ignored — resolved via the binary's zod; JSON-RPC duplicate-id responses are ignored by the server, so shape variants must be tested one per fresh request, not ladder-retried.)

## Tests Run

Spike-only lane: twelve protocol runs against the pinned binary (seven transcripts committed in evidence/, selection criterion stated in design 0063 §2). No Go code touched; `go build ./...` not applicable to this doc+evidence diff (verified: no .go files changed).

## Review rounds

### r1 review round (CHANGES_REQUESTED, 17:35:26Z — commit 46ec1aa)

The reviewer verified every claim against the committed artifacts and the artifacts did not carry the doc: (1) the §4.3 "verbatim" edit/diff permission centerpiece existed in no committed transcript — the run that captured it (tools v1, ask-config) went uncommitted while the committed tools2 was the default-allow run; (2) `[T:tools 18.47]` dangling (that timestamp was the uncommitted run's); (3) "nine observed live" was false — the committed transcripts show **7**, and `user_message_chunk` IS emitted (fork/load replay, refuting my "[zod] only — zero emissions"); (4) the silent-ignore headline cited a transcript with zero permission requests; (5) session/list's ignored `limit` (24 sessions for {limit:10}) — a divergence I had in my own capture and did not read; (6) minor: divergences count 3-vs-4, [zod] content inside an "(all live)" table, initialize ts 1.34→1.29, design-0062 cited as if merged with an unvalidatable characterization.

Root cause, honestly: I wrote claims from my screen memory of runs I did not commit — the exact false-completion class from the 0062 lane, now in the evidence layer. Fixes (this round): re-ran and committed the edit/diff permission capture (editperm, with the ask-config); re-ran and committed the full silent-ignore arc (wrong shape → 24s wedge → notification-cancel recovery → correct-shape grant — one transcript proving the whole §5.1); re-ran fork with full-payload logging (user_message_chunk replay payload now auditable); committed the ask-configs and a binary-zod-extract.md so [zod] citations are in-repo; corrected the count to seven everywhere; user_message_chunk row now states replay-only behavior + a §4.2 replay-ingestion row; session/list row drops "paged" and the limit divergence entered §5.3 + §8; cancel notification-only entered §3.1/§5.4; timestamps fixed; 0062 cited as pending-merge (#1585) with an accurate characterization; §2 now states twelve runs/seven committed/the selection criterion. Bonus findings from the fix runs: the wedge blocks a queued second prompt entirely; `stopReason:"cancelled"` now live-observed.

## Next Steps

- r2 verdict watch on #1596.
- If approved: Stage A charter is a normal implementation story (adapter vocabulary rename + golden regen).

## Files Modified

- `design/0063_2026-09-28_acp-spike.md` (new; r1-corrected)
- `design/evidence/acp-spike/` — drivers (`acp_drive.mjs`, `acp_silentignore.mjs`, `acp_lifecycle.mjs`, `fork.mjs`), transcripts (`transcript-{ping2,tools2,perm3,editperm,silentignore,lifecycle,fork}.ndjson`), `playground-config/` (the two ask-configs), `binary-zod-extract.md` (new at r1)
- `worklogs/NNNN_2026-09-28_acp-spike-design.md` (this file)
