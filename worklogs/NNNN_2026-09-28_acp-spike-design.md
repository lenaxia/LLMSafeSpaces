# Worklog: ACP spike + design-watch (issue #1595, design 0063)

**Date:** 2026-09-28
**Session:** ses_f4990c442ffei8kVvWzcN9qMHU
**Status:** Design doc drafted on branch `design/acp-spike`; PR up; awaiting review rounds.

## Objective

Per issue #1595 (owner-chartered): run the pinned opencode 1.18.15's `opencode acp` server, enumerate its session_update shapes and the permission round-trip, map them onto the session contract's five part types, and settle the staged path / upgrade-resilience accounting / spec-maturity risk in a design/NNNN document. Spike first, doc second — evidence before prose.

## Work Completed

- Verified the pinned binary: `/opencode/usr/local/bin/opencode` = 1.18.15; `opencode acp` subcommand present (JSON-RPC over stdio; `--pure`, `--cwd` flags used).
- Built three node stdio drivers (`acp_drive.mjs`, `acp_lifecycle.mjs`, `fork.mjs`) in /tmp, then committed under `design/evidence/acp-spike/` with five verbatim transcripts (ping2, tools2, perm3, lifecycle, fork) — every message both directions, timestamped.
- Enumerated the surface live: initialize/session new/prompt/close/list/set_config_option/set_model/fork/load; nine of eleven `sessionUpdate` variants observed on the wire; tool kinds edit/execute/other with full payload anatomy.
- Captured the permission round-trip end-to-end: `session/request_permission` (options allow_once/allow_always/reject_once) and the working response shape `{outcome:{outcome:"selected",optionId}}`.
- Cross-checked against the zod schemas bundled in the binary (strings extraction) — the full method tables, the 11-variant update union, tool kinds, stopReasons.
- Wrote `design/0063_2026-09-28_acp-spike.md`: mapping table (zero part-type changes; plan/todowrite → Tool per 0049 discipline), staged path (A: ACP-shaped adapter vocabulary now; B: transport migration assessed-only; C: triggers named), retire/remain accounting vs the #730 golden fixtures + dialect.go + translate.go, spec-maturity risk register.

## Key Decisions

- Stage A only requests a charter; transport migration (Stage B) stays assessed-only behind named triggers (second agent runtime funded; editor-direct access chartered).
- The §5 divergence set (permission shape singular optionId + silent-ignore on malformed consent; fork-requires-cwd; session/list cross-cwd visibility; emission gaps) is proposed as the ACP golden-fixture v0.
- The silent-ignore finding (malformed consent response → tool never runs, no error) is recorded as safety-relevant fail-closed behavior requiring a pipe watchdog in any Stage B.

## Blockers

None. (Driver iteration notes: first permission-response attempts used the draft-spec `optionIds` array and were silently ignored — resolved via the binary's zod; JSON-RPC duplicate-id responses are ignored by the server, so shape variants must be tested one per fresh request, not ladder-retried.)

## Tests Run

Spike-only lane: five protocol runs against the pinned binary (transcripts in evidence/). No Go code touched; `go build ./...` not applicable to this doc+evidence diff (verified: no .go files changed).

## Next Steps

- Design-doc PR through the full protocol (designs get review rounds).
- If approved: Stage A charter is a normal implementation story (adapter vocabulary rename + golden regen).

## Files Modified

- `design/0063_2026-09-28_acp-spike.md` (new)
- `design/evidence/acp-spike/{acp_drive.mjs,acp_lifecycle.mjs,fork.mjs,transcript-ping2.ndjson,transcript-tools2.ndjson,transcript-perm3.ndjson,transcript-lifecycle.ndjson,transcript-fork.ndjson}` (new)
- `worklogs/NNNN_2026-09-28_acp-spike-design.md` (this file)
