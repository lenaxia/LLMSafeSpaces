# Worklog: US-65.3 — opencode Adapter + filediff prototype + pure translator

**Date:** 2026-08-10
**Session:** US-65.3 — implement the opencode-side Adapter, resolve the design §10 FileChange production-path open question, deliver the contract translation layer.
**Status:** Complete (Stream method explicitly deferred to US-65.4)

---

## Objective

Per design 0049 §4.6 and the epic-65 README, US-65.3 implements the opencode-side Adapter that translates opencode's wire shapes to/from the `pkg/session` contract landed in US-65.2. The Adapter folds the existing `AgentConfigWriter` (US-65.1) plus 16 new methods (sessions, messaging, streaming/input, models, capabilities, credentials) into one seam.

Design 0049 §10 flagged one open risk to prototype before committing: does opencode's `session.diff` SSE event carry enough data to produce `FileChange` hunks, or does the adapter need to `git diff` on the PVC?

Three deliverables:

1. Resolve the design §10 FileChange risk (prototype).
2. Land the Adapter interface in `pkg/agent/adapter.go` (folds AgentConfigWriter).
3. Implement the Adapter against opencode's HTTP API.

---

## Assumptions (Rule 7) — stated and validated

1. **opencode's `session.diff` event carries only file paths, not diff hunks.** **Validated:** worklog 0201 row 14 (`{"type":"session.diff","properties":{"files":[...]}}` — "Forwarded only / Ignored") and worklog 0069 line 183 (`session.diff | File changes (empty array if none)`). The hunks must come from `git diff` on the PVC.
2. **Workspace must be a git repo for FileChange parts.** **Validated:** grep'd `controller/` and `runtimes/` — no `git init` calls today. The PVC starts empty or with whatever was committed previously. Prerequisite work (init `/workspace` as git repo on creation + commit-per-turn cadence) is flagged as a separate US-65.3 follow-up — not in this PR's scope.
3. **`git` binary is in the runtime image.** **Validated:** `runtimes/base/Dockerfile` installs git via `apt-get install ... git ...`.
4. **opencode's V2 prompt endpoint accepts `{prompt:{text:"..."}}` (not the parts-based contract shape).** **Validated:** F18 in epic-63 README. The Adapter's `SendAsync` calls `c.PromptV2(...)` which already uses the text shape.
5. **Stream (SSE event translation) belongs in US-65.4, not US-65.3.** **Validated** by reading epic-65 README's US-65.4 scope ("proxy handlers call Adapter, not opencode shapes"). Stream is consumed by `proxy_events.go`'s SSE bridge today; migrating it requires rewriting the proxy (US-65.4's job), not just implementing a method. Documented explicitly in the Stream docstring.

---

## Work Completed

### Commit 1 — `pkg/agent/adapter.go` (interface) + filediff prototype

- `pkg/agent/adapter.go` — the `Adapter` interface. Embeds `AgentConfigWriter` from US-65.1. Adds 16 new methods. Composition rule: existing `AgentRuntime`/`Dialect`/`AgentClient` are NOT embedded — they carry agent-specific shapes (`map[string]any` config patches, opencode path strings, `[]byte` raw model lists) that Adapter exists to eliminate. Their methods become private to the opencode adapter.
- `pkg/agent/opencode/filediff/filediff.go` — Producer wrapping `git diff HEAD` against the workspace PVC. Pure-Go shell around `git`, no commit-cadence ownership. Holds workspace mutex via per-call lock-free `exec.CommandContext`.
- `pkg/agent/opencode/filediff/filediff_test.go` — 12 tests against real git repos in `t.TempDir()`. Covers: no-op, modified file, multiple files, new file (uses `git add -N` intent-to-add — see F1 below), deleted file, binary file, subdirectory paths, paths with spaces, empty file list, missing repo, relative-path rejection.

**F1 — Real finding from the prototype:** untracked new files don't appear in `git diff HEAD` output. opencode creates new files via its tools; the Adapter must call `git add -N <paths>` (intent-to-add) before diffing. Idempotent on tracked paths. Verified empirically against git 2.39.5.

### Commit 2 — `pkg/agent/opencode/adapter.go` + pure translator

- `pkg/agent/opencode/adapter.go` — `*Adapter` implementing 16 of the 17 non-config Adapter methods. Construction via `NewAdapter(pw, ip, logger, opts...)`. `WithAdapterHTTPClient` for connection pooling; `WithAdapterPort` for test override; `WithFileDiffProducer` for FileChange production.
- `pkg/agent/opencode/adapter_helpers.go` — `doPost`/`doGet`/`readBody`/`httpError`/`parseProviderCatalogForContract`. Shared HTTP plumbing.
- `pkg/agent/opencode/translate.go` — pure functions translating opencode wire shapes to `session.*` contract types. Discipline:
  - 5 part types forever (Text/Reasoning/Tool/FileChange/Custom)
  - opencode's `patch` part collects file paths for filediff (NOT a contract type)
  - step-start/step-finish dropped (turn-boundary markers carry no renderable content)
  - Unknown opencode part types preserved as `Custom` with `Kind` set to the opencode type string (design 0049 §4.3 pressure-relief valve — future extensions surface in UI rather than silently disappearing)
- `pkg/agent/opencode/translate_test.go` — 22 tests pinning every translation rule. Table-driven where applicable.
- `pkg/agent/opencode/adapter_test.go` — 16 integration tests driving the Adapter against `httptest.Server` mocks. Validates: ListSessions, GetSession, CreateSession, RenameSession, DeleteSession, Send, SendAsync (V2 prompt), Abort (V2 interrupt), GetHistory, ListAvailableModels, SetModel, Capabilities, ListPending, error handling (5xx/404/no-pod), auth wiring.

### Stream method

Returns `fmt.Errorf("not implemented — lands in US-65.4 ...")`. Documented scope: SSE event translation belongs with the proxy rewrite, not this story. US-65.3's "Done when" requires synchronous session round-trip; streaming is a separate migration.

---

## Key Decisions

1. **Adapter does NOT embed `Dialect`/`AgentRuntime`/`AgentClient`.** design 0049 §4.6 envisions Adapter as the *replacement* for those three, not a composition. Their methods (path strings, raw config maps, raw byte slices) carry opencode-specific shapes that Adapter exists to eliminate. The opencode adapter makes them private helpers; platform code calls platform-shaped methods. This is the C2 ("HTTP API contract") coupling from design 0049 §2 being properly contained.

2. **Stream is explicitly deferred to US-65.4.** Scope management — the design doc's US-65.4 ("Migrate proxy handlers to Adapter") is where the SSE bridge gets rewritten. Implementing Stream here without the proxy rewrite would mean dead code (no caller) plus a half-coupled SSE parser. The reviewer can verify this is honest deferral, not hand-waving, by reading the Stream docstring.

3. **FileChange parts are produced via `WithFileDiffProducer`, optional.** When nil (the API-side Adapter has no PVC access), `Send`/`GetHistory` skip FileChange production silently — the patch part's file paths are still collected by the translator but never rendered. When wired (agentd-side construction), `fileChangeParts(ctx, files)` runs `git diff` and appends `PartFileChange` parts. This is the C4 ("storage/path contract") coupling being contained: the agentd process has filesystem access; the API process does not.

4. **`parseProviderCatalogForContract` filters to `connected` providers.** opencode's `/provider` returns every models.dev entry in `all[]`; only `connected[]` providers have live credentials. Surfacing unconnected providers' models would mislead the UI into thinking they're usable. Mirrors `fetchFreeModels`'s pattern in `relay_injector.go`.

5. **`Resolve` tries question-reply first, then permission-reply.** The Adapter does not know which kind a `requestID` refers to without a `ListPending` round-trip. The caller learns the kind via `ListPending` and could call a kind-specific method; the simple case accepts the cost of one extra call. Future tightening: add `ResolveQuestion(ctx, id, reply)` and `ResolvePermission(ctx, id, reply)` if callers need it.

6. **Adapter struct fields are private; construction only via `NewAdapter`.** Mirrors `WorkspaceClient`. Platform code holds `agent.Adapter` (interface), never the concrete type.

---

## Adversarial Self-Review (Rule 11)

| # | Finding | Class | Resolution |
|---|---|---|---|
| F1 | Untracked new files invisible to `git diff HEAD` (prototype) | Real bug, fixed in prototype | **Fixed.** Producer runs `git add -N` (intent-to-add) before diffing. Idempotent on tracked paths. Test `TestDiffFiles_NewFileNotInHEAD_ProducesDiffWithEmptyOldSide` pins it. |
| F2 | First `translateTool` implementation left `tp.State.Status` empty when `t.State == nil` | Real bug (test caught it) | **Fixed.** Default to `ToolStatusPending` (safe default — UI renders "working"). |
| F3 | `TestTranslateMessage_PatchPart_CollectsChangedFiles` initially asserted wrong text | Test bug | **Fixed.** Test now asserts the actual text content. |
| F4 | Stream returns "not implemented" — could this break a caller? | Acceptable (documented scope) | No fix. Stream's only consumer is `proxy_events.go`'s SSE bridge, which is rewritten in US-65.4. The Adapter is not yet wired into any caller; "not implemented" is honest scope management. |
| F5 | `Resolve` does an extra HTTP call when the first kind is wrong | Acceptable tradeoff | No fix. Caller can branch on `InputRequest.Kind` and call a future kind-specific method if needed. Today the simple case wins. |
| F6 | `adapterSessionMethods` placeholder interface removed from `adapter_helpers.go` after build error | Cleanup | Done. |
| F7 | No file close errors checked — codebase convention is `//nolint:errcheck` | Real (lint) | **Fixed.** All `resp.Body.Close()` calls annotated per codebase pattern. |
| F8 | Empty `if m.Info.Title != ""` branch in translate.go (staticcheck SA9003) | Real (lint) | **Fixed.** Removed leftover placeholder. |
| F9 | `srv.Server.Config.Handler` should be `srv.Config.Handler` (staticcheck QF1008) | Real (lint) | **Fixed.** |
| F10 | No real-pod e2e test (httptest.Server mock only) | Acceptable (US-65.4 scope) | The mock exercises the full HTTP + translation stack end-to-end. A real-pod e2e lands with US-65.4 when the proxy is migrated — that's where the actual user-facing flow gets exercised. |

**Phase 2 result:** zero unresolved real findings in US-65.3 scope.

---

## Blockers

None.

---

## Tests Run

- `go build ./...` — clean
- `go vet ./pkg/agent/...` — clean
- `gofmt -l pkg/agent/` — clean
- `golangci-lint run --new-from-rev=origin/main ./pkg/agent/opencode/` — 0 issues
- `go test -timeout 30s -count=1 -race ./pkg/agent/opencode/filediff/` — PASS
- `go test -timeout 60s -count=1 -race ./pkg/agent/opencode/` — PASS (12.8s with race detector)
- `go test -timeout 60s -count=1 -race ./pkg/agent/...` — PASS (all packages)
- Pre-commit hooks (repolint, gofmt, goimports, golangci-lint) — all pass

---

## Next Steps

US-65.3 is complete (Stream explicitly deferred to US-65.4 — documented scope). Per Epic 65 sequencing:

1. **US-65.4 (proxy migration)** — the next story. Rewrites `proxy.go`/`proxy_handlers.go`/`proxy_events.go`/`proxy_input.go`/`proxy_permissions.go` to call `Adapter` instead of translating opencode shapes inline. Also implements `Adapter.Stream` (SSE bridge).
2. **US-65.5 (delete hacks)** — gated on US-65.4. Removes `proxy_filter*.go`, opencode-shape history parsing, inline question/permission translation.
3. **Workspace init-as-git-repo** (small follow-up) — `controller/internal/workspace/pod_builder.go` runs `git init` in `/workspace` on first pod boot if `.git` is absent. Prereq for FileChange parts in production.

---

## Files Modified

**Created:**
- `pkg/agent/adapter.go`
- `pkg/agent/adapter_test.go`
- `pkg/agent/opencode/adapter.go`
- `pkg/agent/opencode/adapter_helpers.go`
- `pkg/agent/opencode/adapter_test.go`
- `pkg/agent/opencode/translate.go`
- `pkg/agent/opencode/translate_test.go`
- `pkg/agent/opencode/filediff/filediff.go`
- `pkg/agent/opencode/filediff/filediff_test.go`
- `worklogs/0721_2026-08-10_us-65-3-opencode-adapter.md` (this file)
