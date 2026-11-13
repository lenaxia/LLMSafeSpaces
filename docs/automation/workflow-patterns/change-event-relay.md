# The change-event relay — webhook body input, condition-routed
<!-- pattern: slug=change-event-relay; doc=workflow; use-case=code; nodes=script,condition,agent; wiring=webhook-body -->
Description: The body-mode shape — the posted payload IS the run input, a script node normalizes and validates it, a condition routes on the change kind, an agent relays the verified summary.

Body mode (`inputFrom: "body"`, webhook only) makes the posted document the
run input verbatim — placeholder paths are PAYLOAD-relative (`{{.title}}`),
not under `body`. That contrast is the point of this example: the same
payload field reads `{{.body.title}}` in envelope mode and `{{.title}}` in
body mode.

## The workflow

```yaml kind=workflow-spec
nodes:
  - id: normalize
    type: script
    data:
      language: python
      handler: |
        def handler(input):
            # Body mode: input IS the posted document. Validate loudly —
            # an unexpected payload must fail THIS node, not the agent leg.
            for field in ("kind", "title", "url", "actor"):
                if field not in input:
                    raise ValueError("payload missing %r" % field)
            kind = str(input["kind"]).lower()
            head = (input.get("head") or {})
            return {"kind": kind,
                    "title": str(input["title"]),
                    "url": str(input["url"]),
                    "actor": str(input["actor"]),
                    "head_sha": str(head.get("sha", ""))}   # dotted walk done here, once
  - id: route
    type: condition
    data:
      conditions:
        - id: merge
          expression: input.kind == "merge"
  - id: relay
    type: agent
    data:
      prompt: |
        A merge landed: "{{.title}}" by {{.actor}} at {{.url}} (head {{.head_sha}}).
        Send a message to session "amber-orchard-listens" with exactly this text:
        "Merge landed: {{.title}} ({{.head_sha}}) — verify the run on the branch before reporting."
        Then report the sent confirmation and stop.
  - id: note
    type: script
    data:
      language: python
      handler: |
        def handler(input):
            return {"noted": input["kind"], "title": input["title"], "actor": input["actor"]}
edges:
  - {source: normalize, target: route}
  - {source: route, target: relay, sourceHandle: merge}
  - {source: route, target: note, sourceHandle: otherwise}
```

Body: `{workflow: {name: "change-relay", targetWorkspaceId: "<this
workspace>", specYaml: "<the YAML above>"}}`.

## The wiring — webhook, body input

```sh kind=shell
trigger_create {trigger: {name: "change-relay",
  sourceType: "webhook", sourceConfig: {},
  inputFrom: "body",
  workflowId: "<the workflow id>"}}
```

`trigger_rotate_webhook_secret` after create; the sender signs deliveries —
the signature the relay state ("from whom") comes from the trigger's own
secret rotation lifecycle, not the DAG.

## The traps this example teaches

- **Payload-relative paths in body mode.** `{{.title}}` here would be
  `{{.body.title}}` under envelope mode (`threshold-alert-summary.md`) —
  choose the mode, then write the paths to match.
- **Normalize in a script, not in expressions or prompts.** `normalize`
  lowercases `kind` (external payloads return what they return), validates
  required fields with a RAISE (a malformed post fails the node — visible
  in `workflow_runs` — instead of producing a nonsense relay), and does the
  nested walk (`head.sha`) ONCE so downstream nodes see flat fields.
- **Webhook deliveries are at-least-once.** The same merge may POST twice;
  the relay node's agent turn is deduped per (workflow, node, run) by the
  platform — but two FIRES are two runs. Dedup on event identity when the
  side effect must be once-only (`idempotency-and-gates.md`).
- **The exact relay text lives in the prompt.** The agent is a fresh
  session — the destination and the message are both IN the prompt, never
  "looked up".
