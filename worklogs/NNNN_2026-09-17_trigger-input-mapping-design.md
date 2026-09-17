# Worklog: #1425/#1419 — trigger input mapping design (0059)

**Date:** 2026-09-17
**Session:** Trigger-fired (and webhook-fired) workflow runs bypass inputSchema — the run input is the system envelope, so schema-authored workflows see nothing at top level and behave differently between manual and triggered runs.
**Status:** Complete (design stage — implementation in follow-up PRs)

---

## Objective
Write design/0059 proposing the unified fix for #1425 (cron/DAG) and #1419 (webhook): make the fire path capable of producing a schema-satisfying run input, then validate it. Docs-only PR.

## Work Completed
- Read both issues + the code truth on the branch base: both fire paths (`engine.go` fireWorkflowTarget, `webhook_receiver.go` HandleWebhook) stamp the envelope into `workflow_runs.input`; the cron path documents the deliberate bypass (#1425 comment); manual runs validate since #1413.
- Wrote design/0059_2026-09-17_trigger-input-mapping.md: problem with live examples; four-option space from #1425 (+ #1419's inputFrom); recommendation = O2 (per-trigger static `input` + `inputFrom: envelope|body|mapped`) composed with a narrowed O1 wiring guard at create, O3/O4 rejected with reasons.
- Elaborated the recommendation: migration 000031 columns (`input jsonb`, `input_from` CHECK default 'envelope'), DTOs, validation rules V1–V7 at trigger create/update, one shared resolver in pkg/workflows used by both fire paths, failed fires on schema mismatch reusing the #1412 pattern (`validation_error` status already in the CHECK), compat table (legacy triggers byte-identical; R4 fixture untouched), MCP tool-description updates (incl. correcting workflow_create's "enforced on every workflow_run input" claim), rollout notes, test plan with e2e row R6 extending local/issue-1410-1412-automation-e2e.sh, six open questions.

## Key Decisions
- Recommended option: static input + inputFrom mapping (body mode is the #1419 headline fix; mapped mode the #1425 one).
- Fire-time validation only for opted-in triggers — unconditional validation would brick every legacy schema-bearing DAG trigger (the envelope can never satisfy required non-envelope properties).
- `validation_error` fire payloads are typed `{jsonPointer, keyword, message}` violations — locations only, never instance values, 4 KiB cap (the #1419-thread constraint; raw jsonschema messages embed instance content and `action_result` is a rendered audit field).
- V6 wiring guard on update applies only when the patch changes `workflowId`/`input`/`inputFrom` — legacy triggers stay editable (rename/schedule patches re-run no validation).
- `validation_error` fires count toward auto-disable (flagged as open question for the webhook-garbage case).

## Blockers
None.

## Tests Run
None (docs-only). Every file:line citation in the design re-verified against the branch base @ 826836c3.

## Next Steps
PR → review; implementation PRs (migration + resolver + handlers + MCP descriptions + R6) will carry "Refs #1425"/"Refs #1419" and close them.

## Files Modified
- design/0059_2026-09-17_trigger-input-mapping.md (new)
- worklogs/NNNN_2026-09-17_trigger-input-mapping-design.md (new, this file — number bot-assigned at merge)
