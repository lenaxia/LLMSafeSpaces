# Worklog: #1816 — platform MCP surfaces flipped to phrase session IDs (the #1756 §5 remainder)

**Date:** 2026-10-21
**Branch:** `feat/1816-platform-mcp-phrase-ids` (worktree `wt-1816` off `644446fa` = origin/main at start)
**Status:** PR iterating to APPROVED

---

## Objective

Issue #1816: the platform-side MCP surfaces still emit/accept internal
harness session ids everywhere the #1775/#1806 rounds did not reach.
Two concrete surfaces remained:

1. **pkg/mcp** (the external SDK MCP server, `cmd/mcp` — the owner's
   "external MCP"): `session_create` dropped the degraded-window
   marker; `session_message`'s phrase addressing worked only by
   ACCIDENT (the ABI contract stream still carries harness ids until
   the deferred short_id M2 cutover, so a phrase-typed stream filter
   never matched a frame — the wait burned its ENTIRE budget, then
   answered via history fallback; questions never surfaced, permission
   auto-approve never fired); the pending-question result leaked the
   harness id verbatim; deprecated-form inputs carried no §2 notice.
2. **agentd gaps** (the `llmsafespaces_*` handlers the orchestrator
   fleet drives): §2 notice parity was shape-constrained on
   `session_read` (array output) and missing on `session_metadata`
   scopes and `send_message`'s ORIGIN ref; error/warning strings
   embedded raw harness ids where phrases were known (the #1530
   recovery warning, the multiple-busy refusal, suggest clauses); the
   #1530 duplicated-fragment recovery only accepted `ses_`-form
   targets (phrases are the canonical address now); the board manager
   tools' declared-sessionId fallback did not resolve phrase refs.

## What shipped (red-first pins verified failing on main first)

### api/internal/middleware/session_ref.go

- `X-Session-Ref-Resolved: <canonical>` response header on every
  RESOLVED session ref (phrase/alias rewrite; passthrough sets
  nothing — the caller already holds the canonical id). This is the
  resolved-id BRIDGE the platform MCP client consumes to match the
  ABI contract stream without the timeout burn. It is correlation on
  a response the caller already receives under authz — not a new
  session-bearing payload surface (§1 governs bodies); it RETIRES
  with the ABI short_id M2 cutover (phrase-native frames).
  Pins: `TestSessionRef_ResolvedRefCarriesCanonicalHeader` (phrase +
  alias), `TestSessionRef_UnresolvedPassthroughCarriesNoHeader`.

### pkg/mcp (client.go + server.go)

- `SessionResp.DeprecatedSessionID` captured; `session_create`'s
  degraded window emits `session_id_deprecated: true` + a guidance
  content block (agentd `create_session` parity — degraded rows
  surface flagged, never silently wrong).
- `SendMessage` reads `X-Session-Ref-Resolved` off the prompt
  response (`doJSONHeaders`, a doJSON variant returning headers) and
  drives the stream wait/fallback on the CANONICAL id: phrase-targeted
  sends complete from live frames (pinned < 2s where the old path
  burned the whole budget), question events surface, permission
  auto-approve works.
- The pending-question result's `sessionId` is rewritten to the
  CALLER-FACING ref (the phrase on the phrase path) — the harness id
  the ABI frame carries never leaks into tool output.
- `session_message`/`session_history` attach the §2 deprecation
  notice as a SECOND text content block on deprecated-form inputs
  (computed client-side from `shortid.IsDeprecatedInputForm` —
  payload shapes stay byte-compatible; notice is model-visible).
  Tool descriptions document the accepted forms.

### cmd/workspace-agentd (mcp_server.go, mcp_tools.go, session_ref.go, session_suggest.go, board_manager_tools.go)

- §2 envelope parity: `mcpHandler`'s tools/call result (success AND
  error paths) appends the deprecation notice as a second content
  block when the caller typed a deprecated-form `session_id` or
  `from_session_id` (`sessionArgsDeprecationNote`; the
  harness-attested `lsp_injected_session` is deliberately exempt).
  This covers `session_read`'s array shape and `session_metadata`
  uniformly; the object-level notes #1806 pinned are unchanged.
