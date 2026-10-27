# 0070 — Board file-backed CRUD: bodyFile/patchFile/planFile, board_validate_file, auto-cleanup

**Status:** Approved & implemented (2026-10-26) — owner-settled in-session; the approved design document lives verbatim in the charter workspace (`/workspace/reports/board-file-crud-design.md`); this file is the repo-conventions rendering. Extends the #1837 file-param ruling (agent-surface only) and its read discipline (`cmd/workspace-agentd/board_file_params.go`).
**Date:** 2026-10-26
**Issues:** #1837 (the file-param ruling this extends) · #1832/#1849 (the emission-corruption and atomic-create classes that motivated it) · #1747 (the manager board surface) · #1948 (the terminal-target guards the attach arm mirrors)
**Affects:** `cmd/workspace-agentd/{board_file_params,board_manager_tools,board_payload_schema,mcp_server,mcp_tools}.go` + tests · `pkg/board/{rows,plan}.go` + tests · `api/internal/handlers/{board_automation_manager,board,board_parity_test}.go` · `sdks/openapi.yaml` · `runtimes/opencode/plugins/llmsafespaces-origin.js` · `CHANGELOG.md`

---

## 1. The problem

Three occurrences in one orchestrating session: board CRUD calls carry multi-KB **structured** JSON arguments, and model emission corrupts exactly that shape — duplicated argument fragments, stray file keys pasted from prior calls, parameter streams that never parse. #1837 moved single long *strings* to files; the remaining exposure is the structured payload itself.

**Principle:** a tool call should carry only short scalars and one path; all structure rides in a file written via the write tool (one large string — the shape that does not corrupt).

**Cutover principle:** the file path is not an alternative — it is the only path. Additive file params would leave the corruption class alive (models emit inline JSON because the schema advertises it); removing the inline payload properties from the tool schemas makes the unsafe shape *unexpressible*. The platform wire body is unchanged in shape (agentd assembles it from file content), so the cutover is agent-surface-only for the four CRUD tools; the one platform change (§5) serves the unified plan document.

## 2. The cutover (agent surface, `cmd/workspace-agentd`)

| Tool | File key (REQUIRED) | File content | Inline vocabulary |
|---|---|---|---|
| `board_create_epic` | `bodyFile` | `{title, outcome, tags, requestId?}` | `projectId` |
| `board_create_task` | `bodyFile` | the flat single-task doc `{title, description, tags, acceptanceCriteria, dependsOn, requestId?}` | `epicId` |
| `board_update_epic` | `patchFile` | replace-if-present patch `{title?, outcome?, tags?, requestId?}` | `epicId` |
| `board_update_task` | `patchFile` | replace-if-present patch `{title?, description?, tags?, acceptanceCriteria?, requestId?}` | `taskId`/`shortId`, `claimToken`, `setCriteriaMet` |
| `board_import_plan` | `planFile` | the unified plan document (§4) | `projectId`, `dryRun` |

