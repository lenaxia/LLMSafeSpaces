# Placeholder rules — how `{{.path}}` resolves in agent prompts
<!-- pattern: slug=placeholder-rules; doc=workflow; use-case=mechanics; nodes=none; wiring=none -->
Description: The exact resolution semantics of {{.path}} placeholders in agent-node prompts — what the input is, how dotted paths walk, what renders how, and the traps that make a prompt silently wrong.

## Where placeholders work

Agent-node prompts ONLY. A `{{.path}}` ref is replaced from the node's
INPUT — condition expressions use their own vocabulary (`input.<field>`),
and http/script node bodies are literal spec text with no rendering.

## What the input IS

- **The first node's input is the run input**: the fire envelope
  (`{source, received_at, headers, body}`), the posted payload (body mode),
  the static `input` document (mapped mode), or your `workflow_run` input
  (manual).
- **Every later node's input is the PREVIOUS non-condition node's output.**
  A condition node passes its input through unchanged — its downstream sees
  what the condition saw. So `script → condition → agent` keeps the
  script's fields visible to the agent; `script → http → agent` does NOT
  (the agent sees only the http output).
- **Node output shapes worth memorizing**: a script node returns whatever
  its handler returns; an http node returns
  `{status, headers, body, duration_ms}` with `body` a STRING; an agent
  node returns `{response, tokens, prompt, parts, session_id}`.

## How a ref resolves — in order

1. **Exact top-level key first.** A flat key literally named `body.topic`
   beats the dotted walk — key matching is a plain map lookup, any charset.
2. **Dotted path walk.** Segments split on `.` and descend through nested
   maps: `{{.body.pull_request.title}}` walks
   `input["body"]["pull_request"]["title"]`. Hyphens inside a segment are
   fine (`{{.headers.x-request-id}}`).
3. **Unresolvable refs stay literal.** The text `{{.nope}}` reaches the
   agent verbatim — a visible tell in the rendered prompt, not a failure.

Rendering: **scalars render bare** (strings without quotes), **composites
render as compact JSON** (`{{.notes}}` with a list value renders the whole
list), **nil renders as null**.

## The traps

1. **Mode decides the root.** The same payload field is `{{.title}}` in
   body mode and `{{.body.title}}` in envelope mode. Write the paths to
   match the trigger's `inputFrom`.
2. **One left-to-right pass, values never re-scanned.** A value that itself
   looks like a ref cannot trigger further expansion — an externally
   supplied field cannot pull other fields into the prompt.
3. **No braces, no newlines inside a ref.** `{{.x` with no closing `}}`
   never swallows a following valid ref; multi-line keys are simply not
   addressable.
4. **Data two nodes back is GONE.** Only the immediate input resolves. If
   an agent needs earlier data, merge it forward: a preceding script node
   returns one combined object (`threshold-alert-summary.md`), or the
   agent's structured answer echoes the fields downstream still needs
   (`manual-chapter-pass.md` — outputSchema + a script that chains
   `dict(ans, …)`).
5. **These are not trigger-prompt tokens.** The routine path's
   `{{key}}` (gate-script vars), `{{.input}}`, `{{.prevResult}}`, and
   `{{.scriptResult}}` belong to trigger prompts, not workflow agent
   nodes — the dot-prefixed platform namespace never resolves here.

## A worked mini-map

Given body-mode input
`{"title": "Fix login", "head": {"sha": "abc123"}, "actor": "kim"}`:

| Ref | Renders |
| --- | --- |
| `{{.title}}` | `Fix login` |
| `{{.head.sha}}` | `abc123` |
| `{{.head}}` | `{"sha":"abc123"}` |
| `{{.base.sha}}` | `{{.base.sha}}` (literal — unresolvable) |
