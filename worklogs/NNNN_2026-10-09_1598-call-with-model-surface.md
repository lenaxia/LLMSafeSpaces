# Worklog: #1598 — call_with_model surface (unknown-provider 500s, discovery, vision gate, no-text-parts)

**Date:** 2026-10-09
**Worker:** w15, branch `fix/1598-callwithmodel` (from main @ 4615c2b3, v0.34.20)
**Status:** In progress — audit complete, implementation starting.

---

## Entry 1 — Audit: what main already carries vs. the issue's four asks

The issue has a github-actions comment (2026-09-29) describing a full fix on branch
`opencode/issue1598-20260929032940` — **that branch never merged** (the bot's own follow-up
comment died with `fatal: could not read Username`; no PR references it; none of its claimed
error strings exist in main). Its rulings are useful prior art but must be re-derived, not
trusted. Separately, #1624 (worklog 1089, in main since ~v0.34.17) shipped an overlapping
`list_models` + catalog pre-flight + credential-declared `attachment` vision metadata. My
charter: extend/reconcile, don't duplicate.

Per-ask state on main @ 4615c2b3:

### Ask 1 — unknown-provider validation: MOSTLY DONE by #1624; hardcoding NOT done

- `verifyModelInCatalog` (cmd/workspace-agentd/mcp_tools.go:363) runs BEFORE the carrier
  session exists: unknown provider → error naming the connected providers; known provider +
  unlisted model → error naming the provider's model IDs; catalog unreachable/empty →
  fail-open. This kills the raw-500 class for text-only calls (the 500 was the opencode send
  failing after dispatch).
- The hardcoded example survives in **three agentd/seam sites**:
  - mcp_server.go:197 — call_with_model `model` schema description: `(e.g. "anthropic/claude-sonnet-4-5")`
  - mcp_tools.go:344 — `bareModelRefError`: same example
  - pkg/agent/opencode/loopback.go:268 — `SplitModelRef`'s own error (surfaces verbatim via
    `compact`'s explicit-model path, mcp_tools.go:852)
  Two more same-class sites live in pkg/mcp/server.go:411,432 (platform-owner surface:
  credential_create/model_set) — different surface than this issue's repro; noting for the
  orchestrator rather than scope-creeping this lane.
- **Open correctness question (the drift)**: `verifyModelInCatalog` fails CLOSED at model
  level. But the opencode-relay provider block's models come from the controller's
  free-models list (relay_injector.go fetchFreeModels) and custom-endpoint providers' lists
  are boot-fetched with a 24h cache (model_enricher.go) — the issue observed the relay
  serving models absent from the config block. If the pinned opencode ACCEPTS an unlisted
  modelID on a custom baseURL provider at send time, the model-level gate manufactures false
  refusals exactly in the issue's drift scenario. Probe required (below).

### Ask 2 — discovery surface: EXISTS (list_models, #1624); two gaps

- `mcpListModels` serves `{model: "provider/id", name, contextWindow, maxOutput}` from
  GET /provider (seam `AvailableModels`). Deterministic, read per call.
- Gap A — **no vision field**: the vision-gate refusal tells the agent to "pick a
  vision-capable model", but list_models' own description disclaims capability questions
  ("Not for: ... capability questions beyond context/output limits") — an agent cannot act
  on the remedy. `ModelInfo` (loopback.go:582, GET /config/providers) already computes the
  #1307 tri-state OR-merge of `capabilities.input.image` + `capabilities.attachment`; the
  join just isn't surfaced.
- Gap B — **the drift** (above): neither the relay catalog nor the config block alone
  answers "what can I pass to call_with_model" until the probe decides what the wire
  accepts.

### Ask 3 — vision gate "startup snapshot": agentd side is ALREADY call-time; server side is frozen; error not honest; PATCH /config uninvestigated

