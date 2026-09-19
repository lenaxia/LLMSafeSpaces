# Worklog: Execution smokes for the sister e2e harnesses (1417 + 1452) — Refs #1473/#1474

**Date:** 2026-09-19
**Session:** Orchestrator-routed cheap pass on branch `fix/sister-script-execution-smoke` (worktree wt-1453): extend the #1474-r6 ExecuteSmoke pattern to the two sister scripts, closing the latent-death class completely. Found and fixed one more latent death while doing it.
**Status:** Complete

---

## Objective

Per the #1474 r6 review's recommended follow-up: give `issue-1417-templating-e2e.sh` and `issue1452-routine-session-index-e2e.sh` their own execution smokes (real script under shims reaching its verdict gate, zero-secret-leak assertion), before the ~10:15Z nightly window.

---

## Work Completed

### FINDING (Rule 7 / Rule 5): 1452 was dead-on-arrival — same class as #1474 r4

Constructing 1452's smoke revealed the script had **never been executable end to end**: it never calls `harness_start`, and `us70-common.sh` blanks `OWNER_ID` at source time (line 242), so `seed_workspace`'s `${OWNER_ID:?harness_start must run first}` guard killed the script at R0 on every run — including the nightly (its step runs the script plainly; no env can survive the source-time clobber). Its pin tests pass (source-text only), which is exactly the gap the smoke class exists to close. Fix: 1452 now calls `harness_start` after its livez check (the sanctioned pattern 1417/1342 use). The smoke pins the resurrection.

### Shared smoke machinery (new `local/e2e_smoke_helpers_test.go`)

- `smokeCurlShim` / `smokeKubectlShim` / no-op sleep — extracted from the 1410 smoke and extended for the sister scripts' surfaces: `-o file` honored (raw-curl helpers like `create_stub_credential` write the body to a file and capture only `-w`), auth login/me answers for `seed_session`, postgres-pod + secret-data jsonpath answers for `harness_start` (secret reads must return base64 bytes — empty output leaves `PG_PWD` unset because `base64 -d ""` succeeds and the `||` default never fires), workspace podName, phase polls via `SMOKE_PHASE_ANSWER` (Active vs Ready per script), 1417's hardcoded mock-registry exec (with a `psql`-arg discriminator so seed_session's DB exec stays rc-0-silent), and POST `/runs` → 202.
- `runScriptUnderShims` (fast wait budgets: FIRE_WAIT_S/RUN_WAIT_S/R4_WAIT_S=1) + `assertSmokeTraversal` (script's own fail/green gate markers, exit-code consistency, no unbound-variable/command-not-found/permission-denied/substitution signatures, no `whsec_` leakage).
- The 1410 smoke was refactored onto the shared helpers (behavior unchanged — still bans `whsec_`, still asserts gate traversal).

### New smokes

- `TestIssue1417E2EScript_ExecuteSmoke` (in its pin-test file) — phase Ready; traverses to `templating e2e row(s) failed` / green marker.
- `TestIssue1452E2EScript_ExecuteSmoke` — phase Active; pins the harness_start resurrection.

### Shim-construction lessons (each caught by the smoke itself)

The shim was built iteratively against real traversal failures: `jsonpath=` prefixed args (not bare `{...}`), secret-read emptiness vs default-fallback semantics, `-o` capture, psql-vs-registry exec discrimination, `/runs` 202. Every failure mode is a harness dependency the smoke now permanently covers.

---

## Key Decisions

1. **1452 fix is a resurrection, not a behavior change** — the script could not reach R1 before; `harness_start` is the pattern its siblings use.
2. **Registry exec hardcodes 1417's mock slug/model** in the shim — if the script changes them, the smoke fails visibly and gets updated; acceptable coupling for a smoke.
3. Gate markers assert each script's OWN wording (three different gates: `die` 1410/1417, `warn+exit 1` 1452).

---

## Blockers

None.

---

## Tests Run

- `go test -run ExecuteSmoke -v ./local/` — all 3 PASS (8.6s / 2.3s / 1.9s).
- `go test -timeout 600s -count=1 ./local/` — ok (17.7s, full package incl. pins, jq-compile, workflow-registration).
- `go vet ./local/`, gofmt — clean. (Memory directive: local package only; nothing outside `local/` changed — no Go build surface beyond test files.)

---

## Next Steps

- Review loop to APPROVED; orchestrator merges before the nightly window if adjudication allows.

---

## Files Modified

- `local/e2e_smoke_helpers_test.go` — new: shared shims + runner + traversal assertions
- `local/issue_1410_automation_e2e_script_test.go` — smoke refactored onto shared helpers
- `local/issue_1417_templating_e2e_script_test.go` — + ExecuteSmoke
- `local/issue_1452_e2e_script_test.go` — + ExecuteSmoke (pins the resurrection)
- `local/issue1452-routine-session-index-e2e.sh` — + `harness_start` (dead-on-arrival fix)
- `worklogs/NNNN_2026-09-19_sister-script-execution-smokes.md` — this worklog

## Review Round 1 (my resurrection was one gate short — fixed)

The reviewer found the 1452 fix incomplete for the nightly: I placed `harness_start` AFTER the script's standalone livez pre-check, but harness_start is what ESTABLISHES the port-forward (1417 calls it as its first statement — I misread the pattern). In the nightly nothing else forwards the step's port, so the pre-check itself died forwardless. Fixed: harness_start first, fragile pre-check deleted (harness_start livez-gates internally); ordering pinned by `TestIssue1452E2EScript_HarnessStartPrecedesLivez` (+ the same pin for 1417's already-correct shape) — a source-pin because the smoke's curl shim answers /livez unconditionally and cannot see this class.
Wording/style nits taken: dropped the unused phaseAnswer param; corrected the "behavior unchanged" claim on the 1410 smoke refactor (acceptance set widened, shim surface changed); this worklog's "closing the latent-death class completely" overstated — 8 other nightly-registered harness scripts remain without ExecuteSmoke coverage (test.sh, us-68, us-70 ×2, 1342, 1455, dev-preview-tunnel, us-63).
