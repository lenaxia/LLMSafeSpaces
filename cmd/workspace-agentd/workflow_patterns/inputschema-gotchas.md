# inputSchema gotchas — where validation bites and how validation_error fires happen
<!-- pattern: slug=inputschema-gotchas; doc=workflow; use-case=mechanics; nodes=none; wiring=none -->
Description: The enforcement map for a workflow's inputSchema — which runs are validated, which are not, the wiring guard that refuses envelope-mode triggers, and the four mismatches that produce validation_error fires.

## The enforcement map (memorize this first)

A workflow's `inputSchema` is enforced on:

- **manual runs** — `workflow_run` validates your `input` before anything
  executes (`manual-chapter-pass.md` rides this).
- **fired runs with input mapping** — `inputFrom: "mapped"` validates the
  static `input` document at trigger-CREATE time, and each fire carries it
  (`content-batch-descriptions.md` rides this).

It is NOT enforced on envelope- or body-mode fires — their run inputs are
the raw envelope / posted payload by contract. If your DAG assumes fields,
either validate them in a leading script node (raise loudly —
`change-event-relay.md`) or move the assumptions into a mapped document.

## The wiring guard (create-time, a 400 you will meet)

Wiring an envelope-mode trigger to a workflow whose schema requires
non-envelope fields is REJECTED at `trigger_create` — the platform can see
the mismatch before the first fire. Three remedies, pick one deliberately:

1. `inputFrom: "mapped"` + a static `input` document satisfying the schema;
2. `inputFrom: "body"` when the posted payload itself satisfies the schema;
3. relax the schema (drop the strict `required`/`additionalProperties`).

## The validation_error fire

When a fired run's resolved input fails the schema, the fire shows
`validation_error` with the typed violations in `actionResult`
(`schema_mismatch`, locations only — never payload values). Read it with
`trigger_fires`. The four mismatches that cause nearly all of them:

1. **A string where the schema says integer/number.** `{"max_words": "60"}`
   against `"type": "integer"` fails. The static mapped document is JSON —
   write bare numbers, and remember a JSON string "60" stays a string.
2. **`additionalProperties: false` plus merged keys.** Envelope/body modes
   MERGE a static `input` document into the envelope/payload at the top
   level — a strict schema sees the merged keys as violations. This is the
   case `mapped` mode exists for: the static document becomes the run input
   VERBATIM, no merging. Strict schema → mapped mode.
3. **Missing `required` fields.** The mapped document satisfies the schema
   at create time; a later `trigger_update` that drops a required field is
   validated on the patch — but hand-edited API writes can slip. Re-check
   the fire audit after schema-touching updates.
4. **Wrong shape depth.** `"properties": {"sku": {"type": "string"}}` with
   `{"sku": ["a", "b"]}` — an array is not a string, however convenient.
   Schema says what the DAG actually reads; wishful typing fails fires.

## Writing the schema itself

- Type what the script nodes READ (`int(input["chapter"])` → integer, and
   the cast is then a formality, not a rescue).
- `minimum`/`maximum` for bands — bounds are cheap and validation_error
  messages name the location.
- A strict schema (`additionalProperties: false`) is a GOOD default for
  mapped/manual inputs — typos in field names become loud refusals instead
  of silently ignored keys that leave a prompt placeholder unresolved.
- Keep the schema small: it validates the CONTRACT, not the content.
  Content validation (rows exist, files parse) belongs in a leading script
  node where the error message can say what the DAG actually found.
