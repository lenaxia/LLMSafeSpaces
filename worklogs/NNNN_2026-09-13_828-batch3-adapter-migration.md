# Worklog: #828 batch 3 — input/permissions cluster adapter-only

**Date:** 2026-09-13
**Session:** epic-826 / #828 batch 3 (claim: [#828 comment](https://github.com/lenaxia/LLMSafeSpaces/issues/828#issuecomment-5641340986)). Agent: opencode.
**Status:** Complete

---

## Objective

Migrate the question/permission routes and their background helpers to adapter-only, deleting the raw-proxy tails — the mechanical prerequisite epic-71 wave 4a (#1302) gated on.

---

## Work Completed

### Routes (5) adapter-only

- **ListQuestions / ListPermissions**: nil-adapter 503 guard → `adapter.ListPending` filtered by kind → the normalized `agent.QuestionRequest`/`PermissionRequest` envelope (the shape the frontend already consumes and the SSE snapshot emits — one shared translation seam, `toQuestionRequest`/`toPermissionRequest`, extracted from `emitPendingViaAdapter` and deduplicated).
- **QuestionReply**: validation → `tryLateAnswer` (inbox walk-away flow, preserved verbatim) → guard → bounded body parse `{answers: string[][]}` (empty → 400) → **new `agent.Adapter.AnswerQuestion`** → inbox terminalize + activity → 200. The new adapter method posts the question-reply schema verbatim — `Resolve`'s `{reply}` body is additionalProperties-rejected by that endpoint, so question answers cannot ride it.
- **QuestionReject**: validation → guard → `adapter.RejectInput` (already existed, #1313) → inbox "dismissed" → 200.
- **PermissionReply**: validation → `tryLateAnswer` → guard → parse `{reply, message?}` → **new `agent.Adapter.ReplyPermission`** (both fields ride the agent schema — the legacy passthrough carried the message; the adapter method preserves it) → inbox + activity → 200.
- **RequestInputSnapshot**: dialect-guard → nil-adapter 503; the flight rides `emitPendingViaAdapter`.

### Background helpers

- **emitPendingInputRequests**: legacy two-fetch dialect-parsing tail deleted (`fetchFromPod`, `parseQuestionList`, `parsePermissionList`); nil adapter → early return with the deferred D10 `snapshot_ok=false` marker (clients keep state).
- **autoApprovePermission**: raw-HTTP tail deleted; nil adapter → warn + skip.

### Adapter surface

`agent.Adapter` += `AnswerQuestion(ctx, uid, wid, requestID, answers [][]string)` and `ReplyPermission(ctx, uid, wid, requestID, reply, message)`; opencode implementations post the respective schemas verbatim; `Resolve`/`RejectInput` unchanged (auto-approve and inbox keep consuming them).

### Test migration

- 13 new red-first rows (5 guards, 2 validation-precedes-guard pins, 2 envelope rows, AnswerQuestion-schema row + empty-answers 400, RejectInput row, ReplyPermission-with-message row, emitPending nil-adapter marker row).
- Ports: BeginAndOKMarkerOnSuccess + MarkerOKFalseOnBackendError → adapter mocks; FiresFlight → adapter mock; the two inbox live-reply/reject rows → AnswerQuestion/RejectInput mocks (name "StillProxies" retained with a predating-note); the question e2e round-trips → REAL adapter against the same pod stub (Basic Auth + dialect paths + answers-schema body asserted).
- Deleted with rationale (10): the five legacy route rows (superseded by the new adapter rows), WorkspaceNotActive/NotFound (transport contracts; adapter failures surface 502), BodyForwardedVerbatim, DialectNil, fetchFromPod-LimitReader.
- contract_auth: question/permission rows flip to guarded; the zero-record aggregate is now the expected steady state (guarded against future raw-surface reintroduction).

---

## Key Decisions

1. **Two new adapter methods instead of overloading `Resolve`** — the question endpoint's additionalProperties:false schema and the permission message field both demand schema-faithful posts; `Resolve` stays the no-message permission/kind-agnostic seam its existing consumers rely on.
2. **REST keeps the normalized envelope** (not raw contract InputRequest) — that IS the #1302 "contract InputRequest" ask at the REST surface: `agent.QuestionRequest` is the agent-agnostic normalized shape; the envelope is what the frontend types declare.
3. Dialect path methods stay on the opencode Dialect — the adapter consumes them internally (containment); only handler-side dialect usage died.

---

## Assumptions (Rule 7 — stated and validated)

- A1: the frontend's `{answers}` / `{reply, message?}` bodies are the exact pod schemas (validated: input.ts; the legacy proxy forwarded them verbatim; the e2e pod stubs pin them).
- A2: `emitInboxOnlyRecords`/`recordInboxAskFromSession` behavior is unchanged (validated: inbox suites green).
- A3: `TestAutoApprovePermission_NilAdapter_NoPodHTTP` passes pre-migration too (the legacy tail no-ops in that fixture — shouldAutoApprove config fetch short-circuits); kept as a post-state pin, honestly noted here rather than claimed red-first.
- A4: kind-e2e rows for the inbox flows (e2e-nightly) are not runnable locally; PR CI covers unit/integration, nightly covers the cluster legs.

---

## Blockers

None.

---

## Tests Run

- `go test ./api/... ./pkg/agent/...` — green; `go vet` + `gofmt` clean
- New rows red-first: 13 rows failed against the legacy tails before migration (the auto-approve row noted in A3).

---

## Next Steps

1. Batch 4: session-index/parents (fetchAndPersistTitle, runParentBackfill, fetchSessionParent).
2. Final batch: adapter as required ctor param; delete `proxyToWorkspace*`/`doProxy` + both test seams + suites; repolint zero-site gate; 5xx-counter decision at the adapter seam.
3. 4a (#1302): reply-vocabulary edge + OpenAPI/SDK regen + frontend extractor cleanup on top of this batch's REST contract.

---

## Files Modified

- pkg/agent/adapter.go (AnswerQuestion + ReplyPermission on the interface)
- pkg/agent/opencode/adapter.go (implementations)
- api/internal/handlers/proxy_input.go (5 routes migrated; tails + helpers deleted; translation seam extracted)
- api/internal/handlers/proxy_permissions.go (legacy tail deleted)
- api/internal/handlers/mock_adapter_test.go (mock fns)
- pkg/agent/adapter_test.go, pkg/agent/systemnotices/systemnotices_test.go (fakes)
- api/internal/handlers/proxy_batch3_migration_test.go (new — 13 rows)
- api/internal/handlers/proxy_input_test.go, adapter_path_test.go, proxy_inbox_test.go, proxy_question_e2e_test.go, contract_auth_test.go (ports/deletions with rationale)

---

## Review r1 remediation (PR #1357)

- **Transport guards re-homed on all five routes** (r1's silent-regression finding): `resolveWorkspaceForAdapter` + connection-slot accounting — workspace-404 / not-Active-503-with-Retry-After / conn-ceiling-429 all enforced again, pinned by three new rows (NotActive-503, NotFound-404, Ceiling-429). Metering/quota initially scoped out on a WRONG premise ("the transport metered chat writes only") — the transport metered every 2xx unconditionally on these routes; r2 re-homed `checkAdapterQuota` + `postAdapterSuccess` (llm_request metering; session-index skipped for the empty sessionID) on all three WRITE routes, and deliberately keeps the LIST polls unmetered (the transport's poll metering was an over-count).
- **#1302 mandate honored**: ListPending failures on the list routes → 503 non-authoritative (was 502), pinned by a dedicated row; write-route failures stay 502 (the agent's definitive rejection), pinned.
- **SuspendedWorkspace e2e repaired**: real adapter + suspended CRD — the 503 now comes from the re-homed resolve guard (Retry-After asserted), no longer the vacuous green.
- **Adapter unit rows added** (pkg/agent/opencode): AnswerQuestion (verbatim {answers} schema JSONEq, no cross-kind fallthrough, 5xx error) and ReplyPermission ×3 (message rides body, empty-message omitted, 5xx error).
- **PermissionReply real-adapter wire row**: Basic Auth + dialect path + both body fields verbatim to the pod.
- **#1302 S1 note for the epic**: `AnswerQuestion`/`ReplyPermission` encode the write-to-opencode path that the S1 amendment (replies through agentd Act) will replace — they are transitional surface, to be retired at the Act migration, not calcified. Recorded in the claim.
- Commit-label nit accepted: the dead-code removal commit should have been `refactor:` (history not rewritten; squash-merge flattens).

---

## Review r2 remediation (PR #1357)

- **Metering/quota corrected** (see the r1 correction above): writes gated + metered like SendMessage (quota-429 row pins the gate); polls stay unmetered as a deliberate over-count fix.
- **Retry-After** added to both list-failure 503s (consistent retry semantics within a route), pinned.
- **Coverage completed**: mislabeled 503 row fixed (it hit /permission) + the /question twin added; QuestionReject + PermissionReply 502 rows; guard-wiring rows for QuestionReject (NotActive), ListPermissions (NotFound), PermissionReply (Ceiling-429) — all with adapter mocks that t.Fatal if consulted.
- **Contract suite un-vacuumed**: the REAL adapter wired against the auth-recording backend — the question/permission lists reach the pod through the full live stack and the router-level BasicAuth aggregate asserts again (records > 0 required).
- **Epic-25 G1 re-pinned at the adapter seam**: `TestReadBody_TruncatesAtLimit` (the 1 MiB chokepoint ListPending uses).
- **Documented deltas vs the deleted transport** (recorded here per r2): body cap 10MB/413 → 1MB/400 on the reply routes; reply responses are the synthesized `{"status":"answered"}` envelope, not the pod body verbatim (frontend ignores reply bodies); a mid-turn pod-404 now surfaces 502 rather than the pod's raw 404 (#1302's absence-means-resolution reconciliation folds into the S1/Act migration).
- **Batch-4 residue noted for the next session**: the snapshot flights still gate on `h.dialect != nil` in stream_user_events.go:302 + proxy_stream.go:96 though emitPendingInputRequests no longer touches the dialect.
