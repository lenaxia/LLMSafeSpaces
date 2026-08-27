# Worklog: US-67.3 — Attachment manifest contract + send-path integration

**Date:** 2026-08-27
**Session:** Implement the v1 attachment manifest package (`pkg/session/attachments/`) and wire `files[]` composition into the V2 send paths (`/prompt`, `/queue`) with explicit rejection on V1 `/message` (Epic 67, decisions D6/D7/D8/D15).
**Status:** Complete

---

## Objective

Deliver US-67.3: the platform-owned attachment manifest format (compose + parse + path validation, golden-fixture locked) and its integration at acceptance time on the send paths, so uploaded-file references ride prompt text to any agent (agent-agnostic, above the adapter seam per D10).

---

## Work Completed

### 1. Fixture review and completion (testdata/)

The previous (interrupted) attempt left 29 fixture files. Review verdict against the contract:

- **Adopted as-is** (24 files): all 12 `compose_*` pairs and `parse_no_block`, `parse_forged_interior.in`, `parse_roundtrip.in`.
- **Fixed** (1): `parse_roundtrip.want.json` carried `"bytes": 0` on the attachment object — contradicts the v1 contract (`Attachment{Path, Name}` only; the design doc's illustrative `bytes=` sketch is superseded per D8: send-time validation is shape-only because the server cannot stat workspace files). Removed the field.
- **Added** (6 files): `parse_forged_interior.want.json` was missing (orphan `.in`); added `parse_trailing_newlines` and `parse_no_trailing_newline` (U1.3.18), `parse_unknown_version` and `parse_unknown_attribute` (U1.3.19 forward-compat: not consumed, treated as text).

### 2. `pkg/session/attachments/` (new package)

- `Attachment{Path, Name}` with `path`/`name` JSON tags.
- `ValidatePaths` — enforces `^/workspace/uploads/[0-9a-f]{8}-…-[0-9a-f]{12}-` (lowercase hex), ≤10 files, exact-duplicate rejection, empty/whitespace-padded entry rejection, `..` segment rejection. Typed sentinels `ErrTooManyFiles` / `ErrDuplicatePath` / `ErrInvalidPath` (wrapped with the offending index; handlers map any of them to 400).
- `Compose` — validates, extracts the name (segment after the 56-char `uploads/<uuid>-` prefix), strips control chars, backslash-escapes `\` and `"` in BOTH path and name attributes, strips any pre-existing trailing v1 block (D15 idempotency), normalizes trailing newlines, appends one blank line + one line per file in input order. Zero files → text byte-identical; empty text → block only.
- `Parse` — consumes only the contiguous trailing block (trailing-newline tolerant), drops the single blank separator line, returns `nil, text` when no block. Strict line shape: unknown version markers / unknown attributes / non-`\\`-`\"` backslash usage do not match and stay text.
- Tests: golden fixture walkers (byte-exact), validation table (18 cases), idempotency property (6 text shapes incl. forged trailing blocks), round-trips (unicode + hostile names), line-integrity under hostile names. All TDD: tests written first, verified red, then implemented.

### 3. Send-path wiring (api/internal/handlers/proxy_handlers.go)

- `SendPromptAsync`: `extractPromptFiles(bodyBytes)` (new helper, mirrors `extractPromptModel`); when non-empty, compose BEFORE the empty/100KB checks — so the composed text is what the empty-check, length cap, outbox persistence (D3/D7: compose-once at acceptance, retries re-dispatch stored text), adapter `Send`, and legacy `enqueueV2` all see. clientMessageID + model extraction untouched.
- `EnqueueMessage` (queue): added `Files []string` to `enqueueRequest`; same compose-before-checks placement. Removed `binding:"required"` from `Text` so a files-only body reaches the composer instead of failing bind — the explicit `len(req.Text) == 0` check still governs the no-files case (queue's contract-auth short-circuit test unaffected: still 400).
- `SendMessage` (V1 `/message`, D6): new `rejectMessageRouteFiles` probe at handler top — reads the body under the legacy proxy's 10 MB cap (413 on over-cap, mirroring proxy.go), restores it via `NopCloser` (byte-identical downstream — locked by test), and 400s `files not supported on this route; use /prompt` when a non-empty `files` array is present. Fail-open on non-JSON bodies (verbatim proxy preserved). Applies to both adapter and legacy legs.
- Total-length cap (U1.3.11/U1.3.17): composed text is checked against the existing 100,000-char cap with the existing error message (400) — boundary test proves exactly-at-cap passes, one-over fails.

### 4. Handler tests (api/internal/handlers/proxy_attachments_test.go, new)

U1.4.1 (adapter path: dispatched text + model passthrough; outbox e2e: 202 → worker → fake opencode receives composed text), U1.4.2 (queue entry carries composed text once; files-only body → block-only entry), U1.4.3 (503-then-200 flaky backend: both dispatch attempts byte-identical, exactly one manifest line each, entry leaves outbox), U1.4.4 (both legs 400 + adapter/backend never called; empty `files:[]` allowed), U1.4.5 (clientMessageID duplicate retry → single entry, deterministic composed text), U1.4.6 (see below), plus validation-400 table and the prompt-cap boundary.

---

## Key Decisions

1. **`bytes` field absent from v1** — the design doc's U1.3.8 sketch (`[]Attachment{path,name,bytes}`) is superseded: D8 makes send-time validation shape-only (no stat), so the server has no authoritative size. Golden fixtures are authoritative and lock path+name only. Noted in the package doc. The parser's `Attachment` shape is what US-67.5's frontend chips renderer will consume.
2. **Compose before the empty-text check** — a files-only prompt legally dispatches the block-only text (U1.3.3 at the handler level). Rationale: the task places composition "after text extraction", and the manifest-only text is a meaningful prompt ("here is a file"). Both `/prompt` and `/queue` behave identically; locked by `TestPrompt_Files_EmptyText_BlockOnly` and `TestQueue_FilesOnly_BlockOnlyEntry`.
3. **"uuid v4" in prose ≠ enforced v4 semantics** — the task's regex (and the fixtures themselves, e.g. `11111111-2222-3333-4444-555555555555`) specify lowercase-hex groups without version/variant constraints. The regex is authoritative (validated: fixture uuids would fail strict v4 checks).
4. **Empty name after the uuid hyphen is allowed** — the shape regex requires only the trailing `-`; send-time validation stays minimal per D8. Upstream agentd sanitization (U1.1.14) rejects empty-sanitized filenames at upload, and the API can't stat anyway.
5. **`binding:"required"` removed from queue `Text`** — replaced by the already-present explicit empty check so files-only bodies compose; no observable status change for other bodies (still 400).
6. **V1 probe cap = legacy proxy's 10 MB** — on the adapter leg a >10 MB body now 413s at the probe instead of 400 at `extractMessageText`'s 100 KB reader; both are rejections of oversized bodies; the legacy leg is unchanged. Non-JSON bodies fail open (verbatim proxy contract preserved).
7. **maxPromptBodyBytes slack unchanged** — a body with ~100 KB text plus a max-size `files` array may hit the body-read 400 before the composed-length 400; both are 400s and the composed-length check is the authoritative gate (boundary-tested).

---

## Assumptions (stated + validation evidence)

| Assumption | Validation |
|---|---|
| Prompt/queue request DTOs live in `pkg/types/` | **Disproved by reading code.** `SendPromptAsync` parses the body inline (`extractPromptText`/`extractPromptModel`, proxy_handlers.go:403-504); the queue DTO is the package-local `enqueueRequest` (proxy_handlers.go:1235). `pkg/types` has no prompt/queue DTO (grep). Followed the codebase's actual pattern: local extraction helper + local DTO field instead of a new `pkg/types` type. |
| Disk-pressure injection does not apply to `/prompt` (U1.4.6) | **Confirmed.** `injectDiskPressureNotice` is called only in `proxyToWorkspaceWithErrBody` (proxy.go:427-430) behind `isLLMPromptPath` (`/message` suffix only, proxy_disk_pressure.go:215-217). `/prompt` flows outbox → adapter.Send or enqueueV2 → PromptV2 — never through that proxy. README-LLM §Disk-pressure documents this as a known gap since US-63.3. Consequence: the D6 rejection of `files` on `/message` makes notice+manifest co-occurrence impossible at runtime today; test `TestPrompt_Files_DiskPressure_NoInteraction` locks the reality (96% disk + files composes normally, no notice — disk gating is upload-side per D5). The design's "notice first, manifest after" ordering remains fixture-irrelevant until the V2 disk-pressure gap is closed (follow-up noted below). |
| Outbox persists exactly the composed text (U1.4.3) | `outbox.Entry.Text` (`outbox.go:62-66`) receives the handler's `text` verbatim; worker re-dispatches stored text. Proven by `TestOutbox_Files_RetryRedeliversStoredTextOnceManifest`. |
| Fixture want-files are pretty-printed JSON objects (parse) / JSON strings (compose) | Verified bytewise (`od -c`): 2-space indent + trailing newline; compare via `json.MarshalIndent` + TrimRight. |
| `strings.NewReplacer` unescape ordering is safe | Non-overlapping left-to-right semantics; input constrained by the line regex to `\\`/`\"` escapes only. Round-trip tests with `we"ird\name.txt` prove it. |
| Full handler suite needs >120s under `-race` here | Measured 162s (pre-existing suite size + `-race`). Passing run with adequate timeout recorded under Tests Run. |

---

## Blockers

None.

---

## Tests Run

- `go test -timeout 60s -race -count=1 ./pkg/session/attachments/` → **ok** (27 tests: 12 compose golden + 7 parse golden + validation table + idempotency + round-trips + integrity).
- `go test -timeout 120s -count=1 -run 'TestPrompt_Files|TestQueue_Files|TestOutbox_Files|TestMessage_Files|TestMessage_NoFiles|TestMessage_EmptyFiles|TestExtractPromptFiles' ./api/internal/handlers/` → **ok** (15 tests).
- `go test -timeout 300s -race -count=1 ./pkg/session/... ./api/internal/handlers/...` → **ok** (full suite incl. all pre-existing tests; handlers ~162s under race).
- `gofmt -l` / `goimports -l` on all touched files → clean. `go vet` on both packages → clean.

Note: `-timeout 120s` is insufficient for the handlers package under `-race` on this machine (pre-existing; the package alone takes ~162s). 300s is used for the full run.

---

## Next Steps

- US-67.4 (MCP): `session_message(files)` should call the same `attachments.Compose` (shared code path, U1.5.6) and reuse the sentinel→400/tool-error mapping.
- US-67.5 (frontend): TS parser must mirror `attachmentLinePattern` semantics (strict two-attribute shape, `\\`/`\"` unescape, trailing-block-only consumption) — the golden fixtures double as its spec.
- Follow-up (outside this epic's scope): if disk-pressure injection is ever extended to the V2 `/prompt` path (the README-LLM §Disk-pressure known gap), the design's U1.4.6 ordering (notice first, manifest after) must be asserted then; with D15 idempotency the composition already tolerates a notice-prefixed text.

---

## Files Modified

- `pkg/session/attachments/attachments.go` (new)
- `pkg/session/attachments/attachments_test.go` (new)
- `pkg/session/attachments/testdata/parse_roundtrip.want.json` (fixed: removed superseded `bytes` field)
- `pkg/session/attachments/testdata/parse_forged_interior.want.json` (new — was missing)
- `pkg/session/attachments/testdata/parse_trailing_newlines.{in,want}.json` (new)
- `pkg/session/attachments/testdata/parse_no_trailing_newline.{in,want}.json` (new)
- `pkg/session/attachments/testdata/parse_unknown_version.{in,want}.json` (new)
- `pkg/session/attachments/testdata/parse_unknown_attribute.{in,want}.json` (new)
- `api/internal/handlers/proxy_handlers.go` (compose wiring on `/prompt` + `/queue`, `files` rejection on `/message`, `extractPromptFiles`, `rejectMessageRouteFiles`, `enqueueRequest.Files`)
- `api/internal/handlers/proxy_attachments_test.go` (new)
- `worklogs/NNNN_2026-08-27_us-67-3-attachment-manifest.md` (this file)
