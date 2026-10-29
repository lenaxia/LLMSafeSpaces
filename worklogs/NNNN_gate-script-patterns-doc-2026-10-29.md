# Gate-script worked patterns: source doc, agentd-image delivery, boot materialization, description pointer

2026-10-29 · branch `gate-script-patterns-doc` · tsk-arctic-wardrobe-verifies

## §1 The problem

The first real inline-gate trigger (a PR-review watcher) took four
attempts. Every failure was the script's EXTERNAL contract, never the
#1812 machinery (flawless): anon GitHub `review_decision` is null
(poll `/pulls/{n}/reviews` instead), states are lowercase,
`urlopen` THROWS on 304, saved ETags can pin stale CDN content forever.
Conclusion (owner direction): agents need worked patterns, lazily,
without prompt bloat.

## §2 The design (locked pre-claim)

1. Source doc at `docs/automation/gate-script-patterns.md` — the truth.
2. Baked into the agentd IMAGE (one release unit; already in every pod;
   carries the MCP tools whose descriptions point at it). Materialized
   at BOOT by init-fs to `/sandbox-runtime/docs/gate-script-patterns.md`
   (idempotent copy — boot-time, not a sync racing pod start). This
   creates the platform→pod file rail. Rejected: Runtime Base (wrong
   layer), Image Factory (ceremony), MCP resources (not universal),
   inline description content (prompt bloat).
3. Discovery: the trigger_create TRIVIAL worked example is REPLACED by
   a one-line pointer at the pod path, in all three description sites
   (agentd catalog, pkg/mcp surface, trigger-body schema blurb) —
   net-neutral or smaller.

## §3 Implementation

- **The doc** (`docs/automation/gate-script-patterns.md`): five
  patterns, each the canonical form tonight validated —
  1. External HTTP poll: fetch → NORMALIZE (`.lower()`; never compare
     against invented constants) → evaluate; the GitHub-anon contract
     (`review_decision` null unauthenticated → poll
     `/pulls/{n}/reviews`, last element's `state`+`submitted_at`;
     lowercase states; the per-IP SHARED 60/h budget and the
     calls-per-fire × fires-per-hour ≤ ~40 cadence rule).
  2. Conditional requests: 304 RAISES `HTTPError` — catch
     `e.code == 304` as the no-change path; CDN-stale bodies make
     saved ETags mask real changes → plain fetches + marker dedup for
     CDN-fronted anon APIs.
  3. State dedup across fires (fires have NO memory): the marker-file
     signature idiom — signature of observed state to a PVC file, skip
     when unchanged; at-most-once relay per state change, tradeoff
     stated.
  4. Failure posture: unhandled exceptions fail CLOSED and count
     toward auto-disable; catch-all → the CHOSEN verdict
     (`proceed=false` quiet — common poller; `proceed=true` fail-open —
     "if I can't verify, wake someone"), choice stated in the handler.
  5. Self-contained action prompts: the LLM leg is a fresh context —
     exact relay text, exact destination, like create_session prompts.
  Plus the full worked example (the PR-review watcher itself) composing
  all five, and a cadence budget note (one call/fire, `*/15` = 4/h).

- **Delivery** (`cmd/workspace-agentd/gate_docs.go`): `go:embed` of a
  byte-identical copy (`gate_script_patterns.md` in the package dir —
  go:embed cannot reach outside it), drift-pinned equal to the source
  by `gate_docs_test.go`. `ensureGateScriptPatternsDoc` materializes to
  `$runtimeDir/docs/` idempotently: identical file → untouched (stable
  mtime, verified by test); stale/absent → atomic rewrite (temp +
  rename, 0644 public data). init-fs step 7 calls it BEST-EFFORT —
  advisory content, bytes live in the binary, a failed copy logs and
  never blocks boot. The image needs no layout change: the Dockerfile
  copies `cmd/` wholesale, so the embedded doc rides the digest-pinned
  binary — "ships in the agentd image" is satisfied at the binary
  layer, one release unit.

- **Pointer, not content** — three sites, all net-smaller:
  - agentd catalog (mcp_server.go trigger_create): the
    `count_open_items` digest example → the pointer sentence (−59).
  - pkg/mcp (workflow_tools.go triggerCreateTool): the `n = 2; n >= 3`
    example → the same pointer sentence (−15).
  - trigger-body schema blurb (InputSchema `trigger` description):
    posture detail condensed + the pointer appended (−13); the
    platform invariant ("Unhandled exceptions count toward
    auto-disable") stays in the main descriptions, which the existing
    pins watch.

## §4 Verification

- `TestGateScriptPatternsDoc_DriftPin_SourceOfTruth` — embed ==
  source of truth (edit one without the other ⇒ red).
- `TestGateScriptPatternsDoc_ContentPins` — every live-fire trap named
  (`review_decision`, `/reviews`, `.lower()`, `submitted_at`,
  `per-IP`, `calls-per-fire`, `e.code == 304`, `stale`, `marker dedup`,
  `NO memory`, `signature`, `at-most-once`, `fail CLOSED`,
  `proceed=false`, `fail-open`, `FRESH context`, `exact relay text`).
- `TestEnsureGateScriptPatternsDoc_MaterializesAndIsIdempotent` —
  lands, identical → no rewrite (mtime-pinned), stale → replaced;
  `_ModeIsPublicData` — 0644.
- init-fs subprocess suite: fresh boot materializes the doc under
  `$runtimeDir/docs/`; the idempotent re-run keeps it (byte-equal).
- Catalog tests: pointer Contains-pinned in BOTH catalogs; NotContains
  pins hold the examples gone (`count_open_items`, `n >= 3`); the
  schema blurb pointer pinned via the schemaDescs seam; all existing
  posture-sentence pins green.
- `go build ./...`, `go test ./cmd/workspace-agentd/... ./pkg/mcp/...`,
  `golangci-lint run` on the touched packages — green, 0 issues.

## §5 Out of scope (deliberate)

Human-facing docs that still carry fuller worked examples
(`docs/api/mcp.md` § Triggers, `README-LLM.md`, the Go SDK doc
comment) are NOT description sites — no prompt cost — and their
examples are the fuller JSON/SDK forms, not the trivial one being
replaced. Left untouched; the doc links from the three agent-visible
sites only.
