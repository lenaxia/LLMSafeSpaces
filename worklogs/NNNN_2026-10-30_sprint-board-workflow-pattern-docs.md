# Worklog: worked-pattern docs for the Sprint Board and workflows (owner ruling 2026-10-30, tsk-rested-shanty-feels)

Branch: `worked-patterns-board-workflows` off `origin/main` @ `a345b8101`.

## 1. The rulings

1. (13:xx owner ruling, verbatim intent) "We should have a diverse set of examples, for different use cases" — for the Sprint Board MCP tools and the workflow DAG tools, following the gate-script-patterns.md precedent (embedded in the agentd image, pointer from the tool description).
2. FORMAT ruling (mid-flight, binding): ONE FILE PER EXAMPLE in `sprint-board-patterns/` and `workflow-patterns/`, embedded via `//go:embed <dir>/*.md`; every file self-indexes with the canonical greppable header; the catalog is grepped, never hand-maintained; NO generated INDEX.
3. FENCE CONVENTION ruling (mid-flight, binding): every fenced code block carries `<lang> kind=<kind> [op=<op>]` — exactly one kind=, no bare fences; board-payload fences pass the REAL board_validate_file schema; workflow-spec fences pass the REAL workflow spec validation; verified in-repo by this story's own tests.

## 2. What landed

### The delivery mechanism (the gate-doc precedent, generalized to directories)

