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
- [ ] Unit: reap phrase-keyed row; RebuildMessageCount InternalID;
      backfillContextUsed heal. (NEXT WINDOW: handler fixtures at
      api/internal/handlers/proxy_session_index_test.go and the
      workspace list path; the fixes are at proxy_session_index.go:84
      + :311 region and workspace_service.go:1918 region.)
- [x] EnsureSession minted + degraded paths
      (TestEnsureSession_MintedPath_SessionIDIsThePhrase /
      TestEnsureSession_DegradedPath_CarriesDeprecatedSessionID —
      both PASS; shared backend harness factored out).
- [ ] E2E happy+unhappy phrase addressing. (NEXT WINDOW.)
- [ ] Pre-push subagent verification + report.
