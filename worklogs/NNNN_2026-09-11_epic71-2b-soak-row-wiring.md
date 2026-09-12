# Worklog: Epic 71 / 2b (part 5) — the soak row wired + leg-10 parse-site pins

**Date:** 2026-09-11
**Session:** Scope-completion pass (the "are you done?" audit found two gaps in my own claim: the soak row's execution wiring and #1312 change item 4's every-parse-site mandate). Agent: opencode-kestrel [glm-5.3].
**Status:** In Progress

---

## Objective

Close the gaps: (1) the #1312 soak row — driver existed, execution did not; (2) leg-10 wire-drift pins at every hand-adjacent parse site — abiclient was pinned, the API's terminus and actions seams were not.

---

## Work Completed

### The soak row, wired for execution (#1312's 2b merge gate)
- `TestSoak_Dispatch` — the env-driven hours-scale soak row (`LLMSAFESPACES_SOAK_DURATION/_SESSIONS/_FAULT_RATE/_SEED`; skip unless set / under -short). Gates: zero violations, samples exist, worst convergence within the lease bound (+drift epsilon). Verified live: 10s dispatch locally (8 sessions, λ=0.5, seed 1) → maxL3 52.6ms.
- `.github/workflows/epic71-soak.yml` — dispatchable (duration default 2h0m0s = the ≥2h gate, sessions, rate, seed); standard runner (in-process soak — the real authority + fault stream; never occupies the privileged dind pool); go test -timeout derived from duration + 30m; job cap 360m. The kind-native live-stack variant remains the pool upgrade (owner-scheduled) — recorded here and in the reserved comment, not silently assumed.

### Leg-10 parse-site pins (#1312 change item 4 — "every hand-adjacent parse site")
- `TestAbiAct_WireDriftCorruption` (proxy_actions): the four corruption modes riding HTTP 200 through the REAL generated handler must fail the parse loudly — no payload escapes. Mutation-validated: swallowing the Unmarshal error (the lenient-parser regression) fails the pin.
- `TestAgentdDeliver_WireDriftCorruption` (outbox_terminus): the same four modes against the terminus's Deliver — a corrupted ack must surface as a retryable delivery error, never a silent completion. (The terminus is the #1308 incident site — its happy/error wire shapes were pinned; the drift shapes now are too.)

---

## Key Decisions

1. The soak's CI-realizable form is in-process hours-scale; the live-stack (kind) variant is explicitly the pool upgrade — declared, not conflated.
2. Parse-site pins assert FAIL-LOUD per mode; discrimination proven by parser-leniency injection for the actions seam (the terminus pin shares the mechanism).

---

## Blockers

None. After merge: dispatch the ≥2h soak (the 2b gate) + a US-70 pool regression run on main (the epic convention — #1328 touched the outbox's registration; no pool run since).

---

## Tests Run

- `LLMSAFESPACES_SOAK_DURATION=10s go test -run TestSoak_Dispatch -v` — soak complete, PASS
- `go test -race -run "WireDriftCorruption" ./api/internal/handlers/` — ok
- Mutation: lenient abiAct parser → pin FAILS; restored → green

---

## Next Steps

1. PR; review-iterate; merge.
2. Dispatch epic71-soak at 2h0m0s; dispatch the US-70 pool (fast knobs) on main; record both in the reserved comment.

---

## Files Modified

- cmd/workspace-agentd/faultmatrix/soak_test.go (TestSoak_Dispatch + env helpers)
- .github/workflows/epic71-soak.yml (new)
- api/internal/handlers/proxy_actions_test.go (drift pin)
- api/internal/handlers/outbox_terminus_test.go (drift pin)

---

## Review r1 remediation (2026-09-11, PR #1352)

- **The terminus pin now discriminates** (r1 finding 1, probe-confirmed by the reviewer against my blanket claim): per-mode `require.ErrorAs(*json.SyntaxError)` (the failure IS the parse) + `NotContains("agentd owns admission")` (never the ledgered-window timeout a lenient parser degrades into). Injection-validated in-session: the swallowed-Unmarshal mutation on `post` FAILS the pin; restored, green. The earlier "(the terminus pin shares the mechanism)" worklog line was an unvalidated assumption — wrong, corrected here.
- **The third hand-adjacent procedure pinned** (r1 finding 2): `TestTerminus_StatusWireDrift_FailsOpenToKeyedRePOST` — GetDeliveryStatus corrupted on the retry path's prior-attempt lookup degrades to the KEYED re-POST at attempt+2 (fail-open, safe only because the harness write is keyed), observable via the leg-7 call recorder — never a phantom completion. All four modes.
- **The workflow cannot go green on a no-op** (r1 finding 3): input validation step (empty/unparsable duration fails) + the run step tees and greps for `--- SKIP: TestSoak_Dispatch` and fails on it.
- Wording fixed: corrupted bytes ride the faultMiddleware and bypass the generated handler (route/transport real; the handler is not).

## Tests Run (r1)

- `go test -race -run "WireDrift" ./api/internal/handlers/` — ok (both seams + the status procedure, 4 modes each)
- Mutation: lenient terminus parser → pin FAILS; restored → green
- Workflow guard tokens verified present


## Review r2 remediation (2026-09-11, PR #1352)

- **"Ran and passed", not just "not skipped"** (r2 finding 1): the run step now also requires `--- PASS: TestSoak_Dispatch` in the teed log — a zero-match `-run` (rename/move/build-tag) exits 0 with no tokens and now fails the guard.
- **The ≥2h gate is a check, not a default** (r2 finding 2): the validation step rejects durations below 7200s unless explicitly suffixed `-iter` (an iteration cycle); the run step strips the suffix before the Go test sees it.

## Tests Run (r2)

- Workflow guard tokens verified; no Go changes this round (r1 gates stand).


## Review r3 remediation (2026-09-11, PR #1352)

- **The quoting bug is dead by construction:** both python blocks now run via quoted heredocs (`python3 - <<'PY'`) — the shell never rewrites python syntax (the r2 f-string made every input a SyntaxError; caught by the reviewer's execution, not by me running the step I shipped — the exact class this workflow's own guards exist for). Validated by a local simulation of the EXACT step bodies: 2h0m0s→OK, 3h→OK, 10s-iter→OK, 500ms/1.5h→below-gate, garbage/empty→unparseable. The parser now handles ms/us/ns and decimals (Go duration syntax).
- The r2 PASS-token guard kept as-is (reviewer-verified correct).

## Tests Run (r3)

- Step-body simulation: ALL-SIM-PASS (7 cases)
- No Go changes; r1/r2 gates stand

