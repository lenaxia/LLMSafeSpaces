# 0059 — Trigger input mapping: fired runs satisfy the workflow's inputSchema

**Status:** Proposed (2026-09-17) — design stage, holds for review; implementation lands in follow-up PRs (this document closes nothing)
**Date:** 2026-09-17
**Issues:** #1425 (trigger-fired runs bypass inputSchema) · #1419 (webhook runs receive the fire envelope; payload buried under `body`)
**Depends on:** #1413 (manual-run `ValidateRunInput` at the write boundary — the validator this design reuses), #1412 (failed-fire audit + auto-disable accounting pattern this design extends), #1426 (trigger-update scoping — the update path this design plugs into)
**Composes with:** Epic 64 (triggers & workflows data model — migrations 000016/000020/000021; the `validation_error` fire status already in the `trigger_fires` CHECK)
**Supersedes:** nothing merged. The deliberate-bypass comment at `api/internal/workflows/engine.go:540-549` becomes obsolete and is removed by the implementation PR.

---

## 1. Problem

### 1.1 What a fired run's input actually is (code truth, branch base @ `826836c3`)

| Path | Input the run gets | Where it is set | Validated against `workflows.input_schema`? |
|---|---|---|---|
| Manual run (`POST /workflows/:id/runs`) | caller's `input` document | `api/internal/handlers/workflows.go:631-636` (`Input: req.Input`) | **Yes** — `wf.ValidateRunInput(wfRow.InputSchema, req.Input)` 400s before queueing (`workflows.go:611-617`, the #1413 fix; validator at `pkg/workflows/input_schema.go:36`) |
| Cron-fired DAG run | system envelope `{source:{type:"cron",id}, received_at}` | envelope built `api/internal/workflows/engine.go:494-498`; `run.Input = envelopeJSON` via `inputForRun` at `engine.go:550` + `engine.go:567` | **No** — deliberately bypassed, documented at `engine.go:540-549` |
| Webhook-fired DAG run | system envelope `{source:{type:"webhook",id}, received_at, headers, body}` | envelope built `api/internal/handlers/webhook_receiver.go:200-211`; `run.Input = envelopeJSON` at `webhook_receiver.go:239` | **No** — same bypass class, no comment on this path (#1419) |
| Routine fire (no `workflow_id`) | envelope, always | `engine.go:584-603`; prompt template `{{.input}}` substituted with the envelope at `engine.go:670` | n/a (no DAG) |

Execution truth: the **first node's input is the run input verbatim** (`engine.go:287` `currentInput := run.Input`) and each node's output feeds the next node (`engine.go:305-307`). There is no per-node input mapping in v1 — so whatever the fire path stores in `workflow_runs.input` is exactly what the DAG's first condition expression / script handler / agent prompt sees.

### 1.2 Live consequences (concrete, from the issues)

**#1425 (cron/DAG):** a workflow authored with `inputSchema {type:"object", required:["topic"], properties:{topic:{type:"string"}}}` and wired to the DAG triggers #1412 made creatable fires a run whose input has **no `topic`** — ever. The run queues, the workspace activates, nodes execute, and the workflow fails *late* with `node_failed`-class errors (a condition like `input.topic == ""` misbranches; a script handler reads a missing key; an agent prompt renders empty). Manual runs of the same workflow 400 in milliseconds (`workflows.go:614-617`). Cost: burned node executions + workspace activations per fire, and an audit trail (`trigger_fires`) that says `fired` while the run rots — the exact waste the bypass comment at `engine.go:545-548` predicts.

**#1419 (webhook, observed live 2026-09-17):** the webhook-fired run's input is the fire envelope, so the sender's JSON payload sits under `input.body.*` and nothing the sender sent is at the top level. A workflow authored around its declared schema (`{topic, urgency}`) sees `input.topic` undefined; every condition must reach into `input.body.*` and every prompt into the body subtree — schema-driven and body-driven authoring diverge, and the path performs **zero** schema validation, so a malformed payload surfaces as the same late node failures as #1425.

The bypass is *correct about the mechanism and wrong about the input*: validating the envelope against a user schema can never pass — the fix is to make the fire path capable of producing a schema-satisfying input, then validate it.

---

## 2. Option space and decision

The four options sketched in #1425 (plus #1419's `inputFrom` direction):

| # | Option | Assessment |
|---|---|---|
| O1 | **Reject DAG-trigger wiring at create** when the workflow's inputSchema has required properties beyond the envelope shape | Cheapest correctness guard — but *only* a guard: it makes schema-bearing workflows un-triggerable instead of triggerable-with-input, and says nothing about webhooks (where the payload could satisfy the schema if it weren't buried). Too strict alone, valuable narrowed (D4). |
| O2 | **Per-trigger static input document** (validated against the schema at trigger create) **+ `inputFrom` mapping (`body` \| `envelope` \| `mapped`)** so webhook payloads can become the run input directly | Fixes both issues at their cause: cron DAGs get a stable schema-satisfying input; webhooks get the payload at the top level; fire-time validation becomes *possible* rather than always-failing. **RECOMMENDED.** |
| O3 | **Envelope-rendering templates** on the trigger (e.g. `{"topic": "{{.body.issue.title}}"}`) | Maximal power, maximal cost: a second template language to validate, escape, version, and debug (injection-prone by construction — template output becomes run input). Dominated by O2's `body` mode for the common case; revisit only if a real need for cross-field mapping emerges. |
| O4 | **Document + enforce "trigger-targetable workflows must not require non-envelope properties"** at workflow create | Pushes the constraint onto the *workflow* author (who must not use the schema feature the platform advertises) instead of the *trigger* wirer. Backwards-invisible (existing workflows unaffected) but forecloses `inputSchema` for every triggered workflow — rejected as the primary mechanism; its *documentation duty* is folded into D4's error text and the MCP descriptions (D7). |

