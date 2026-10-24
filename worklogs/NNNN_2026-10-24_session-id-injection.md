# Worklog: #1919 — session-ID injection: phrase-carried sentinel + raw-id leak audit

**Date:** 2026-10-24
**Branch:** `fix/session-id-injection`
**Status:** Part 1 complete (red pin e61dc339 → fix 71565e8e, full suites green); Part 2 audit in progress

---

## Objective

The opencode harness adapter's origin-injection plugin
(`runtimes/opencode/plugins/llmsafespaces-origin.js`) stamps the calling
session's identity onto platform MCP tool calls. The raw `ses_…` form
was reaching agent- and user-facing surfaces. Part 1: the delivered
agent-message sentinel must carry the platform 3-word phrase. Part 2:
sweep every other surface where raw opencode session IDs leak through
to agents/users; fix clear bugs red-first here, file design questions
as issues.

## Pre-work validation (standing rule)

- Reproduced on main @ c7694117: warm-replica probe (fake agent,
  replica holds `amber-falcon-glides` → `ses_3`) — result fields
  phrase-emit (`"origin":"amber-falcon-glides"`) while the delivered
  sentinel carried `{"fromSession":"ses_3"}` (the orchestrator's live
  datapoint (a), mechanism-identified).
- Already-fixed sweep: open PRs #1917/#1916/#1904/#1897/#1804 — none
  touch the injected-origin/sentinel surface. Merged prior art built
  the machinery this lane finishes: #1881 (phrase persistence),
  #1756/#1806 (emitID/emitIDFields emission contract).
- Legacy-phrase datapoint: post-restart phrase assignment for
  `id_deprecated` rows is the designed backfill —
  `EnsureSessionRegistered` (`short_session_ids.go:112-129`, `UPDATE …
  WHERE short_id IS NULL RETURNING`) driven by the convergence sweep
  (`sessionindex/service.go:172`), learned agentd-side via registry
  pull (`session_ref.go`). Not a bug.

## Part 1 — the sentinel (fix, red-first)

- RED e61dc339: `cmd/workspace-agentd/sentinel_phrase_1919_test.go` —
  warm-replica leg fails on main (`fromSession` = `ses_3`, not
  phrase-shaped); protective cold-replica leg born green (raw fallback
  keeps attribution, #1756 §1 parity).
- FIX 71565e8e: `mcp_tools.go` Compose boundary —
  `Origin{FromSession: emitID(origin), Mode: mode}`. Validation
  (`resolveSessionRef` + `SessionExists`), the self-send guard, and
  the #1530 recovery comparisons stay canonical. Recipients can reply
  to a phrase-valued `fromSession` (resolution accepts short ids).
- Plugin UNCHANGED per orchestrator ruling recorded in issue #1920:
  the wire attestation is agentd-internal (injected post-emission,
  never model-visible), canonical is the durable attestation form, and
  a plugin→replica lookup path would add attestation failure modes.
- Legacy pins (mcp_tools_test.go:1631 etc.) required NO migration:
  they run in the cold-replica window where the raw fallback is the
  contract — identical before and after the fix.

## Part 2 — raw-ID leak audit (complete)

Method: walk every surface where a session identifier is emitted
toward an agent (MCP results, message payloads) or a user (REST
bodies, SSE, frontend renders), classifying repo-fixable vs
platform-side vs by-design.

| Surface | Form on main | Verdict |
|---|---|---|
| agent-message sentinel `fromSession` | RAW always, even warm | **BUG — fixed here (#1919)** |
| send_message result fields (origin/session_id/warnings) | phrase-first, flagged degraded | clean (#1756/#1806/#1816) |
| session_metadata rows + not-found errors | phrase-first via emitIDFields/emitID | clean |
| session_list rows; create/delete/abort/archive/rename results | phrase-first, flagged | clean |
| SSE user-stream events | phrase at the edge + additive `internal_session_id` + `session_id_deprecated` flags | clean (#1817/#1821) |
| REST GET /sessions listing | phrase-first; backfill-window degradation documented in `types.SessionListItem` | clean — heals via the sweep (verified below) |
| REST session routes `:sessionId` | phrase/alias resolved at the edge; RFC-8594 Deprecation/Sunset headers on legacy input (no raw ids in header values) | by-design |
| `X-Session-Ref-Resolved` response header | canonical | by-design (#1816 bridge; consumed by pkg/mcp for dual-space contract matching) |
| workflow/trigger engine envelopes (`NodeExecResponse.SessionID`/`deletedSessionId`) | canonical, engine↔agentd loopback | by-design internal (#1470/#1882 contracts) |
| workflow_runs / trigger_fires records | no session ids in run records (openapi + engine audit) | clean |
| board `Task.assignee_session` (drawer render, health, manager board_list/board_health passthrough) + `board_events.actor.session_id` | RAW | **LEAK — filed #1922** (fix has design choices: seam, workspace resolution, degraded posture, audit immutability) |
| agentd/API structured logs | canonical | ops-facing by design (accepted) |
| frontend session joins | phrase-keyed; `internal_session_id` correlation-only, never rendered | clean |

Orchestrator datapoint (b) (legacy `id_deprecated` rows): verified the
healing path — `EnsureSessionRegistered` backfills pre-rollout rows
atomically (`api/internal/services/database/short_session_ids.go:112-129`,
`UPDATE … WHERE short_id IS NULL RETURNING`), driven by the
convergence sweep (`api/internal/services/sessionindex/service.go:172`)
and restart passes; agentd learns via registry pull
(`cmd/workspace-agentd/session_ref.go`). The orchestrator's live
observation (post-restart phrases assigned to legacy sessions) is this
machinery working. Not a bug.

Orchestrator datapoint (c) (API deprecation warnings citing raw ids):
no warning TEXT on main cites a raw id — Deprecation/Sunset headers
are RFC-shaped (`api/internal/middleware/session_ref.go:64-72`), and
agentd deprecation notes echo only the caller's own input. The
raw-bearing emissions are the flagged `session_id_deprecated`
degradation markers (by design) and the `X-Session-Ref-Resolved`
bridge header (by design, #1816). Residual raw exposures beyond
those: the board surfaces (#1922).

Design question filed by the lane: **#1920** — phrase-at-the-plugin
ruled AGAINST (wire attestation is agentd-internal and canonical is
the durable form; orchestrator ruling recorded in the issue).

## Union merge

Main advanced c7694117 → cdde5e72 (post-#1804/#1897/#1917). Merged
clean (2b01b0a5), zero conflicts. #1897 semantic overlap check: the
diff touches `session_shortids.go` (`forget` — delete invalidates the
replica), `workflow_execute.go`, and a new test; NO change to the
Compose/send path. Interaction with this fix is compositional:
post-`forget`, a deleted session's phrase leaves the replica and
emitID degrades to raw — and dead sessions neither send (validate-by-ID
fails) nor receive. No semantic conflict; mechanical only.

## Gates

- `go test ./cmd/workspace-agentd/ ./pkg/session/agentmessage/ ./pkg/shortid/` — ok (372s agentd full suite) at 71565e8e; targeted re-run green at merge head 2b01b0a5; `go build ./...` clean.
