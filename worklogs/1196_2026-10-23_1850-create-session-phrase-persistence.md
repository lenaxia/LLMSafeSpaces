# Worklog: #1850 — create_session phrase persistence-by-construction (write-once short_id + phrase-bearing row births + reap spawn-window grace)

## The defect (5 reproductions, 2026-10-22/23)

Workers spawned via create_session were RE-PHRASED after spawn: the
3-word phrase returned to the spawner (vibrant-trout-loses,
fallow-tornado-stencils, thematic-dune-thirsts, lofty-hammer-covers,
profound-okra-rushes) stopped resolving after the first turn —
`id_deprecated` on the original id, sends to it 500ing, and the
session's addressable identity collapsing onto its 2-word harness
slug (stellar-comet, shiny-moon, swift-lagoon, eager-comet,
playful-star — the alias layer). Churn fires exactly once, early
(first turn boundary), then the new state is stable — an id-issuance
race signature, not ongoing churn.

## Root cause: lazy settlement (three cooperating holes)

The mint itself was sound — `EnsureSessionRegistered` mints the
phrase in the row-creating INSERT (single statement, durable at
commit) and create_session returns it only after the platform
round-trip. The phrase died AFTER a durable mint:

1. **Six phrase-less row-creating INSERTs.** UpsertSessionMessage /
   Title / ContextUsed / Parent, UpdateSessionLastSeen,
   UpsertSessionMessageCount (database.go) all inserted WITHOUT
   short_id. Any of them landing while the row was absent birthed a
   PHRASE-LESS row; the phrase was then "settled lazily" by the
   reconcile backfill sweep or the next Register — a FRESH mint, not
   the one create_session promised.

2. **The spawn window is reap-admissible.** The #1340 convergence
   reaper (2×30s harness-list misses) and reapSessionIfGone
   (ErrSessionNotFound) delete rows admitted by
   DefaultReapGuard == message_count==0. A create_session worker
   keeps count 0 through its ENTIRE first turn: the prompt delivery
   is a pod-local POST (mcp_tools.go SessionSend → the seam
   directly) that the API proxy never sees, and the usage stream is
   busy-gated on the OUTBOX delivery path (proxy_lifecycle.go:474)
   which create_session bypasses. The guard is blind precisely for
   platform-spawned workers; two list misses inside the window
   (harness store lag is pod-side, pinned opencode) killed the LIVE
   row and orphaned the phrase.

3. **Resurrection re-issues identity.** After row death the next
   message/context/last-seen event re-created the row phrase-less
   (hole 1) and the next Register minted P2. P1 permanently
   unresolvable → send_message 500, id_deprecated, addressing fell
   to the harness slug. Live evidence: this lane's own worker
   session read `id_deprecated: true` in session_metadata at
   mission start.

## The fix — persistence-by-construction (owner direction)

- **Migration 000044 (api + byte-identical helm mirror): short_id is
  WRITE-ONCE.** A BEFORE UPDATE OF short_id trigger refuses any
  change to a minted phrase (different value or NULL); identical
  rewrites and the legacy NULL-backfill (WHERE short_id IS NULL)
  stay legal. INSERTs untouched.
- **Every row-creating upsert births the row WITH its phrase.** All
  six upserts now carry a Go-minted phrase in the INSERT leg (same
  statement = same transaction as the row birth); the ON CONFLICT DO
  UPDATE legs never bind short_id, so an existing phrase always wins
  (idempotent-to-the-original). Phrase collisions arbitrate through
  the same ruling-3 discipline as the registration seam (base drawn
  once, deterministic counter suffix, 23505 re-mint) via
  execWithMintedPhrase (short_session_ids.go).
- **Reap spawn-window grace.** DefaultReapGuard refuses
  message_count==0 rows younger than 15 minutes (reapGraceWindow,
  sessionindex/service.go) — a fresh session is not a ghost;
  harness-list lag must never orphan a minted phrase. Aged count-zero
  rows keep the #1340 ghost-hygiene verdict. ListSessionIndex now
  surfaces si.created_at (types.SessionListItem.CreatedAt, internal
  json:"-") to feed the guard; nil CreatedAt keeps the pre-#1850
  verdict (hand-built rows).
- **Read path unchanged:** the registry/list surfaces already prefer
  the column; the id_deprecated marker remains for legacy rows and
  the degraded window only — new sessions never need it once every
  birth is phrase-bearing.

## Known effect (accepted, deliberate)

The grace window also shelters genuinely stillborn rows (probe
shells, spawn crashes) for up to 15 minutes — they converge one
grace period later. This is NOT a regression of #1340's ghost
hygiene (the reaper's purpose is stale-ghost cleanup, not
instantaneous cleanup), but spawn-hygiene findings should read it
here first. Cross-referenced on #1847 (lease rework — sibling lane)
since DefaultReapGuard brushes its surface.

## Verification (red-first)

- RED captured pre-implementation: phrase-birth SQL pins failed
  against the phrase-less INSERTs (six shapes);
  TestDefaultReapGuard_GraceWindow + the two-miss plan pin failed
  against the count-only guard.
- Unit: api + pkg + cmd full suites green locally
  (go test ./... ×3 trees).
- Integration (rides CI's secrets-integration Postgres; skip-if-
  unreachable per house contract — no local server in this pod):
  write-once trigger matrix (change refused, NULL refused, identical
  rewrite allowed, legacy NULL-backfill allowed), phrase-bearing
  births for all upserts, original-phrase survival under the
  concurrent post-spawn write storm, ListSessionIndex CreatedAt feed.
- repolint: all checks passed (migration sequence + helm mirror
  byte-identical + worklog numbering).

## Review r1 remediation (CHANGES_REQUESTED → this commit)

The automation's verdict: implementation + regression pins sound
(mutation-verified independently by the reviewer), CHANGES on two
wiring-tier test gaps + one minor gap + one truthfulness nit.

1. **Read-path grace pin (the blocking gap)** — the composition in
   reapSessionIfGone (guard sees the listed row's CreatedAt) was the
   only unpinned link: a future row source dropping the CreatedAt feed
   would re-introduce the bug with every suite green. Added
   TestGetSession_Harness404_FreshCountZeroRow_KeptBySpawnGrace
   (fresh count-zero row + harness 404 → typed 410, KEPT body, row
   SURVIVES) + the aged twin (AgedCountZeroRow_StillReaped: the grace
   defers, never pardons).
2. **Wiring-tier phrase-stability pin** —
   TestIntegrationSessionRegistry_PostSpawnWriteStormKeepsCreationPhrase:
   register through the REAL pod-registry handler (the creation mint
   create_session consumes), drive the post-spawn storm through the
   REAL session-index service (RecordMessage+drain, title, context,
   last-seen), assert the CREATION phrase survives on all three
   surfaces (registry pull / sidebar row / phrase resolution).
3. **Tier gap** — UpsertSessionMessageCount + UpsertSessionParent got
   integration birth pins (all six birth paths now covered at both
   tiers).
4. **Truthfulness split** — the reconcile pass's kept population was
   two populations under one metric/log ("history-bearing" is untrue
   for a grace-sheltered count-0 row): kept_spawn_window outcome +
   truthful Warn for grace rows; kept_for_operator unchanged for
   history-bearing rows.

