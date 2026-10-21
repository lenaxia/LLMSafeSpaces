# Worklog: #1817 — piece 1: user-stream session events carry the phrase (#1821 rapid slice)

**Date:** 2026-10-21
**Session:** continuation worker (orchestrator rapid-fix dispatch after the #1821 root-cause ruling). Worktree `wt-1817`, branch `fix/1817-streaming-busy-regression` off main @ 644446fa.
**Status:** user-broker half complete; the contract-stream half scoped for ruling (see Next).

## Root cause (adopted from #1821, investigator file:line-verified)
SSE session.status legs keyed by HARNESS id (proxy_usagestream.go:182) vs the frontend identity flipped to PHRASE (sessionindex rows) — the join misses for every minted row. One root cause, both symptoms: sporadic busy + dead live streaming (ChatPage.tsx:720 vs fold.ts:39 — same join, the contract-stream consumer). E2E mocks injected ONE id into both surfaces — every suite blind.

## Work Completed
### (a) RED-first pins (first commit)
`proxy_usagestream_phrase_test.go` — T0.1 (session.status / agent.question / agent.question.resolved each must carry the phrase + additive internal_session_id; RED on main with the exact root-cause predictions quoted in the commit message) + the degraded-passthrough pin (green on main — never-worse pinned) + T1.6 (the cross-surface join tripwire: list row keyed phrase + ABI event keyed harness → ONE joinable wire key). Fixture class: mockSessionIndex.phrases (dual-id).

### (b) Implementation (second commit)
`ProxyHandler.translateSessionIDToPhrase` (proxy_usagestream.go): reverse-resolve via the sessionIndex Register seam (idempotent, LRU-cached, degraded-safe) at each of the three emission sites; wire id = phrase; harness id additive as internal_session_id (map-typed Data both shapes); passthrough on no-phrase.

## Tests Run
- `go test ./api/internal/handlers/ -count=1` — FULL suite green (82s).
- golangci --new-from-merge-base: 0 issues.

## Next (scope ruling requested)
The STREAMING half of the acceptance: the contract-stream frames (the fold's key source) still carry harness ids — sessionstate/projection.go:417 `SessionId: id`. Two seams: (A) agentd emission translation (agentd natively knows both ids — cmd/workspace-agentd/session_ref machinery; ships fast via the OnRefresh overlay policy) — an ABI frame id-space change needing the freeze-gate's additive judgment; (B) api-side rewriting at the contractstream raw-forward boundary (parse→rewrite→remarshal per frame — hot-path cost, no ABI change). Recommendation: (A).
