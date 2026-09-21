# Worklog: suspend drops the busy gate — bounded graceful termination (#1507)

**Date:** 2026-09-21
**Session:** The owner's incident (workspace d8bed486, this very pod): a wedged opencode (69h CPU, session POSTs timing out, sessions re-marked busy by the sessionstate flap loop every ~15s) kept the #761 drain deferring FOREVER — busySessions flapped 4↔5 with progressAge pinned at 0s (the flap resets lastProgressAt, so the stall window never ages), Suspending hung 1h+, manual kubectl delete was the only fix. Owner ruling: drop the busy check; suspend = bounded graceful opencode termination, then proceed regardless.
**Status:** Complete

---

## Objective

The suspend path never consults busy sessions; the pod's termination grace period IS the bounded graceful window; a wedged workspace suspends in bounded controller passes.

---

## Work Completed

1. **The gate removal** (`phase_suspend.go` handleSuspending): the `drainBeforePodDeletion(drainReasonSuspend)` call is DELETED. The pod deletion proceeds immediately; kubelet SIGTERMs agentd whose serial shutdown budget (HTTP drain 25s → bg wait 5s → opencode child SIGTERM→SIGKILL 5s = 35s) is the graceful termination, hard-cut at grace expiry. The flap-loop physics documented in the block comment: progressAge-0s flapping means the stall window NEVER fires — the incident explains why no stall-bound tuning can fix this class.
2. **The bound is explicit + tunable** (`pod_builder.go`, reconciler field, `--workspace-termination-grace-seconds` flag, Helm `controller.workspaceTerminationGraceSeconds`): default 40s (the 35s budget + margin); startup guard rejects values <36 (a grace below the budget short-circuits agentd's drain mid-flight — the pre-#761 5s bug class).
3. **Scope**: the OTHER drain sites (restart-generation recycle, arch drift, password-secret self-heal) KEEP their drains — #1507's AC scope is the suspend path; the recycles are a different decision (agent stays alive; mid-turn recycle more disruptive). Pinned unchanged.
4. **The drain machinery tests re-targeted** (session_drain_test.go): the machinery-level tests (busy-defers, busy-then-idle, progress-extends-window, stall-forces, fail-opens, churn, recreation-reset) now ride the restart-generation path (`makeRestartGenDrainWorkspace` helper) — same machinery, live path; terminal-phase assertions updated (Creating not Suspended), metric labels updated to the restart-gen reason. `TestDrain_SuspendRequestFlowDefers` rewritten as `TestDrain_SuspendRequestFlowBounded` — the Spec.Suspend handshake end-to-end with busy sessions: no deferral, no statusz consult, pod deleted, request acknowledged.
5. **The incident pins** (`phase_suspend_1507_test.go`): the flap-loop repro (busy→immediate suspend, PVC retained), unreachable-agent (statusz never dialed), grace-rides-the-pod (default 40, flag override), restart-gen drain unchanged (scope guard).
6. **Helm chart pin** (`TestControllerArgs_TerminationGraceFlag`): no flag by default; explicit value propagates.

## Key Decisions

1. **The grace already existed**: #761 raised terminationGracePeriodSeconds to 40s for exactly this purpose (the pre-#761 5s grace was cutting in-flight turns). The pod's grace IS the "graceful opencode termination" the issue asks for — the only missing piece was the busy-gate making the controller WAIT INDEFINITELY before issuing the deletion. Removal + explicit flag = the issue's proposed behavior, no new termination machinery needed.
2. **Why the stall window could never fire** (the incident's design-grade evidence): the flap loop (sessionstate clears stranded busy → the wedged opencode's tracker immediately re-marks busy) makes `snap.differsFrom(state.lastSnapshot)` true every poll — `lastProgressAt` resets each time — so `progressAge` stays ~0s and the 60-minute stall bound (drainStallBound = 60m) is unreachable. Any bound keyed on "progress" is defeated by a wedge that oscillates.
3. **Waiting provided no value**: the PVC-retention design means busy sessions are terminated either way; the only question is grace (which kubelet provides). Cited in the block comment.
4. **Metric/event labels for the retired path**: drainReasonSuspend no longer fires from production code — the constant was DELETED outright in the r1 round (no dead vocabulary kept; grep-clean repo-wide).

## Assumptions → validation record (Rule 7)

- "The flap resets progress tracking" → verified by reading the incident log (progressAge 0s across 1h of polls) against the drain's progress logic (`differsFrom → lastProgressAt = now`).
- "agentd's SIGTERM budget is 35s serial" → pod_builder.go's #761 comment documents the budget (HTTP drain 25s + bg 5s + child 5s); the guard threshold 36 > 35.
- "The flag renders only when set" → chart pin (both directions).

## Blockers

None.

## Tests Run

- `go test ./controller/internal/workspace/` — full package green (the re-targeted machinery suite + the 4 new incident pins + the rewritten suspend-flow pin).
- `go test ./helm/ -run TestControllerArgs_TerminationGraceFlag` — green.
- `go build ./...` — green.

## Next Steps

1. #1506 adjudication (this PR's semantics supersede its suspend-force layer): the annotation machinery rebases onto these semantics — the vestigial "suspend" marker is honored-and-cleared in handleSuspending for one release (already handled in this diff when #1506's branch merges; on THIS branch main has no force code — the rebase direction is #1506 onto #1507).
2. The sessionstate flap itself (clear/re-mark disagreement under a wedged opencode) deserves its own look — symptomatic noise now, but the detector/tracker disagreement is real. Out of scope here; noted in the issue's spirit.

## Files Modified

- `controller/internal/workspace/phase_suspend.go` (the gate removal + incident rationale)
- `controller/internal/workspace/pod_builder.go` (tunable grace)
- `controller/internal/workspace/reconciler.go` (the field)
- `controller/internal/controller/controller.go`, `controller/main.go` (flag + guard + wiring)
- `helm/values.yaml`, `helm/templates/controller-deployment.yaml` (+ chart pin in `helm/chart_test.go`)
- `controller/internal/workspace/phase_suspend_1507_test.go` (new — the incident pins)
- `controller/internal/workspace/session_drain_test.go` (machinery re-target + the rewritten flow pin)
- `worklogs/NNNN_2026-09-21_suspend-bounded-graceful.md` (this file)

## r1 review — findings closed

1. Guard contract falsified for negatives (accepted -5) → the condition now rejects every non-zero value below the floor; table-driven pin added (controller/main_grace_test.go: 0/36/120 accept; 35/1/-5 reject).
2. Dead drainReasonSuspend constant removed (the machinery vocabulary shrinks to the live reasons).
3. The unreachable-test comment/code mismatch → comment rewritten (live stub wired, zero scrapes asserted); a GENUINELY-unreachable pin added (Port 1, no listener — the deeper incident state).

## r2 review — findings closed

1. Busy-suspend kind e2e: NOT delivered — closure claim adjusted instead (Refs, not Fixes; AC-level scoping documented in the PR body). The fake-client pins cannot catch real-cluster wiring divergence; the kind row is the post-merge nightly follow-up, and the incident workspace is the live target. Also avoids colliding with the local/ + e2e-nightly.yml lane (#1456's ownership).
2. Unreachable-test misnomer → renamed LiveAgentNeverConsultedOnSuspend (fixture deliberately reachable — the strongest never-dials proof; dead-agent pinned separately).
3. Stale "phase stays Suspending" comment → corrected to Active (restart-gen retarget).

## r3 review — findings closed

1. Drive-by scope creep REVERTED (partially at r3 — CORRECTION: the r3 commit itself still left TWO flag texts divergent, the arm64 sha help and the refresh-interval help; the "net diff zero" line as originally written here was FALSE at r3): the lint-hoist commits' accidental rewrites of --free-models-api-url's default (""→literal URL) and the --agentd-image/--agentd-binary-sha256-* help texts were restored across r3+r4; the hoist survives as pure structure (registerFreeModelsFlags). Net diff from main on those flags: zero AS OF the r4 commit (d656e45f) — verified byte-exact by the r5 review.
2. buildPod defense-in-depth: sub-36 programmatic values clamp to the 40s default (structural — the invariant no longer lives only in the main() guard); pinned.
3. Kind e2e: escalated to the orchestrator (the review framework's REQUEST CHANGES held every round — no reviewer accepted a known-missing test level). Ruling (a): standalone script in THIS PR; landed in the r5 commit.

## r4 — flag texts byte-exact; r5 — the kind row landed in-PR (ruling (a))

- r4: the two remaining flag texts (arm64 sha help, refresh-interval help) restored to main's exact bytes — the hoist diff is provably pure structure.
- r5 (orchestrator ruling (a)): local/issue-1507-bounded-suspend-e2e.sh + local/issue_1507_script_test.go pins. R1 = THE incident replay at cluster scale: seed → wait Active → in-flight turn (bash sleep 300, running-tool-part asserted BEFORE the suspend call — a LIVE busy turn is the stronger case: pre-#1507 the drain would legitimately wait for it, exactly what the incident faked forever) → API suspend → Suspended within 120s budget (grace 40s + reconcile + margin; the incident was 3600+) → pod gone, PVC retained. R2 = AC1's log assertion: no suspend drain-defer line in the controller log (discoverable-pod conditional; the phase/timing verdicts carry the row without log access). Env/budget overrides for execution smokes. Standalone by design — NO e2e-nightly.yml touch (the #1456 lane boundary); the nightly wiring is the documented follow-up on the next #1456-lane touch.
- Pins: bash syntax, busy-before-suspend ordering, the four outcome assertions (Suspended/pod-gone/PVC-retained/no-defer-line), unconditional WS_BASE isolation.

## r5 corrections (the review's three falsehood findings + the AC4 scoping)

- The kind e2e WAS landed in the same round this review landed (ee95c4F4 — the review ran against the r4 head d656e45f; the review's own "no kind-level e2e" is stale against the pushed head).
- AC4 honestly re-scoped in the PR body: the agentd shutdown-budget path and the grace-expiry hard cut are NOT exercised anywhere in this repo (kubelet-side / agentd-suite rows — nightly follow-ups); what IS delivered is enumerated precisely.
- Style minors: the flap-repro test's comment reworded to the static-busy truth (the path never reads statusz — the fixture models the incident's observable state, not the oscillation); pod_builder's "flag-parse time" → "controller startup".

## r6 — the runnable-row pass

- SCRIPT_DIR bug fixed (the misplaced-paren subshell idiom killed the script at source — now the standard cd&&pwd form; reviewer-verified empty).
- Pod-gone verdict BOUNDED: the phase flips at deletion-issue but the pod object lingers in Terminating while kubelet runs the 40s grace — the wait is now budget+120s, matching the semantics under test (the r5 instant-gone check would have false-failed every honest run).
- R2 fail-closed: an undiscoverable controller pod is a FAIL, not a skip — the AC1 log assertion is a verdict, not decoration.
- The 10-minute figure corrected to 60 minutes everywhere (drainStallBound = 60m, session_drain.go:91) — worklog, PR body, and the in-code comment; the unreachability argument is number-independent but the record must match the code.

## r7 — the fail-open closes + the execution requirement

- Pod-gone predicate NotFound-aware: only an explicit NotFound counts as gone (query errors keep waiting and surface as the timeout FAIL with the last query error inline).
- R2 truly fail-closed: logs captured to a file first (fetch failure = FAIL with the error text), then the grep; no 2>/dev/null swallow anywhere in a verdict path.
- suspend_elapsed initialized at 0 (the R2 fetch's --since never reads an unset var if the phase wait itself timed out).
- Pins extended to the new verdict shapes (pod_gone/NotFound/log-fetch-failed markers).
- The execution requirement (one recorded harness run) is OUTSIDE this pod's reach — no kubectl here; the row runs on the pool/nightly harness. Flagged to the orchestrator for adjudication: either the orchestrator executes it (the nightly harness is theirs) or accepts the static-delivery + post-merge-first-run scoping. The PR body's "delivered" wording is corrected to "landed, awaiting its first harness execution".

## r8 — the predicate actually works now (and the r7 claims corrected)

- r7's pod_gone was DOUBLY broken (reviewer-verified empirically, both bugs): the 2>&1 sat INSIDE the command substitution so the NotFound error text made the empty-check unreachable, and pipefail made the confirmation clause return kc's exit 1 rather than grep's 0 — a universal false-fail. The r7 worklog line "only an explicit NotFound counts as gone" was FALSE as written (no case counted). Corrected: the pipefail-safe capture-or-true form; the r7 record stands corrected by this entry (append-only).
- NEW PIN: TestIssue1507Script_PodGonePredicateMockTable — executes the predicate's EXACT extracted source (not a copy) against a mock kc across the three cases (exists→false, NotFound→true, query-error→false), the bash-level harness the reviewer asked for; all three pass under the script's own set -euo pipefail.
- The recorded-harness-execution finding: the orchestrator disposition (ruling (c) with (b)'s scoping) is on the PR record — the execution lands as the #1456 worker's first wiring artifact post-merge.
