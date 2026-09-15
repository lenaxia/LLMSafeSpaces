# Worklog: Vision capability gating for text-only models (#1307)

**Date:** 2026-09-15
**Session:** Fix issue #1307 — text-only model sessions wedge permanently when a tool result carries an image into the replayed history (glm-5.3/zai 400 on every turn).
**Status:** Complete

---

## Objective

Close the three product gaps the incident exposed: (1) no capability gating on tool-result images, (2) no recovery path for a wedged session (raw provider error on every turn), (3) the model catalog does not surface vision capability so clients cannot warn.

---

## Work Completed

### 1. Catalog capability surfacing (the registry)

- `CatalogModel` gains `SupportsVision *bool` (`api/internal/handlers/model_catalog.go`); the opencode `/provider` parser now reads three optional per-model metadata shapes (see A1) and resolves them with precedence `capabilities.input.image` > `attachment` > `modalities.input` containing `image`.
- `annotatedModel` gains `SupportsVision *bool` (`supportsVision,omitempty`) — known true/false serialize; unknown (absent metadata) omits the key entirely. `GET /workspaces/:id/models` surfaces it; `sdks/openapi.yaml` `ModelItem` documents it (nullable); `make -C sdks sdk-check` green.
- Frontend: `ModelInfo.supportsVision?: boolean | null` + a "text-only" badge in the ModelSelector dropdown when `supportsVision === false`.

### 2. Send-path wedge classification

