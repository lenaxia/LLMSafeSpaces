# Worklog: #1795 — worklog uniqueness invariant (ruling c): renumber-all stays, duplicates redden CI

**Date:** 2026-10-21
**Session:** Implementation of the owner's option-(c) ruling on the post-rewrite lag window: keep the distributed self-healing renumber-all semantics, add the repolint uniqueness invariant the removed check used to pretend was unnecessary, and correct the stale single-writer comment.
**Status:** Complete

---

## Objective

Duplicate worklog numbers — producable by two lag-window writers under the sanctioned semantics — must redden CI; the false "sentinels never appear on origin/main" assumption must be corrected in code.

---

## Work Completed

- **Root cause mapped**: runWorklogSentinels' comment records that the old duplicate check was "intentionally removed" on the assumption that "the post-merge bot assigns numbers atomically per-merge, so merge-time collisions cannot occur" — falsified by the 25ed1853 lag window (origin/main carrying unnumbered sentinels between a merge and the bot's numbering commit), where a rebasing lane's post-rewrite renumber-all and the bot can independently mint the same number.
- **Red-first pins** (both demonstrably RED pre-implementation, GREEN post):
  - TestWorklogUniqueness_RedOnDuplicates — CLI-level: a scratch root with two worklogs claiming 1150 must produce a "FAIL worklogs uniqueness" naming the number. The DISCRIMINATING assertion is the check's own output line (the scratch's other missing-dir failures would otherwise trivially satisfy an exit-code check — caught while building the pin: the first fixture also had a hyphen/underscore typo that deformed the duplicate into an unversioned file; both fixed before relying on it).
  - TestRepolintMain_RegistersWorklogUniqueness — the #1305 registration pattern: silent deregistration fails the pin.
- **The lag-window e2e pin** — TestPostRewrite_LagWindowRenumbersMainInheritedSentinels: the 25ed1853 scenario end-to-end through the REAL post-rewrite hook (scratch repo + linked worktree, main carrying two unnumbered sentinels, lane rebasing onto it): every sentinel numbered INCLUDING the main-inherited ones, all numbers unique. Pins ruling c's chosen semantics — an option-(a) "fix" (skip main-inherited) must fail here. Building it surfaced two harness realities, both documented in the pin: the hook's fix-worklogs pass resolves root via go.mod (the scratch must carry one) and the pre-existing hookScratch seeds its own lane sentinel + numbered fixtures (the assertion counts accordingly).
- **The invariant**: runWorklogUniqueness in cmd/repolint/main.go — SequenceCheck over worklogs with WorklogNumberPattern (since the r6 blind-spot fix; originally WorklogPattern) + AllowGaps (duplicates FAIL red; gaps WARN — historical or bot-lag holes that nothing backfills).
- **Comment fix** in sequence.go's assignSentinels: the sentinel pass now documents the lag window, the deliberate renumber-all heal (assignments skip both local and remote occupied numbers — every number WorklogPattern can see; uppercase-slug files are invisible to the pass, the uniqueness invariant is their net), and points at the uniqueness invariant as the safety net.
- **Mutation verification on the final tree**: deregistering the check (file-copy mutation, restored) → both pins FAIL; registered → full pkg/repolint suite green (21.9s); `make repolint` on the real repo clean (sentinels ok, uniqueness ok-with-gap-warnings).

## Key Decisions

- Gaps are warnings, duplicates are failures — the ruling names duplicates as the invariant; nothing backfills historical gap holes (72 at this head, per the built binary), so gap-gating would redden every branch forever.
- The e2e pin runs the REAL hook binary path (prebuilt real repolint where the hook looks for it) rather than unit-mocking FixWorklogs — the scenario IS the hook's contract.

## Blockers

None.

## Tests Run

- Red: TestWorklogUniqueness_RedOnDuplicates + TestRepolintMain_RegistersWorklogUniqueness FAIL pre-implementation (demonstrated); lag-window e2e GREEN pre-implementation by design (pins unchanged sanctioned semantics).
- Green: `go test -count=1 ./pkg/repolint/ ./cmd/repolint/` ok; mutation round-trip verified; `make repolint` clean on the repo.

## r6 Addition: the uppercase-slug blind spot, closed

The r6 review proved the invariant's matcher blind spot with LIVE evidence: `WorklogPattern`'s lowercase-only slug class hid two real duplicate pairs on main (0498: epic-54-US-54.1 vs relay-leak-fix; 0500: epic-54-US-54.2 vs epic-43-verify-pending-invitee — the uppercase `US` made the first of each pair invisible). Fixed by: `WorklogNumberPattern` (any `NNNN_`-prefixed .md) as the uniqueness matcher + the pinned red test `TestWorklogUniqueness_UppercaseSlugsCounted` + healing the two pairs (the non-series members renumbered to 1166/1167; the epic-54 series keeps its numbers).

## Next Steps

- PR review loop to APPROVED → rebase-current → final packet.

## Files Modified

- CHANGELOG.md — Unreleased entry (the invariant; the security bumps shipped separately: 43a7ba29 toolchain, #1828/57495a4c vite, #1819 the five builder digests)
- cmd/repolint/main.go — runWorklogUniqueness + registration
- pkg/repolint/sequence.go — assignSentinels comment corrected
- pkg/repolint/worklog_uniqueness_test.go — the four pins (CLI red, uppercase matcher, registration+matcher pin, post-rewrite e2e)
- README-LLM.md + docs/contributing/worklogs.md — the sentinel-scheme race narrative corrected (bot-lag window, not atomic-impossible)
- pkg/repolint/sequence_test.go — the retired-test comment's same falsified sentence corrected
- .githooks/pre-commit — the run_repolint_gate comment's 'structurally impossible' premise corrected; the printed failure hint gained the worklogs-uniqueness heal guidance
- .github/workflows/worklog-renumber.yml — the race-safety comment corrected (partial: serial-common-case, lag-window counter-case named)
- worklogs/0498→1166_relay-leak-fix-implementation.md, 0500→1167_epic-43-verify-pending-invitee.md — the two healed historical duplicates
- worklogs/1169_2026-10-21_worklog-uniqueness-invariant.md — this worklog (NNNN->1161 bot-in-branch; ->1162; ->1164; ->1165 by our own post-rewrite hook; ->1169 past main's bot-assigned 1165_trigger-script-gate — the fourth lag-window renumber)
