# Worklog: canary tool-contract drift + MCP surface bugs (#880)

**Date:** 2026-08-16
**Session:** Close #880 — SDK canary tools scenario pinned an obsolete contract (15 tools, three session_*_reply tools collapsed into run_resolve by US-65.7); the same canary run exposed two real MCP-surface bugs.
**Status:** Complete

---

## Work Completed

- `sdks/canary/mcp/main.go`:
  - Tools scenario rewritten per the issue's own fix direction: stable NAMED subset (13 base tools incl. `run_resolve`, documenting the collapse; 5 Epic-64 workflow/trigger tools) + minimum count (≥20, additive-tolerant). Removal detection preserved: a removed named tool fails `tool-present`; a net removal fails the floor. Exact-count pinning is gone — it broke on every additive change (15 → 24 when Epic 64 landed).
  - `ws-create` deep scenario now passes `name` (the API's CreateWorkspace validation rejects empty names with 422 — Epic 46).
  - `credential_create`: DEK-unavailable (403 "encryption key not available; re-authenticate") is asserted as the documented API-key-auth limitation and skips the CRUD body — the canary authenticates with an API key, and credential writes require the per-session DEK that only interactive login unlocks. Previously this surfaced as unexplained failures.
- `pkg/mcp/client.go` `ListCredentials`: decodes the API's actual response shape `{"secrets":[...]}` (SecretsHandler.ListSecrets) — was decoding a bare array, so **every** `credential_list` MCP call failed once the wrapper landed. Real product bug surfaced by the canary, not canary drift.
- `pkg/mcp/server.go` `workspace_create`: `name` schema updated Optional → Required, matching the API validation that guarantees a 422 when omitted — the schema was advising clients into a guaranteed error.
- `sdks/canary/TESTPLAN.md`: stale references updated (collapse noted, min-count assertion).

## Key Decisions

- Min-count + named subset over exact count (the issue's direction): additive tool changes no longer redden CI; removals still fail loudly.
- DEK-unavailable asserted-and-skipped rather than "fixed": the limitation is the documented auth architecture (API keys cannot unlock per-session DEKs); the canary now encodes that knowledge instead of failing on it.
- Twin check: the tools assertion exists only in the Go MCP canary (grepped python/typescript/go — no duplicates); no twins to update.

## Assumptions (validated)

1. 24 tools is the authoritative current set — `pkg/mcp/server.go` AddTools (13) + AddWorkflowTools (11).
2. run_replace collapse intentional — US-65.7, documented in server.go:241-260.
3. `{"secrets": [...]}` wrapper is the current API contract — SecretsHandler.ListSecrets (api/internal/handlers/secrets.go:147-166).

## Tests Run

- `go build ./... && go vet ./pkg/mcp/` — clean; `go test ./pkg/mcp/ -count=1` — green
- `cd sdks/canary/mcp && go build ./...` — clean (canary is a separate module; exercised by the sdk-canary CI job)

## Files Modified

- sdks/canary/mcp/main.go
- sdks/canary/TESTPLAN.md
- pkg/mcp/client.go
- pkg/mcp/server.go
