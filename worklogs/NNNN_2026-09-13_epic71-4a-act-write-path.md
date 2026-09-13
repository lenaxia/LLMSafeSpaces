
## Review r4/r5 remediation

**r4 (stale against main):** rebased onto #1361's dialect retirement (NewProxyHandler port); full suites green on the merged tree.

**r5 (W6 400 — the e2e earned its keep):** the missing-ask harness contract re-captured LIVE (question-reject → **404** QuestionNotFoundError; permission-reply → **404** PermissionNotFoundError — recorded in `missing_ask_reject_contract`): the D1 fallback and the 404-typed fold consume the RIGHT codes. The 400 was the AUTHORITY rejecting the synthetic session id (its session validation runs before the answer's resolve-by-absence). Fix: W6 creates a REAL session via the API and seeds the record against it — matching what a real walk-away ask is (an ask on a session that exists). Re-land: pool green on the final head.

## Review r6 remediation — the true root cause

r6 refuted the r5 diagnosis (session validation) and identified the real defect: **the harness PREFIX-VALIDATES request IDs** — captured live: a `que_` id on the permission endpoint is `400 BadRequest/Params "Expected a string starting with \"per\""`, symmetric for `per_` on question. My D1 cross-kind fallback (question-reject 404 → permission reply) therefore converted the absence signal into a 400 the resolve-by-absence fold cannot consume — stranding the record (the W6 e2e failure, exactly).

**Re-coded (product code, all three sites):**
- Actor reject routing: prefix-aware — `que_` → question-reject ONLY (its 404 surfaces as the typed NotFound the authority folds, S6); `per_` → permission reply directly; a non-reject `reply` on a `que_` id is now a loud InvalidArgument (the vocabulary is permission-only).
- Actor legacy answer forms: `per_` goes straight to the permission reply; `que_` never probes permission (its 404 = absence); unknown prefixes keep the probe order (agent-agnostic callers).
- Flag-off `Adapter.RejectInput`: same prefix-awareness (the same-shaped assumption).
- Recorded as `cross_prefix_contract` in the fixture (the real bytes).

**Actor rows (4 new/rewritten):** reject-on-que hits exactly the question endpoint; dead-que reject surfaces 404 with ZERO cross-kind posts; reject-on-per goes direct; dead-que ANSWER surfaces 404 without permission probes. The unprefixed probe rows retained + retitled honestly.

**Narrative corrections:** the r5 worklog section's session-validation claim is WRONG — the harness contract was always 404; the defect was the cross-prefix fallback. This section supersedes it. W6's real-session change is still correct hygiene (the authority does validate sessions) but was not the fix.

## Tests run (r6)

- `go test ./cmd/workspace-agentd/ ./api/internal/handlers/ ./pkg/agent/...` — ok; `golangci-lint` 0 issues
