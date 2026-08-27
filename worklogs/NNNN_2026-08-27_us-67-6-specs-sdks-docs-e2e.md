# Worklog: US-67.6 — Specs, SDKs, Docs, and E2E Completion (Epic 67 closer)

**Date:** 2026-08-27
**Session:** Epic 67 US-67.6: OpenAPI/SDK upload+files surface in all four SDKs, README-LLM/design/docs reconciliation, e2e rows E2–E12 at the deepest CI-enforceable level, full-repo validation + Rule-11 adversarial pass over the whole epic.
**Status:** Complete

---

## Objective

Close Epic 67: verify/extend `sdks/openapi.yaml` for the new upload surface, expose upload + `files` in the Go/TypeScript/Python/Java SDKs with per-toolchain verification, document the as-built system (README-LLM 1.25 + design-doc flip), implement e2e rows E2–E12 honestly (CI-enforceable vs cluster-only), and run a fresh-eyes adversarial pass over the entire epic surface.

---

## Work Completed

### OpenAPI + SDKs (Part A)

- `sdks/openapi.yaml` 0.6.0 → 0.7.0:
  - `/prompt` requestBody corrected to the as-built shape: `parts` (required) + `files[]` (maxItems 10, upload-namespace pattern), `clientMessageID`, `model`. The previously documented `content` field was dead (handler extracts text from parts only).
  - `/queue` requestBody gained `files[]` + `clientMessageID`.
  - `/message` documents the explicit 400 `files` rejection (D6).
  - Upload route + `FileUploadResponse` were already present from US-67.2 — verified, untouched.
- Go SDK: `WorkspacesService.UploadFile` (streaming multipart via io.Pipe — all multipart writing inside the copy goroutine; a first cut created the form part header on the caller goroutine and deadlocked until `Do` started reading), typed `FileUpload`, variadic `files` on `SendPromptAsync`/`Enqueue`, `APIError.Phase` on 409 upload gates, `authorize` extracted from `send` for reuse. Wire-level tests: multipart framing, streamed-not-buffered (one-byte reader probe), error mapping table, phase surfacing, body-shape regressions.
- TypeScript SDK: `workspaces.upload` (FormData — `request` detects FormData and lets the platform set the boundary), `FileUpload` type, `files` on `sendPromptAsync`/`enqueue`, `ConflictError.phase`. Same wire-level test set.
- Python SDK (sync + async): `workspaces.upload_file` (httpx `files=`), `FileUpload` dataclass, `files` on `send_prompt_async`/`enqueue`, `ConflictError.phase`. respx wire tests.
- Java SDK: `LLMSafeSpacesClient.requestMultipart` (manual multipart; `BodyPublishers.ofByteArrays` streaming), `models/FileUpload`, `sessions.sendPromptAsync/enqueue` overloads with `files`, `ConflictException.withPhase`. HttpServer wire tests.
- **Found + fixed (adversarial): all four SDKs sent dead prompt bodies** (`{"message":...}` / `{"content":...}`) that the live handler rejects with 400 "text must not be empty" — the SDK prompt methods had been broken since the Epic 63 V2 path change; nobody noticed because the deep canaries that exercise them need LLM creds + controller and are non-blocking. Fixed to the parts shape with no-dead-field regression tests in all four.
- `sdks/canary/Makefile`: **pre-existing breakage fixed** — the `s-mcp-crud`/`s-ownership` lines from #1072 lost their backslash continuations, making every canary target die with `missing separator` (verified `make -n canary-go-fast` before/after).
- Deep canaries `d-prompt-async` (Go/TS/Python): raw `{"message":...}` probe bodies updated to the parts shape (same drift class as above).
- New `make -C sdks sdk-check` (E9 core): spec validation + spec structural tests + `TestOpenAPIRouterContract` (spec↔router parity) in one locally/CI-runnable target. The Makefile's `generate-*` targets are placeholders (US-14.3–14.6 never shipped) — the SDKs are hand-written; `sdks/README.md` + `docs/api/sdks.md` now say so honestly instead of documenting phantom regeneration.

### Docs (Part B)

- README-LLM 1.25: new "File Attachments (Epic 67)" section — as-built flow diagram, the two design corrections (user mux :4097 + sidecar clean-fail; manifest v1 path+name only), caps table, D16 gate order, manifest format fenced **verbatim** against the golden fixture (sentinel-comment delimited for E12), per-surface file references, SDK surface, out-of-scope list. Version History 1.25 entry. TOC updated.
- Design doc: Status → "Shipped — as-built"; new "As-built deviations" section (port 4097 + sidecar; bytes= drop; SDK prompt-body correction; e2e coverage honesty).
- `docs/api/rest.md`: upload route + "File uploads (Epic 67)" subsection with curl examples; `/prompt`+`/queue` rows note `files`; `/message` row notes the 400.
- `docs/api/sdks.md`: dropped the removed VS Code extension reference + phantom generation commands; documented `sdk-check`, parity enforcement, and the attachments surface with Go/Python examples.
- `README.md` route inventory: uploads row + prompt/message annotations.

