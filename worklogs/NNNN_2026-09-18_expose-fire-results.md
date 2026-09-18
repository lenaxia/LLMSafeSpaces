# Worklog: #1441 — expose routine fire results (the invisible failure column)

**Date:** 2026-09-18
**Session:** Debugging #1441 (intermittent routine-fire failures) hit a wall: failure causes are written to the trigger_fires.result column (UpdateTriggerFireResult), but ListTriggerFires never SELECTed it and the response only marshaled action_result — routine failures were undiagnosable from outside by construction. Bisect so far: capture-full alone and memory-without-marker both survive; the failure is intermittent and correlates with the {{.prevResult}} injection path.
**Status:** Complete (observability half)

---

## Objective
Make every fire row fully observable so the intermittent cause names itself on the next repro.

## Work Completed
- TriggerFireRow.Result + the two fire SELECT sites + scan carry the result column.
- TriggerFireResponse.Result (omitempty) marshaled in triggerFireRowToResponse.
- Handler test: a failed routine fire's cause is visible in the fires list.

## Key Decisions
- Result stays distinct from ActionResult (pre-execution action payload vs execution outcome) — both now surface.

## Blockers
None.

## Tests Run
TestTriggerFires_ExposeRoutineResult; full handlers + workflows suites green.

## Next Steps
Ship → prod → rerun the memory bisect; the failed fire's result field will carry the engine's errMsg verbatim → root-cause the intermittent failure.

## Files Modified
- pkg/workflows/store.go (+row/SELECT/scan), pkg/types/workflows.go, api/internal/handlers/triggers.go (+test)
