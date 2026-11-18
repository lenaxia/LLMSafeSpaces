# NNNN — cross-surface simplification roadmap (synthesis of the six audits, read-only)

Task `tsk-chilled-hawthorn-notices` (SYNTHESIS: gated on all six surface
audits — dedupe cross-surface findings, resolve conflicts, produce the ranked
simplification roadmap with sequencing and a do-NOT-touch list; flag
functionality-reducing findings out of scope). Deliverable:
`reports/audits/simplification-roadmap.md` — no code changes.

## Inputs consumed (in full)

- `reports/audits/board-engine-complexity.md` (tsk-sappy-nickel-shines, PR #2098)
- `reports/audits/api-app-complexity.md` (tsk-tidal-wolfhound-restricts, PR #2096)
- `reports/audits/agentd-complexity.md` (tsk-superb-hammer-pricks, PR #2097)
- `reports/audits/emission-defenses-complexity.md` (tsk-dual-tanker-ambles, PR #2095)
- `reports/audits/frontend-complexity.md` (tsk-pallid-lavender-supports, PR #2099)
- `reports/audits/ci-release-complexity.md` (tsk-hazel-mint-waits, PR #2094)

All six confirmed done on the board before synthesis began; their docs read
from their audit worktrees at the reviewed heads.

## What the synthesis did

1. **Dedupe** (roadmap §3): the same accretion showed in 2+ audits in eight
   identifiable themes — the #2040 re-derivation class (api-app F1/F7/F9 +
   agentd F1/R1/R3), engine-semantics re-derivation outside the decision plane
   (api-app F2 + emission F6), reset-by-enumeration (board-engine B1/B3 +
   frontend FC-2/FC-6), comment-enforced lockstep sets (emission F4 + §7.2 +
   api-app F7 + board-engine B9), copy-paste machinery with shipped divergence
   (frontend FC-5/FC-7 + api-app F3 + agentd F11 + ci-release F1), dead mass
   (six findings across four audits), config-surface hygiene (board-engine B5 +
   emission F3), documentation-as-code (agentd F2 + board-engine B4/B10).
2. **Adjudicate** (roadmap §4): nine conflicts resolved with the audits' own
   evidence — converge only decision-gating busy reads (not the sealed-module
   fold or healthz); unify alert machinery, keep the two evidence classes;
   affirm the 22→8 fence; retire strict-args on the corrected all-time bound
   with a +7 d re-measure gate on deletion; posture-gate breadth and the
   release-path test-full drop ruled owner-gated/out-of-scope; canary
   fix-then-block preferred over nightly; board-engine's ruled-proportionate
   twins left documented.
3. **Rank + sequence** (roadmap §5/§6): 15 initiatives. The charter's posed
   question answered explicitly — the board-engine state work splits around
   wait-lifecycle: episode-reset fragment BEFORE its implementation (the
   typed-wait columns join the fragment), the `last_attempt_failed` derivation
   WITHIN its migration window, ladder extraction AFTER its engine work, and
   the full 22→8 collapse NEVER (fence affirmed). Release automation
   HARD-gated on #2092 (PR #2093 verified open at this head).
4. **Fence** (roadmap §8/§9): a 16-entry do-NOT-touch armor list, each with
   live incident provenance and the no-equivalent-simplification argument;
   eight functionality-reducing or verification-reducing findings flagged
   OUT of scope by charter.

## Notes for the orchestrator

- The epic-comment deliverable is embedded verbatim as roadmap §0 (the board
  API exposes no epic comment or read surface — see worklog note below; the
  PR body carries the same summary for the owner).
- The six audit PRs were still open at this head; this doc cites their paths
  and PR numbers and should merge after (or with) them.

## Verification

- Docs-only diff; `make fmt-check` green. No code changes.