### E2E rows (Part C)

- **E7** `api/internal/handlers/e2e_attachments_golden_test.go`: real `POST /prompt` through the router + adapter against the stub-agent httptest harness; the dispatched prompt must byte-match four golden fixtures (`compose_one_file`, `compose_three_files`, `compose_strip_existing_block`, `compose_trailing_newlines_3`) — locks format v1 cross-release.
- **E8** `pkg/mcp/integration_test.go` `TestIntegration_ExternalStdioClient_UploadAndMessageWithFiles`: builds `cmd/mcp`, spawns it as a subprocess, connects with mcp-go's stdio client (`NewStdioMCPClient`), drives `workspace_file_upload` + `session_message(files)` against a fake REST API asserting multipart framing, per-hop auth, and the exact manifest bytes in the dispatched prompt. 2.2s warm. (First cut overrode GOCACHE to a cold temp dir — 34s; fixed.)
- **E12** `pkg/session/attachments/readme_consistency_test.go`: greps the sentinel-fenced snippet out of README-LLM.md and byte-compares against `testdata/compose_one_file.want.json`.
- **E3/E4** `frontend/tests/e2e/attachments.spec.ts` extended: 26 MiB upload → stubbed 413 → chip error + friendly notice + no `files[]` in the subsequent prompt; suspended workspace → stubbed 409 → phase hint surfaced. Plus the frontend improvement E4 demanded: `useComposerAttachments` now appends `(phase: …)` to the chip error from the 409 body (`uploadErrorMessage`, `ApiError.phase` in types.ts) with a vitest regression test.
- **E2/E10/E11** `local/us-67-attachments-e2e.sh` (new, executable, `bash -n` clean): kind-cluster harness following `local/test.sh` conventions — two seeded users/workspaces, PVC-persistence across suspend/resume (sha256 compare), multi-tenant concurrent uploads + cross-user 404 + no leakage, pod-kill-mid-upload chaos (rate-limited 6 MiB body, kill at 4s, clean-5xx-or-completed acceptance, retry, no `.tmp` residue, all stored copies exactly 6 MiB). Wired into `e2e-nightly.yml` after `local/test.sh`. **Sidecar gate:** detects agentd sidecar mode, asserts the D1 clean-fail (5xx + nothing written) and skips the three rows with an explicit message — the nightly installs sidecar mode, so the rows run fully only on single-container clusters. Honestly reported as cluster-only.

### Validation (Part D)

Full repo green (commands + counts in Tests Run). Rule-11 adversarial pass findings in Key Decisions/Next Steps below; two real fixes landed (Java multipart filename neutralization + canary/d-prompt-async dead-body drift), both with regression tests.

---

## Key Decisions

