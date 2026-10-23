# Worklog: #1844 (PR-2) — the zero-run watchdog (automated detection + recovery sweep)

**Date:** 2026-10-23
**Session:** Lane #1844 (run watchdog), PR 2 of 2. PR-1 (#1874) added the manual dispatch levers; this PR automates the detection the issue asks for ("if a head push has zero runs N minutes after push, auto-comment or re-dispatch").
**Status:** Complete

---

## Objective

An external poller that notices absent runs: every 10 minutes, enumerate open PRs; a head older than 15 minutes carrying ZERO total check-runs (and no Actions skip marker) is a #1844 delivery fault → post the namespaced #1844 comment (deduped per head SHA) + dispatch ci.yml on the branch (the lever verified live 02:23Z). The decision matrix lives in Go, red-first, per the numbering-gate pattern from #1867.

## Detection predicate (pkg/runwatchdog.Evaluate)

A PR head is a fault iff ALL hold:
1. **age ≥ 15 min** (default, `WATCHDOG_MIN_AGE_MINUTES`-tunable; ci.yml starts within seconds of a delivered event, so 15m covers event queuing + runner startup with wide margin),
2. **head commit message carries no Actions skip marker** (`[skip ci]`/`[ci skip]`/`[no ci]`/`[skip actions]`/`[actions skip]`, case-insensitive — the renumber bot's commits legitimately produce zero runs; without this the watchdog false-alarms on every bot commit on an open PR),
3. **TOTAL check-runs on the head SHA == 0** — total, not per-workflow: ci.yml triggers on every pull_request with NO paths filter, so any delivered synchronize produces at least its runs; path-filtered workflows (secrets-integration, posture-gate) legitimately skip, and keying on them would false-positive. Zero-TOTAL also matches the documented #1843 shape ("zero check-runs on the head").

### Red-first evidence

Test suite written FIRST and run against the naive baseline (`NaiveEvaluate`: age+zero-runs only, the pre-fix logic) — 6 behavioral FAILs captured:

```
--- FAIL: TestEvaluate_FaultDetectedOnSuppressedHead
--- FAIL: TestEvaluate_SkipCiHeadIsNotAFault
--- FAIL: TestEvaluate_DedupMarkerSuppressesComment
--- FAIL: TestEvaluate_DedupMarkerOtherSHAIgnored
--- FAIL: TestEvaluate_NoAutoRetriggerLabel
--- FAIL: TestFaultCommentBodyContract
```

Then `Evaluate` implemented → all green. Control kept green by design: `TestNaivePredicateFlagsSkipCi` pins that the naive predicate DOES flag a `[skip ci]` head — the documented insufficiency the full predicate exists to fix (#1867's control-test convention).

### Dedup + escalation

One fault comment per head SHA, via the marker `<!-- run-watchdog: fault sha=<full SHA> -->` (ai-workflows verdict-marker pattern). The literal is pinned by `TestFaultMarkerFormat` — it is the SAME marker pr-review.yml's dispatch-recovery posts (PR-1), so the manual shim and the automated sweep share one dedup space. Comment body tags #1844 (instances accumulate as issue evidence), carries the head SHA, the recovery levers, and the dedup note. Body is EMITTED by the decision (`Decision.CommentBody`) so the format lives in tested Go, not bash.

### Auto-retrigger default — OPT-OUT (design call, ratified)

`watchdog:no-auto-retrigger` label withholds the dispatch (comment still posts — evidence stands); default is dispatch ON. Rationale: the fault is silent and GitHub-side — nobody labels a PR in advance for a fault they cannot see, and an opt-in default would have caught ZERO of the four documented instances; a ci.yml dispatch is side-effect-free (build+test, no pushes); amplification is double-capped: comment dedup per SHA, and the predicate itself (a dispatched run creates check-runs on the SAME head SHA → next sweep sees runs → fault clears → no re-dispatch loop; pinned by `TestEvaluate_DedupMarkerSuppressesComment` keeping dispatch armed only WHILE the fault persists).

## Structure

- `pkg/runwatchdog/predicate.go` — PRState/Decision/Evaluate/NaiveEvaluate/FaultMarker/FaultCommentBody/HasSkipMarker. Pure data in, decision out; zero I/O.
- `pkg/runwatchdog/predicate_test.go` — the red-first suite + control.
- `cmd/runwatchdog/main.go` — thin caller: one PRState JSON on stdin (`min_age_minutes` ergonomic field) → one Decision JSON on stdout.
- `Makefile` `runwatchdog-build` (repolint-build conventions).
- `.github/workflows/run-watchdog.yml` — cron `*/10` + workflow_dispatch, `run-watchdog` concurrency newest-wins, permissions contents:read/pull-requests:write/actions:write (workflow_dispatch via GITHUB_TOKEN is a documented recursion exemption), per-PR failure-tolerant `watch_one` (gate_one principle: only enumeration failure fails the job).

## Live validation (dry-run — gather + decide only, no actions fired)

Against real repo data at 03:17Z, with the workflow's exact gh api + jq query shapes:

- **PR #1873 (probe/ci-event-check, head from 02:02:41Z): FAULT=true, comment+dispatch** — head 1h15m old, ZERO check-runs, no skip marker. This is the live still-suppressed probe branch from the issue — the predicate caught a real instance on its first live pass.
- PR #1872 (feat/board-review-settle): fault=false — head 1m44s old (under threshold; the suppression-window branch had just been pushed by its lane).
- PR #1874 (this lane's PR-1): fault=false — under threshold, and runs present.
- A bool-cast bug was caught by this dry-run before it shipped: `--argjson` feeding `no_auto_retrigger` needs a JSON boolean (tsv column now emits `true`/`false`), not 0/1.
- YAML parses; every `run:` block `bash -n` clean; `gofmt`/`go vet`/`go test ./pkg/runwatchdog/` green; `make repolint` clean.

## Residuals / follow-ups

- PR-scoped by design: branch-scoped suppression with NO open PR is out of scope (nothing to comment on; the manual `gh workflow run ci.yml --ref <branch>` lever covers it — now documented in PR-1's shim).
- Fork PRs: comment posts, ci.yml dispatch fails (fork-side ref) — warning, not error; the comment carries the manual lever.
- A head pushed <15m ago is unwatched until the next sweep past the threshold: detection latency 15–25 min from push (documented; the suppression windows run 40+ min).
- PR #1873's live fault was NOT acted on from this lane (sibling's probe PR; not merged yet) — reported to the orchestrator for coordination.

## Addendum (r2) — review findings: the inert sweep and the honest dispatch

1. **[Critical] Missing `checks:` grant made the sweep SILENTLY INERT.** The check-runs read (GET commits/{sha}/check-runs) needs the Checks permission under GITHUB_TOKEN; without it every call 403s, the per-PR guard skips every PR, and the job stays green-on-warnings — the exact silent-non-delivery class this workflow exists to fight. Fixed: `checks: read` granted (numbering-gate.yml grants checks: for the identical call); pinned by TestRunWatchdogPermissionsPinned (asserts all four grants AND forbids unnecessary write grants). My 03:17Z live dry-run used a privileged user token, so the 403 path was never exercised — lesson recorded.
2. **[High] "Side-effect-free dispatch" premise was FALSE.** ci.yml pushes sha-/ts- image sets on every non-pull_request event — a dispatched run PUBLISHES from the unmerged branch. Per the adjudication lean (adopted): ShouldDispatch now consults ExistingMarkerSHAs (exactly-once per head SHA, the same dedup register as the comment), and the rationale is corrected everywhere (workflow header, predicate doc, CHANGELOG, here) — the default stays ON with the honest cost stated: one image set per suppressed head, visible in the fault comment, removable per-PR via the label. Ordering window documented in Evaluate: comment-fail + dispatch-ok may re-dispatch once (bounded by predicate-clear); comment-ok + dispatch-fail withholds further dispatches (manual lever in the comment).
3. **[Low] Premise comment named the wrong universal producer.** ci.yml filters branches: [main]; the guarantee the predicate rests on is pr-review.yml's (every pull_request, opened/synchronize, no branch/paths filter). Corrected in predicate.go, workflow header, CHANGELOG.
4. **[Nit] Worklog said 14 tests; the suite was 13** (miscount). Current pkg/runwatchdog count after r2: 15 (renamed dedup test, +MarkerAndLabelBothSuppressDispatch, +WorkflowEmissionNormalize).
5. **Wiring contract pinned** (TestRunWatchdogWiringContract): the workflow's jq -n emission is extracted from the YAML, key-quoted, substituted exactly as bash would, and checked two ways — key-set equality against WorkflowEmission (the emission type moved from cmd into pkg/runwatchdog so the contract is importable; PRState.min_age is the documented in-process-only exemption, zero-safe via DefaultMinAge) and a round-trip of the rendered document through the REAL binary (go run ./cmd/runwatchdog) asserted equal to Evaluate's Decision. TestRunWatchdogNoSilentProbeSwallows pins the never-a-silent-skip rule (no `|| MARKERS_JSON='[]'`; --paginate mandatory) — the #1874 r1 dead-probe class.
6. **Marker extraction paginates** (>100-comment PRs hid recent markers on page 2+): per-page streaming + jq -R -s reassembly, LOUD on failure (the dedup register must not degrade to empty).

Validation: pkg/runwatchdog 15/15 green (gofmt/vet clean); tests/ghaworkflows green incl. the four new pins; YAML+bash -n clean; the corrected marker pipeline executed live (paginate → reassemble → state → binary Decision) plus synthetic multi-hit reassembly.
