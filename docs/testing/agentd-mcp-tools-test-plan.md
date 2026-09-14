# agentd MCP Tool Expansion — Test Plan

**Status:** L0/L1/L2 implemented and green; L3 scripted (`scripts/mcp-tools-liveprobe.sh`, 7/7 on the live pod 2026-09-13). The L2/L3 legs found and fixed two real defects: the silent no-op rename (POST vs PATCH — §2) and the unbounded health probe.
**Date:** 2026-09-13
**Scope:** The SEVEN NEW agent-side MCP tools on `/v1/mcp` (`rename_session`, `rename_workspace`, `call_with_model`, `create_session`, `get_datetime`, `session_metadata`, `compact`), the two PRE-EXISTING tools rerouted through the new seam (`session_list`, `session_read`), the seam itself (`pkg/agent/opencode` loopback methods), and the API-side `POST /internal/v1/workspace-rename`.
**Related:** worklog `worklogs/0923_2026-09-13_agentd-mcp-five-tools.md` (initial five) and `worklogs/0924_2026-09-13_agentd-mcp-tools-v2.md` (this revision)

---

## 1. What is being integrated

| # | Component | Lives in | Role |
|---|---|---|---|
| C1 | **Wire seam** (`pkg/agent/opencode` Client methods) | shared pkg | The ONLY code that knows opencode's HTTP shapes (Rule 12 containment). agentd tools call this, never raw URLs |
| C2 | **MCP tools** (`cmd/workspace-agentd/mcp_tools.go`, `mcp_server.go` JSON-RPC layer) | workspace pod / sidecar | Tool dispatch, input validation, result shaping |
| C3 | **opencode agent** (:4096) | workspace pod | The external dependency; pinned wire shapes below |
| C4 | **Platform API** (`/internal/v1/workspace-rename`) | api service | Workspace display-name writes (PostgreSQL owner) |
| C5 | **K8s TokenReview** | cluster | Pod identity for C4 |

## 2. Empirically validated wire contract (opencode 1.18.15, live pod, 2026-09-13)

Every shape below was exercised against the real binary in the live workspace pod (session `ses_f66ef51e3ffeFahEENfR3r2ia4`). These are the contract the tests pin.

