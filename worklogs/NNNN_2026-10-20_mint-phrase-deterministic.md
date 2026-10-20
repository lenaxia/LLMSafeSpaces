# Worklog: #1794 — TestMintPhrase_HighEntropyNotSequential made deterministic (seeded window) with a stronger entropy guard

**Date:** 2026-10-20
**Session:** Test-design fix for the ~6.2%-per-run birthday-collision flake in the session-phrase entropy test. No production change — the fix and its strengthening live entirely in the test.
**Status:** Complete — parked review-ready (PR held until #1797 clears review, per the one-reviewable-at-a-time ruling)

---

## Objective

Make TestMintPhrase_HighEntropyNotSequential deterministic across repeated CI runs without weakening the entropy property it guards.

---

## Work Completed

- **Root cause confirmed** (the issue's math): 500 unseeded draws from the 160×192×64 = 1,966,080 phrase space → P(any duplicate) = 1 − exp(−500·499/(2·1,966,080)) ≈ 6.2% per run — a 1-in-16 CI redden with no defect behind it.
- **Fix**: the test reseeds the package-private `mintRand` (same-package test access — zero production diff) with a fixed seed for the draw window and re-arms it with a fresh crypto seed in `t.Cleanup`, so the window is byte-deterministic while the production path stays crypto-seeded (#1756 ruling: enumeration is a non-issue; ids are references under authz).
- **Entropy guard STRENGTHENED** (the issue's "without weakening" bar, exceeded): new per-slot distribution bounds — distinct adjectives ≥ 100, nouns ≥ 120, verbs ≥ 36 over the 500-draw window (true uniform expectations for 500 draws over N slots — N(1-(1-1/N)^500) — are 153.0/177.9/64.0; the r1 review caught my miscomputed ~135/~158/~47; bounds sit far below expectation and detect a generator drawing from under ~55% of a list (per-list floors: verbs ≈55%, adjectives ≈62%, nouns ≈63%)).
- **Determinism pin**: `go test -count=20` → ok (20 identical passes).
- **Mutation demonstrations** (both on file-copy discipline, restored after):
  1. Degraded generator (all slots narrowed to first 8): the duplicate check fires within the seeded window.
  2. SUBTLE narrowing (verbs only, 24 slots — 737,280 composed-phrase space ≈ 1475× the draws, so under the pinned seed the duplicate check passes): the verb distribution bound fires ("verb entropy low: 24 distinct in 500 draws"). This is the class the OLD test could never catch — the fix's second net.

## Key Decisions

- Seeded-RNG option chosen over collision-tolerance (weakens the distinctness assertion) and fewer-draws (still flakes, just less often — a 200-draw window keeps a ~1% redden). The seed makes the flake structurally impossible and buys room to strengthen the guard.
- Test-only seam (no exported hook, no production diff): the mint's package-level PRNG is same-package-accessible; the reseed holds the existing mintMu and the cleanup re-arms with cryptoSeed() so sibling tests observe production seeding.

## Blockers

None (PR opening gated on #1797's review clearance — orchestrator ruling).

## Tests Run

- `go test -count=20 -run TestMintPhrase_HighEntropyNotSequential ./pkg/shortid/` → ok (determinism).
- Mutation 1 (first-8-slots): FAIL via duplicate check ✓. Mutation 2 (verbs→24): FAIL via distribution bound ✓. Both reverted; `go test -count=5 ./pkg/shortid/` → ok (full package).

## Next Steps

- On #1797 clearing: push branch, open PR (Fixes #1794), review loop, rebase-current, final packet.

## Files Modified

- pkg/shortid/phrase_test.go — seeded window + distribution bounds + the math documented in the test comment
- worklogs/NNNN_2026-10-20_mint-phrase-deterministic.md — this worklog
