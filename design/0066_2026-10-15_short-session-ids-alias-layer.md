# 0066 — Harness-abstracted short session IDs + keyword alias layer + did-you-mean resolution

**Status:** Design (2026-10-15) — every scheme decision is owner-settled in #1691 (transcribed verbatim in §2); this document elaborates **HOW**, not whether. Implementation is sequenced behind the #1643-adjacent lanes and the current SSE/UX train (§7); this PR is the design deliverable only.
**Date:** 2026-10-15
**Issues:** #1691 (charter) · #1663 (motivating near-miss incident) · #1667 (did-you-mean precedent, shipped) · #1643 (session-state single-owner frame — §4.2 nests inside it) · #1649 (enumeration posture this design must not widen)
**Affects (future implementation):** `api/migrations/`, `session_index` + registration seam, `pkg/abi/llmsafespaces/abi/v1/contract.proto` + `action.proto` (additive only), `pkg/session/contract_gen.go` (generated), `pkg/types/session.go`, `api/internal/server/router.go` session routes, `cmd/workspace-agentd/` (MCP tools, replica store), `frontend/src/abi/…/contract_pb.ts` (generated) + ChatPage/Sidebar URLs, `sdks/openapi.yaml`.

---

## 1. The problem, in half a page

Session IDs in every platform contract today are the harness's native identifiers — opencode's 30-char `ses_…` mixed-case strings (`pkg/abi/llmsafespaces/abi/v1/contract.proto:66`, `Session.id`). That is a leaked namespace: the platform's own contract (design 0049's "zero agent-specific identifiers" discipline, `pkg/session/session.go` package doc) carries an agent-internal fact as its addressing key. The human/agent cost is on record: in the #1663 incident a caller's model re-emitted a live id one character off in two different variants, and the platform's bare `not found` turned a recoverable typo into an hour-long "zombie session" diagnosis. #1667 shipped the mitigation — suggest-only did-you-mean clauses on the agentd MCP tools' 404 branches (`cmd/workspace-agentd/session_suggest.go`) — but the address itself stays 30 chars of case-sensitive entropy that no human can hold, and every additional harness (claude code, codex, pi on the multi-harness roadmap) would add its own hostile alphabet at the same seam.

This design replaces the address with a platform-minted, per-workspace-unique short ID (`oc-ab12cd34`), makes session titles a first-class alias layer, and generalizes #1667's tolerance to every resolution surface. All scheme decisions were settled by the owner in #1691; they are transcribed below and are binding on this document.

## 2. Settled rulings (owner-approved in #1691 — transcribed verbatim)