| Fact | Endpoint | Evidence |
|---|---|---|
| Session create: `POST /session` `{title?}` → `{id,...}` | V1 | created S1 live; full list shape captured (id, title, agent, model{id,providerID,variant}, version, time{created,updated}, tokens{input,output,reasoning,cache{read,write}}, summary{additions,deletions,files}) |
| Rename: **`PATCH /session/{id}` `{"title"}`** — POST is accepted with 200 and IGNORED (silent no-op; found via L2, live-confirmed, adapter + seam fixed) | V1 | live probe + `TestLoopbackL2_SessionLifecycle` |
| Delete: `DELETE /session/{id}` | V1 | adapter `DeleteSession` |
| Message send: `POST /session/{id}/message` `{messageID?, parts, model?}` — synchronous, returns `{info, parts}` | V1 | live 200 in 2.7s with model override `{modelID, providerID}` — response `info.modelID` echoes the override |
| **Image parts ARE accepted**: `{"type":"file","mime":"image/png","filename":...,"url":"data:image/png;base64,..."}` rides `parts` | V1 | live 200; input tokens grew ~100 for a 1px PNG; the (non-vision) model replied "this model does not support image input" — bytes reached the model |
| **Busy sessions BLOCK incoming messages** (no 409, no queue on V1) | V1 | `GET /session/status` → `{ses: {type:"busy"}}`; second POST hung → HTTP 000 at 20s |
| Busy-block implies: a synchronous message to the CURRENTLY RUNNING session deadlocks by construction | — | structural consequence, pinned by `TestSeam_SessionSend_BusyBlocks` at the fake level + documented in tool descriptions |
| Compact: `POST /api/session/{id}/compact` → **503 "not available yet"** on 1.18.15 | V2 | live 503 (the actor's boot probe correctly treats it as absent) |
| **Compact (working): `POST /session/{id}/summarize` `{providerID, modelID}` → 200 `true`**; context collapses to the summary | V1 | live 200 in 6s (idle); `GET /api/session/{id}/context` → 1 in-context message after |
| **Run-at-boundary semantics**: summarize fired while busy QUEUES server-side and completes after the turn ends | V1 | live: summarize-during-busy returned 200 after 21s, exactly when the generation finished |
| Context window: `GET /api/session/{id}/context` → `{data: [{id,time,text,type}]}` | V2 | live |
| Messages pagination: `GET /session/{id}/message?limit=N` + `X-Next-Cursor` header; V2 twin `GET /api/session/{id}/message?limit&order&cursor` → `{data, cursor{previous,next}}` | V1+V2 | live |
| Busy map: `GET /session/status` → `{sesID: {type:"busy"\|"idle"}}` | V1 | live |
| Model catalog: `GET /config/providers` → per-model `limit.context` + `capabilities.input.image/attachment` | V1 | live (used for vision pre-check + context-limit fill %) |
| V2 prompt strips model overrides; V2 runner lacks MCP tools | V2 | #1292b, #1313 (repo goldens) — why call_with_model uses V1 |

**Wire-shape regressions this contract guards against:** the model-override object form (string 400s — #909), messageID upsert semantics (#1313/S2), file-part data-URL form, summarize-as-compact (V2 compact absence).

## 3. Design consequences under test

1. **call_with_model** runs its single synchronous call on a transient idle session (proven required: busy sessions block; the caller's session is definitionally busy while its tool call executes) and returns the text as the tool result — the exchange is in-line in the CURRENT session via the tool call + result channel, like every other tool. Session is deleted after; the description explains why (evidence-backed). Optional image inputs become file parts.
2. **compact** on an idle session is synchronous; on a busy session (e.g. the agent's own, mid-turn) it returns immediately and the summarize POST completes at the turn boundary (proven run-at-boundary semantics).
3. **create_session** is a top-level independent session; prompt delivery is a detached V1 POST with no artificial cap (the prior 30min bound served no correctness purpose and would orphan long turns).
4. **session_metadata** is read-only aggregation of already-exposed surfaces (session list, statuses, context count, model catalog) plus `WORKSPACE_ID` — no new information class crosses the boundary; no env values, credentials, or platform internals.

## 4. Level definitions

| Level | What runs | Where | Status |
|---|---|---|---|
| **L0 — unit** | Seam methods vs httptest fakes; tool funcs vs httptest fakes; API handler vs fakes | `pkg/agent/opencode/loopback_test.go`, `cmd/workspace-agentd/mcp_tools_test.go`, `api/internal/handlers/pod_workspace_rename_test.go` | ✅ green (CI) |
| **L1 — integration** | Full `mcpHandler` JSON-RPC path (auth → dispatch → seam → stateful fake opencode); rename_workspace full-stack vs live httptest API | `cmd/workspace-agentd/mcp_tools_test.go` (integration section), `TestMCPHandler_RenameWorkspaceFullStack` | ✅ green (CI) |
| **L2 — binary integration** | Seam + tools against a REAL opencode instance (spawned binary, temp project dir, offline mock OpenAI-compatible provider on a pinned port) | `pkg/agent/opencode/loopback_integration_test.go` (`-tags integration`, `OPENCODE_BINARY` override; 7/7 green 2026-09-13 on the live 1.18.15 binary) | ✅ green |
| **L3 — live-pod e2e** | The §2 evidence table, re-runnable as a script against any active workspace pod | `scripts/mcp-tools-liveprobe.sh` | ✅ 7/7 (2026-09-13; `LIVEPROBE_BUSY=1` enables the busy-block probe) |

## 5. L0 — unit matrix

### 5.1 Wire seam (`pkg/agent/opencode`)

| Test | Pins |
|---|---|
| `SessionCreate` happy/empty-title-omitted/non-2xx/no-id | route, body shape (`{}` vs `{title}`), error mapping |
| `SessionRename` happy/non-2xx/URL-build-fail | `POST /session/{id}` + `{"title"}` exactly |
| `SessionDelete` happy/non-2xx | DELETE route |
| `SessionSend` happy: model object `{modelID, providerID}`, text part shape, response parse (`info.id`, text parts joined) | the #909 object form |
| `SessionSend_WithImages` | file parts render as `{type:"file", mime, filename, url:"data:<mime>;base64,..."}` |
| `SessionSend_BusyBlocks` | a handler that sleeps → context deadline exceeded surfaces (the deadlock guard documented for call_with_model) |
| `SessionSend_ModelSplit` | `a/b`→(a,b); `a/b/c`→(a,b/c); bare/empty-tail rejected pre-call |
| `SessionSummarize` happy/503/non-2xx | `{providerID, modelID}` body; 503 = V2-absence class error |
| `SessionStatuses` | busy map parse |
| `SessionContextCount` | `{data:[...]}` parse |
| `SessionPromptTokens` | last-assistant input+cache.read+cache.write scan (Epic 36 formula) |
| `ModelInfo` | context limit + image capability from `/config/providers` |
| `SessionMessagesRaw` | passthrough + `X-Next-Cursor` honored for pagination |
| `SessionMessageCount` | bounded pagination walk with cursor termination |

### 5.2 Tools (`cmd/workspace-agentd`)

| Test | Pins |
|---|---|
| `rename_session` happy/missing/too-long/non-200 | seam called with trimmed title; result JSON |
| `rename_workspace` happy/missing-env/missing-token/4xx-family/5xx | SA-token bearer, exact internal route, body, error taxonomy |
| `call_with_model` happy (model object on the wire; text extracted)/bare-model rejected/empty prompt/image happy (data URL on the wire)/image unreadable/oversize image/non-image mime/model-without-image-capability pre-check/no-text-parts/create-fail/message-fail-still-deletes | the full contract incl. cleanup-on-failure |
| `create_session` happy (returns before turn completes; delivery observed)/empty prompt/title optional (omitted when empty)/create-fail | fire-and-forget timing pin |
| `get_datetime` shape | UTC + local + offset + zone, RFC3339 round-trip |
| `session_metadata` (all-sessions shape + per-session)/context fill %/busy flags/workspace id present/**secrecy: no env, no password, no token material, no internal URLs in output** | the security contract |
| `compact` idle (synchronous 200)/busy (returns immediately, POST detached, completes after idle — proven via channel)/summary-model default = session model/non-2xx | run-at-boundary semantics |
| Description-guidance subtests for every tool | descriptions are the only docs agents see (existing convention) |

### 5.3 API handler (`pod_workspace_rename_test.go` — shipped earlier, listed for completeness)

Auth matrix (missing/rejected/expired token, SA/namespace/principal mismatch), name validation (empty/whitespace/256/trim), lookup nil/error, rename error, malformed body.

## 6. L1 — integration matrix (`mcpHandler` end-to-end)

A stateful fake opencode (in-memory sessions map, busy set, message log, image-part recorder) behind httptest; every test drives REAL JSON-RPC through `mcpHandler` with Basic auth.

| Test | Flow |
|---|---|
| `tools/list` | all 9 tools present with input schemas |
| `session_list`/`session_read` | JSON-RPC → seam → fake; non-200 mapping |
| `rename_session` full-stack | JSON-RPC → fake state mutated |
| `rename_workspace` full-stack | JSON-RPC → agentd tool → live httptest API (route/bearer/body pinned) |
| `call_with_model` full-stack | JSON-RPC → create+send+delete sequence observed on the fake; image bytes → data URL on the wire; model override object form |
| `create_session` full-stack | JSON-RPC returns pre-delivery; async POST lands |
| `compact` full-stack idle + busy | busy: tool returns immediately; summarize POST observed after turn-end signal |
| `session_metadata` full-stack | aggregates fake state; fill % vs fake catalog limit; secrecy asserts |
| auth gate | every tool behind Basic auth (existing `RequiresAuth` + one per-tool probe) |

## 7. L2 — binary integration (`-tags integration`)

Spawn the real opencode binary (temp dir, random port, temp auth) — same harness discipline as `opencode_integration_test.go`; `OPENCODE_BINARY` env override.

| Test | Validates |
|---|---|
| create → send(model override) → response model echo | the synchronous single-call primitive on the real agent |
| send with image part (1px PNG data URL) | part accepted by the real parser (schema-level, model-independent) |
| busy map flips busy↔idle around a real turn | the status signal |
| summarize on idle session → context collapses | compact mechanism |
| statuses/context/messages pagination | metadata inputs |
| delete → list empty | cleanup |

L2 intentionally does NOT assert model OUTPUT content (provider-dependent) — only transport/shape/acceptance.

## 8. L3 — live-pod probe script

`scripts/mcp-tools-liveprobe.sh` encodes the §2 curls (create scratch → each probe → delete) against `127.0.0.1:4096` with the pod password; documents expected codes. Manual leg for incidents + pre-upgrade pins.

## 9. Security test matrix (session_metadata + all tools)

| Concern | Test |
|---|---|
| Tool surface auth | every JSON-RPC call without Basic → 401 (existing gate, per-tool probe) |
| No credential material in any tool output | `session_metadata`/`get_datetime`/etc. outputs asserted free of: password, SA token, `Authorization`, env values beyond `WORKSPACE_ID`, in-cluster URLs |
| session_metadata adds no new info class | outputs ⊆ {session list fields, busy flags, context count, catalog limits, WORKSPACE_ID, agent version} |
| Image reads stay workspace-local | path traversal cases still resolve within the pod fs (no egress); data URLs are the only exfil path and are size-capped |
| rename_workspace can only name own workspace | SA/workspace match (API side, shipped) + agentd never sends an ID from tool args |
| call_with_model images size caps | per-file 5 MiB / total 8 MiB / mime allowlist → pre-call rejection |
