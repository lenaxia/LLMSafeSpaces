# #1756/#1775 post-union remediation (fix-forward against main)

Verdict: the post-union CHANGES_REQUESTED on #1775 @ `6c024f10`
(ten findings, five reachable production regressions). This worklog
tracks the remediation; pre-push subagent verification per the #1747
standard applies before anything is pushed.

## Findings → fixes (code complete, building green)

1. §2 notice for ses_ inputs: IsLegacyIDForm + hash-miss passthroughs
   now set DeprecatedInput (sessionindex/ref.go); Sunset is an RFC
   8594 HTTP-date (Tue, 01 Dec 2026 00:00:00 GMT = v0.41.0 removal).
2. Registry pull emits {sessionID: InternalID, shortID: phrase};
   unminted rows skipped (replica install rejects empty short ids).
3. Archived set appends InternalID (matches agentd harness row ids).
4. Reap verdict compares row.InternalID (was phrase vs canonical).
5. RebuildMessageCount keyed row.InternalID (no ghost rows).
6. context_used heal + UpsertContextUsed keyed sessions[i].InternalID
   (heal fires; CRD not fetched every list call).
7. Mint base drawn ONCE outside the retry loop; retries suffix the
   SAME base (deterministic convergence per the ruling).
8. Did-you-mean candidates carry InternalID (ses_-form edit-distance
   leg live again; degraded rows rank by full id). aliasSuggestions
   parity.
9. agentd §1: send_message/session_metadata/session_read (raw-body
   best-effort rewrite)/compact_session emit phrases +
   session_id_deprecated cold marker + deprecation notes; catalog's
   oc- guidance retired.
10. Sunset HTTP-date (with 1); MintPhrase's nil error documented as
    kept for call-site symmetry.

## Test hygiene

- TestMintPhrase_HighEntropyNotSequential (birthday bomb, ~6.2%/run)
  REPLACED by TestMintPhrase_CoverageAndDistinctness (prefix coverage
  + distinct fraction; 40x stable).

## Records

- CHANGELOG [Unreleased] reconciled to one umbrella per category;
  ### Fixed entry added for this remediation.
- Original #1756 worklog's two record-integrity errors corrected
  inline (phantom file references; the false step-2 claim).

## Pending (this window or next — NOTHING pushes before these exist)

- [x] Middleware pin: ses_ input → Deprecation/Sunset headers
      (TestSessionRef_LegacyInputCarriesDeprecationNotice — PASS;
      asserts RFC 8594 HTTP-date via http.ParseTime).
- [ ] Integration: registry payload → pullSessionShortIDs →
      resolveSessionRef-by-phrase → harness id; ArchivedSet →
      filterArchivedFromSessionList.
- [x] Unit: reap phrase-keyed row; RebuildMessageCount InternalID;
      backfillContextUsed heal (reap + rebuild pins in the gone/count
      test files with ID≠InternalID seeds; four backfill tests
      reseeded to the post-#1756 shape — the fix had exposed their
      pre-union seeds, which was the red-first evidence).
- [x] EnsureSession minted + degraded paths
      (TestEnsureSession_MintedPath_SessionIDIsThePhrase /
      TestEnsureSession_DegradedPath_CarriesDeprecatedSessionID —
      both PASS; shared backend harness factored out).
- [x] E2E happy+unhappy phrase addressing (happy:
      TestSessionRef_ShortIDCaseInsensitive pre-existed; unhappy:
      TestArchivedRoute_Unindexed404CarriesSuggestions reshaped —
      id-like miss suggests via the InternalID ranking leg AND the
      candidate's harness id asserted ABSENT from the payload).
- [x] Pre-push subagent verification + report (the verify ran pre-push
      and found 3 gaps, all fixed pre-push: send_message origin leak,
      PlanReconciliation prevMiss phrase-key, threshold flake — see
      Completion; the checkbox was left unchecked by oversight, noted
      by review r2 Project Alignment)..

## Completion (2026-10-21)

- Pre-push adversarial verify (subagent) verdict: findings 1-8,10 OK;
  finding 9 PARTIAL (send_message `origin` raw leak) — FIXED +
  `TestMCPSendMessage_OriginEmitsPhraseOrFlaggedID`. Verify also
  found: PlanReconciliation error-path prevMiss keyed phrase — FIXED;
  distinctness threshold 998 carried ~1/430 flake — re-sized to 996
  (P(fail)≈1.4e-4, 60× loop clean).
- #1799 (deterministic-seed entropy test) landed mid-flight: resolved
  by UNION at rebase — their seeded HighEntropyNotSequential kept,
  my CoverageAndDistinctness added.
- Rebased on fb687084, suites green, pushed:
  origin/fix/1756-post-union-remediation @ e71fc021. PR #1806.

## Review r1 (PR #1806, CHANGES_REQUESTED) — remediated

- 1a mint determinism: TestEnsureSessionRegistered_RetriesSuffixTheSameBase
  (bind-recording arg; attempt N binds base-N — red on a re-roll revert).
- 1b PlanReconciliation: discriminating error-path case (ID=phrase,
  InternalID=ses_cold; counter must carry by InternalID).
- 2 session_read rewrite: TestMCPSessionRead_WarmReplicaRewritesSessionIDs
  (warm store; three id spellings rewritten, cold id untouched).
- 3 hash-miss passthrough: TestSessionRef_HashMissPassthroughCarriesDeprecationNotice
  (oc- miss still attaches Deprecation + RFC 8594 Sunset).
- 4 records: CHANGELOG rebuilt from origin/main + ONE flat entry (the
  umbrella restructure had orphaned main's #1747 Task-Board heading —
  dropped in favor of main's canonical structure); integration claim
  reworded to "handler pins"; worklog renumbered 1160→1161 (collision
  with main's 1160_2026-10-19_1747).
- 5 workspace-name lane Sunset now the RFC 8594 HTTP-date (same v0.41.0
  window); TestWorkspaceRef_UnambiguousNameRewrites pins ParseTime.

## Review r2 (PR #1806, CHANGES_REQUESTED) — the mass-reversion repair

r1's commit 623fac53 carried a tree built from the PRE-merge checkout:
the branch bot had merged main into the PR branch (f06c129f, bringing
the #1747/#1763 lane) and my local pointer advanced, but the worktree
never updated — `git add -A` committed a tree that silently reverted
101 files of main's merged content. The reviewer caught it; the r1
commit message and comment disclosed none of it (true failure on my
part — the push was fast-forward so nothing flagged).

Repair @ this commit: working tree restored to origin/main (8386ce0e)
via `git restore --source`, my cumulative intended delta
(fb687084..623fac53, 28 files) reapplied with `git apply --3way`
(clean; the two NEW files recovered explicitly — restore had removed
them too), CHANGELOG citation de-SHA'd (PR #1806, not the branch-only
6c024f10). Diff vs main verified = 28 files, all intended, zero board
deletions. r2's missing-test 1 closed (TestRegistryPullToReplicaSeam);
missing-test 2 covered by the existing wiring-tier cold pins
(OriginEmitsPhraseOrFlaggedID cold leg; WarmReplicaRewrites' ses_cold
passthrough), cited in the reply.