- `pkg/agent/errors.go`: new sentinel `ErrImageInTextOnlyHistory` (wraps alongside `ErrHTTPStatus` so the #987 at-least-once semantics keep working — definitive rejection, no blind retry).
- `pkg/agent/opencode/vision_gate.go`: `textOnlyWedgeError` classifies the live-verified 400 signature (`messages.content.type is invalid`, tight full-phrase match) on the message-send funnels (`Adapter.httpError`, V2 prompt path) and renders the actionable error (cause + switch-model remediation).
- `api/internal/handlers`: `SendMessage` and `syncSend` return `422 {"code":"text_only_model_image_history", ...}` naming the recovery instead of the generic 502.
- Frontend `sessionErrorText` maps the code AND the raw provider-body pattern (the async/SSE surface carries no code) to the switch-model guidance.

### 3. Read-time repair for already-wedged sessions (#1374 pattern)

- `pkg/agent/opencode/translate.go`: file parts now translate explicitly to `Custom{kind:"file"}` carrying metadata ONLY (mime/filename). The V2 content-part path previously rode `Raw` verbatim into `Custom.Data` — leaking the full base64 data URL into every served history page; both paths now strip the blob at translate time (derive the image mime from a `data:image/...` URL when no mime is declared — the incident's minimal `{type:"file", uri:...}` shape). `translateV2Tool` strips embedded image dicts from `state.structured` (the runbook's structured copy) to metadata markers.
- `pkg/agent/opencode/vision_gate.go`: `repairTextOnlyHistoryImages` — when the served page carries image-bearing content AND the session's active model is KNOWN text-only, the image parts are downgraded to an explicit omission notice naming the recovery. STRICT failure semantics (any session-model or capability fetch error → transcript untouched; unknown capability → untouched). Scan-first: zero remote calls on pages without image parts. Wired into `getHistoryV1` and `getHistoryV2Store` next to `repairOrphanedRunningTools`.
- `pkg/agent/opencode/loopback.go`: `ModelInfo` gains `ImageInputKnown` (tri-state — absent capabilities are UNKNOWN, never false); new `SessionModelRef` seam method (GET /session/:id → model ref, shape pinned by `testdata/session_get_1_18_10.json`).
- `cmd/workspace-agentd/mcp_tools.go`: the `call_with_model` vision pre-check now refuses only on KNOWN-false capability (unknown no longer refuses image calls on custom vision gateways).

### 4. Docs

- `docs/getting-started/concepts.md` §Session: text-only models and image-bearing history — the wedge mechanism, the catalog field, the recovery.
- `README-LLM.md` known fragilities #5: the wedge class, the platform containment, the out-of-repo litellm fix, and the runbook's non-obvious details (`pkill -9 -x`, dual-write into `session_message.data`).

---

## Key Decisions

1. **Where the gate lives** — catalog surfacing + send-time classification + read-time repair, all platform-side behind the existing seams (`ModelCatalogParser`/`annotateModels`, `pkg/agent/opencode`, proxy handlers). The wedge's actual replay is opencode-internal; the platform cannot intercept it (no message-edit API — confirmed by the issue), so prevention at the provider boundary is the external litellm pre-call hook (filed out-of-repo per the issue triage). What the platform CAN do — warn before (picker), classify at (422), and repair after (honest transcript + named escape) — is implemented.
2. **Fail-safe direction: unknown ⇒ vision-capable (pass-through, never strip).** A false "text-only" silently strips user images from vision-capable models on custom gateways (irreversible data loss); a false "vision" preserves status-quo behavior for unknowns, now covered by detection + classified error + escape hatches. Implemented consistently: catalog (`nil`), `ModelInfo` (`ImageInputKnown`), the repair, and the `call_with_model` pre-check.
3. **Tight classification signature** — the full phrase `messages.content.type is invalid` (verified 1:1 against live litellm per the issue). Everything else falls through to existing error surfaces; no heuristic matching on generic 400s.
4. **Repair mirrors the runbook's surgery, at serve time** — image dicts replaced by an explicit text placeholder, file parts downgraded to metadata + omission notice. The harness's durable store is never written (read-time repair, the #1374 precedent).

### Assumptions stated and validated (Rule 7)

- **A1 — three capability wire shapes on opencode model entries.** Validated: `capabilities.input.image` (live-validated 2026-09-13, docs/testing/agentd-mcp-tools-test-plan.md §2 row "Model catalog"); `attachment` (pkg/agent/opencode/testdata/opencode-config.schema.json, ProviderConfig.models.additionalProperties.attachment: boolean); `modalities.input` (the issue body's own "capabilities.input: []" registry quote). Parser reads whichever is present; all absent → unknown.
- **A2 — `GET /session/:id` returns `model.{id,providerID}`.** Validated: pkg/agent/opencode/testdata/session_get_1_18_10.json (pinned live capture).
- **A3 — the wedge 400 body signature.** Validated: issue body ("verified 1:1 against the live litellm: a request with an assistant/user content part of any non-text type returns exactly this 400").
- **A4 — image carriers in history are file parts (V1) and file content parts + `state.structured` copies (V2).** Validated: issue body + recovery runbook (the image exists in BOTH the event payload and a structured copy). A hypothetical V1 plain-string data-URL tool output is NOT an evidenced shape and is not detected (documented here, not silently assumed).
- **A5 — the platform's own prompt path never emits image parts.** Validated: `attachments.Compose` composes file references into TEXT; `/message` rejects `files` outright (`rejectMessageRouteFiles`); the only platform-originated image send is the `call_with_model` MCP tool (already gated, now tri-state-correct).

---

## Blockers

None. (One transient: `TestLive_Worklogs_NoDuplicates` failed locally mid-session — the post-merge numbering bot had not yet run for #1374/#1375; it self-healed with commit 2e839cd6 and the suite is green on the rebased branch.)

---

## Tests Run

- `go test -timeout 120s ./api/internal/handlers/ -run "TestOpencodeProviderParser|TestAnnotateModels|TestListModels"` — ok
- `go test -timeout 120s ./api/internal/handlers/ -run "TextOnlyWedge"` — ok (3 e2e: sync message 422, generic-400 stays 502, /prompt fallback 422)
- `go test -timeout 120s ./pkg/agent/opencode/ -run "TestGetHistory|TestAdapterSend|TestAdapterSendAsync"` — ok (11 repair/translate rows + 4 classification rows)
- `go test -timeout 30m ./...` — ok (full repo)
- `go test -timeout 900s -short ./cmd/workspace-agentd/...` — ok
- `golangci-lint run` — 0 issues
- `make -C sdks sdk-check` — ok (spec valid + router parity)
- `cd frontend && npx vitest run` — 1825 passed (166 files), including the new text-only badge and SSE-error mapping rows
- `go build ./...` — ok

---

## Next Steps

- The litellm pre-call hook (drop non-text content for `supports_vision:false` models) is filed against the external talos-ops-prod config — the only candidate that stops the poison before opencode burns retries. Nothing further actionable here until that lands.
- If a future opencode adds a message-edit/delete API, the read-time repair can become a true write-back repair; revisit then.

---

## Files Modified

- api/internal/handlers/model_catalog.go
- api/internal/handlers/model_catalog_test.go
- api/internal/handlers/models.go
- api/internal/handlers/models_test.go
- api/internal/handlers/proxy_chat_enrichment.go
- api/internal/handlers/proxy_handlers.go
- api/internal/handlers/vision_wedge_test.go (new)
- cmd/workspace-agentd/mcp_tools.go
- docs/getting-started/concepts.md
- frontend/src/api/workspaces.ts
- frontend/src/components/chat/ModelSelector.tsx
- frontend/src/components/chat/ModelSelector.test.tsx
- frontend/src/pages/ChatPage.tsx
- frontend/src/pages/ChatPage.sse.test.tsx
- pkg/agent/errors.go
- pkg/agent/opencode/adapter.go
- pkg/agent/opencode/adapter_helpers.go
- pkg/agent/opencode/client_v2.go
- pkg/agent/opencode/loopback.go
- pkg/agent/opencode/translate.go
- pkg/agent/opencode/vision_gate.go (new)
- pkg/agent/opencode/vision_gate_test.go (new)
- README-LLM.md
- sdks/openapi.yaml
