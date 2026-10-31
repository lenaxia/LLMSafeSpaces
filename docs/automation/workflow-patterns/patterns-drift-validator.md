# The patterns-drift validator — a workflow that files stories when examples rot
<!-- pattern: slug=patterns-drift-validator; doc=workflow; use-case=mechanics; nodes=script,condition,agent; wiring=cron -->
Description: The async tier of example validity — a weekly workflow whose leading script node hashes the drift surface (both example catalogs plus the tool-catalog sources), routes on change, and whose agent leg walks every example against live reality, filing ONE Sprint Board story when the examples have rotted and nothing when they are clean.

This is the async sibling of the repo's static drift pins: CI catches
mechanical rot (tool names, payload schemas, placeholder grammar) at
merge time; this workflow catches semantic drift WEEKLY against the
live surface — renamed tools, changed validation rules, stale advice —
and reports it the board-native way: a story with per-example pointers,
never a direct edit to the docs. It also validates ITSELF: this file is
one of the examples the walk covers (the dogfood ruling — the spec
below is the live workflow, byte-for-byte).

## The workflow

```yaml kind=workflow-spec
nodes:
  - id: gather
    type: script
    data:
      language: python
      handler: |
        import glob, hashlib, os

        REPO = "/workspace/LLMSafeSpaces"
        STATE = "/workspace/patterns-drift/state.txt"
        DIRS = [("sprint-board-patterns", "board"), ("workflow-patterns", "workflow")]
        # Precedence: a pinned pre-merge copy wins, then the pod-served
        # set, then the repo checkout's sources of truth.
        ROOTS = [
            ("/workspace/patterns-drift/example-sources", "pinned"),
            ("/sandbox-runtime/docs", "pod"),
            (os.path.join(REPO, "docs/automation"), "repo"),
        ]
        PROJECT_ID = "f83bcda7-305b-4cb8-ae13-b338e4c37515"
        EPIC_ID = "24bfec2b-60bf-4959-8b20-3c44adbd361c"

        def sha256_file(path):
            h = hashlib.sha256()
            with open(path, "rb") as fh:
                for chunk in iter(lambda: fh.read(65536), b""):
                    h.update(chunk)
            return h.hexdigest()

        def handler(input):
            root, label = "", "none"
            for candidate, lab in ROOTS:
                if os.path.isdir(os.path.join(candidate, DIRS[0][0])):
                    root, label = candidate, lab
                    break
            files = []
            if root:
                for dirname, _kind in DIRS:
                    for path in sorted(glob.glob(os.path.join(root, dirname, "*.md"))):
                        files.append({"dir": dirname, "file": os.path.basename(path),
                                      "sha256": sha256_file(path)})
            examples_h = hashlib.sha256("".join(
                f["dir"] + "/" + f["file"] + "@" + f["sha256"] + "\n" for f in files).encode()).hexdigest()
            # The tool-catalog sources: the same files the #1415/0059
            # drift pins read (agentd mcp_server.go + the pkg/mcp
            # projection) — a renamed or reworded tool moves this hash.
            catalog_paths = [os.path.join(REPO, "cmd/workspace-agentd/mcp_server.go")]
            catalog_paths += sorted(glob.glob(os.path.join(REPO, "pkg/mcp", "*.go")))
            catalog_h = hashlib.sha256("".join(
                p + "@" + (sha256_file(p) if os.path.isfile(p) else "missing") + "\n"
                for p in catalog_paths).encode()).hexdigest()
            digest = examples_h[:16] + "-" + catalog_h[:16]
            prev = ""
            if os.path.isfile(STATE):
                with open(STATE) as fh:
                    prev = fh.read().strip()
            base = {"digest": digest, "example_root": label, "files": files,
                    "file_count": len(files), "epic_id": EPIC_ID, "project_id": PROJECT_ID}
            if not files:
                # Nothing to validate yet (pod dirs absent, repo branch
                # pre-merge): quiet leg, and do NOT advance the state —
                # the walk runs the day sources appear.
                base.update({"changed": False, "reason": "no example sources materialized yet"})
                return base
            # Mark-after-effect: gather does NOT write the state here;
            # the agent writes it as its FINAL step, so a failed walk
            # leaves the surface stale and the next fire retries
            # (retry-until-validated, per idempotency-and-gates.md).
            base.update({"changed": digest != prev, "reason": ""})
            return base
  - id: route
    type: condition
    data:
      conditions:
        - id: changed
          expression: input.changed == true
  - id: validate
    type: agent
    data:
      prompt: |
        You are the patterns-drift validator: the async judgment tier of
        example validity for this workspace's Sprint Board and workflow
        example catalogs. The repo's CI pins catch mechanical rot; you
        catch semantic drift against LIVE reality. Drift surface digest
        {{.digest}}, example root {{.example_root}}, {{.file_count}} files:
        {{.files}}

        Walk EVERY file listed above, exactly once, and check it against
        the live surface you hold right now — never memory:
        1. TOOL NAMES — every board_/workflow/trigger tool token a file
           references must exist in your CURRENT tool set (liveness
           probes: board_read, workflow_list, trigger_list answer their
           families; the rest you know intrinsically).
        2. BOARD PAYLOADS — every fenced block tagged kind=board-payload
           op=<op>: write its JSON body to
           /workspace/patterns-drift/scratch/probe.json and call
           board_validate_file {op, file}; any error-severity finding is
           drift. Remove the probe file after each check.
        3. WORKFLOW SPECS — every fenced kind=workflow-spec block: node
           types within script/agent/http/condition, script nodes
           carrying language and handler, agent nodes carrying prompt,
           every condition branch and its otherwise edge present — the
           same validation workflow_create applies. Compare shapes
           against workflow_list when unsure.
        4. VOCABULARY AND NEUTRALITY — Sprint Board vocabulary holds
           (stories, never the pre-rename wording), the wire-term note
           appears at most once per catalog, and examples name artifacts
           and verification outcomes, never vendors.
        5. ADVICE CURRENCY — where a file states tool behavior (stage-move
           semantics, verdict rules, trigger rules), verify against the
           live tool descriptions; flag anything the current surface
           contradicts.

        OUTPUT CONTRACT — you are read-only over the docs and
        board-native over findings:
        - You NEVER edit any example file, docs tree, repo file,
          workflow, or trigger. Your only writes: the scratch dir above
          (cleaned up), /workspace/patterns-drift/state.txt, and at most
          ONE Sprint Board story.
        - Drift found: file ONE story via board_create_task (manager
          route) into epic {{.epic_id}} (project {{.project_id}}):
          write the bodyFile first — title "Patterns drift: N findings
          across M files (digest {{.digest}})", a description
          enumerating per-example findings (file, section, the offending
          line or fence, the suggested correction), at least one
          checkable acceptance criterion — then submit {epicId, bodyFile}.
        - No drift: create nothing; append one line to
          /workspace/patterns-drift/last-clean.txt:
          "<UTC date> {{.digest}} clean".
        - FINAL STEP either way: write {{.digest}} as the only content of
          /workspace/patterns-drift/state.txt (mark-after-effect: a
          failed walk leaves the state stale so the next fire retries).
        Your final message is ONE line: either "patterns drift: N
        findings filed" or "patterns clean: no findings filed".
  - id: quiet
    type: script
    data:
      language: python
      handler: |
        def handler(input):
            # The cheap leg the weekly cadence rides: unchanged surface
            # (or no sources yet) — one logged line, no LLM call, no story.
            return {"skipped": input.get("reason", "surface unchanged since last validated state"),
                    "digest": input.get("digest", ""), "example_root": input.get("example_root", "")}
edges:
  - {source: gather, target: route}
  - {source: route, target: validate, sourceHandle: changed}
  - {source: route, target: quiet, sourceHandle: otherwise}
```

