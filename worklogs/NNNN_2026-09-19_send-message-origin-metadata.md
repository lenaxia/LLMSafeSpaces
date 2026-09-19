# Worklog: send_message origin-metadata sentinel (#1465)

**Date:** 2026-09-19
**Session:** Implement issue #1465 — attribute agent-to-agent send_message traffic with a versioned sentinel, rendered distinctly in the frontend; three design pivots mid-flight (LLM param → programmatic injection via harness plugin).
**Status:** Complete (final design: hybrid origin, mode-labeled; vehicle (a) overlay COPY wired)

---

## Objective

`send_message` delivered bare text: the receiving session's transcript and the frontend gave no indication where a message came from. Add a machine-detectable, version-pinned provenance sentinel (agent-message-v1) prepended by agentd, rendered as a provenance badge on the user side of the chat, with the calling session identified programmatically — no LLM-supplied identity.

---

## Work Completed

### FINAL DESIGN ADDENDUM (post-r1, owner rulings 2026-09-19)
- **Hybrid origin, mode-labeled**: PRIMARY = plugin injection via `tool.execute.before` (sentinel/result mode "injected"); FALLBACK = model-supplied optional `from_session_id` (schema-advertised), validated by-ID, mode "self-declared". The ONLY refusal left is when NEITHER source exists (interpreted from "ALWAYS attributed, mode VISIBLE" + "no hard refusal in either mode" — stated in the PR body for reviewer confirmation). A degraded pod self-reports instead of breaking.
- **Plugin namespace key**: the plugin injects `lsp_injected_session` (NOT the schema-advertised `from_session_id`) so agentd labels the mode without trusting model-supplied copies; injection always wins when present.
- **Sentinel schema gains `mode`** (additive; known key, string, optional-on-parse for pre-mode tolerance; strict type on both parsers). Compose requires it.
- **Vehicle (a) WIRED**: Dockerfile COPY `/plugins/llmsafespaces-origin.js` + NOTE amended (one executable + digest-pinned data, #1416 precedent); design 0053 §4.2 amended; ConfigWriter captures/re-emits user `plugin` arrays; `injectPlatformAgentConfig` (renamed from injectAgentdMCPServer) appends `file:///opencode/plugins/llmsafespaces-origin.js` concat+dedup.
- **create_session description** +1 sentence: spawners include the returned session_id in the child prompt (fallback near-reliable for orchestrated sessions).
- **r1 fixes**: composerHistory sentinel leak (strip before manifest); strict TS/Go parser parity (exact-key, type-strict known keys — case-insensitive Go field match and bad-typed workspace divergence closed, new fixtures); freeze-pin job fetches the Dockerfile-pinned version (grep ARG) not the stale 1.18.10 const; real+real e2e leg (`TestOriginE2E_RealHarnessRealAgentd`: real opencode + real agentd dispatch + real seam — passes live, 20s); CI job runs both legs; hand-rolled strings helpers replaced.
- **Harness lessons (for the record)**: pinned binary cannot reach this pod's ephemeral ports (pinned low ports + listener-first construction); swapping httptest .Listener after NewServer leaves the accept loop AND .URL stale; MCP stubs MUST set Content-Type application/json (Go sniffs text/plain and the streamable transport rejects initialize); never require.FailNow inside a stub handler (Goexit writes no response → the harness client retries forever); /global/health answers BEFORE bootstrap completes (~8.5s window — stateful calls need bounded retry); the pkg boot's hardcoded OPENCODE_SERVER_PASSWORD=test-password must match the e2e's client/MCP header.

### Wire-format investigation (the design gate)
Decompiled the pinned harness binary (`/opencode/usr/local/bin/opencode`, 1.18.15, bun-embedded JS via `strings`) and live-probed this pod (8 sessions, 6 busy). Findings in Key Decisions.

### `pkg/session/agentmessage/` — the sentinel contract
- `agentmessage.go`: `Origin{FromSession, Workspace omitempty}`, `Compose(message, origin)` (strip-then-prepend, idempotent; JSON encoding structurally neutralizes hostile values — `-->`, newlines, quotes can never break the line), `Parse(text)` (leading-line only; unknown versions/keys/malformed/missing-fromSession → plain text, payload never breaks).
- 23 golden fixtures (20 parse/compose pairs + null-key/mode additions at r2) in `testdata/` (compose × 5, parse × 12) + `agentmessage_test.go` (golden runners, idempotency, round-trip, hostile-value neutralization, never-mutate table).

### Seam: `SessionExists` (pkg/agent/opencode/loopback.go)
- By-ID existence probe: `GET /session/{id}` — 2xx exists, 404 not (live-proven: unknown ID → `404 {"name":"NotFoundError"}`), else error. Status-only (no body parse → no phantom-exists on corrupted 200). Replaces session-LIST membership for send_message's target check — immune to the #1452 list-visibility gap.

### The origin-injection plugin
- `runtimes/opencode/plugins/llmsafespaces-origin.js`: first-party opencode plugin, `tool.execute.before` hook, matches `llmsafespaces_send_message`, unconditionally sets `output.args.from_session_id = input.sessionID`. Feature-detects everything; no-op on shape mismatch (drift → agentd refuses loudly — detection by construction); never throws.

### agentd (`cmd/workspace-agentd/mcp_tools.go`, `mcp_server.go`)
- `mcpSendMessage(ctx, password, sessionID, message, fromSessionID)`: validates BOTH ends by-ID, composes the sentinel, detached delivery unchanged, result now echoes `"origin"`.
- Missing origin → loud error naming the plugin cause, ZERO wire traffic (delivery ⇔ attribution). Unknown origin → error. Indeterminate probe → error.
- Dispatcher reads the injected `from_session_id` arg; the tools/list schema does NOT advertise it (model-invisible).
- Tool description rewritten: auto-attribution, return-address framing ("reply via send_message to that origin"), metadata-not-authn caveat in the description itself.

### Freeze pin (CI)
- `pkg/agent/opencode/origin_plugin_integration_test.go` (+ `origin-plugin-pin` job in ci.yml): pinned binary + plugin + stub agentd-MCP + tool-calling mock provider. Positive leg: `from_session_id` arrives on the wire carrying the calling session's ID (settles the decompile residual: no layer strips injected args). Negative leg: without the plugin NO identity arrives (the wire-format finding held as a regression pin). **Ran live on this pod's pinned binary: PASS (25s).**

### Frontend
- `frontend/src/lib/agentMessage.ts` — TS parser port, golden-verified against the Go fixtures (runtime-read, attachments precedent); projects to known origin fields (Go struct parity).
- `frontend/src/components/chat/AgentOriginBadge.tsx` — "message from session X" / future "· workspace Y", tool-call-part styling, user-side placement (owner refinement).
- `MessagePart.tsx` user branch: sentinel → badge + stripped text, then attachment manifest parsing (both decorations coexist).
- `ChatPage.tsx` `messageIdentityKey`: sentinel stripped before manifest stripping (server-side decorations never change the key).
- Vitest: agentMessage.test.ts (16), AgentOriginBadge.test.tsx (3), MessagePart sentinel tests (2 new in 83).

---

## Key Decisions

1. **No wire identity exists (validated).** tools/call params = `{name, arguments}` only (decompiled `SessionTools.resolve` → `client.callTool`); `_meta` = `{progressToken, taskId?}` (SDK progress correlation — checked, not identity); transport headers = static config entry (decompiled `MCP.connectRemote`: `requestInit = F.headers`); no session identity in tool env (live `env`). Evidence: decompiled strings at the `callTool`/`SessionTools.resolve`/`Plugin.trigger` regions.
2. **Busy-heuristic rejected (validated).** Live `session_metadata` probe: 6/8 sessions busy simultaneously — `resolveSingleBusySession`'s exact failure mode, in the target environment.
3. **LLM-supplied `from_session_id` rejected by owner** — bad use of LLM resources; the model should never carry its own identity. (Was briefly the approved ruling; superseded.)
4. **Store-pull correlation shelved by owner** — running-tool-part correlation via the V1 message route couples to pre-execution persistence, mid-turn V1 reads, and part shape; all three break under the coming v2 store (#730 drift class). Seam additions reverted before any wiring landed.
5. **Programmatic injection via `tool.execute.before` plugin (final).** Decompiled `Plugin.trigger`: `for (H of hooks) { M = H[W]; await M(K, U) } return U` — output by reference, NO clone, hooks awaited before the tool runs; the MCP call site passes `{args: V}` where `args === V`, then invokes the tool with the same `V`. The bundle's own docs state "mutate output.args before the tool runs". Freeze-pinned empirically (CI job + live run).
6. **Plugin loading vehicle-safe by construction:** `plugin:` config arrays CONCAT+DEDUP across configs (decompiled `Config.loadInstanceState` → `deduplicatePluginOrigins`) — platform injection cannot clobber user plugins; `file://` bare-js specs import in place (no install, no version gate).
7. **Sentinel format v1** — `<!-- lsp:agent-message-v1 {"fromSession":"ses_…"} -->` + `\n` + message; additive-only, golden-locked; `workspace` is a legal v1 key v1 never emits (parser tolerates, #1260 forward-compat); unknown-version-parses-as-plain-text.
8. **Delivery ⇔ attribution** — missing/invalid origin refuses delivery outright. A pod with a broken plugin self-diagnoses from the tool error text.
9. **Metadata-not-authn, in the description** — a session can claim a sibling's ID under the shared pod credential; the plugin's unconditional overwrite means model-supplied values never survive a working plugin; the caveat lives in the tool description (agents read it there).
10. **OPEN — delivery vehicle (owner):** overlay artifact (version-coupled; amends design 0053 §3 "one file, one sha256" NOTE) vs controller-projected volume. Everything landed is vehicle-independent; the plugin source lives at `runtimes/opencode/plugins/llmsafespaces-origin.js` pending the pick.

## Assumptions → validation record (Rule 7)

- "The MCP wire carries no caller identity" → decompiled params/_meta/headers/env + live probes (above).
- "`tool.execute.before` can mutate args" → decompiled trigger loop (by-reference, no clone) + empirical freeze pin on the pinned binary (positive leg passes ⇒ injection survives to the wire; nothing strips unknown keys).
- "Plugin loads from read-only absolute path via file://" → decompiled loader (`Jq`/`pQ`/`Xq`: file specs import in place; no package.json → no compat gate).
- "`plugin:` arrays don't clobber across configs" → decompiled concat+dedup.
- "`GET /session/{unknown}` 404s" → live curl against this pod's opencode.
- "The in-flight turn's message is V1-visible mid-turn" → (shelved mechanism, validated then dropped) `SessionPromptTokens` mid-turn precedent.

---

## Blockers

- Delivery-vehicle decision (owner): overlay vs controller volume. Vehicle wiring (Dockerfile/0053/configmap) intentionally untouched per orchestrator HOLD.

---

## Tests Run

- `go test -timeout 600s -race ./cmd/workspace-agentd/ ./pkg/session/...` — ok (318s).
- `go test -timeout 600s -race ./pkg/agent/...` — ok.
- `OPENCODE_BINARY=/opencode/usr/local/bin/opencode go test -tags=integration -run TestOriginPlugin ./pkg/agent/opencode/ -count=1` — ok (25s, both legs).
- `npx vitest run` (agentMessage, AgentOriginBadge, MessagePart, attachments) — 121+83 pass; `npx tsc --noEmit` clean.
- `go vet`, `gofmt -l` clean; `go build ./...` ok.

---

## Next Steps

1. Owner picks the vehicle → wire delivery (overlay COPY + amend 0053 §3 NOTE, or controller volume + file:// in the ConfigWriter pre-marshal hook — `injectAgentdMCPServer` precedent) in the same PR.
2. Post-merge live validation on a FRESH pod (never restart the running one): send a message between two sessions, confirm the badge renders and `origin` echoes.
3. Reviewers: mutation-test the pins (revert the plugin's injection line → freeze pin positive leg fails; revert agentd's origin check → refusal tests fail).

---

## Files Modified

- `pkg/session/agentmessage/agentmessage.go` (new)
- `pkg/session/agentmessage/agentmessage_test.go` (new)
- `pkg/session/agentmessage/testdata/*.json` (17 new fixtures)
- `pkg/agent/opencode/loopback.go` (SessionExists)
- `pkg/agent/opencode/loopback_test.go` (SessionExists tests)
- `pkg/agent/opencode/origin_plugin_integration_test.go` (new, freeze pin)
- `runtimes/opencode/plugins/llmsafespaces-origin.js` (new, the plugin)
- `cmd/workspace-agentd/mcp_tools.go` (mcpSendMessage origin flow)
- `cmd/workspace-agentd/mcp_tools_test.go` (origin matrix + updates)
- `cmd/workspace-agentd/mcp_server.go` (dispatcher arg, schema/description)
- `.github/workflows/ci.yml` (origin-plugin-pin job)
- `frontend/src/lib/agentMessage.ts` + `.test.ts` (new)
- `frontend/src/components/chat/AgentOriginBadge.tsx` + `.test.tsx` (new)
- `frontend/src/components/chat/MessagePart.tsx` (+ sentinel tests in `MessagePart.test.tsx`)
- `frontend/src/pages/ChatPage.tsx` (identity key)
- `worklogs/0995_2026-09-19_send-message-origin-metadata.md` (this file)
