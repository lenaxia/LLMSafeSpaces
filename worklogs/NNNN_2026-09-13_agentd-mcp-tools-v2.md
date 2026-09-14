# Worklog: agentd MCP tools v2 — seam containment, in-line call_with_model, session_metadata, compact, and the test-plan legs

**Date:** 2026-09-13 (second session, continues `0923_2026-09-13_agentd-mcp-five-tools.md`)
**Session:** opencode (main dev box). User direction: call_with_model must be in-line (no separate persistent session), all opencode interfacing must go through the contained seam (not raw coupling), new tools session_metadata + compact, create_session is a top-level session with no artificial delivery cap, and a comprehensive unit/integration/e2e test plan.
**Status:** Complete. All levels green: L0 (seam + tools + API handler), L1 (JSON-RPC full-stack), L2 (real binary, 7/7), L3 (live pod, 7/7).

---

## Objective

1. **Containment (Rule 12):** move ALL opencode wire knowledge out of `cmd/workspace-agentd` into `pkg/agent/opencode` (one seam), including the pre-existing `session_list`/`session_read`.
2. **call_with_model v2:** in-line in the current conversation; image support.
3. **create_session v2:** top-level peer session; drop the 30min detached cap.
4. **New: session_metadata** (messages, context fill, tokens, age, workspace id — no new security surface).
5. **New: compact** (summarize-based; run-at-boundary for busy targets).
6. **Test plan:** `docs/testing/agentd-mcp-tools-test-plan.md` with L0/L1/L2/L3 legs, all implemented and green.

## Empirical validation (Rule 7 — live pod, opencode 1.18.15)

Everything below was exercised against the real binary in this pod (scratch sessions, cleaned up):

1. **Busy sessions BLOCK incoming V1 messages** — `GET /session/status` = `{type:"busy"}`; a second POST hung (HTTP 000 at 20s, no 409/queue). Consequence: a synchronous message to the CURRENTLY RUNNING session deadlocks by construction (its turn waits on the tool). call_with_model therefore runs its single call on a transient idle carrier and returns the text as the tool result — the exchange is in-line via the tool call + result channel, like every tool. Documented in the tool description with the reason.
2. **Image file parts ARE accepted on the wire**: `{type:"file", mime, filename, url:"data:<mime>;base64,..."}` — with a VALID PNG the send completes 200; with garbage bytes the attachment processor 400s (decode failure, not schema). Whether bytes reach the MODEL depends on the model's catalog capability — for a non-vision model opencode injects an in-band "this model does not support image input" note (the model's own reply confirms). The tool pre-checks the catalog (`ModelInfo.ImageInput`) and refuses vision-incapable targets up front.
3. **RENAME IS PATCH, NOT POST** — `POST /session/{id} {"title"}` returns **200 and silently does nothing** on 1.18.15; `PATCH` renames immediately (SDK `sessionUpdate`). **Pre-existing product bug found and fixed**: the adapter's `RenameSession` (the user-initiated rename flow) used POST — agent-side renames never landed. Fixed in adapter + seam; pinned by `TestAdapter_RenameSession`, `TestSeam_SessionRename_UsesPatchNotPost`, `TestLoopbackL2_SessionLifecycle`, and the liveprobe.
4. **Compact (working route): `POST /session/{id}/summarize {providerID, modelID}` → 200 `true`**; the V2 `/api/session/{id}/compact` is 503 "not available yet" on 1.18.15. Run-at-boundary proven: summarize fired while busy queued server-side and completed (200, 21s) exactly when the generation finished → compact-on-own-session = detached POST, tool returns "scheduled".
5. **V2 context endpoint tracks the agent-runner's sessions** — native sessions populate it; raw-V1-driven ones report 0. L2 pins the no-error contract; the collapse effect is live-proven (3 exchanges → 1 in-context after summarize).
6. Session list shape (typed `SessionSummary`), `/config/providers` per-model limits + capabilities, `X-Next-Cursor` pagination — all pinned in tests.

## Test-infra findings (all fixed in-harness, each live-debugged)

- **`waitForHealthy` had no probe timeout** — a stalled child boot (catalog fetch blackholed on egress-filtered runners) hung the probe forever. Now 2s-bounded → retries → loud failure. This was the systemic L2 instability.
- **The pod's ephemeral port range is hostile to the child's outbound fetch** — identical mocks on pinned low ports answer; `httptest`'s `:0` ports yield "Cannot connect to API" retries. L2 mocks now bind pinned ports (probed/claimed).
- **Go test temp dirs filled the shared PVC** (`/tmp` hit 100%) — periodic cleanup added to the workflow; failures from that state were environmental.
- **Harness child env is explicit** (PATH/TMPDIR + the opencode vars), and stderr goes to a file (dumped at cleanup) — defensive hardening from the debugging trail; see the harness comments for the full stories.
- Orphaned children from timeout-killed tests hold ports and poison subsequent runs (ServeError + talking to a stale listener). The bounded-probe fix removes the hangs that caused them.

## Files changed

