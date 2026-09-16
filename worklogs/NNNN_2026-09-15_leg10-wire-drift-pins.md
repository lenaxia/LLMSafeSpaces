# Worklog: leg-10 wire-drift pins at the opencode seam parse sites

**Date:** 2026-09-15
**Session:** epic-71 / leg10-pins — close the #1312 assessment gap: the #1379-era parse sites (`pkg/agent/opencode/loopback.go`, `translate.go`) lacked verified leg-10 (wire drift) pins. Audit, pin red-first, mutate-to-red evidence.
**Status:** Complete

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

- RED (pre-fix): `go test -race -run 'TestSeam_WireDriftCorruption|...CorruptBody|...IDDriftLoud' ./pkg/agent/opencode/` → 5 FAIL (trailing_garbage × 5 Decode sites) — the true gap
- GREEN (post-fix): same run → ok
- Full package gate: `go test -race -timeout 300s ./pkg/agent/opencode/` → **ok 20.195s**
- Mutation red-checks M1/M2b/M3/M4/M5a/M5b: 5/20/4/1/4/4 red respectively (all restored; final suite green)
- `golangci-lint run pkg/agent/opencode/...` → 0 issues
- `gofmt -l pkg/agent/opencode/` → clean
- faultmatrix NOT touched (no harness change) — not re-run; CI runs it

---

## Next Steps

1. (Deferred, needs live pod) Capture real 1.18.x fixtures for the seam's decode sites (POST /session response, /session/:id/message response, /api/session/:id/context, /config/providers catalog) into `pkg/agent/opencode/testdata/` — the #730 REFRESH.md discipline; closes the renamed-field blind spot that corruption pins cannot see (except `SessionCreate`'s id).
2. (Deferred) The L2 real-binary integration leg (`loopback_integration_test.go`, build-tagged) is the strongest seam drift guard — consider scheduling it in CI on the pinned binary.
3. Reviewer-verified merge of this PR updates #1312 §1 row 10 to "green at 3+9 sites".

---

## Files Modified

- `pkg/agent/opencode/loopback.go` — `decodeStrict` helper + 5 Decode call sites switched
- `pkg/agent/opencode/loopback_test.go` — `TestSeam_WireDriftCorruption` (28 subtests), `TestSeam_SessionCreate_IDDriftLoud`, drift fixtures/helpers
- `pkg/agent/opencode/translate_test.go` — `TestParseSessionListWire_CorruptBody`, `TestParseSessionWire_CorruptBody` (8 subtests)
- `COORDINATE.md` — claim row
- `worklogs/NNNN_2026-09-15_leg10-wire-drift-pins.md` — this worklog
