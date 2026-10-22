# Worklog: Structured review verdicts (#1832 item 5, part 1/2)

## Scope
The forge-agnostic verdict core (owner ruling on PR-B): a validated
payload persisted as an ordered per-task record + a guarded transition
through the EXISTING stage machine. Forge adapters map INTO the core.

- pkg/board/verdict.go: VerdictKind (approve|fail), findings
  {severity blocker|major|minor, pointer, criterionIndex? bounds-checked},
  ValidateVerdict (fail requires >=1 finding), RecordVerdict (ONE
  review_verdict event = record + transition outcome, idempotent per
  requestId; a refused transition persists applied=false
  transition="refused:<code>" — the chain keeps the evidence), and
  TaskVerdicts (the ordered chain read; B2's envelope input).
- Amended #1832 semantics pinned: the fail verdict rides the existing
  in_review→in_progress bounce — fresh claim minted, attempts UNCHANGED
  (a bounce is an iteration inside the verdict chain, never a
  failure-ledger entry). approve NEVER bypasses the Done guard.
- Surfaces (construction parity): REST POST /me/tasks/:id/verdict +
  GET /me/tasks/:id/verdicts; external MCP board_record_verdict +
  board_task_verdicts (26 board tools now); openapi paths + schemas;
  parity matrix rows (agent absent — reviewers are manager surfaces),
  replay/audit/route-table entries, tool counts 50→52.

## Tests
- pkg/board/verdict_test.go: the validation matrix (10 rows, pure).
- pkg/board/verdict_integration_test.go (PG harness, CI): approve-guard
  refusal persists + task unchanged; approve after criteria+evidence →
  done; chain ordered; fail bounce: stage in_progress, attempts
  UNCHANGED, NEW claim token minted.
- handlers board_verdict_rest_test.go: bind errors, perimeter 404,
  validation guard class, chain read.
- parity: both rows' allow arms + replay driver + audit stamps + route
  table; mcp allow arms + counts.

## Gates
go build/vet clean; handlers suite ok (79.6s incl. parity); pkg/board
ok; pkg/mcp ok; make openapi-validate ✓; make repolint all-pass.