- **Inline payload keys are refused** on the five tools (routing + control-plane + identity keys only); the payload file is **required** — absence is a loud refusal teaching the workflow (write → `board_validate_file` → submit). The five tools' **schemas drop the inline payload properties** entirely.
- The **#1837 single-field params (`descriptionFile`/`outcomeFile`) are deleted** — a body/patch file already carries long content. The read discipline and shared reader survive.
- Stray `*File`/`*_file` keys are refused on **every** manager tool; nested file keys inside payload files are refused (one indirection level); routing/control keys inside a file are refused (they are read from inline arguments — a file spelling would be silently unread).
- The one inline write that is not payload structure stays: the bare criteria check-off `{taskId, setCriteriaMet}` (#1946/#1837 P2b — small, index-addressed control-plane) needs no patch file.
- External MCP and REST stay file-free **and inline** (#1837 ruling; their surface is programmatic, not model-emitted).

## 3. Resolver, anchoring, cleanup

- `resolveBoardPayloadFile` reads under the #1837 discipline verbatim (`O_NONBLOCK`, regular-file gate, stat AND read caps — TOCTOU, UTF-8 validation), cap **256 KiB** (platform's largest text cap is 128 KiB `MaxDescriptionBytes`; the doubling is documented headroom for a whole structured payload), then **strict-decodes**: single JSON document, duplicate keys refused at every object depth, object top level.
- **Path anchoring fix**: relative file paths resolve against the **workspace root** (the PVC mount, `pkg/agentd.WorkspacePath`), never agentd's process cwd — the live failure (`reports/charters/p1a.md` → `open reports/...: no such file`). Absolute paths pass through. Applies to every file param.
- **Auto-cleanup**: a successful call deletes its consumed payload file. `board_import_plan`'s carve-out: a dry run mutates nothing — the file survives for the real run. A delete failure folds a `cleanup_warning` field into the succeeded result (never fails the call). A later call referencing a missing file gets the hint: *file not found — files are deleted after a successful CRUD call; if you already submitted it, re-write it; payloads are file-only (if this was a retry, check board_list / board_read to confirm the create landed before re-submitting)*. `board_validate_file` never deletes.

## 4. The unified plan document (§1a of the approved design)

`planFile` accepts ONE document shape covering any granularity:

```
{ "epic": {"title","outcome","tags"},  // new epic   — OR
  "epicId": "epc-…",                   // existing epic
  "tasks": [ {key, title, description, tags, acceptanceCriteria, dependsOn}, … ] }  // 1..N
```

Exactly one of `epic`|`epicId` (both/neither → validation finding naming the `exactly-one-of` rule). The whole plan is **all-or-nothing** in every mode; `dependsOn` addresses payload keys. A single task is a one-element plan or the flat `bodyFile`. The `epic` object rides the wire verbatim (the platform binds it); the legacy `epicTitle`/`outcome`/`tags` spellings stay wire-accepted for the REST/external surfaces.

## 5. The one platform change (epicId attach mode)

`ImportPlan` gains the existing-epic arm: **exactly-one-of** validation (both → 400, neither → 400, two create-spellings → 400 — `resolvePlanEpicArm`), ownership/workspace binding via the URL project, the epic-must-belong-to-that-project guard, and the **#1948 terminal-epic guard** mirrored from the create-task path (pre-write, and re-checked under the epic row lock inside the transaction). The transaction skips the epic-create step and attaches tasks under the resolved epic — a bad task in the array leaves the existing epic untouched (atomicity pinned). The seam handler (`BoardImportPlan`) and the REST handler bind the typed `board.PlanInput` (which gains `Epic *PlanEpicSpec` and `EpicID`); `sdks/openapi.yaml`'s `BoardPlanRequest` documents both arms.

## 6. The validator — strongly typed, one implementation, two entry points

**Engine:** `santhosh-tekuri/jsonschema/v6` (the `pkg/workflows/input_schema.go` precedent, including the `kind` violation package). Five compiled schemas — one per op — object-rooted, `additionalProperties:false` (unknown fields, nested file keys, shapes, caps all fall out), typed per the platform DTOs (title ≤ 200 chars agent-side; tags ≤ 16 × 64; criteria `{text ≥ 1 char, met bool}`).

**Typed coupling:** `TestBoardPayloadSchema_DTOParity` reflects over the platform's typed request mirrors — `pkg/mcp`'s `BoardTaskBody`/`BoardEpicBody`/`BoardPlanBody` (each mirroring the REST request shapes per their own doc comments, as the board parity suites pin) and `pkg/board`'s `PlanInput`/`PlanTaskInput` — and asserts the schema property sets match modulo DECLARED exclusions (inline-only control-plane keys; the legacy REST arms; `dependsOn` absent from the update patch because the platform update DTO does not bind it — a file spelling would be silently dropped). A DTO change without a schema update fails CI.

**Semantic checks above the schema:** #1849 atomic-create (trimmed non-empty description/criteria — the cross-field rule is explicit code), the placeholder-class scan (REUSE `isPlaceholderClassPayload` → `board.IsPlaceholderClass` — ONE vocabulary with send_message and the engine's seed guard), plan-key uniqueness, and the unified doc's `exactly-one-of` arm rule.

**Findings** (both entry points, identical bytes): `{path (instance location, arrays indexed — `tasks[2].acceptanceCriteria[0].text`), rule, severity error|warning, problem, fix, expected/actual}` — deterministically ordered (pipeline stage, then path). Warnings never block (e.g. `acceptanceCriteria` in an update patch REPLACES the full set).

**Entry points:** (1) the `board_validate_file` tool — local, advisory, zero wire calls, never deletes; for import_plan, graph semantics (cycles, caps) stay with `dryRun`. (2) **The CRUD dispatch gate** — the SAME function runs on every payload file before the wire call; severity=error findings refuse locally with the identical report embedded (validate-first is UX, not correctness). The dual-entry-point equivalence is pinned byte-for-byte.

## 7. Decisions

| Decision | Call |
|---|---|
| Cutover | STRAIGHT — no dual path, no deprecation window; the platform wire body is unchanged, in-flight sessions get a loud actionable refusal |
| Merge semantics | DROPPED — patch-only; an additive `merge` wire flag remains possible later |
| Cleanup | unconditional on success; no opt-out |
| import_plan | `planFile` = the unified plan document; THE multi-task-in-one-file path |
| Validate | local-only at both entry points; the platform stays the authority |
| Title cap | 200 chars agent-side (design-doc number; the platform's `MaxTitleBytes` 512 stays wire-authoritative) |
| requestId | rides the payload file (part of every manager route's DTO); NOT inline on the five tools |
| External MCP/REST | no file params, no cutover — inline and programmatic |

## 8. Coordination

Agent-side and board-endpoint changes only; the in-flight lanes in this clone (`session_notifier*`, `pkg/abi/**`, `cmd/workspace-agentd/sessionstate/**`) are untouched. The plugin's `INJECTED_SESSION_TOOLS` gains `llmsafespaces_board_validate_file` (identity supply parity); the api board parity matrix gains the `validate_payload` row (agent-live, REST/external absent).