- mcpCallWithModel resolves `client.ModelInfo(...)` live per call (mcp_tools.go:283) —
  there is NO agentd-side snapshot to fix. What's frozen is the opencode server itself
  (reads config at process start; worklog 1089 + README-LLM relay-config section). The
  refusal error (mcp_tools.go:285) says "declare attachment:true" but never says the
  declaration takes effect only after restart — the user who follows the remedy sees no
  change (the issue's exact trap).
- `PATCH /config` echo-but-not-stick: our seam uses PATCH **/global/config** (SetModel);
  PATCH /config is upstream's. No agentd code path calls it. Whether agentd re-projects the
  config file over disk edits needs a check of when FlushProviders/configwriter re-run
  (suspect: boot + secrets resync only) + a pinned-binary probe.

### Ask 4 — "no text parts" detail: NOT done

- `SendResult` (loopback.go:99) carries only MessageID/ModelID/Text; SessionSend's parse
  loop (loopback.go:321-339) discards everything non-text. mcp_tools.go:306 renders the
  bare error. Need part types in wire order + bounded excerpt of the first non-text part
  that carried text (reasoning blocks DO carry text); zero-part responses get their own
  message.

## Probes planned (pinned binary /opencode/usr/local/bin/opencode, 1.18.15)

1. **Unlisted-model send on a custom baseURL provider**: throwaway serve + fake
   openai-compatible backend; config block lists model A only; POST /session/{id}/message
   with override model B (unlisted). 200 → model-level gate must fail-OPEN for custom
   (baseURL) providers; 500 → current fail-closed matches the wire, drift is a controller
   catalog-freshness matter (document + report, no gate change).
2. **PATCH /config on bare 1.18.15** (no agentd): PATCH then GET — separates upstream
   behavior from any agentd projection. Plus code check of FlushProviders re-run triggers.

## Implementation plan (one PR — the asks share the call_with_model surface)

1. De-hardcode the three agentd/seam example sites; guidance test forbids concrete
   provider/model examples in the tool schema and refusal errors.
2. Probe 1 → decide drift posture; if fail-open warranted: `verifyModelInCatalog` consults
   the provider entry's `options.baseURL` (GET /provider carries it — recorded fixture
   confirms) and proceeds for custom-endpoint providers' unlisted models (curated providers
   keep the strict named-alternatives refusal). Either way: worklog records the evidence.
3. list_models: one GET /config/providers per call (new seam method, capabilities parsed
   seam-side per Rule 12), join → tri-state `vision` field; description updated; fake-seam
   + recorded-fixture contract tests.
4. Vision-gate refusal: add the restart-required truth; pin call-time resolution (catalog
   flip between two calls flips the gate decision — kills any future snapshot regression).
5. SessionSend: record part types + bounded (≤256-rune) first-non-text excerpt in
   SendResult; mcpCallWithModel renders them; zero-parts distinct message; seam tests on
   synthetic + recorded shapes.
6. Red-first pins for every behavior change; `go test -run` counts verified; suites:
   cmd/workspace-agentd TestMCP*, pkg/agent/opencode full; integration legs with
   OPENCODE_BINARY + -tags=integration where they exist; goimports + golangci-lint clean.
7. Push → AI review rounds; poll on cadence; fix pushes reported per round.

## Measures

- Audit reads: mcp_tools.go (full), loopback.go (seam surface), client.go (AvailableModels/
  PatchConfig), model_enricher.go (full), relay_injector.go (model-list path), worklog 1089,
  recorded fixtures opencode-{provider,config-providers}-recorded.json, issue #1598 + both
  comments, PR list (no open PRs; the 2026-09-29 branch unmerged).
- No tests written yet — entry 2 starts red-first.

---

## Entry 2 — Probes (pinned 1.18.15), implementation, mutation evidence

### Probe results (throwaway serve, fake openai-compatible backend, /tmp/opencode/probe1598)

Setup: `opencode serve --port 18491` with `OPENCODE_CONFIG` carrying provider `fakeprov`
(baseURL → fake backend) whose config block lists ONLY `listed-model`; the fake backend's
/v1/models also offers `unlisted-on-config-model` (the drift shape). The pod's real
providers (opencode-relay: 33 controller-listed models; thekaocloud) layer in alongside —
the drift is visible live here too (issue reported 78 relay-served).