Body: `{workflow: {name: "patterns-drift-validator", targetWorkspaceId:
"<this workspace>", specYaml: "<the YAML above, one string>"}}`. The
project/epic ids and the repo checkout path inside `gather` are this
workspace's constants — substitute yours when copying.

## The wiring — weekly cron

```sh kind=shell
trigger_create {trigger: {name: "patterns-drift-validator-weekly",
  sourceType: "cron", sourceConfig: {expr: "23 4 * * 1", tz: "UTC"},
  workflowId: "<the workflow id — camelCase field name>"}}
```

No `inputSchema` (envelope fires are not schema-enforced) and NO inline
gate script — see trap 1. The trigger stays armed forever: when the
surface is unchanged the run costs two script nodes and stops
(`trigger_fires` shows the run; `workflow_runs` shows the quiet line).

## The run book

- **State**: `/workspace/patterns-drift/state.txt` holds the last
  VALIDATED digest (written by the agent leg, never by gather). Force a
  re-walk by deleting it; the next fire behaves like first sight.
- **Pinned sources**: drop a pre-merge copy of both catalogs under
  `/workspace/patterns-drift/example-sources/` to validate a revision
  under review — it wins over the pod-served and repo sets until
  removed.
- **Findings**: drift becomes ONE story in the reference epic, worked
  like any other story (AI reviewer until APPROVED; the validator never
  edits the docs itself).
- **Diagnosis**: `workflow_runs` for the walk, `trigger_fires` for the
  schedule; disarm with `trigger_update {enabled: false}` (the weekly
  fire is cheap, so the default is to stay armed).

A clean run's whole footprint:

```text kind=output
patterns clean: no findings filed
```

## The traps this example teaches

- **The gate lives in the DAG, not the trigger.** An inline gate script
  is REJECTED alongside `workflowId` (a DAG carries its own script
  nodes) — so the hash-and-dedup that a routine's gate script would do
  becomes the leading script node plus a condition. Same discipline
  (state dedup across fires), native placement.
- **Mark AFTER the effect, not before.** Gather compares but never
  writes the state; the agent writes it as its final step. A walk that
  dies mid-flight leaves the surface stale and the next fire retries —
  the retry-until-validated posture (`idempotency-and-gates.md`), chosen
  because the effect (a filed story) must not be lost to a crash.
- **State under `/workspace`, never `/sandbox-runtime`.** The workspace
  volume is durable; the runtime dir is tmpfs and dies with the pod
  (`idempotency-and-gates.md`).
- **Conditions pass their input through.** `validate` sits downstream
  of `route` yet resolves `{{.digest}}`, `{{.files}}` and friends
  against GATHER's output — the condition's downstream sees the
  condition's input (`placeholder-rules.md`).
- **Findings are board-native, docs are read-only.** Drift is reported
  as a story with per-example pointers, worked through the normal
  review flow — a validator with edit rights is a self-merging reviewer.
