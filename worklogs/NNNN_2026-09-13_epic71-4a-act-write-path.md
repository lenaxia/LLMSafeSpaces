
## Review r4/r5 remediation

**r4 (stale against main):** rebased onto #1361's dialect retirement (NewProxyHandler port); full suites green on the merged tree.

**r5 (W6 400 — the e2e earned its keep):** the missing-ask harness contract re-captured LIVE (question-reject → **404** QuestionNotFoundError; permission-reply → **404** PermissionNotFoundError — recorded in `missing_ask_reject_contract`): the D1 fallback and the 404-typed fold consume the RIGHT codes. The 400 was the AUTHORITY rejecting the synthetic session id (its session validation runs before the answer's resolve-by-absence). Fix: W6 creates a REAL session via the API and seeds the record against it — matching what a real walk-away ask is (an ask on a session that exists). Re-land: pool green on the final head.