- **Sources of truth**: `docs/automation/sprint-board-patterns/` (13 files) and `docs/automation/workflow-patterns/` (9 files).
- **Embedded copies**: `cmd/workspace-agentd/sprint_board_patterns/` + `workflow_patterns/` — `//go:embed <dir>/*.md` into `embed.FS` vars in `gate_docs.go`; adding an example is doc-only (no Go change).
- **Materialization**: `ensureEmbeddedPatternDir(fsys, srcDir, dstDir)` in `gate_docs.go` — per-file idempotent atomic writes (0644, temp+rename, the shared `writeDocAtomic` helper extracted from — and error-string-compatible with — the single-file gate-doc path) PLUS set-mirroring: a file dropped from the image is REMOVED from the pod dir, so the pod path always reflects the embedded set (no orphan a description's grep catalog could still find). init-fs step 8 materializes both to `/sandbox-runtime/docs/sprint-board-patterns/` and `/sandbox-runtime/docs/workflow-patterns/` — best-effort, logs loudly, never blocks boot.

### The board catalog (13 files)

- SIX use-case stories (the drawer's taxonomy, agent-shaped): `code-story`, `research-story`, `creative-writing-story`, `data-analytics-story`, `data-post-processing-story`, `batch-content-ops-story` — each a full create_task payload (kind=board-payload fence), 2-3 CHECKable criteria with the verification move named, and the evidence shape.
- THREE lifecycles: `lifecycle-happy-path` (the exact tool moves, claim-token discipline, the 409 recovery), `lifecycle-fail-bounce` (findings ride the remediation charter; iterate on the SAME pull request), `lifecycle-salvage` (salvage note + `session_read` on the prior assignee session; keep/revert/next).
- `process-discipline` — the standing rules every charter hand-copies today (AI reviewer until APPROVED; never self-merge; push-verify-END, no sleep-polling; evidence URLs per work type table; `make fmt-check`; claim/nudge/throttle discipline), carrying the ONE-TIME wire-term note.
- THREE shapes: `shape-single-story` (flat bodyFile), `shape-epic-plan` (import_plan with a `clean → report` dependency edge, dry-run-first), `shape-reviewer-checkable` (uncheckable→checkable rewrites; observable + verification move + evidence shape).

### The workflow catalog (9 files)

- SIX canonical DAGs (kind=workflow-spec fences): `weekly-research-digest` (cron; script gate+condition+agent), `content-batch-descriptions` (webhook-mapped; script+condition+agent; strict schema), `data-pipeline-export` (cron; http+script+condition+http), `change-event-relay` (webhook-body; script+condition+agent; payload-relative paths), `threshold-alert-summary` (webhook-envelope; script+condition+agent; incident dedup + fetch-then-merge), `manual-chapter-pass` (manual; script+agent(condition-routed); structured output + count-the-artifact). Node-type union: script, agent, http, condition. Wiring union: cron, webhook-envelope, webhook-body, webhook-mapped, manual.
- THREE mechanics files: `placeholder-rules` (resolution order, dotted walks, one-pass, literal-unresolvable, immediate-upstream-only), `idempotency-and-gates` (at-least-once, marker signature, event-identity dedup, the gate-script cross-reference), `inputschema-gotchas` (enforcement map, the wiring 400 + three remedies, the four validation_error mismatches).

### The pins (`cmd/workspace-agentd/pattern_docs_test.go`)

- FORMAT: canonical header on every file, slug==stem, doc matches directory, enum-validated use-case/nodes/wiring, board files carry no DAG metadata, workflow DAG files carry both nodes and wiring; NO INDEX/README files.
- CATALOG COVERAGE: the six themes in the board catalog; all four node types + all five wirings + ≥5 DAGs in the workflow catalog.
- CONTENT: the canonical fragments (process rules, lifecycles, shapes; placeholder/dedup/schema fragments; the gate-script doc path).
- DRIFT: embedded sets == source sets, byte-identical per file (both directions).
- INFRA-NEUTRALITY: the drawer's vendor blocklist over BOTH catalogs + the "task board" absence pin.
- FENCE CONVENTION: every fence carries exactly one kind= from the enum; board-payload fences name a valid op; **every board-payload fence passes the REAL validateBoardPayload pipeline; every workflow-spec fence passes the REAL ParseSpec+ValidateSpec** (nil predecessor schemas, empty defaults — what workflow_create applies; YAML converted with the API's dialect semantics).
- MATERIALIZATION: `ensureEmbeddedPatternDir` mirrors the embedded set (absent→land, identical→untouched mtime, stale→replace, orphan→remove); error paths wrap with step identity and strand no temps.
- init-fs happy path extended: both directories materialize under `$runtimeDir/docs/` in the subprocess boot test.

### Tool-description pointers (both surfaces)

- agentd `mcp_server.go`: `board_create_task`, `board_import_plan`, `workflow_create` each gain the one-line pointer ("Worked … patterns … live at /sandbox-runtime/docs/<dir>/ — grep '^Description:' there for the catalog").
- api-side `pkg/mcp`: `boardCreateTaskTool`, `boardImportPlanTool` (board_tools.go), `workflowCreateTool` (workflow_tools.go) gain the same pointers.
- Drift pins: `TestMCPHandler_ToolDescriptionGuidance` (new "board pattern-doc pointers" block + the workflow_create pointer pin), `TestBoardTools_PatternDocPointerPins`, and the pointer pins beside the gate-doc pointer in `pkg/mcp/server_test.go`.

## 3. Decisions worth recording

- **Condition pass-through is THE teaching spine** of the workflow catalog: engine truth (a condition node's downstream sees the condition's input; every non-condition node replaces `currentInput`) drives three files' shapes (script→condition→agent keeps fields visible; script→http→agent loses them; agent structured answers echo what downstream still needs).
- **The http node's body is fixed spec text** (no placeholder rendering) — the pipeline example teaches fixed-shape pings + config fetches, dynamic payloads in scripts (fetching for themselves, per gate-script-pattern 1).
- **manual-chapter-pass round 1 had a real data-flow bug** (count read `draft_path` from the agent node's output, where it cannot exist) — fixed with outputSchema + enforceStructuredOutput and the answer-echo pattern, which then BECAME the file's fourth trap.
- The gate-script fence in `idempotency-and-gates.md` is a fragment that PARSES (the convention's bar for gate-script is parseability); the full worked gate scripts stay in gate-script-patterns.md.
- `inputSchema` JSON fences are tagged `kind=shell` (illustrative) — the closed enum has no schema-document kind, and they are not machine-validated by the convention's classes.

## 4. Gates

- `go build ./...`; `go test ./pkg/mcp/`; full `go test .` in cmd/workspace-agentd (includes the new pins + the init-fs subprocess leg); `make fmt-check`.
- Fence structure self-checked (bare-opening/closer-infostring walk) before the test pins ran.

## 5. Process

Conventional commits; this worklog (NNNN sentinel — the bot assigns the number); CHANGELOG [Unreleased]; COORDINATE.md active-claim row. AI reviewer until APPROVED; the orchestrator merges; never self-merge.
