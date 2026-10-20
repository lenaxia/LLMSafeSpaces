# Worklog: #1691 — Harness-abstracted short session IDs + keyword alias layer + did-you-mean (backend lanes)

**Date:** 2026-10-19
**Lane:** lenaxia/LLMSafeSpaces#1691 (implementation), design 0066 (merged via PR #1693 — normative), motivating incident #1663, precedent #1667
**Status:** Complete — PR open, orchestrator merges on APPROVED

---

## Objective

Design 0066's backend lanes in one PR: the mint-and-persist short-id
registry (session_index + registration seam + backfill), the §4.4
resolution precedence behind every id-taking surface, and the §4.6
did-you-mean generalization of #1667 — across the API session
endpoints and the agentd MCP tools this orchestrator uses. Aliases
resolve, never replace: the canonical wire id stays authoritative.

**Scoped out (sequenced by design 0066 M2/M3, stated in the PR body):**
the ABI `short_id` fields + regen and the frontend URL flip.

## What shipped (3 code commits + docs, red-first per area)

1. `feat(sessionref)` — `pkg/shortid` (ruling-2 format, closed prefix
   registry `oc-/cc-/cx-/pi-`, crypto/rand mint, id-form classifiers)
   and `pkg/sessionref` (the ONE shared §4.6 ranking: bounded
   Levenshtein — moved verbatim from the #1667 layer — over short +
   legacy ids; word-boundary prefix / token overlap over titles +
   slugs; class ordering exact-prefix > token > edit-distance;
   last-active DESC tiebreak; top-3 always). RED verified first
   (panics), then green.
2. `feat(api)` — migration `000037` (+helm sync), `EnsureSessionRegistered`
   (insert-once + 23505 retry + NULL backfill), short-id/alias/cross-
   workspace lookups, the sessionindex `Register` seam behind a
   known-registered LRU wired into all six row-creating UPSERT paths +
   the reconcile backfill (budget 25/pass), the ref middleware on
   every id-taking route, suggestions on the send-404 / session_gone
   410 / archived-404 / admin paths, the `q` filter, EnsureSession
   `shortId`, pod-identity registry endpoints, OpenAPI additive rows.
   RED verified first ("not implemented" across the sqlmock pins).
3. `feat(agentd)` — the replica store (dev-preview-headers pattern),
   `resolveSessionRef` (replica short-id + on-miss pull, legacy
   byte-identical with ZERO extra seam calls, title/slug alias
   unambiguous-only), wiring into all eight id-taking tools, the
   generalized clause (candidates = replica short ids + harness ids +
   titles + slugs), create_session register round trip, session_list
   join, tool-description updates.

## The safety-critical decisions (canonical vs alias boundary)

