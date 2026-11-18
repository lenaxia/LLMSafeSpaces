# NNNN — pkg/board engine complexity audit (read-only)

Task `tsk-sappy-nickel-shines` (read-only complexity assessment of pkg/board:
the 22+ task state columns and the ladder arms they feed, the #2042-#2045/
#2061/wrap-up hardening stack as a SUM, and the reconciler/stage/audit/config
seams). Deliverable: `reports/audits/board-engine-complexity.md` — no code
changes.

## What was measured

- 37 non-test files, ~15.4 kLOC (reconciler 1,781 / service 1,388 / config
  1,039 / stage 987 / wrapup 930 the five largest); 21.2 kLOC of pins (58%).
- 26 task-table state columns across 10 column-adding migrations
  (000039-000059); 48 `UPDATE tasks` sites in 15 files; 11+ hand-maintained
  episode-reset sets; 21 nil-inert Service seams; 13 engine knobs (11
  deploy-exposed) + 4 constants; ~30-term audit-action vocabulary over 32
  `recordEventTx` sites; 3 coexisting event-id minting schemes.
- The ladder's effective 11-arm precedence reconstructed from rc1Reclaims
  (freeze → #2044 at-cap demotion → heartbeat-response → #2043 lapse
  zeroing → renewal rungs (movement/reaction/busy-capped) → dead-evidence →
  quiet arms), each arm traced to its incident.

## Headline findings

1. The dominant cost is the episode-reset threading discipline (the ~10
   column "clear the claim" set re-typed per exit site) — the package's
   worst observed bug class (3 real incidents), and exactly what #2060's
   22→8 collapse was chartered to remove. A SQL-fragment consolidation
   kills the same class at ~1/50th the risk.
2. The 22→8 ClaimEvidence collapse: **NO-GO now, narrowed GO later** — the
   columns are load-bearing single-statement SQL predicates (the resolved
   replica-arbitration design), 3 of the 8 target fields are new state not
   derived state, and the wire cost is permanent. Narrowed survivors:
   derive `last_attempt_failed` (verified ≡ `failure_count > 0` at every
   write site), treat the sentinel triple as one slot, and cap future
   evidence growth at jsonb slots (the `salvage` precedent).
3. The hardening stack is semantically proportionate (every layer a real
   incident, no no-behavior state found — the lexicon ruling holds) but
   overlaps at the edges: the lapse arm subsumes the renewal rungs when
   stale, the #2044 demotion subsumes all evidence at cap, four caps share
   "3" without a family statement, and the 90m lapse threshold's 2×-lease
   coupling is documentation-only.

## Process

Read-only: full-file reads + migration archaeology + CHANGELOG/spec
provenance. `make fmt-check` green (docs-only diff). PR iterating with AI
reviewer — never self-merge.
