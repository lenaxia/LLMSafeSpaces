# Worklog: #1879 — archive degradation made explicit (stale-set markers + post-write verification)

**Date:** 2026-10-23
**Branch:** `fix/1879-archive-durability`
**Status:** PR r1

---

## Objective

Issue #1879 (owner report, workspace `b150f355`, 2026-10-23
03:30–04:45Z): the orchestrator archived finished workers via
`session_archive`, got `status: archived` success replies — then its own
`session_list`/`session_metadata` kept showing those sessions as live
("archives not sticking"), mixed across a ~25-session sweep. Root shape:
archive state exists in exactly ONE place (`session_index.archived`) and
every harness-side consumer reads it through a pod cache that fails
open — past the ~15s freshness window the filter silently degrades to
the unfiltered list, with zero signal to the caller.

## Root cause (verified against the tree)

1. **Index-only write path.** `mcpSessionArchive`
   (cmd/workspace-agentd/mcp_tools.go) → platform
   `PodSessionArchiveHandler.Archive` → `SetSessionArchived` →
   `session_index.archived`. Nothing harness-side records archive
   state; `time_archived` appears NOWHERE in this repo (grep-verified).
2. **Fail-open freshness gate.** `filterableArchivedSet()` returns nil
   when the last SUCCESSFUL fetch is beyond `archivedSetCacheTTL` (15s)
   — the #1735 review r1 finding 1 ruling (stale data must not keep
   hiding rows) — and `filterArchivedFromSessionList` then passes the
   body through unfiltered, SILENTLY. Sustained fetch failures
   (platform 5xx/unreachability on `/internal/v1/session-archived`)
   produce exactly the incident: index flags correct in Postgres, pod
   listings unfiltered for the outage's duration.
3. **No post-write convergence.** A successful archive on THIS pod left
   the pod's own cached archived set untouched for up to 15s — and for
   the outage duration under fetch failure. agentd subscribes to no
   platform SSE (the `PublishSessionArchived` event feeds the UI-side
   proxy streams only), so the TTL was the only invalidation.

### Direction fork resolution (issue's "owner's call", resolved by verification)