- **Legacy ids are never rewritten, never gated.** Precedence step 2
  is byte-equal passthrough WITHOUT a DB or seam probe: index lag can
  never unaddress a live session (the #1663 lesson). Pinned: full-id
  byte-identity + zero extra round trips; the one-char-truncated
  incident id passes through untouched everywhere and is met with
  suggestions, never a silent wrong resolution.
- **Id-shaped misses pass through; alias-shaped misses cannot.** A
  charset-valid ref the resolver doesn't know still reaches the seam
  (its 404 + clause is the verdict — no regression for ids outside
  the `ses_` shape; caught live by the fake-fleet `ses-two` pin). A
  ref the seam could never accept (spaces) fails at resolution with
  the did-you-mean — a name was meant.
- **Ambiguity never resolves.** Two live sessions sharing a title or
  slug (the fleet's own `mighty-harbor` ×2, `misty-planet` ×2) refuse
  with BOTH candidates named — pinned at the resolver, the API
  middleware, and the metadata tool.
- **One mint owner.** Short ids are born only in the API's
  `EnsureSessionRegistered`; agentd replicates (boot pull / on-miss /
  register-one) and never writes mappings.
- **Suggestions are scoped.** Workspace first; the owner's other
  workspaces only on an own-workspace shutout, exact-match only, one
  bounded query; no user identity ⇒ no widening; authz/5xx/transport
  never suggest.

## Test evidence (what was actually run, locally)

- `pkg/shortid`, `pkg/sessionref`: green (`go test`, run repeatedly
  during development; the #1663 one-char variants, slug-alias
  acceptance, collision-refusal, determinism, silence-when-nothing-
  scores all pinned).
- `api/internal/services/database` (sqlmock): registration idempotence,
  collision retry (23505 via pgconn.PgError), NULL backfill, case-
  insensitive short-id lookup, ambiguity-visible alias query,
  cross-workspace IN-expansion — RED first, then green.
- `api/internal/services/sessionindex`: precedence + suggestions
  scoping pins, green.
- `api/internal/server`: router-level pins (legacy byte-identical,
  truncated never-resolved, short-id case-insensitive, alias happy
  path, collision 404 with both, suggestions-not-silence, q-filter
  semantics incl. the no-q bare array), green.
- `cmd/workspace-agentd` FULL package: green (exit 0, ~331s), after
  fixing three real finds of its own pins (see below); `pkg/agent/
  opencode` green.
- Integration tests (real Postgres, `-tags=integration`): NOT run
  locally — no PG server in the pod; they run in CI. Honestly noted
  in the PR body. The sqlmock tier pins the SQL shapes; the unique-
  index race is CI's to exercise.
- `golangci-lint --new-from-merge-base=origin/main ./api/... ./pkg/
  ...` and `./cmd/...`: 0 issues (one gocyclo finding fixed by
  extracting the sessions-list handler; the errcheck trap from the
  charter checked — no bare error-channel receives added).

## Findings worth keeping

- The full agentd suite caught a genuine regression my first cut
  introduced: treating EVERY non-`ses_`/non-`oc-` ref as a
  must-resolve alias broke `ses-two`-shaped ids the fake fleet (and
  any odd real id) relies on. The fix (charset-valid ⇒ passthrough)
  is now itself pinned.
- httptest servers with handlers blocked on `r.Context().Done()` can
  outlive client-side cancellation in transport teardown — the
  degraded-platform pin uses a self-limiting timer so the fake is
  always reappable.
- The API contract test's `implOnlyAllowlist` requires entries to be
  REGISTERED by the fixture; nil-handler routes (the session-archive
  precedent) stay out of it and get a registration-only pin instead.
- `TestRollbackDrill_UnderLoad` (outbox) flaked once under parallel
  package load, both with and without this branch (`-count=3` clean
  both ways) — pre-existing, not touched.

---

## r1 — review remediation (CHANGES_REQUESTED @ ed5d211f)

All six findings + the six missing tests, one commit set (`e89a687c`):

1. **Wiring (finding 1)** — the four unwired row-creating shapes
   (last-seen, rename, message-count drift writes, workflow-engine
   preserved fires) now route through the seam; per-path pins at every
   site. "All six paths" is now true, not just claimed.
2. **Cross-workspace byte-equality (finding 2)** — two ref bindings
   (normalized + raw); mixed-case legacy ids match. sqlmock + real-PG
   pins.
3. **Ambiguity payload (finding 3)** — typed ruling-8 rows with the
   route's workspace and uniform lastActive semantics; ORDER BY makes
   the newest-first truncation deterministic.
4. **Top-3 (finding 4)** — the cross-workspace tail is ranked
   deterministically and the whole payload capped.
5. **5xx never suggests (finding 5)** — alias-lookup DB errors degrade
   to passthrough; pinned.
6. **#1340 covenant (finding 6)** — the adapter Send 404 mapping and
   the API send-404 branch are reverted; the covenant comment stands.
   Skeptical-pass metadata on-miss-pull parity included.

Test tiers added: `short_session_ids_integration_test.go` (real
Postgres, `-tags=integration`, runs in CI's database job) — the
same-session race, distinct-session unique-index arbitration, backfill
convergence; plus the budget/backfill pin and the four wiring pins.

Verification at `e89a687c`: `go test ./api/... ./pkg/...` (80 pkgs) +
full `cmd/workspace-agentd` + `go vet -tags=integration` green;
`golangci-lint` on every touched package 0 issues. PR-body claims
corrected (all-six wiring, top-3, integration-in-CI now true; the
send-404 claim withdrawn; the M2/M3 order inversion now commented in
CHANGELOG + PR body).

## r2 — review remediation (CHANGES_REQUESTED @ 9d07b0f8)

The r1 verdict's own review confirmed all six r1 fixes line-by-line and
raised three new validated findings + minor notes; remediated in one
commit set (`832a586b`):

- **A** — cross-workspace sort hoisted above the top-3 branch (runs
  for every payload; DB order no longer load-bearing); ≤3 stale-first
  fixture pinned. Vestigial `own` removed.
- **B** — `DegradedLookup` flag on the alias-error passthrough; the
  middleware 503s seam-invalid refs under the flag (no ghost-row mint,
  no false 404); charset gate shared as `shortid.IsSeamIDForm`
  (middleware + agentd, local pattern copy dropped). Router pins both
  ways.
- **C** — alias ORDER BY anchored in the sqlmock expectation
  (deleting it now fails the pin) + real-Postgres newest-first pin.
- Minor — `sessionref.MaxSuggestions` imported (no local mirror),
  `sessionindex.LastActiveOf` exported (duplicate deleted), CHANGELOG
  records the two tracked follow-ups (agentpush push-on-mint; send
  surface's deliberate bare-502 exception per the #1340 covenant).

Verification at `832a586b`: full `./api/... ./pkg/...` sweep (no
FAILs), full `cmd/workspace-agentd` suite green (~337s), `go vet
-tags=integration` clean, golangci 0 issues on all touched packages.
One infra note: the shared /tmp PVC hit 99% mid-round (test-binary
copy ENOSPC — other lanes' build temp); cleaning MY caches (gocache,
logs, stale testbins) freed 2.6G and unblocked; other lanes' dirs
untouched.

## r3 — documentation-accuracy remediation (CHANGES_REQUESTED @ 09ffbde7)

The r2 verdict mutation-verified the code remediation complete (A/B/C
each proven red/green by direct mutation) and blocked on the release
facing record alone. One commit set:

- CHANGELOG: the degrade sentence now states the r2 SHAPE-dependent
  semantics (charset-valid refs pass through; seam-invalid refs answer
  the new 503) and the 503 response class is documented.
- OpenAPI: a reusable `SessionResolutionUnavailable` 503 response,
  referenced on the title/archived operations and folded into the
  GET-session 503 description (which already documented the
  workspace-inactive 503).
- PR body: head pin refreshed, the resolution paragraph's passthrough
  claim corrected, and the "archived 404" claim reworded to what the
  code does (the route's alias-miss/unindexed 404s carry suggestions;
  the archived READ gate stays the typed 409 session_archived) — with
  a companion pin for the /archived alias-miss 404.
- The skeptical pass's cheap non-blocking nit landed too: a
  table-driven `IsSeamIDForm` unit test (the `_`/128-byte edges).