**Decision — this design composes O2 (the mechanism) with O1-narrowed (a create-time wiring guard for new wiring only) and O4's documentation folded in:**

- **D1:** triggers gain `inputFrom ∈ {envelope, body, mapped}`, default `envelope` — existing triggers keep byte-identical behavior (§3.6).
- **D2:** triggers gain a static `input` document (JSON, ≤ 64 KiB), stored per trigger; in `envelope`/`body` modes it is a shallow top-level **overlay** on the base (static wins), in `mapped` mode it **is** the run input. Validated against the workflow's schema at trigger create/update when the resolved input is fully known (§3.3).
- **D3:** at fire time, **opted-in** triggers (`inputFrom ≠ envelope`, or static `input` present) get the resolved input validated with the existing `wf.ValidateRunInput` **before** `CreateWorkflowRunWithFire`; a mismatch records a `validation_error` failed fire (status already in the `trigger_fires` CHECK, `api/migrations/000016_triggers_workflows.up.sql:338`; constant at `pkg/types/workflows.go:108`) with **typed, location-only violations — never instance values** and a 4 KiB cap (§3.5, the constraint established in the #1419 thread), increments `consecutive_failures`, and honors auto-disable — the #1412 pattern (`engine.go:526-535`) reused verbatim. No run is queued; zero node executions burned. A stored schema that no longer *compiles* is a **workflow defect, not an input defect** (`pkg/workflows/input_schema.go:41-45`) and records a `failed` fire with `{"code":"invalid_input_schema"}` instead.
- **D4 (narrowed O1):** trigger create/update **rejects with 400** a *new* envelope-mode no-static wiring when the target workflow's schema requires properties outside the source's envelope key set, with a remediation message naming the three fixes (set `input`, `inputFrom: "body"`, or relax the schema). Existing triggers are never re-scanned — on update the guard fires only when the patch itself changes `workflowId`/`input`/`inputFrom` (V7, §3.6).
- **D5:** `inputFrom: "body"` is webhook-source-only (cron envelopes have no `body` key — `engine.go:494-497`); the body is the **parsed JSON document verbatim** (any JSON value — the `{"raw": …}` non-JSON fallback at `webhook_receiver.go:194-198` is documented behavior, not silently coerced; the implementation aligns that fallback to the epic's `{raw, content_type}` shape so a validation failure on a form-encoded delivery is explainable in the fire audit — the #1419-thread alignment).
- **D6:** one resolver, two fire paths: input resolution + validation live in `pkg/workflows` beside the validator (`input_schema.go`) and are called from **both** `Scheduler.fireWorkflowTarget` and the webhook receiver — today the two paths duplicate the envelope→`run.Input` wiring (`engine.go:550,567`; `webhook_receiver.go:239`); after this design they share it.
- **D7:** all contract surfaces are updated in the same PR: API DTOs (`pkg/types/workflows.go`), `sdks/openapi.yaml`, `docs/api/mcp.md`, the agentd MCP tool descriptions (`cmd/workspace-agentd/mcp_server.go`), and the external-MCP tool pins (`pkg/mcp/workflow_tools.go`) — including correcting `workflow_create`'s now-inaccurate "inputSchema … is enforced on every workflow_run input" claim (`mcp_server.go:254`).

---

## 3. Design (recommended option, elaborated)

### 3.1 Data model — triggers table columns

Migration `000031_trigger_input_mapping.{up,down}.sql` (next free number; mirrored in `api/migrations/` and `helm/migrations/` as 000016/000020/000021 are):

```sql
-- up
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS input jsonb;              -- static input document (D2)
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS input_from text NOT NULL DEFAULT 'envelope';
ALTER TABLE triggers DROP CONSTRAINT IF EXISTS triggers_input_from_check;
ALTER TABLE triggers ADD CONSTRAINT triggers_input_from_check
    CHECK (input_from IN ('envelope', 'body', 'mapped'));
-- down: drop constraint, drop both columns
```

- `input_from text NOT NULL DEFAULT 'envelope'` — the DEFAULT *is* the backfill: every existing row reads as envelope mode with no UPDATE pass (§3.6).
- `input jsonb NULL` — absent = no overlay. Nullable so the partial-update contract ("nil keeps existing", `pkg/workflows/store.go:180-182`) can distinguish *unset* from *cleared*: an absent `input` key in the update body keeps the stored document; an explicit JSON `null` clears the column (handler-level discrimination — `json.RawMessage` is nil when the key is absent and carries the bytes `null` when present — before the store call; the store itself keeps the `CASE WHEN $n IS NULL THEN keep` shape `source_config` uses, `store.go:414`).
- No DB-level coupling to `workflow_id` (a CHECK "input IS NOT NULL ⇒ workflow_id IS NOT NULL" would break future routine-input use); enforced at the API layer (§3.3 rule V1).

Row/DTO plumbing: `TriggerRow` (`pkg/workflows/store.go:73-99`), `triggerSelectColumns` (`store.go:336`), `CreateTrigger` (`store.go:341-359`), `UpdateTrigger` COALESCE chain (`store.go:403-453`), and `TriggerUpdate` (`store.go:183-203`) each gain the two fields; the update SQL uses the same `CASE WHEN $n IS NULL THEN keep ELSE set` shape as `source_config` (`store.go:414`) for `input_from`, and jsonb-IS-NOT-NULL discrimination for `input`.

### 3.2 API DTOs (`pkg/types/workflows.go`)

JSON casing follows the existing camelCase DTO convention (`workflowId`, `sourceConfig` — `types/workflows.go:360-382`):

```go
// CreateTriggerRequest (workflow mode fields only shown):
type CreateTriggerRequest struct {
    // ... existing fields (types/workflows.go:360-382) ...
    InputFrom string          `json:"inputFrom,omitempty"` // envelope (default) | body | mapped
    Input     json.RawMessage `json:"input,omitempty"`     // static input document (DAG mode)
}

// UpdateTriggerRequest:
type UpdateTriggerRequest struct {
    // ... existing pointer fields (types/workflows.go:386-403) ...
    InputFrom *string         `json:"inputFrom,omitempty"` // nil = keep
    Input     json.RawMessage `json:"input,omitempty"`      // present (incl. JSON null) = replace; absent = keep
}

// TriggerResponse gains:
    InputFrom string          `json:"inputFrom"`
    Input     json.RawMessage `json:"input,omitempty"`
```

Behavioral defaults mirror the neighboring enum fields (`memoryMode` etc., `api/internal/handlers/triggers.go:161-186`): empty `inputFrom` on create → `envelope`; invalid value → 400 via a new `types.ValidTriggerInputFrom` (sibling of `ValidMemoryMode`, `pkg/types/workflows.go:187-193`). A present `input` must parse as a JSON document and is size-capped at 64 KiB (constant; webhook bodies are separately capped at 1 MiB, `webhook_receiver.go:70-74` — the static doc is authored config, not transport).

### 3.3 Validation rules at trigger create/update

Runs in `TriggersHandler.create`/`update` (`api/internal/handlers/triggers.go:145-321`, `:337-431`) after the existing field checks, and therefore also covers the pod-automation delegation (`api/internal/handlers/pod_automation.go:343-360`, `:375-389`) which forwards through these handlers. Rules, in order:

| # | Rule | Failure |
|---|---|---|
| V1 | `input`/`inputFrom` (beyond default) require `workflowId` (DAG mode). Routine triggers (`workspaceId` mode) keep the envelope-fed `{{.input}}` prompt contract (`engine.go:670`). | 400 `input mapping requires a workflow target` |
| V2 | `inputFrom: "body"` requires `sourceType: "webhook"` (D5 — cron envelopes carry no `body`). | 400 |
| V3 | When `workflowId` is set **and** an opt-in is present (`input` non-null or `inputFrom ≠ envelope`): fetch the workflow (owner-scoped `GetWorkflow`, as the pod path already does in `gateWorkflowTarget`, `pod_automation.go:318-336`); missing workflow → cannot validate. | 400 `target workflow not found` (create-time; distinct from #1412's fire-time ghost, see §3.6) |
| V4 | `inputFrom: "mapped"`: `wf.ValidateRunInput(workflow.InputSchema, input)` must pass (`pkg/workflows/input_schema.go:36`; absent schema accepts everything — the #1413 semantics). | 400 with the validator's message |
| V5 | `envelope`/`body` + static `input`: `input` must be a JSON **object** (it overlays an object base — §3.4). Full merged validation is fire-time (base content unknowable at create). | 400 `static input must be a JSON object in envelope/body modes` |
| V6 | **Wiring guard (D4):** `workflowId` set, no opt-in (default `envelope`, no `input`), workflow fetched OK, and `workflow.InputSchema`'s `required` names any property outside the source's envelope key set (`{source, received_at}` cron / `{source, received_at, headers, body}` webhook — the shapes at `engine.go:494-497` and `webhook_receiver.go:205-210`). | 400 naming the violation and the three remedies (D4) |
| V7 | Update runs **only the rules whose inputs the patch touches**. When the patch sets or clears any of `workflowId`, `input`, `inputFrom`, the post-patch trigger (patch merged into the stored row first — the update path already re-reads it for cron rescheduling, `triggers.go:383-392`) is validated under V1–V6; V6 applies when the *post-patch* trigger is un-opted — including patches that **remove** an opt-in (clearing `input` to null, switching `inputFrom` back to `envelope`) re-expose the wiring and re-trip the guard. A patch touching none of the three fields (rename, enable flip, schedule change) re-runs **none** of V1–V6. `sourceType` is immutable (`store.go:181-182`), so V2 always checks the stored source type. | as above |

`required`-predicate scope: V6 inspects only top-level `required` (syntactic, no deep schema walk). A schema requiring `body` on a cron trigger is caught; one requiring `properties.body.topic` is not — for **opted-in** triggers, fire-time validation (D3) is the backstop for everything the predicate cannot see. Deliberate: the guard exists to make the *common* mis-wiring loud at create, not to re-implement a schema analyzer.

Workflow-side changes (a later `workflow_update` tightening `inputSchema`) do **not** re-scan wired triggers — the trigger was valid when wired. Drift on an **opted-in** trigger is caught at the next fire as a `validation_error` (D3), the same steady-state the manual-run path has (a tightened schema only bites the next run). An **un-opted** trigger (legacy-grandfathered, or a wiring that passed V6 because the schema then required nothing beyond the envelope) keeps today's no-validation behavior (§3.6): workflow-side drift on it remains the documented #1425 waste class — silent until the author opts in or the next wiring/mapping edit trips V6.

### 3.4 Fire-time input resolution — one resolver, both paths

New `pkg/workflows.ResolveTriggerInput(trigger InputSpec, envelope json.RawMessage) (json.RawMessage, error)` (sibling of the validator in `input_schema.go`; consumed by both `engine.go:507-582` and `webhook_receiver.go:218-264`):

```
base := switch trigger.InputFrom {
  case "body":    envelope.body            // webhook-only (V2); parsed JSON document, any value (D5)
  case "mapped":  nil                      // the static document IS the input
  default:        envelope                 // 'envelope' — compat
}
if trigger.Input == nil  -> runInput = base                       // byte-path identical to today
if base is an object     -> runInput = shallow-merge(base, Input) // top-level keys, static wins
else (body non-object)   -> runInput = Input verbatim             // overlay undefined; static is the input
```

Shallow merge is pinned deliberately: deep-merge semantics are unspecified territory (array concat? null-delete?) and the first 90% use case is "provide `topic` (and friends) at the top level". The envelope stays reachable under its keys in `envelope` mode (provenance preserved); `mapped` mode exists precisely for schemas with `additionalProperties: false`, which a merged envelope would violate.

Then, **only when opted in** (D3's scope — see §3.6 for why not-always): `wf.ValidateRunInput(wfRow.InputSchema, runInput)`; on error → §3.5; on success (or no opt-in) → the unchanged `CreateWorkflowRunWithFire` flow with `run.Input = runInput` (`engine.go:565-571`; `webhook_receiver.go:237-245`). `trigger_fires.input_envelope` keeps recording the **raw envelope** in every mode (the audit doc — "what the source sent" — not "what the DAG consumed"; the run row already persists the resolved input).

### 3.5 Failed fires on schema mismatch — the #1412 pattern, reused; sanitized payloads

On a D3 validation failure, both fire paths record a fire structurally identical to the missing-workflow failed fire at `engine.go:526-535` (status `failed` there, `validation_error` here — both already in the `trigger_fires` CHECK, migration 000016:338; constants `pkg/types/workflows.go:107-108`):

```json
// trigger_fires.action_result — input mismatch (validation_error fire)
{
  "code": "schema_mismatch",
  "inputFrom": "body",
  "violations": [
    {"jsonPointer": "/topic", "keyword": "required",
     "message": "missing property \"topic\""}
  ]
}
```

**Payload contract (the constraint established in the #1419 thread — security-load-bearing):** violations are **typed `{jsonPointer, keyword, message}` records, locations only, never instance values**, and the marshaled payload is hard-capped at **4 KiB**. The implementation must derive them from the jsonschema v6 error's *typed* trail (instance locations + keyword), **not** from `ValidationError.Error()` — the library's rendered messages provably embed instance-derived content (`pattern` failures quote the instance string, `format` failures render the received value, `additionalProperties` failures list instance property names), and under `inputFrom: "body"` the instance is the external sender's payload. `action_result` is persisted and rendered by the UI and the `trigger_fires` MCP tool (`TriggerFireResponse`, `pkg/types/workflows.go:452-463`) — a raw message would give an attacker-influenced body a durable, displayed reflection surface. Keyword-derived static messages (e.g. `missing property "topic"` — a *name* from the schema's own `required` list, not from the instance) are safe; `message` is generated from `(keyword, schemaLocation)` only. **Cap semantics:** violation records are appended only while the marshaled payload stays under 4 KiB; the remainder is summarized by a final `{"keyword": "truncated", "message": "N more violations omitted"}` marker. Because a single deep `jsonPointer` from an attacker-controlled 1 MiB body can itself approach the cap, the cap is enforced recursively: a record that does not fit first drops its `message`, then the record itself (counted in the marker); a pointer that still cannot fit yields the bare `{"code","inputFrom"}` skeleton plus marker — the payload size is an invariant, never a best-effort. The manual-run 400 path (`handlers/workflows.go:614-616`) may keep echoing the richer validator message: that is a same-request echo of the caller's own input, not a persisted audit field. Both failure classes — input mismatch (`validation_error`) and non-compiling stored schema (`failed`, `{"code":"invalid_input_schema"}`) — count toward auto-disable, uniform with the missing-workflow precedent (`engine.go:533-535`).

The run is **not** created: `uq_workflow_run_single_inflight` is never contended, no workspace activates, no node executes — the #1425 waste class is eliminated, not relocated. The fire is followed by the #1412 accounting on both paths: `IncrementTriggerFailures`, `DisableTrigger` at `auto_disable_after` (`engine.go:533-535`). `code: "schema_mismatch"` reuses the vocabulary of `RunErrorCodeSchemaMismatch` (`pkg/types/workflows.go:129`) without creating a run row to hang it on. The webhook receiver answers the sender **202 `{"status":"fired"}`** as today (delivery succeeded; the workflow's input contract failed — surfaced via `trigger_fires`, the same place `already_running` skips surface, `webhook_receiver.go:248-256`; a non-2xx would make GitHub-style senders retry a permanently invalid payload into a storm, the #1419-thread rationale).

### 3.6 Compatibility — existing triggers change nothing observable

| Existing state | Read as | Behavior |
|---|---|---|
| Any trigger created before this design | `input_from = 'envelope'` (column DEFAULT), `input = NULL` | Resolver returns the envelope bytes; **no** opt-in ⇒ **no fire-time validation** (D3 scope) ⇒ the fire path is byte-for-byte today's. |
| Ghost-workflow DAG triggers (#1412's R4 fixture) | envelope mode, no opt-in | V6 needs the workflow fetched to apply; a missing workflow ⇒ guard skipped (nothing to require) ⇒ still creatable via the user API, still loud at fire time (`engine.go:523-537`). R4 stays green unchanged. |
| Schema-bearing workflow + existing envelope trigger | envelope mode, no opt-in | Untouched — no re-scan (V7 fires only when a patch changes `workflowId`/`input`/`inputFrom`). The author opts in (or hits V6) on the next *wiring-or-mapping* edit; a rename or schedule tweak never trips the guard. |

Why D3 validates **only opted-in** triggers: the envelope can *never* satisfy a schema with required non-envelope properties — that is the documented premise of the bypass (`engine.go:541-544`). Validating unconditionally would convert every legacy schema-bearing DAG trigger from "runs with garbage input" to "hard-fails every fire and auto-disables within N ticks" — a fleet-wide brick-by-default. Opt-in scope makes validation a promise the trigger author made (`input`/`inputFrom`) rather than one imposed retroactively; V6 ensures *new* un-opted wiring cannot be created silently. Rollout can therefore be a single additive deploy (§4).

### 3.7 MCP tool descriptions and contract surfaces (D7)

- **agentd `trigger_create`** (`cmd/workspace-agentd/mcp_server.go:212-217`): extend the description — *"`input` (object) is the static run input for workflow-mode triggers, validated against the workflow's inputSchema when `inputFrom` is `mapped`; `inputFrom` selects what a fired run's input is: `envelope` (default — the system envelope `{source, received_at, headers, body}`), `body` (webhook only — the posted payload becomes the run input), or `mapped` (the static `input` document). Wiring an envelope-mode trigger to a workflow whose schema requires non-envelope fields is rejected with 400 — set `input`, use `inputFrom: "body"`, or relax the schema."* The tool body passes through verbatim (`mcp_tools.go:733-734` `passThroughBody`), so no plumbing change — description only.
- **agentd `trigger_update`** (`mcp_server.go:219-225`): note `inputFrom`/`input` are patchable.
- **agentd `workflow_create`** (`mcp_server.go:253-258`): correct *"inputSchema (JSON Schema) is enforced on every workflow_run input"* to *"enforced on manual runs and on fired runs that carry input mapping (`input`/`inputFrom`)"* — the current claim is the exact misconception #1419 disproved.
- **agentd `trigger_fires`** (`mcp_server.go:234-239`): mention `validation_error` status + its `actionResult` payload as the schema-mismatch trail.
- **External MCP pins** (`pkg/mcp/workflow_tools.go:68-85`): add `input_from`/`input` params to `triggerCreateTool`/`triggerUpdateTool` descriptions (these tools are the flat-arg legacy surface — extend descriptions, keep passthrough).
- **`sdks/openapi.yaml`** (`/me/triggers` at 3882, `/orgs/{id}/triggers` at 4243) and **`docs/api/mcp.md`** (trigger_create row at 163): document both fields and the V6 400.

---

## 4. Migration & rollout

1. **Single additive deploy:** migration 000031 (§3.1) + API + resolver + MCP descriptions ship together; `input_from` defaults make the fleet read as envelope mode with zero backfill. No feature flag — the default path is provably inert (§3.6) and the new paths are opt-in per trigger.
2. **Order within the deploy:** migration first (columns exist before the new SELECT/INSERT column lists run — same ordering every prior trigger migration used, e.g. 000020 before the `TriggerRow` fields it backs).
3. **Rollback:** deploy the down migration; the API reverts to ignoring the columns. Rows created with opt-ins during the window revert to envelope behavior (their `input` is simply unread) — acceptable and documented.
4. **Observability:** no new metrics; `trigger_fires.status = 'validation_error'` is the rollout signal (grep-able in the audit surface the `trigger_fires` MCP tool already exposes). A non-zero rate after deploy = authors opting in and mis-configured senders being caught — triage via `actionResult`.
5. **Comms:** the V6 400 lands only on *new* wiring; the release notes name the three remedies once (they are also the 400 body).

---

## 5. Test strategy

| Suite | Proves | Type |
|---|---|---|
| `TestResolveTriggerInput` table: modes × static-present × body-kind (object/array/string/`{"raw":…}`) × nil-static | D1/D2 resolution matrix incl. byte-identical envelope default and overlay-wins merge | unit (`pkg/workflows`) |
| `TestValidTriggerInputFrom`, create/update handler matrix V1–V7 (opt-in shapes, cron+`body` reject, ghost-workflow skip for un-opted create, guard hit/miss on `required` ⊆ envelope keys; V7 scope: rename-only patch never trips V6, a patch clearing an existing opt-in does) | §3.3 rules incl. the V7 touch-scope | unit (`api/internal/handlers`) |
| `TestSanitizeSchemaViolations`: build failing instances whose *values* would appear in raw jsonschema messages (pattern-mismatched string, format-invalid value, extra property); assert the persisted `action_result` contains **no instance-derived substrings** (typed `{jsonPointer, keyword, message}` only) and respects the 4 KiB cap (truncation marker present, pointers retained) | §3.5 payload contract — the #1419 no-instance-values rule | unit (`pkg/workflows`) |
| Scheduler fire: schema-bearing workflow + `mapped` trigger with conforming `input` → run queued with resolved input; divergent `input` → **no** run, one `validation_error` fire, `consecutive_failures` incremented, auto-disable at threshold | D3 + §3.5 on the cron path (extends the `mockSchedulerStore` harness, `api/internal/workflows/engine_test.go:527`) | unit |
| Webhook receiver: signed delivery → `inputFrom: body` run input == posted document; violating body → 202 + `validation_error` fire, no run; `envelope`+static → merged input | D3 + §3.5 on the webhook path (extends `webhook_e2e_test.go`'s harness, `api/internal/handlers/webhook_e2e_test.go:150`) | unit + integration |
| Store roundtrip: create/update/select with `input`/`input_from`; update's keep-vs-replace discrimination (absent vs JSON-null) | §3.1 plumbing | integration (`store_integration_test.go`) |
| Migration up/down: columns + CHECK + default; down restores shape | §3.1 | integration (`api/migrations/test`) |
| **R6** in `local/issue-1410-1412-automation-e2e.sh` (after R5, `local/issue-1410-1412-automation-e2e.sh:157-184`; reuses its `api()` helper and the R5 schema workflow): **R6a** wiring guard — cron trigger, no `input`, against the R5 required-`topic` workflow → 400; **R6b** `mapped` validation — `inputFrom:"mapped"`, `input:{}` → 400; `input:{topic:"nightly"}` → 201; **R6c** webhook body mode — webhook trigger on the R5 workflow with `inputFrom:"body"`, rotate-secret (`/rotate-secret`, `sdks/openapi.yaml:4029`), signed POST `{"topic":"e2e"}` → 202 and a queued run whose input has top-level `topic`; signed POST `{"wrong":true}` → 202 and a `validation_error` fire carrying `schema_mismatch` with typed violations only (no `wrong`-value echo in `actionResult`); no pod/LLM needed (runs queue against the dummy workspace exactly as R5's conforming-input row does) | End-to-end contract incl. HMAC path | e2e (pool/nightly) |

R1–R5 stay green unmodified (§3.6 — R4's ghost-workflow fixture is deliberately untouched by V6).

---

## 6. Assumptions (validated — citations on the branch base)

| # | Assumption | Validation |
|---|---|---|
| A1 | Fired-run inputs bypass schema validation on both fire paths, deliberately (cron) and silently (webhook) | `engine.go:540-550`; `webhook_receiver.go:237-245` |
| A2 | The first node consumes `workflow_runs.input` verbatim; no per-node input mapping exists in v1 | `engine.go:287`, `engine.go:299-307` |
| A3 | A schema-aware validator with the right semantics already exists and is workflow-owned (not handler-owned) | `pkg/workflows/input_schema.go:36-57` (`ValidateRunInput`); manual-run precedent `handlers/workflows.go:611-617` |
| A4 | `validation_error` is already a legal `trigger_fires.status`; no enum migration needed | `api/migrations/000016_triggers_workflows.up.sql:338`; `pkg/types/workflows.go:108` |
| A5 | The failed-fire + auto-disable accounting pattern is established and e2e-pinned | `engine.go:526-535`; R4 in `local/issue-1410-1412-automation-e2e.sh:128-155` |
| A6 | Trigger create on the pod path already fetches the target workflow (scope gate); the user-API path does not fetch at all | `pod_automation.go:343-360` + `:318-336` vs. `triggers.go:145-321` (no `GetWorkflow` call) |
| A7 | `TriggerRow`/`TriggerUpdate`/DTO/store plumbing follow one repeating extension pattern (most recently the 000020 routine fields) | `store.go:73-99`, `:183-203`, `:341-359`; `api/migrations/000020_trigger_routine_fields.up.sql` |
| A8 | Webhook bodies are parsed as JSON with a `{"raw": …}` non-JSON fallback | `webhook_receiver.go:193-198` |
| A9 | The next migration number is 000031; api/ and helm/ migration trees mirror | last entries `000030_key_rewrap_retention` in both |

---

## 7. Rejected alternatives

| Alternative | Rejected because |
|---|---|
| **O1 alone — reject all schema-bearing DAG wiring at create** | Makes the #1412 feature strictly less useful (schema-bearing workflows become un-triggerable) and does nothing for #1419 (webhook payloads stay buried). Kept only as D4's narrowed guard for *un-opted new* wiring. |
| **O3 — envelope-rendering templates (`{"topic": "{{.body.issue.title}}"}`)** | A second template language to validate/escape/version; template output becomes run input (injection surface); the dominant need (payload at top level) is met by `inputFrom: "body"` with zero new syntax. Revisit only with a concrete cross-field-mapping requirement the static overlay cannot express. |
| **O4 alone — forbid non-envelope `required` on trigger-targetable workflows** | Blames the workflow author for the trigger wirer's choice; makes `inputSchema` (an advertised feature — `mcp_server.go:254`) unusable for every triggered workflow; unenforceable retroactively without a trigger backscan. Its doc duty survives inside D4's 400 text and D7's descriptions. |
| **Unconditional fire-time validation (all triggers, incl. legacy envelope mode)** | §3.6 — the envelope can never satisfy required non-envelope properties (the bypass's own premise, `engine.go:541-544`); unconditional validation bricks-and-auto-disables every legacy schema-bearing DAG trigger within N ticks of deploy. |
| **Deep-merge static overlay** | Unspecified semantics (arrays, nulls-as-deletes) with no demonstrated need; shallow merge covers "provide top-level fields" completely and is trivially testable. |
| **Recording the resolved input in `trigger_fires.input_envelope`** | The audit row is "what the source sent" (its name and the `TriggerFireResponse` contract say so, `pkg/types/workflows.go:452-457`); the resolved input already lives on the run row. Overloading the envelope column would break every existing consumer of the fire audit (e.g. routine `{{.input}}` debugging). |
| **A run row in `validation_error` state instead of a failed fire** | No node executed and no spec snapshot was consumed; a run row would contended the single-in-flight index (`uq_workflow_run_single_inflight`, migration 000016:276-278) and misrepresent the state machine. The fire audit is where trigger-side failures already live (#1412). |

---

## 8. Open questions (owner — non-blocking for the design, deciding for implementation)

1. **Auto-disable accounting for webhook `validation_error` fires.** D3 counts them toward `auto_disable_after` (uniform with #1412). A sender with a valid HMAC but a drifting payload can thereby disable the trigger — authenticated, but possibly *itself* rather than an attacker. Alternative: `validation_error` fires increment a *separate* visible counter that never trips auto-disable. This design says: count them (uniformity, and the remedy is on the sender side); flag if you want the split counter.
2. **Should the user/org API trigger-create also enforce workflow *existence*** (the pod path already does, `pod_automation.go:356`)? V3 fetches only when an opt-in is present, so un-opted ghost wiring remains creatable via the API precisely to keep #1412's fire-time loudness (and its R4 e2e row) intact. Confirm that asymmetry is intended, or accept the R4 fixture rework.
3. **`workflows.defaults` interplay.** The column exists (`workflows.defaults`, migration 000016:48) but nothing applies it to run inputs today (no reference in `engine.go`). Should the eventual defaults mechanism merge *under* the trigger's static input (workflow defaults → trigger `input` → base)? Proposed: out of scope here; noted so the two features don't collide silently later.
4. **Form-encoded / non-JSON webhook payloads.** D5 passes the `{"raw": "…"}` fallback through as the run input (a schema would need `properties.raw`). Should `inputFrom: "body"` eventually decode `application/x-www-form-urlencoded` into an object first? Deferred until a sender requires it.
5. **`mapped` + envelope provenance.** Authors of `additionalProperties: false` schemas lose all envelope context in `mapped` mode. Option: reserve a `_trigger` key injected by the resolver (schema authors opt in by declaring it). Rejected for v1 (implicit keys are surprise keys); revive if asked for.
6. **Larger static inputs.** 64 KiB cap is a first guess sized against authored-config-not-transport; say the word if trigger-carried inputs should approach webhook-body scale (1 MiB) instead.
