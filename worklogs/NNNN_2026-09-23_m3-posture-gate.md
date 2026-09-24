# Worklog: M3 — the posture gate workflow (design 0061 §5)

**Date:** 2026-09-23 (r1 fixes: 2026-09-24)
**Session:** Design 0061 implementation, M3 lane (the gate itself): `.github/workflows/posture-gate.yml` + `local/posture_gate_workflow_test.go` structural pins + the README-LLM contribution rule (the M3 AC's fourth clause). Merge sequenced per the #1548 recorded order.
**Status:** r1 fixes pushed; awaiting re-review.

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

## Key Decisions

1. **Unconditional assertions.** The nightly's cancel-guard arming protects EVIDENCE lanes from unrelated row failures; here the install is the thing under test — a failed `helm --wait` already fails the job, and conditioning the assertions would only manufacture skip-paths around red gates.
2. **The stability window rather than zero-restarts-absolute**: a healthy cold install may legitimately restart the controller during first-boot arming waits (the 30s startup guard vs. router keypair availability); the invariant is NO NEW RESTARTS inside the window, which the crashloop class (sub-30s exit cycles) cannot satisfy.
3. **The provenance channel is the running binary's own startup line** (`pkg/version.CommitSHA`, the same ldflags channel the release stamps) — not a registry label lookup through the node's containerd, which would re-derive the same attestation with more moving parts on the kind path.
4. **Router from tree, local ref** (the drill-shape precedent): deterministic on PR runs, no external image dependency, and the router binary is this tree's code — the gate exercising it is coverage, not drift.
5. **`mcp.enabled=false` is environmental, not posture**: the mcp image does not exist (issue #28); a cold install would ImagePullBackOff on infrastructure that cannot ship. Recorded in the install comment and in pin (c)'s allowed list.

## Blockers

**The gate is RED on current main — by design and by an open defect, and this is the true dependency record (correcting r0's "Blockers: None", which the review's live run refuted):**

1. **#1555 (OPEN)** — the shipped posture's namespace-scope + `watchNamespaces` unset sends the controller's workspace informer cluster-wide → `cannot list resource "workspaces" … at the cluster scope` (56 forbidden lines in the live run) → cache-sync exit → CrashLoop. Assertions 1/3 will stay red until #1555's fix lands. The red IS the gate working — it detected a real shipped-posture defect at first contact, retroactively validating design 0061 — but the gate cannot go green before it.
2. **M2 (#1557, open) and M4** — the #1548-recorded merge order is M1 → M2 → M4 → gate → e2e; the gate lands after them. (M1's armed line and #1552's RBAC fixes are already on main — confirmed by the live run: the armed-line grep matched the real emission path, and the r0 premise "red until M1 + #1552" was already satisfied.)

The merge call (ship the red gate as the detector it is, once #1555/M2/M4 resolve, vs. wait) is the orchestrator's.

## Tests Run

- `go test ./local/ -run TestPostureGate -count=1` — 6/6 PASS (r1 shape).
- Mutation checks (r1, mine): full llm-relay gutting / `--previous` removal / assertion-4 comparison gutting / `continue-on-error` — all caught.
- `bash -n` on every run block — clean (re-verified after the r1 edits).
- `go test ./local/ -count=1` — full package green. `go vet ./local/` clean; gofmt/goimports clean.
- Live-cluster execution: r0's review run (their evidence, cited above); the r1 stability-window mechanics are newly authored and NOT yet live-validated — first live contact rides the next review run or the gate's own first dispatch after merge.

## Next Steps

1. Re-review (r1 verdict pending).
2. The orchestrator sequences the merge per the #1548 order once #1555, M2, and M4 land; then the gate's first dispatched green run closes the loop.
3. Watch the stability window's first live contact (the 45s re-check + restart-diff mechanics) — if job pods' pod-set churn false-positives the restart snapshot, the snapshot scope narrows to the chart's Deployments' pods.

## Files Modified

- `.github/workflows/posture-gate.yml` (new)
- `local/posture_gate_workflow_test.go` (new)
- `README-LLM.md` (the contribution-rule subsection)
- `worklogs/NNNN_2026-09-23_m3-posture-gate.md` (this worklog)
