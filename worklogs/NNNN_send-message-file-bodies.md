# Worklog: send_message file-mediated bodies (#2030b — the #1837 *_file pattern for the fragile long-string emission)

Task `tsk-windy-nutmeg-invokes` (epic: #2030 residual: injection-stamp
bleed + fragile long-string emission). Binding context: issue #2030
owner adjudication (PR #2032 r2 comment) — the unparseable storm
subclass (medium+ bodies, 3rd+ call position) stays tracked in #2030;
this task ships the shortlisted mitigation: file-mediated message
bodies. Charter update mid-flight (owner ruling 2026-11-15): the change
must be HARNESS-AGNOSTIC BY CONSTRUCTION, pinned through both in-tree
harness adapters.

## The shape of the problem

The fragile emission shape is the long prose string INSIDE a JSON
argument object: the model dies resuming structural emission after it
(Unterminated string / Expected '}' — the 5 live #1849 corruptions, the
#2030 storm subclass). The proven-safe shape is a SHORT path argument:
the board payload tools (bodyFile/planFile under /exchange, design
0070) never corrupt across thousands of calls. So the mitigation is
exactly that pattern applied to the message body — the agent writes the
body to a file (its write tool, long-string-LAST, never corrupts) and
emits `messageFile: "body.md"`.

## Delivered

### `messageFile` on the agent send_message surface

- `cmd/workspace-agentd/send_message_file.go` (NEW):
  `resolveMessageFile` (read under the shared discipline), the
  send-specific not-found hint, `consumeMessageFile` (delete-after-
  success, cleanup_warning fold), `foldSendMessageResultFields`.
- `mcp_server.go` dispatch arm: exactly-one-of admission, the file
  resolve, the message_source annotation, the post-success consume.
- `mcp_tools.go`: the emptiness gate's error now names both body
  spellings (a whitespace-only FILE is not a message either).
- `board_file_params.go`: `boardReadFileField`/`boardFileReadError`
  gained a `notFoundHint` param — the board callers pass their existing
  hint verbatim (byte-identical behavior); messageFile passes the send
  hint (consumed-after-success + session_read-before-retry + /exchange).
  ONE reader, ONE discipline, two retry semantics.

### Conventions carried verbatim from #1837 / design 0070

- `/exchange` canonical; relative paths anchor there (the var-override
  test pattern), absolute pass through.
- Flat contract: nested exchange paths refused at read (a nested file
  could be read but never auto-deleted — the write tool's subdirs lack
  the cross-uid group-write bit).
- loadImages read discipline: O_NONBLOCK open (FIFO wedge), regular-file
  gate, stat AND read caps (TOCTOU), UTF-8 validation.
- 256 KiB cap (`boardPayloadFileMaxBytes` — the established budget).
- Delete-after-success: unlinks once the send is SCHEDULED (the tool's
  success return — the detached POST's documented loss semantics are the
  same as a direct message's); delete failure warns via the result,
  never fails the succeeded send; every FAILED send leaves the file
  (retryable without rewriting).

### Design decisions worth recording

- **Trim discipline (the round-trip contract)**: the file body flows
  into the SAME `strings.TrimSpace` + placeholder gate + compose as the
  inline body. The pin's letter is "byte-identical vs direct message of
  the same body" — one message semantic, the file is a transport. A
  trailing newline from the write tool never sneaks into delivery.
- **Precedence = refusal**: both params present (null included — a
  mention is a mention, the board stray-key posture) refuses loudly;
  neither defers to the shared required-error. No silent winner — the
  schema documents exactly that.
- **Schema order is load-bearing**: `message` stays LAST on the wire
  (#1849); `messageFile` (a short PATH argument) sits with the discrete
  keys. The old "alphabetical order is also correct" coincidence is
  superseded (message < messageFile), noted in the schema comment; the
  order pin enforces the explicit RawMessage bytes.
- **`message_source`**: file-mediated successes carry
  `message_source:"messageFile"` (+ `cleanup_warning` only on delete
  failure); the inline arm is unannotated.

### Harness parity (owner ruling 2026-11-15)

The file mediation ends at the composed text; the same dispatch arm and
send path deliver it for every harness. THREE legs, one pin
(`TestMCPSendMessage_HarnessParity`):

1. opencode leg, end-to-end: file-mediated send through the real
   `callMCPTool` dispatch against the fake opencode seam — the
   sentinel-stamped artifact.
2. pi leg, through the in-tree adapter: `pi.NewAdapter` (pkg/agent/pi)
   over a minimal recorded Transport; `SendAsync(composed)` issues a
   `prompt` command whose message field is the artifact BYTE-FOR-BYTE.
3. structural: a source scan over the send-path files
   (send_message_file.go, mcp_tools.go, mcp_server.go) for the
   harness-predicate vocabulary — zero occurrences (the scan caught the
   author's own COMMENT naming tokens first; comments reworded, the
   scan kept whole-file simple).

## Pins (`cmd/workspace-agentd/send_message_file_test.go`)

- (a) `TestMCPSendMessage_FileBodyByteIdenticalRoundTrip` — hostile
  body (quotes, backslashes, newlines, JSON-lookalike injection
  fragments, unicode, whitespace edges), both spellings, delivered text
  EQUAL including the sentinel line; file consumed after success;
  `message_source` present.
- (b) `TestMCPSendMessage_MessageParamUnaffectedThroughDispatch` — the
  inline arm through the NEW dispatch code; plus the pre-existing
  send_message suites (all green).
- (c) `TestMCPSendMessage_BothBodyParamsRefuseLoudly` — refusal text,
  null-mention included, file NOT consumed, no wire traffic.
- (d) edges: missing-file hint, oversize cap, nested-path flat
  contract, non-UTF-8, whitespace-only body, placeholder-class body
  (file survives), file-survives-failed-send, cleanup_warning on the
  undeletable file (review r1: the mid-POST NON-EMPTY-DIRECTORY swap —
  board_exchange_dir_test.go:114-125's root-proof refusal — replacing
  the uid-dependent chmod-0500 parent, which a root runner's
  CAP_DAC_OVERRIDE would sail through and false-fail the pin),
  wrong-typed/empty messageFile.
- (e) `TestMCPSendMessage_HarnessParity` (above).
- Updated: `TestMCPSendMessage_SchemaInjectedOnly_MessageLast` —
  property set {destination, messageFile, message}, required
  {destination}, wire order [destination, messageFile, message].

## Verification

- `go test ./cmd/workspace-agentd/ -count=1` GREEN (544s full; the two
  `TestSupervisePiSubprocess_*` signal-ladder failures reproduced on
  PRISTINE origin/main in this sandbox — fake-child process-group
  semantics, environment-broken, untouched by this diff).
- `-race` clean on the new pins + the emission-shape/catalog suites.
- `gofmt` clean; `go vet` clean.
- External MCP/REST surfaces untouched (the #1837 ruling: only the
  in-workspace agent resolves a workspace path; their inline surface is
  programmatic, not model-emitted).

## Process

Worktree `/workspace/send-message-file` off origin/main (71cb54f80);
branch `feat/send-message-file`; origin/main merged mid-flight (the
#2043/#2030c/#2063 entries; CHANGELOG conflict resolved keeps-both,
post-merge send_message + strict-board-args suites coexist green —
send_message is not a board tool). AI reviewer rounds per pr-review.yml
until APPROVED; orchestrator merges; `make fmt-check` before push.

Review r1 (head add8b0a01, CHANGES_REQUESTED): ONE finding — the
cleanup_warning pin's chmod-0500 parent is uid-dependent (a root
runner's CAP_DAC_OVERRIDE sails through; false suite-fail on root CI).
Remediated verbatim per the reviewer's pointer: the board precedent's
root-proof refusal (NON-EMPTY-DIRECTORY swap mid-POST,
board_exchange_dir_test.go:114-125) — ENOTEMPTY for every uid. The
reviewer's summary upheld everything else (admission, shared reader,
round-trip, schema order, harness parity).
