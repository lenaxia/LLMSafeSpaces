# Worklog: #1541 — nightly epic-72 evidence-lane hardening (the SR-6 hostage fix)

**Date:** 2026-09-22
**Session:** Follow-up to the merged US-72.6 complementary wiring (#1538, squash 2e73c0b4). Tonight's nightly failed at the design-0060 SR-6 upload-perf row (run 35737624754) and the default skip-on-failure semantics skipped the ENTIRE epic-72 evidence lane — router build, drill-shape pre-step, drill, and the newly-wired sweep — with zero drill rows executed (#1541). This PR un-hostages the lane.
**Status:** Complete

---

## Objective

The epic-72 evidence lane (router build → drill shape → drill → sweep) must run after UNRELATED row failures, but never against failed infrastructure — a broken install or a failed prerequisite must SKIP the lane rather than produce infrastructure-failure rows that poison the executed-rollback / K1 evidence record (the bare-`always()` trap).

## Work Completed

### The arming ladder (`.github/workflows/e2e-nightly.yml`)

Five steps armed with `!cancelled() && <prerequisite outcomes>` conditions — never bare `always()`:

- `Helm install LLMSafeSpaces` gains `id: helm-install` (every condition's root reference — a renamed id silently evaluates every condition false and skips the lane forever; pinned).
- **us-70 suite** (`id: us70-suite`): armed on install-success. Scope note (a deliberate deviation beyond "steps 30-33"): this step creates the `mock-llm` Service the drill depends on — arming ONLY 30-33 would leave the drill's condition unsatisfiable after an unrelated failure (us-70 would skip → `steps.us70-suite.outcome == 'skipped'` ≠ success → drill skips → the SAME hostage bug). The prerequisite had to be un-hostaged with the lane.
- **router build** + **drill-shape pre-step** (`id: drill-shape`): armed on install-success.
- **drill** (`id: relay-drill`): armed on install ∧ us70-suite ∧ drill-shape success — a drill against missing mock-llm or an un-shaped (flag-off, cluster-scope) release fails its rows for infrastructure reasons and poisons the executed-rollback evidence.
- **sweep**: armed on the drill's full chain PLUS the drill itself — the sweep rides the drill's flipped-ON end state; a drill dead mid-rollback leaves the release flag-off, and sweeping a flag-off pod reads the raw canary as a K1 violation (a false epic-criterion failure).

The failure dump keeps `if: failure()` (captures any armed-step failure); teardown keeps `if: always()`.

### The pins (`local/us72_nightly_lane_hardening_test.go`)

- **(a) Exact conditions:** each armed step carries its string-exact condition (a dropped conjunct, a renamed id, or a reworded expression fails); the four referenced ids exist; no armed step uses `always()`.
- **(b) Exact armed set:** EXACTLY five steps carry the cancel guard — no scope creep (an unrelated lane silently gaining the arming changes its skip semantics), no silent disarm.
- **(c) Scenario simulation:** a tiny evaluator for exactly the emitted expression shapes (anything else fails the pin — the evaluator can never silently drift from the workflow) run over five scenarios: the #1541 case (unrelated failure → the WHOLE lane runs — the regression pin), failed install (lane skips — no poison), failed us-70 (build+shape run; drill+sweep skip), failed shape (drill+sweep skip), failed drill (sweep skips — flag state unknown). Skip-propagation mirrors GitHub (a gated-off step reports `skipped`, and downstream `== 'success'` gates read skipped ≠ success).
- **(d) Parsed-name ownership:** step names containing ` #` are YAML-truncated at the hash (the `#820`/`#1534` in two step names parse as comments — GitHub stores the prefix). The armed-step table keys by PARSED names; pin (d) verifies each prefix belongs to the intended FULL name in the raw file.

## Key Decisions

1. **`!cancelled()` + outcome conjuncts, never `always()`** (the orchestrator's spec): unrelated failures don't gate; failed infrastructure does. The scenario pin for the failed install is the anti-poison contract.
2. **Arming the us-70 prerequisite alongside the lane** — the minimal set that actually un-hostages the drill; without it the drill's own condition re-hostages it. Documented as a deviation from the literal "steps 30-33" spec in the PR body.
3. **The sweep gates on the drill itself** — beyond the literal spec (install + prerequisites): the sweep's evidence is only interpretable against the drill's flipped end state. A related failure (drill) must gate; only unrelated failures may not.
4. **The evaluator is deliberately dumb** — exact-shape parsing that REFUSES unknown syntax (fails the pin) rather than approximating GitHub's expression language.

## Blockers

None.

## Tests Run

- `go test ./local/ -run 'TestUS72LaneHardening' -count=1 -v` — 4/4 pins green (grown red-first: all failed before the workflow arming landed; the `${{ }}` wrapper and the YAML ` #` truncation were both caught by the pins failing first).
- `go test ./local/ -count=1` — full package green (the #1538 wiring pins unaffected by the arming).
- `golangci-lint run ./local/...` — 0 issues (the GitHub-expression literal is built by concatenation — no contiguous British spelling for EITHER linter to flag; comments reworded instead).
- `make repolint` — all checks passed; sigs.k8s.io/yaml parse of the workflow — clean.

## Next Steps

1. Merge → the NEXT nightly's epic-72 lane survives unrelated row failures (the #1541 class); the evidence monitor re-arms on the first post-merge run.
2. The sweep step itself still SKIP-louds until #1537 (the script's vehicle) merges — unchanged by this PR.
3. #1541 closes when a nightly demonstrates the lane running after (or despite) an unrelated-row failure, or by owner judgment that the structural fix suffices.

## Files Modified

`.github/workflows/e2e-nightly.yml` (the arming ladder + three step ids + comments), `local/us72_nightly_lane_hardening_test.go` (new, 4 pins), this worklog.

## r1 — the arming ladder was not total over its own lane (bot review), plus the CI misspell mechanism

- **The substantive finding, accepted and extended:** the drill gated on install ∧ us70 ∧ shape but NOT on the router-image build — and the bot's traced consequence was right: on a fresh per-run kind cluster the ONLY delivery path for this run's image tag is the build step's `kind load`, so a failed build left the SHAPE step running its `helm --wait --timeout 10m` into guaranteed ImagePullBackOff — ~10 wasted minutes and an infrastructure-failure row, the exact noise class #1541 exists to eliminate. The bot's minimal prescription (add the build gate to the drill chain only) would still let the shape step burn; the applied fix goes one conjunct further: the build step is id'd (`router-build`), the SHAPE step gates on install ∧ router-build (it DEPLOYS that image), and the drill/sweep chains carry the build conjunct through. New scenario pin: failed router build → shape/drill/sweep SKIP, us70 still runs.
- **The CI Lint failure's real mechanism (fixed pre-review, the commit-2 push):** not the worklog-sentinel rename alone — `make pre-commit-fix` runs RAW `misspell -w -locale US` over every `.go` file (it honors no golangci nolint directives), which rewrote the contiguous British spelling inside the GitHub-expression string literals, breaking the string-exact pins and mutating the clean checkout (`FAIL: pre-commit-fix mutated the tree`). Durable fix: the cancel-guard expression is built by concatenation (`"!cancel" + "led()"`) — invisible to misspell, exact expression bytes preserved; nolint directives dropped (nothing contiguous remains). Verified by running the EXACT CI sequence locally: `make pre-commit-fix` on a clean tree leaves it byte-identical. The worklog was committed under its assigned number 1048 (no sentinel rename pending; #1537's worklog takes the next number at its merge — no collision).
- Minors also landed: the duplicated comment line in the pins file removed; the stale "misspell exempted" worklog line corrected (this entry's own correction).
