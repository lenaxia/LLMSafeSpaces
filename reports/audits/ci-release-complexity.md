# CI matrix + release train — complexity audit

**Task:** tsk-hazel-mint-waits (AUDIT: CI matrix + release train — check value vs cost, deploy friction)
**Date:** 2026-11-17. **Base:** origin/main `b1987029b`. **Method:** read-only — workflow source, git archaeology, live Actions timing/failure data (`gh api`), the 0.44.16/0.44.17 cut runbooks banked in this workspace. **No code changes** (this document is the only artifact).

---

## 1. Executive summary

- A typical code PR lights **~29 jobs in ci.yml + 4–8 jobs in sibling workflows** (43 check-runs observed on PR #2090) at a **measured ~150 runner-minutes per push** (run 44188388905, all timings below). Docker builds are **49% of that spend** and produce artifacts that are **never pushed on PRs** (`push: false`).
- The matrix is *not* uniformly high-value: several jobs are proven incident-catchers (repolint, test, envtest, migration-safety, origin-plugin-pin), one is a **functional duplicate** (e2e-pr.yml re-runs ci.yml's entire frontend step set), one is **pure information that never blocks** (coverage-delta, 11 min), and one is **non-blocking by flag** (sdk-canary, `continue-on-error: true` — zero gate value today, plus a 3/3 flake signature and 260s of literal `sleep 65` steps).
- The release train's cut mechanics are already half-automated (`make release-tag`) but the **cut commit (CHANGELOG heading + Chart.yaml bumps) and the cross-repo gitops bump (ref.tag + 4 semver tags + 3 digest pins hand-copied from job logs) are manual**, documented as exact line edits in runbooks. The **CRD-never-upgrades trap bit twice** (#2088 symptom, #2092 class closure — *in flight, not yet merged*): helm never upgrades `crds/`, and the Flux CRD path was absent from the bump runbook entirely.
- The AI review lane is process-critical but carries the **#80-class fragility**: a strict machine-parsed text contract (`REVIEW-LEDGER v1`) over free-form LLM output, where a one-character formatting drift (`## REVIEW-LEDGER v1`) produced 8/8 rejections on PR #2077.

---

## 2. Per-PR check inventory and costed matrix

**Measured timing source:** Actions run `44188388905` (PR #2083 final head, 2026-11-17, green; `gh api .../jobs` durations). Sibling-workflow costs from their own recent runs. "Catches" = documented incident/regression archaeology (commit messages, worklogs, workflow comments).

### 2.1 ci.yml (pull_request)

| # | Job | Measured cost | What it has actually caught (evidence) | Verdict |
|---|-----|--------------|------------------------------------------|---------|
| 1 | `changes` (docs-only detect) | 16 s | Enables the docs-only fast path; fails safe (detection failure → full suite) | **KEEP** |
| 2 | `lint` (repolint, fmt, imports, vet, context.TODO, golangci, helm-render, smoke targets) | 426 s | repolint: duplicate migrations + worklog collisions (0098; #1823/#1840 class); opencode/pi fixture-freshness gates (#942/#739 class); pin drift (US-68.2, twice) | **KEEP** — but the golangci pin has drifted between ci.yml/release.yml and stalled a release train (§4.1); single-source the pin |
| 3 | `test` (-short, race, coverage, 50% floor) | 934 s | 2026-11-17 alone: `TestTrustedAppBotsInputMatchesCanonicalList` (#2083 lane, fixed in 4404a71a5) and `TestDocs_MCPTools_Parity` (#2084 lane) — both real catches | **KEEP** (core gate) |
| 4 | `coverage-delta` | 647 s | Nothing — the job posts an informational PR comment only; "nothing `needs` this job; the merge gate is the 50% floor" (its own header). Degrades on base flake by design | **DEMOTE** — biggest zero-gate-value line item; see Top-5 #4 |
| 5 | `test-full` (full suite, race) | 856 s | Real marginal surface: 43 `testing.Short()` skip-guards in 33 files (16 in `cmd/workspace-agentd`, 9 in `local/`) that only run here. NOTE: its header comment ("today the codebase doesn't actually have any -short-skipped tests") is **stale** — finding F9 | **KEEP**, re-scope later (Top-5 alternate); fix the stale comment |
| 6 | `frontend-test` (tsc+vitest+playwright) | 583 s | The #1629/#2013-class frontend regressions; TS2554 main-red repair (#2089) surfaced here | **KEEP** |
| 7 | `abi-schema` (buf lint, freeze, stub freshness, contract tests) | 261 s | Stale-codegen catches; the breaking-change gate is armed via `abi/FROZEN` at S2 (US-69.1) — pre-freeze it proves the gates on fixtures | **KEEP** (load-bearing for the ABI freeze) |
| 8 | `origin-plugin-pin` | 291 s | Three documented classes: #1465 (hook surface), #1722 (abort busy-settle), #1530 (arg-mutation falsification) | **KEEP** |
| 9 | `sdk-contract` (Go/TS/Py/Java + hurl/Prism) | 87 s | #1044 (hurl contract, blocking); cheap for four languages | **KEEP** |
| 10 | `sdk-canary` | 503 s (incl. **4 × `sleep 65` = 260 s** of rate-limit waits) | Currently `continue-on-error: true` → **blocks nothing**. Known flake: `S-ENV-VARS → 429 login failed` 3/3 on PR #2087 (runner-env drift tripping the login anti-bruteforce limiter, diagnosed in the .16 runbook) | **TUNE** — either make it a real gate or stop paying for it (Top-5 #5) |
| 11 | `prepare` | 3 s | Build metadata | **KEEP** |
| 12–29 | 9 builds × 2 arch (api, controller, relay-router, relay-proxy, runtime, agentd, opencode, pi, frontend) | **4 405 s total** (controller 522/449 s the longest; opencode 73/68 s the shortest) | PR-side value = Dockerfile-break detection only — **artifacts are never pushed on PRs**. Caught: the arm64 opencode build failure on 3286ffa03 (run 44180181647, a docs-only head — flake-class, QEMU). Post-merge, main push builds + pushes everything; release.yml independently rebuilds everything on tags | **CONDITIONALIZE** — 49% of runner spend for non-shipped artifacts (Top-5 #2) |
| — | 9 `merge-*` manifest jobs | skipped on PRs | — | fine as-is |

### 2.2 Sibling workflows (pull_request)

| Workflow | Trigger | Measured cost | Catches | Verdict |
|---|---|---|---|---|
| `pr-review.yml` (AI review) | every PR open/synchronize | 5–35 min (LLM lane) | process-critical (AI reviewer until APPROVED is the merge gate) | **KEEP** (§5 fragility noted) |
| `security-scan.yml` | every PR | ~1 min | gitleaks/govulncheck/trivy; **3/13 recent runs red** on picomatch CVE-2026-112366 — dev-only transitive dep, **no fix published**; blocked #2090's merge pending owner disposition | **KEEP** — but no-fix dev-dep CVEs need an explicit `.trivyignore` discipline (a064dcb0d pattern) or they redden every PR until upstream fixes |
| `e2e-pr.yml` `e2e-frontend` | frontend paths | ~10 min + a `changes` job | **Nothing marginal — functional duplicate of ci.yml `frontend-test`**: same step set (npm ci / vitest / tsc / playwright mocked-backend / report upload), different step order, on `ubuntu-latest` where ci.yml pins `ubuntu-24.04`; its second job (`e2e-fullstack`) is `if: false` dead code since issue #24 | **CUT** (Top-5 #1) |
| `posture-gate.yml` | `helm/**`, `controller/**`, `api/**` | ~20 min (kind cold-install + ~4 in-job image builds) | design 0061 M3 — young gate, no documented incident catch yet | **TUNE** — the path breadth makes most Go PRs pay 20 min; narrow paths or reuse `:dev` images |
| `envtest.yml` | CRD/webhook paths | ~5 min | #231 (CRD defaults), #317 (webhook objectSelector) | **KEEP** |
| `migration-safety.yml` | `api/migrations/**` | ~5 min | worklog 0099 (image_tag drift), 0094 (binding orphans) | **KEEP** |
| `numbering-gate.yml` | push to main only | cheap | #1838/#1823/#1840 between-pushes collision window | **KEEP** |

**Per-PR totals (typical Go PR touching `api/**`):** ci.yml 150 runner-min + posture-gate 20 + pr-review + security-scan ≈ **~170+ runner-min per push**; wall-clock ~30 min (critical path `test` 934 s → `build-controller` 522 s).

### 2.3 Flake/retry tax (last 48 h, measured)

- CI runs (last 30): **19 green / 4 failed / 4 cancelled** — ~27% non-success; the cancelled ones are mostly intended supersession.
- **#1844 event-delivery wedge**: ≥5 tree-identical `ci: retrigger` commits today alone (46189ad16, f6cfe5649, 717410924, 9215cbe82, b4757a9ce, efeeb37eb…). Each re-trigger re-runs the **full ~150-runner-min matrix on an unchanged tree** — the single largest avoidable cost in the system, and it compounds with the AI-review round (each synchronize restarts the lane).
- Canary `S-ENV-VARS → 429` 3/3 on #2087 (non-blocking noise, but it poisons signal — the runbook had to prove the tree byte-identical to exempt it).
- `Build Opencode (linux/arm64)` red on a docs-only head (3286ffa03, QEMU-class).
- Race-suite timeout flake history: `TestApplyBatch_ConcurrentAnchorsSerializeInPipelineOrder` (c680938a8); `Test1732`-class re-arm timing tests live in the same regime (state_reconciler_rearm_1732_test.go).
- Lint red on #2083 (real catch — trusted-app-bots list drift — but on a bot-authored bump PR whose review lane was skipped by label, so lint was the only net).

---

## 3. Release train

### 3.1 What the train is

Tag-triggered `release.yml` (~1 809 lines): verify-changelog gate → prepare → lint/test/test-full/frontend-test (self-contained copies of the CI jobs) → 9 components × 2 arch builds → merges (semver/latest tags) → cosign sign → Trivy scan → chart publish (gh-pages) → SBOM → relay binaries → GitHub Release (notes generated from CHANGELOG) → fail-soft SDK publishes + `sdk-outcome` surfacing (#1918 discipline). `make release-tag` pre-validates semver/CHANGELOG/main-sync/tag-novelty and pushes the tag.

### 3.2 Measured friction

| Event | Evidence | Cost |
|---|---|---|
| **v0.44.15: 4 tag pushes for 1 release** | failed runs 43983788986 (512 s), 43986233145 (265 s), 43987590002 (1 870 s); fix commits faf9cdb90, d89a10a6e, 8ed843500 | (a) golangci **pin drift** between ci.yml and release.yml (v2.15.0 errcheck flagged a deliberate discard mid-release); (b) staticcheck+errcheck union on the fix; (c) `merge-pi` emitted no semver tag → sign/scan/SBOM died `MANIFEST_UNKNOWN` (first pi release). ~45 min of dead release runs + 3 fix-and-retag cycles |
| v0.44.16/.17 cut mechanics | runbooks release-0.44.16/-0.44.17-runbook.md | green train ≈ 32 min (1 914 s) — fine. The friction is **around** it: manual CHANGELOG 2-line heading insert, Chart.yaml `version`+`appVersion` bumps, push, tag, watch; then the **gitops bump in a second repo** (§3.4) |
| SDK legs | #1918 | already fail-soft with distinct surfacing — good pattern, keep |

### 3.3 The CRD-never-upgrades trap (bit twice)

- **Mechanism:** helm never upgrades `crds/` on `helm upgrade` (install-only by design, helm/README.md documents the CRD apply as a manual step). A structural apiserver **prunes unknown spec fields and returns success** — so a released schema change silently does not reach the cluster.
- **Bite 1 (#2088/.15):** the bind-once lane's CRD field (`spec.harness`) shipped in .15; the cluster served the pre-.15 Workspace CRD; the backfill stamped-and-retried forever (`handled=true` on a pruned write) → refresh-compute dead AND start-path stalled — owner-reported outage, fixed under time pressure in .16 (dc5632044, ed484ea22, 51499164b).
- **Bite 2 / class closure (#2092, commit a64c8541b — on branch `fix/2092-crd-skew-convergence`, **NOT yet in origin/main** as of this audit):** adds the release-train CRD guard — `deploy/flux/crds` tracked mirror + repolint `FluxCRDDriftCheck` (born-red: a schema change must bump the CRD apply in the same PR) + `make flux-sync-crds` + release-bump-checklist §§1/2/5/6, plus skew alerting and a recovery runbook.
- **Audit note:** until #2092 merges, the trap is *open* — the audit's automation candidate (Top-5 #3) must sequence **after** that merge, consuming its guard rather than duplicating it.

### 3.4 Flux path coupling (gitops bump, repo `lenaxia/talos-ops-prod`)

The post-release bump is a hand-edited PR across divergent version schemes (docs/release-bump-checklist.md, born from #1237's three incidents incl. a **fleet-wide outage** when a CalVer base tag was set by rote to the platform semver):

1. `kubernetes/flux/repositories/git/llmsafespaces.yaml` — `ref.tag` (chart consumed **from git**, not the registry).
2. `helm-release.yaml` — 4 semver image tags (api/controller/relay-router/frontend) **hand-copied**.
3. 3 delivery-overlay digest pins (agentd/opencode/pi) — copied from the release run's `merge-*` "Print Helm values block" job logs, each with a provenance comment, each digest-GET verified.
4. `runtimeEnvironments.base` — CalVer, **never** touched by the train.
5. PR → AI reviewer → orchestrator merge (same process discipline as this repo).

Every digest/tag is already machine-printed by the release run; the human is a transcription layer between two repos. This is the automation seam.

---

## 4. The AI review-gate lane

- **Dogfooding shape:** `pr-review.yml` is a thin caller over the reusable `lenaxia/ai-workflows` workflow (SHA-pinned), verdict delivered as an official review event, commit-pinned verification (job fails unless an APPROVED/CHANGES_REQUESTED by `github-actions[bot]` exists on the triggering head SHA), per-PR concurrency with cancel, plus a `dispatch-recovery` lever and `run-watchdog.yml` sweep for the #1844 event-drop class.
- **Ledger contract:** every verdict must embed a `REVIEW-LEDGER v1` block (round/head/lane/verdict/coverage/finding/waive lines) parsed by `scripts/review-gate/`; the validator maps class→merge-gating per lane, computes counts, and rejects `approve` with open gating findings, invented classes, or unparseable ledgers — rejection reasons feed retries.
- **#80-class fragility:** the contract is **exact-format text emitted by an LLM**. On PR #2077 the model wrote `## REVIEW-LEDGER v1` (heading-prefixed marker) — 8 rejections in a row (issue #80). The validator now tolerates the heading form as a fallback, but the class is structural: any format drift between what the model emits and what the parser accepts converts verdicts into burn (LLM rounds + CI re-dispatch). Mitigations already landed: prompt carries the literal contract + the failure story; round budgets (post-round-3 demand caps) ended the 39/20/19/8-round non-convergence; the CI-watching ban recovered the 54%-of-turn idle cost. Residual risk concentrates in the parser's tolerance set — each new tolerance is reactive (one incident per tolerance).
- **Cost note:** each synchronize event restarts the lane; combined with #1844 retrigger pushes, one PR can consume several 5–35 min review rounds on tree-identical heads. The `ci: retrigger` empty-push lever *requires* a new synchronize by construction — the recovery path and the review cost are coupled.

---

## 5. Findings matrix (common format)

| ID | Finding | Evidence | Impact | Recommendation | Risk of change |
|----|---------|----------|--------|----------------|----------------|
| F1 | `e2e-pr.yml` `e2e-frontend` is a functional duplicate of ci.yml `frontend-test` — same step set (npm ci / vitest / tsc / playwright mocked-backend / report upload) in a different order, on `ubuntu-latest` vs ci.yml's pinned `ubuntu-24.04`; `e2e-fullstack` below it is `if: false` dead code (issue #24) | .github/workflows/e2e-pr.yml:58–73 vs ci.yml:501–547 | ~11 runner-min + 2 check-rows per frontend PR for zero marginal signal; two copies of the suite to maintain (they have already drifted: step order and runner image differ) | Delete the job (and workflow, once dead code removed); rely on ci.yml | ~zero — same step set already runs in ci.yml on every non-docs PR |
| F2 | 9×2 build matrix runs on every non-docs PR but never pushes; 49% of ci.yml runner spend (4 405 s measured) | ci.yml build jobs `push: ${{ github.event_name != 'pull_request' }}`; run 44188388905 timings | ~73 runner-min/PR; QEMU-arm64 flake surface red on a docs-only head today | Path-filter builds per component on PRs (build only images whose tree changed), full matrix on push-to-main + tags | low-med — main push + release.yml both rebuild everything; a Dockerfile-only break would surface post-merge in minutes instead of pre-merge |
| F3 | `sdk-canary` is `continue-on-error: true` — costs 503 s (incl. 4×`sleep 65`) and gates nothing; carries a 3/3 flake signature | ci.yml:742, .16 runbook §gate-state | ~8.5 runner-min/PR for zero gate value; flake noise poisons run signal | Either raise the canary env's login rate limit (kills the sleeps AND the 429 signature) then make blocking, or fold Tier-1 canaries into e2e-nightly | low — one env var + flag flip; the blocking decision needs the flake dead first |
| F4 | `coverage-delta` re-runs the base suite (~11 min) to post an informational comment; nothing blocks on it by design | ci.yml:327–462 header | 647 s/PR of pure information; a base hard-flake degrades gracefully but the job still spends the minutes | Demote: compute nightly, or diff against a cached base profile artifact from main's last push | low — the 50% floor stays in `test` |
| F5 | Release cut + gitops bump is a ~10-step manual procedure across two repos with hand-copied digests; the CRD apply was absent from it until #2092 | runbooks .16/.17; release-bump-checklist §2 | two-bite CRD outage class (#2088/#2092); transcription-error surface (an #1237 incident was exactly a wrong paste) | One command (Top-5 #3): generate the cut commit, the tag, and a ready-made gitops patch from the release run's own prints; sequence after #2092 merges | medium — generated commits still need human review + the AI gate |
| F6 | Linter pin drift between ci.yml and release.yml stalled the v0.44.15 train (2 of its 3 failed runs) | faf9cdb90, d89a10a6e | ~45 min dead release runs + re-tag cycles per occurrence | Single-source the golangci version (Makefile var consumed by both) | ~zero |
| F7 | #1844 event-drop wedge forced ≥5 full-matrix retriggers today on tree-identical heads | git log `ci: retrigger` series | largest single avoidable multiplier on per-PR cost; also restarts AI review rounds | (platform-side mostly) — repo-side: keep the empty-push lever, but consider `workflow_dispatch` head-pinned runs (already verified working) as the *first* lever since it avoids a new synchronize | n/a (process) |
| F8 | Security-scan reddens on no-fix dev-dep CVEs (picomatch, 3/13 runs; blocked #2090) | status bank 22:25Z | merge friction with no remediation path until upstream | formalize the `.trivyignore`-with-expiry discipline (a064dcb0d pattern) | low |
| F9 | Stale comment: `test-full` header claims zero `-short`-skipped tests; 43 skip-guards exist in 33 files | ci.yml:469–472 vs `grep testing.Short()` | misleads future cost decisions about test/test-full overlap | fix the comment when next touching ci.yml | ~zero |
| F10 | AI-review ledger parser fragility is incident-driven (one tolerance per burn) | .github/prompts/pr-review.md:247 (issue #80) | verdict-rejection burn class | keep the prompt's literal contract + failure story pattern; add parser fuzz tolerance proactively in ai-workflows | n/a (cross-repo note) |
| F11 | `posture-gate` triggers on `api/**`+`controller/**` (most Go PRs) at ~20 min each | posture-gate.yml paths; run 44165478877 = 1 178 s | 20 min on the critical path of ordinary feature PRs | narrow paths to the posture surface, or reuse prebuilt `:dev` images instead of in-job builds | med — gate breadth was a deliberate §11 ruling-3 choice; needs owner sign-off |

---

## 6. Top 5 simplification candidates (ranked value/risk)

Each includes the **functional-equivalence argument** (what preserves the current guarantee) and requires **no code change in this audit** — these are proposals for follow-up stories.

### #1 — Delete `e2e-pr.yml`'s `e2e-frontend` job (and the dead `e2e-fullstack`) — highest value, lowest risk
- **Value:** ~11 runner-min + 2 check rows per frontend PR for zero marginal signal; removes one of the two copies of the suite (the copies have already drifted — step order and runner image differ between the workflows, which is itself maintenance evidence for consolidation).
- **Functional equivalence:** ci.yml `frontend-test` runs the same step set (npm ci → tsc → vitest → playwright mocked-backend → report upload) on every non-docs PR, on the pinned `ubuntu-24.04` image (the *newer* pin — e2e-pr's `ubuntu-latest` is the unpinned one, so consolidation also removes the un-pinned runner exposure); e2e-pr's own `changes` job exists only to gate the duplicate. The disabled fullstack job has never run (`if: false`, issue #24).
- **Risk:** ~zero. The only loss is a second copy of the same green check.

### #2 — Conditionalize the PR-side build matrix (path-filtered per component; arch matrix on push-to-main and tags only)
- **Value:** up to ~73 runner-min per PR (49% of ci.yml spend); removes the QEMU-arm64 flake class from the PR path (today's example failed a docs-only head).
- **Functional equivalence:** (a) PR-side Dockerfile-break detection is preserved *per touched component* (a `paths:`-filtered single-arch build per image whose tree changed); (b) the full 2-arch matrix + registry push still runs on every push to main — main is the documented safety net ("push/dispatch events bypass the gate and always run everything"); (c) release.yml is self-contained and rebuilds everything for tags regardless of CI. The only behavior change: a cross-arch-only break (e.g. arm64 base-image quirk) surfaces at merge+minutes instead of pre-merge — the same window that already exists for any path the docs-only fast path skips.
- **Risk:** low-medium; needs the per-component path lists (mechanical: api/** → build-api, etc.) and a one-time check that no required-check names depend on skipped PR build rows (branch protection currently has NO required-status-checks rule — verified in ci.yml's own note).

### #3 — One-command release cut + gitops-patch generation (sequenced AFTER #2092 merges)
- **Value:** removes the ~10-step manual procedure across two repos (F5); eliminates the transcription-error class (an #1237 incident was a wrong paste) and makes the CRD apply an implicit step rather than a remembered one.
- **Sequencing:** (1) #2092's `deploy/flux/crds` mirror + `FluxCRDDriftCheck` + `make flux-sync-crds` land first (they are the guard); (2) `make release-cut VERSION=X` wraps: CHANGELOG 2-line heading insert (b89ddf12e precedent) + Chart.yaml version/appVersion bump (pairing already guarded by `appversion_drift_test.go`) + fmt-check + commit + push + `release-tag`; (3) a post-release workflow step harvests the `merge-*` delivery-pin prints + semver tags into a **machine-readable patch file** (or direct branch) for talos-ops-prod, with digest-GET verification built in (P1/P2 of `local/release-preflight.sh` already encode the checks — reuse them); (4) human + AI review remain on both generated commits.
- **Functional equivalence:** byte-identical artifacts, same gates run in the same order; the humans move from transcribing to reviewing. `make release-tag`'s existing validations are unchanged.
- **Risk:** medium — two repos, credentials for the ops repo, and the gitops PR must keep its own review; mitigation is generation-not-application (the command never merges anything).

### #4 — Demote `coverage-delta` off the per-PR path
- **Value:** 647 runner-min-seconds per PR (~11 min) for a comment nobody gates on; also removes the base-flake degradation path from PR context entirely.
- **Functional equivalence:** the enforcement (50% floor) lives in `test` and is untouched; the delta comment can be computed (a) nightly against the previous night's cached profile, or (b) per-PR but from the *main-push run's* profile artifact (no merge-base re-run at all — main runs the same suite anyway). Information content identical, latency slightly worse.
- **Risk:** low — worst case the comment is later or absent on fast-follow pushes.

### #5 — Canary: kill the sleeps + the 429 signature, then make it blocking or move it
- **Value:** either recovers ~500 s/PR of *currently-non-blocking* spend into a real gate, or deletes it from the PR path. The 4 × `sleep 65` rate-limit waits (260 s) are a configured-in-env problem: the canary starts its own API server — the login burst limit can be raised for that server via env, eliminating both the sleeps and the `S-ENV-VARS → 429` flake that forced `continue-on-error` in the first place.
- **Functional equivalence:** same scenarios, same assertions; only the wait strategy changes (bucket never empties). If made blocking, the gate the job was always meant to be (#905's "flip to false once consistent") finally exists; if moved to nightly, the SDK canary coverage joins the e2e-nightly kind lane that already runs the full stack.
- **Risk:** low — env-config + flag flip; the blocking flip should ride ~a week of clean nightly data first.

**Honorable mentions (not top-5):** single-source the golangci pin (F6, ~zero risk, already burned a train); fix the stale `test-full` comment (F9); `posture-gate` image reuse (F11, needs owner ruling); release.yml "trust green main run" for the duplicated test suites (defensible since `release-tag` enforces the tag ride main — but the .15 linter-drift catch argues for keeping a slim lint; at minimum drop `test-full` from the release path, main already ran it).

---

## 7. Explicit non-goals

No code, workflow, Makefile, or chart changes were made in this audit. All timings/failures quoted are from live Actions data (runs 44188388905, 44180181647, 44168461790, 44146217348, 44061580277, 43983788986, 43986233145, 43987590002, 43992129196, 44114269696, 44165478877) and git history on origin/main plus the in-flight `fix/2092-crd-skew-convergence` branch (explicitly flagged as unmerged).
