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