- Emission closure: the #1530 recovery warning, the multiple-busy
  refusal, `suggestSessionID`'s did-you-mean candidates, metadata's
  not-found echo, compact's no-model error, and send_message's
  not-found/resolve errors all render ids through `emitID` (phrase
  when the replica knows it) while RANKING stays on the canonical id
  (#1806 f8 discipline).
- The #1530 duplicated-fragment recovery accepts 3-word phrase forms
  on both halves (`recoverableRefForm`) — phrases are the canonical
  address and the emission slip follows the form the caller sees.
- Board manager tools' declared `sessionId` fallback resolves through
  `resolveSessionRef` (phrases/aliases accepted; the injected
  harness-attested path is unchanged).

### e2e (the issue's acceptance)

- `TestOriginE2E1816_PhraseTargetedSendMessage` +
  `TestOriginE2E1816_PhraseTargetedAbort` +
  `TestOriginE2E1816_DeprecatedFullIDCarriesNotice`
  (`mcp_phrase_e2e_integration_test.go`, //go:build integration):
  through the REAL pinned opencode binary + the real origin-injection
  plugin + the real catalog/dispatch, a send_message addressed by
  phrase DELIVERS to the harness session behind it (transcript-polled)
  and emits the phrase; abort by phrase reaches the real seam and
  emits the phrase; a full-id abort still works (window) with the
  notice. All three PASS with OPENCODE_BINARY set.
- Unit-tier full-chain guard: `TestRealMCP_PhraseTargetedSendMessageAndAbort`
  (real mcpHandler HTTP + dispatch + warm replica) — passes on main
  too (the core flip was already live); now pinned against regression.

## Explicitly out of scope (disclosed)

- The ABI `short_id` fields (Session 13/ActionResult 12) + SSE payload
  value-flip: the deferred M2/SDK-major work — no M2 PR is in flight,
  so no simultaneous cutover to coordinate; the
  `X-Session-Ref-Resolved` bridge is the transitional seam and retires
  when frames go phrase-native.
- `GET /sessions/:id` (GetSession) still serializes the harness id in
  its raw contract shape — feeding the frontend's still-harness-keyed
  URLs (#1775's explicit frontend-coordinated deferral, not silently
  flippable here).
- Board/workflow automation blobs (`assignee_session`, run/ActionResult
  records) carry harness ids as audit/attribution records — the §1
  admin/debug carve-out.

## Test evidence (vs main @ 644446fa, then at head)

- Red-first: all pins listed above verified FAILING on main before
  implementation (envelope notice; recovery warning phrase; phrase
  fragment recovery; busy-error phrases; session_create degraded;
  message/history notice blocks; phrase stream matching — 3s timeout
  burn observed; question leak; middleware header absent).
- Green at head: `go test ./cmd/workspace-agentd/` (full, 359s),
  `go test ./pkg/...` + `./api/...` (-p 1; only the KNOWN pre-existing
  `TestRollbackDrill_UnderLoad` outbox load flake — green in
  isolation, package untouched), `go vet` (incl. `-tags=integration`),
  `golangci-lint run` on every touched package — 0 issues.
- Integration e2e (OPENCODE_BINARY=/opencode/usr/local/bin/opencode):
  the three TestOriginE2E1816 legs PASS.
- Disk note: the shared 15G /tmp PVC hit 99-100% mid-sweep (recurring
  fleet-wide); `-p 1/2` + cache cleanup worked around it.

## Files touched

- api/internal/middleware/session_ref.go (+ header, tests in
  api/internal/server/router_session_ref_test.go)
- pkg/mcp/client.go, pkg/mcp/server.go, pkg/mcp/phrase_ids_test.go
- cmd/workspace-agentd/mcp_server.go, mcp_tools.go, session_ref.go,
  session_suggest.go, board_manager_tools.go,
  mcp_phrase_parity_test.go, mcp_phrase_e2e_integration_test.go,
  mcp_tools_test.go (mcpAutomation signature churn), mcp_board_tools_test.go
  (same), board_manager_e2e_integration_test.go (same)