- **P1 (decisive, drift):** `POST /session/{id}/message` with override
  `{providerID: fakeprov, modelID: unlisted-on-config-model}` → **500 UnknownError**
  (`ref=err_44ef0137` — the issue's exact error). Control: `fakeprov/listed-model` →
  **200** with text "FAKE-REPLY…". Conclusion: pinned opencode refuses ANY modelID absent
  from the provider's config block, custom baseURL provider included. The model-level
  fail-closed `verifyModelInCatalog` (#1624) MATCHES wire truth — kept unchanged. The
  relay-vs-config drift means relay-only models are genuinely not dispatchable:
  **list_models (GET /provider) IS the complete authoritative answer to "what can I pass
  to call_with_model"**; making the controller's free-models list track the relay's live
  catalog is the (out-of-lane) fix for the drift itself.
- **P2:** `GET /provider` lists exactly the config block's models for fakeprov — no
  internal-list fallback models for custom providers on this shape.
- **P3 (ask 3):** `PATCH /config {"theme":"dark"}` on bare 1.18.15 (zero agentd in the
  loop) → 200 with `{}`; follow-up `GET /config` → old doc (no theme). Non-stick is
  **upstream opencode behavior**, not an agentd projection overwrite; our seam never
  calls PATCH /config (only PATCH /global/config for SetModel). Restart is genuinely the
  only path → the refusal error now says so.

### Implementation (one PR, all four asks)

1. **Ask 1b (de-hardcode):** `SplitModelRef` error (loopback.go), `bareModelRefError`
   (mcp_tools.go), and the call_with_model `model` schema description (mcp_server.go) now
   illustrate the FORM ("provider-id/model-id") with zero concrete providers; the schema
   points at list_models for real names. list_models' description also drops its
   "thekaocloud/classifier" example ("the entries themselves are the examples").
   Provider-level validation itself (ask 1a) was already #1624's and is probe-confirmed
   correct above.
2. **Ask 2 (vision discovery):** seam `Client.ModelCapabilities` — ONE GET
   /config/providers → provider→model→ModelInfo index (shared parse with ModelInfo via
   `modelInfoFromCapabilities`, Rule 12: raw bytes stay in the seam);
   `mcpListModels` joins it into a tri-state `vision` field (true/false known, ABSENT
   unknown; fail-open: capability-catalog failure degrades to name-only output, logged).
   The refusal errors now say "find one with the list_models tool".
3. **Ask 3 (honest restart):** vision-gate refusal adds the frozen-catalog truth: the
   catalog is read at workspace agent startup, does not hot-reload, and a newly declared
   capability takes effect only after the workspace agent restarts. Call-time
   re-resolution (already the behavior — no agentd snapshot) is now PINNED by
   `TestMCPCallWithModel_VisionGateResolvesPerCall` (catalog flipped between two calls
   flips the decision).
4. **Ask 4 (no-text detail):** `SendResult.PartTypes` (wire order) +
   `SendResult.NonTextExcerpt` (≤256-rune excerpt of first non-text part carrying text);
   `noTextPartsDetail` renders "response parts: step-start×1, reasoning×1, tool×1; first
   non-text excerpt: …"; zero-part responses get their own "no parts at all" message.

### Mutation evidence (sabotage → red, right reason)

- M1 ask 4: `noTextPartsDetail` forced to "" → 3 tests red (NoTextParts,
  ReasoningExcerpt, EmptyResponse). ✓
- M2 ask 3: restart sentence stripped from refusal → VisionRefusalNamesRestartPath red. ✓
- M3 ask 1b: `anthropic/claude-sonnet-4-5` reintroduced in schema description →
  ToolDescriptionGuidance/no-hardcoded- examples red. (Two botched M3 attempts inserted
  invalid Go escapes/build breaks — a broken build is NOT a red pin; redone with a
  syntactically valid mutation.) ✓
- M4 ask 2: vision join disabled (`if false`) → TestMCPListModels red (tri-state asserts). ✓

### Incidents (in-flight, honest)

- **Careless `git checkout -- cmd/workspace-agentd/mcp_tools.go` after M1** wiped all my
  (then-uncommitted) tool-layer edits. Restored by redoing the four edits verbatim;
  re-verified the full targeted sweep green and re-ran mutations M2-M4 afterward, so all
  mutation evidence postdates the restoration.
- Two mutation attempts (M3) produced build failures (escaped quotes) before the valid
  one — recorded above, no false red claimed.
- Probe serve initially launched without `OPENCODE_SERVER_PASSWORD` (401) and a
  `pkill -f` pattern matched my own wrapper shell (self-kill); relaunched with the env +
  PID-scoped kills. The pod's own opencode (:4096, PID-managed) was never touched; probe
  server + fake backend killed after use.

### Test measures (run counts verified via -v where ambiguous)

- pkg/agent/opencode full: **ok** (12.7s); -race targeted seam/model sweeps: **ok**.
- New seam tests: 10 (part detail ×4, SplitModelRef guidance, capabilities ×4, clip ×1)
  + existing TestModelInfo* rows all green (recorded-fixture legs included).
- cmd/workspace-agentd targeted (TestMCP*/guidance): 27 PASS incl. 8 new/extended
  (NoTextParts detail, ReasoningExcerpt, EmptyResponse, RestartPath, ResolvesPerCall,
  BareRefusal guidance, ListModels tri-state, CapabilityUnreachable fail-open).
- golangci-lint (cmd/workspace-agentd + pkg/agent/opencode): **0 issues**.
- Full cmd/workspace-agentd suite: running (worklog 1089 documents pre-existing
  watchdog/managed-process timing flakes under full-suite load — result appended in
  entry 3; targeted lanes all green).
