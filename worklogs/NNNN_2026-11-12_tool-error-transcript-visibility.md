# Worklog: Failed Tool Executions Render Their Error in the Transcript

**Date:** 2026-11-12
**Session:** tsk-copper-archer-rushes (epic "Frontend reliability mopup", owner report 2026-11-12)
**Status:** Review round 1 (CHANGES_REQUESTED) remediated — e2e gate added, re-pushed

---

## Objective

When a tool call fails, the chat transcript must show WHY, not just a ✗ marker + invocation parameters. Owner repros: `llmsafespaces_board_import_plan` → HTTP 409 `epic_state`; `llmsafespaces_board_epic_activate` → invalid-reference error. Frontend scope only; do not regress the success path.

---

## Root cause (validated on the wire, not guessed)

The runtime sends `ToolState.error` on BOTH wires — the live SSE contract stream (`abi.ToolState.error`, contract_pb.ts field 2) and the history adapter (`partFromABI` in `proxy_sessions_act.go` copies `Error: st.GetError()`; `pkg/session.ToolStatusError = "error"`). The frontend dropped it at both decode seams:

- `ChatPage.partToStreamPart` mapped `state.status` but never `state.error`;
- `messages.ts transformHistory` same;
- and any output that DID arrive rendered behind TWO collapsed toggles (the tool pane's `LazyDetails` — whose children do not even MOUNT until opened — plus the inner "Output (N chars)" `<details>`). The 1eee7f28 dev-preview lesson: content behind a collapsed pane effectively does not exist.

## Work completed

### 1. Plumbing (both decode paths + the streaming partition)

- `api/types.ts`: `MessagePart.toolError?: string` — the runtime's `ToolState.error`.
- `ChatPage.tsx`: `StreamPart.toolError`; `partToStreamPart` threads `tool.state?.error`.
- `ChatView.tsx`: `StreamingPart.toolError`; `partitionStreamPartsByMessage` forwards it.
- `messages.ts`: `transformHistory` threads `p.tool.state?.error`.
- `MessageBubble.tsx`: `extractMessageText` includes `Error: <msg>` for failed tools (copying a failure carries the WHY — the owner's manual re-quoting pain).

### 2. Rendering (`MessagePart.tsx`, the tool_use/tool_call branch)

- `extractToolError` (exported for unit pins) picks the most specific message, in order: state error → JSON body's `error` field (string / object's `message` / stringified object) → plain-text payload. `raw` carries the untouched payload when distinct.
- `ToolErrorBlock` (`data-testid="tool-error"`, red-tinted, AlertCircle header) renders as a SIBLING of the (possibly collapsed) tool pane — visible without expanding anything:
  - ≤ 10 lines: inline in full;
  - longer: a 3-line head inline + the full text behind a visible "Show full error (N lines)" `<details>` toggle;
  - distinct raw payload: behind its own "Raw output (N chars/KB)" toggle.
- Error-state tool with NO payload: the honest bare ✗ card stands (no empty error box).
- When the error block carries the output, the inner generic "Output" toggle is suppressed inside the pane (it would only duplicate the raw payload the block already exposes behind its toggle).
- Success path byte-identical: completed tools keep the collapsed "Output (N chars)" toggle exactly as before (regression-pinned).

### 3. Pins (TDD: 25 of the 30 new jsdom pins born-RED on the pre-fix tree, green post-fix)

- **NEW `MessagePart.toolErrors.test.tsx`** (23 tests): pin (a) error text visible WITHOUT expanding (full shape + bare ✗ card shape); the extraction ladder incl. state-error precedence over the JSON body; ≤10-line inline; pin (c) 30-line payload collapsed behind the affordance (closed `<details>.open === false` by default — jsdom visibility for closed details is ambiguous, the open state is the deterministic pin — click opens it); no-payload error keeps the bare card; pin (b) completed tool unchanged incl. the "Output (…)" toggle behind the (now-opened) pane; running tools never render an error block; the `extractToolError` unit ladder (9 cases).
- **`messages.test.ts`** (+2): history `state.error` → `toolError` threading + omission when absent.
- **`ChatPage.sse.test.tsx`** (+1, harness extended): ABI `ToolState{status: 4, error}` through the real contract event → streaming part carries `toolState: "error"` + `toolError`.
- **`ChatView.test.tsx`** (+2, NEW describe): streaming parts carry `toolError` through the bubble partition into the rendered transcript — from both the state error and a JSON output payload.
- **`MessageBubble.test.tsx`** (+2): the copy path (`[Tool: bash]\nError: exit status 1`, with output appended after).
- Born-RED accounting (review r1 correction — the r0 claim of "23/23" was wrong): measured on the FINAL test set against pre-fix sources (`f45ba872b` + test files applied): **25 red / 5 green-by-design of 30** — the 5 green are deliberate absence pins (no-payload error, running tool, input-but-no-payload error, completed-tool regression, toolError-omitted) that assert the pre-fix state too. Reviewer's independent r0 measurement (22/26 on the r0 test set) agrees in method.

### 3b. Review round 1 (CHANGES_REQUESTED → remediated)

- **Finding 1 (blocking) — the e2e gate**: no browser-level coverage of the changed behavior; jsdom structurally cannot assert real visibility (my own key decision conceded closed-`<details>` visibility is ambiguous there). Fixed: NEW `frontend/tests/e2e/tool-error-visibility.spec.ts` (3 tests, real Chromium, `dev-preview-button.spec.ts` harness precedent): (1) error-state tool renders its error VISIBLE with zero clicks + the completed-tool contrast (exactly one tool-error block in the transcript); (2) JSON error body extracts the message with the raw payload genuinely hidden behind the "Raw output" toggle until clicked (`toBeHidden`/`toBeVisible` — real-browser assertions); (3) a 30-line payload collapses behind the visible "Show full error" affordance — the tail invisible until clicked. All 3 green locally (chromium headless-shell 148).
- **Finding 2 — worklog numbering**: self-picked 1253 violated the NNNN sentinel rule (hooks uninstalled in the worktree — now known). Renamed to `NNNN_2026-11-12_tool-error-transcript-visibility.md`.
- **Finding 3 — doc accuracy**: born-RED figure corrected everywhere (25/30 measured, was "23/23"; the reviewer's r0 measurement was 22/26).
- **Items 2–4 (missing unit cases)**: added — failed file-edit tool (diff pane + error sibling), error-state tool with input but no extractable message (no block anywhere), `extractToolError` non-object JSON top levels (number/boolean/null/array).
- **Minor (addressed)**: failed `dev_preview_url` early returns now render the error block alongside the action card (state.error was dropped on that path); `as never` casts in ChatView tests replaced with `React.ComponentProps<typeof ChatView>["streamParts"]`-typed fixtures; `extractToolError` computed once per render (`ToolErrorBlock` takes the precomputed display).
- **CI note (reviewer-adjudicated)**: the red govulncheck check is the pre-existing GO-2026-6859 stdlib advisory surfaced through unchanged Go code — diff-independent; needs a toolchain bump outside this story's scope.

## Verification

- Full frontend suite: **2626/2626 green (227 files)**; e2e `tool-error-visibility.spec.ts` 3/3 in real Chromium locally.
- `tsc --noEmit` clean; `eslint .` 0 errors (7 warnings, all pre-existing); `make fmt-check` clean.
- Isolated worktree `/workspace/opencode/worktrees/copper-archer-rushes` (branch `fix/tool-error-visibility` off `origin/main` @ f45ba872b); `node_modules` symlinked from the shared checkout.

## Key decisions

- **Sibling placement, not inside the pane**: the pane's LazyDetails does not mount children until opened — anything inside is invisible by default. The error block renders after the tool card, always in the DOM (the dev-preview 1eee7f28 precedent applied to the error path).
- **State-error-first extraction ladder**: the runtime's `ToolState.error` is the most specific source (it is the harness's own failure verdict); JSON-body unwrapping handles MCP/HTTP error bodies (`{"error": "..."}`, `{"error": {"message": ...}}`); plain text passes through. Non-error JSON bodies fall back to the payload text (honest, never fabricated).
- **Gate on `toolState === "error"`, not on payload sniffing**: a completed tool whose output happens to contain `"error"` keeps its success rendering — the story is the error path only.
- **`<details>.open` as the collapse pin**: jsdom's visibility semantics for closed `<details>` are ambiguous; asserting the open attribute (and the content's presence inside it) is deterministic.

## Blockers

- None.
