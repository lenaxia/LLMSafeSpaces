# The weekly research digest — cron + gate + agent
<!-- pattern: slug=weekly-research-digest; doc=workflow; use-case=research; nodes=script,condition,agent; wiring=cron -->
Description: The canonical scheduled-agent DAG — a cron trigger fires weekly, a script node gates on a one-per-week marker, a condition routes, an agent drafts the digest from the workspace's notes.

The question this shape answers: "produce a digest of the week's research
notes, once a week, without re-firing on every schedule tick or burning an
agent turn when there is nothing new." The moving parts: the cron wiring
(the run input is the fire envelope), the script gate (marker dedup — see
`idempotency-and-gates.md`), and an agent node whose prompt placeholders
resolve against the gate's output.

## The workflow

`workflow_create` — the spec rides `specYaml` as YAML text:

```yaml kind=workflow-spec
nodes:
  - id: gate
    type: script
    data:
      language: python
      handler: |
        import datetime, os
        MARKER = "/workspace/research-markers/digest-week.txt"
        NOTES = "/workspace/notes"

        def handler(input):
            recv = str(input.get("received_at", ""))[:10]
            y, w, _ = datetime.date.fromisoformat(recv).isocalendar()
            week = "%d-W%02d" % (y, w)
            try:
                with open(MARKER) as f:
                    if f.read() == week:
                        return {"ready": False, "week": week, "reason": "digest already sent"}
            except OSError:
                pass
            os.makedirs(os.path.dirname(MARKER), exist_ok=True)
            with open(MARKER, "w") as f:
                f.write(week)   # at-most-once per week; the tradeoff is stated in idempotency-and-gates.md
            titles = sorted(fn for fn in os.listdir(NOTES) if fn.endswith(".md")) if os.path.isdir(NOTES) else []
            return {"ready": True, "week": week, "notes": titles}
  - id: route
    type: condition
    data:
      conditions:
        - id: ready
          expression: input.ready == true
  - id: digest
    type: agent
    data:
      prompt: |
        Draft the {{.week}} research digest. Read every file listed here from /workspace/notes: {{.notes}}.
        Write /workspace/reports/digest-{{.week}}.md with: a five-line summary up top, then one
        section per note with its key finding and open questions. Cite each claim with the note's
        filename. This is a fresh session — carry nothing from context; the file list above is complete.
  - id: skip
    type: script
    data:
      language: python
      handler: |
        def handler(input):
            return {"skipped": input.get("reason", "not ready")}
edges:
  - {source: gate, target: route}
  - {source: route, target: digest, sourceHandle: ready}
  - {source: route, target: skip, sourceHandle: otherwise}
```

Body: `{workflow: {name: "weekly-research-digest", targetWorkspaceId: "<this
workspace>", specYaml: "<the YAML above, one string>"}}`. No `inputSchema`
here — the cron fire's input is the envelope, and schema enforcement does
not apply to envelope fires (see `inputschema-gotchas.md`).

## The wiring — cron

```sh kind=shell
trigger_create {trigger: {name: "weekly-research-digest",
  sourceType: "cron", sourceConfig: {expr: "0 9 * * 1", tz: "UTC"},
  workflowId: "<the workflow id — camelCase field name>"}
```

A new trigger does NOT fire immediately — scheduling starts at the next
occurrence. Each fire's run input is the envelope
`{source, received_at, headers, body}` — the gate reads `received_at` to
derive the ISO week.

## The traps this example teaches

- **Condition nodes pass their input through.** `digest` sits downstream of
  `route`, but its placeholders (`{{.week}}`, `{{.notes}}`) resolve against
  the GATE's output — a condition's downstream sees the condition's input,
  not the condition's (empty) output. (`placeholder-rules.md`, trap 1.)
- **Condition expressions see the same object as `input.<field>`.** The
  expression is `input.ready == true` — the env is
  `{"input": <upstream output>}`.
- **Every condition branch needs an edge** — `ready` routes to `digest`,
  and the implicit `otherwise` MUST have its own edge (`skip`), or spec
  validation refuses the DAG.
- **The agent prompt is a fresh session.** It carries everything: the note
  list, the output path, the digest structure. Never "look up what you were
  sent" — resolve it into the prompt (`{{.notes}}` renders the list as
  JSON).