1. **"Regenerate SDKs" was actually "hand-extend SDKs."** The Makefile generation targets are no-op placeholders; the four SDKs are hand-written against the spec. I extended each per its own conventions (verified by reading the #1072 MCP-CRUD precedent) and made the docs stop implying codegen exists.
2. **Spec bodies reconciled to code, not the reverse.** The spec's `/prompt` `content` field and the SDKs' `message` bodies were dead wire shapes; the live contract is `{parts, files, clientMessageID, model}`. Spec + SDKs + canaries now match the handler (validated by TestOpenAPIRouterContract + the four SDK wire-test suites + E7).
3. **E3's pod-side half is Go-suite-covered, not browser-covered** — with a stubbed backend the browser can only prove the 412/413 UX; the "nothing lands on the PVC" half is asserted by U1.1.4/U1.2.5. Labeled as such in the spec comment and the report.
4. **Sidecar-mode uploads are an as-built limitation, documented and gated** — not worked around. The cluster script asserts the clean-fail instead of pretending the rows run.
5. **No uploads.hurl added** — hurl is unavailable in this sandbox (no root, no libglib for chromium either); I will not commit a contract file I cannot execute. The same request shapes are asserted by the four SDK wire tests against real Go HTTP parsers, and the existing hurl suite has no /prompt coverage to drift against (verified by grep).

## Assumptions (stated + validated)

| # | Assumption | Validation |
|---|---|---|
| A1 | SDK generation targets are placeholders; SDKs hand-written | sdks/Makefile read; #1072 diff; sdks/README.md |
| A2 | `/prompt` spec body (`content`) dead vs handler (parts) | `extractPromptText` proxy_handlers.go:427-449; SDK tests now pin parts shape; E7 proves end-to-end |
| A3 | API upload route + FileUploadResponse already in spec | openapi.yaml:1114-1196 (from #1083) |
| A4 | Sidecar mode RO /workspace → uploads 5xx | agentd_sidecar.go:182 `ReadOnly: true`; US-67.1 worklog A9; uploads.go header comment |
| A5 | Cluster e2e harness = local/*.sh on kind (nightly) | e2e-nightly.yml runs local/test.sh; canary job has no controller/pods |
| A6 | mcp-go v0.54 stdio client can spawn a subprocess server | client/stdio.go `NewStdioMCPClient`; E8 passes (2.2s) |
| A7 | Playwright runnable in sandbox | **DISPROVEN** — chromium installs but system libs missing (libglib), no root; specs typechecked (`tsc --ignoreConfig` clean) + CI-covered (ci.yml frontend job). Honest report item. |
| A8 | hurl/Prism contract suite unaffected by spec change | grep: no hurl file posts /prompt//message/uploads; /queue posts `{"text"}` which remains valid |
| A9 | agentd EEXIST retry can't lose bytes | uploads.go: create() fails before any body read; LimitReader shared across attempts |
| A10 | Golden fixtures authoritative for manifest v1 | pkg/session/attachments/testdata + package doc; E7/E12 lock them |

---

## Blockers

None. Two environment limitations, honestly scoped: Playwright browsers cannot run in this sandbox (missing system libs, no root) — CI-covered; hurl absent locally — existing suite verified unaffected by inspection.

---

## Tests Run

| Command | Result |
|---|---|
| `go build ./...` (repo root) | OK |
| `go test -timeout 1800s -race -count=1 ./...` (repo root) | **84 packages ok, 0 failures** |
| `make lint` (golangci-lint) | **0 issues** |
| `make -C sdks sdk-check` | OK (validate + spec tests + router parity) |
| `cd sdks/go && go build ./... && go test -race ./...` | ok |
| `cd sdks/typescript && npx tsc --noEmit && npx vitest run && npx tsup` | **62/62 tests**, typecheck + build OK |
| `cd sdks/python && pytest tests/ -q` | **106 passed** |
| `cd sdks/java && mvn test -B` | **30/30 tests**, BUILD SUCCESS |
| `cd frontend && npx tsc --noEmit && npm test -- --run` | **1797/1797 tests** (161 files), typecheck OK |
| `npx tsc --ignoreConfig … tests/e2e/attachments.spec.ts` | clean (browser rows CI-covered; see A7) |
| `go test -run TestE2E_PromptFilesGoldenBytes ./api/internal/handlers -race -v` | 4/4 fixtures byte-match |
| `go test -run TestIntegration_ExternalStdioClient ./pkg/mcp -race` | PASS (2.2s) |
| `go test ./pkg/session/attachments/` (incl. E12) | ok |
| `make -n canary-{go,python,typescript}-fast -f sdks/canary/Makefile` | parses, all scenarios present |
| `bash -n local/us-67-attachments-e2e.sh` | clean |

---

## Next Steps

- Re-run the Playwright attachments spec in CI on this PR (E1/E3/E4/E5/E6 browser rows).
- Nightly: the attachments e2e step will exercise the sidecar clean-fail path as configured; to execute E2/E10/E11 fully, run the nightly (or a kind cluster) with `controller.agentdSidecar.enabled=false` — tracked follow-up alongside the sidecar control-socket write op.
- Optional follow-up: uploads.hurl once a hurl-capable environment is available.
- The d-prompt-async deep canaries should be executed once against a live cluster to confirm the parts-shape fix end-to-end (they need LLM creds).

---

## Files Modified

- sdks/openapi.yaml, sdks/Makefile, sdks/README.md, sdks/canary/Makefile
- sdks/go/{client.go,errors.go,services.go,types.go,uploads.go,uploads_test.go}
- sdks/typescript/src/{client.ts,errors.ts,types.ts}, sdks/typescript/tests/client.test.ts
- sdks/python/llmsafespaces/{__init__.py,async_client.py,client.py,errors.py,types.py}, sdks/python/tests/{test_client.py,test_async_client.py}
- sdks/java/src/main/java/com/llmsafespaces/sdk/{LLMSafeSpacesClient.java,exceptions/ConflictException.java,services/{SessionsService,WorkspacesService}.java,models/FileUpload.java}, sdks/java/src/test/java/com/llmsafespaces/sdk/LLMSafeSpacesClientTest.java
- sdks/canary/{go/scenarios/d-prompt-async/main.go,typescript/scenarios/d-prompt-async.ts,python/scenarios/d_prompt_async.py}
- README-LLM.md, README.md, docs/api/{rest.md,sdks.md}, design/stories/epic-67-chat-file-attachments/README.md
- api/internal/handlers/e2e_attachments_golden_test.go
- pkg/mcp/integration_test.go
- pkg/session/attachments/readme_consistency_test.go
- frontend/src/api/types.ts, frontend/src/hooks/{useComposerAttachments.ts,useComposerAttachments.test.tsx}, frontend/tests/e2e/attachments.spec.ts
- local/us-67-attachments-e2e.sh, .github/workflows/e2e-nightly.yml
