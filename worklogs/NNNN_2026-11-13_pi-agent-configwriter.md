# NNNN — pi AgentConfigWriter: render pi's config surface from AgentConfigInput (tsk-torrid-igloo-tramps)

Board story: tsk-torrid-igloo-tramps (pi second-harness integration, story S3 of the scoping-report epic)

## The gap

The `AgentConfigWriter` seam (US-65.1, design 0049 §5) had exactly one
implementation — opencode. The owner selected pi (earendil-works/pi) as
the second harness; scoping report §3.3 (validated against pi's docs at
`packages/coding-agent/docs/`) scoped this story: render pi's whole
config surface from the same partial-update `AgentConfigInput` the
platform already produces, with a per-source restartRequired contract
that exploits pi's request-time credential resolution.

## Changes

New package `pkg/agent/pi` (configwriter.go), implementing
`agent.AgentConfigWriter` over a pi agent directory
(`PI_CODING_AGENT_DIR` relocates every write target —
readOnlyRootFilesystem-compatible):

1. **models.json** — custom-endpoint providers as
   `{baseUrl, api, apiKey, models[]}` with `contextWindow`/`maxTokens`
   (models-configuration.md); the relay renders under the same
   `opencode-relay` id the opencode writer uses, its credential
   deliverable as a models.json `!command`/`${VAR}` key ("Commands in
   `models.json` run at request time and are not cached by Pi",
   models.md).
2. **auth.json** — first-party provider keys as `{type: "api_key"}`
   entries; pi-written OAuth/foreign entries preserved verbatim.
3. **mcp.json** — stdio `{command, args, env}` and http `{url, headers}`
   entries (mcp.md's own opencode-migration table); `timeoutMs`
   converted to pi's integer-seconds timeout (ceil).
4. **APPEND_SYSTEM.md** carries AdminPrompt. APPEND, never SYSTEM:
   SYSTEM.md *replaces* pi's own operational system prompt; the
   platform prompt is additive control-surface content. SYSTEM.md is
   never written (pinned by test).
5. **settings.json** — `defaultProvider`/`defaultModel` from the
   `providerID/modelID` selection, with ModelResolutionWarning parity
   (pkg/agentd/types.go:75-86): unresolvable ids are omitted and
   recorded in `model-resolution-warning.json` beside the agent dir
   instead of poisoning pi's startup model (2026-08-16 incident class);
   the marker self-clears on a resolving render.
6. **restartRequired matrix** — relay-with-dynamic-key returns FALSE
   (the hot-reload seam `agentconfig.go:60-63` anticipated); providers
   (auth.json-keyed — command keys cached for the process lifetime,
   providers.md), model, MCP and prompt sources return TRUE.
   AllowedDirs accepted-and-dropped (§1.5 ruling: permission-less
   harness, the sandbox is the boundary).

Design notes worth remembering:

- The Providers source arrives as `opencode.FormatOpenCodeConfig` bytes
  (the shape every existing caller produces); the writer parses that
  shape (and a pi-native `providers` shape for the future pi
  formatter), so the secrets pipeline and relay injector paths work
  unmodified.
- The writer is the state holder across partial Applies, mirroring the
  opencode writer: sources merge under one lock, every render rewrites
  each file atomically (temp+rename), in-memory state rolls back on
  render error.
- Corrupt auth.json/settings.json/mcp.json are left alone (never
  clobbered, never deleted); an explicitly staged MCPServers source
  still takes ownership of a corrupt mcp.json.

## Review round 2 (AI reviewer CHANGES_REQUESTED → fixed)

Five confirmed defects, one root cause: the construction-time load path
was not the inverse of the render path. Two reproduced by the reviewer
(C1 first-party-with-models credential lost across restart; C2 pi-native
`api` field silently re-labeled openai-completions), three code-traced
(C3 settings-persisted first-party selection dropped + spurious marker
on restart; C4 first-party no-models credential undeletable after
restart; C5 corrupt mcp.json deleted instead of left alone). Fixes:

- auth.json ownership rule: `{"type":"api_key"}` entries are
  writer-owned and RE-HYDRATE the provider set (empty-key seeded
  providers get their key back; unknown ids become seeded first-party
  entries) — closing C1, C3 and C4 at the root.
- `providerEntry.API` now flows through the render (declared api wins;
  openai-completions is only the default for undeclared custom
  endpoints) and is parsed back by loadExistingModels — C2.
- mcp.json gains the same mcpOK leave-alone contract as auth/settings,
  with staged-source ownership override — C5.
- Five regression tests, one per defect, all failing on the pre-fix
  code paths.

## Tests

`configwriter_test.go` / `configwriter_golden_test.go`: hot-vs-restart
matrix (incl. `!command`, `${VAR}`, `$VAR`, literal, clear, combined
sources, AllowedDirs drop); opencode-shape and pi-native provider
parsing; embedded-model-key exclusion; MCP shape + timeout conversion
(2500ms→3s, 1ms→1s floor, sse→URL entry); APPEND-only prompt; marker
parity matrix (unresolvable qualified, flat relay qualification,
allowlist-claim requirement, first-party existence-stands, clear);
partial-update preservation; HasRelay pre-boot short-circuit from disk;
foreign-state preservation; corrupt-file leave-alone; the five restart
round-trip regressions; 16-way concurrent Apply. Golden files pin all
five rendered shapes plus byte-identical round-trip through a writer
restart (`REGEN_GOLDEN=1` refresh procedure documented in the test).

## Validation

- Live pi v1.5.2 binary over a writer-rendered agent dir (workbench,
  not CI): `pi mcp list` parsed + attempted the rendered entry;
  `pi -p` booted on settings.json's `acme/acme-large` startup model;
  `pi -p --model opencode-relay/glm-5-free` resolved the
  `!cat /sandbox-runtime/rt/relay-token` apiKey AT REQUEST TIME
  (live proof of the models.md quote the restart matrix relies on —
  pi failed with "Failed to resolve API key ... from shell command",
  exactly the request-time resolution path).
- Assumption validation (README-LLM Rule 1): pi file shapes assumed
  from docs were all confirmed by the live binary; the one assumption
  the reviewer broke (load/render inverse-ness) is now pinned by the
  round-trip regression tests.
- `make fmt-check`, `golangci-lint`, `go vet`, full pkg/agent test tree
  green; CI green on the head except govulncheck, which fails
  byte-identically on origin/main HEAD (stdlib GO-2026-6859,
  go1.26.10 vs 1.26.11 — no findings in pkg/agent/pi; needs the
  repo-wide toolchain bump, deliberately out of scope here).

## Follow-ups (out of scope, tracked by the story split)

- Harness-selection plumbing (S5) wires this writer into
  agentd/pod_builder — nothing constructs `pi.ConfigWriter` yet (Rule
  12 second-consumer sanction: funded series, seam pre-authorized by
  design 0049 §5 / 0053 §5.4).
- The wired e2e coverage obligation transfers to that story.
- models.json file-reload cadence in RPC mode (report U-4) rides the
  S6 drift-gate fixtures.
