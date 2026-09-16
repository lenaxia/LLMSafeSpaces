# Worklog: leg-10 wire-drift pins at the opencode seam parse sites

**Date:** 2026-09-15
**Session:** epic-71 / leg10-pins — close the #1312 assessment gap: the #1379-era parse sites (`pkg/agent/opencode/loopback.go`, `translate.go`) lacked verified leg-10 (wire drift) pins. Audit, pin red-first, mutate-to-red evidence.
**Status:** Complete — APPROVED (5th review round, commit 3adcb1c1); awaiting owner merge (PR #1384)

---

## Objective

Fault-matrix leg 10 (wire drift) was green at 3 pinned sites (`outbox_terminus_test.go:506/550`, `proxy_actions_test.go:226` — commit 82aee7f6, PR #1352) but the parse sites touched by PR #1379's era — `pkg/agent/opencode/loopback.go` and `pkg/agent/opencode/translate.go` — were UNVERIFIED against drift (#1312 assessment §1 row 10, "What remains" item 5). Audit every parse site in those files for a drift guard; add leg-10 pins where missing, red-first, with mutation evidence that each pin is load-bearing.

---

## Work Completed

### 1. Audit (site → pin status BEFORE)

**loopback.go — the in-pod tool seam (7 body-parsing sites):**

| Site | Wire shape parsed | Pin before | Leg-10 corruption pin before |
|---|---|---|---|
| `SessionCreate` (POST /session → `{id}`) | decode `{id}` | happy (`ses_new`) + 500 | **NONE** — and the empty-ID guard (`out.ID==""`) was unpinned |
| `SessionSend` (POST …/message → `{info,parts}`) | decode info+parts | happy (Text/ModelID/MessageID), busy-blocks, image parts | **NONE** |
| `SessionList` (GET /session → `[]SessionSummary`) | Unmarshal array | happy (`ses_1`) | **NONE** |
| `SessionMessageCount` (page walk) | Unmarshal `[]RawMessage` per page | bounded walk (1001) | **NONE** |
| `SessionContextCount` (GET /api/…/context → `{data}`) | decode `{data:[]}` | happy (count 2) | **NONE** |
| `SessionModelRef` (GET /session/:id → `{model}`) | decode `{model:{id,providerID}}` | fixture-cited shape (`session_get_1_18_10.json` via `model_agent_status_test.go:18`) | **NONE** |
| `ModelInfo` (GET /config/providers) | decode catalog | happy + not-found | **NONE** |

**Excluded by design (documented, not pinned):** `SessionRename`/`SessionDelete`/`SessionSummarize` (2xx-only, no body parse); `SessionListRaw`/`SessionMessagesRaw` (byte passthrough — no parse site); `SessionPromptTokens` (best-effort display value, fail-open 0 by contract — its shape is pinned by the Epic-36 formula tests `loopback_test.go:352-439`).

**translate.go — pure wire→contract translators:**

| Site | Pin before | Leg-10 corruption pin before |
|---|---|---|
| `ParseHistoryWire`/`ParseHistoryStream` | #730 golden fixtures (`history_1_18_10_flat_tool.json`, `history_1_15_12_nested_tool.json` via `session_shape_test.go:76/101`), `TotallyGarbage_StillErrors` (not-json/object/html/empty), `OneMalformedPart_DoesNot502`, `TruncatedBody_ReturnsPartialResults` | **COVERED** (cite — no duplicate added) |
| `ParseSessionListWire` | wrapped/bare/RealShape fixture + `MalformedJSON` (not-json only) | **PARTIAL** — trailing-garbage/html/empty unpinned |
| `ParseSessionWire` | wrapped/bare + `session_get_1_18_10.json` fixture | **NONE** — no corruption pin at all |
| `ocPart`/`ocModelRef`/`ocTime` UnmarshalJSON dual-shape normalizers | #730/#743 fixtures + `AllObservedToolNames` | COVERED (cite) |
| SSE event translation (`translate_sse_test.go`) | `MalformedJSON_Dropped`, golden ABI fixtures | COVERED (cite) |

**Fixture discipline (issue #730):** testdata golden fixtures cover the adapter paths — `session_get_1_18_10.json` (ParseSessionWire + SessionModelRef), `session_list_1_18_10.json` (ParseSessionListWire), `history_*.json` (ParseHistoryWire), `event_store`/`sse_events`/`v2_messages` (client events/SSE/V2). The seam's decode sites have no captured fixtures in testdata/; their shape pins are the hand-written fake happy tests + the build-tagged real-binary L2 leg (`loopback_integration_test.go`). Deferred: capturing live seam fixtures is a live-pod task (see Next Steps).

### 2. The real gap the audit found (Rule-7 assumption, validated)

Assumption: "the seam's decode sites fail loudly on corrupted 200s." Validated FALSE for the trailing-garbage mode: `json.Decoder.Decode` reads the FIRST value and silently accepts trailing bytes — proven with a scratch program (decoder=`<nil>` on `{"id":"x"}garbage`) and then red-first by the new pin: 5 of 7 sites silently misparsed (`SessionCreate`, `SessionSend`, `SessionContextCount`, `SessionModelRef`, `ModelInfo` — all Decode-based; the Unmarshal-based `SessionList`/`SessionMessageCount` were already loud). This is exactly the #1308/#1352 `CorruptTrailingGarbage` class pinned at the 3 green sites — the seam was the gap.

### 3. Pins added (red-first)

- `TestSeam_WireDriftCorruption` (`loopback_test.go`) — 7 sites × 4 canonical modes (`invalid_json`, `trailing_garbage`, `empty_body`, `html_error_page`, mirroring `abitest.CorruptMode`): every corrupted 200 fails LOUDLY, with discrimination (the terminus r1 pattern): the error must carry `decode` — the failure is AT THE PARSE, never a downstream phantom (ModelInfo's not-found guard would otherwise mask a swallowed parse; found during M1 red-check).
- `TestSeam_SessionCreate_IDDriftLoud` — valid-JSON-but-renamed-`id` (the shape-rename drift class) trips the empty-ID guard loudly.
- `TestParseSessionListWire_CorruptBody`, `TestParseSessionWire_CorruptBody` (`translate_test.go`) — 4 modes each → loud error, no phantom session/list.

### 4. Production fix (minimal, seam-local)

`decodeStrict(r, v)` in `loopback.go`: decode one value, then require `io.EOF` at the next token (trailing whitespace stays acceptable). Wired into the 5 Decode sites. RED before (5 trailing subtests), GREEN after. No behavior change for clean bodies.

### 5. Mutation red-checks (reviewer falsifies-by-execution evidence)

| Mutation | Result |
|---|---|
| M1: `decodeStrict` trailing check disabled | 5 red — trailing_garbage × 5 strict sites (ModelInfo red ONLY after the discrimination assert was added; before it, not-found masked the swallow — the discrimination is load-bearing) |
| M2b: `decodeStrict` full swallow | 20 red — 4 modes × 5 strict sites |
| M3: `ParseSessionWire` swallow → phantom session | 4 red |
| M4: `SessionCreate` empty-ID guard removed | `IDDriftLoud` red (corruption subtests stay green — the guard is uniquely load-bearing for the renamed-field class) |
| M5a: `SessionList` Unmarshal swallow | 4 red |
| M5b: `SessionMessageCount` page-decode swallow | 4 red |

Note: M2 as first attempted (swallow Decode error, keep Token check) only failed empty_body — the Token check backstops partial swallows. Defense in depth, recorded.

### 6. Leg registration

`cmd/workspace-agentd/faultmatrix/` contains NO leg-10 site registry/test list (grep for leg-10/wire-drift: zero hits) — the leg-10 site inventory lives in the #1312 assessment table (§1 row 10), which this PR's site list updates via the audit table above. No harness change made (pin-audit stream; minimal-footprint rule).

### 7. Review round 1 — extended sweep (r1 finding: audit incomplete)

The reviewer confirmed all round-1 claims by execution but ruled the audit incomplete: live same-class parse sites beyond the assessment's two files were unenumerated. Validated real, then pinned/fixed:

| Site (r1 finding) | Before | After |
|---|---|---|
| `pkg/agent/opencode/adapter.go:359` (`Adapter.Send` — same POST /session/:id/message wire as the seam; its lenient twin) | lenient Decode, loud only on syntax errors | strict via `decodeStrict` + 4-mode pin (`TestAdapter_Send_WireDriftCorruption`) |
| `pkg/agent/opencode/verifydelivery.go:87` (VerifyDelivery V1 page walk) | lenient Decode — a valid-empty-page + garbage tail could be read as PROVEN absence | strict + 4-mode pin asserting error AND `definitive=false` (never prove absence from drift) — the pin uses type-satisfying bodies (`[]garbage`), the phantom case itself |
| `pkg/agent/opencode/client.go:219` (`GetSessionStatuses`) | lenient Decode | strict + 4-mode pin (type-satisfying prefix — phantom all-idle map) |
| `pkg/agent/opencode/client_v2.go:139` (`PromptV2` envelope) | lenient Decode | strict + 4-mode pin — a phantom zero ack (admittedSeq 0) must never escape |
| `pkg/agent/opencode/client_v2.go:292` (`MessagesV2` envelope) | lenient Decode | strict + 4-mode pin |
| `cmd/workspace-agentd/client.go:143` (`ListSessions`, fillGaps path; #1379-touched file) | lenient Decode | strict (local `decodeStrict` twin, package main) + 4-mode pin |
| `cmd/workspace-agentd/client.go:181` (`fetchSessionTitle`) | **fully swallowed decode** (`_ = json.NewDecoder(...).Decode(&s)`) | strict + `log.Debug` on failure (Rule 3); contract unchanged (best-effort title, never fails the listing) — pinned by `TestOpenCodeClient_FetchSessionTitle_DriftStaysBestEffort` with a zap observer asserting the drift is logged |

Two r1 pins initially passed for the wrong reason (their target types rejected the generic corrupt body outright, so trailing-garbage was never exercised): fixed with type-satisfying bodies — `[]garbage` for the verify page walk (the real phantom-absence shape) and `{"ses_1":{"type":"idle"}}garbage` for the status map. RED before the strict switch at all six extended sites (trailing_garbage); GREEN after.

Also applied r1 minors: `decodeStrict` uses `errors.New` for the constant case and wraps the underlying `dec.Token()` error (names the offending byte).

### 8. Round-2 mutation red-checks

| Mutation | Result |
|---|---|
| M6a/M6b: adapter.go / verifydelivery.go reverted to lenient Decode | trailing_garbage red at each (2 red) |
| M7: agentd `ListSessions` decode swallowed | 4 red (all modes) |
| M8: `fetchSessionTitle` reverted to the swallowed decode | `DriftStaysBestEffort` red via the observer (the log assert is load-bearing) |

### 9. Review round 2 — the remaining 11 same-class sites (r2 finding)

The reviewer reproduced a live defect on HEAD (`MessagePresence`: `[]garbage` page → `err=nil, present[msg_1]=false` — absence PROVEN from drift, flowing into definitive ledger transitions) and enumerated 10 further lenient opencode-wire decodes. All validated, all closed (commit r2):

| Site (r2 finding) | Before | After |
|---|---|---|
| `sessionstate_wiring.go:156` `MessagePresence` page walk | **live defect: drift → proven absence** | strict + 4-mode pin (`err`, no presence map) — type-satisfying `[]garbage` body, the phantom case itself |
| `sessionstate_wiring.go:453` `Admit` ack decode | lenient — phantom `info.id` keys promotion correlation | strict + 4-mode pin |
| `sessionstate_wiring.go:379` `post(out)` | lenient (latent: live callers pass `out=nil`) | strict + 4-mode pin (direct `post` subtests) |
| `client.go:67` `IsHealthy` | lenient — phantom healthy feeds readiness gate + watchdog | strict + 4-mode pin (type-satisfying `{"healthy":true,...}garbage`) |
| `client.go:82/97` `ConnectedProviders`/`ConfiguredProviderCount` | lenient — phantom provider vitals | strict + 4-mode pins |
| `client.go:122` `ModelContextLimit` | lenient + unlogged swallow → 0 | strict + logged (fail-open 0 by contract), observer-pinned |
| `client.go:237` `fetchSessionPromptTokens` | lenient (`[]garbage` decodes cleanly, no log) → phantom token-absence | strict + logged, observer-pinned |
| `workflow_execute.go:370` agent-node message decode | lenient — phantom output from valid prefix | extracted `parseAgentNodeResponse` (strict) + 4-mode pin |
| `workflow_execute.go:470` `createOpencodeSession` | lenient — phantom session ID (delete path would target it) | extracted `parseCreatedSessionID` (strict) + 4-mode pin |
| `relay_injector.go:182` `fetchFreeModels` | lenient — phantom empty free-model catalog | strict + 4-mode pin (slice-typed `all` — first body failed on type mismatch, corrected) |

Harness note: the workflow path dials the fixed `127.0.0.1:AgentPort` by design (design 0053 containment); fixed-port listener pins SKIP wherever the real opencode occupies 4096 (this pod!). The decodes were therefore extracted into helpers (`parseAgentNodeResponse`/`parseCreatedSessionID`) and pinned deterministically at the parse seam.

Out-of-class remainder (enumerated, NOT opencode wire — left lenient deliberately): `bootstrap.go:252` + `pre_boot_relay.go:194` (platform/API-side wire or staged files), `control_client.go`/`control_socket.go` (agentd control mux), `mcp_server.go:80`/`workflow_execute.go:91` (inbound request decodes), `model_enricher.go:166` (external provider model lists), `client.go:44` (decodeStrict itself).

### 10. Round-3 mutation red-checks

| Mutation | Result |
|---|---|
| M9a `MessagePresence` reverted | trailing red (the reproduced defect re-detected) |
| M9b `IsHealthy` reverted | trailing red |
| M9c `parseAgentNodeResponse` reverted | trailing red |
| M9d `fetchFreeModels` reverted | trailing red |
| M9e `Admit` decode reverted | trailing red |
| M9f `post(out)` reverted | `post/trailing_garbage` red |

RED evidence pre-fix (r2 sites): trailing_garbage failures at IsHealthy, ConnectedProviders, ConfiguredProviderCount, MessagePresence, Admit, post, fetchFreeModels, agentMessage parse, FailOpenDisplayValues log assert.

### 11. Review round 3 — fetchList + cosmetics (r3 findings)

The r3 review reproduced one more live in-class defect on HEAD and two cosmetics; all closed:

- **`sessionstate_wiring.go` `fetchList` (the `SessionStates` boot-reseed path)** — a corrupted 200 on `/question`//`/permission` rode silently in as `err=nil` with empty PendingInputs (phantom-empty pending inputs on the authority/reseed path; reproduced by the reviewer with the identical `[]garbage` body). Fix: the corrupted-200 decode now ERRORS (`gather %s: malformed body`); the documented 404/conn-refused → authoritative-empty boot contract is unchanged (opencode versions without the endpoints never had questions). `SessionStates` propagates the error. 4-mode pin: `TestOpencodeStoreReader_SessionStates_WireDriftCorruption`. Mutation M10 (swallow reverted) → 4 subtests red.
- Cosmetic: duplicated Admit comment block removed (introduced by the r2 scripted edit); duplicated §8 table in this worklog removed (edit error).
- Enumerated drift-safe loud sites (error-propagating `Unmarshal` on the same wires — NOT in the phantom-success class, no pin needed): `sessionstate_wiring.go` `opencodeActor.post`, `pkg/agent/opencode/adapter_helpers.go` `parseProviderCatalogForContract`.

### 12. Review round 4 — ListPending + fetchList item level + MECHANICAL inventory (r4 findings)

r4 findings validated and closed, plus the reviewer's pattern note adopted: the site inventory is now derived MECHANICALLY (every `json.NewDecoder|json.Unmarshal` in `pkg/agent/opencode/*.go` + `cmd/workspace-agentd/*.go` + the proxy pending-input handlers, classified per opencode endpoint), not asserted per file.

**r4 fixes:**
- `Adapter.ListPending` (`adapter.go` `parsePendingQuestions`/`parsePendingPermissions`): every corruption mode was swallowed into `(empty, nil)` — fabricating the "no pending input" verdict the fail-closed dismiss path (`proxy_inbox.go` `askLivenessOf`) treats as positive death evidence. Both helpers now return errors (read/malformed/item-drift); `ListPending` wraps with `ErrPendingUnavailable`; the documented 404-authoritative-empty is preserved by skipping the parse on 404 (the 404 body is an error object, not an array) — pinned.
- `fetchList` item-level (r4 reproduced `[{"sessionID":"ses_1"},…]` → `err=nil, PendingInputs=0`): item parse failure now errors ("item drift"), matching `fetchListStrict`; `io.ReadAll` error propagated.

**Pins (RED pre-fix):** `TestAdapter_ListPending_WireDriftCorruption` (4 modes → `ErrPendingUnavailable`), `TestAdapter_ListPending_ItemDriftErrors`, `TestOpencodeStoreReader_SessionStates_ItemDriftErrors`, plus the 404-authoritative-empty contract pins on both paths (green before and after — contract preserved). Mutations M11a/M11b (item guards reverted) → both ItemDrift pins red.

**Mechanical inventory — every opencode-wire decode site, classified (complete, grep-derivable):**

| Endpoint(s) | Site | Status |
|---|---|---|
| POST /session, /session/:id/message, GET /session, /session/:id, /session/status, /config/providers, /api/session/:id/context | seam `loopback.go` 7 sites | strict + 4-mode pins (r1) |
| GET /session/:id/message (adapter Send + verify page walk + V2 prompt/messages + status map) | `adapter.go` Send, `verifydelivery.go`, `client.go` GetSessionStatuses, `client_v2.go` ×2 | strict + pins (r2) |
| translate.go pure parsers (history/session list/session get) | fixtures (#730) + corruption pins | covered (r1) |
| GET /question, /permission | agentd `fetchList` (SessionStates reseed) | strict body+item, errors; 404 authoritative-empty pinned (r3+r5) |
| GET /question, /permission | agentd `fetchListStrict` (PendingInputs lease) | loud by construction (pre-existing) |
| GET /question, /permission | `Adapter.ListPending` helpers | strict body+item → `ErrPendingUnavailable`; 404 pinned (r5) |
| GET /session/:id/message pages | agentd `MessagePresence` | strict + pin (r3) |
| POST /session/:id/message (admitter ack, `post(out)`) | `sessionstate_wiring.go` | strict + pins (r3) |
| GET /provider | agentd `fetchFreeModels`, `ConnectedProviders` | strict + pins (r3) |
| /config/providers | agentd `ConfiguredProviderCount`, `ModelContextLimit`(+log), `parseProviderCatalogForContract` (loud) | strict + pins / loud (r3) |
| /global/health, listing, title, prompt-tokens, agent-node, created-session | agentd `client.go` + `workflow_execute.go` helpers | strict + pins (r2/r3) |
| SSE `data:` event lines | `client_events.go`, `translate_abi.go`, `dialect.go`, `session_tracker.go`, `adapter.go` event internals | per-event dialect, drop-unknown semantics — pinned by `TestTranslateSSEEvent_MalformedJSON_Dropped`, `TestClientEventsFromNextIgnoredTypes`, golden SSE fixtures (out of HTTP-body class) |
| Config/marker FILES (agent-config.json, auth.json, model-resolution-warning, staged catalogs, secrets-env, XDG layer, rev anchors) | `configwriter*.go`, `opencode.go`, `relay_injector.go` file ops, `healthz.go`, `pre_boot_relay.go`, `xdg_config_layer.go`, `rev_anchor.go`, `restart_reason.go`, `secrets.go`, `spawn_*` | platform-owned files, not agent wire (out of class) |
| Platform/API wire (pod-bootstrap, spawn-env/files) | `bootstrap.go`, `spawn_*.go` | platform server responses, not agent wire (out of class) |
| agentd control mux | `control_client.go`, `control_socket.go` | internal IPC (out of class) |
| Inbound request decodes | `mcp_server.go`, `workflow_execute.go:91` | client-supplied bodies, validated upstream (out of class) |
| External provider APIs | `model_enricher.go` | not opencode wire (out of class) |
| Contract-internal Custom.Data re-parses | `vision_gate.go` | operates on already-parsed contract parts (out of class) |

(Cosmetic: stale `:737` cite for `opencodeActor.post` superseded by this table.)

### 13. Review outcome

Round 5 (commit `3adcb1c1`): **APPROVED** — both r4 findings verified closed by execution (M11 red-checks independently reproduced, pre-fix reds rebuilt from the `7de57267` tree), and the mechanical inventory independently re-derived by two review passes with zero unaccounted in-class sites. Non-blocking follow-ups recorded by the reviewer (all out-of-class or pre-existing):
1. Enumerate the agentd `/v1/statusz` decodes in `api/internal/handlers` (`proxy_connections.go:219`, `proxy_events.go:236`, `proxy_lifecycle.go:539`) for future sweeps — verified loud/defaulted.
2. `mcpSecretsResync` self-call decodes (`mcp_server.go:613-645`) — loud/defaulted.
3. Workflow structured-output text re-parse (`workflow_execute.go:378`) — loud `schema_mismatch`.
4. `fetchList` boot branch conflates any ≥400 (not just 404) into authoritative-empty — pre-existing settled contract; doc comment could name the actual status set; a 5xx-during-boot pin is nice-to-have.
5. Stale doc comment on `parseProviderCatalogForContract` (`adapter_helpers.go:78-79`, introduced `13114e52`, file untouched here) — drive-by fix on a future touch.

---

## Key Decisions

1. **Port the 4-mode corruption contract, not the abitest knob.** `abitest.CorruptMode` is the ABI connect-rpc fake; the seam is plain HTTP and translate.go is pure functions. The pins define the same four body shapes locally with the abitest names in comments — same contract, no cross-package test dependency.
2. **Fix the trailing-garbage tolerance rather than pin around it.** Leg-10's contract at the 3 green sites is "never a silent success" across all four modes; leaving the seam tolerant would be pins-in-name-only. The fix is 8 lines, seam-local.
3. **Discrimination assertion (`decode` substring)** — mirrors the terminus pin's r1 fix; proven necessary by M1 (ModelInfo's not-found sentinel masking).
4. **Fail-open sites documented, not pinned:** `SessionPromptTokens` (display-only, 0 on failure by contract — Epic-36 formula tests pin its shape), history trailing-garbage-after-complete-array (indistinguishable from the #737/#730 graceful-partial design — truncated bodies return partial results by design), `ParseHistoryStream` per-message downgrade (pinned by `OneMalformedPart_DoesNot502`).

---

## Blockers

None.

---

## Tests Run

- RED (pre-fix, round 1): 5 FAIL (trailing_garbage × 5 seam Decode sites)
- RED (pre-fix, round 2): 6 FAIL (trailing_garbage × Adapter.Send, VerifyDelivery, GetSessionStatuses, PromptV2, MessagesV2, agentd ListSessions)
- RED (pre-fix, round 3): 10 FAIL (trailing_garbage × IsHealthy, ConnectedProviders, ConfiguredProviderCount, MessagePresence, Admit, post, fetchFreeModels, agentMessage/createdSessionID parses + FailOpenDisplayValues log assert)
- GREEN (post-fix): all pins ok
- Full gates: `go test -race -timeout 300s ./pkg/agent/opencode/` → **ok ~20s**; `go test -race -timeout 600s -count=1 ./cmd/workspace-agentd/` → **ok ~309s**
- Mutation red-checks: r1 M1 (5), M2b (20), M3 (4), M4 (1), M5a/M5b (4+4); r2 M6a/b (2), M7 (4), M8 (1); r3 M9a-f (one trailing red per site, incl. `post`); r4 M10 (4); r5 M11a/M11b (2) — all restored
- RED (pre-fix, round 4): `SessionStates_WireDrift` 4 FAIL (corrupted /question → phantom-empty pending inputs)
- RED (pre-fix, round 5): `ListPending_WireDrift` 4 FAIL + both ItemDrift FAILs; 404 contract pins green throughout
- `golangci-lint run ./pkg/agent/opencode/... ./cmd/workspace-agentd/...` → 0 issues
- `gofmt -l` → clean; `make repolint` → all checks passed
- faultmatrix NOT touched (no harness change) — CI runs it

---

## Next Steps

1. (Deferred, needs live pod) Capture real 1.18.x fixtures for the seam's decode sites (POST /session response, /session/:id/message response, /api/session/:id/context, /config/providers catalog) into `pkg/agent/opencode/testdata/` — the #730 REFRESH.md discipline; closes the renamed-field blind spot that corruption pins cannot see (except `SessionCreate`'s id and the workflow created-session id).
2. (Deferred) The L2 real-binary integration leg (`loopback_integration_test.go`, build-tagged) is the strongest seam drift guard — consider scheduling it in CI on the pinned binary.
3. Reviewer-verified merge of this PR updates #1312 §1 row 10 to "green at 3+9+11+1+2 sites (mechanical inventory in the worklog)".
4. (Note) The workflow agent-node path dials the fixed `127.0.0.1:AgentPort` with no override seam — the r2 pins therefore target the extracted parse helpers; a future addr-override seam would allow socket-level pins.

---

## Files Modified

- `pkg/agent/opencode/loopback.go` — `decodeStrict` helper + 5 seam Decode call sites switched
- `pkg/agent/opencode/loopback_test.go` — `TestSeam_WireDriftCorruption` (28 subtests), `TestSeam_SessionCreate_IDDriftLoud`, shared `leg10DriftModes`/`seamDriftServer` helpers
- `pkg/agent/opencode/translate_test.go` — `TestParseSessionListWire_CorruptBody`, `TestParseSessionWire_CorruptBody` (8 subtests)
- `pkg/agent/opencode/adapter.go` — Send decode strict (r1); ListPending helpers strict, ErrPendingUnavailable, 404-empty preserved (r5)
- `pkg/agent/opencode/adapter_test.go` — `TestAdapter_Send_WireDriftCorruption` (r1)
- `pkg/agent/opencode/verifydelivery.go` — V1 page walk strict (r1)
- `pkg/agent/opencode/verifydelivery_test.go` — `TestVerifyDelivery_WireDriftCorruption` (r1)
- `pkg/agent/opencode/client.go` — `GetSessionStatuses` strict (r1)
- `pkg/agent/opencode/client_test.go` — `TestGetSessionStatuses_WireDriftCorruption` (r1)
- `pkg/agent/opencode/client_v2.go` — `PromptV2`/`MessagesV2` envelope decodes strict (r1)
- `pkg/agent/opencode/client_v2_test.go` — `TestPromptV2_WireDriftCorruption`, `TestMessagesV2_WireDriftCorruption` (r1)
- `cmd/workspace-agentd/client.go` — local `decodeStrict`; all wire decodes strict (ListSessions, fetchSessionTitle, IsHealthy, ConnectedProviders, ConfiguredProviderCount, ModelContextLimit+log, fetchSessionPromptTokens) (r1+r2; #1379-touched file)
- `cmd/workspace-agentd/client_drift_test.go` — agentd client drift pins (r1+r2)
- `cmd/workspace-agentd/sessionstate_wiring.go` — MessagePresence / Admit / post decodes strict (r2); fetchList corrupted-200 → error, boot 404 contract unchanged (r3); duplicate comment removed
- `cmd/workspace-agentd/sessionstate_wiring_test.go` — MessagePresence + Admitter drift pins (r2); SessionStates drift pin (r3)
- `cmd/workspace-agentd/workflow_execute.go` — `parseAgentNodeResponse` / `parseCreatedSessionID` extracted + strict (r2)
- `cmd/workspace-agentd/workflow_execute_test.go` — workflow parse drift pins (r2)
- `cmd/workspace-agentd/relay_injector.go` — fetchFreeModels strict (r2)
- `cmd/workspace-agentd/relay_injector_test.go` — fetchFreeModels drift pin (r2)
- `COORDINATE.md` — claim row
- `worklogs/0961_2026-09-15_leg10-wire-drift-pins.md` — this worklog
