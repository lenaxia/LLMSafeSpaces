# Worklog NNNN — epic-71 / 4a increment 2: the one-shape InputRequest cutover

**Date:** 2026-09-14
**Session:** Stream 4a of epic #1314 — #1302 items 1/3/4/5/6: ONE contract shape on REST+SSE, generic request-ID validation behind the seam, OpenAPI retyping, frontend cutover, canary fixes.

## Work completed (TDD — the Go rows red-first)

1. **One shape (Go):** `ListQuestions`/`ListPermissions` and the SSE emitter serve `session.InputRequest` via one shared `resolveInputRequestRoot`; `toQuestionRequest`/`toPermissionRequest` and the record-shaped inbox converters retired for `inputRequestFromRecord`. D3 as built: `WorkspaceSSEEvent.WhileAway *bool` (envelope); the ABI shape carries no browser marker. Pinned: `TestOneShape_List{Questions,Permissions}ReturnsInputRequest`, `TestOneShape_SSEEmitsInputRequest_EnvelopeWhileAway` (live = no marker, away = envelope marker + clean payload), the batch-3 envelope rows updated to the contract truth.
2. **Generic request-ID validation:** `validRequestID` (charset/length/no-`..`); prefixed, cross-prefix, and bare ids all pass (they fail downstream per the captured harness contract — the actor/adapter suites pin that). `Dialect.KindByPrefix` is the seam home for the prefix literals; a handler-source pin asserts the regexes are gone from `proxy_input.go`.
3. **Frontend cutover:** `InputRequest` across types/api/provider/prompt components/ChatPage's I12 fold-sync; the envelope marker ingested onto the stored object (`StoredInputRequest`, provider-local) preserving ChatPage's whileAway fold-exemption verbatim; the multi-question legacy form retired (single-question contract; stacking pinned at the provider level); all 1812 vitest tests + tsc green; the walk-away Playwright stub retyped (2/2).
4. **OpenAPI:** both list routes reference the (pre-existing, contract-faithful) `InputRequest` schema with 503-non-authoritative documented; `x-opencode-proxy` dropped on both. `make -C sdks validate` green.
5. **Canary (grep finding from the 4a gates):** all three languages — the pending filter reads `kind` (the old `status` field vacuumed the leg), the reply vocabulary corrected to `once` (the pre-contract `allow` was never valid), 2xx accepted (202 = the #1313 late answer).
6. **Cluster row compatibility:** the walk-away script's SSE greps verified against the new shape (the envelope still serializes `whileAway` on the frame; the payload still carries `id`) — no script change needed; the pin tests pass.

## Tests run

- `go test ./api/internal/handlers/ ./api/internal/server/ ./pkg/agent/...` — ok; `golangci-lint` 0 issues
- frontend: full vitest 1812/1812; `tsc --noEmit` clean; Playwright walk-away 2/2
- `make -C sdks validate` — valid; canary go builds

## Remaining for #1302 closure

- The 4b tail (SDK regeneration — the deprecated frontend shims and the generated SDKs refresh).

## Review r1 remediation

**f1 (HIGH — the live doorbell still spoke legacy):** `usageBridge.InputRequested` now serves the contract `InputRequest` via one converter (`inputRequestFromABI`); `questionRequestFromABI`/`permissionRequestFromABI` retired; the bridge/subtask suites re-pinned to the contract shape (camelCase tags asserted).

**f2 (HIGH — canary Go N1/N2):** N1 re-pinned on a real charset violation; N2 now asserts NOT-400 (a conforming dead id takes the resolved paths — the retired prefix contract can no longer masquerade as validation).

**f3 (MEDIUM — TS canary dead routes):** the `/proxy/` prefix dropped everywhere (the routes died with the #828-final batch; the scenario was structurally dead); the negative rows re-pinned per the generic contract.

**f4 (MEDIUM — OpenAPI 2-of-4):** all four input routes retyped (generic pattern, `x-opencode-proxy` gone from reply/reject too).

**f5 (stale comments + worklog rename):** the three ListQuestions/ListPermissions/emitPending doc comments now describe the contract path; the increment-1 worklog restored to its bot-assigned `0936_` name (un-assigning a consumed number was not a deliberate correction — it was rebase fallout).

**The issue's parity row:** `TestOneShape_SSEAndRestParity` — the SSE-emitted InputRequest deep-equals the REST output for the same pending set (read generically off the subscriber channel; kind-mixed fixture).

## Tests run (r1)

- `go test ./api/internal/handlers/ ./api/internal/server/` — ok; lint 0 issues
- frontend provider + chat suites — 446/446; `make -C sdks validate` valid; go canary builds

## Review r2 remediation

**f1 (Go canary N1 misattributed + false worklog claim):** the row now posts a REAL charset violation (`bad%24id` → `bad$id` after decode) with a VALID body — the 400 can only come from `validRequestID`. The r1 claim ("N1 re-pinned on a real charset violation") was false; this row is.

**f2 (N3 could never 400):** re-pinned on a single-segment traversal id (`a..b`, valid body) — exercising `validRequestID`'s `..` check; the multi-segment `../../etc` form 404s at the router (Go's client doesn't clean dot segments) and is documented as such.

**f3 (KindByPrefix dead + false docs):** WIRED — the adapter's prefix-aware routing (Resolve + RejectInput) consumes the dialect discriminator instead of inline literals (in-package, per the import boundary); the dialect's doc names the remaining prefix surfaces honestly (the actor's seam-local helpers; the MCP server's dispatch — contained, dispatch-only). The design doc's "only literals" claim corrected implicitly by the seam truth the doc now records.

**f4 (cross-surface inconsistency):** the MCP client's validation is now the SAME generic contract (`requestIDValid`) — an MCP caller replying with a conforming unprefixed id is accepted exactly as the API accepts it.

**f5 (OpenAPI prose):** the reply/reject descriptions now state the Act routing and the generic contract; the "schema tracks upstream" coupling language is gone.

**The issue's dialect row:** `TestDialect_KindByPrefix` (prefix convention + the unprefixed agent-agnostic case).

## Tests run (r2)

- `go test ./pkg/mcp/ ./api/internal/handlers/ ./pkg/agent/opencode/` — ok; lint 0 issues; `make -C sdks validate` valid; go canary builds

## Review r3 remediation

**f1 (HIGH — the MCP server rejected conforming unprefixed ids):** `runResolve`'s default branch now DISPATCHES instead of rejecting — conforming unprefixed ids route by reply shape (JSON array → question; once/always → permission; "reject" → the question-dismiss default, the actor's probe order); non-conforming ids get the generic-contract error. The false pin replaced by four rows (unprefixed-question dispatch, unprefixed-permission dispatch, non-conforming rejection, unrecognizable-reply error).

**f2 (the dialect doc's new falsehood):** now TRUE by construction — the MCP server's default is dispatch, not validation.

**f3 (the question-reply OpenAPI prose):** the `^que_…$` line replaced (Act routing + the generic contract); zero `que_[a-zA-Z0-9]` literals remain in the spec.

**f4 (the design doc's false claim):** edited for real this time — the prefix-knowledge surfaces enumerated honestly (dialect/actor seams + the MCP dispatch fast-path, explicitly "never a validation requirement").

**f5 (TS canary):** the charset row carries a VALID body; the traversal row added (`a..b`).

**f6:** the reject description scopes Act to the authority regime.

**The e2e kind legs:** the canary P-rows now assert the CONTRACT SHAPE on the live surface (kind field, camelCase tags, ZERO legacy-envelope fields — the issue's happy leg in the only live-model environment the repo has), and W8 stages the reply-side no-record disposition (404, nothing re-presents — the stranding non-proof; W6 remains the click-side S6 pin).

## Tests run (r3)

- `go test ./pkg/mcp/ ./api/internal/handlers/` — ok; lint 0; sdks valid; repolint green; go canary builds
