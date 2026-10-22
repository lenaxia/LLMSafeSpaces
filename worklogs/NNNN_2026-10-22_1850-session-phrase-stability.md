# Worklog: #1850 — create_session workers get re-phrased after spawn; 3-word ids must be persistent

**Date:** 2026-10-22
**Branch:** `fix/1850-session-phrase-stability`
**Status:** PR opening (r1)

---

## Objective

Issue #1850: sessions spawned via the platform `create_session` path
are issued NEW ids at their first turn boundary (3/3 reproductions
2026-10-22: vibrant-trout-loses→stellar-comet,
fallow-tornado-stencils→shiny-moon, thematic-dune-thirsts→swift-lagoon,
each marked `id_deprecated: true`). A `send_message` to the spawn-time
phrase 500s once re-phrased; worker panes stop rendering deprecated-id
sessions. Expected fix per the issue: once generated, a session's
phrase id is IMMUTABLE; if an id migration is truly unavoidable, the
old phrase must keep resolving — deprecation markers must never become
hard 500s.

## Root cause (two independent defects, both firing exactly once,
early, on fresh spawns — the reported signature)

### Defect 1 — the reconcile reap deletes live spawn rows (the re-mint)

The #1340 convergence pass (`runSessionIndexReconciliation`,
cadence 30s, miss threshold 2) diffs `session_index` against the
adapter's harness LIST and deletes guard-admitted rows
(`message_count == 0`). Two facts collide on a fresh
`create_session` spawn:

1. the registration seam's row is born with `message_count = 0` and
   stays 0 for the ENTIRE first turn (message events land only at the
   boundary — a worker turn easily spans two 30s passes);
2. the harness list is not a liveness authority for harness/Act-path
   sessions — #1452 documents sessions live and readable by ID while
   never appearing in the list; the spawn path IS the Act path.

So the row is reaped mid-turn; the first turn-boundary event
re-registers the session (`Register` → `EnsureSessionRegistered`) and
the single-mint seam mints a NEW phrase. The old phrase is gone from
the registry — sends to it degrade to passthrough and fail; the
replica/pulls converge on the new phrase; the churn never repeats
(afterwards `message_count > 0` and the guard holds forever) — exactly
the observed fires-once-early-then-stable.

### Defect 2 — the agentd replica's pull apply wipes raced puts

`sessionShortIDStore.putAll` REPLACED the whole map on every registry
pull (boot / on-miss / emission-warm worker at a 1s floor). A pull
whose snapshot predates the spawn's register round-trip returns
without the fresh row and lands AFTER `create_session`'s
`put(phrase → harness)` — wiping it. Immediately: `session_list`
emits the raw harness id + `id_deprecated: true` (the UI-invisibility
impact), phrase-addressed sends lose replica resolution (the
delivery-boundary failure class), until some later pull re-learns what
the store already knew. This contradicts the replica's own documented
contract ("only ever RECEIVES entries"; phrases are immutable once
minted — rows are only born).

### Non-defects audited and ruled out (documented for reviewers)

- `EnsureSessionRegistered` is idempotent (INSERT … ON CONFLICT DO
  NOTHING + read-back); it cannot re-mint an existing row. The re-mint
  requires the row to be GONE first — defect 1.
- The #1817/#1824 phrase-keyed-frame guards
  (`translateSessionIDToPhrase` double-translation,
  `ContextUsedWithInternal` tombstone guard) are in place;
  `SessionTitle` receives the NESTED `session.updated` object id
  (`s.GetId()`), which `wireEvent` deliberately does NOT translate —
  the harness id, the correct Register key.
- `reapSessionIfGone` (read-path 404 reap) only fires on the adapter's
  definitive not-found verdict — not the spawn class.
- opencode 1.18.15 mints `ses_`-prefixed ids server-side (verified in
  the pinned binary: `SessionID.create` → `"ses_" + <12-hex
  monotonic>`); the 2-word forms in the issue's table are the
  observers' post-hoc identifiers (title/slug class) for sessions
  whose phrase had already broken — not minted ids.

## The fix

