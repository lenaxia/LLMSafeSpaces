# Worklog: OpenCode message endpoint — model must be the object wire form

**Date:** 2026-08-17
**Status:** Complete
**Session:** Fix opencode per-prompt model wire form (object, not string)
**PR:** (this change)
**Issue:** #911 (relay-router /provider timeout — related incident class);
          regression introduced by #909

---

## Objective

Fix the live bug where every per-prompt model override sent by the adapter
fails opencode 1.18.10's schema decode. The adapter sends the per-prompt
`model` field as the STRING `"providerID/modelID"`; opencode 1.18.10's
`POST /session/{id}/message` requires an OBJECT `{"modelID","providerID"}`
(both required, `additionalProperties: false`). Verified directly against
the pinned runtime's own schema (`packages/sdk/openapi.json` @ `v1.18.10`,
the version pinned by `OPENCODE_VERSION` in `runtimes/base/Dockerfile`).

## Evidence

- `packages/sdk/openapi.json` @v1.18.10,
  `POST /session/{sessionID}/message` → `model`: type object,
  `{providerID: string, modelID: string}` required, additionalProperties
  false. A string fails decode ("Expected object | null, got string") —
  the all-sessions-502 incident class from #909, missed because the mocked
  tests asserted the string shape and no real-schema validation existed.
- The V1 `Send` (`adapter.go`) and the V2 `SendAsync`
  (`client_v2.go`/`PromptV2WithModel`) both built the string form.
- `PATCH /config` is the EXCEPTION: its `Config.model` schema is
  `type: string` — `SetModel`/`PatchConfig` correctly send the joined
  `"providerID/modelID"` string and must NOT change.

## Work Completed

1. **`pkg/agent/opencode/adapter.go`**
   - Replaced `qualifiedModelID` (string builder) with `modelOverride`
     returning the split `(modelID, providerID, ok)`, applying the
     #913-round-5 Provider-authoritative rules: Provider present → it is
     the routing providerID, an already-prefixed ID is stripped of it
     (never `x/x/y`), and a slashed ID with a different first segment
     (frontend double form) keeps the full ID as modelID; Provider absent
     → first-segment split (opencode's routing rule); bare flat IDs and
     empty-tail (`a/`) shapes return `ok=false` (omitted → session
     default).
   - `Send`: `body["model"]` is now the object
     `map[string]string{"modelID": mid, "providerID": prov}`.
   - `SendAsync`: builds a `*V2ModelRef` from `modelOverride` and passes it
     to `PromptV2WithModel` (same object-form contract).
2. **`pkg/agent/opencode/client_v2.go`**
   - `v2PromptBody.Model` is now `*V2ModelRef` (object form, omitempty on
     the pointer); `PromptV2WithModel` takes `model *V2ModelRef`.
3. **Tests** (`pkg/agent/opencode/adapter_test.go`)
   - `TestAdapter_Send_ModelReferenceForm` rewritten for the object form:
     every subtest now passes through a schema-shape guard asserting the
     wire `model` is an object with EXACTLY `modelID`+`providerID` string
     keys when present — the regression test for the #909 failure mode.
     New Provider-authoritative, provider-less-first-segment,
     frontend-double-form, and empty-tail (with and without matching
     provider) cases.
   - `TestAdapter_SendAsync_ModelReferenceForm` asserts the object form on
     the V2 path.
   - `api/internal/handlers/e2e_adapter_test.go`: both full-pipeline
     forwarding pins (`TestE2E_Adapter_SendPromptAsync_ModelForwarding`,
     `TestE2E_Adapter_SendMessage_ModelForwarding`) now assert the object
     form at the backend — these were the "mocks enshrining the string
     form" that let #909 through.
   - Unchanged (correct): `TestAdapter_SetModel...` and
     `TestClient_PatchConfig_HappyPath` assert the string form on
     `PATCH /global/config`, matching the `Config.model` string schema.

## Key Decisions

1. **One primitive (`modelOverride`) for both send paths** — guarantees the
   V1 and V2 wire forms can't drift, and gives the split logic a single,
   tested definition. The V2 path is dormant on 1.18.10 (#755) but stays
   wired for revival; its own contract said it "must honor the same
   model-form contract as Send."
2. **Provider-authoritative split** (not the first-slash-everything split
   the salvage stash attempted) — the frontend double form
   (`{ID:"anthropic/claude-4.5", Provider:"openrouter"}`) must keep the
   vendor namespace in `modelID`; naive first-slash split would have put
   "anthropic" in `providerID` and reintroduced the #913-round-5
   misroute.
3. **Schema-shape guard in the shared inspect helper** — mirrors upstream
   `openapi.json` (object, exactly two string keys, additionalProperties
   false) so a future revert to the string form fails CI in both
   directions, not just the targeted cases.

## Blockers

None.

## Tests Run

- `go test -count=1 ./pkg/agent/opencode/ ./api/internal/handlers/` →
  PASS (opencode 12.2s, handlers 126.4s).
- Red/green: with adapter.go + client_v2.go reverted to the string-form
  code, `TestAdapter_Send_ModelReferenceForm` FAILS on the schema-shape
  guard (confirmed). Green with the fix.
- `go build ./...` PASS; `go vet ./pkg/agent/opencode/
  ./api/internal/handlers/` PASS; `gofmt -l` clean.

## Next Steps

1. PR + review (first review under the new onboarded ai-workflows
   pipeline).
2. The superseded `not-mine: #911 wire-form object fix` stash (stash@{0})
   can be dropped once this lands — this change is the rebased, corrected
   implementation of it. Confirm with the originating session first.
3. Out of scope here: #911's relay-router /provider timeout is a separate
   issue.

## Files Modified

- `pkg/agent/opencode/adapter.go`
- `pkg/agent/opencode/client_v2.go`
- `pkg/agent/opencode/adapter_test.go`
- `api/internal/handlers/e2e_adapter_test.go`
- `api/internal/handlers/proxy_handlers.go` (comment: stale qualifiedModelID reference)
- `worklogs/NNNN_2026-08-17_model-wire-form-object.md`
