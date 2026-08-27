# Worklog: US-67.1 — agentd file-ingest endpoint (PUT /v1/files)

**Date:** 2026-08-27
**Session:** Implement Epic 67 US-67.1 — the agentd `PUT /v1/files` upload endpoint (sanitize → atomic tmp+fsync+rename under /workspace/uploads), TDD-first, with boot scrub, squat defenses, cap, read-deadline, and metrics.
**Status:** Complete

---

## Objective

Add the workspace file-ingest API to agentd per `design/stories/epic-67-chat-file-attachments/README.md` (D1–D4, D9, D14, D19; test plan §1.1 scenarios U1.1.1–U1.1.21): streamed request body → sanitized filename → `/workspace/uploads/<uuid-v4>-<name>` via `O_EXCL` create + fsync + rename, with Basic auth, 25 MiB cap, slowloris read deadline, boot scrub of stale `*.tmp`, and Prometheus counters.

---

## Work Completed

### `PUT /v1/files` handler (cmd/workspace-agentd/uploads.go)

- **Route**: registered on the **user mux (port 4097)** in `buildUserMux` (server.go), symmetric with `/v1/reload-secrets` — see Key Decisions #1 for the port-4098 deviation.
- **Auth**: `checkBasicAuthAny(r, workspacePassword, controlPlanePassword)` — the design-0051 §D1 control-plane credential set, identical to reload-secrets (secrets.go:874). 401 via `rejectUnauthorized` before anything is written.
- **Sanitizer** (`sanitizeUploadFilename`): basename flatten (both `/` and `\` treated as separators) → strip control chars (C0/C1 via `unicode.IsControl`, covering `\n` `\r` `\x1b` NUL etc.), bidi/RTL overrides (U+202A–U+202E, U+2066–U+2069), double/single quotes, backslash, residual slash → trim trailing dots/spaces → truncate to 200 bytes on a rune boundary → re-trim → reject empty/whitespace-only with 400.
- **Atomic write**: `os.OpenFile(tmp, O_CREATE|O_EXCL|O_WRONLY, 0644)`; `EEXIST` (plain-file or symlink squat — D14) retries with a fresh uuid (3 attempts); body streamed with `io.Copy` from `io.LimitReader(cap+1)`; over-cap ⇒ close, remove `.tmp`, 413; `Sync()` → `Close()` → `os.Rename` to final; any mid-write error ⇒ close + remove `.tmp` and map status (ENOSPC→507, deadline/timeout→504, other IO→500; verified generic error bodies leak no paths — U1.1.20).
- **Cap**: 25 MiB default, `UPLOAD_MAX_BYTES` env override (overflow-guarded).
- **Timeout**: connection read deadline via `http.NewResponseController(w).SetReadDeadline` (5 min default, `UPLOAD_TIMEOUT_MS` env override, 1000 ms floor — sibling `MEMORY_*` env conventions); stalls abort with 504 and no residue (U1.1.18).
- **Uploads root**: `LLMSAFESPACES_UPLOADS_PATH` override, default `agentd.UploadsPath = /workspace/uploads` (new const, pkg/agentd/types.go); dir auto-created 0755 idempotently.
- **Response**: typed `agentd.FileUploadResponse{Path, Name, Size}` — 201 JSON `{path,name,size}`.
- **Fault-injection seams** (typed, no globals): `fileUploadConfig{uuid, create, rename}`; `uploadSink` interface (Writer+Sync+Close).

### Boot scrub (D3)

- `scrubUploadTmpFiles(dir)` globs `uploads/*.tmp`, removes best-effort, returns count.
- Wired into BOTH boot paths: `main()` (single-container) and `runSidecarCommand()` (sidecar) before the muxes serve; count logged + counted in metrics.

### Metrics (ops_metrics.go)

- `workspace_agentd_file_uploads_total{workspace_id, outcome}` (accepted / rejected_name / rejected_cap / write_error / unauthorized).
- `workspace_agentd_upload_scrub_removed_total{workspace_id}`.
- Follows the promauto/opsMetrics singleton pattern; exposed on the existing admin-port `/metrics`.

### Testability refactor (server.go)

- Extracted `buildUserMux(bgCtx, bgWg, deps)` from `wireHTTPServers` (behavior-identical route set) so the live registration path is testable without binding ports 4097/4098.

### Pre-existing failures fixed (Rule 5)

- `TestSidecar_MarkerPathEnvOverride` failed on any host whose environment presets `LLMSAFESPACES_RESTART_MARKER_PATH` (this dev pod does): the "env unset → default" assertion now force-clears the var via `t.Setenv(key, "")` first.
- `TestWorkflowExecute_ScriptSuccess` required `python3` on PATH: now skips with an explicit reason when `exec.LookPath("python3")` fails (script-node runtime is a workspace-image dependency; error-path test unaffected).

---

## Key Decisions

1. **User mux (4097), not admin mux (4098) — documented deviation from the design doc's literal "(port 4098)".** The design doc's D1 anchors on symmetry with reload-secrets and its assumptions table claims "POST /v1/reload-secrets over 4098" — that claim is contradicted by the code: reload-secrets serves on the user mux at `agentd.AgentdPort` 4097 (server.go `wireHTTPServers`/`buildUserMux`; main.go:31 `listenAddr = agentd.AgentdAddr`; types.go:73) and the API dispatches to `http://<podIP>:4097/v1/reload-secrets` (api/internal/services/agentpush/agentpush.go:200). The byte-entry channel from the API is 4097 with Basic auth; the admin mux (4098) is the kubelet/controller probe surface behind bearer auth. Following the validated pattern (Rule 7: failed validation ⇒ finding), `/v1/files` serves on 4097 with the reload-secrets credential set.
2. **`name` in the response is the sanitized name, not the raw input** — echoing raw hostile bytes (control chars/RTL) back to clients would re-introduce the injection the sanitizer exists to stop. U1.1.2's "original name echoed" is interpreted as the upload's name post-sanitization (consistent with U1.1.3 expectations).
3. **Backslash treated as a path separator** (flattened during basename): Windows-shaped traversal (`..\..\x`) must flatten; a residual-backslash strip would be unreachable after the separator split. Both rules from the task are implemented; the separator interpretation wins for `a\b.txt` → `b.txt`.
4. **Quotes stripped = both `"` and `'`** — defense-in-depth for the (US-67.3) manifest line structure; real-world cost is trivial.
5. **Rename onto an adversary-guessed FINAL path is accepted (POSIX replace)** — the D14 argument (the in-pod agent cannot race 128 bits of per-request uuid) covers the final path exactly as it covers the `.tmp`; `O_EXCL` guards the creation, the uuid entropy guards the target.
6. **Sidecar mode is a known clean-fail**: the sidecar's `/workspace` mount is read-only (controller agentd_sidecar.go volume mounts), so writes fail `EROFS` → 500 "storage unavailable", nothing written. Full sidecar support needs a control-socket file-write op — flagged as follow-up, out of US-67.1 scope.
7. **Single quotes of the task's "admin server" wording**: see decision 1 — the task's "admin HTTP server (port 4098)" derived from the design doc's incorrect port claim; the reload-secrets pattern the task also mandates is the authoritative one.

### Assumptions stated and validated

| # | Assumption | Validation |
|---|---|---|
| A1 | reload-secrets serves on the user mux :4097 with Basic auth | cmd/workspace-agentd/server.go (`buildUserMux`/`wireHTTPServers`), main.go:31, pkg/agentd/types.go:73; api/internal/services/agentpush/agentpush.go:200-207 |
| A2 | Control-plane routes accept {agentdPassword, workspacePassword} via `checkBasicAuthAny` | cmd/workspace-agentd/auth.go:50-63, secrets.go:874, workflow_execute.go:84-89 |
| A3 | Atomic-write pattern is temp-file + Sync + os.Rename (same fs) | pkg/agent/opencode/configwriter.go:655-689 |
| A4 | Path overrides use `LLMSAFESPACES_*_PATH`; numeric tunables use bare env names | cmd/workspace-agentd/us4b_paths.go:21-53, memory_pressure.go:120-129 |
| A5 | google/uuid v1.6.0 is a direct dependency (v4 `uuid.NewString`) | go.mod:27; api/internal/services/auth/auth.go:23 |
| A6 | agentd has a Prometheus surface (promauto + admin /metrics) — counters required, not invented | cmd/workspace-agentd/ops_metrics.go:44-91, server.go `/metrics` |
| A7 | `O_EXCL` never follows symlinks; EEXIST triggers fresh-uuid retry | verified behaviorally by `TestUploadFilesHandler_TmpSquatSymlink` (symlink intact, sentinel outside file unchanged) |
| A8 | io.Copy to os.File may take os.File.ReadFrom — fault-injection sinks must not embed *os.File or Write is bypassed | discovered via failing test; fixed with named-field sinks (uploads_test.go comment) |
| A9 | Sidecar /workspace is read-only → uploads clean-fail in sidecar mode | controller/internal/workspace/agentd_sidecar.go (sidecar container volume mounts, `ReadOnly: true`) |

---

## Blockers

None.

---

## Tests Run

- TDD order honored: `uploads_test.go` + `pkg/agentd/types_test.go` additions written first; run confirmed RED (build failure on the then-undefined API); implementation made them GREEN.
- Scoped suite: `GOPROXY=direct GONOSUMCHECK=* GONOSUMDB=* go test -timeout 600s -race -count=1 ./cmd/workspace-agentd/... ./pkg/agentd/...` → **all ok** (cmd/workspace-agentd 337.5s, pkg/agentd 1.0s, pkg/agentd/secrets 1.3s), `-race` clean.
- US-67.1 scenario coverage: U1.1.1–U1.1.21 all covered (28 top-level test functions, 78 including subtests): happy path + sha256; path/name contract; hostile-name table (sanitizer + on-wire); cap boundary exact/+1; io.Pipe slow-writer stream; mid-write failure atomicity; rename-failure (dir collision, target untouched); boot scrub (incl. real-upload untouched); ENOSPC→507; auth table + control-plane credential; dir auto-create modes 0755/0644 (umask pinned); 32-parallel concurrency with per-file hash verification; fsync-before-rename ordering (op-recording sink); symlink squat; plain-file squat; EEXIST exhaustion; same-name-twice; slowloris (real server, read deadline); whitespace-only name; no-path-leak; metrics counters; env config table; user-mux wiring via `buildUserMux` on a real httptest server.
- `go vet ./cmd/workspace-agentd/... ./pkg/agentd/...` clean; `gofmt -l` clean; `go build ./...` (whole repo) passes.
- golangci-lint binary unavailable in this environment — not run (gofmt+vet used as the gate); flag for CI.
- Pre-existing failures `TestSidecar_MarkerPathEnvOverride` and `TestWorkflowExecute_ScriptSuccess` reproduced on untouched HEAD (git worktree baseline) and fixed as described above.

---

## Next Steps

1. US-67.2 (API upload route): dispatch to `PUT http://<podIP>:4097/v1/files?filename=<sanitized>` with the Basic credential set, streaming 25 MiB without buffering; agentd error mapping per U1.2.9.
2. Sidecar mode: add a control-socket file-write op (Appendix-A protocol extension) so uploads work when the sidecar's /workspace is read-only; until then uploads clean-fail 5xx in sidecar mode.
3. Fix the design doc's "(port 4098)" and the stale reload-secrets port in its Validated Assumptions table (D1 correction) when the doc is next touched.
4. Run `make lint` in an environment with golangci-lint installed.

---

## Files Modified

- `cmd/workspace-agentd/uploads.go` (new) — endpoint, sanitizer, config, scrub, error mapping
- `cmd/workspace-agentd/uploads_test.go` (new) — U1.1.1–U1.1.21 scenario tests
- `cmd/workspace-agentd/server.go` — extracted `buildUserMux`; registered `/v1/files`
- `cmd/workspace-agentd/main.go` — boot scrub call (single-container path)
- `cmd/workspace-agentd/sidecar_mode.go` — boot scrub call (sidecar path)
- `cmd/workspace-agentd/ops_metrics.go` — upload outcome + scrub counters
- `cmd/workspace-agentd/sidecar_mode_test.go` — env-pollution fix (pre-existing failure)
- `cmd/workspace-agentd/workflow_execute_test.go` — python3 LookPath skip guard (pre-existing failure)
- `pkg/agentd/types.go` — `UploadsPath` const, `FileUploadResponse` DTO
- `pkg/agentd/types_test.go` — `FileUploadResponse` JSON round-trip test
- `worklogs/0837_2026-08-27_us-67-1-agentd-file-endpoint.md` — this worklog
