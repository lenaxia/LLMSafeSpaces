# Worklog: Dev-preview header configuration — design 0062 implementation

**Date:** 2026-09-29
**Session:** Implement the r30-simplified design 0062 in cmd/workspace-agentd: the `dev_preview_headers` MCP tool, its JSON state, the Rewrite injection, and the `feature_status` entry. Full lane protocol (TDD red-first, PR #1600, iterate with the automated reviewer).
**Status:** In Progress — PR #1600 open, awaiting the automated reviewer

---

## Objective

Deliver design 0062 (refs #1583, owner-simplified r30): the agent sets dev-preview headers directly as literals through a plain agentd MCP tool; agentd injects them at its own forwarding hop. The §8 register/transport machinery is explicitly out of lane.

---

## Work Completed

### The tool (`cmd/workspace-agentd/dev_preview_headers.go`)
- `dev_preview_headers {action: set|clear|list, name, value}` on agentd's `/v1/mcp` surface, registered via the `devPreviewHeadersTool` package var (so the schema pin introspects the exact schema the wire serves).
- §2 validation at tool-call time: `validateDevPreviewHeader` canonicalizes (`textproto.CanonicalMIMEHeaderKey`), enforces ≤128-char token-char names, the reserved denylist (canonical forms — `TE`→`Te`; `Sec-Websocket-` prefix match covers all WS variants), ≤4 KiB values of valid header bytes, and the ≤20-entry cap (overwrites don't consume slots). `X-Forwarded-*`/`Forwarded` deliberately NOT denied (§2/§8: stdlib strips inbound copies before `Rewrite`, so agent-set forward headers deliver — the free forward-mode).
- Empty values rejected — see Key Decisions.

### The state (same file)
- Plain JSON map at `/sandbox-runtime/dev-preview-headers.json` (memory-backed emptyDir class — dies with the pod, survives agentd container restarts; NOT the PVC-durable `/platform` class — source-scan pinned).
- 0600, deterministic temp + `os.Rename` (`sessionstate/cursor.go` mechanics; no fsync — tmpfs, state ephemeral by design).
- Mutations all-or-nothing: build the next map, persist it, swap on success (a failed persist leaves memory+file at the previous state — pinned by occupying the temp name with a directory).
- Boot-load re-validates every entry (a hand-edited file cannot smuggle reserved/oversized entries past the denylist — pinned); a corrupt file loads empty; the next mutation rewrites the file valid.
- Process-wide default via `devPreviewHeadersAtomic` — the file's own `resyncBaseURLAtomic` sharing precedent ("tests mutate it"); tests swap via `swapDefaultDevPreviewHeaders` with cleanup restore.

### The injection (`cmd/workspace-agentd/dev_preview.go`)
- `devPreviewHandler(password, configured *devPreviewHeaderStore)` — nil-safe store param (nil = zero configuration); the loop over `configured.headers()` sits AFTER the existing tunnel-credential strip in `Rewrite`, last-writer over the G34-allowlisted caller content. Both topology modes funnel through this one mount (`server.go` wires `currentDevPreviewHeaders()`).

### `feature_status` (§5) (`cmd/workspace-agentd/feature_status.go`)
- New entry: `{feature: dev_preview_headers, active: N>0, source: "tool" (new enum value), source_detail: "N entries (agent-set literals in agentd memory-backed state)", controllable: true}` — the first controllable entry. The "controllable set is empty today" comments in `feature_status.go` and the feature_status tool description updated to name the one exception.

### Rule 5 fix riding along (commit d7463f59)
- 17 pre-existing `TestCallMCPTool_DevPreviewURL_*` failures when the suite runs inside a real workspace pod: the controller projects `WORKSPACE_DEV_PREVIEW_ENABLED=false`, CI runs with it unset. Verified failing on pristine main v0.34.10 in-pod. Fixed by TestMain normalization (the `podDiskUsage` ambient precedent); the `dev_preview unreported skew` subtest additionally pins its own env via `t.Setenv("")`.

### Salvage audit (recorded per the orchestrator's request)
- The retired predecessor left an untracked `dev_preview_headers_test.go` referencing nonexistent APIs; its `throughPreview` helper was broken (created a probe backend, discarded its server, then proxied to a different port — `<-recv` would block forever; the file never compiled against the tree). Sound arms kept (validation table, cap, clear/list, boot-reload, file shape, source scans); the helper rewritten to proxy to its own probe backend.

---

## Key Decisions

- **Empty values rejected** (not literally in §2's table): an empty value is caller error — `clear` is the removal operation; fail-loud is this surface's lineage (#1561/#1580). Owner-endorsed in the lane charter; flagged to the reviewer in the PR body.
- **No env-override for the state path**: single consumer (one agentd process, both topologies share the default) — the `pkg/agentd/types.go` `LLMSAFESPACES_*_PATH` convention exists for cross-process coordination that doesn't apply. Owner-endorsed.
- **Hand-rolled header-byte check** (`isValidHeaderFieldValueByte`): implements exactly `x/net/httpguts.ValidHeaderFieldValue` semantics (HTAB, SP, 0x21–0x7E, obs-text 0x80–0xFF); the comment states the equivalence. Avoids promoting an indirect dep to direct — no go.mod churn. Owner-endorsed with the equivalence-comment condition (satisfied).
- **`active` semantics for the feature_status entry**: `active = N>0` (headers currently being injected), `source_detail` carries the count — the inventory answers "is header injection in effect", not "does the tool exist".
- **Boot-load discards are silent by design**: the state is advisory tooling state on memory-backed storage (§3 "failure semantics: none"); agentd must not brick on a corrupt cosmetic-state file. The discard semantics are documented at the constructor; the next mutation repairs the file.

---

## Blockers

None. Residual: the pod-tier lifecycle arms (container-restart survival via the emptyDir, suspend/pod-death wipe) are e2e-tier per the design's own §6 tiering; no harness exists for them in `tests/` — documented in the PR for the e2e lane.

---

## Tests Run

Red-first: the full test file was written before any implementation; `go test -run TestDevPreviewHeaders` failed compile-red (all referenced APIs undefined) before the implementation existed.

Targeted (in-pod, `PATH=/tmp/opencode/bin:$PATH GOBIN=/tmp/opencode/bin go test -timeout 300s -run '...' ./cmd/workspace-agentd/`):
- `TestDevPreviewHeaders|TestDevPreview_` — PASS, 34 subtest PASS lines (validation table ×30 incl. denylist ×14 + byte arms; forward-names accepted; entry cap; clear/list; boot reload; boot-load drop-invalid; corrupt-file repair; file shape 0600/JSON/no-tmp-residue; persist-failure atomicity; memory-backed source-scan; nil-store; injection; last-writer; Authorization-still-stripped; X-Forwarded disposition both arms; MCP dispatch set/clear/list/"*"; dispatch errors ×7; registration schema + enum pin; literal-only pin; JSON-RPC roundtrip incl. clear-reverts; feature_status entry).
- `TestMCPHandler_FeatureStatus|TestCallMCPTool_DevPreviewURL|TestSecretsResync_Registration|TestControlPlane` — ok.
- Full-package `go test ./cmd/workspace-agentd/` — attempted twice; the turn died mid-run both times (shared-pod turn-death, not a test failure). Verification basis: the targeted suites above; CI owns the full sweep.

---

## Next Steps

- Iterate with the automated reviewer on PR #1600 until APPROVED; STOP at APPROVED and notify the orchestrator (merge is theirs).
- If the reviewer rules otherwise on empty-value rejection, it is a one-line change plus two table rows.

---

## Files Modified

- `cmd/workspace-agentd/dev_preview_headers.go` (new — store, validation, tool def, dispatch)
- `cmd/workspace-agentd/dev_preview_headers_test.go` (new — all §6 unit/integration arms)
- `cmd/workspace-agentd/dev_preview.go` (store param + Rewrite injection)
- `cmd/workspace-agentd/server.go` (wire the shared default store)
- `cmd/workspace-agentd/mcp_server.go` (registration + dispatch; feature_status description)
- `cmd/workspace-agentd/feature_status.go` (§5 entry, enum + comments)
- `cmd/workspace-agentd/mcp_server_test.go` (inventory pin → six features; skew subtest env pin; arity)
- `cmd/workspace-agentd/dev_preview_test.go`, `cmd/workspace-agentd/control_plane_auth_test.go` (arity only)
- `cmd/workspace-agentd/e2e_test.go` (TestMain env normalization — Rule 5 fix)
- `COORDINATE.md` (lane claim)
