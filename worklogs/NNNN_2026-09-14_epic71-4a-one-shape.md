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
