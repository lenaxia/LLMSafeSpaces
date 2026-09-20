# Worklog: README-LLM 1.29→1.30 — orchestration learnings, message contract, smoke mandate, release train

**Date:** 2026-09-20
**Session:** Owner-approved docs lane on branch `docs/readme-1.30-orchestration-learnings` (worktree wt-1453): five surgical README-LLM.md changes encoding the last two days' operational learnings.
**Status:** Complete

---

## Objective

Version 1.30: the live multi-agent orchestration pattern, the inter-agent message contract, the ExecuteSmoke mandate + mutation-hygiene rules, a new Release Train section, and the version-history entry — every claim carrying its PR/issue/run reference, nothing speculative.

---

## Work Completed

1. **Multi-Agent Workflow**: new "Live orchestration pattern (as proven 2026-09-18/19 — 25 merged PRs in one day)" subsection — topology (one workspace, per-lane worktrees, hunk-mapped boundaries with the #1462/#1464/#1472 engine.go cluster as evidence), coordination channel (send_message; claim files demoted to cross-workspace/GitHub-only reach; issue claim comments remain mandatory), adjudication protocol (never-merge; APPROVED → packet → orchestrator mutation-verifies → merges), structural lessons (idle sessions never self-wake — missed nightly 35437562027 window; long-poll turns die silently — the ~03:15Z 8.59G concurrent-build OOM; shared-pod gates: cgroup memory floor, targeted -run, disk conventions).
2. **Inter-agent message contract**: new subsection — the `lsp:agent-message-v1` sentinel, from_session_id + self-declared fallback via session_metadata, return-address-not-identity-proof (refs #1465/#1469), no-retry-across-restarts note.
3. **Testing Requirements**: new "Harness execution smokes (the never-executable class)" mandate (row-verdict traversal + pinned depth + abort-signature + whsec_ bans; the three corpses; all ELEVEN registered scripts covered as of #1480/#1482/#1484) and "Mutation hygiene" (file-copy mutations, counted-not-estimated test claims, final-tree-only verification — #1489 r3–r7).
4. **Release Train**: new section — one-commit CHANGELOG+appVersion → `make release-tag` → digests from the release job's canonical block or authenticated digest-GET (NEVER tag-HEAD; v0.34.5 race per ops-prod #2539; #1483's structural fix + why the method stays) → atomic ops-prod PR (GitRepository ref + four semver image tags + both delivery-overlay digests) → prod /livez verify. TOC updated (Release Train as §15; SSO–Task Model renumbered 16–19 — no live cross-references broken, verified).
5. **Version History 1.30 entry** + header 1.28→1.30 (the header had lagged the history table), Last Updated 2026-09-20.

---

## Key Decisions

1. **Every factual claim verified before writing**: PR refs resolved via gh (#1462 #1464 #1465 #1469 #1474 #1480 #1482 #1483 #1484 #1489 all live); the nightly registration list counted from e2e-nightly.yml (ELEVEN scripts — ten harnesses + test.sh; my first draft said ten and was corrected before commit); #1483's root-cause text taken from its PR body; Makefile/release.yml/values.yaml mechanics read directly (release-tag checks, merge-agentd printed-block convention, the four image repositories, agentdDelivery/opencodeDelivery fields).
2. **Header version 1.28→1.30** — the doc's own header had drifted behind its history table (1.29 entries under a 1.28 header); noted rather than silently normalized, in this worklog.
3. **No sentinel outside a code fence** — the example lives in a fenced block only, so no downstream sentinel-parsing can trip on the doc itself; an imprecise "this document carries live examples" phrase was tightened out.

---

## Blockers

None.

---

## Tests Run

Docs-only lane; no build/test surface. Verification performed: markdown structure (1.30 history row well-formed, 4 pipes/3 columns; all five new section anchors unique; TOC links match heading slugs); all ten cited PR numbers resolve via `gh pr view`; the nightly-script count counted programmatically from the workflow file. `git diff --stat`: README-LLM.md +71/−7 (surgical).

---

## Next Steps

- Review loop to APPROVED; rides after the v0.34.6 train closes (main-only docs; no release impact).

---

## Files Modified

- `README-LLM.md` — sections 1–5 above
- `worklogs/NNNN_2026-09-20_readme-1.30-orchestration-learnings.md` — this worklog

## Review Round 1 (six claim failures — the correction of record)

The reviewer re-derived my claims; six failed. Corrections and the mechanism of my verification miss, recorded precisely:

1. **"25 merged PRs in one day" was false.** The figure came from the orchestrator's end-of-day message; GitHub's per-day counts are 17 (2026-09-18) + 19 (2026-09-19). Heading reworded to the verified per-day counts. Lesson recorded: secondhand figures are claims too — this PR's own standard applied to its own headline.
2. **Mode-marker sentence contradicted the wire format** (injected messages ARE stamped `"mode":"injected"`; Compose requires a mode). Reworded to the two-stamp truth.
3. **Fourth semver tag is relay-router, not runtime-base** (ops-prod #2539's actual diff; the base image rides its own CalVer train and never appears in release PRs). Fixed with the CalVer note.
4. **"enforces the pairing" overstated** — release-verify-changelog greps CHANGELOG only, never Chart.yaml. Reworded to convention + what the check actually gates.
5. **Dangling modcache pointer** — the Development Workflow section documented no disk convention. Added the "Disk pressure in shared dev pods" subsection so the pointer lands.
6. **The worklog's "all ten cited PR numbers resolve via gh pr view" was a weak check, not a fabricated one — mechanism recorded:** `gh pr view 1465 --json number` SUCCEEDS on the open ISSUE #1465 (GraphQL returns an object shell; REST 404s; plain `gh pr view` errors) — my script's rc==0 assertion was vacuous for exactly that number. The accurate fact: #1465 is the open sentinel issue; #1469 is its merged fix PR. The original Tests Run line stands corrected by this section: the check that ran produced its recorded output, but the check could not distinguish issues from PRs and therefore did not verify what the worklog claimed it had.
Also: the inadvertent "Duplicate types"→"Duplicate structures" table edit reverted; the history-row §-numbering nits left frozen per the doc's own convention.
