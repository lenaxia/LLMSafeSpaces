# Worklog: #1735 — session_list / session_metadata default to non-archived; archived only via explicit include_archived flag

**Date:** 2026-10-18
**Session:** worker lane for #1735 (parallel with lanes 1700/1701/1702/1709/1729)
**Status:** Complete (PR pending review)
**Branch:** `feat/1735-archived-session-filter`

---

## Objective

Close #1735: archived sessions grow unboundedly in long-lived
orchestration workspaces (60+ after a week of lanes) and every listing
ships the full dead set — burning agent context on every send (the
orchestrator's discipline requires a fresh list before each message),
slowing the API, and making ID emission errors MORE likely precisely
because the payload is huge. Archived sessions are read-only history —
make them opt-in, not ambient noise, on BOTH surfaces the issue names:
the agentd MCP tools (session_list, session_metadata) and the underlying
API session-listing endpoint they shadow (GET /api/v1/workspaces/:id/sessions).
Single-ID lookups of archived sessions must keep resolving; the per-entry
`archived` field stays; archive/unarchive flows untouched.

## Design decisions

1. **Param name: `include_archived`** (boolean, default false) — one
   name on every surface: MCP session_list, MCP session_metadata, the
   REST query param, the Go/Python SDKs, the frontend wrapper.
   `session_metadata` already had `all_sessions` (a scope selector);
   `include_archived` reads as its additive filter complement and says
   the direction it flips.
2. **agentd filter is advisory, platform-sourced.** The archived set is
   the same #1627 source the metadata annotation already uses
   (`GET /internal/v1/session-archived`, 15s TTL cache). On ANY
   platform fetch failure the filter degrades to a no-op — listing
   everything rather than failing or silently shrinking (the #1627 §4
   posture: the platform fact must never fail the tool). Pinned.
3. **session_list keeps its raw-shape contract.** The verbatim
   pass-through is that tool's output contract, so the default filter
   drops entries via `json.RawMessage` — surviving elements are
   re-served byte-exact (unknown fields intact), and a body where
   nothing matches the archived set is returned untouched (no
   re-marshal at all). `include_archived:true` short-circuits to the
   verbatim body.
4. **session_metadata filters the AGGREGATE only.** The `match`
   predicate: single-ID scope (explicit session_id, injected
   current-session, or the busy-fallback resolution) is NEVER
   filtered — an archived session still resolves by ID with its
   `archived:true` marker; the scope=="" aggregate (all_sessions)
   defaults to non-archived, `include_archived` adds them back.
5. **API filter is response-shape only.** Router-level, after the
   existing backfill/reconcile/statusz-overlay block — index
   reconciliation still sees every row (ghost-row reaping must not
   depend on a query param), and per-session routes resolve archived
   sessions regardless. Service/mocks signatures untouched.
6. **Not changed (deliberately):** `session_read` (history stays
   addressable), the 409 archived guard, `PUT .../archived`,
   `GET /sessions/active` (statusz ground truth), the standalone
   `pkg/mcp` server (no session-list tool), seam `SessionList` /
   `SessionListRaw` (harness-owned, no archived concept), canary
   scenarios (they list freshly-created live sessions only).

## Implementation

- `cmd/workspace-agentd/mcp_server.go` — session_list +
  session_metadata schemas gain `include_archived` (descriptions
  rewritten: default-exclusion is now part of the agent's only docs);
  dispatcher parses the flag for both; `mcpSessionList` gains the
  param and routes to the filter.
- `cmd/workspace-agentd/mcp_tools.go` — `filterArchivedFromSessionList`
  (byte-exact drop, fail-open); `mcpSessionMetadata` gains the param,
  aggregate-only filtering in `match`.
- `api/internal/server/router.go` — GET /sessions: filter unless
  `?include_archived=true`.
- `sdks/openapi.yaml` — listSessions documents the param + the
  response-shape-only semantics.
- SDKs: Go `SessionsService.List(ctx, wsID, includeArchived ...bool)`
  (variadic matches the file's existing style); Python sync+async
  `list(ws_id, *, include_archived=False)`.

## Caller migration (the issue's grep-and-migrate acceptance)

- **Frontend** (`frontend/src/api/workspaces.ts` + Sidebar + ChatPage
  ×2): the sidebar renders the Archived group from the sessions query,
  so those callers opt in explicitly
  (`getSessions(id, { includeArchived: true })`). The
  SessionActivityProvider's SSE archived/unarchived patches ride the
  same query key — unchanged, now fed by the opted-in fetch.
- **#1627 test pins**: `TestMCPSessionMetadata_ArchivedAnnotation` (+
  its cache second-call) asserts the archived ENTRY is served with its
  marker — migrated to `include_archived:true` with a comment saying
  why. The PlatformUnreachable degrade pin keeps the default (no flag)
  — that IS the pin.
- **Canary scenarios**: verified they only list live freshly-created
  sessions — no migration needed.
- Internal Go callers of the listing service: only the router route
  (checked); reconciliation/engine paths go through the index service
  and seam directly, not the filtered surface.

## Tests (red-first)

Red commit `374096ce` (pre-implementation evidence in its message):
dispatcher-level pins compile against the old surface and fail —
`TestSessionList_DefaultExcludesArchived` and
`TestSessionMetadata_AllSessions_DefaultExcludesArchived` (3 sessions
served, 2 expected, archived `ses_4` present in both); API
`TestListWorkspaceSessions_DefaultExcludesArchived` +
`_ExplicitFalseExcludesArchived` fail the same way; the include flag
was ignored.

Green additions in the impl commit: opt-in directions (both tools),
`TestSessionMetadata_ArchivedSingleIDResolves` (explicit archived id →
1 entry, `archived:true`), handler-level defaults/opt-in,
`TestMCPSessionList_PlatformUnreachable_ListsAll` (advisory fail-open),
`TestMCPSessionList_FilterPreservesElementBytes` (deterministic
cache-seeded unit: survivors byte-exact, unknown fields intact), API
param tests (default / `=true` / explicit `=false`), dispatcher pins
that `include_archived` is actually read on both tools.

Verification runs: agentd package full (`ok, 324s`), `-run 'TestSession|TestMCPSession' -count=5` (only
the PRE-EXISTING `TestSessionStateWatchdog_ReconcilesLedger` count-flake fails — verified failing
on pristine origin/main with `-count=3` via stash; single-count green; not this lane's scope,
recorded here per the standing rules), api server/workspace/sessionindex/handlers all `ok`,
sdks/go build+tests `ok`, python `138 passed`, frontend Sidebar/ChatPage/SessionActivityProvider
suites `177 passed` total.

## Notes for reviewers

- The filter lives agentd-side and router-side, NOT in
  `sessionindex.ListByWorkspace` — the internal consumers of the index
  (ArchivedSet endpoint, reconciliation, routine mirrors) need the full
  row set by construction; changing the shared service signature would
  force mock churn and risk the reaper's correctness for a
  response-shape concern.
- Design 0066 (short session IDs, #1691) remains the structural fix
  for the ID-fragility half of the incident class this issue cites;
  this lane shrinks the payload the fragile IDs ride in.

## Review round 1 (CHANGES_REQUESTED → remediated in one commit set)

Verdict: core semantics correct, red-first independently reproduced,
all head suites green — REQUEST CHANGES on four items, all taken:

1. **Warm-cache outage posture (finding 1, substantive).** The shared
   archived-set cache re-stamps its retry-throttle timestamp on FAILED
   fetches, so during an outage the last-known set kept filtering
   forever — contradicting the documented "degrades to the unfiltered
   list". Fixed behaviorally: a second timestamp
   (`archivedSetCacheFetchedAt`) records only SUCCESSFUL fetches, and
   the #1735 listing filters read it through `filterableArchivedSet` —
   a set whose last successful fetch is past the TTL no-ops the filter
   (bounded staleness: one ~15s window, the same window the #1627
   annotation serves; the annotation itself still serves last-known
   markers, where staleness is cosmetic). Pinned both directions:
   `TestMCPSessionList_WarmCacheOutage_DegradesToUnfiltered` +
   `TestMCPSessionList_WarmCacheWithinTTL_StillFilters`
   (deterministic cache seeding, no platform race). Schema text,
   CHANGELOG updated to the accurate claim.
2. **`docs/api/rest.md` stale** — the session-management table row and
   its paragraph now document the archived-by-default +
   `include_archived=true` semantics and the response-shape-only rule.
3. **TypeScript SDK missing the flag** —
   `sdks/typescript/src/client.ts` `sessions.list(workspaceId, opts?)`
   with `{ includeArchived: true }`, matching Go/Python (the #1699
   all-SDKs-together convention).
4. **Missing test cases (all five added):** frontend opt-in pins
   (Sidebar.archived + ChatPage.archived assert the migrated callers
   pass `(wsId, { includeArchived: true })`); SDK serialization pins
   (Go path-builder test, Python respx URL assertions, TS fetch-URL
   assertions — default no-param, opt-in exact query);
   warm-cache outage pins (item 1); malformed/non-array body
   passthrough (`TestMCPSessionList_MalformedBody_PassesThrough`); API
   all-archived edge (default renders `[]`, not `null`; `=true`
   returns every row).

Also noted by the reviewer (minor, no action possible): the
gocyclo-extraction commit wears a `fix:` prefix where `refactor:` was
accurate — history is pushed; not rewritten. This round's commits use
accurate types.

Mid-lane incident (coordination hazard for the orchestrator): the
local worktree `/workspace/wt-1735` was deleted by an EXTERNAL cleanup
during CI waiting (PVC pressure; several lane directories vanished,
including ACTIVE ones). No work lost — everything was committed and
pushed; worktree recreated from the branch. Also two ENOSPC episodes
on the shared PVC were resolved by cleaning only this lane's own
caches (my npm cache, my Playwright browsers, my go scratch) plus one
clearly-dead probe dir from the already-merged #1722 lane
(`/tmp/probe17221228456251`, Oct 17, worktree prunable).

r1 verification: full `cmd/workspace-agentd` suite ok (328s; one
ENOSPC-induced `TestBootGate` failure disappeared after reclaiming my
own caches — rerun green), `api/internal/server` ok, `sdks/go` ok,
python 139 passed, `sdks/typescript` 88 passed, frontend
Sidebar.archived + ChatPage.archived 9 passed (with the new opt-in
assertions).

## PR #1743 (v2 recovery) — the API_PREFIX mock miss

The orchestrator's close-and-reopen recovery (#1739 → #1743, full
lineage cherry-picked onto current main) re-ran CI, and the fresh run
exposed a miss in the e2e mock migration: the first sweep only patched
`${API}`-prefixed route registrations; six specs route the same
sessions list via `API_PREFIX = "**/api/v1"` and kept the bare-path
pattern — the opted-in `?include_archived=true` fetch bypassed them,
failing 8 specs (input-requests ×2, sidebar-hierarchy ×4,
sidebar-kebab-viewport ×2; the earlier "full local suite green" claim
was made against a stale local file set — this run reproduced the
failures exactly). All 7 remaining registrations gained the `?*`
suffix; the six specs (25 tests) and then the FULL playwright run went
green locally: 207 passed, 13 skipped, 0 failed. Committed as
`test(frontend): #1735 — patch the remaining e2e sessions mocks
(API_PREFIX family)` on the v2 branch.

## Review round 2 on #1743 (CHANGES_REQUESTED → remediated)

One blocking finding, validated by the reviewer's execution: the
"survivors byte-exact" claim was NOT guaranteed — `json.Marshal` on
`[]json.RawMessage` compacts and HTML-escapes (`&`→`\u0026`,
`<`→`\u003c`), rewriting titles that carry those characters, and the
pin's fixture had no escapable characters so it couldn't catch the
gap. Remediation took the make-it-true direction (option a):
`filterArchivedFromSessionList` now splices the survivors by raw-byte
concatenation (`[` + elements joined with `,` + `]`) — no re-marshal,
no compaction, no HTML escaping; only inter-element separators
normalize. The pin's fixture now carries `Fix a & b <script>` plus
non-compact internal whitespace and asserts the exact element bytes
plus the absence of `\u0026`/`\u003c`. The reviewer's non-blocking
observation was also taken: `TestMCPSessionList_HappyPath` resets the
archived-set cache, removing its alphabetical file-order dependence.

Also on the v2 CI: `Test (-short, with coverage)` failed once on
`TestBusyTruthE2E_InStreamReseedReconciles` (usagestream, 30s timing
window, "bridge statuses never reached") — a package this PR never
touches; it passes locally at `-count=3` and full-package, and the
race-detector job on the SAME commit succeeded. Recorded as a
pre-existing CI-runner timing flake, same disclosure class as the
watchdog count-flake.

r2 verification: agentd full suite ok (327s); `api/internal/server`
ok; the #1735 test set green at `-count=2` (only the documented
pre-existing watchdog count-flake fails).
