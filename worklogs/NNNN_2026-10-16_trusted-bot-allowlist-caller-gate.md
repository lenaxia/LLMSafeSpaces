# Worklog: #1710 — trusted-bot allowlist on the ai-comment caller gate (outage /review fallback fix)

**Date:** 2026-10-16
**Session:** P1 lane (owner ruling scope; folded #1709 L2 shared-allowlist definition). Upstream caller contract shipped in ai-workflows v0.2.16 (PR #49, README §4); this is the consumer half.
**Status:** Complete

---

## Objective

Stop the silent no-op of AI-command fallbacks posted by the platform's own delivery app: `agentic-actor-coder[bot]` structurally carries `author_association: NONE` (app bots are neither owners nor collaborators), so the ai-comment caller's association-only gate skipped every command it posted. During the 2026-10-16 event-delivery outage (#1704/#1707) both orchestrator `/review` fallbacks on PR #1708 concluded `skipped` with zero PR-visible trace (runs 38836736982, 38850671818). Add an explicit, exact-login, opt-in trusted-bot allowlist arm — one shared definition, pinned by tests — to every caller gate that filters on `author_association`.

## Root cause (verified in tree)

`.github/workflows/ai-comment.yml` gated `respond` on `OWNER || MEMBER || COLLABORATOR`. An app bot can never satisfy any of the three (association is `NONE` by construction), so the outage-recovery path — app-bot `/review` when `pull_request` events are lost — was dead by design. The fix could not land upstream: the reusable workflow cannot gate on the triggering comment (ai-workflows lesson #4; the `if:` must live in the caller).

## Audit (ruling item 1 — every author_association gate)

Repo-wide scan of `.github/workflows/*.{yml,yaml}`: **one** gate filters on `author_association` — the ai-comment caller `if:`. Ruling-site contradictions found (per delivery instructions, noted on the PR rather than forced):

- **pr-review caller `if:`** — does not exist: the pr-review caller has no actor gate; automation-bot skipping lives inside the reusable workflow (job-level `if:` there), and the PR-author assert path is the opencode CLI's `assertPermissions` pre-flighted by ai-workflows' same-source-contract `author-permission.sh` (no consumer-side arm to extend; v0.2.16 exposes no allowlist input).
- **renovate-analysis** — schedule-triggered, no actor gate (its design doc explicitly avoids `pull_request` for exactly the assert reason).
- **merge-command gate** — no such workflow; `/merge` routes through ai-comment.yml (covered).
- **issue-opened** — no actor gate in the caller; the assert failure there is #1709's lane (L1 skip / L2 mode-B are tracked there; the L2 shared-allowlist *definition* this lane owns is the one landed here).

## Work Completed

### (a) RED-first pins — `tests/ghaworkflows/gha_callers_test.go` (commit 2e137577)

Four pins, following the existing `gha_callers_test.go` invariants style:

- `TestTrustedBotAllowlistArmOnEveryAssociationGate` — scans ALL workflow files; any file filtering on `author_association` must carry the allowlist arm `contains(fromJson(...), github.event.comment.user.login)`. Future association gates are covered automatically. **RED at the pre-fix commit** (ai-comment.yml lacked the arm).
- `TestTrustedBotAllowlistCopiesDoNotDrift` — every gate copy's JSON literal must be byte-identical to `canonicalTrustedAppBots` (single source; ruling item 2).
- `TestTrustedBotAllowlistExactLoginsOnly` — every entry (canonical and every copy) must match `^[A-Za-z0-9][A-Za-z0-9-]*\[bot\]$`: wildcards (`*`), globs, and non-bot logins fail.
- `TestNoBlanketAssociationNonePass` — no workflow may carry `author_association == 'NONE'` / `!= 'NONE'` (blanket pass forbidden; each entry is a deliberate trust decision).

### (b) The fix — `.github/workflows/ai-comment.yml` (commit 446546b0)

The ai-workflows v0.2.16 README §4 canonical arm, appended to the association clause group:

```yaml
contains(fromJson('["agentic-actor-coder[bot]"]'), github.event.comment.user.login)
```

`agentic-actor-coder[bot]` is the sole initially-allowed entry (owner ruling). Header comment documents the shared-literal + never-blank-NONE contract.

## Verification

- **Red-first:** `go test ./tests/ghaworkflows/ -run 'TrustedBot|BlanketAssociation'` at the test-only commit (2e137577, parent = main @ c9ee41d3): `TestTrustedBotAllowlistArmOnEveryAssociationGate` FAIL — "ai-comment.yml filters on author_association but has no trusted-bot allowlist arm" (the outage bug class). Green at head (446546b0).
- **Pin mutation matrix (not committed):** extra entry in a gate copy → drift test fails; non-bot login → drift + exact-login fail; replacing the arm with a blanket `== 'NONE'` → arm + blanket tests fail; restored → `go test ./tests/...` green (`ghaworkflows`, `renovateanalysis` ok).
- Pre-commit gates passed (repolint, fmt, imports, scoped golangci-lint new-only).

## Operator impact

The outage fallback is functional again: when `pull_request` events are lost, an orchestrator/worker `/review` (or any AI command) posted by `agentic-actor-coder[bot]` now reaches the reusable AI Commands workflow instead of clean-skipping. Deliberate trust decision, reversible by deleting the login from the shared list (the drift test then forces every copy to drop it).

## Key Decisions

- **Literal list replicated verbatim + pin test** over a `vars.TRUSTED_*` repo variable (ruling item 2's two options): an unversioned repo variable is invisible to offline invariants tests — the exact-login pin (item 3c) requires the definition to live in the tree; the drift pin supplies the single-source guarantee instead.
- **Association-gate discovery is generic** (scan all `*.yml`/`*.yaml` for `author_association`): future gates are auto-covered without remembering to register them.
- The allowlisted bot can fire ALL commands (`/merge` included) under `secrets: inherit` — the issue's explicit ask; disclosed in CHANGELOG; revocation = delete the entry.

## Blockers

None.

## Next Steps

- Orchestrator merges on APPROVED (re-review pending after the r1 rework below).
- Live confirmation: the next orchestrator app-bot fallback command should appear as a non-skipped AI Commands run (post-fix E2E observation noted as thin by the reviewer).

## Files Modified

- `.github/workflows/ai-comment.yml` — allowlist arm + header contract.
- `tests/ghaworkflows/gha_callers_test.go` — four pins + helpers.
- `worklogs/NNNN_2026-10-16_trusted-bot-allowlist-caller-gate.md`, `CHANGELOG.md` — records.

## Rework r1 (PR #1715 review, CHANGES_REQUESTED @ 6e3a5a51 — commit-pinned verdict)

- **Blocking (docs):** the pre-existing header claim that the reusable workflow "skips automation-bot commenters itself" was factually false at v0.2.16 (verified: reusable `respond` has no job `if:`; `route-command.sh` has zero bot/author checks) and contradicted this PR's own added block. Corrected: caller admission is the ONLY authorization check.
- **Pin hardening:** `TestNoBlanketAssociationNonePass` now strips YAML comment lines (prose may cite the forbidden arm) and matches case-insensitively across both quote styles (`== "none"` is semantically identical in GitHub expressions) — mutation-verified: double-quoted lowercase blanket pass fails arm + blanket pins.
- **Style nit:** dropped the unused `name` parameter from `gateAllowlistArms`.
- **Assessed out of scope by the reviewer** (tracked in the review, not folded here): mid-sentence ` /review` mention dispatching a full review — pre-existing, pinned as intended by `TestAiCommentAuthorizationGatePresent`; belongs in its own issue if ever changed.