> 1. LSS mints a short ID at session registration; harness's native ID becomes an internal implementation detail (mapped, never contract-visible long-term).
> 2. Format: harness-type prefix (`oc-`, `cc-`, `cx-`, …) + 8 chars, lowercase+digits. Case-insensitive resolution.
> 3. Per-workspace uniqueness (owner ruling: global NOT necessary) — composite unique index (workspace_id, short_id), check-and-retry at mint (retry fires ~0.002% lifetime in a 10k-session workspace).
> 4. Prefix = immutable mint-time origin metadata (survives any future harness-swap semantics).
> 5. Keywords are the ALIAS layer, never IDs: existing adjective-noun titles become first-class resolvable aliases (search + unambiguous API/CLI acceptance within workspace scope). IDs immutable; titles remain mutable.
> 6. Mint-and-persist (API DB + agentd store mapping; survives reseeds/restarts). No deterministic hashing (preserves coupling in disguise).
> 7. Additive ABI migration: new `shortId` alongside legacy `sessionId`; UI/URLs/SDKs migrate to short IDs; legacy stays a stable alias until a major version. Freeze-gate compliant (additive only).
> 8. Did-you-mean on EVERY resolution surface (API 404s, search, CLI, MCP/agentd tools — generalize #1667): `404` + structured `suggestions:[top-3 {title, shortId, workspace, lastActive}]`, edit-distance + prefix/token match, workspace-scoped first, NEVER auto-resolve ambiguity, authenticated/authorized contexts only (enumeration oracle otherwise).

(Transcribed verbatim from #1691's "Settled rulings" block; this section is the normative source for §4.)

## 3. What exists today (verified in this tree @ 9b1f317c)

| Surface | Fact | Reference |
|---|---|---|
| API session registry | `session_index` table: PK `(workspace_id, session_id)`, columns title/last_message_at/message_count/parent_session_id/last_seen_at/context_used/archived | `api/migrations/000001_initial_schema.up.sql:375-387`, PK `:960-961`, indexes `:1352`, `:1359` |
| Row-creating writes | Five UPSERT shapes all `INSERT … ON CONFLICT (workspace_id, session_id)` — first write registers the session, none is a dedicated registration seam | `api/internal/services/database/database.go:1176` (message), `:1187` (title), `:1237` (parent), `:1247` (last-seen), `:1201`/`:1217` (count/context) |
| Session-state projection | The #1643 single-owner: agentd `sessionstate` authority, reseeded from the harness store via `StoreReader`; volatile by design | `cmd/workspace-agentd/sessionstate/` (e.g. `authority.go:80-84`), wiring `cmd/workspace-agentd/sessionstate_wiring.go:47-51` |
| Session routes (id-taking) | `GET /sessions`, `POST /sessions/new` (`EnsureSession`), `PUT /sessions/:id/title|seen|archived`, `POST /sessions/:id/message` | `api/internal/server/router.go:1570`, `:1610`, `:1624`, `:1659`, `:1676`, `:1787`; `workspace_service.go:1546` |
| Harness id mint | opencode mints `ses_…` on `POST /session`; the platform's adapter surfaces it verbatim; the ABI's own comment names the coupling ("opencode mints the id") | `pkg/agent/opencode/adapter.go:151`; `pkg/abi/llmsafespaces/abi/v1/action.proto:67-74` |
| Peer-session creation | agentd MCP `create_session` returns the harness id in its JSON result | `cmd/workspace-agentd/mcp_tools.go:539-570` |
| Did-you-mean precedent | #1667: bounded Levenshtein ≤2 (`editDistanceBounded`), suggest-only, 404-class-only (`isSessionNotFoundErr`), one bounded inventory fetch, wired into every id-taking agentd tool | `cmd/workspace-agentd/session_suggest.go:39,49,94,112,125` |
| ABI freeze | `abi/FROZEN` armed (baseline `e58cecd9…`): schema is additive-only; `make abi-breaking` (buf) gates CI; `make abi-check` verifies regen freshness of `pkg/abi/v1`, `pkg/session/contract_gen.go`, and the TS contract | `Makefile:329-357`; `abi/FROZEN` |
| ABI Session message | `Session.id = 1 … archived = 12` — next free field number 13 | `pkg/abi/llmsafespaces/abi/v1/contract.proto:65-78` |
| ABI action results | `ActionResult.session_id = 1`, oneof results through `rename_session = 11`, `effect_seq = 7` — next free field number 12 | `pkg/abi/llmsafespaces/abi/v1/action.proto:90-107` |
| Sidebar/listing JSON | `SessionListItem` (id/title/…), `SessionStatusItem`, `EnsureSessionResponse` — the REST shapes that gain `shortId` | `pkg/types/session.go:44-50`, `:61-77`, `:96` |
| Frontend addressing | Route `/chat/:workspaceId/:sessionId`; every navigation carries the harness id | `frontend/src/pages/ChatPage.tsx:179`, `:350`; `frontend/src/components/layout/Sidebar.tsx:118`, `:182` |
| Titles | Titles are caller-owned mutable strings (create/rename routes; fleet convention mints adjective-noun titles — e.g. the #1663 session "Worker: #1648 mobile viewport fix"). The in-tree adjective-noun generator (`frontend/src/lib/names.ts:7-12`) mints **workspace** names, not session titles: the platform itself mints no session title, by design | `api/internal/server/router.go:1624`; `mcp_tools.go:64-90` |
| Structured-error precedent | `bindingErrorResponse` serves field-level `details` maps — the house pattern for structured 4xx payloads | `api/internal/handlers/binding_errors.go:16-30` |
| Agentd persisted-file precedent | dev-preview headers: plain JSON at `/sandbox-runtime/…`, 0600, atomic rename, memory-backed class (survives agentd restarts; rebuilt on boot) | `cmd/workspace-agentd/dev_preview_headers.go:15`, `:39` |

## 4. The design

### 4.1 ID format, registry, and the arithmetic that makes check-and-retry sufficient

Form: **`<prefix><8 chars>`**, e.g. `oc-ab12cd34`. Charset `[0-9a-z]` (36 symbols). Stored lowercase; **resolution is case-insensitive** (the resolver lowercases input before compare — `OC-AB12CD34` resolves). No separator beyond the prefix hyphen.

Prefix registry (ruling 2 + ruling 4):

| Prefix | Harness | Status |
|---|---|---|
| `oc-` | opencode | assigned (the only live adapter, `pkg/agent/opencode/`) |
| `cc-` | claude code | reserved |
| `cx-` | codex | reserved |
| `pi-` | pi | reserved |

The registry is a closed map in the mint package; new harnesses take a prefix at their adapter's landing PR (reserved values above are not reassignable). Ruling 4's semantics: the prefix is **frozen at mint** — it records which harness minted the session, never what harness currently serves the workspace; any future harness-swap machinery must not rewrite it. Resolution never dispatches on the prefix (a `cc-` id resolves identically in a workspace that swapped to another harness): the prefix is inert metadata for humans.

Arithmetic (ruling 3's stated 0.002%): 36⁸ ≈ 2.82×10¹² ≈ 41.4 bits of entropy per workspace. Expected mint-time collisions across a workspace's entire 10,000-session lifetime ≈ C(10⁴, 2)/2.82×10¹² ≈ 1.8×10⁻⁵ ≈ 0.002%. A blind check-and-retry loop (bounded, small backoff) is therefore not an availability risk; no mint coordination is needed. Uniqueness is **per-workspace only** (owner ruling: global not necessary) — the arbiter is the composite unique index `(workspace_id, short_id)`, which is also the correctness backstop when two API replicas race a registration (both insert; exactly one wins per Postgres uniqueness; the loser re-mints and re-inserts).

### 4.2 Mint point and ownership — nested in the #1643 single-owner frame (S1–S4)

#1643 establishes, for session **state** (busy/idle/delivery truth), one owner: the agentd `sessionstate` authority, with the API demoted to consumer. Its tracker issue stages this as S1 (age unification) → S2 (delivery idempotency) → S3 (write-path breaker) → S4 (lease model). (Note for the record: the "design/0064 session-state single-owner" document #1643 references was never pushed to this repository — verified absent from disk, history, and all branches; the frame is taken from the #1643 issue body, which contains the full stage definitions.)

The short-ID mapping is a **new fact of a different kind**: not state (volatile, reseeded, convergent) but **registry/identity** (immutable once minted, must survive pod deletion and projection reseeds). Nesting it into the #1643 frame yields three rules, which this design treats as load-bearing:

1. **One owner for the mapping fact — and it is not the sessionstate authority.** Putting the mint inside the authority would bolt a durable registry onto a component whose defined lifecycle is volatile-by-design (reseeded from the harness store, `sessionstate_wiring.go:47-51`), and #1643's motivating incident is precisely what happens when a session fact lives in five stores. The **single mint owner is the API's registration seam** (the `EnsureSessionRegistered` write path below): the one DB write path that creates `session_index` rows. Every other surface — agentd, frontend, SDKs — reads or replicates; none of them mints. The mapping becomes exactly what the S1–S4 frame demands of every session fact: one writer, N readers.
2. **No sixth parallel store.** The agentd-side artifact (the replica, §4.3) is a *replica* with a defined convergence path (on-miss pull + boot rebuild), the same discipline the #1340 reconcile pass applies to `session_index` itself. It is advisory: any divergence can only degrade resolution to did-you-mean, never corrupt the mapping, because the DB is the only place a short ID can be born.
3. **Registration, not state-watching.** The mint fires when the platform first *registers* a session (creates its index row), which is an event the API already owns. It does not observe busy/idle, does not ride the projection, and therefore cannot regress the S1–S4 convergence work.

**The registration seam.** Today row creation is implicit in whichever UPSERT hits first (§3). The design introduces one explicit DB method, `EnsureSessionRegistered(workspaceID, sessionID, harnessType) (shortID, error)`: `INSERT INTO session_index (workspace_id, session_id, short_id, …) VALUES (…) ON CONFLICT (workspace_id, session_id) DO NOTHING`, with the minted short_id computed in Go (crypto/rand over the 36-symbol alphabet), retried on `(workspace_id, short_id)` uniqueness violation, then a read-back of the surviving row's short_id. Callers:

- **`POST /sessions/new`** (`EnsureSession`) — registers before returning; `EnsureSessionResponse` gains `shortId`.
- **The proxy's row-creating index writes** (message/title/parent/last-seen UPSERT paths, `proxy_lifecycle.go:493` and siblings) — call `EnsureSessionRegistered` first when the (workspace, session) pair is not in a small in-process LRU of known-registered pairs, so the hot per-message path stays write-free after first sight. This is the registration point for sessions created **inside** the harness (TUI, workflow engine) and learned via SSE.
- **agentd MCP `create_session`** — after the harness create succeeds, agentd asks the control plane to register (§4.4) and returns the short id in its result JSON (additive `short_id` beside `session_id`, `mcp_tools.go:565-568`); on control-plane timeout it returns the harness id alone (degraded, self-heals at first observation by the proxy path).
- **The #1340-style convergence sweep** — a periodic pass (amending the existing reconcile walk, `proxy_session_index.go:114`) backfills `short_id IS NULL` rows: the safety net for any registration path that raced, failed, or predates the rollout.

The harness type fed to the mint comes from the workspace's runtime configuration at registration time; today every workspace is opencode (`oc-`), so the multi-prefix paths are exercised only by tests until a second adapter lands.

### 4.3 Persistence mapping (ruling 6: mint-and-persist; no deterministic hashing)

**API DB — the durable truth.** `session_index` gains `short_id text NULL` plus `CREATE UNIQUE INDEX idx_session_index_short ON session_index (workspace_id, short_id) WHERE short_id IS NOT NULL` (partial during backfill; the WHERE clause is dropped — i.e. a plain unique index plus `NOT NULL` — in a follow-up migration once the fleet-wide backfill has converged; two-phase, honest about the transition rather than blocking a migration on a table scan of every fleet row). Existing rows are backfilled by the convergence sweep (§4.2), minting in Go — never in SQL, because a `random()`-derived SQL backfill would either shrink the alphabet (md5-hex = 16 symbols, 32 bits) or embed unreviewable RNG in the migration layer; and **no deterministic derivation of any kind** (ruling 6: hashing the harness id "preserves coupling in disguise" — a hash of a `ses_…` id is still the harness's namespace wearing a mask, and would collide-in-spirit with the goal of making the harness id internal).

Reads/joins inside the API stay keyed on the harness `session_id` (including `session_origins`, metering contexts, D6 alerts, outbox entries): the short id is an *edge alias*, resolved once at the API boundary to the internal key. This keeps the change additive everywhere internal.

**agentd store — the replica.** A plain JSON file at `/sandbox-runtime/session-shortids.json` (the dev-preview-headers pattern: 0600, atomic rename, memory-backed class — `dev_preview_headers.go:15,39`): `{ "oc-ab12cd34": "ses_…", … }` scoped to the workspace pod. Populated by (a) a boot pull from the control plane, (b) an on-miss pull when a tool resolves an unknown short id, (c) push notifications from the API on mint along the existing agentpush path. It survives agentd restarts and sessionstate reseeds (it is not part of the projection); on pod recreation it rebuilds from the boot pull. agentd **never** mints and never writes the mapping to the API — single-owner rule 1 above. Loss of the file degrades resolution to on-miss pulls; loss of the API degrades to harness ids and did-you-mean; nothing is ever unaddressable.

The control-plane surface for (a)/(b) is an additive internal endpoint pair (list-registry / register-one) on the API's existing internal control plane that agentd already speaks (`cmd/workspace-agentd/control_client.go`) — internal transport, not the frozen ABI, so the ABI's role stays what ruling 7 scopes it to: carrying `shortId` as *data* (§4.5).

### 4.4 Resolution semantics: precedence and the alias layer (ruling 5)

Every id-taking surface (API routes, agentd tools, SDKs/CLI) resolves through one precedence:

1. **Exact short ID, case-insensitive** — `lower(input)` equals a registered `short_id` in scope.
2. **Exact legacy harness ID** — byte-equal `session_id`. (Ruling 7: legacy stays a stable alias until a major version; possibly longer — removal is a major-version decision, not a promise of this design.)
3. **Alias (title), workspace-scoped, unambiguous only** — `casefold+trim` equality between the input and exactly one session's current title in the workspace. Two sessions sharing a title is an alias miss → did-you-mean (ruling 8's "NEVER auto-resolve ambiguity"). Prefix/token *subsets* of a title are not auto-resolved — partial matches belong to suggestion ranking (§4.6), never to acceptance.

Ruling 5's semantics made precise:

- **Keywords are never IDs.** An alias resolves a request; it is never persisted as a reference. Anything stored (parent links, origins, outbox, alerts) stores the platform short id or internal harness id — a title resolves at request time against the *current* title holder.
- **Titles remain mutable; IDs are immutable.** A rename changes future alias resolution immediately (the `UpsertTitle` path, `database.go:1187`, is the live view). There is no alias history, no frozen binding: rename two sessions into ambiguity and the previously-unambiguous alias starts returning did-you-mean — correct by construction because aliases are computed, never stored.
- The adjective-noun convention needs no platform change: those titles already live in `session_index.title` and already flow through the alias query. The alias layer is a *view* over existing data, which is why it ships with zero new storage.

### 4.5 ABI + API + UI migration (ruling 7: additive, freeze-gate compliant)

**ABI (additive fields only; the FROZEN gate passes by construction):**

- `contract.proto` `Session`: `string short_id = 13;` (`omitempty` semantics per proto3; unset for the migration window's stragglers).
- `action.proto` `ActionResult`: `optional string short_id = 12;` — set on `create_session` results so peer-session creation hands back the platform address in the same response.
- Everything else in the ABI (`Event.session_id`, `Message.session_id`, action targeting) keeps carrying the harness id **internally** during the migration: the API resolves short→harness at its boundary before any ABI hop. This is what keeps the change additive — no wire field changes meaning.
- Freeze-gate note (ruling 7's "compliant"): `abi/FROZEN` is armed (`Makefile:329-345`); buf's breaking detector permits field *additions* and rejects renumbering/removal — both new fields are tail-additions on frozen messages. `make abi-generate` regenerates `pkg/abi/v1`, `pkg/session/contract_gen.go`, and `frontend/src/abi/llmsafespaces/abi/v1/contract_pb.ts`; `make abi-check` + the CI abi job fail any PR whose schema and generated code drift — including the TS surface (§3, `Makefile:347-357`). The unknown-field-survival property the freeze relies on is already pinned by the `abitest` roundtrip suite for exactly this class of addition.

**REST/JSON (additive, omitempty):** `SessionListItem.shortId`, `SessionStatusItem.shortId`, `EnsureSessionResponse.shortId`; `sdks/openapi.yaml` schema updates ride the same PR per the house contract-test pattern.

**Acceptance:** every id-taking route (`router.go:1624/1659/1676/1787`, history/stream routes, admin force-abort) accepts short, legacy, and unambiguous-alias forms via §4.4 precedence — resolution failure is the 404-with-suggestions of §4.6.

**UI/URLs:** `/chat/:workspaceId/:sessionId` keeps its parameter name (a route is not a contract field) but its value becomes the short id everywhere a session is addressed (`ChatPage.tsx:350`, `Sidebar.tsx:118,182`); deep links carrying legacy ids keep working (precedence 2), which is the entire URL-migration story — old links never break, new links are short.

**End-state (documented, not scheduled):** at a future major version, the ABI's `session_id` fields themselves carry short ids and the harness id retreats fully behind the adapter seam — completing design 0049's "zero agent-specific identifiers" discipline. This design deliberately does not schedule that flip; it only guarantees the additive path reaches it without another breaking step.

### 4.6 Did-you-mean on every resolution surface (ruling 8, generalizing #1667)

**Trigger:** an id-form or alias-form input that fails §4.4 resolution with a 404-class outcome. Authz failures (403), validation failures (400), transport and 5xx never suggest — #1667's `isSessionNotFoundErr` discipline verbatim (`session_suggest.go:112-118`): a suggestion on a real outage would train callers to ignore errors.

**Candidate scoring** (pure function, deterministic, seeded by the failed input and the in-scope candidate set):

- **Id forms** (input matches `^[a-z]{2}-[0-9a-z]{2,10}$`-ish or the legacy shape): bounded Levenshtein ≤ 2 reusing `editDistanceBounded` (`session_suggest.go:125`) over the workspace's registered short ids (and legacy ids, during the migration); distance-2 covers the observed #1663 drop/insert/substitute/transposition variants; ≥3 edits is noise.
- **Titles**: casefolded prefix match and token-overlap match over `session_index.title` in scope.
- **Ranking**: (match class: exact-prefix > token-overlap > edit-distance), then `last_message_at DESC` as the deterministic tiebreak. **Top-3, always.**

**Response shape** (ruling 8 verbatim; the `bindingErrorResponse` structured-payload precedent, `binding_errors.go:16-30`):

```json
404
{
  "error": "session not found",
  "suggestions": [
    { "title": "Swift Heron — #1667 follow-up", "shortId": "oc-ab12cd34",
      "workspace": "7f3e2a41-9c6b-4d2e-a8f1-3b5c9d0e6a72", "lastActive": "2026-10-15T09:14:02Z" }
  ]
}
```

`workspace` carries the workspace id (the field exists because suggestions may cross workspaces when the requester is authorized in several — see scoping); `title` omitted when the session has none.

**Scoping:** workspace-scoped first — the surface's own workspace (the route's, the tool call's), then, only if nothing scores, the requester's *other authorized workspaces* (the session-list surface already exposes exactly that set to exactly that caller; suggestions reveal no row that caller cannot already list). Never global; never for unauthenticated callers.

**Never auto-resolve ambiguity:** one ruling with two edges — unique-alias acceptance (§4.4.3) is *acceptance of an exact match*, not disambiguation; near-matches and multi-candidate ties only ever *suggest*. #1667's suggest-only covenant carries over whole: nothing is delivered, re-addressed, or acted upon on a guess; destructive verbs (abort/delete/archive) refuse exactly as they do today.

**Surfaces, concretely:**

| Surface | Attachment point |
|---|---|
| API REST 404s | the §4.4 resolution helper's failure branch, used by every id-taking route (§4.5) |
| Search/list | the sessions list (and any future search) returns suggestions on empty results when the query looks id/title-like |
| agentd MCP tools | `sessionSuggestClause` generalizes: candidates = replica's short ids + harness ids + titles from `GET /session`; same clause format, same 404-only guard (`session_suggest.go:94`) |
| CLI/SDKs | the structured `suggestions` payload passes through; SDK helpers render it (no client-side reimplementation of scoring — the platform owns the algorithm) |

### 4.7 Security and enumeration (what this design must not widen, per #1649)

- **Entropy**: 41.4 bits per workspace is not enumeration-resistant on its own and does not need to be: every resolution surface sits behind authentication + `WorkspaceAccessMiddleware` workspace authorization, same as today's session routes. The short id removes *incidental* difficulty (a 30-char case-sensitive string) and replaces it with *designed* access control. No unauthenticated surface gains id resolution or suggestions (ruling 8: "authenticated/authorized contexts only").
- **Did-you-mean is not an oracle**: suggestions enumerate only rows the caller is already authorized to list (§4.6 scoping); the payload fields (title/shortId/workspace/lastActive) are a subset of the list surface's fields. Authz denials return 403 with **no** suggestions — a non-member probing `oc-…` guesses learns nothing, member or not.
- **Rate limiting**: resolution endpoints ride the router's existing per-route limits; suggestion computation is bounded by the workspace's session count (the same bound the list endpoint already pays).
- **Prefix** (ruling 4) leaks only mint-time origin — which harness family created the session — and nothing about current workspace configuration.
- **No new cross-tenant signal**: per-workspace uniqueness means the same short id can exist in two workspaces; resolution is always scoped, so cross-workspace existence is never tested by a resolution attempt outside the requester's scope.

## 5. What deliberately does not change

- The harness store: opencode keeps minting and owning `ses_…` ids internally; LSS maps. No upstream asks (the #1577 record stands).
- The frozen ABI's existing fields: meanings, numbers, and the event/action `session_id` wiring during the migration window (§4.5).
- Internal API joins on `session_id` (session_origins, metering contexts, D6 alerts, outbox) — short id resolves at the edge, internal keys unchanged.
- The agentd sessionstate authority: no registry state enters the projection; S1–S4 convergence work is untouched by construction (§4.2 rule 3).
- #1667's guards: bounded inventory fetch, suggest-only, error-prefix stability — generalized, not weakened.

## 6. Test strategy (for the implementation lane; red-first there per house rules)

- **Mint**: charset/length/prefix-lowercase format pins; retry-on-collision pin (fake DB conflicting N times then succeeding); concurrent registration race (two replicas, unique index arbitrates, both callers converge on one short id); idempotence (re-register returns the existing id, never re-mints); backfill convergence (NULL rows minted exactly once by the sweep).
- **Resolution**: case-insensitivity; precedence order (short > legacy > alias); alias unambiguous-accept / ambiguous-reject; rename flips alias resolution live; alias never persisted (row-content pin).
- **Did-you-mean**: response shape pin (exact JSON schema incl. ≤3 suggestions); scoring determinism; workspace-scoped-first (out-of-scope candidate never suggested to a lesser-privileged caller); 403-without-suggestions; 5xx/transport without suggestions; top-3 truncation; the #1663 id variants produce the clause (regression class from `session_suggest_test.go`).
- **ABI/contract**: additive regen freshness (`make abi-check`); unknown-field survival for the two new fields (existing `abitest` roundtrip class); `SessionListItem`/OpenAPI additive-only contract tests.
- **Persistence**: replica rebuild-on-boot (file deleted → boot pull repopulates); on-miss pull; agentd never mints (no write path exists to the API mapping — arity pin).

## 7. Migration and rollout sequencing

Per the issue's deliverable note, **implementation starts only after** the #1643-adjacent lanes (session-state single-owner S1–S4) settle, and after the current SSE/UX train (#1686, #1687; with #1665/#1666 on the same busy-truth surface) clears — the registration seam and reconcile walk touch the same `session_index`/proxy paths those lanes are reworking, and the sequencing rule is the same one #1643's triage recorded: same-surface work lands serially. Within this lane, the shippable phases are:

- **M1 — mint + persist (no behavior change):** migration (nullable column + partial unique index), `EnsureSessionRegistered` seam wired into row-creating paths, convergence backfill sweep, agentd replica + control-plane endpoints. Observably: nothing yet — short ids exist but nothing requires them.
- **M2 — additive surfacing:** `shortId` on ABI Session/ActionResult + REST shapes + SDK/OpenAPI; API routes *accept* short ids (precedence resolution); legacy ids unaffected. ABI additive fields + regen in the same PR (§4.5).
- **M3 — addressing flips human-facing:** frontend URLs/sidebar carry short ids; agentd MCP tools accept short ids + aliases and return `short_id` on create; did-you-mean live on API 404s, list, and agentd tools (the #1667 generalization).
- **M4 — docs + adoption:** operator/user docs (short ids as the canonical address; legacy-as-alias policy; alias semantics), SDK README updates.
- **Phase C (end-state, unscheduled):** ABI internal `session_id` fields carry short ids; harness id fully internal (§4.5). Requires a major version; this design only ensures the additive road reaches it.

Each phase is independently revertible: M1's column is additive; M2/M3 are precedence layers that degrade to today's behavior if the short-id path is disabled; at no point is a session unaddressable by its legacy id.

## 8. Open questions (few — the scheme is settled; these are owner-taste edges, none blocking M1)

1. **Suggestion payload `workspace` field**: workspace id (proposed — cheap, unambiguous, matches the list surface) vs workspace display name (friendlier for multi-workspace orchestrators; needs a join). Default id-only unless the owner wants names.
2. **Prefix registry spellings**: `cc-`/`cx-`/`pi-` are the issue's own examples and are reserved as proposed; the owner may confirm or swap any reserved value before its adapter lands (after first mint under a prefix it is frozen, ruling 4).

## 9. History

Chartered by #1691 (2026-10-15, w30 design lane). All eight scheme rulings were settled by the owner in the issue body before drafting; this document transcribes them (§2) and elaborates the how: mint ownership nested in the #1643 single-owner frame, persistence mapping, additive ABI migration under the armed freeze gate, alias-layer semantics, the did-you-mean specification generalizing #1667, and the rollout sequencing behind the current lanes. Related shipped work: #1667 (suggest layer this generalizes), #1627 (the archived marker — the precedent for additive `session_index` columns). The #1643-referenced session-state design document remains absent from the repository; its frame is cited from the issue body (§4.2).
