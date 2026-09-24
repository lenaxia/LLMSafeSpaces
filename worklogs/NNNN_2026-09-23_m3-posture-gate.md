# Worklog: M3 — the posture gate workflow (design 0061 §5)

**Date:** 2026-09-23 (r1–r4 fixes: 2026-09-24)
**Session:** Design 0061 implementation, M3 lane (the gate itself): `.github/workflows/posture-gate.yml` + `local/posture_gate_workflow_test.go` structural pins + the README-LLM contribution rule (the M3 AC's fourth clause). Merge sequenced per the #1548 recorded order.
**Status:** r4 fixes pushed; awaiting re-review.

---

## Objective

One CI job that cold-installs the chart under its OWN mandated default posture on kind and asserts the posture is livable: the four §5 assertions in order (all-Ready, the armed line, zero forbidden, the commit-stamp envelope), on every PR touching the posture surface (§11 ruling-3 breadth: `helm/**` + `controller/**` + `api/**`).

## Work Completed

- **`.github/workflows/posture-gate.yml`** — the e2e-nightly bootstrap reused verbatim (kind v0.32.0, helm, the `lss-e2e-registry` digest-pin registry, the 2-node `kind-cluster-nightly.yaml` topology, the stamped image-build chain, cert-manager, the credentials Secret, test Postgres/Redis), plus the router image built from tree under a local ref (the drill-shape precedent — kind cannot pull the ghcr default; the override is environmental image plumbing, not posture).
- **The install** carries ONLY environmental overrides (image repos/tags/pullPolicy, delivery pins, `mcp.enabled=false` (issue #28 — no image exists), test DB/Redis, logging verbosity). Every posture lever — `rbac.scope`, `relayOnlyKeyDelivery.enabled`, `agentdSidecar.enabled`, `allowRelayRouterEgress` — reaches helm UNTOUCHED at its shipped default. If the shipped defaults cannot go all-Ready, the gate is red. That is the point.
- **The four assertions, in order, as separate unconditional steps** (r1: each hardened — see the round record).
- **`local/posture_gate_workflow_test.go`** — six structural pins, mutation-checked (r0 by the reviewer, r1 by me — see the round record).
- **README-LLM** — the contribution rule subsection (the M3 AC's "contribution rule lands in README-LLM" clause): a PR flipping any multi-component default must include posture-gate evidence covering the new posture.

## r1 round record (the review EXECUTED the gate on a live kind cluster)

The review's live run validated the bootstrap + install wiring end-to-end and the mechanics of assertions 2/3/4 against real controller logs — and caught real defects, all fixed this round:

1. **Assertion 1 used nonexistent kubectl syntax** (`rollout status deployment --all` → `error: unknown flag: --all`, verified live twice). Fixed to the verified idiom (`kubectl wait --for=condition=available deployment --all -n <ns>`), and pin (b) now enshrines the VALID literal.
2. **`helm --wait` returned rc=0 mid-crashloop** (a transient Available window; `RESTARTS 3 (36s ago)` observed with install success). Fixed: assertion 1 gained a stability window — Available re-asserted after a 45s settle, and restart snapshots before/after must be IDENTICAL (a crashlooping pod cannot stay quiet 45s; a healthy cold install that restarted during first-boot arming records no NEW restarts). The install step comment now states that `--wait`'s rc is not the verdict; the assertions carry it.
3. **Pin gaps (four mutations the r0 pins did not catch — reviewer-verified):** (a) assertion 4's comparison could be gutted while the FAIL-echo retained the literal → pin (d) now requires the comparison shape `!= '${{ github.sha }}'`; (b) the llm-relay halves of assertions 1/3 could be deleted → their literals now name BOTH namespaces; (c) silent-disarm mutations (job-level `if:`, `continue-on-error`, `pull_request.types`/`branches` filters) → pin (f) bans continue-on-error on every step and `if:` on the job, pin (a) requires the pull_request block to carry exactly the `paths` key; (d) `2>/dev/null` swallowed per-pod log-fetch failures and `--previous` was absent → a fetch failure is now itself a red verdict ("a pod whose logs cannot be read cannot be cleared") and prior crashed containers are grepped too.
4. **The honest dependency record** (the "Blockers: None" correction — see Blockers below).

**r1 mutation self-checks (my own, post-fix):** deleting a single llm-relay wait line from assertion 1 leaves the pin green — because the stability re-check still carries the both-namespace scope (verified: fully gutting BOTH llm-relay waits fails pin (b)); removing `--previous` fails; gutting the assertion-4 comparison fails; adding `continue-on-error: true` fails.

## r2 round record (review validated the design sound; five findings, all fixed)

r2 confirmed every r1 fix live-verified (the wait idiom against kubectl's own docs, the stability window against the chart's hook-delete policies — no legitimate pod-churn source false-positives the restart snapshot, the render under the gate's exact flags, 7/7 mutation catches). Findings and fixes:

1. **Pin (c) was bypassable (the r0 skeptical-reviewer's values-file note — my r1 miss):** a `--values posture-override.yaml` (or `--set-json`) smuggled whole posture overrides past the `--set` ban list, and `--set controller.watchNamespaces=<ns>` would silence the exact #1555 crashloop class the gate exists to catch. Fixed: `--values`, ` -f `, `--set-json`, and `watchNamespaces=` all banned on the install run block.
2. **`set -euo pipefail` was unpinned:** deleting it from an assertion block neuters every check with zero literal drift (failed kubectl waits continue; a stuck-not-ready deployment prints OK). Fixed: pin (f) requires it as the FIRST statement of each assertion block.
3. **Assert 3's pod-LIST fetch was invisible to `set -e`** (process-substitution feed): a namespace whose pods cannot be enumerated was silently cleared. Fixed: `PODS=$(kubectl …)` under `set -e`, loop fed via herestring; the failure-checked-fetch shape is pinned.
4. **Assert 4's `-z` diagnostic was dead code** (pipefail killed the step before the branch). Fixed: failure-checked log fetch to a file, extraction with `|| true` so the unstamped-artifact diagnostic actually fires. Assert 2 gained the same failure-checked fetch pattern for consistency.
5. **The dependency record mislabeled M2/M4** (r1's own correction was itself wrong): #1557 is the **M4** PR; **no M2 PR exists yet** (open-PR scan: M1 #1553, M3 #1556, M4 #1557). Corrected here and in the PR body.
6. (Minor) README-LLM.md:300's structure block said `charts/` — corrected to `helm/` while the file was open (the zero-pre-existing-errors rule).

## r3 round record (three one-liners on a validated foundation)

r2's verdict: design sound, every mechanical fix since r0 verified, 5/5 new mutation classes caught — remaining: (1) the PR body still carried the M2/M4 mislabel while this worklog claimed the correction had landed there (my r2 `sed` on the body had silently missed the bold-marked line — the replace-verification lesson, again; fixed with an asserted Python replace, grep-verified in the live body); (2) the ` -f ` ban missed the pflag `-f=<file>` form — `-f=` banned too; (3) the `set -euo pipefail` pin checked presence, not persistence — a later `set +e` countermanded it under the pinned prefix — `set +e` and `set +o pipefail` are now banned in assertion blocks. Plus the two offered polish items: the README-LLM tree comment-column alignment, and `permissions: contents: read` on the workflow (the r3 security hardening note — the job needs nothing more of the token).

## r4 round record (completeness closes + the live-scan discipline)

r3's closes were incomplete within their own classes (both mutation-demonstrated by the review): the exact-spelling `set +e` ban missed `set +o errexit`/`set +o nounset`/double-space variants — now the whole countermand family is banned against WHITESPACE-NORMALIZED run text; the literal ` -f `/`-f=` bans missed the tab-delimited `-f<TAB>file` form (tab is IFS whitespace — a live smuggle) — replaced by the regex `(^|\s)-f[\s=]` over the install run block. Plus r4's optional finding adopted: Assert 1's `kubectl wait` lines must be bare (`|| true` appends now red). And the record-accuracy lesson landed for good: r3's push asserted `#1555 (OPEN)` and `M4 (#1557, open)` AFTER both had flipped (merged 01:50, closed-unmerged 02:05 vs push 02:08) — the r4 refresh above is a LIVE scan at write time, and the worklog now states scan time.

## Key Decisions

1. **Unconditional assertions.** The nightly's cancel-guard arming protects EVIDENCE lanes from unrelated row failures; here the install is the thing under test — a failed `helm --wait` already fails the job, and conditioning the assertions would only manufacture skip-paths around red gates.
2. **The stability window rather than zero-restarts-absolute**: a healthy cold install may legitimately restart the controller during first-boot arming waits (the 30s startup guard vs. router keypair availability); the invariant is NO NEW RESTARTS inside the window, which the crashloop class (sub-30s exit cycles) cannot satisfy.
3. **The provenance channel is the running binary's own startup line** (`pkg/version.CommitSHA`, the same ldflags channel the release stamps) — not a registry label lookup through the node's containerd, which would re-derive the same attestation with more moving parts on the kind path.
4. **Router from tree, local ref** (the drill-shape precedent): deterministic on PR runs, no external image dependency, and the router binary is this tree's code — the gate exercising it is coverage, not drift.
5. **`mcp.enabled=false` is environmental, not posture**: the mcp image does not exist (issue #28); a cold install would ImagePullBackOff on infrastructure that cannot ship. Recorded in the install comment and in pin (c)'s allowed list.

## Blockers

**The gate is RED on current main — by design and by an open defect, and this is the live-scanned dependency record (r4 refresh, 2026-09-24 ~02:45Z — supersedes r1–r3's versions, two of which asserted states that had flipped before their pushes):**

1. **The shipped-posture cache-scoping defect** — namespace scope + `watchNamespaces` unset → cluster-wide informer → forbidden → CrashLoop (56 denial lines in the r0 live run). **#1555 was closed UNMERGED at 02:05:56Z; the fix now rides #1558 (OPEN, "namespace-scope cache-scoping derivation")**. Assertions 1/3 stay red until #1558 (or successor) lands. The red IS the gate working — first contact detected a real shipped-posture defect, retroactively validating design 0061.
2. **The #1548 recorded order**: M1 → M2 → M4 → gate → e2e. Live scan at r4: **M4 (#1557) MERGED 01:50Z** (the recorded order has already diverged — the orchestrator's call); **M1 (#1553) still OPEN**; **M2 has no PR yet**. The gate's assertion 2 is satisfiable without M1 (the armed line already emits from `controller.go:164` on main; M1 adds the exit-85 crash-loudness), but the recorded order names M1 and M2 before the gate.

The merge call (ship the red gate as the detector it is vs. wait for #1558/M1/M2) is the orchestrator's.

The merge call (ship the red gate as the detector it is, once #1555/M2/M4 resolve, vs. wait) is the orchestrator's.

## Tests Run

- `go test ./local/ -run TestPostureGate -count=1` — 6/6 PASS (r4 shape).
- Mutation checks across rounds (r1 mine; r2–r4 the reviews', each re-verified by me after closing): llm-relay gutting / `--previous` removal / assertion-4 comparison gutting / `continue-on-error` / job `if:` / `types:` filter / posture `--set` injection / `--values` / `--set-json` / `watchNamespaces=` / `set -euo pipefail` deletion / process-substitution reversion / `-f=` / `set +e` / `set +o errexit` / tab-form `-f` / `|| true` on a wait line — all caught.
- `bash -n` on every run block — clean (re-verified after each round's edits).
- `go test ./local/ -count=1` — full package green. `go vet ./local/` clean; gofmt/goimports clean.
- Live-cluster execution: r0's review run (their evidence, cited above); the r1 stability-window mechanics, r2's fetch shapes, r3's permissions block, and r4's pin changes are newly authored and NOT yet live-validated — first live contact rides the next review run or the gate's own first dispatch after merge.

## Next Steps

1. Re-review (r4 verdict pending).
2. The orchestrator sequences the merge — live scan at r4: #1558 (the defect fix) and M1 (#1553) open, M2 unopened, M4 merged; the #1548 recorded order names M1 and M2 before the gate. Then the gate's first dispatched green run closes the loop.
3. Watch the stability window's first live contact (the 45s re-check + restart-diff mechanics) — if legitimate pod-set churn ever false-positives the restart snapshot (r2 found none: the hook Jobs delete on success), the snapshot scope narrows to the chart's Deployments' pods.

## Files Modified

- `.github/workflows/posture-gate.yml` (new)
- `local/posture_gate_workflow_test.go` (new)
- `README-LLM.md` (the contribution-rule subsection; `charts/` → `helm/` structure correction)
- `worklogs/NNNN_2026-09-23_m3-posture-gate.md` (this worklog)
