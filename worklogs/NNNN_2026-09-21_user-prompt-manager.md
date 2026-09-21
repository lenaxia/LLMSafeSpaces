# Worklog: #1499 — user-level prompt manager (settings surface + API + storage)

**Date:** 2026-09-21
**Session:** Owner-requested full-stack lane: saved-prompt CRUD with the settings UI; the composer @-recall lane (#1496) consumes the published contract read-only; branch `feat/user-prompt-manager`
**Status:** Complete

---

## Objective

A settings surface where users manage saved prompts (name + content, user-scoped) — the API, storage, and UI behind it, with the endpoint contract published first-hour for the parallel composer lane.

---

## Work Completed

- **Contract first (#1496's gate):** issue #1499 filed with the full endpoint contract in the body + a direct message to the composer worker: `GET /api/v1/me/prompts` → `{"prompts":[…]}` (updatedAt DESC, no v1 pagination), POST/PUT/DELETE with the error codes. One divergence from their proposal flagged and accepted: NAMED envelope (`prompts`, house convention), not `{items}` — their one-file adapter swap confirmed working.
- **Storage:** dedicated `user_prompts` table (migration 000032 + helm mirror via chart-sync): uuid PK, `UNIQUE(user_id, name)`, timestamptz. Validated over a user_settings blob per CRUD semantics (row-per-item natural delete/uniqueness; the settings service is tiered config, not collections — mcp_servers/user_secrets precedent). Content NOT encrypted — not a credential (settings-tier trust).
- **Store:** `pg_user_prompt_store.go` — typed errors (`ErrPromptNotFound`/`ErrPromptNameTaken`), unique-violation → name-taken via the package's `isUniqueViolation`, update/delete zero-rows → not-found. sqlmock SQL-shape pins + real-Postgres integration rows (CRUD round-trip, the unique constraint, cross-user isolation both directions — CI's path-triggered job).
- **Handlers:** `user_prompts.go` (UserPromptsHandler + caller-shaped UserPromptStore interface, the mcpServersHandler pattern): userID from context only; validation name 1–100 runes/no-control-chars/trimmed, content 1–64KiB (attachment-manifest scale argued the ceiling); 409 name-taken, 404 unknown, nothing-to-update 400. 7 handler test rows incl. full CRUD round-trip and the validation matrix.
- **Router + wiring:** `/api/v1/me/prompts` group (nil-guarded registration, the mcp-servers precedent); app.go constructs from the pg store; the openapi contract test's fixture wired (its nil-handler gap was a false green — flagged in the PR body).
- **openapi.yaml:** 4 paths + UserPrompt/CreatePromptRequest/UpdatePromptRequest schemas; both-directions parity green.
- **Frontend:** `api/userPrompts.ts` adapter (typed, the composer lane's second consumer is their own file); `PromptsTab` settings tab (list + inline editor + delete-confirm modal + name-taken friendly error), registered in SettingsPage + router; 5 vitest rows (list envelope, create, conflict, edit-in-place, delete-only-after-confirm); tsc clean.

---

## Key Decisions

- **Dedicated table** over settings blob (validated per Rule-7 against the house owner-scoped CRUD precedents).
- **Named envelope** `{"prompts":[…]}` over `{items:[]}` — house consistency wins over the consumer's first proposal; the divergence was explicitly negotiated and accepted.
- **No org-policy gate in v1** — prompts are personal composer accelerators, not the platform/org prompt-POLICY surface; if an org ever wants to lock them, the mcp `allow_user_*` policy pattern is the precedent to follow.
- **Content ceiling 64KiB** — attachment-manifest scale; a bigger artifact is a file, not a prompt.

### Assumptions stated and validated (Rule 7)

| # | Assumption | Validation |
|---|---|---|
| A1 | No existing user-prompt storage | migrations grep (32 free, no user_prompts) |
| A2 | `/api/v1/me/prompts` route free | router.go grep |
| A3 | The bare "prompts" noun is OWNED by two other surfaces | **learned the hard way — see incidents** |
| A4 | Named envelope acceptable to the composer lane | their confirmation message |

---

## Blockers

None.

---

## Incidents (both mine, both disclosed)

1. **Clobbered `api/internal/handlers/prompts.go`** — main already has a prompts.go (the platform/org/workspace prompt-POLICY handler; `userPromptAllowedFromPolicies` that agent_roles.go depends on). My write tool overwrote it blind; the package broke on an undefined symbol the stash-dance traced back to the clobber. Restored from HEAD; my surface renamed `user_prompts.go`/`UserPromptsHandler`.
2. **Clobbered `frontend/src/api/prompts.ts`** — the SAME collision class hours later (the platform-prompt frontend adapter, consumed by RoleSelector/OrgAgentConfigTab; tsc caught it). Restored; mine renamed `userPrompts.ts`. Lesson enforced in-process: **grep/ls the target path before ANY write tool call in shared namespaces** — the write tool never warns.

---

## Tests Run

- `go test ./api/internal/handlers/` — ok 85.4s (7 new rows + the restored policy-handler suite alongside)
- `go test ./api/internal/services/database/` (sqlmock rows) + `-tags=integration` rows (SKIP locally, CI-executed)
- `go test ./api/internal/server/` — ok (openapi contract both directions)
- `npx vitest run src/components/settings/PromptsTab.test.tsx` — 5/5; `npx tsc --noEmit` clean
- `go build ./api/...` clean; migration mirrored via chart-sync (repolint gate)

---

### Review round 1 (CHANGES_REQUESTED → addressed)

- **Trimmed name PERSISTED (their finding 1):** `validateUserPromptName` now returns the trimmed name and Create/Update store THAT value — the 100-rune ceiling holds on the stored row and `" foo"`/`"foo"` cannot coexist under UNIQUE(user_id, name). Regression row: `NameIsStoredTrimmed` (create with padded name → stored/returned trimmed; padded duplicate → 409).
- **Routes de-nested (their finding 2):** my scripted splice had landed the prompts block INSIDE the MCP-handler guard — the exact blind-splice class the incidents section warns about, now on the router too. Own nil-guarded block, outside any sibling resource's guard; comment states the invariant (a conditional MCP construction must never gate the unconditional prompts handler).
- **updatedAt DESC pinned (their missing-#3):** at the layer that owns it — the store's sqlmock row now names the ORDER BY clause as THE order pin (the handler passes store order through; a handler-level order test would only test its own stub — noted and not written vacuously).
- **Composed rows (their missing integration level):** `router_prompts_test.go` — real router → AuthMiddleware → handler → store stub over HTTP: the 401 gate on all four verbs + full CRUD on the wire (201 trimmed-name envelope, 200 list, 200 partial update, 409 duplicate, 204/404 delete).
- **SDK freshness verified delivered:** `make -C sdks sdk-check` green ("SDK surface is current (spec valid + router parity holds)") — the typed-client generators remain unimplemented house-wide (US-14.3/14.4 placeholders), so spec+parity IS the freshness bar today.



- Adversarial review loop until APPROVED; orchestrator merges.
- The composer lane wires the real endpoint when this lands (their mock swap is one file).

---

## Files Modified

- `api/migrations/000032_user_prompts.{up,down}.sql` + `helm/migrations` mirror
- `pkg/types/agent_prompt.go` — UserPrompt + DTOs (appended; no symbol clash — the existing `UserPrompt` at :45 is a struct FIELD, different namespace)
- `api/internal/services/database/pg_user_prompt_store.go` + `_test.go` + `user_prompts_integration_test.go`
- `api/internal/handlers/user_prompts.go` + `user_prompts_test.go`
- `api/internal/server/router.go` (route + RouterConfig field) + `router_openapi_contract_test.go` (fixture wiring)
- `api/internal/app/app.go` — handler construction
- `sdks/openapi.yaml` — paths + schemas
- `frontend/src/api/userPrompts.ts`, `frontend/src/components/settings/PromptsTab.tsx` + `.test.tsx`, `SettingsPage.tsx`, `router.tsx`
