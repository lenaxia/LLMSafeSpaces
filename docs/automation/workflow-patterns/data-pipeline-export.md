# The nightly data pipeline — script + condition + http
<!-- pattern: slug=data-pipeline-export; doc=workflow; use-case=data-post-processing; nodes=script,condition,http; wiring=cron -->
Description: The pipeline leg — a cron fire, an http node fetching the night's limits at the head, a script node cleaning the partition, a condition routing on the anomaly rate, and fixed-shape http pings at the branches.

The http node's place in a DAG: requests whose shape is FIXED at spec time —
a config/limits fetch at the head, side-effect pings at the branches. Its
`body` is literal spec text (no placeholder rendering — see trap 1 below);
dynamic payloads belong in a script node, which can fetch for itself.

## The workflow

```yaml kind=workflow-spec
nodes:
  - id: limits
    type: http
    data:
      method: GET
      url: "https://ops.example.internal/qa-limits/nightly"
      timeout: 10s
  - id: clean
    type: script
    data:
      language: python
      handler: |
        import csv, datetime, json, os

        def handler(input):
            limits = json.loads(input["body"])          # http output: {status, headers, body(str), duration_ms}
            max_rate = float(limits["max_anomaly_rate"])
            day = datetime.date.fromisoformat(str(input["headers"].get("X-Config-Day", ""))[:10] or datetime.date.today().isoformat())
            raw_dir = "/workspace/data/raw/%s" % day.isoformat()
            rows = list(csv.DictReader(open(raw_dir + "/readings.csv", newline="")))
            seen, out, flagged = set(), [], 0
            for r in rows:
                key = (r["sensor_id"], r["ts"])
                if key in seen:
                    continue                              # exact duplicates dropped, counted
                seen.add(key)
                try:
                    v = float(r["temp_f"])
                except ValueError:
                    flagged += 1
                    continue                              # unparseable rows flagged by count in the summary
                out.append({"sensor_id": r["sensor_id"], "ts": r["ts"], "temp_c": round((v - 32) / 1.8, 3)})
            clean_path = "/workspace/data/clean/%s.parquet" % day.isoformat()
            os.makedirs(os.path.dirname(clean_path), exist_ok=True)
            with open(clean_path.replace(".parquet", ".json"), "w") as f:   # stand-in writer for the worked example
                json.dump(out, f)
            rate = flagged / max(len(rows), 1)
            return {"day": day.isoformat(), "rows_in": len(rows), "rows_out": len(out),
                    "flagged": flagged, "anomaly_rate": rate, "max_rate": max_rate,
                    "clean_path": clean_path}
  - id: route
    type: condition
    data:
      conditions:
        - id: over
          expression: input.anomaly_rate > input.max_rate
  - id: escalate
    type: http
    data:
      method: POST
      url: "https://ops.example.internal/review-queue"
      headers: {"Content-Type": "application/json"}
      body: '{"kind": "nightly-clean-hold", "note": "anomaly rate over limit; cleaned partition held for review"}'
  - id: publish
    type: http
    data:
      method: POST
      url: "https://ops.example.internal/archive/ping"
      headers: {"Content-Type": "application/json"}
      body: '{"kind": "nightly-clean-ok"}'
edges:
  - {source: limits, target: clean}
  - {source: clean, target: route}
  - {source: route, target: escalate, sourceHandle: over}
  - {source: route, target: publish, sourceHandle: otherwise}
```

Body: `{workflow: {name: "nightly-clean", targetWorkspaceId: "<this
workspace>", specYaml: "<the YAML above>"}}`.

## The wiring — cron

```sh kind=shell
trigger_create {trigger: {name: "nightly-clean",
  sourceType: "cron", sourceConfig: {expr: "30 3 * * *", tz: "UTC"},
  workflowId: "<the workflow id>"}}
```

The run input is the fire envelope; here nothing reads it (the cleaning
node derives the day itself), so the http node leads and the envelope is
simply not consumed.

## The traps this example teaches

- **The http node's `body` is fixed at spec time.** No placeholders render
  in it — that is why `escalate` posts a fixed-shape hold notice (the
  review queue reads the workspace for detail) rather than trying to
  template the anomaly numbers in. Dynamic payloads: script node, fetching
  for itself (`threshold-alert-summary.md` shows the pattern).
- **An http node's output is `{status, headers, body, duration_ms}` with
  `body` a STRING.** `clean` parses `input["body"]` with `json.loads` —
  forgetting the parse and reading `input.body.max_anomaly_rate` is the
  classic first bug.
- **Branch endpoints can be terminal pings.** Both `over` and `otherwise`
  end in one http node each — a DAG needs at least one terminal, and
  condition branches must ALL carry edges (`over` + `otherwise`).
- **Timeout the http node** (`timeout: 10s`) — the nightly leg should fail
  visibly in `workflow_runs`, not hang until the runner gives up.
- **Fail loudly in scripts.** A missing directory raises, the node fails,
  the run shows `failed` with the node id — far better than a run that
  "succeeds" with an empty partition.
