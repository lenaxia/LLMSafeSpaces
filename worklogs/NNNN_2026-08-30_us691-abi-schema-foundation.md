# Worklog: US-69.1 — harness-ABI IDL, schema, codegen, transport decision

**Date:** 2026-08-30
**Session:** Epic 69 (#1134) US-69.1 (#1135): IDL & schema foundation (D5) — proto schema, buf toolchain, Connect codegen, contract tests, CI freeze gate, transport ADR + Epic 65 source-of-truth agreement.
**Status:** Complete

---

## Objective

Land the D5 "IDL at S1 start" deliverable: the full harness-ABI surface as schema (five ops, Epic 65 contract types, capability report, cursors), codegen for all three consumers (agentd server stub, API client, frontend TS types), generated contract tests against a reference in-memory implementation, CI wiring with lint + breaking-change detection armed for the S2 freeze, and the two owed decisions recorded (transport; `pkg/session` ↔ schema source of truth).

---

## Work Completed

### Schema + toolchain (T1)
- `pkg/abi/llmsafespaces/abi/v1/` — six protos: `abi.proto` (HarnessABIService: Events/GetSnapshot/Deliver/GetDeliveryStatus/Act + StreamFrame/SnapshotFrame/SequencedEvent/ReseedNotice + SessionSnapshot/PodSnapshot), `contract.proto` (Epic 65 mirror: 16 messages, 7 enums), `delivery.proto` (parts-capable DeliveryRequest per D3, LedgerState machine), `action.proto` (op-5 union + typed results + effect_seq), `capability.proto` (provenance, supported actions/part kinds, NotSupported error detail), `history.proto` (Cursor + history messages, defined-now/wired-at-S5 per design).
- `buf.yaml` (module `pkg/abi`, STANDARD lint with two documented RPC-naming exceptions; FILE breaking), `buf.gen.yaml` (protoc-gen-go + protoc-gen-connect-go with `module=` → `pkg/abi/v1`; protoc-gen-es → `frontend/src/abi/llmsafespaces/abi/v1/`).
- Makefile: `abi-generate`, `abi-lint`, `abi-breaking` (arms via `abi/FROZEN`, D5), `abi-check` (lint + breaking-if-frozen + regen freshness), `abi-codegen-tools` (pinned: buf v1.72.0, protoc-gen-go v1.36.11, protoc-gen-connect-go v1.20.0), folded into `tools-install`.

### Contract tests (TDD — written first, red confirmed before codegen)
`pkg/abi/` — all named per issue #1135's test plan:
- `TestSchemaSurfaceCompleteness` — exactly 5 ops, Events server-streaming, ~40-message coverage incl. Cursor/History, closed unions (PartType=5 forever, LedgerState=M2 machine, EventType=pinned set, ActionType=op-5 union). The ABI cannot silently shrink.
- `TestContractGeneratedRoundtrip` — every message, every oneof arm, deterministic population via protoreflect; binary round-trip + unknown-field survival (forward compat the S2 freeze relies on).
- `TestNotSupportedExpressible` — file-part delivery + undeclared action refusals as connect `CodeUnimplemented` + `NotSupported` detail, over proto AND an independently implemented JSON codec, against the reference server.
- `TestNoHandWrittenWire` — generated-tree marker purity; paired with `make abi-check` regen freshness (hand edits can't survive regeneration).
- `TestSchemaLintFreezeGate` — buf lint on live module; `buf breaking` proven on fixtures (field deletion detected, clean copy passes); freeze-marker state surfaced.
- `TestPkgSessionContractParity` — the Epic 65 dual-track guard: 16 paired messages field-name parity (json-tag ↔ proto JSON name), 7 paired enums value-set parity via documented canonicalization (strip enum prefix, lowercase, `_`→`.` for dotted events).

### Reference implementation
- `pkg/abi/abitest` — in-memory HarnessABIService over real connect handler (httptest): ledger map with (entry_id, attempt) dedupe, snapshot with in-flight parts + pending inputs, capability-gated refusals (file parts, compact). Seeds US-69.2's shape; production must not import it.

### Decisions recorded (ADR design/0056)
- **T2 transport: Connect RPC** (net/http composition with agentd :4097, no trailers through the proxy chain, JSON-debuggable under starvation, free gRPC interop, lowest barrier for future harness adapters). Reversible: same schema generates gRPC stubs.
- **T3 source of truth: schema-authoritative, phased** — S1 dual-track pinned by the parity test; at S2 freeze `pkg/session` flips to generated types as an Epic 65 story and the parity scaffold is deleted. Two sources of truth do not survive the freeze.
- Pointer added in design 0055 open-items.

### CI
- `abi-schema` job in ci.yml: pinned codegen tools, npm ci (protoc-gen-es), `make abi-check`, TS freshness diff, `go test -race ./pkg/abi/...`.
- Frontend deps: `@bufbuild/protobuf` (dep), `@bufbuild/protoc-gen-es` (devDep); `tsc --noEmit` covers the generated TS.

### Pre-existing breakage fixed (Rule 5)
- Removed four dead `replace` directives in go.mod (`llmsafespaces/pkg => ./pkg`, `mocks` ×3) — pointing at directories without go.mod; they mangled any import under those prefixes when the package didn't exist yet (surfaced by this work's pre-codegen imports). `go build ./mocks/... ./api/...` verified unaffected.
- `hack/add-license-headers.sh` excludes `pkg/abi/v1/` (generated trees must stay byte-stable under regeneration).

---

## Key Decisions

1. **Proto files live in `pkg/abi/llmsafespaces/abi/v1/`** (buf STANDARD directory layout) with `module=github.com/lenaxia/llmsafespaces` emitting generated Go at the clean import path `pkg/abi/v1`. First attempt (flat layout + bare imports) violated PACKAGE_DIRECTORY_MATCH; restructuring beat suppressing the rule.
2. **ENUM_VALUE_PREFIX stays enforced** — proto enum values are package-scoped; unprefixed `TEXT`/`ERROR` collide across enums. Contract-string mapping (strip prefix etc.) lives in the parity test.
3. **Two lint exceptions only** (RPC request/response naming) — keeps the design-0055/#1135 domain vocabulary (`DeliveryRequest`, not `DeliverRequest`).
4. **NotSupported rides connect error details** (CodeUnimplemented + typed detail) rather than an ActionResult member — errors stay errors; round-trip proven per codec.
5. **Service named `HarnessABIService`** — complied with SERVICE_SUFFIX rather than taking a third exception; the design doesn't pin the service name, only the ops.
6. **`bytes input/output` on ToolPart** (raw JSON, mirroring `json.RawMessage`) — fidelity over JSON-codec readability, matching pkg/session's own choice.

---

## Blockers

None.

---

## Tests Run

- `go test -timeout 120s -race ./pkg/abi/...` — PASS (all six suites; freeze gate runs with buf on PATH, skips with a message otherwise; CI installs buf).
- `buf lint` — clean. `make abi-check` — PASS (lint + advisory breaking + freshness).
- `npm run typecheck` (frontend) — PASS over generated TS.
- `go build ./mocks/... ./api/...` — PASS after replace-directive removal.
- Full-suite gates (`make test`, `make lint`) deferred to CI/pre-push in this environment; pkg scope + typecheck verified locally.

---

## Next Steps

1. US-69.2 (#1136): sessionstate module — ingest from the existing SSE subscription, seq assignment under the projection lock, reseed path, 4097 listener with agentdPassword auth (the ABI handler lands here, consuming the generated `abiconnect` stubs; `abitest` is the shape reference).
2. US-69.3 (#1137): contract projection & translation (opencode dialect → `pkg/abi/v1` contract types behind the pod seam; reuse the parity test's canonicalization rules).
3. Epic 65 coordination: file the S2-freeze flip story (`pkg/session` → generated types + delete parity scaffold) so it's scheduled before US-69.8 arms `abi/FROZEN`.
4. PR review loop for this branch (feat/epic-69-us691-abi-schema) — automated reviewer, iterate to APPROVE, squash merge.

---

## Files Modified

- `buf.yaml`, `buf.gen.yaml` (new)
- `pkg/abi/llmsafespaces/abi/v1/{abi,contract,delivery,action,capability,history}.proto` (new)
- `pkg/abi/v1/` + `pkg/abi/v1/abiconnect/` (generated, committed)
- `frontend/src/abi/llmsafespaces/abi/v1/*.ts` (generated, committed)
- `pkg/abi/{surface_completeness,roundtrip,not_supported,no_hand_written_wire,freeze_gate,parity_session_contract}_test.go` (new)
- `pkg/abi/abitest/server.go` (new)
- `abi/testdata/frozen/{buf.yaml,probe.proto}` (new)
- `Makefile`, `.github/workflows/ci.yml`, `hack/add-license-headers.sh`, `go.mod` (+`go.sum`), `frontend/package.json` (+lockfile)
- `design/0056_2026-08-30_harness-abi-idl-transport.md` (new), `design/0055_...md` (pointer edit)
