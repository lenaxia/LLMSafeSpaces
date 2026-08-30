# Worklog: Task Model design contract (README-LLM.md §17)

**Date:** 2026-08-27
**Session:** Revive and rewrite the Task Model design section in README-LLM.md against current main, after the DEK-cut prerequisite landed.
**Status:** Complete (docs only — no code)

---

## Objective

Capture the Task Model design contract in README-LLM.md: a platform-side background-LLM feature (workspace naming first, summaries later) distinct from the workspace agent model. An earlier draft (2026-08-10, never merged) was written against a pre-cut tree and stashed; this session re-verified every claim against current main and rewrote it.

## Work Completed

### Design section (README-LLM.md §17, version 1.25)
- **Purpose + first consumer:** server-side OpenAI-compatible LLM calls for platform UX. First consumer is workspace naming, replacing the frontend-only auto-rename hack (`ChatPage.tsx:578-592`, gated on the `adjective-noun-number` placeholder from `names.ts:7`).
- **Precedence chain:** user (`user_settings` key `taskModel`) → org (`org_policies` key `task_model`) → platform default (`instance_settings` key `taskModel.defaultModel`, Tier 2) → workspace model fallback (`workspaces.default_model`).
- **Storage patterns:** Tier 2 instance setting; `org_policies` CHECK-swap migration (template: `000017_allowed_image_configs`); new server-resolved `user_settings` key kept distinct from client-side-seeding `preferredModel` (`schema.go:201`).
- **Org enumeration gap documented:** no org-level model catalog exists; recommended validate-at-write-time via per-credential probe (`org_credentials.go:355`) over a new aggregate endpoint.
- **Call shape:** generalizes the image-factory explainer precedent (`imagefactory_explainer.go:21`) — graceful degradation, never fatal to the chat path.

### Re-verification against current main (post 330-commit drift)
- **Credential prerequisite now SATISFIED:** password-tier DEK cut landed (worklog 0673, migration 000014) and the 0662 cleanup landed (PR #734, worklog 0755, migration 000023). Every credential is server-KEK-wrapped.
- **New gate identified and verified:** sessionless decryption does not exist. `GetDEK` (`key_service.go:492`) resolves Redis → `jwt_sessions` rehydrate → `ErrDEKUnavailable`; `GetDEKForUser` (`:653`) requires an active session row via `ListActiveJWTSessionsForUser`. Neither unwraps `user_keys.wrapped_dek` via `rootKeyProvider`. Documented the v1 scoping fork: (a) session-triggered tasks only (naming qualifies), or (b) new `GetDEKSessionless` + distinct audit-log action label (Epic 58 Q4 precedent).
- **Citation refresh:** ChatPage auto-rename effect moved `:409-423` → `:578-592`; `preferredModel` moved to `schema.go:201`; CHECK-swap template updated from `000012_mcp_servers` to the newer `000017_allowed_image_configs`; confirmed `ProbeModels`, `LLMExplainerConfig`, `filterByOrgPolicy`, `OrgPolicyKey`/`applyPolicyValue` all still present.

## Key Decisions

- **Prerequisite status flipped from "not landed" to "satisfied"; sessionless decryption recorded as the actual remaining gate.** The old draft treated the DEK cut as the blocker; the cut landed, but the practical blocker for offline tasks is that no code path decrypts a user DEK without a live session. Naming is session-triggered, so v1 can ship under scope (a) without new decrypt surface.
- **Platform default (#3) kept in the precedence chain** despite being an addition to the user's original user/org/workspace proposal — operators need a cheap-model pin so background tasks don't default to a premium workspace model.
- **`taskModel` kept separate from `preferredModel`** — the former is the first server-resolved user setting; the latter is client-side seeding only. Overloading would conflate two different consumption semantics.

## Blockers

None for the docs. Implementation is gated on the scope (a)/(b) decision above.

## Tests Run

None — documentation-only change. Citation targets verified by grep/read against `origin/main` (`43ed3f34`).

## Next Steps

1. Decide v1 scope: session-triggered only (a), or build `GetDEKSessionless` (b) first.
2. Implement per the contract: precedence resolver service, three storage keys, write-time probe validation, naming trigger behind the placeholder gate; remove the frontend auto-rename effect when the server path ships.
3. Follow the repo branch → PR → review cycle.

## Files Modified

- `README-LLM.md` — version 1.25; §17 "Task Model" added; ToC entry 17; version-history row
- `worklogs/0860_2026-08-27_task-model-design-doc.md` — this entry (added)
