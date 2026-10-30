# The content batch — webhook-mapped input + agent
<!-- pattern: slug=content-batch-descriptions; doc=workflow; use-case=batch-content-ops; nodes=script,condition,agent; wiring=webhook-mapped -->
Description: The mapped-input shape — a webhook is the go signal, the static input document pins the run's parameters verbatim, a script node loads the live roster, a condition guards the empty case, an agent writes the batch.

Mapped input (`inputFrom: "mapped"`) means the run's input is the STATIC
`input` document carried by the trigger — byte-for-byte, nothing merged.
Use it when the workflow expects a FIXED shape the posted payload cannot
guarantee: the webhook says "go now", the mapped document says "with these
parameters". The live data is read inside the DAG from the workspace, where
it can be validated and counted before any agent turns run.

## The workflow

```yaml kind=workflow-spec
nodes:
  - id: load
    type: script
    data:
      language: python
      handler: |
        import csv

        def handler(input):
            path = str(input["roster_path"])
            with open(path, newline="") as f:
                rows = list(csv.DictReader(f))
            skus = [r["sku"] for r in rows if r.get("sku")]
            return {"count": len(skus), "skus": skus,
                    "roster_path": path, "output_path": str(input["output_path"]),
                    "max_words": int(input["max_words"]), "tone": str(input["tone"])}
  - id: guard
    type: condition
    data:
      conditions:
        - id: has_rows
          expression: input.count > 0
  - id: write
    type: agent
    data:
      prompt: |
        Write product descriptions for these {{.count}} SKUs: {{.skus}}.
        Read the roster for name/category/features: {{.roster_path}}. Follow the brand sheet at
        /workspace/content/brand-sheet.md — two sentences per description (what it is; who it is
        for), at most {{.max_words}} words each, tone: {{.tone}}, no banned phrases from the sheet.
        Write the completed sheet (all columns plus a description column) to {{.output_path}}.
        Every row gets a description — none blank. Re-run your own checks on the finished sheet
        before finishing: count, blanks, duplicates, the word cap.
  - id: empty
    type: script
    data:
      language: python
      handler: |
        def handler(input):
            return {"skipped": "roster has no sku rows", "roster": input.get("roster_path", "")}
edges:
  - {source: load, target: guard}
  - {source: guard, target: write, sourceHandle: has_rows}
  - {source: guard, target: empty, sourceHandle: otherwise}
```

The `inputSchema` is the contract the mapped document must satisfy
(`additionalProperties: false` is the shape mapped mode exists for — a
merged envelope would violate it; see `inputschema-gotchas.md`):

```json kind=shell
{
  "type": "object",
  "additionalProperties": false,
  "required": ["roster_path", "output_path", "max_words", "tone"],
  "properties": {
    "roster_path": {"type": "string"},
    "output_path": {"type": "string"},
    "max_words": {"type": "integer", "minimum": 10},
    "tone": {"type": "string"}
  }
}
```

Body: `{workflow: {name: "content-batch", targetWorkspaceId: "<this
workspace>", specYaml: "<the YAML above>", inputSchema: <the schema>}}`.

## The wiring — webhook, mapped input

```sh kind=shell
trigger_create {trigger: {name: "content-batch-go",
  sourceType: "webhook", sourceConfig: {},
  inputFrom: "mapped",
  input: {roster_path: "/workspace/content/spring-skus.csv",
          output_path: "/workspace/content/spring-descriptions.csv",
          max_words: 60, tone: "warm, plain, no hype"},
  workflowId: "<the workflow id>"}}
```

Each fire's run input is EXACTLY the `input` document — the posted webhook
payload does not reach the DAG. Re-point the batch at a new roster with
`trigger_update` on the `input` field, not a new workflow.

## The traps this example teaches

- **Validate data in a script node, not the prompt.** `load` counts rows
  and fails loudly on a malformed roster (a raised error fails the node —
  visible in `workflow_runs`) before any agent turn is spent.
- **Type the schema honestly.** `max_words` is an integer in the schema, so
  the mapped document must carry `60`, not `"60"` — a string where the
  schema says integer is a `validation_error` fire
  (`inputschema-gotchas.md`).
- **The condition passes `load`'s output through** — `write` sees
  `{{.count}}`, `{{.skus}}`, `{{.tone}}` from the load node, and the
  `guard` expression reads the same object as `input.count`.
- **Bound the batch in the prompt.** The agent is one turn: the count, the
  cap, and the self-check instruction are all in the prompt because nothing
  else crosses into its fresh session.
