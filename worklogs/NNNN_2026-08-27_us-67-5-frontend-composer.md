# Worklog: US-67.5 — Frontend composer drawer, upload UX, and manifest strip

**Date:** 2026-08-27
**Session:** Implement the Epic 67 frontend story: composer options drawer (D12), "+" attach button with upload chips (D11/D17), `files[]` send wiring (D11), TS port of the attachment manifest parser for history render, and removal of the dead `MessagePart.files` field (D13). Branch `feat/us-67-5-frontend-composer` (worktree `/workspace/llmsafespaces-us675`, based on main @ `c6501144` which carries US-67.1 + US-67.3).
**Status:** Complete

---

## Objective

Deliver US-67.5 test-plan scenarios U1.6.1–U1.6.21: the composer grows an options drawer (ModelSelector + RoleSelector moved out of the ChatPage header), an always-visible attach button driving multipart uploads through the new `POST /api/v1/workspaces/:id/uploads` route, workspace-scoped attachment chips with upload/error/retry states, send-path `files[]` threading (prompt + busy-queue), manifest strip-on-render for user bubbles, and the dead `MessagePart.files` type removal — TDD-first.

---

## Work Completed

### 1. TS parser port — `frontend/src/lib/attachments.ts` (D11, U1.6.9/U1.6.10/U1.6.19)

Port of `Parse` from `pkg/session/attachments/attachments.go` (US-67.3): strict two-attribute line regex (`path="…"` + `name="…"` with `\\`/`\"` escapes), trailing-block-only consumption, blank-separator drop, trailing-newline tolerance, unknown-version/attribute lines treated as plain text. The port is parse-only — composition stays server-side at send acceptance (D7 compose-once, D11 never-mutate). `ValidatePaths`/`Compose`/`sanitizeName` deliberately NOT ported (no client-side caller; Rule 4).

**Fixture reuse:** `src/lib/attachments.test.ts` imports the Go golden fixtures **directly from `pkg/session/attachments/testdata/`** (all 7 `parse_*` pairs byte-exact + 7 `compose_*.want.json` outputs fed through the parser) — cross-tree vitest imports verified working, so TS and Go can never drift apart short of a fixture change.

### 2. Upload API — `frontend/src/api/uploads.ts` + `client.ts` multipart support

`client.ts` `request()` now omits `Content-Type` for `FormData` bodies (the browser sets the multipart boundary); added `api.upload(path, form)`. `uploadsApi.upload(workspaceId, file)` appends the single `"file"` part and returns `{path, name, size}` — matching the US-67.2 route contract (validated against `feat/us-67-2-api-upload-route`: `uploads.go` field name + `FileUploadResponse`, `router.go` `idGroup.POST("/uploads")`).

### 3. Chip state machine — `frontend/src/hooks/useComposerAttachments.ts` (U1.6.3–U1.6.6, U1.6.13–U1.6.18)

Workspace-scoped (clears on workspaceId change, survives session switch — U1.6.17). Per-upload uuid identity; retry creates a fresh uuid and re-uploads the ORIGINAL `File` (kept in a ref map, never render state — U1.6.18). 10-file client cap mirrored from `maxFiles`, enforced across multi-select batches with a dismissible violation notice (U1.6.6/U1.6.15). Same-file-twice = two chips, two uploads (U1.6.16). Failed chips do NOT set `uploading` — send stays possible (explicit user choice, D17).

### 4. Composer rework — drawer, attach button, chips row (D12/D17)

- **Drawer:** chevron (`>` collapsed / `v` open) with `aria-expanded`/`aria-controls`, keyboard operable (native button), containing the MOVED `ModelSelector` + `RoleSelector` (components reused verbatim; header row in ChatPage deleted, U1.6.1/U1.6.2/U1.6.20). State = user preference `composerDrawerOpen` written through the settings API following the `useUserSetting` read pattern (new module-level `setUserSetting` writer, mirroring the optimistic-cache + PUT contract of `useUserSettings().setSetting` — same pattern `ThemeProvider.setTheme` uses). Enum `auto|open|collapsed`, default `auto` → media-query-aware: open on desktop, collapsed on mobile (U1.6.1).
- **Attach:** always-visible "+" (Paperclip) icon button in the input row; hidden `<input type=file multiple>`; empty selection is a no-op; input value resets after handling so the same file can be re-picked (U1.6.3/U1.6.14/U1.6.15/U1.6.16).
- **Chips:** name + human size (`formatBytes`, extracted to `src/lib/format.ts`, DiskUsageBar deduped onto it), rendered between the drawer row and the textarea. States visually distinct: `uploading` (spinner + pulse), `attached`, `error` (destructive styling + inline error + retry). Remove/retry are keyboard-operable icon buttons with `aria-label`s (U1.6.4/U1.6.20).
- **Send gating (D17):** `canSend` requires non-empty text AND no uploading chip. Payload = `(trimmedText, attachedPaths)` — the text is never mutated client-side (U1.6.7). Error chips excluded from the payload but do not block (U1.6.5).
- **Toasts:** upload-failure notice (once per failed chip id, 4 s auto-dismiss) and cap-violation notice (U1.6.4/U1.6.6), following ModelSelector's inline-toast convention.
- **State hygiene:** text state is untouched by drawer/chip state transitions (U1.6.21 — locked by test).

