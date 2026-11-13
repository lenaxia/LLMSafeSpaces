# Idempotency and gates — re-fires, re-deliveries, and the marker idiom
<!-- pattern: slug=idempotency-and-gates; doc=workflow; use-case=mechanics; nodes=none; wiring=none -->
Description: How to keep a workflow safe under repeated fires — the delivery contract (at-least-once), the marker-file signature idiom, event-identity dedup for webhooks, and where the gate-script doc takes over.

## The delivery contract you are writing against

- **Webhook deliveries are at-least-once.** The same event may POST twice;
  two deliveries are two fires, two runs.
- **Cron fires are independent.** A schedule that fires while the previous
  run is still going does not wait — another run starts.
- **Node retries are per-node** (`maxAttempts`), and the agent message leg
  is deduped per (workflow, node, run) by the platform — a retried agent
  node does not double-message the session. But RETRY-level dedup never
  covers FIRE-level duplication.

Any side effect that must happen once (a relay, a batch write, an external
post) needs FIRE-level idempotency — keyed on what the effect is ABOUT, not
on the fire itself.

## The marker-file signature idiom

Runs are stateless; the workspace filesystem is the memory. The idiom
(worked in `weekly-research-digest.md` and `threshold-alert-summary.md`):

1. Compute a **signature** of the thing being effected — the ISO week, the
   incident id, the event id. Minimal: it changes exactly when a NEW effect
   is warranted.
2. Check a marker file under `/workspace/…` (the durable volume —
   `/sandbox-runtime/…` is tmpfs and dies with the pod).
3. Skip when the marker already holds the signature; write it when you
   proceed.

Mark-on-proceed is **at-most-once per signature**: if the run fails after
the mark, the effect is not retried (the failure is visible in
`workflow_runs` — diagnosable, not silent). If you need
retry-until-delivered instead, mark only after the effect's success and
accept re-running the idempotent prefix. State the choice in a comment at
the mark site.

## Event-identity dedup for webhooks

Key the marker on the EVENT's own identity — an `event_id`/`incident_id`
field in the payload — not on wall-clock time:

```python kind=gate-script
event_id = str(input["body"].get("event_id", ""))
marker = os.path.join(MARKER_DIR, "evt-" + re.sub(r"[^A-Za-z0-9_-]", "_", event_id) + ".txt")
```

Sanitize the value before it touches a path (payload fields are external
input), and raise loudly when the identity field is missing — a dedup key
that silently defaults to the empty string collapses every event into one.

## Where the gate-script doc takes over

The full worked forms — normalized external polling, conditional requests,
marker dedup across fires, failure postures (quiet vs fail-open),
self-contained action prompts — live in the pod-side gate-script doc:
**`/sandbox-runtime/docs/gate-script-patterns.md`**. Its patterns apply
verbatim to workflow SCRIPT nodes (same inline-handler contract, same
workspace execution); the difference is placement — a trigger gate script
runs before a routine's single LLM call, while a workflow script node is a
DAG node whose return feeds the next node.

## The checklist before you wire any trigger

- Does a side effect need once-only? → signature marker at the effect's
  key; the choice of mark-time stated.
- Cron cadence vs run duration → concurrent runs are possible; either the
  work is idempotent per fire, or the leading script detects
  already-in-progress (a lock marker with the run id).
- External fetches inside scripts → timeout set (a hung socket burns the
  run), failure posture chosen per the gate doc.
- `http` nodes → fixed-shape requests only; anything dynamic belongs in a
  script node (`data-pipeline-export.md`).
