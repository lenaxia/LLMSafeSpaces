# Worklog: US-67.4 — MCP upload tool + session_message files param

**Date:** 2026-08-27
**Session:** Implement Epic 67 US-67.4 — `workspace_file_upload` MCP tool (base64, D18 whitespace normalization, 5 MiB decoded cap D4, 7 MiB encoded early guard U1.5.13, shared agentd filename sanitizer, phase-naming tool errors U1.5.11) + `session_message` optional `files []string` (shared `attachments.Compose` U1.5.6, maxMessageSize accounting incl. manifest U1.5.7, strip-then-append idempotency D15/U1.5.10) + tool-description contract documentation (5 MiB cap, REST/SDK alternative, v1 manifest format). TDD-first.
**Status:** Complete

---

## Objective

Per `design/stories/epic-67-chat-file-attachments/README.md` (D4, D7, D8, D15, D18; test plan §1.5 U1.5.1–U1.5.13; integration row I10 at the in-process-client level): expose the US-67.2 upload primitive and the US-67.3 attachment manifest to MCP clients (agent-as-caller surface), sharing the exact validation/composition code the REST path uses.

---

## Work Completed

### Branch base (dependency merge)

`feat/us-67-4-mcp-tools` was stacked on US-67.2 only, but US-67.4 depends on US-67.3 (`pkg/session/attachments` — design dependency graph "US-67.3 ──► US-67.4"; the task mandates sharing Compose/ValidatePaths). Merged `feat/us-67-3-attachment-manifest` (single commit `8757a67a`, base `83ec5a1e` already an ancestor, file-disjoint from US-67.2's changes — clean merge, no conflicts). Post-merge baseline: `./pkg/mcp/... ./pkg/session/...` green before any new code.

### New tool: `workspace_file_upload` (pkg/mcp/uploads.go)

- **Args**: `workspace_id`, `filename`, `content_b64` (all required, schema-enforced + handler-checked).
- **Base64 (D18)**: `\n` `\r` space tab stripped before a strict `base64.StdEncoding` decode; malformed-after-normalization → `mcp.NewToolResultError` (never a panic, U1.5.2); empty/whitespace-only content → tool error (U1.5.9); URL-safe alphabet and bad padding rejected (strict standard alphabet).
- **Caps (D4/U1.5.3/U1.5.13)**: encoded input > 7 MiB rejected *before decode* (and before any HTTP dial — asserted); decoded > 5 MiB rejected after decode; both errors name the 5 MiB cap and point to the REST/SDK upload endpoint for files up to 25 MiB. Exactly 5 MiB passes (boundary test). 7 MiB is a safe encoded bound: 4/3 · 5 MiB ≈ 6.99 MB < 7 MiB, so the guard never rejects decodable-under-cap input.
- **Filename (U1.5.5)**: `agentd.SanitizeFilename` — the same shared source the REST handler and agentd use (US-67.2 moved it to `pkg/agentd` precisely for this); sanitizes-to-empty → tool error. The API re-sanitizes (D9 defense-in-depth, idempotent).
- **Result (U1.5.1)**: JSON `{path, name, size}` from the API's 201.
- **Phase errors (U1.5.11)**: `UploadError{Status, Message, Phase}` typed error decoded from the API's 409 body (`{"error","phase"}`); the tool error names the phase ("cannot upload: workspace not active (phase: Suspended)") — asserted NOT to contain the raw HTTP status. 413 → cap message; 507 → disk-full message; other/garbage bodies → public-safe fallback.

### `session_message` files (pkg/mcp/server.go)

- Optional `files` array arg (`mcp.WithArray` + `WithStringItems`, not required; max 10/dup/shape validated by the shared `attachments.ValidatePaths` inside Compose).
- **Shared code path (U1.5.6)**: the handler calls `attachments.Compose(message, files)` — the exact function the REST `/prompt` and `/queue` handlers call (proxy_handlers.go) — and dispatches the composed text via the unchanged `SendMessage`. Non-string entries become empty strings → explicit rejection.
- **Size accounting (U1.5.7)**: `maxMessageSize` is checked on the *composed* text; the no-files error message stays byte-compatible with the original ("message too large (N bytes, max M)"), the files variant adds "including attachment manifest".
- **Idempotency (D15/U1.5.10)**: Compose's strip-then-append applies — a caller message already ending with a (possibly forged) v1 block dispatches with exactly one manifest block (asserted via count + exact composed-text mock expectation).
- Compose-once at MCP acceptance (D7): the composed text is dispatched as plain parts text; the server does not recompose (no `files` key on the wire), so retries/resends cannot double-append.

### Tool descriptions (US-67.4 C)

`workspace_file_upload` documents the 5 MiB decoded cap, the REST/SDK alternative (route + 25 MiB), the `{path,name,size}` result, and its link to `session_message.files`. `session_message` documents the files parameter, the max-10/no-duplicates rule, and the v1 manifest line format (`[llmsafespaces:attachment path="..." name="..."]`) so agent-callers can reference files in raw prompts; both descriptions assert these contract strings in tests (schema pinning, mirrors the #880 pattern).

### HTTPClient.UploadFile (pkg/mcp/uploads.go)

POSTs `multipart/form-data` (single `file` field, sanitized filename) to `/api/v1/workspaces/:id/uploads` with Bearer API-key auth — same route, gates (phase/disk/cap), and middleware stack as web/SDK clients. Response read capped at `maxResponseBody`; non-201 → typed `UploadError` with phase extraction; `validateID` guards the path segment.

### Tests (TDD — written first, red confirmed via `go vet` before implementation)

- `pkg/mcp/uploads_test.go` (new): schema pins; U1.5.1 happy path; U1.5.4 missing-args table; U1.5.2 invalid-base64 table (garbage/bad length/bad padding/urlsafe); U1.5.8 whitespace-wrapped base64; U1.5.9 empty/whitespace content; U1.5.3 decoded-cap boundary (exactly 5 MiB / +1); U1.5.13 encoded guard (8 MiB, zero dials asserted) + 6 MB garbage arg; U1.5.5 hostile-filename table pinned to `pkg/agentd/sanitize_test.go` literals; U1.5.11 phase error (names phase, no HTTP passthrough); server-cap/disk/generic error mapping; U1.5.12 4-parallel uploads through a real `HTTPClient` + httptest API (distinct paths, multipart asserted); wire tests for `HTTPClient.UploadFile` (method/path/auth/field/filename/bytes, 409-phase parse, 413/507/502/garbage table, invalid workspace ID no-dial).
- `pkg/mcp/server_test.go`: MockAPIClient gains `UploadFile`; session_message files tests (U1.5.6 composed-dispatch exact-text, no-files unchanged, invalid-files table incl. traversal/relative/non-uuid/empty/padded/duplicate/11-files/non-string; U1.5.7 manifest-inclusive size accounting; U1.5.10 forged-trailing-block idempotency).
- `pkg/mcp/integration_test.go`: tool count 24 → 25 + name assertions; new `TestIntegration_UploadThenMessageWithFiles` — in-process mcp-go client through the real `NewServer` wiring: upload (base64 arg) → `session_message(files)` → composed dispatch asserted (I10 at the in-process tier; the deployed-cluster variant is E8/US-67.6).

### Test commands + results

- `go test -timeout 300s -race -count=1 ./pkg/mcp/...` → **PASS** (full package: 178 RUN cases incl. subtests, 0 failures; US-67.4 additions: 26 test funcs / 58 cases).
- `go test -timeout 900s -race -count=1 ./api/...` → **PASS** (merge validation; no handler code touched by US-67.4).
- `go test -timeout 300s -race -count=1 ./pkg/session/...` → **PASS**.
- `golangci-lint run ./pkg/mcp/...` → 0 issues. `gofmt -l pkg/mcp/` → clean. `make repolint` → all checks passed (3 NNNN sentinels pending the numbering bot, expected).

---

## Key Decisions

1. **Seam: MCP calls the REST upload route — no handler refactor.** The MCP server is a separate binary (`cmd/mcp`) that talks to the API exclusively through `HTTPClient` over REST (verified: `cmd/mcp/main.go` constructs `llmmcp.HTTPClient`; every one of the ~30 `APIClient` methods is an HTTP call; the MCP process has no K8s/service-layer access). The "upload service" IS `POST /api/v1/workspaces/:id/uploads` (US-67.2) — adding `UploadFile` to `APIClient` puts MCP behind the identical phase/disk/cap gates and middleware with zero duplication. Extracting a service-level function shared by the gin handler and MCP would require the MCP binary to link the API's service layer (K8s clients, secrets, metrics) — a major coupling for a second consumer that doesn't exist as a process topology. Rule 4/12: no speculative abstraction.
2. **session_message composes at the MCP layer and dispatches composed text** (not `files[]` on the wire). U1.5.6's "shared code path" is satisfied literally — both REST and MCP call the one `attachments.Compose` (D7's single-function rule). Local composition makes U1.5.7 (exact manifest-inclusive accounting) and U1.5.10 (idempotent single block, asserted on the dispatched text) directly enforceable at the MCP seam, and keeps compose-once-at-acceptance: the server sees plain text with no `files` key and cannot recompose.
3. **Phase errors are decoded, not passed through.** The 409 body's `phase` field is parsed into `UploadError.Phase` client-side and re-emitted as a phase-naming tool error (U1.5.11). Generic `doJSON` error strings ("API error 409: …") are intentionally bypassed for this route.
4. **Encoded guard at 7 MiB, measured pre-normalization.** Stripping whitespace only shortens, so a raw-length check is conservative; decode + decoded-cap remain the authoritative bounds (a whitespace-padded >7 MiB input that normalizes under 7 MiB decodes to at most ~5.25 MiB and is still cap-checked after decode).

---

## Assumptions → Validation

| Assumption | Validation |
|---|---|
| US-67.3 (`pkg/session/attachments`) is available to this branch | It was NOT an ancestor (git merge-base check); merged `feat/us-67-3-attachment-manifest` (1 commit, disjoint files, clean merge) per the design dependency graph US-67.3 ──► US-67.4 |
| MCP server reaches services only via REST | `cmd/mcp/main.go:52-58` (HTTPClient + NewServer — no service imports); `pkg/mcp/client.go` APIClient is all-HTTP |
| API upload 409 body carries `phase` | api/internal/handlers/uploads.go:167-174 (`gin.H{"error":"workspace not active","phase":…}`) |
| `pkg/agentd` is dependency-light enough for the MCP binary | `go list -deps ./pkg/agentd` — no k8s/gin/pgx/redis; sanitize.go imports only stdlib |
| Shared sanitizer behavior (hostile table) | `pkg/agentd/sanitize_test.go` literals pinned into U1.5.5 table |
| mcp-go v0.54.0 supports array-of-string args | `mcp.WithArray` + `WithStringItems` in `tools.go:1251,1382`; round-trip proven by the in-process integration test (JSON args arrive as `[]any`) |
| mcp-go delivers tool arguments as `map[string]any` with `[]any` arrays | server.go's existing `args[...].(string)` pattern + integration test passing end-to-end |
| `maxMessageSize` (1 MiB, pkg/mcp/client.go:30) is the MCP-side send cap to account against | client.go:390 double-checks in `HTTPClient.SendMessage` — composed text flows through both checks consistently |
| REST `/prompt` accepts `files[]` (server-side compose exists for REST/SDK clients) | US-67.3 diff: proxy_handlers.go `extractPromptFiles` + `attachments.Compose` (not used by MCP per Key Decisions #2) |

## Deviations

- **U1.5.13's literal example is unsatisfiable as written** ("6 MB of `z` → tool error before decode"): `z` is in the standard base64 alphabet, and 6 MB of it decodes to 4.5 MiB — *under* the 5 MiB decoded cap — so no implementation of the specified caps rejects it. Surfaced as a design-doc approximation error (Rule 7.5); tests cover the scenario's intent: 8 MiB input → early rejection with zero dials (asserted), 6 MB of non-alphabet garbage → strict-decode tool error, no panic/OOM.
- **Branch base**: merged US-67.3 into the stacked branch (the task described it as already merged; it was not an ancestor). No other branch operations; nothing pushed.
- **I10 covered at the in-process tier** (mcp-go client over the real `NewServer` with MockAPIClient); the deployed-API variant is E8 (US-67.6).

## Adversarial self-review (Rule 11)

- Oversized-error wording leaked "including attachment manifest" into the no-files path → fixed (branch on `len(files) > 0`); plain oversized messages keep the original wording (existing `TestSessionMessage_TooLarge` still green).
- Phase message initially appended "activate the workspace first" — wrong advice for Terminating/Failed phases → removed; the requirement is phase-naming only.
- Test-table bug caught red-phase: the "eleven files" row had 10 paths (validated fine and dispatched, panicking the mock) → 11th path added; now exercises `ErrTooManyFiles`.
- `stringSliceArg` drops non-string entries to `""` → rejected by `ValidatePaths` as empty entries (explicit rejection, table-covered).
- False alarm dismissed: guard-before-normalization admits whitespace-padded >7 MiB inputs past the early exit — bounded by decode + decoded-cap (Key Decisions #4); memory ceiling ~5.25 MiB, acceptable.
- False alarm dismissed: non-201 success codes from the API error out — the route emits exactly 201 on success (uploads.go switch), strictness is correct.