### 5. Send wiring (D11/D17, U1.6.7/U1.6.8)

`onSend(text, files: string[])` threaded Composer → ChatView → ChatPage. `ChatPage.handleSend` routes: busy/queue path → `queue.enqueue(text, files)`; direct path → `doSendNow(text, files)` → `useChatStream.send(text, cb, model, files)` → `messagesApi.sendAsync` with top-level `files` (omitted when empty). `messagesApi.queueMessage` gains the same field; `QueuedMessage.files` preserves paths through the local re-enqueue fallback. After dispatch, attached chips are cleared (error chips stay for the user's retry/remove choice).

**Two integration details surfaced by adversarial review:**
- `messageIdentityKey` (optimistic-vs-history dedupe, #447 mechanism) now strips the manifest from USER text before keying — otherwise the optimistic raw-prose bubble and the history composed-text bubble key differently and render as duplicates.
- The SSE user-echo strip: with files, the echo returns the COMPOSED text; a remainder that parses as manifest-only is classified as user-echo instead of assistant content (regression-locked in `ChatPage.sse.test.tsx`).
- `extractUserMessageTexts` (Up-arrow history) strips manifests — history candidates are prose, never manifest lines; manifest-only messages are dropped.

### 6. History render (U1.6.9/U1.6.10/U1.6.19)

`MessagePart` user-text branch parses the trailing block, renders stripped prose (omitted entirely when empty — no empty-bubble `<p>`) + read-only `AttachmentChips` (name + file icon). Interior forged lines and assistant text pass through untouched.

### 7. Type cleanup (D13/U1.6.11)

`MessagePart.files` removed from `api/types.ts`; `SendMessageRequest.files?: string[]` added (top-level, per the backend contract). `tsc --noEmit` green; repo grep confirms zero remaining `part.files` consumers.

### 8. Backend: settings schema key for the drawer preference

The user-settings API rejects unknown keys (`pkg/settings/user_service.go:114` `unknown user setting key`), so the persisted drawer state required a schema entry: `composerDrawerOpen` (Tier 3, TypeEnum `auto|open|collapsed`, default `auto`) + `SchemaVersion` 13→14 with the bump comment, canary twins updated in lockstep (Go/Python/TypeScript — enforced by `TestCanary_SchemaVersion_TwinParity`), and a pinning test `TestComposerDrawerOpenEnum` (TDD: written red first).

---

## Assumptions (stated + validation evidence)

| Assumption | Validation |
|---|---|
| Upload route contract is `POST /api/v1/workspaces/:id/uploads`, multipart field `"file"`, 201 `{path,name,size}` | **Validated against the US-67.2 branch** (`git show feat/us-67-2-api-upload-route`: `uploads.go` `uploadFileFieldName = "file"`, `router.go` `idGroup.POST("/uploads"`, agentd `FileUploadResponse{path,name,size}` at `pkg/agentd/types.go:148`). That route is NOT yet on this branch's backend (US-67.2 lands separately); the frontend codes to the contract. Until it merges, uploads in a live stack will 404 → chip error state with retry — the designed failure path (U1.6.4). |
| `/prompt` + `/queue` accept top-level `files[]` on THIS branch | **Confirmed on main:** `proxy_handlers.go:222` (prompt compose) and `:1308/:1354` (`enqueueRequest.Files`) — US-67.3 merged. |
| Drawer state must persist via the settings API (D12) and the API rejects unknown keys | `user_service.go:112-118` — unknown key → error. Hence the schema addition (§8). A stale backend (pre-schema) makes the toggle's PUT fail; the optimistic local cache still applies for the session (swallowed `.catch`), and the repo deploys frontend+backend together. |
| `GetAll` fills schema defaults, so a bool could not express "unset vs collapsed" | `user_service.go:85-103` — every key always present in the response. The **enum** keeps the media-query-aware default (`auto`) distinguishable from explicit `open`/`collapsed`; a bool default would either break mobile-default-collapsed or make explicit collapse impossible. |
| Vitest can import Go testdata fixtures cross-tree | Probe test passed (`src/lib/attachments.test.ts` imports `../../../pkg/session/attachments/testdata/*.json`). |
| Widening `onSend(text, files)` breaks exact-match vitest assertions | Confirmed: 9 assertions across Composer/ChatView/ChatPage.queue/useChatStream/useMessageQueue tests updated to the two-arg form. |
| `formatBytes` duplication vs reuse | DiskUsageBar had a private copy; extracted to `lib/format.ts` with its own tests and both consumers import it (zero behavior change). |
| Playwright e2e cannot run in this sandbox | **Partially disproven — worked around.** No system chromium and no root for `--with-deps`; extracted the 37 required Debian bookworm libs into `/tmp/opencode/libs` via `deb.debian.org` + `LD_LIBRARY_PATH`, after which the headless shell launches. Full e2e suite runs: my 5 new tests pass; the pre-existing suite shows **82 failed both before and after my change** (baseline run on stashed tree — identical failing tests, e.g. all keyboard-text-insertion specs, which their own file headers document as failing in sandboxed Chromium without `--with-deps`, passing in CI). Zero regressions. |
| Pre-existing eslint errors (35 `no-explicit-any` in workflows files) are not mine to fix here | Verified identical on baseline (stash run). All files touched by this story are eslint-clean (scoped `npx eslint <my files>` → zero findings). Out of scope for US-67.5; noted for a dedicated chore. |

---

## Blockers

None. (US-67.2's upload route is not on this branch — see assumption 1; the frontend is contract-complete against it.)

---

## Tests Run (all TDD: written first, verified red, then implemented)

**Unit/component (vitest, `npm test`):** 161 files / **1796 tests passed** (baseline before this work: 155/1712 — net +84 tests, +6 files). New coverage:
- `src/lib/attachments.test.ts` — 21 (golden fixtures + edges)
- `src/lib/format.test.ts` — 4
- `src/api/uploads.test.ts` — 5 (FormData, field name, no JSON content-type, error mapping)
- `src/hooks/useComposerAttachments.test.tsx` — 13 (happy/failure/retry-new-uuid/cap/multi-select/same-file-twice/workspace-switch/session-persist/attached-only/error-no-block)
- `src/components/chat/AttachmentChips.test.tsx` — 3
- `src/components/chat/Composer.attachments.test.tsx` — 23 (drawer toggle+persist+mobile default+explicit override+aria+keyboard, selectors wiring, picker open/cancel/multi/same-file, chips render/error-toast/retry/remove, send gating, payload snapshot, cap toast, state hygiene)
- `MessagePart.test.tsx` +5 (strip+chips, multi-chip, forged-interior, manifest-only, assistant-untouched)
- `composerHistory.test.ts` +3, `ChatPage.sse.test.tsx` +1 (manifest echo), `useChatStream.test.ts` +2, `useMessageQueue.test.ts` +2, `messages.test.ts` +2 (files threading)

**e2e (Playwright, user-space chromium libs — see assumptions):** `tests/e2e/attachments.spec.ts` — **5 passed** (E1 happy path w/ payload assertion, E6 chip-removed→no `files[]`, history-strip, E5 mobile drawer default+persona-selectable, E5 chips-no-overflow at 375px). Full-suite comparison runs (19 min each): baseline 82F/42P/13S vs post-change 82F/47P/13S — identical failure set (documented sandbox limitation), zero regressions.

**Backend (go):** `go test ./pkg/settings/` ok (incl. new `TestComposerDrawerOpenEnum`), `./pkg/repolint/` ok (canary parity), settings-handler run ok, `go build ./...` ok, `gofmt -l` clean.

** Gates:** `tsc --noEmit` ok; `npm run build` ok; scoped eslint on all touched files → zero findings; `./bin/repolint` → all checks passed.

---

## Next Steps

- US-67.6: regenerate SDKs (uploads + files already in openapi on the US-67.2 branch), docs, and the kind-cluster E1–E12 e2e battery (my mocked-browser specs cover the D11/D12/D17 frontend contracts; the stub-agent golden-prompt check is E7's job).
- When US-67.2 merges, run the live-stack upload path once (phase-gate 409 chip error copy is generic today; a phase-specific message mapping would be a small follow-up).

---

## Files Modified

**New (frontend):** `src/lib/attachments.ts`(+test), `src/lib/format.ts`(+test), `src/api/uploads.ts`(+test), `src/hooks/useComposerAttachments.ts`(+test), `src/components/chat/AttachmentChips.tsx`(+test), `src/components/chat/Composer.attachments.test.tsx`, `tests/e2e/attachments.spec.ts`

**Modified (frontend):** `src/api/client.ts` (FormData-aware + `api.upload`), `src/api/messages.ts`(+test) (queueMessage files), `src/api/types.ts` (SendMessageRequest.files; MessagePart.files removed), `src/components/chat/Composer.tsx`(+tests) (drawer/attach/chips/gating), `src/components/chat/ChatView.tsx`(+test) (prop threading), `src/components/chat/MessagePart.tsx`(+test) (user manifest strip), `src/hooks/useChatStream.ts`(+test) (send files), `src/hooks/useMessageQueue.ts`(+test) (enqueue files), `src/hooks/useUserSettings.ts` (module-level `setUserSetting`), `src/lib/composerHistory.ts`(+test) (manifest strip in history candidates), `src/pages/ChatPage.tsx` (hook wiring, header selectors removed, handleSend/doSendNow files, identity-key + echo-strip manifest handling) + `ChatPage.queue.test.tsx`/`ChatPage.sse.test.tsx`, `src/components/workspace/DiskUsageBar.tsx` (shared formatBytes)

**Modified (Go/canary):** `pkg/settings/schema.go` (+`composerDrawerOpen`, SchemaVersion 14), `pkg/settings/schema_test.go` (+pin test), `sdks/canary/{go,python,typescript}` s-user-settings expected-schema-version → 14

**Coordination:** `COORDINATE.md` (completed-work row)
