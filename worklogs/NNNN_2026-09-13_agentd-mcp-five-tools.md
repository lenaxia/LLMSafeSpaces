# Worklog: agentd MCP tool expansion — rename_session, rename_workspace, call_with_model, create_session, get_datetime

**Date:** 2026-09-13
**Session:** opencode (main dev box). Feature request: five new
self-management tools on the agentd platform MCP (`/v1/mcp`), per the
user's direction.
**Status:** Complete (unit/integration; see Validation)

---

## Objective

Give the in-workspace agent first-class control over its own context
surfaces, mirroring what the user can already do from the UI:

1. **rename_session** — rename a session (typically the agent's own
   current one; past ones via session_list IDs).
2. **rename_workspace** — rename the workspace the pod belongs to.
3. **call_with_model** — one single-shot LLM call with a different
   model (vision, long-context, cheap-draft, second opinion) on a
   throwaway session that is deleted afterward.
4. **create_session** — create a new independent agent session,
   fire-and-forget: prompt in, session_id back immediately, the first
   agent turn runs in the background.
5. **get_datetime** — current time in UTC AND the pod's local timezone.

## Assumptions (stated, then validated — Rule 7)

1. **opencode wire shapes** for session create / rename / message /
   delete are `POST /session` `{title?}` → `{id}`, `POST /session/{id}`
   `{"title"}`, `POST /session/{id}/message` (V1, per-prompt `model` as
   the OBJECT `{modelID, providerID}`), `DELETE /session/{id}`.
   **Validated:** `pkg/agent/opencode/adapter.go` — `CreateSession`,
   `RenameSession`, `Send`/`modelOverride` (the string model form 400s
   on pinned opencode — #909 regression), `DeleteSession`.
2. **The platform session index converges agent-side renames without a
   separate index write.** **Validated:**
   `cmd/workspace-agentd/sessionstate/projection.go:136` — the
   projection reads titles from live opencode sessions (`s.GetTitle()`);
   agent-originated titles (opencode's own auto-titling included) reach
   the index through this path, so the tool rides the same one.
3. **Workspace display names cannot be written from the pod** (they
   live in PostgreSQL behind the API), so rename_workspace needs a
   pod-identity internal endpoint. **Validated:** `workspace_service.go`
   `RenameWorkspace` (owner-gated DB update); state table in README-LLM
   ("Workspace display name | API | PostgreSQL").
4. **The process serving /v1/mcp has WORKSPACE_ID,
   LLMSAFESPACE_API_URL, and the projected SA token** in both
   single-container and sidecar modes. **Validated:**
   `pod_builder.go:101-109,324-326` (main) and `agentd_sidecar.go:103,152,213`
   (sidecar; LLMSAFESPACE_API_URL is the in-cluster svc coordinate —
   exactly what an internal call wants, unlike dev-preview's
   public-origin requirement #1332).
5. **TokenReview auth surface is reusable for a second internal
   endpoint.** **Validated:** `pod_bootstrap.go` — reviewer, audience,
   `parseSAPrincipal`, F4b expiry check; mirrored verbatim.
6. **V1 `POST /session/{id}/message` returns the completed assistant
   message synchronously** (the adapter's `Send` depends on it), so
   fire-and-forget delivery must NOT block on it.
   **Validated:** `sessionstate_wiring.go` Admit (#1313) + adapter
   `Send` decode shape `{info, parts}`.

## Design decisions

- **rename_workspace rides the pod-bootstrap auth pattern**: TokenReview
  → SA-name/namespace match → body workspaceID must equal the
  SA-derived one → owner resolved server-side from the lookup. A pod
  can only ever rename itself; no user identity is involved or needed.
  Name is trimmed, non-empty, ≤255 (the `workspaces.name` varchar).
- **Workspace identity never comes from tool arguments** (mirrors the
  secrets_resync no-identity-input rule): agentd stamps `WORKSPACE_ID`
  from pod env; the API cross-checks it against the token.
- **call_with_model** creates a scratch session titled
  `call_with_model` (identifiable if cleanup fails), sends the V1
  message with the per-prompt model object, extracts text parts, and
  deletes the session on every exit path (deferred, best-effort).
  Model refs follow the adapter's split rules: first-segment provider
  (`a/b/c` → provider `a`, model `b/c`); bare flat IDs are rejected
  pre-call (opencode parses them as provider-with-empty-model).
- **create_session** returns after the session exists; the prompt POST
  runs in a detached goroutine bounded at 30min. Loss semantics are
  documented in the tool description (no retry) rather than papered
  over with a queue — the platform's durable admission path exists for
  user-initiated prompts; this is the agent delegating sub-tasks.
- **Titles ≤200, workspace names ≤255**: the latter is the DB column;
  the former is a tool-side sanity cap on LLM-generated titles.

## Files changed

agentd (in-pod):

- `cmd/workspace-agentd/mcp_tools.go` (new) — the five tools + shared
  scratch-session helpers (`mcpCreateScratchSession`,
  `mcpDeliverSessionPrompt`, `mcpDeleteSession`; the pre-existing
  `workflow_execute.go` helpers of the same intent are lossy — no
  errors, no titles, hardcoded port — and are left untouched).
- `cmd/workspace-agentd/mcp_server.go` — tools/list entries (with
  use/when-not-to-use guidance, the contract per
  `TestMCPHandler_ToolDescriptionGuidance`) + dispatcher cases.
- `cmd/workspace-agentd/mcp_tools_test.go` (new) — per-tool happy/unhappy
  paths incl. wire-shape pins (model object form, title omission,
  delete-on-failure) and the fire-and-forget timing pin (returns
  before the turn completes; delivery observed).
- `cmd/workspace-agentd/mcp_server_test.go` — description-guidance
  subtests for the five tools; PLUS two pre-existing flakes fixed (see
  Findings).

API (platform):

- `api/internal/handlers/pod_workspace_rename.go` (new) —
  `PodWorkspaceRenameHandler` (+`...FromClientset` constructor sharing
  `k8sTokenReviewer`).
- `api/internal/handlers/pod_workspace_rename_test.go` (new) — auth
  matrix: missing/rejected/expired token, SA/namespace/principal
  mismatch, name validation (empty/whitespace/256), trim behavior,
  lookup nil/error, rename error, malformed body.
- `api/internal/server/router.go` — `PodWorkspaceRenameHandler` config
  field + route registration beside pod-bootstrap (same no-JWT zone).
- `api/internal/server/router_openapi_contract_test.go` — fixture wires
  the handler; impl-only allowlist row with rationale.
- `api/internal/app/app.go` — production wiring (k8sClient clientset,
  dbSvc lookup, workspace Service renamer, expected namespace).

## Findings (pre-existing, fixed under Rule 5)

1. **Three dev-preview tests failed under ambient pod env.**
   `TestCallMCPTool_DevPreviewURL_{HappyPath,OriginMode,ButtonMarkerShape}`
   do not clear `LLMSAFESPACE_API_PUBLIC_URL`; running the suite inside
   a workspace pod (whose env carries the public URL) flipped origin
   resolution and broke the asserted `platform.example.com` links.
   Reproduced on the clean tree (this dev box IS such a pod). Fixed by
   clearing the env in those tests — the convention neighboring tests
   in the same file already follow.
2. **`TestOutboxDeliver_V2NoPromotionNeverFalselyCompletes` flaked
   (~15%).** Pass 2's absent-verdict stamps `NextAttemptAt =
   now+backoffFor` (1ms shrunk); the immediately-following pass 3 can
   start within that millisecond and `deliverOne` correctly skips the
   backoff-gated entry (`false`). Reproduced 3/20 isolated and 1-in-3
   full-suite runs on the CLEAN tree. Production semantics are correct
   (backoff gating is the designed behavior); fixed in the test with a
   documented 10ms wait before pass 3. 30/30 green post-fix.

## Adversarial self-review (Rule 11)

- **Index divergence window on rename_session**: the session index
  learns the new title on the next projection pass rather than
  synchronously. Not a defect — agent-originated titles already flow
  exclusively through this path (opencode auto-titling), and the API's
  user-initiated rename remains the synchronous dual-write. Documented
  here; no action.
- **Scratch sessions transiently visible**: a call_with_model session
  exists for the call's duration and may flash in session_list /
  sessionstate projection; deletion removes it (projection reconciles
  from live sessions). Self-healing; documented.
- **rename_workspace has no rate limit** (TokenReview + one DB UPDATE
  per call) — same exposure class as pod-bootstrap (TokenReview + K8s
  API per call), both in-cluster-only. Noted; not addressed in this
  change.
- **app.go type assertion**: if `svc.Workspace` is not the concrete
  `*workspace.Service`, the handler stays nil and the route is not
  registered — defense-in-depth consistent with the file's other
  optional-handler wirings; `services.New` always constructs the
  concrete type.
- **create_session goroutine lifetime**: dies with the process on
  agentd shutdown; the loss is the documented fire-and-forget
  semantics, and the session (already created) survives in opencode.

## Validation

- `go build ./...` — clean.
- `go vet` on all touched packages — clean; `gofmt` applied.
- `go test ./cmd/workspace-agentd/ -timeout 900s` — **ok (224.9s)**,
  full package, includes the two pre-existing flake fixes.
- `go test ./api/internal/handlers/ -timeout 900s` — **ok (94.9s)**,
  full package, plus 30/30 on the formerly-flaky outbox test.
- `go test ./api/internal/server/` — **ok**, including the OpenAPI
  contract test with the new route registered in the fixture.
- Wire-level integration: the agentd tests drive the real
  `mcpHandler` JSON-RPC surface against a live httptest opencode
  (create → message → delete sequences asserted on the wire), and the
  full-stack rename_workspace test drives JSON-RPC → agentd tool →
  real HTTP against a live httptest API asserting the exact internal
  route, bearer credential, and body — the same integration depth the
  existing `/v1/mcp` tests use. Live kind-cluster e2e deferred (no
  cluster leg in this session).
