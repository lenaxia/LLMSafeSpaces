# Worklog: US-67.2 — API upload route (streaming multipart + gates)

**Date:** 2026-08-27
**Session:** Implement Epic 67 US-67.2 — `POST /api/v1/workspaces/:id/uploads`: streaming multipart handler on the proxy idGroup, D16 gate order (auth/access → phase → disk → cap), forwarding to agentd's user-mux `PUT /v1/files` (:4097) under Basic auth, API-side filename sanitization (D9), error mapping, Prometheus outcome counters, OpenAPI surface. TDD-first.
**Status:** Complete

---

## Objective

Per `design/stories/epic-67-chat-file-attachments/README.md` (D1, D4, D5, D9, D16; test plan §1.2 U1.2.1–U1.2.22; integration rows I1–I5, I8, I9 at the httptest+fake-agentd level): one upload primitive on the API that streams a single multipart `file` part to the workspace pod's agentd ingest endpoint, never buffering the body, with gates asserted in contract order and per-reason metrics.

---

## Work Completed

### Route + handler

- **Route**: `idGroup.POST("/uploads", proxyHandler.UploadFile)` in `registerProxyRoutes` (api/internal/server/router.go) — inherits AuthMiddleware + WorkspaceAccessMiddleware like every other proxied `:id` route (I1). Gin's default method handling gives 404 for GET/PUT on the path (U1.2.12, no `HandleMethodNotAllowed` in the router — verified).
- **Handler**: `ProxyHandler.UploadFile` (api/internal/handlers/uploads.go) — reuses ProxyHandler's k8s client, namespace, httpClient, logger, and `getPassword` (K8s Secret `workspace-pw-<id>` with wsstate cache). No new client or service is constructed.
- **Gate order (D16)**: phase (`Status.Phase != Active || PodIP == ""` → 409 `{"error":"workspace not active","phase":<phase>}`, terminal.go-style) → disk (`systemnotices.Ratio(DiskUsedBytes, DiskTotalBytes) >= criticalThreshold` → 507; `totalBytes==0` → fail open via Ratio's 0 return) → cap (Content-Length > cap+envelope allowance → local 413, no agentd dial) → content type (non-multipart or missing boundary → 415) → multipart shape (no/wrong-field/duplicate `file` part → 400; empty filename → 400).
- **Streaming**: manual `multipart.Reader` over `c.Request.Body`; non-file parts drained (≤1 MiB each, ignored per U1.2.18); the single `file` part is forwarded through an `io.Pipe` fed by a copy goroutine reading `io.LimitReader(part, cap+1)` — chunked transfer to agentd, no `ReadAll` of the file part anywhere. Over-cap (chunked or spoofed CL) closes the pipe with `errUploadOverCap`, aborting the agentd request mid-stream → 413 (U1.2.5/U1.2.14). Client disconnect: the agentd request is a child of `c.Request.Context()`; cancellation aborts `Do`, and the pipe is closed with the failure so the copy goroutine and the agentd body read terminate (U1.2.6/I3).
- **Dispatch target**: `http://<podIP>:4097/v1/files?filename=<sanitized>` — `agentd.AgentdPort` (user mux), PUT, `SetBasicAuth(agentd.AuthUsername, password)`, `Content-Type: application/octet-stream`. Part headers are never forwarded (only filename-in-query + body bytes).
- **Filename sanitization (D9)**: `agentd.SanitizeFilename` — see Key Decisions #2.
- **Error mapping (U1.2.9)**: transport conn-refused → 502; timeout (per-request deadline / net.Error.Timeout / DeadlineExceeded) → 504; agentd 413 → 413; agentd 5xx/garbage/non-201 → 502 with fixed public text; agentd internals never echoed (asserted in tests).
- **Response**: 201 with the agentd-decoded `pkg/agentd.FileUploadResponse{Path,Name,Size}` re-emitted verbatim (decode capped at 64 KiB; undecodable 201 → 502, U1.2.19).
- **Metrics (U1.2.21)**: `llmsafespaces_uploads_total{reason}` with the closed reason set `{success, cap, phase, disk, agentd_error}` (+`unknown` guard); `RecordUploadRequest` / `UploadsCounter()` in api/internal/services/metrics, following the `secretAutoPushTotal` pattern. agentd-side 413 counts as `cap`; request-shape 400/415/404 are visible in `api_requests_total` by status (documented decision — auth rejections happen in middleware before the handler and are counted by the auth layer's own metrics).
- **Env knobs**: `UPLOAD_MAX_BYTES` (default 25 MiB) and `UPLOAD_TIMEOUT_MS` (floor 1 s, default 5 min) — mirroring agentd's own knob names (they run in different containers) and the DISK_* package-var pattern; test overrides via `SetUploadLimitsForTest`.

### Shared sanitizer extraction (duplication decision)

`sanitizeUploadFilename` in `cmd/workspace-agentd/uploads.go` (US-67.1, package main — not importable) was moved to `pkg/agentd/sanitize.go` as exported `agentd.SanitizeFilename` / `agentd.IsForbiddenFilenameRune` / `agentd.MaxFilenameBytes`. The agentd side keeps a one-line delegating wrapper, so US-67.1's tests (including the hostile table) pass unchanged and pin the wrapper. Zero duplication: both upload layers consume one implementation, and the hostile tables in `pkg/agentd/sanitize_test.go` and `cmd/workspace-agentd/uploads_test.go` are byte-identical by construction.

### OpenAPI / swag

- Task asked for swag annotations. **Finding**: this repo has NO swag toolchain — zero `@Router` annotations repo-wide, no swag binary/target in the Makefile; the spec is hand-maintained at `sdks/openapi.yaml` and enforced by `TestOpenAPIRouterContract` (route-presence diff router↔spec). Following repo convention, the route was added to `sdks/openapi.yaml` (`/workspaces/{id}/uploads`, multipart requestBody with binary `file` property, `FileUploadResponse` schema, 201/400/401/404/409+phase/413/415/502/504/507) and the swag-format annotation block was placed on the handler as documentation. `make openapi-validate` green; contract test green.

### Tests (TDD — written first, red before implementation)

- `api/internal/handlers/uploads_test.go` (~40 scenarios / 71 test cases incl. subtests): U1.2.1 happy path (bytes forwarded identical, sha-checked), U1.2.2 Basic auth + exact `http://10.0.0.1:4097/v1/files?filename=...` target (captured pre-redirect transport), U1.2.3 streaming proof (see Key Decisions #5), U1.2.4 local CL reject without dial, U1.2.5/U1.2.14 chunked + spoofed-CL overrun, exact-cap boundary (cap accepted / cap+1 rejected), U1.2.6 client-disconnect propagation (agentd read aborts, handler returns), U1.2.7 all 9 phases + Active-without-PodIP, U1.2.8 disk table (0.95/above gated, 0.949 passes, unknown total fails open), U1.2.9 error mapping (502/413/502/504), U1.2.10 malformed shapes incl. two-file-parts, U1.2.11 sanitize-before-forward table, U1.2.12 wrong method → 404, U1.2.13 empty filename, U1.2.15 disposition smuggling (raw CR/LF/ESC → 400 at the MIME boundary; CRLF header-split → the smuggled part header never reaches agentd), U1.2.17 content-type table (json/empty boundary/urlencoded/empty → 415), U1.2.18 non-file fields ignored, U1.2.19 garbage responses → 502 no leak, U1.2.20 CRD-gone → 404 no dial, U1.2.16 gate order (phase beats disk beats cap, metrics prove which gate fired), U1.2.21 metrics per reason, I5 agentd-down 502, I9 agentd-closes-mid-stream clean 502.
- `api/internal/server/router_uploads_test.go`: I1 (no auth → 401; unknown workspace → 404; foreign workspace → 403 — see Key Decisions #7) and U1.2.22 (route participates in the global rate-limit stack: burst 3 → 3 pass / 3× 429).
- `pkg/agentd/sanitize_test.go`: full hostile table on the shared implementation.
- `api/internal/services/metrics/metrics_test.go`: counter increments + unknown-reason guard.

### Test commands + results

- `go test -timeout 900s -race -count=1 ./api/... ./pkg/agentd/... ./cmd/workspace-agentd/...` → **PASS, 41 packages ok, 0 failures** (handlers included).
- `make openapi-validate` → valid. `make repolint` → all checks passed. `gofmt -l` on touched trees → clean.

---

## Key Decisions

1. **Agentd dispatch reuses ProxyHandler's plumbing, not agentpush.** `pkg/agentd/agentpush.go` does not exist (task caveat: "pkg/agentd/agentpush.go exists per US-67.1 report" — disproved; the reload-secrets push lives at `api/internal/services/agentpush/agentpush.go` and hardcodes `fmt.Sprintf("http://%s:4097/v1/reload-secrets", podIP)` for small buffered JSON POSTs). agentpush.Service is the wrong shape for a 25 MiB stream (it builds the whole payload in memory via InjectSecrets). The upload handler follows the AgentReloadHandler/agentpush *pattern* instead: pod IP from the Workspace CRD (same `Get` the proxy uses), password from `getPassword` (same Secret), `http.NewRequestWithContext` + Basic auth, but with an `io.Pipe` body for streaming. Evidence: agentpush.go:200, agent_reload.go:242, proxy.go:466.
2. **Filename sanitizer: exported to `pkg/agentd`, not duplicated in the API.** US-67.1's `sanitizeUploadFilename` is package-private in `cmd/workspace-agentd` (package main — unimportable). Duplicating it in the API would be exactly the two-copies-drift failure D9 exists to prevent, so the function moved to the shared `pkg/agentd` (already imported by both layers) with the agentd side delegating. The alternative (reimplement "minimally" in the API) was rejected under Rule 5 (zero tech debt).
3. **Timeout: 5 minutes, not the sibling 5 s.** The task suggested "5s-ish, follow sibling conventions" — but the siblings (agentpush, agent reload) post small buffered JSON; a total 5 s client timeout on a 25 MiB streaming PUT would abort legitimate in-cluster uploads (25 MiB at 5 MB/s ≈ 5 s). The API hop mirrors agentd's own body deadline (`defaultUploadBodyTimeout` = 5 min, US-67.1): the API never aborts a body agentd would still accept. Env-overridable (`UPLOAD_TIMEOUT_MS`, 1 s floor), field-overridable for tests (the 504 test uses 150 ms). The 504 *mapping* is what U1.2.9 requires and is asserted.
4. **Content-Length fast-reject uses a 64 KiB envelope allowance.** Rejecting when `CL > cap` would locally 413 a file of *exactly* the cap (multipart framing pushes the envelope past cap). The allowance covers preamble + boundaries + disposition with a max-length (200-byte) filename; the stream-side `cap+1` LimitReader remains the authoritative check, so any file > cap still 413s (just after a dial). Front-loading >64 KiB of ignored form fields to force a false local 413 is a pathological self-own; the drain cap would reject such bodies anyway.
5. **Streaming proof is a deadlock discriminator, not a timing sniff.** The producer writes 512 KiB + 512 KiB, then waits for a signal that the fake agentd has *received* ≥1 MiB before writing the tail. A fully-buffering API never dials agentd until the producer finishes — which it never does — so the handshake times out and fails the test. With the real streaming path the handshake completes and the sha256 of the agentd-received bytes equals the file. (The naive first draft signaled at EOF — a test bug, fixed with a threshold-signaling reader.)
6. **Two-file-parts rejection happens post-upload (documented orphan).** Detecting a second `file` part before streaming is impossible without buffering the whole body. The scan runs after agentd 201 but before the client is acknowledged: two parts → 400, and the already-atomically-stored first file becomes an ordinary workspace file — the same accepted-orphan semantics as D19 retries. The scan is bounded (1 MiB drain budget; tail read errors ignored).
7. **I1's "other user's workspace → 404" refined by the real middleware contract.** `WorkspaceAccessMiddleware` maps `ResolveWorkspace` NotFound → 404 (unknown ID) and `CheckOwnership` denial → 403 (known foreign workspace) — verifyOwner semantics, existing production behavior (workspace_access.go, workspace_service.go:1146). The router test asserts both; changing 403→404 for uploads alone would diverge from every other `:id` route.
8. **Part-level `Content-Length` is not trusted.** Multipart parts virtually never carry a reliable Content-Length, and a lying one (Go's transport errors when body ≠ declared length) would abort otherwise-valid uploads. The PUT is always chunked — "ContentLength when known" resolves to "never known mid-stream".
9. **Slowloris on the post-201 tail parks the request goroutine until client disconnect** — identical exposure to every body-reading handler in this codebase (the generic proxy reads the full body up-front). The scan's byte budget bounds CPU/IO; auth + the 10-connection-per-workspace cap bound goroutine count. agentd's own read deadline (US-67.1 U1.1.18) bounds the upstream hop.

---

## Assumptions → Validation

| Assumption | Validation |
|---|---|
| agentd `PUT /v1/files` serves on the **user mux :4097** with Basic auth (query param `filename`, 201 `agentd.FileUploadResponse`) | cmd/workspace-agentd/server.go:355 (`buildUserMux` registers `/v1/files` via `uploadFilesHandler`), uploads.go:202 (`r.URL.Query().Get("filename")`), uploads.go:285 (201 + `agentd.FileUploadResponse`); README epic-67 D1 correction |
| Phase + disk come from the Workspace CRD status fetched via the proxy's K8s client | proxy.go:359-392 (workspace CRD get + `c.Get("workspace")` cache pattern — mirrored); workspace_types.go:380-381 (`DiskUsedBytes`/`DiskTotalBytes`); ratio via `systemnotices.Ratio` (proxy_disk_pressure.go:58) |
| The user mux is reachable from the API at `<podIP>:4097` | api/internal/services/agentpush/agentpush.go:200 (reload-secrets dispatches to `http://<podIP>:4097/...` in production today); NetPol allowance documented at pkg/apis/llmsafespaces/v1/workspace_types.go:60 |
| Password for agentd Basic auth = K8s Secret `workspace-pw-<id>` key `password` | proxy_connections.go:94-116 (`getPassword`), uploads.go:188-195 (`checkBasicAuthAny(r, workspacePassword, …)` accepts it) |
| Critical disk threshold = 0.95, env-overridable, shared with the notice injector | systemnotices.go:38-63 (`Thresholds()` single source; proxy_disk_pressure.go delegates) |
| No swag toolchain exists; spec is hand-maintained + contract-tested | repo-wide grep (zero `@Router`), Makefile (no swag target), sdks/openapi.yaml + router_openapi_contract_test.go |
| Routes on idGroup inherit the global rate-limit middleware | router.go:372 (`router.Use(RateLimitMiddleware(...))` before route registration) — asserted behaviorally in router_uploads_test.go |
| `multipart.Part` streams lazily (no full-body parse) | Go stdlib mime/multipart Reader semantics; empirically pinned by the streaming test's handshake |
| Raw CR/LF/ESC in a Content-Disposition value cannot reach the filename logic | Empirically verified against Go's parser (malformed MIME header line error) — encoded as the smuggling test's 400 expectations |

## Integration-row coverage (CI has no cluster)

| Row | Coverage |
|---|---|
| I1 auth/access | **Full** (router_uploads_test.go: 401 / 404 / 403) |
| I2 real handler ↔ real agentd | **Partial** — the real agentd handler lives in package main (unimportable from api tests); covered against a behavior-matching fake (Basic-auth asserted, streamed bytes sha-verified, 201 shape). The real agentd side is covered by US-67.1's own suite. A cross-binary vertical would need an e2e (E-tier). |
| I3 cancellation | **Full** at httptest level (agentd read abort observed) |
| I4 concurrency 8 parallel | **Partial** — `-race` across the suite exercises concurrent uploads implicitly; a dedicated 8-parallel case adds little beyond US-67.1's 32-parallel disk-level test. Noted as follow-up with E-tier. |
| I5 agentd down | **Full** (502, no panic, phase still Active fixture) |
| I8 disk 96% → 507 | **Full** at handler level (send-path non-interference is by construction: the gate lives only in the upload handler; the send path is untouched by this diff) |
| I9 phase race mid-upload | **Partial** — modeled as agentd closing the stream mid-body (clean 502); a true suspend-during-upload needs a live controller (E-tier). |

## Deviations

- **swag**: annotations written as doc comments, but the spec entry that actually lands the route is the hand-maintained `sdks/openapi.yaml` (repo has no swag toolchain — see Key Decisions).
- **Timeout 5 min vs "5s-ish"** (Key Decisions #3).
- **agentpush reuse**: reused the *pattern* + ProxyHandler plumbing, not the service (wrong shape for streaming — see Key Decisions #1).
- **Two-file-parts orphan semantics** (Key Decisions #6).

## Adversarial self-review (Rule 11)

- Copy-goroutine/pipe lifecycle on every error path re-walked: `Do` error → `pr.CloseWithError` + `<-copyCh` drain (no leak; transport also guarantees request-body close); over-cap-after-headers → `resp.Body.Close()` + 413; part-CL trust removed (lying header aborted valid uploads). All fixed in-session.
- Deadlock audit of the three-goroutine pipeline (client producer → mr/copy → transport/agentd): the only intentional blocking is the bounded post-scan tail (documented) and the `<-copyCh` drain (terminates because agentd responds only after EOF and every failure path closes the pipe).
- Metric reason set closed `{success,cap,phase,disk,agentd_error}` per task; 400/415/404 outcomes deliberately uncounted here (visible in `api_requests_total`) — documented, not silent.
- False alarm dismissed: `c.Get("workspace")` never set in production — harmless legacy pattern mirrored from proxy.go for test compatibility.
