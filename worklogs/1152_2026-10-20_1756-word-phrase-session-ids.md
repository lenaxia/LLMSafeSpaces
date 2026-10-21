# Worklog: #1756 — word-phrase session IDs; short-only emission; full-id input deprecation

**Date:** 2026-10-20
**Session:** continuation of the #1691 worker (same lane). Branch `feat/1756-word-phrase-session-ids`, worktree `wt-1756`, off main @ 3e596362.
**Status:** Complete — PR open, orchestrator merges on APPROVED

---

## Objective

#1756's fast-follow to #1745, per the owner's final rulings: session short IDs become 3-word grammatical phrases (adj-noun-verb, frozen curated lists, NO hash/prefix/checksum, deterministic counter suffix on collision, exact-word-count validity); outputs go short-only (the phrase is THE identifier; full/hashed ids leave default output); full-id INPUT is accepted one release with a deprecation notice. #1757 (workspace phrases) is the next PR on this lane.

---

## Work Completed

### The mint (pkg/shortid)

- `wordlists.go`: frozen curated lists — 160 adjectives × 192 nouns × 64 verbs (~21 bits/workspace; enumeration ruled a non-issue) — lowercase ASCII ≤9 chars, pairwise disjoint, EXCLUDING the harness's observed slug vocabulary (namespace rule PINNED: disjointness is BY SHAPE — 3-word phrase vs 2-word slug vs #1757's 2-word+digit — with the vocabulary exclusion as belt-to-braces). `TestWordlists_Frozen` pins sizes/membership so silent edits fail.
- `MintPhrase` (crypto-seeded PRNG; the space is not a secret), `WithCounterSuffix(base, n)` = deterministic `-2`/`-3`… collision disambiguator; `EnsureSessionRegistered` re-mints with the counter on 23505 — never a re-roll.
- Classifiers: `IsShortIDForm` = exact 3-word phrase shape (case-insensitive), optional 4th counter token; partials categorically invalid; the 2-word slug and 2-word+digit workspace shapes never classify. `IsLegacyShortIDForm` = the #1745 hash form (INPUT-only, never minted). `IsDeprecatedInputForm` = full id or hash short id.

### Resolution + deprecation (sessionindex, middleware)

- Precedence step 1 accepts BOTH phrase and hash form (same column/index) with `DeprecatedInput` flagged for the hash form. [CORRECTED by the post-union review: step 2's full-id passthrough did NOT flag — fixed in the #1756 post-union remediation PR, which also flags the hash-miss passthrough.] Truncated full ids still never silently resolve (#1691 pin).
- The edge middleware attaches the #1756 notice on deprecated inputs: `Deprecation: true` + a `Sunset` header naming the phrase form. 503 degrade behavior unchanged.

### Short-only emission (REST + agentd)

- `SessionListItem.ID` IS the phrase (`json:"id"`); the harness key rides `InternalID json:"-"`; `ParentID` is the parent's PHRASE via a LEFT JOIN self-join (degrades to the harness parent id until the parent is minted); `ShortID` left the wire. The reconcile/counts/active-merge/backfill machinery keys on `InternalID` (verified by the existing #1340/#1481 pins going green unchanged in semantics).
- `EnsureSessionResponse.SessionID` = the phrase; degraded window carries `deprecatedSessionId` instead.
- `Suggestion.SessionID` → `json:"-"` — the re-copy affordance is the PHRASE.
- agentd: `session_list` rewrites each row's `id` to the replica-known phrase (unmapped rows keep the harness id + `id_deprecated: true`; empty replica = verbatim passthrough); `create_session` returns `session_id` = phrase (deprecation note if the seam timed out); rename/archive/delete/abort results emit the phrase + a `deprecation` note when the caller used a full/hashed id.

### Scoped to the next PRs (explicitly, per spec sequencing)

- SSE payload value-flip + frontend URL/keys, platform MCP output shapes, and the SDK/OpenAPI MAJOR: #1756 spec §5 coordinates the SDK-breaking change with the ABI short_id M2 work — this PR updates openapi.yaml descriptions only (no breaking SDK shape churn here). #1757 (workspace phrases + {workspace-phrase, session-phrase} addressing) follows immediately.

---

## Key Decisions

- **Word-list namespace (pinned in-lane per the ruling):** disjoint-by-shape, with vocabulary exclusion as the secondary guard. Session phrases can never be confused with harness slugs or #1757 workspace phrases in mixed text.
- **~21 bits is the deliberate size** — the owner's ruling explicitly says the sparse space plus resolver+did-you-mean is the recovery; entropy machinery is forbidden.
- **Counter suffix on the FIRST collision retry** = `-2` (attempt N ≥ 2), deterministic, never a random re-roll: two racers converge on the same suffixed form or the unsuffixed one, arbitrated by the partial unique index.
- **Full-id INPUT acceptance keeps the #1691 semantics** — byte-identical passthrough, zero DB probes, truncated ids never resolve — with the deprecation notice layered on top.

---

## Blockers

None.

---

## Tests Run

- `go test ./pkg/shortid/ ./pkg/sessionref/` — green (phrase format pins, frozen lists, namespace disjointness, counter suffix, classifier table incl. partials/slugs/workspace shapes; Rank over phrase-edit neighbors).
- `go test ./api/internal/services/database/` — green (seam mints phrases with counter retry; the list self-join fixture incl. parent-phrase degradation).
- `go test ./api/... ./pkg/...` — full sweep, zero FAILs (the #1340 reconcile matrix, #1481 counts, q-filter, precedence, ambiguity, degrades — all re-based onto the phrase fixtures).
- `go test ./cmd/workspace-agentd/` — full package (~330s) green, incl. the new `TestSessionList_EmitsPhraseAsTheID` (mapped row id = phrase, shortId gone; unmapped row marked `id_deprecated`; empty replica verbatim) and the create_session phrase pin.
- `golangci-lint run` on every touched package — 0 issues (two scoped gosec nolints on the seeded PRNG, documented).

---

## Next Steps

- #1757 on this lane: workspace phrase ids (2 words + 1 digit, fleet-unique check-and-retry), {workspace-phrase, session-phrase} cross-workspace addressing, trigger targetWorkspaceId phrase acceptance.
- SSE value-flip + platform MCP + SDK major with the ABI M2 coordination per spec §5.

---

## Files Modified

- pkg/shortid/{shortid.go, phrase.go, wordlists.go, shortid_test.go, phrase_test.go}
- pkg/sessionref/sessionref.go + tests (Suggestion payload)
- pkg/types/session.go (SessionListItem/EnsureSessionResponse)
- api/internal/services/database/{short_session_ids.go, database.go} + test fixtures
- api/internal/services/sessionindex/{ref.go, service.go} + tests
- api/internal/services/workspace/workspace_service.go
- api/internal/middleware/session_ref.go
- api/internal/server/{router.go, router_session_ref_test.go, router_frontend_workspace_test.go, router_session_reconcile_test.go}
- api/internal/handlers/{proxy_session_index.go, opencode_upgrade_test.go, proxy_shortid_wiring_test.go}
- cmd/workspace-agentd/{session_ref.go, mcp_tools.go, mcp_server.go, session_ref_test.go} [CORRECTED by the post-union review: session_shortids.go and mcp_tools_test.go were NOT in the shipped tree — record now reflects the final diff.]
- sdks/openapi.yaml, CHANGELOG.md, this worklog
