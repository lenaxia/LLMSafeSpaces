# Worklog: 0059 agentd half — trigger_create input-mapping contract + e2e R6

**Date:** 2026-09-17
**Session:** AGENTD + E2E half of design 0059 (trigger input mapping). The MCP `trigger_create` description must teach the `input`/`inputFrom` contract, and the automation e2e gains row R6 (webhook body mode over the rotated HMAC secret).
**Status:** Complete (R6 executes on the nightly after the core PR merges)

---

## Objective
Ship the agentd-facing half of design/0059: extend `trigger_create`'s tool description with the input-mapping contract (D7), pin it in the drift-pin suite, and add e2e row R6 per the design's test plan — all in `cmd/workspace-agentd/mcp_server.go(+_test.go)` and `local/issue-1410-1412-automation-e2e.sh(+_script_test.go)`. The API/engine half (migration 000031, resolver, V1–V7, sanitized violations) is the sibling session in /workspace/lss-0059-core; no api/, pkg/, or non-MCP agentd files touched.

## Work Completed
- `cmd/workspace-agentd/mcp_server.go` — `trigger_create` description extended with the design §3.7 contract verbatim: `input` (object) is the static run input for workflow-mode triggers, validated against the workflow's inputSchema when `inputFrom` is `mapped`; `inputFrom` selects the fired run's input — `envelope` (default — the system envelope {source, received_at, headers, body}), `body` (webhook only — the posted payload becomes the run input), `mapped` (the static `input` document); envelope-mode wiring to a schema requiring non-envelope fields is rejected with 400 — set `input`, use `inputFrom: "body"`, or relax the schema. Every pre-existing pinned phrase kept (workflowId camelCase, does NOT fire immediately, both are validated, etc.). Description-only change — the tool body already passes through verbatim.
- `cmd/workspace-agentd/mcp_server_test.go` — trigger_create guidance subtest extended with the 0059 pins: static run input, mapped-mode create-time validation, the three `inputFrom` spellings + envelope shape, body-mode semantics, the wiring 400 and its three remedies; `NotContains "input_from"` anti-drift (snake_case silently dropped — the workflowId lesson).
- `local/issue-1410-1412-automation-e2e.sh` — row R6 added per the design test plan, reusing the `api()` helper and the R5 required-`topic` schema workflow (standalone fallback create if R5 setup failed):
  - **R6a** envelope-mode cron wiring to the schema workflow → 400 (the D4 wiring guard).
  - **R6b** `inputFrom:"mapped"`: `input:{}` → 400; `input:{topic:"nightly"}` → 201 (V4 create-time validation).
  - **R6c** webhook trigger with `inputFrom:"body"` → `POST /api/v1/me/triggers/<id>/rotate-secret` → signed `POST http://127.0.0.1:${PORTFWD_PORT}<webhookUrl>` with `X-Hub-Signature-256: sha256=hex(hmac-sha256(body, secret))` (openssl): `{"topic":"e2e"}` → 202, fired fire, and a queued run whose input has top-level `topic` (polled via the workflow runs endpoint, matched on triggerId); `{"wrong":true}` → 202, `validation_error` fire carrying `schema_mismatch` with NO instance echo, and still exactly 1 hook-triggered run. All created triggers/workflows appended to the trap's cleanup arrays (including unexpected-success legs). Header comment + verdict updated to R1-R6.
- `local/issue_1410_automation_e2e_script_test.go` — structural pins extended: R6a/R6b/R6c row labels, `inputFrom:"body"`/`inputFrom:"mapped"` spellings, `/rotate-secret`, `X-Hub-Signature-256: sha256=`, `.input.topic == "e2e"`, `select(.status=="validation_error")`, `*"schema_mismatch"*`, `R6c: violating payload queued no run`; no-calendar-date-rot rule kept (R6 uses monthly cron `0 5 1 * *`, never fires in-run).

## Key Decisions
- Description-only change in mcp_server.go — design D7 explicitly notes the tool body passes through, so no schema/plumbing change was warranted.
- R6c matches the run by `triggerId == <hook>` AND `input.topic == "e2e"` (distinct from R5b's manual `topic:"ship"` run) so reusing the R5 workflow cannot cross-contaminate assertions.
- The violating payload is `{"wrong":true}` and the no-instance-echo assertion checks the fire's `actionResult` does not contain `wrong` — mirrors the design §3.5 typed-violations-only contract at the e2e level.
- Defensive cleanup: every create leg (including the ones expected to 400) appends its id to `created_triggers` whenever a row unexpectedly appears, so the nightly owner's trigger list stays clean even on regressions.
- Happy/violating webhook legs both expect 202 — the design's delivery-vs-input-contract split (a non-2xx would make GitHub-style senders retry a permanently invalid payload).

## Blockers
None. R6 cannot run in-session (needs the live kind cluster + the sibling's API fields); it is structurally pinned (`bash -n` + row needles) and executes on the nightly after both PRs merge.

## Review Iteration 1 (PR #1438, CHANGES_REQUESTED → addressed)
- **Single-inflight race (findings 1+2):** R6c's deliveries raced `uq_workflow_run_single_inflight` — R5b's manual run (and R6c's own conforming run) hold the workflow's only inflight slot until the scheduler tick fast-fails them (~10s), so a delivery landing mid-flight 409s with a `skipped` fire instead of the asserted 202/`validation_error`. Fixed with `r6_wait_slot()` — polls the workflow runs until no run is `queued`/`running` before EACH signed delivery (drain-wait pinned structurally).
- **R6b-bad cleanup leak (finding 3):** the mapped-`{}` leg captured no id, so an unexpected 201 (pre-core merge: `inputFrom` silently dropped by JSON binding) leaked a monthly-cron trigger every nightly. Fixed with the R6a-style id-capture + `created_triggers` append guard (pinned structurally).
- **D7 agentd-side trio (Project Alignment):** delivered the remaining §3.7 agentd descriptions this half owns — `trigger_update` notes `inputFrom`/`input` are patchable; `workflow_create`'s "enforced on every workflow_run input" corrected to "enforced on manual runs and on fired runs that carry input mapping (`input`/`inputFrom`)" (with a NotContains anti-drift on the old misconception); `trigger_fires` names the `validation_error` status, its `actionResult` schema_mismatch trail, and the locations-only sanitization. ④ pkg/mcp pins and ⑤ openapi/docs belong to the core half.
- Minor: the inert "rejected with 400" pin tightened to "rejected with 400 — set `input`" so it uniquely pins the wiring-guard sentence (it previously also matched the pre-existing cron-validation sentence).

## Tests Run
- `bash -n local/issue-1410-1412-automation-e2e.sh` — clean.
- `go test ./cmd/workspace-agentd/ ./local/ -count=1` — ok (includes TestMCPHandler_ToolDescriptionGuidance and TestIssue1410E2EScript_*).
- `golangci-lint run ./cmd/workspace-agentd/... ./local/...` — 0 issues.
- `gofmt -w` on both touched Go files — clean.

## Next Steps
PR → review iterations → APPROVED (never merge; the core PR closes #1425/#1419, this one carries Refs).

## Files Modified
- cmd/workspace-agentd/mcp_server.go (trigger_create description)
- cmd/workspace-agentd/mcp_server_test.go (trigger_create guidance pins)
- local/issue-1410-1412-automation-e2e.sh (row R6)
- local/issue_1410_automation_e2e_script_test.go (R6 structural pins)
- worklogs/0981_2026-09-17_agentd-trigger-input-mapping-e2e-r6.md (new, this file — number bot-assigned at merge)
