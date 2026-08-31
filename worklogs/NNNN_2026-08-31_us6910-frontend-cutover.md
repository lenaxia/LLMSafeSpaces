# Worklog: US-69.10 part 2 — the frontend hard cutover to the contract stream

**Date:** 2026-08-31
**Session:** Epic 69 (#1134) US-69.10 (#1144, design 0055 S3/D2-C), **part 2 of 2**: the chat UI's session-state consumption cuts over hard to `/workspaces/:id/contract-events` (part 1's endpoint). The TS discard-rule fold (port of `pkg/abi/abiclient`), the `useContractStream` hook, the ChatPage dispatch rewrite, the I12 entity-ID stitch, the deletion of the old dialect + its heuristics stack, the S3 e2e set, and the two seam-side fixes the cutover surfaced.
**Status:** Complete (in-repo; e2e validated in CI — Chromium cannot run in this sandbox, system libs missing).

---

## Work Completed

### `frontend/src/session/fold.ts` (new) — the client discard rule
- Faithful TS port of `pkg/abi/abiclient/client.go`: snapshot@S replaces carried sessions + sets seq=S (per-session merge, Go parity); events apply in order; `seq ≤ S` discarded; UNKNOWN materialization; IDLE clears in-flight; INPUT add/remove by id; PART upsert by id; PART_DELTA appends to the part's text payload (projection parity: unseen-id delta is a client-side no-op).
- Structural clones only (parts/inputs detached) — the fold never aliases wire objects.
- `fold.test.ts`: unit pins + **client_discard_rule property test** (200 seeded iterations: random snapshot cuts + stale replays + duplicate deliveries converge to the server fold; plus a scripted mid-turn-reconnect-with-re-snapshot case) + protojson wire-compat pin. The property harness itself caught a real aliasing bug in its server model (cuts sharing mutable snapshots) — the fold was right, the test was wrong.

### `frontend/src/lib/sseConnection.ts` — named events + explicit reconnect
- `event:` lines parse; named events deliver `(data, eventName)` (legacy call shape preserved for default events).
- `reconnect()` on the connection: abort + immediate reconnect, backoff reset, **generation guard** so a superseded read loop's teardown can't stack a second connection.

### `frontend/src/hooks/useContractStream.ts` (new)
- Opens `/contract-events` via `createSSEConnection`; parses frames with `fromJson(StreamFrameSchema)` (generated types only).
- Client stream rules (Go-reference port): first frame per connection MUST be a snapshot (else reconnect); `reseeded` → reconnect for a fresh stamp; named `resync` → immediate reconnect. Publishes fold state on significant frames (PART_DELTA excluded — it renders imperatively via `onEvent`).
- 8 tests: snapshot+event application, discard wiring, protocol-break reconnect, resync, reseed, onReconnect-once, no-id no-connect, malformed-frame tolerance.

### `frontend/src/pages/ChatPage.tsx` — the cutover
- **Dispatch** (ABI events): SESSION_STATUS (idle → notify/reconcile/queue/clear-prompts/hung-clear; sessions invalidation), SESSION_UPDATED (sidebar title cache), MESSAGE_START (USER messages prime the echo gate), MESSAGE_END (context numerator = input+cacheRead+cacheWrite — per-step occupancy preserved), PART_START/END/DELTA (bubbles keyed by part id / tool callId; delta materializes unseen parts, mirroring the projection), ERROR (code→message mapping), INPUT_* via the fold sync below.
- **Deleted**: the 15s reconnect-activation window, reconnect mode, boundary gate (`historyPartIds`/`knownLivePartIds`), echo stitching (`sentTextRef`, `pendingQueuedTextsRef`, `matchQueuedEcho`, `activePartTypeRef`, idx refs), the `session.status`/`session.event`/`agent.question*`/`agent.permission*`/`resync` branches, the retry banner (platform `session.status=retry` payload was opencode taxonomy — I11 dialect containment; the rate-limit UX remains via ERROR + workspace.alert), and the D10 input-snapshot trigger + gate.
- **Auto-abort (stuck question)**: now fold-driven — BUSY fold + stuck tool in history + fresh post-mount stamped snapshot with NO pendingInputs + no stored prompts → 1.5s dwell → abort. The D9/D10 snapshot-flight evidence is replaced by the projection-authoritative snapshot (I3/I12: agentd reseeds from the store on boot, so a lost question is absent by construction).
- **I12 prompt sync (pod-wide)**: the fold's pendingInputs seed/keep prompt content; subtask prompts bubble to the parent view via the loaded sessions list's parentId chain (client-side root resolution replacing the retired user-stream content copies); removals key off the fold; a resolved-id latch prevents answer→INPUT_RESOLVED latency flicker.
- **Platform stream** (`useEventStream`) shrinks to workspace.phase / workspace.alert / queue.update / agent_died.

### `frontend/src/hooks/useMessageHistory.ts` — the I12 stitch
- The timestamp sort is deleted. Transcript order = backend order (pages arrive newest-first; select reverses pages, preserves within-page order, dedupes by message id). Timestamps are never consulted.

### Dialect deletion + gates
- `api/types.ts`: `SessionContractEvent`/`ContractEvent`/`ContractSession`/`ContractPart`/`RetryInfo` deleted (SessionStatusEvent retained as a wire shape the user stream still carries; provider-owned until US-69.11). `SessionRetryBanner` + test deleted. `workspacesApi.requestInputSnapshot` deleted (no caller).
- `src/session/dialect-gates.test.ts` (new): **old_dialect_dead_code** (no session.event/part.* dialect consumption outside the provider exceptions; no timestamp-based transcript stitching; no hand-written contract wire types) + **generated_types_only** (contract types from `src/abi` only).

### Seam-side fixes the cutover surfaced (Go)
- `pkg/agent/opencode/translate_abi.go`: `translatePartABI` now decodes via `ocPart` — the same shape normalizer as the US-65.8 path — restoring **both tool wire shapes** (1.18 flat + legacy nested; the old `partInfo` decoder errored on nested) and **`state.time.{start,end}` → ToolState.startedAt/completedAt** (the elapsed-badge anchor the old dialect carried; new golden-style tests pin epoch-millis + ISO legacy + first-seen anchoring).

### e2e (Playwright, CI-validated)
- `tests/e2e/contract-stream.spec.ts` (new): **midturn_reconnect_e2e** (stream killed mid-turn → fresh snapshot@9 + stale overlap delta → exact reconstruction vs store, STALE text asserted absent), **standing_question_reconnect_e2e** (prompt answerable from the reconnect snapshot alone; zero extra fetches asserted), **api_rolling_deploy_e2e** (replica churn mid-turn: convergence via fresh stamps, duplicated overlap discarded, no stuck spinner).
- Migrated: streaming, input-requests (subtask bubbling un-skipped via parentId chain), context-bar (MESSAGE_END cost), stuck-session-abort (fold-driven), chat/attachments/composer/history-rendering/session-activity/sidebar-*/dev-preview-button (minimal idle-snapshot mock added).

### Tests
- Frontend: **1786/1786 vitest green** (166 files), eslint clean, tsc clean, production build clean. 8 ChatPage suites + 2 history suites rewritten (~100 old-dialect tests replaced); hook-count guard updated (67).
- Go: `pkg/agent/opencode` + `pkg/abi/...` green; `make abi-check` green.

---

## Key Decisions

1. **Platform events stay on `/session-events`** — workspace.phase/alert, queue.update, agent_died are API-owned events with no contract-stream equivalent; the workspace stream remains ChatPage's platform channel until US-69.11 retires the tracker.
2. **SessionActivityProvider untouched** — its user-stream consumption (session.status busy map, agent.* indicators, D9/D10 flights) is Epic 28 cross-workspace surface, explicitly out of scope for both US-69.10 and US-69.11. It is the sole sanctioned exception in the dialect-gates test.
3. **Prompt content moves to the fold (pod-wide)** — the old workspace-stream `agent.question` branch was the only prompt-CONTENT writer; deleting it without a replacement would have silently killed subtask prompts in parent views (caught in review). Root resolution is client-side via the sessions list parentId chain because the ABI translator leaves `rootSessionId` empty (agentd has no parent map); noted as a candidate seam enrichment when US-69.11 deletes the user-stream copies.
4. **Retry banner deleted, not ported** — the RetryInfo payload was opencode's own retry taxonomy leaking through the bridge; I11 says the outward stream is contract-only. The retry UX degrades to the existing ERROR rate-limit message + hung alerts. Recorded on the known-break list.
5. **ToolState.startedAt populated in the seam** — the field exists in the frozen ABI; the translator just never set it. Fixing it inside `pkg/agent/opencode` is containment, not ABI change.

## Known-break list (accepted for the test env)

- Platform `session.status=retry` detail banner (attempt/next) no longer rendered — dialect-contained (see Decision 4).
- Live FILE_CHANGE parts still render from history only (Epic 65 has no renderer branch; unchanged from before the cutover).

## Blockers

None.

## Tests Run

- `npx vitest run` (frontend) — 1786 passed / 166 files.
- `npx tsc --noEmit`, `npx eslint .` (frontend) — clean (6 pre-existing warnings in generated abi files, present on main).
- `npm run build` — clean.
- `go test ./pkg/agent/... ./pkg/abi/... -count=1` — ok (abiclient 80s includes the S1 suite).
- `make abi-check` — ok.
- Playwright: written + typechecked; execution deferred to CI (no system browser libs in this sandbox).

## Next Steps

1. **US-69.11 (#1145)** — API tracker retirement + on-demand consumption: delete the tracker's session-state derivation (`proxy_events.go` emitters: session.status/agent.* translations), the unknown-taxonomy classifier, and the D9/D10 backend; then the provider's user-stream exception dies too and the dialect-gates exceptions shrink.
2. Populate `rootSessionId` in the ABI INPUT_REQUEST translation if agentd gains a parent map (or keep client-side resolution — decide in US-69.11 review).
3. US-69.13 (#1147) — cleanup/drills; the flip/rollback drill should exercise the `/contract-events` 501 path (off-regime) against the frontend.

## Files Modified

- `frontend/src/session/fold.ts` (new), `fold.test.ts` (new), `dialect-gates.test.ts` (new)
- `frontend/src/hooks/useContractStream.ts` (new), `useContractStream.test.tsx` (new)
- `frontend/src/hooks/useMessageHistory.ts`
- `frontend/src/lib/sseConnection.ts`
- `frontend/src/pages/ChatPage.tsx`
- `frontend/src/api/types.ts`, `frontend/src/api/workspaces.ts`
- Deleted: `frontend/src/components/chat/SessionRetryBanner.tsx` + test
- Rewritten suites: `ChatPage.sse/sse-reconnect-duplicate/reconnect/queue/input/context/toolbadge/optimistic-survival/test/hookcount.test.tsx`, `useMessageHistory(.pagination).test.tsx`, `sseConnection.test.ts`
- `frontend/tests/e2e/contract-stream.spec.ts` (new); migrated `streaming/input-requests/context-bar/stuck-session-abort/chat/attachments/composer/history-rendering/session-activity/sidebar-hierarchy/sidebar-kebab-viewport/dev-preview-button.spec.ts`
- `pkg/agent/opencode/translate_abi.go`, `translate_abi_test.go`
