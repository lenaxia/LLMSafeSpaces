# The threshold alert summary — webhook envelope input
<!-- pattern: slug=threshold-alert-summary; doc=workflow; use-case=data-analytics; nodes=script,condition,agent; wiring=webhook-envelope -->
Description: The envelope-mode shape — a monitor's alert webhook, the envelope {source, received_at, headers, body} as run input, incident-level dedup in a leading script, and the fetch-then-merge pattern for agents that need data from more than one source.

Envelope mode is the webhook default: the run input is
`{source, received_at, headers, body}` — the posted payload sits UNDER
`body`, so placeholder paths walk (`{{.body.metric}}`,
`{{.headers.x-request-id}}`). Use it when the DAG wants the delivery's
context alongside the payload; use body mode
(`change-event-relay.md`) when only the payload matters.

## The workflow

```yaml kind=workflow-spec
nodes:
  - id: ingest
    type: script
    data:
      language: python
      handler: |
        import json, os, urllib.request

        MARKER_DIR = "/workspace/alert-markers"

        def handler(input):
            body = input.get("body") or {}
            incident = str(body.get("incident_id", ""))
            metric = str(body.get("metric", ""))
            if not incident or not metric:
                raise ValueError("alert payload missing incident_id/metric")
            marker = os.path.join(MARKER_DIR, incident + ".txt")
            if os.path.exists(marker):
                return {"fresh": False, "incident": incident}   # flapping alert — already relayed this incident
            os.makedirs(MARKER_DIR, exist_ok=True)
            open(marker, "w").close()                           # at-most-once per incident (idempotency-and-gates.md)
            # Fetch INSIDE the script when the next agent needs payload + fetch together:
            # an agent sees only its IMMEDIATE upstream's output, so an http node in
            # between would leave the alert fields two nodes back (trap 1 below).
            with urllib.request.urlopen("https://metrics.example.internal/current/" + metric, timeout=10) as r:
                snapshot = json.load(r)
            return {"fresh": True, "incident": incident, "metric": metric,
                    "observed": body.get("value"), "threshold": body.get("threshold"),
                    "fired_at": str(input.get("received_at", "")), "snapshot": snapshot}
  - id: route
    type: condition
    data:
      conditions:
        - id: fresh
          expression: input.fresh == true
  - id: summarize
    type: agent
    data:
      prompt: |
        Metric {{.metric}} crossed its threshold ({{.threshold}}) at {{.fired_at}} — observed {{.observed}}.
        Incident {{.incident}}. Current snapshot as JSON: {{.snapshot}}
        Write /workspace/reports/alerts/{{.incident}}.md: what crossed, by how much, the three most
        affected series in the snapshot, and one recommended next check. Cite snapshot fields by name.
        Then send a message to session "amber-orchard-listens" with exactly:
        "Alert {{.incident}}: {{.metric}} = {{.observed}} (threshold {{.threshold}}) — report at
        /workspace/reports/alerts/{{.incident}}.md". Stop after the send.
  - id: quiet
    type: script
    data:
      language: python
      handler: |
        def handler(input):
            return {"skipped": "incident already relayed", "incident": input.get("incident", "")}
edges:
  - {source: ingest, target: route}
  - {source: route, target: summarize, sourceHandle: fresh}
  - {source: route, target: quiet, sourceHandle: otherwise}
```

Body: `{workflow: {name: "alert-summary", targetWorkspaceId: "<this
workspace>", specYaml: "<the YAML above>"}}`.

## The wiring — webhook, envelope input (the default)

```sh kind=shell
trigger_create {trigger: {name: "alert-summary",
  sourceType: "webhook", sourceConfig: {},
  workflowId: "<the workflow id>"}}
```

`inputFrom` omitted = envelope mode. Rotate the webhook secret after create
(`trigger_rotate_webhook_secret`) and hand it to the monitor.

## The traps this example teaches

- **An agent sees only its IMMEDIATE upstream's output.** If the chain were
  script → http → agent, the agent would see only the http node's
  `{status, headers, body, duration_ms}` — the alert fields would be
  unreachable two nodes back. When an agent needs payload + fetched data
  together, fetch inside the leading script node and return ONE merged
  object. (Conditions differ: they pass their input through, so
  script → condition → agent keeps the script's fields visible —
  `placeholder-rules.md`.)
- **Envelope paths walk from the envelope root.** `{{.body.metric}}`,
  `{{.received_at}}`, `{{.headers.x-request-id}}` — hyphens are fine inside
  a path segment.
- **Dedup at incident level, not delivery level.** Deliveries are
  at-least-once and monitors flap; the marker file keys on `incident_id` so
  one incident relays once no matter how many fires arrive
  (`idempotency-and-gates.md`).
- **Composite values render as JSON.** `{{.snapshot}}` (an object) renders
  as compact JSON in the prompt — exactly what a fresh agent session needs;
  scalars render bare.