- **Direction A (persist `time_archived` harness-side) is
  unimplementable against the pinned harness.** The opencode seam
  client's only session mutation is `SessionRename` — `PATCH
  /session/:id` with `{title}` (pkg/agent/opencode/loopback.go:234,
  live-proven POST-ignored/PATCH-honored discipline in its comment);
  there is no harness archive route on 1.18.15, and writing the
  harness's own `opencode.db` from platform code is a Rule 12
  containment violation. If upstream ever exposes archive state,
  direction A becomes the structural endgame (index as projection);
  the issue's own analysis comment reached the same verdict.
- **Direction B (the issue's stated minimum) is implemented here.**

## Fix design (all three halves)

### 1 — `session_list`: degraded listings are stamped, never silent

When the DEFAULT filter cannot prove freshness
(`filterableArchivedSet() == nil` post-filter), every surviving row is
stamped `"archived_set_stale": true` — the unfiltered list is
KNOWN-unfiltered (`stampArchivedSetStale`, mcp_server.go wiring).
`include_archived:true` listings take no stamp (unfiltered by request).
Byte-splice insertion before each element's closing brace — the same
no-re-marshal discipline as the filter (#1735 r2 f1): internal
whitespace, `& < >` unescaped, unknown fields all survive; `{}` becomes
the bare marker; non-array/malformed bodies pass through byte-identical
(stamp must never break the raw list contract).

### 2 — `session_metadata`: top-level staleness marker

`"archived_set_stale": true` at the top level when the archived
annotations (and, on the aggregate, the filter) behind the result
cannot prove freshness; absent = provably fresh. One `filterableArchivedSet()`
capture now serves both the aggregate's match predicate and the marker
(a per-row re-read could race a background refresh and describe a set
the just-served rows didn't use). Single-ID scopes carry it too —
their `archived` annotation rides the same cache.

### 3 — `session_archive`: post-write verification + immediate convergence

After a successful platform write, `forceArchivedSetRefresh(ctx)`
re-fetches the archived set UNGATED (no TTL, no throttle) and:

- updates the shared cache (val + throttle + freshness watermark), so
  THIS pod's listings/annotations converge immediately instead of
  waiting out the ~15s window — the healthy-case half of the incident;
- the reply gains `archived_visibility`: **verified** (fresh set agrees
  with the write) or **unverified** (confirmation read failed or
  disagreed — the state still holds; listings may lag up to one cache
  window).

A failed confirmation never fails the tool (the platform write is the
state truth) and never fabricates cache freshness (failure leaves the
cache untouched — the filter would otherwise trust a set it never
saw). The tool description preempts the incident's failure mode
verbatim: "do NOT re-archive on unverified, check with
session_metadata instead."

### Not changed (verified correct, deliberately untouched)

- Read-only enforcement (409 `session_archived` on user chat sends) is
  PROXY-side per-request (`rejectIfArchived` → Postgres), not pod-cache
  — the issue's "worth confirming" suspicion; confirmed it does not
  ride the fail-open set.
- The fail-open DIRECTION of the filter stays (#1735 r1 f1: stale data
  must not keep hiding rows); what changes is that it is never silent.
- Platform side (`pod_session_archive.go`, workspace svc, session_index
  writes): correct as-is; the defect was the pod-side consumer.

## Red-first evidence

Compile-RED (undefined `stampArchivedSetStale`) then runtime-RED with
the helper stubbed to identity — 7 behavioral pins fail on pre-fix
behavior:

```
--- FAIL: TestSessionList_StaleArchivedSet_StampedUnfiltered
--- FAIL: TestStampArchivedSetStale_PreservesElementBytes
--- FAIL: TestSessionMetadata_StaleSet_TopLevelMarker
--- FAIL: TestMCPSessionArchive_VerifiedRefreshesCache
--- FAIL: TestMCPSessionArchive_UnarchiveVerifiedWhenSetLacksID
--- FAIL: TestMCPSessionArchive_ConfirmationUnreachable_UnverifiedNotFailed
--- FAIL: TestMCPSessionArchive_ConfirmationMismatch_Unverified
```

## Test matrix (all green post-fix)

New pins (`cmd/workspace-agentd/mcp_archive_degradation_test.go`, 11):

- stale-set default listing: unfiltered AND every row stamped; fresh
  set: no marker anywhere; `include_archived:true` + stale: never
  stamped.
- byte-exact stamping (`& < >`, internal whitespace, unknown fields,
  `{}`, malformed/`[]`/non-array pass-throughs).
- metadata: top-level marker stale, absent fresh.
- archive: verified + cache refresh (freshness watermark stamped, id
  present); unarchive verified on absence; unreachable confirmation →
  unverified, tool succeeds, freshness NOT fabricated; fresh-set
  disagreement → unverified (the "not sticking" shape, reported not
  masked).
- catalog description pins (`TestMCPHandler_ToolDescriptionGuidance`:
  new "archive degradation disclosure (#1879)" block) — the markers and
  verdicts are named in every description the model sees.

Updated in-same-commit (their platform fakes answered ALL paths with
204, so the new confirmation GET overwrote their captured requests):
`TestMCPSessionArchive_HappyPath` (+verified assert),
`_Unarchive`, `TestArchiveSession_AcceptsAlias` — route-aware fakes.

Full `cmd/workspace-agentd` suite green (361s, -count=1); gofmt/vet
clean; `pkg/repolint` green (NNNN sentinel is feature-branch-correct;
the post-merge bot numbers it).

## Residuals

- Stale windows remain unobservable in metrics (a fetch-failure counter
  would make degraded windows alertable) — follow-up, not this lane.
- Cross-POD archive visibility still converges only within the ~15s TTL
  (this PR fixes same-pod convergence + disclosure everywhere; a
  platform→agentd event push would be the structural follow-up).
- Direction A (harness-persisted archive state, index as projection)
  stays blocked on an upstream opencode archive API; worth an upstream
  ask.
- Mutation-verify suggestions: (a) delete the
  `filterableArchivedSet() == nil` stamp branch in `mcpSessionList` →
  `TestSessionList_StaleArchivedSet_StampedUnfiltered` fails;
  (b) make `forceArchivedSetRefresh` respect the TTL (call
  `archivedSessionSet` instead) → `TestMCPSessionArchive_VerifiedRefreshesCache`'s
  freshness assert fails; (c) stamp via `json.Marshal` re-encode →
  `TestStampArchivedSetStale_PreservesElementBytes` fails (escaping);
  (d) fabricate freshness on failed confirmation →
  `TestMCPSessionArchive_ConfirmationUnreachable_UnverifiedNotFailed`
  fails.
