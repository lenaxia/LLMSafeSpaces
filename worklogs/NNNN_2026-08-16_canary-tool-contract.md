# Worklog: canary tool-contract drift + MCP surface bugs (#880)

**Date:** 2026-08-16
**Session:** Close #880 — SDK canary tools scenario pinned an obsolete contract (15 tools, three session_*_reply tools collapsed into run_resolve by US-65.7); the same canary run exposed two real MCP-surface bugs.
**Status:** Complete (4 review rounds)

---

## Work Completed

- `sdks/canary/mcp/main.go`:
  - Tools scenario rewritten per the issue's own fix direction: stable NAMED subset (13 base tools incl. `run_resolve`, documenting the collapse; 5 Epic-64 workflow/trigger tools) + minimum count (≥24, additive-tolerant; floor restored to the full current registry in round 2). Removal detection preserved: a removed named tool fails `tool-present`; a net removal fails the floor. Exact-count pinning is gone — it broke on every additive change (15 → 24 when Epic 64 landed).
  - `ws-create` deep scenario now passes `name` (the API's CreateWorkspace validation rejects empty names with 422 — Epic 46).
  - `credential_create`: DEK-unavailable (403 "encryption key not available; re-authenticate") is asserted as the documented API-key-auth limitation and skips the CRUD body — the canary authenticates with an API key, and credential writes require the per-session DEK that only interactive login unlocks. Previously this surfaced as unexplained failures.
- `pkg/mcp/client.go` `ListCredentials`: decodes the API's actual response shape `{"secrets":[...]}` (SecretsHandler.ListSecrets) — was decoding a bare array, so **every** `credential_list` MCP call failed once the wrapper landed. Real product bug surfaced by the canary, not canary drift.
- `pkg/mcp/server.go` `workspace_create`: `name` schema updated Optional → Required, matching the API validation that guarantees a 422 when omitted — the schema was advising clients into a guaranteed error.
- `sdks/canary/TESTPLAN.md`: S-MCP-TOOLS row, P1–P18 subset, P14 floor, and S-MCP-CRED DEK-skip note updated. (Round-1 worklog claimed this file was updated when it was not — corrected here with the real edit landed in round 2.)

## Round 2 (review on #905): all blockers

- `.gitignore`: bare `mcp` anchored to `/mcp` — new files under
  `sdks/canary/mcp/` are no longer silently untracked (this ate the
  promised tests in round 1; root `/mcp` intent preserved).
- `ListCredentials` regression tests (wrapper decode + filter, empty,
  API error) — verified red on the bare-array decode, green after.
- `workspace_create` schema pin in the integration test:
  `inputSchema.required` must contain `name` (catches silent revert to
  optional).
- Canary `tools_test.go`: contract extracted to `checkToolContract` +
  `canaryExpectedTools`/`canaryToolFloor`; tests cover
  current-registry-passes, additive-passes, stale-#880-signature-fails
  (both failure classes), missing-named-tool-fails, below-floor-fails.
- Floor restored to 24 (the issue's on-record number; PR-body claim "a
  net removal fails minCount" is now true).
- `pkg/repolint TestCanary_MCPTools_Parity`: parses the canary's subset
  + floor from source, validates against the REAL registry via an
  in-process MCP client — the drift-class guard.
- CI: `Canary module unit tests` step added (separate module; root
  `./...` never covered it); `CANARY_NO_CONTROLLER=1` job env replaces
  phase-inference for the ws-wait-active skip (honest env truth; no
  150s burn; a stuck-Pending workspace in a controller environment
  still fails).
- DEK skip narrowed: only the CRUD positives skip; N1–N4
  handler-validation negatives always run (extracted
  `runCredNegatives`).

## Key Decisions

- Min-count + named subset over exact count (the issue's direction): additive tool changes no longer redden CI; removals still fail loudly.
- DEK-unavailable asserted-and-skipped rather than "fixed": the limitation is the documented auth architecture (API keys cannot unlock per-session DEKs); the canary now encodes that knowledge instead of failing on it.
- Twin check: the tools assertion exists only in the Go MCP canary (grepped python/typescript/go — no duplicates); no twins to update.

## Assumptions (validated)

1. 24 tools is the authoritative current set — `pkg/mcp/server.go` AddTools (13) + AddWorkflowTools (11).
2. run_resolve collapse intentional — US-65.7, documented in server.go:241-260.
3. `{"secrets": [...]}` wrapper is the current API contract — SecretsHandler.ListSecrets (api/internal/handlers/secrets.go:147-166).

## Tests Run (cumulative, rounds 1-2)

- `go build ./... && go vet ./pkg/mcp/` — clean; `go test ./pkg/mcp/ -count=1` (incl. the three new ListCredentials tests + schema pin) — green
- `cd sdks/canary/mcp && go build ./... && go vet ./... && go test ./...` — clean/green (5 contract tests)
- `go test ./pkg/repolint/ -run TestCanary_MCPTools_Parity` — green
- Verified red: ListCredentials_WrapperDecode on the bare-array decode (reverted fix, test failed, restored)

## Files Modified

- sdks/canary/mcp/main.go (+tools_test.go, new)
- sdks/canary/TESTPLAN.md
- pkg/mcp/client.go (+client_test.go additions)
- pkg/mcp/server.go (+integration_test.go schema pin)
- pkg/repolint/canary_mcp_tools_parity_test.go (new)
- .gitignore
- .github/workflows/ci.yml


## Round 4 (reviews 3-4 on #905)

- Round-3 record fixes: TESTPLAN S-MCP-CRED table row split at the P1/P2
  boundary; worklog floor "≥20" (round-1 text) corrected to the round-2
  value 24; Tests Run refreshed with the round-2 suite.
- `104c5909` — the round-2 skip was itself wrong: it asserted
  phase=="Pending", but **Pending is also controller-written**
  (controller phase_active.go); with no controller the field is EMPTY,
  so the skip failed in CI (phase=""). With CANARY_NO_CONTROLLER=1 the
  Active-gated tail now skips on the env flag alone — no phase
  inference at all. This corrects the round-2 bullet above, which
  described the skip as "no phase inference" while still keying on
  Pending.
- Assumption disproof recorded (this belongs in Assumptions, not just
  the commit): **"workspaces stay Pending without a controller" was
  false** — the assumption round 2 was built on, inherited from
  ci.yml's own stale comment. The three stale comments (ci.yml ×2,
  TESTPLAN ×1) corrected at HEAD so the next reader cannot rebuild
  phase inference on them — the drift class #880 exists to close.
- `cbed6ca2`: staticcheck S1011 lint fix (append spread).
