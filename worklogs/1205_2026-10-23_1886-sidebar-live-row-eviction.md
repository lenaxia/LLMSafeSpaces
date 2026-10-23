# Worklog: #1886 — sidebar arbitrarily evicts live unarchived sessions past 100 rows

**Date:** 2026-10-23
**Branch:** `fix/1886-sidebar-live-row-eviction`
**Status:** PR r1

---

## Objective

Issue #1886 (owner report, workspace `b150f355`, 2026-10-23 ~05:2xZ):
the sidebar listed 9 unarchived sessions while `session_index` held 16
(104 archived; 121 rows). The missing 8 were all dispatch-spawned lane
workers — the sessions a user most needs to see — and the truncation
was server-side, so a hard refresh re-ran the same cut.

## Root cause

`ListSessionIndex` (api/internal/services/database/database.go) was:

```sql
ORDER BY si.last_message_at DESC NULLS LAST LIMIT 100
```

Two defects compound:

1. **The sort key is missing on ~93% of rows.** Only 6 of 225 live
   index rows had a non-NULL `last_message_at` — the incremental
   write path never records it for board/dispatch-spawned sessions
   (root cause #1880, separate lane). 219 rows formed one NULL tie
   pool with NO secondary sort key: a worker spawned minutes ago
   ranked identically to a session from September.
2. **The cap cut the tie pool arbitrarily.** 6 rows ranked
   meaningfully; the other 94 slots drew from the NULL pool with no
   tiebreaker — ~20 rows fell off, not the oldest, a random ~20.
3. **The badge hid it.** The sidebar fetches `include_archived=true`
   and renders `archived.length` of the response — a page-local count
   (91), not the true total (104). The arithmetic 9+91=100 (= the cap)
   made the truncation invisible.

This is a DIFFERENT defect from #1887/#1882 (delete propagation,
`deleted_at` filter — kept intact; this lane builds on the post-#1887
tree).

## Fix

Per the issue's fix directions (immediate tiebreaker + hardening),
both halves, in `ListSessionIndex` only:

```sql
ORDER BY si.last_message_at DESC NULLS LAST, si.created_at DESC, si.session_id ASC
```

— no LIMIT.

- **Deterministic order:** message-recency head first; the #1880
  NULL-key pool trails by `created_at DESC` (newest-spawned first);
  the unique `session_id` makes the order TOTAL (dispatch cohorts
  spawn in one burst and `now()` is transaction-start, so equal
  `created_at` values are real — the third key resolves them).
- **Cap removed:** the listing returns the workspace's complete
  indexed set. The load-bearing half: a tiebreaker alone still evicts
  a live worker whenever 100+ newer-created rows exist (e.g. a fresh
  archived bulk-import), and the badge stays a page-local count. With
  the full set, every unarchived row renders and `archived.length`
  equals the archived total — ZERO frontend change (the sidebar
  already fetches `include_archived=true` and counts the response).
- `created_at` is `timestamptz NOT NULL DEFAULT now()` (migration
  000001; no later migration touches it) — a never-NULL fallback key.
- Response shape unchanged; OpenAPI documents no cap on
  `GET /workspaces/{id}/sessions`. No SDK/contract churn.

### Side-benefits (same query, other consumers)

The cap silently harmed three other `ListSessionIndex` consumers —
none ever saw rows 101+:

- ref did-you-mean suggestions (`sessionindex/ref.go`)
- the agentd session-registry pull (`pod_session_registry.go`)
- the #1340 ghost-reap reconcile pass (`proxy_session_index.go`)

### Comments corrected

Stale comments documenting the removed cap updated:
`sessionindex/ref.go` ("bounded by the list surface's own LIMIT"),
`sessionindex/service.go` ("ordered by last_message_at DESC"),
`pod_session_registry.go` ("bounded by the list surface"), and
`proxy_session_index.go`'s count-rebuild convergence estimate
("at the 100-row sidebar cap" → "at a 100-row workspace"; math
unchanged, the cap reference dropped).

## Tests (TDD)

### Unit pins — `session_index_listing_test.go` (sqlmock)

- `TestListSessionIndex_DeterministicOrder_NoTrailingLimit` — an
  end-anchored regex (`...si\.session_id ASC\s*$`) pins the full
  tiebreaker chain AND that nothing (a LIMIT) trails it. RED pre-fix
  (the old query ended `NULLS LAST LIMIT 100` → no match).
- `TestListSessionIndex_ReturnsEveryRowPastTheHundredCap` — 125 rows
  through the scan loop, all 125 returned, order preserved (no
  Go-side cap either).
- `TestListSessionIndex_QueryErrorPropagates` /
  `TestListSessionIndex_ScanErrorPropagates` — a failed listing must
  never degrade to an empty 200 ("no sessions" would hide the fleet a
  second way).

### Integration regression — `session_index_listing_integration_test.go`
(`-tags=integration`; CI secrets-integration suite against real
Postgres; skips locally — no server in this lane's sandbox)

The issue's exact repro shape at observation scale: 116 rows — 12
unarchived NULL-key workers (the NEWEST rows: the evicted class), 103
archived NULL-key rows (older), 1 unarchived relay-survivor WITH
`last_message_at`. Asserts:

- **116 items** — the workspace's complete set, no cap (pre-fix this
  fixture yields 100 with ~16 arbitrary evictions).
- membership: every seeded id present.
- badge truth: 13 unarchived, 103 archived.
- exact order: relay-survivor (message-recency head) → worker-01..12
  (`created_at DESC` through the NULL pool) → arch-30..132
  (newest-first archived block, oldest closes).

## Commands + outcomes

```
go test ./api/internal/services/database/ -run 'TestListSessionIndex' -count=1   # RED pre-fix (4 new), GREEN post-fix (all)
go test ./api/internal/services/database/ -count=1                               # ok
go test -tags=integration -count=1 -run TestIntegration_ListSessionIndex_Live ./api/internal/services/database/  # SKIP locally (no pg), runs in CI
go test -count=1 ./api/internal/services/sessionindex/ ./api/internal/handlers/ ./api/internal/services/workspace/  # ok
go test -count=1 ./api/internal/server/                                          # ok
go vet ./api/...                                                                 # clean
go vet -tags=integration ./api/internal/services/database/                       # clean
```

## Assumptions

- The issue's "Hardening" direction ("don't cap the unarchived set
  below the workspace's real session count") is implemented as
  cap-removal for the whole listing — the analysis comment on the
  issue (the prior `/fix` run whose push failed; this lane
  independently implements the same design) chose the same, and it is
  what makes the archived badge truthful with zero frontend change.
- #1880 (populating `last_message_at` for dispatched sessions) is a
  separate lane; once it lands, the NULL pool empties and the
  `created_at` fallback remains as the never-NULL safety net.
- Frontend untouched: the sidebar contract (`SessionListItem[]`) is
  unchanged; only its completeness improves.

## Residuals

- Response size now scales with the workspace's own indexed rows
  (owner-scoped, self-bounded). If a workspace ever grows into the
  tens of thousands, the issue's pagination-with-totals alternative
  is the follow-up (needs frontend pagination UI — unbuilt until a
  consumer justifies it).
- Mutation-verify suggestion: revert ONLY the `si.created_at DESC,
  si.session_id ASC` tiebreaker (keep the cap removed) — the
  integration test's exact-order block must fail while the
  116-count pin still passes, proving the count pin cannot carry the
  ordering guarantee alone. Second: restore only `LIMIT 100` — the
  116-count pin must fail with `should have 116 item(s), but has 100`
  (the production symptom, made deterministic).
