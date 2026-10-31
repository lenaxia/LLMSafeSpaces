# Worklog: example validity — static drift pins + the async patterns-drift validator (owner ruling 2026-10-30, tsk-surging-frontier-dines)

Branch: `surging-frontier-dines` off `origin/worked-patterns-board-workflows` @ `e478d46ca` (the example-directories story this one builds on; that branch sits on `origin/main` @ `a345b8101`).

## 1. The ruling

(2026-10-30 owner ruling, verbatim intent) "We should have tests or something or maybe an async AI workflow which validates that the examples are still valid." — for `sprint-board-patterns/` and `workflow-patterns/` (tsk-rested-shanty-feels). Validity has TWO tiers: mechanical rot (cheap, static, CI-blocking) and semantic drift (judgment, async, periodic). This story ships both, plus THE EXTRACTOR the same day's FENCE CONVENTION ruling promised.

## 2. What landed (repo leg)

### THE EXTRACTOR + Tier 1 pins (`cmd/workspace-agentd/`)

- `pattern_fence_extractor_test.go` (NEW): the fence machinery moved out of `pattern_docs_test.go` and completed into the promised extractor — `extractFences` (split on fences, read the `kind=` infostring) + per-kind validators + `TestPatternDocs_EveryFence_PassesItsKindValidator`, the dispatch walk. Dispatch: board-payload → `decodeBoardPayloadJSON` + `validateBoardPayload` (the REAL board_validate_file pipeline); workflow-spec → `specJSONFromFence` + `wf.ParseSpec` + `wf.ValidateSpec(nil, DefaultsBlock{})` (the REAL validation workflow_create applies); gate-script → `py_compile` / `node --check` (lang token from the infostring; `exec.LookPath` first, LOUD skip when the interpreter is absent — the `local/us70_harness_script_test.go` precedent; structural non-empty check always); shell/output → human-rendered, format-pinned only. Failures report FILE AND SLUG. The gate-script leg now also asserts the infostring's language token is python|node.
- `pattern_docs_test.go` (EXTENDED): `TestPatternDocs_ToolNameRegistryPin` — regex `\b(?:board|workflow|trigger)_[a-z_]+` over both embedded catalogs; catalog = `mcpToolCatalog()` ∪ `pkg/mcp NewServer(nil).ListTools()` (same sources the #1415/0059 pins read); failure names the referencing file(s). `TestPatternDocs_PlaceholderGrammarPin` — every `{{…}}` candidate per line: `{{.dotted.path}}` (segments `[A-Za-z0-9_-]+`, single dots) or `{{key}}`; whitespace/backtick-bearing candidates and truncated `{{` openers refused outside `placeholder-rules.md` (the grammar doc quotes its own counter-examples); file:line in every failure.
- Mutation-tested by hand before landing (each class reddens naming the file): a fabricated tool token; `{{ broken.ref }}`, `{{.a..b}}`, truncated `{{.x`; a broken-python gate-script fence (py_compile output surfaces in the failure).

### The dogfood example (workflow catalog, 10th file)

- `docs/automation/workflow-patterns/patterns-drift-validator.md` + byte-identical embed copy in `cmd/workspace-agentd/workflow_patterns/`. use-case=mechanics; nodes=script,condition,agent; wiring=cron. Its `kind=workflow-spec` fence is the LIVE validator's spec verbatim (below), so the repo pins validate the running workflow and the Tier-2 walk covers this file too — the validator validates itself (binding).
- All existing pins re-verified green with the file present: header/format, catalog coverage (7 DAGs now), content fragments, vendor blocklist + vocabulary, drift pins (embed == source), tool names, placeholder grammar, fence walk.

## 3. What landed (platform leg — this workspace)

### The workflow

- Created via `workflow_create`: id **`20b66e49-11c3-4300-81c9-8e2d09e66df6`**, name `patterns-drift-validator`, targetWorkspaceId `7eaa3f1b-dee8-473c-9650-4126102b256a` (this workspace). specYaml = the `kind=workflow-spec` fence in the dogfood example, byte-for-byte. DAG: `gather` (script) → `route` (condition `input.changed == true`) → `validate` (agent) | `quiet` (script).

### The trigger

- Created via `trigger_create`: id **`85dd36ad-df2a-4352-90d0-e0ce6407aa13`**, name `patterns-drift-validator-weekly`, weekly cron `23 4 * * 1` UTC (next fire 2026-11-02T04:23Z), `workflowId` camelCase, `inputFrom` envelope, no `inputSchema`. NO inline gate script — the platform REJECTS `script` alongside `workflowId` (400; "a DAG carries its own script nodes"), which is why the hash-dedup gate is the DAG's leading script node (trap 1 of the example doc). The trigger stays armed: an unchanged surface costs two script nodes and stops (no LLM leg).
- Consecutive-failure auto-disable: platform default (`autoDisableAfter` 10).

### The gate (the gather node, verbatim semantics)

- Hashes the drift surface: both example catalogs (precedence: pinned pre-merge copy at `/workspace/patterns-drift/example-sources/` → pod-served `/sandbox-runtime/docs/` → repo `docs/automation/`) PLUS the tool-catalog sources (`cmd/workspace-agentd/mcp_server.go` + `pkg/mcp/*.go`). Digest = `examples[0:16]-catalog[0:16]`, compared to `/workspace/patterns-drift/state.txt` (durable volume — never `/sandbox-runtime`, which is tmpfs).
- **Mark-AFTER-effect** (chosen posture, stated at the site): gather never writes the state; the agent writes it as its FINAL step. A walk that dies leaves the surface stale → next fire retries. The alternative (mark-on-observation, at-most-once per state) would silently skip a failed validation.
- No example sources anywhere → `changed: false`, reason logged, quiet leg (this is the pre-merge world; the day the worked-patterns branch merges and the checkout updates, the walk arms itself).

### The agent leg (the `validate` node charter, in the spec)

Walks every inventory file against LIVE reality: (1) tool-name tokens vs the CURRENT tool surface (liveness probes `board_read`/`workflow_list`/`trigger_list`); (2) every `kind=board-payload` fence → scratch file → live `board_validate_file`; (3) every `kind=workflow-spec` fence vs the live node vocabulary and branch rules; (4) Sprint Board vocabulary + infra-neutrality spot check; (5) advice currency vs live tool descriptions. Output contract: read-only over docs/repo/workflows/triggers; drift → ONE story via `board_create_task` into the reference epic (`24bfec2b-60bf-4959-8b20-3c44adbd361c`, project `f83bcda7-305b-4cb8-ae13-b338e4c37515`) with per-example pointers; clean → nothing filed, one line in `last-clean.txt`; final step either way writes the state digest; final message one line.

### Smoke evidence

- The gather script was executed standalone in this pod before the workflow was created: no-sources world → `changed: false`, quiet routing; pinned pre-merge copy dropped under `/workspace/patterns-drift/example-sources/` → 22 files, `changed: true` on first sight, `changed: false` after the state mark (then the state file was removed so the LIVE first run takes the changed branch itself).
- **Live run: `workflow_run` id `87582d48-f053-47d3-85e2-b4fe7015f451` (manual, input `{}`) — status `succeeded` (2026-10-31T01:35:32Z → 01:41:08Z).** Gather saw the pinned 23-file set (both catalogs INCLUDING this validator's own example — the dogfood self-validation is real), `changed: true` (fresh state), routed to the `validate` agent leg. The agent leg ran a full live walk (~129K tokens): every tool-name token checked against the live surface, all nine board payloads extracted to scratch and probed with live `board_validate_file` (scratch observed mid-run under `/workspace/patterns-drift/scratch/`, cleaned up at the end), spec fences checked against the live vocabulary, vocabulary + neutrality spot-checked. **Verdict: `patterns clean: no findings filed`** — no story created (verified on the board), one line appended to `last-clean.txt`, and the state digest `d9224e8c8f2def27-378759ab5a623b372026` written as the final step. The unchanged-surface quiet leg was proven in the standalone pre-test (`changed: false` → two script nodes, no LLM leg).

## 4. Process discipline

AI reviewer until APPROVED on the repo leg; orchestrator merges; `make fmt-check`; no sleep-polling. No behavior changes to any tool — docs + tests + platform objects only.

### Review round 1 (CHANGES_REQUESTED) → remediated

The AI reviewer independently reproduced my mutation checks (fabricated tool token, broken placeholder, broken python fence — all red) and demonstrated TWO green-holes plus a stale CHANGELOG claim:

1. **inputSchema examples rode `kind=shell`** (the human-rendered no-op): its executed mutation (`"type": "object"` → `"strng"` in `content-batch-descriptions.md`) kept every pin green while `workflow_create` would 400. REMEDIATED: the kind vocabulary gains `kind=input-schema` → the REAL `wf.ValidateInputSchema` dispatch; the two schema fences (`content-batch-descriptions.md`, `manual-chapter-pass.md`) re-tagged from `kind=shell`; coverage floors in both fence pins (input-schema ≥1, gate-script ≥1 — the reviewer's missing-case 3: the sole gate-script fence could otherwise vanish behind the combined shell/output count). Mutation re-run: RED, naming file and slug.
2. **An unclosed final fence silently exited all validation**: `extractFences` appended only on the closing ``` line; a dangling opener at EOF dropped the content from every check (its executed mutation — removing the last closer — kept the whole suite green). REMEDIATED: a dangling opener at EOF is now a per-file error naming file, slug, and the orphaned infostring. Mutation re-run: RED.
3. **Stale CHANGELOG wording** ("skips loud … us70 precedent") contradicting the fail-loud flip from the self-review commit. REMEDIATED: the entry now states the fail-loud contract and the input-schema extension.

Reviewer-acknowledged minors (no action demanded): the placeholder pin is stricter than the runtime for `{{key}}` (fails safe); indented/`~~~` fences are latent-only (corpus clean, 35/35 balanced); the tool-name pin unions the two audiences (declared trade-off).

### Review round 2 (CHANGES_REQUESTED) → remediated

Round 2 verified round 1's findings 2/3 and the floors closed, and caught that **finding 1's remediation had landed for ONE file only**: my r1 mutation-test discipline reverted the `content-batch-descriptions.md` re-tag via `git checkout --` AFTER applying it (the checkout was meant to undo the mutation; it also undid the re-tag riding in the same working tree) — so the docs/CHANGELOG/worklog claims of "two schema fences re-tagged" were false for that file, and the reviewer's executed `"strng"` mutation on it stayed green. Record now matches the diff: BOTH fences re-tagged (verified by grep in both copies), mutation re-run RED by DIRECT `go test` invocation naming file and slug.

New findings, both remediated and mutation-verified:

4. **The unclosed-fence protection was EOF-only**: deleting a MID-FILE closer made the fence silently swallow the following fence and re-sync with all pins green (its executed mutation proved it). REMEDIATED: an opener-like line (```` ``` ````-prefixed, not the bare closer) INSIDE an open fence is now a per-file error naming file, slug, and the offending line — closers are bare, bodies never carry openers. Mutation re-run (mid-file closer removed): RED.
5. **A brace-unbalanced tool-call sketch rode the shell no-op**: `weekly-research-digest.md`'s `trigger_create` sketch shipped 3 `{` vs 2 `}` — a verbatim paste cannot parse (pre-existing on the base branch, in scope here per the zero-pre-existing-errors rule). REMEDIATED: the sketch is balanced (`}}` like every sibling), and kind=shell / kind=output fences gain a structural floor — brace/bracket balance — in the dispatch. Mutation re-run (one `}` dropped): RED naming file and slug.

Also: the per-kind floor messages now say "the catalogs (aggregate)" (counts span both dirs); and the Tier-2 charter gained step 3b (input schemas: JSON well-formed, rooted at type object, the create path's shape) so the extractor header's "the Tier-2 walk mirrors this dispatch" is true — the live workflow `20b66e49-11c3-4300-81c9-8e2d09e66df6` was `workflow_update`d to the amended spec (2026-10-31T03:39Z), keeping the dogfood doc byte-identical to the running workflow. Pinned example sources refreshed; the next weekly fire re-walks the amended surface (the digest moved — by design).

## 5. Run book (for whoever inherits this)

- **Inspect**: `workflow_runs` (the walk), `trigger_fires` (the schedule + gate outcome). **Disarm**: `trigger_update {enabled: false}` (default: stay armed — cheap when gated). **Force a re-walk**: delete `/workspace/patterns-drift/state.txt`. **Validate a pre-merge revision**: drop both catalog dirs under `/workspace/patterns-drift/example-sources/` (wins over pod/repo until removed). **Findings**: one story in the reference epic per drifted surface, worked through the normal review flow — the validator never edits the docs.