- `pkg/agent/opencode/loopback.go` (new) — the contained seam: `SessionCreate/Rename(PATCH)/Delete/Send(model+images)/Summarize/List(ListRaw)/MessagesRaw/MessageCount/ContextCount/PromptTokens/ModelInfo`, `SplitModelRef`, `NewLoopbackClient` (keep-alive disabled — ghost-connection hardening, comment documents the live evidence).
- `pkg/agent/opencode/loopback_test.go` (new) — L0 seam matrix (21 tests).
- `pkg/agent/opencode/loopback_integration_test.go` (new) — L2: 7 tests, offline mock provider.
- `pkg/agent/opencode/opencode_integration_test.go` — harness hardened (bounded health probe, explicit env, file stderr, config-injectable boot); legacy tests unchanged in behavior.
- `pkg/agent/opencode/adapter.go` — **`RenameSession` POST→PATCH** (the product bug).
- `cmd/workspace-agentd/mcp_tools.go` — rewritten onto the seam (zero wire knowledge left in agentd); call_with_model v2 (carrier + images + catalog pre-check); create_session v2 (top-level, no cap); session_metadata (new); compact (new, run-at-boundary).
- `cmd/workspace-agentd/mcp_server.go` — session_list/session_read route through the seam; tools/list + dispatcher updated (11 tools).
- `cmd/workspace-agentd/mcp_tools_test.go` — rewritten L0+L1 matrix incl. full-stack JSON-RPC tests and the secrecy contract for session_metadata.
- `cmd/workspace-agentd/mcp_server_test.go` — description-guidance pins updated; seam-routed error-message pins.
- `scripts/mcp-tools-liveprobe.sh` (new) — L3 probe (7 checks; busy-block behind `LIVEPROBE_BUSY=1`).
- `docs/testing/agentd-mcp-tools-test-plan.md` (new) — the comprehensive plan: validated wire-contract table with evidence, design consequences, L0–L3 matrices, security matrix.

## Adversarial self-review (Rule 11)

- **"In-line" delivered via the tool-result channel, not by writing into the running session** — validated as the only non-deadlocking shape (busy-block evidence). The tool description explains this to the agent in one sentence.
- **session_metadata security** — outputs ⊆ already-exposed surfaces (session list fields, busy flags, catalog limits) + `WORKSPACE_ID`; secrecy test asserts no password/token/env/internal-URL leakage; read-only by construction.
- **compact ambiguity** — omitted session_id resolves only when exactly one session is busy; otherwise it errors teaching session_metadata. Busy targets never block the tool (detached, run-at-boundary).
- **L2 environment sensitivities documented, not hidden** — pinned ports + bounded probes + cleanup discipline; CI runners (fresh container, downloaded binary) don't share this pod's quirks, and the harness now fails loudly rather than hanging if any recur.
- False alarm retired: "Go httptest mock incompatible with Bun" — the real culprits were ephemeral-port hostility + unbounded probes + dead mocks in corrupted experiments; the final suite runs the Go mock against the real binary cleanly.

## Validation

- `go build ./...`, `go vet` (+`-tags integration`), `gofmt` — clean.
- L0/L1: `go test ./pkg/agent/opencode/ ./cmd/workspace-agentd/ ./api/internal/handlers/ ./api/internal/server/` — **all ok** (12.4s / 224.6s / 95.5s / 0.3s).
- L2: `OPENCODE_BINARY=<pod binary> go test -tags integration ./pkg/agent/opencode/ -run TestLoopbackL2_` — **7/7, 33.5s** (lifecycle, send+model+image, bare-model rejection, busy flip, messages/tokens, summarize, catalog).
- L3: `scripts/mcp-tools-liveprobe.sh` — **7/7 PASS** on the live pod.

## Decision addendum (2026-09-13, user call): direct provider-call seam — DECLINED

The option to make `call_with_model` a direct OpenAI-compatible call from
agentd (sessionless, `Kind`-keyed seam, ~150 lines; relay + every
`openai_compatible` credential covered, native/OAuth kinds excluded with a
clear error + `task` fallback) was evaluated and **rejected as
architecturally off-limits** — direct provider invocation from
platform/pod code is contrary to this platform's design regardless of
scope. "Only OpenAI-compatible" is still a provider wire format; one
protocol is still provider integration; and it forks credential access,
egress, and audit out of the single path that owns them. The harness
(opencode + ai-sdk) is the sole provider caller, full stop. Revisit only
if the product fundamentally changes (e.g. a funded platform service
that is itself an agent consumer), never as a tool-call shortcut.

Honest-correction shipped with the decision: the tool description claimed
the target model "has NO tools and sees only your prompt"; the L2 evidence
(loopback mock request body) shows the carrier send DOES include the
workspace agent's system prompt and tool schemas. Description and its
guidance pin now state the real contract: no conversation context crosses
over, but the agent's standard setup does.

Left on the table (separate, unfunded): `Attachment *bool` on
`LLMModelConfig` — the operator-declared vision-capability override for
gateways whose models.dev catalog entry is wrong (the thekaocloud default
model question). Not built.
