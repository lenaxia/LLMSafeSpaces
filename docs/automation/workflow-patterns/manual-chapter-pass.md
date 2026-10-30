# The manual chapter pass — one-shot writing pipeline
<!-- pattern: slug=manual-chapter-pass; doc=workflow; use-case=creative-writing; nodes=script,agent,condition; wiring=manual -->
Description: The manual-run shape — workflow_run carries the chapter brief as a schema-validated input, a script preps the beats, an agent drafts, a script counts the draft on disk, a condition routes short drafts to an expansion pass.

Manual wiring is the test loop's fire button: no trigger at all — you call
`workflow_run {id, input}` and the input is validated against the
workflow's `inputSchema` BEFORE anything runs. Reach for this shape when
the work is on-demand with parameters that change per run.

## The workflow

```yaml kind=workflow-spec
nodes:
  - id: prep
    type: script
    data:
      language: python
      handler: |
        def handler(input):
            chapter = int(input["chapter"])
            # The outline contract: "- beat text" lines under a "## <chapter-number>" heading.
            beats, in_ch = [], False
            for line in open(str(input["outline_path"])):
                s = line.strip()
                if s.startswith("## "):
                    in_ch = s[3:].strip() == str(chapter)
                elif in_ch and s.startswith("- "):
                    beats.append(s[2:])
            return {"chapter": chapter, "beats": beats,
                    "target_words": int(input["target_words"]),
                    "band_low": int(input["band_low"]), "band_high": int(input["band_high"]),
                    "draft_path": str(input["draft_path"])}
  - id: draft
    type: agent
    data:
      prompt: |
        Draft chapter {{.chapter}} of the manuscript — target {{.target_words}} words
        (acceptable {{.band_low}}-{{.band_high}}). Hit these beats, in order:
        {{.beats}}
        Write the draft to {{.draft_path}} with a header line "words: N" where N is your count.
        Close-third present tense, short sentences in tense moments (the series voice).
        Answer with ONLY a JSON object: {"draft_path": "<where you wrote>",
        "band_low": {{.band_low}}, "band_high": {{.band_high}},
        "beats": <the beats you covered, in order, as a JSON array of strings>} —
        the downstream count node reads those fields from your structured answer.
      outputSchema:
        type: object
        additionalProperties: false
        required: [draft_path, band_low, band_high, beats]
        properties:
          draft_path: {type: string}
          band_low: {type: integer}
          band_high: {type: integer}
          beats: {type: array, items: {type: string}}
      enforceStructuredOutput: true
  - id: count
    type: script
    data:
      language: python
      handler: |
        def handler(input):
            # The agent node's output carries its structured answer under
            # "response" (enforceStructuredOutput parses it) — everything
            # else (bands, path, beats) does NOT survive the hop, so the
            # answer echoes what downstream nodes still need.
            ans = input["response"]
            text = open(str(ans["draft_path"])).read()
            words = len(text.split())
            out = dict(ans)
            out["word_count"] = words
            out["in_band"] = ans["band_low"] <= words <= ans["band_high"]
            return out
  - id: route
    type: condition
    data:
      conditions:
        - id: in_band
          expression: input.in_band == true
  - id: polish
    type: agent
    data:
      prompt: |
        The draft at {{.draft_path}} is {{.word_count}} words — inside the band. Read it once for
        voice breaks (person, tense, rhythm) and fix them in place. Keep the beat order intact.
        Report what you changed, then stop.
  - id: expand
    type: agent
    data:
      prompt: |
        The draft at {{.draft_path}} is {{.word_count}} words — under the {{.band_low}}-word floor
        by {{.band_low}} minus its count. Expand it toward the band by deepening the scenes around
        these beats (do not add new beats): {{.beats}}
        Update the header word count when done, then report the new count and stop.
edges:
  - {source: prep, target: draft}
  - {source: draft, target: count}
  - {source: count, target: route}
  - {source: route, target: polish, sourceHandle: in_band}
  - {source: route, target: expand, sourceHandle: otherwise}
```

The `inputSchema` is the run's contract — manual runs are validated against
it (`inputschema-gotchas.md`):

```json kind=shell
{
  "type": "object",
  "additionalProperties": false,
  "required": ["outline_path", "chapter", "target_words", "band_low", "band_high", "draft_path"],
  "properties": {
    "outline_path": {"type": "string"},
    "chapter": {"type": "integer", "minimum": 1},
    "target_words": {"type": "integer", "minimum": 100},
    "band_low": {"type": "integer", "minimum": 50},
    "band_high": {"type": "integer", "minimum": 100},
    "draft_path": {"type": "string"}
  }
}
```

Body: `{workflow: {name: "chapter-pass", targetWorkspaceId: "<this
workspace>", specYaml: "<the YAML above>", inputSchema: <the schema>}}`.

## The wiring — manual

```sh kind=shell
workflow_run {id: "<the workflow id>",
  input: {outline_path: "/workspace/book/outline.md", chapter: 7,
          target_words: 2500, band_low: 2200, band_high: 2800,
          draft_path: "/workspace/book/manuscript/chapter-7.md"}}
```

A run appears immediately; `workflow_runs {id}` shows
running/succeeded/failed with per-node detail.

## The traps this example teaches

- **Count the artifact, not the agent's claim.** `draft` self-reports a
  word count; `count` re-reads the FILE and the condition routes on that —
  verification belongs to a script with no incentive.
- **Files are the inter-node bus for agent output.** The agent writes the
  draft where the prompt says; the next script reads it back. An agent
  node's structured output (its `response`) is fine for short fields, but
  a 2,500-word draft belongs on disk.
- **The structured answer echoes what downstream still needs.** `draft`
  returns draft_path, the bands, and the beats in its structured output
  because NOTHING else survives the node hop — `count` reads them from
  `input["response"]`, re-counts the file itself, and `dict(ans, …)`
  chains the same fields onward so `route`'s downstream prompts keep
  `{{.draft_path}}`/`{{.band_low}}`/`{{.beats}}`.
- **Every condition branch lands somewhere.** `in_band` → `polish`,
  `otherwise` → `expand`; both terminals are agent nodes, and both prompts
  restate the numbers they need (fresh session, every time).