1. **Reap by-ID confirmation** (`proxy_session_index.go`): every
   planned reap is confirmed by `probeSessionAliveByID`
   (`adapter.GetSession` — the `SessionExists` precedent):
   - alive-by-ID → veto (`reap_vetoed_live` metric + Info log), miss
     counter reset (presence semantics);
   - not-found verdict → reap, exactly as before
     (`GhostReapedWhenByIDProbeConfirmsGone` keeps the leg honest);
   - indeterminate (transport/5xx, nil session) → defer, counter
     pinned at threshold so the next pass retries
     (`reap_deferred_probe`; the harness-error-never-reaps rule
     applied to the confirmation itself).
2. **Replica pull-apply merge** (`session_shortids.go`): `putAll`
   upserts; a pull never removes entries. A deleted session's retained
   mapping resolves to a dead harness id and fails the
   `SessionExists` probe cleanly with the did-you-mean clause — never
   a hard error (the advisory posture the file's header pins). A pull
   that DOES list a changed entry still wins (upsert, not
   first-writer-wins).

## Red-first evidence (verified failing on unmodified main, commit
`131fbb3b` "test: #1850 red-first")

```
cmd/workspace-agentd:
--- FAIL: TestSessionShortIDs_PullMustNotWipeRacedPut
    session_phrase_stability_1850_test.go:60: stale pull wiped the raced
    put: the spawn phrase no longer resolves (#1850 re-phrase class)
--- FAIL: TestCreateSession_PhraseStableAcrossFirstTurnBoundary
    Error: Not equal: expected: "amber-falcon-glides" actual: "ses_2"
    Messages: the spawned session must emit its ORIGINAL phrase as the id
    (row: map[agent:build id:ses_2 ...])
    session_phrase_stability_1850_test.go:203: stale pull wiped the spawn
    phrase "amber-falcon-glides" — session_list degrades to the harness
    id + id_deprecated

api/internal/handlers:
--- FAIL: TestSessionIndexReconcile_ListInvisibleLiveSessionNotReaped
    (the live-by-ID, list-invisible spawn row is reaped off the list gap)
--- FAIL: TestSessionIndexReconcile_ProbeIndeterminateDefersReap
    Error: Not equal: expected: map[string]int{"ses_maybe":2}
    actual  : map[string]int(nil)
```

All green post-fix; `TestSessionIndexReconcile_GhostReapedWhenByIDProbeConfirmsGone`
pins the true-deletion leg (never neutered).

## Scope / surfaces

- MCP tool surfaces: unchanged (no docs-parity delta). API surface:
  unchanged (no openapi delta). Storage: unchanged (no migration).
  Metrics: two new `session_index_reconcile_outcomes_total` label
  values (`reap_vetoed_live`, `reap_deferred_probe`) — Help updated.
- Test-isolation fix disclosed: `resetShortIDWarmForTest` now resets
  the on-miss pull TTL timestamp (`shortIDLastPull`) —
  `TestResolveSessionRef_ShortIDMissOnMissPull` fails on UNMODIFIED
  main at `-count>1` (verified via stash); pre-existing, surfaced by
  this lane's stress runs.
- The pre-existing `putAll must replace, not merge` pin
  (`TestShortIDStore_PutAllReplaces`) is INVERTED to the merge
  contract (`TestShortIDStore_PutAllMerges`) — the replace semantics
  were themselves the #1850 defect 2, contradicting the store's
  documented receive-only contract.

## Verification (all at branch head)

- `go test ./api/internal/handlers/ -count=1` — ok (82.9s), including
  the pre-existing reconcile matrix re-based onto the probe default
  (not-found) and the three new #1850 pins.
- `go test ./cmd/workspace-agentd/ -count=1` — ok (360.3s full
  package; an earlier full run's single FAIL was the pre-existing
  TTL-isolation flake documented above — green once isolated, and
  green on the full re-run).
- `-count=3` stress over every replica/resolution/list/send/create
  family — ok after the TTL reset fix.
- `go vet ./cmd/workspace-agentd/ ./api/internal/handlers/
  ./api/internal/services/sessionindex/` + `go vet -tags=integration
  ./api/internal/handlers/` — clean.
- `make repolint` — all checks passed.

## Playbook alignment

The session-ID playbook (#1756/#1790, per #1831/#1852): phrase-as-id,
immutability, migrations keep old ids resolving. This fix RESTORES the
immutability invariant (the re-mint class is closed at the reap, not
papered over at resolution) and keeps every degraded path soft
(did-you-mean clauses, never hard 500s). No new id migration is
introduced; no window-exit semantics touched.
