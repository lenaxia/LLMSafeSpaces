# 0056 — Harness-ABI IDL, transport, and type source-of-truth (US-69.1, D5)

**Status:** Accepted (2026-08-30, owner) — implements design 0055 D5 ("IDL toolchain at S1 start; transport chosen with the toolchain")
**Date:** 2026-08-30
**Epic:** #1134 / US-69.1 (#1135)
**Affects:** `pkg/abi/` (schema + generated code), agentd sessionstate (US-69.2+), API client (S1 shadow), frontend (S3), CI abi-schema job

---

## Context

Design 0055 froze the *what* — five operations in platform schema — and
deferred two decisions to S1 start: the IDL toolchain + transport (D5), and
the `pkg/session` ↔ schema source-of-truth arrangement with Epic 65. Both
are recorded here. The deciding frame: **long-term stability = one contract,
one authority per fact, minimal irreversible commitments.** Every historical
fragility in this repo (relay-config, dual turn-lifecycle derivations, the
verify oracle) came from two components knowing one fact; the same disease
applies to two hand-maintained type sets.

---

## Decisions

### T1 — IDL: protobuf, managed by buf

The schema is the ABI and the durable asset; transports and stubs are
projections of it.

- Schema lives in `pkg/abi/llmsafespaces/abi/v1/*.proto`; buf manages lint,
  breaking-change detection, and codegen (`buf.yaml`, `buf.gen.yaml`).
- Lint runs buf STANDARD with two documented exceptions
  (RPC request/response naming — the design-0055/#1135 domain vocabulary
  `DeliveryRequest`/`DeliveryAck`/`ActionResult`/`StreamFrame` is deliberate).
  `ENUM_VALUE_PREFIX` stays ON — it is load-bearing: proto enum values are
  package-scoped and unprefixed names collide across enums.
- Breaking-change policy follows D5 exactly: the schema evolves freely during
  the S1 shadow; **it freezes at S2 entrance** (US-69.8 writes the baseline
  git ref into `abi/FROZEN`, arming `make abi-breaking` in CI). Until then
  the gate is proven on fixtures (`TestSchemaLintFreezeGate`).

### T2 — Transport: Connect RPC (connect-go)

Connect over gRPC, decided on operational merit — and cheap to revisit,
because both transports generate from the same schema (the transport is a
codegen flag, not an architectural commitment).

Why Connect wins in this topology:

| Factor | Connect | gRPC |
|---|---|---|
| Composition with agentd :4097 | generated handler is an `http.Handler`; drops into the existing mux next to healthz/files behind the existing Basic-auth middleware | own server loop; cmux/h2c dual-stack; Basic auth becomes a gRPC interceptor |
| Proxy chain | no trailers; works over HTTP/1.1 through the Gin reverse proxy, `kubectl port-forward`, any hop | HTTP/2 + trailers end-to-end — a permanent fragility through every proxy hop |
| Debuggability under starvation (R5/M3.3) | JSON codec: curl the snapshot endpoint during a stall | JSON not native |
| Future harness adapters (pi, claude-code) | lowest implementation barrier — any HTTP server | full gRPC stack per adapter |
| Interop | Connect server speaks the gRPC wire protocol for free | — |
| Maturity | stable since 2023, maintained by Buf | industry default |

### T3 — Type source of truth: schema-authoritative, phased to D5

One source of truth is the only stable end-state; two hand-maintained
representations of one contract WILL drift. The migration is phased to D5:

1. **S1 (now):** the proto schema is wire-authoritative for the 5-op ABI;
   `pkg/session` remains the in-memory Go contract. Equivalence is pinned by
   `TestPkgSessionContractParity` — field-name parity per paired message,
   value-set parity per paired enum, both directions. Drift fails CI.
2. **S2 entrance (freeze):** `pkg/session` flips to generated types (type
   aliases where shapes match 1:1, thin wrappers where Go ergonomics demand
   constructors) as an Epic 65-owned story; the parity scaffold is deleted
   in the same change. Two sources of truth do not survive the freeze.
3. Translation rules that exist only during the transition (and inside the
   opencode adapter afterwards) are documented in the parity test: strip the
   enum prefix, lowercase, `_`→`.` for the dotted event set.

### T4 — Governance: codegen freshness + no hand-written wire

- All stubs are generated, committed, and verified fresh in CI
  (`make abi-check`: regen + `git diff --exit-code`). A hand edit cannot
   survive regeneration.
- Zero hand-written wire structs in the surface path
  (`TestNoHandWrittenWire`).
- Generated contract tests run against the reference in-memory
  implementation (`pkg/abi/abitest`): round-trip fidelity, unknown-field
  survival (the forward-compat property the freeze relies on), the frozen
  five-op surface, closed unions, and NotSupported expressibility over both
  codecs.

---

## Consequences

- `connectrpc.com/connect` enters the Go module graph (v1.20.0; the API
  client and agentd server both consume it).
- Frontend gains `@bufbuild/protobuf` (runtime) and
  `@bufbuild/protoc-gen-es` (devDep); generated TS types land under
  `frontend/src/abi/llmsafespaces/abi/v1/` now, consumed in S3 (US-69.10).
- The `bytes input`/`bytes output` fields on `ToolPart` (raw JSON) trade
  JSON-codec readability for golden-fixture fidelity and unknown-shape
  preservation — the same fidelity `pkg/session`'s `json.RawMessage` fields
  exist for.
- Schema changes during S1 are additive-first; any wire-incompatible change
  must update the parity test and all consumers in the same PR.

## Assumptions (validated)

| # | Assumption | Validation |
|---|---|---|
| A1 | buf + local protoc plugins suffice (no BSR dependency) | `make abi-check` green; tools pinned in Makefile; CI installs them |
| A2 | connect-go v1.20 error details round-trip through both codecs | `TestNotSupportedExpressible/codec_json` |
| A3 | protoc-gen-go `module=` option emits into `pkg/abi/v1` from the buf-standard proto tree | `buf generate` output inspected; freshness gate green |
| A4 | proto3 unknown-field preservation holds in protobuf-go v1.36.11 | `TestContractGeneratedRoundtrip` unknown-field subtest |
| A5 | `pkg/session` JSON tag names ↔ proto JSON names match 1:1 for all paired types | `TestPkgSessionContractParity` (16 messages, 7 enums) |

## Rejected alternatives

- **gRPC** — trailer fragility through the proxy chain, non-composable with
  agentd's existing HTTP server, JSON-debuggability absent; interpo loss is
  nil (Connect serves the gRPC protocol).
- **JSON-with-codegen (e.g. OpenAPI/TypeShare)** — no breaking-change
  tooling comparable to buf, no cross-language contract tests, weaker
  oneof/discriminated-union ergonomics for the closed unions this ABI is
  built around.
- **Immediate pkg/session migration to generated types** — churn across
  API/frontend/SDK consumers against a schema that is deliberately still
  moving (D5); pays the migration cost twice.
- **Permanent dual-track with parity tests only** — parity tests compare
  shape, not semantics; keeping them forever is permanent debt and a
  standing drift risk the freeze exists to eliminate.
