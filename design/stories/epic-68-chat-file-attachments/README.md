# Epic 68: Chat File Attachments — Upload, PVC Ingest, and Prompt Reference

**Status:** Shipped — as-built (US-68.1…US-68.6 merged)
**Created:** 2026-08-26
**Tracking:** GitHub issues (epic + US-68.1…US-68.6); this document is the authoritative design and as-built record. Execution status lives only on GitHub — do not duplicate here.

> **Renumbered 67 → 68 (2026-08-27).** Two earlier claimants of "Epic 67" exist: the
> workspace security-review epic (issues #819–850, `[epic-67]` tags) and the dev-preview
> line's code comments (`api/internal/handlers/preview_origin.go`). This epic was numbered
> from `design/stories/` max (66) without cross-checking GitHub issue titles; the collision
> was caught at closure. Living records (this doc, README-LLM, issues #1056–1062, code
> comments, `local/us-68-attachments-e2e.sh`) carry the corrected number. Immutable history
> — merged PR titles, commit messages, and bot-numbered worklog filenames — still says 67
> and is left untouched (worklogs are append-only). Grep accordingly.

---

## Problem Statement

### Current State

Users cannot get files into a workspace except by having the agent create them (tools/git) or via the terminal. The web composer is textarea-only (`frontend/src/components/chat/Composer.tsx`); no upload infrastructure exists anywhere — no multipart handling in the API, no FormData in the frontend client, no upload route in the proxy, no uploads-directory convention on the PVC. The session contract is text-only end to end: `SendPromptAsync` (`api/internal/handlers/proxy_handlers.go:193`) extracts text parts, the adapter `Send(ctx, …, text, opts)` forwards a text string, and the V2 prompt body (`{prompt:{text}}`) actively rejects parts arrays (opencode 1.18.10).

### Desired State

A user (web, REST/SDK, or MCP client) attaches files; bytes land on the workspace PVC under `/workspace/uploads/`; the prompt references them; the agent reads them with its own tools. One upload primitive, one send-path semantic, four client surfaces.

### Framing

We are not building "chat attachments" — we are building a **workspace file-ingest API that the composer happens to surface**. This framing is load-bearing: it decides the seams (below) and keeps the feature agent-agnostic (design/0049; Rule 12).

## Relationship to Existing Subsystems (do not confuse)

- **Session contract** (`pkg/session/part.go`, design/0049): unchanged. 5 part types forever; no `PartFile`. File references ride as composed text.
- **Relay config subsystem** (README-LLM §Relay Config): unrelated — that is `agent-config.json` construction inside the pod. This epic adds one agentd admin endpoint alongside `readyz`/`reload-secrets`.
- **Disk-pressure prompt injection** (`proxy_disk_pressure.go`): sibling text-append feature; coexistence order fixed by fixture (U1.4.6).
- **Epic 65 agent-session contract**: this design deliberately stays *above* the adapter seam. Manifest composition is agent-agnostic text; a future agent with native attachment support may move composition behind the adapter — a second-consumer decision, not now.

## Scope

### In scope

- agentd `PUT /v1/files` ingest endpoint (atomic tmp+rename, sanitize, cap, boot scrub)
- API `POST /api/v1/workspaces/:id/uploads` (streaming multipart, phase + disk gates)
- `files: string[]` on `/prompt`, `/queue`; explicit rejection on V1 `/message`; MCP `session_message(files)` + `workspace_file_upload` tool
- Attachment manifest format v1 (stable API contract, golden-fixture tested) + round-trip parser
- Composer drawer (model + persona move in), "+" attach button, chips, manifest strip on render
- SDK/OpenAPI surface; docs; e2e completion

### Out of scope (with rationale)

- **Vision/multimodal** — showing image pixels to a model needs file parts through the adapter seam; separate, seam-aware feature. Here images are dumb files.
- **Drag-drop + clipboard paste** — follow-ups on the same upload endpoint; not new semantics.
- **Dedup on retry** — new uuid per upload; orphans are ordinary workspace files the user/agent may delete. Dedup machinery is speculative.
- **S3/object-store staging** — external dependency, egress, credentials, and the file never lands "in" the workspace.
- **Session-scoped folders** — sessions are lazy; files may attach before any session exists; uploads are reusable across sessions.
- **Per-tenant PVC quotas** — Epic 43 territory; disk gate + ENOSPC are the guards here.
- **Content sniffing/thumbnails/downloads-browse UI** — UX polish, later.

## Alternatives Considered

| Alternative | Rejected because |
|---|---|
| New `file` part type in the session contract | Contract capped at 5 types forever (design/0049); V2 prompt path rejects parts; all six validated agents consume files via tools, not message payloads |
| opencode native `AttachmentConfig` | Couples deeper to opencode against Epic 65's direction; schema limits (base64) hostile to 25 MiB files; invisible to a second agent |
| API writes directly to PVC | API server has no filesystem access to the PVC (`pkg/agent/adapter.go:26-28`); would require new volume mounts on the API deployment and break statelessness |
| `pods/exec` file transfer (terminal's channel) | exec was a WS-handshake workaround; HTTP upload doesn't need it; exec bypasses agentd's auth/sanitization seam |
| S3 staging + agent fetches URL | See out-of-scope; also adds egress + credentials to the threat model |
| Frontend composes mention text | Hidden-mutation inversion; with API/SDK/MCP as funded consumers the semantics must be platform-owned (Rule 12 second-consumer trigger) |

## Architecture

```
Web (+ picker)  ── multipart ──┐
REST / SDK      ── multipart ──┼─► POST /api/v1/workspaces/:id/uploads   [JWT + WorkspaceAccess;
MCP tool        ── base64 ────┘        streaming, caps, phase+disk gates]
                                        │ Basic auth (per-workspace password)
                                        ▼
                              agentd PUT /v1/files (port 4098)
                              sanitize → <uuid>-<name>.tmp → fsync → rename
                                        ▼
                              /workspace/uploads/<uuid>-<name>   (PVC — persists across suspend/resume)
                                        ▼
       Send (all clients): files: string[] on /prompt, /queue, MCP session_message
                                        ▼
              ComposeAttachmentManifest(text, files) — ONE function, compose-once at acceptance
                                        ▼
                     Adapter.Send(text) unchanged → agent reads files via its own tools
```

## Threat Model

- **Filename injection** — control chars/RTL in names could forge manifest lines or traverse paths. Mitigation: sanitize at both layers (API defense-in-depth, agentd authoritative); sanitization precedes manifest composition (D9).
- **Path forgery via `files[]`** — sender referencing arbitrary workspace paths (`/etc/passwd`, `../secret`). Mitigation: strict `^/workspace/uploads/<uuid>-` shape validation (D8). Note: sender is the workspace owner; path mentions in plain text are already possible — trust model unchanged.
- **Partial-file poisoning** — interrupted upload leaves truncated file; agent reads half a PDF and proceeds. Mitigation: atomic tmp+rename + boot scrub; uploads are atomic or absent (D3).
- **Resource exhaustion** — oversized bodies buffered in API memory; PVC fill. Mitigation: streaming with propagated cancellation, never buffered (API); server-side cap in agentd (never trust the API); disk gate ≥95% + ENOSPC 507.
- **Cross-tenant leakage** — Mitigation: existing auth/access middleware; path namespace is per-workspace PVC; e2e E10.
- **Not a threat:** prompt injection via manifest content (sender = owner, equivalent to typing), zip bombs (nothing is decompressed), retry duplicates (documented behavior).

## Design Decisions

### D1 — agentd-mediated ingest (user mux, validated as-built), not exec, not API-direct, not opencode
The API has no PVC filesystem access. agentd is the platform's own code in the pod; symmetric with `reload-secrets`, which already pushes bytes behind Basic auth. **Correction (US-68.1 validation):** reload-secrets serves on the agentd **user mux (:4097)**, not the admin port 4098 as originally sketched — `cmd/workspace-agentd/server.go:356`. `PUT /v1/files` follows the validated user-mux pattern. Sidecar mode clean-fails (5xx): the sidecar's `/workspace` is read-only.

### D2 — Flat `/workspace/uploads/<uuid>-<name>`, workspace-scoped
Sessions are lazy (files may precede any session); uploads reusable across sessions; visible to terminal/git/agent tools like any workspace file. Cleanup is the user's/agent's — same as any workspace file.

### D3 — Atomic writes
`<uuid>-<name>.tmp` → fsync → `os.Rename` (ConfigWriter pattern); scrub `*.tmp` on boot. Never partial.

### D4 — Caps: 25 MiB/file REST (streamed), 5 MiB decoded MCP, ≤10 files/send
25 MiB via base64 ≈ 33 MB JSON through stdio is transport-hostile → MCP cap lower, tool description points big files to REST/SDK. File-count cap bounds manifest bloat against the ~100 KB prompt cap.

### D5 — Gates on upload: phase Active (409 + phase), disk ≥95% critical (507), unknown disk fails open
Terminal-gate style; disk ratio reuses `proxy_disk_pressure.go` helper semantics.

### D6 — `files` on V2 paths (`/prompt`, `/queue`) + MCP; V1 `/message` rejects with explicit 400
V1 handler proxies the body verbatim — it must not gain rewriting logic; silent drop is worse than explicit rejection.

### D7 — Manifest is API contract, composed by exactly one function
`ComposeAttachmentManifest` in one package (`pkg/session/attachments/`), called at acceptance (compose-once; retries must not double-append). Format v1, golden-fixture tested, additive-only changes; any format change = new version marker + v1 parser support. Three client transports + the frontend history renderer consume it. **As-built (US-68.3):** v1 lines carry `path` + `name` only — no `bytes` attribute. Rationale: D8 makes send-time validation shape-only (no stat), so the composer cannot know file sizes; the illustrative `bytes=` sketch in the original architecture diagram is superseded by the golden fixtures in `pkg/session/attachments/testdata/`. Also as-built: the interaction locked by U1.4.6 is that `/prompt` never receives disk-pressure injection (existing V2 gap, README-LLM §Disk-pressure) — disk gating is upload-side (D5). The "notice first, manifest after" ordering becomes assertable only if that gap is ever closed.

### D8 — Send-time validation is a shape check, not an existence probe
`^/workspace/uploads/<uuid>-`. Deleted file ⇒ agent gets "not found" from its tools — same as any workspace path. No TOCTOU machinery.

### D9 — Sanitize at both layers; sanitization precedes composition
API sanitizes (defense in depth); agentd is authoritative. Prevents forged manifest lines via hostile filenames.

### D10 — Seams respected
Contract unchanged; `Adapter.Send` unchanged; composition above the seam. opencode `AttachmentConfig` unused.

### D11 — Frontend sends `files[]`, never mutates text; strips/parses manifest on render
User bubbles show chips, not manifest lines. Interior forged lines render as plain text.

### D12 — Composer UX: "+" always visible; chevron drawer holds model + persona
Attach is one tap on mobile; selectors move from ChatPage header; drawer state is a persisted user preference, default-collapsed on mobile.

### D13 — Dead `MessagePart.files` removed (types.ts:152)
Two similar-but-different fields, one dead — zero-tech-debt rule.

### D14 — Symlink/tmp squatting defense: `O_EXCL` create (stress-test addition)
The in-pod threat is the agent itself (uid 1000 owns `/workspace`): a compromised agent can pre-create `uploads/<guessed>.tmp` — or a symlink at that path pointing outside the PVC — before an upload lands. Mitigation: agentd creates the `.tmp` with `O_CREATE|O_EXCL` (O_EXCL never follows an existing symlink) and retries with a fresh uuid on `EEXIST`. The API cannot guess uuids; the agent cannot race 128 bits.

### D15 — Compose is idempotent: strip-then-append (stress-test addition)
`ComposeAttachmentManifest` removes any pre-existing **trailing** v1 manifest block from the input text before appending the new one. Property: `compose(compose(t, f), f) == compose(t, f)` for all t, f. This kills the double-append class entirely (retry storms, client resubmission, a user pasting a copied block) rather than enumerating its sources. Duplicates within `files[]` and empty-string entries are rejected 400 (explicit over dedup-silently).

### D16 — Gate order is contract: auth → access → phase → disk → cap (stress-test addition)
Asserted by test (U1.2.16), not incidental. A `401` must never leak phase info; a disk-gated request must not reach agentd; a cap violation on a non-Active workspace reports phase, not size.

### D17 — Send blocked while uploads in flight (frontend, stress-test addition)
The composer disables send while any chip is mid-upload (uploading chips are visually distinct from attached chips). Rationale: sending without the settled path would reference a file that may 404 at the agent; sending after a failed upload is the user's explicit choice via chip retry/remove.

### D18 — MCP base64 tolerates wrapped input (stress-test addition)
Base64 with embedded newlines/whitespace (common from CLI-wrapped clients) is normalized before decode; strictly malformed input after normalization → tool error. Never a panic.

### D19 — Orphan-on-late-crash is accepted behavior (stress-test addition)
If agentd crashes between rename and response, the file exists but the client sees 5xx. The orphan is an ordinary workspace file (D2): no journal, no cleanup job. Retry creates a new uuid. Accepted and documented rather than engineered around.

## Validated Assumptions

| Assumption | Evidence |
|---|---|
| API has no PVC filesystem access | `pkg/agent/adapter.go:26-28`; volume mounts only on pod spec (`controller/internal/workspace/pod_builder.go`) |
| V2 prompt body rejects `parts` | opencode 1.18.10; `pkg/agent/opencode/client_v2_test.go:84-101` (F18) |
| No multipart infra exists repo-wide | survey: zero `FormFile`/`ParseMultipartForm`/`upload` hits in `api/`, `cmd/`, `pkg/`; frontend has no FormData usage |
| agentd admin port is the established byte-entry channel | `POST /v1/reload-secrets` over 4098 with Basic auth (`pkg/agentd/types.go:71,76`) |
| Existing body caps to respect | proxy 10 MB generic (`proxy.go:404-411`); prompt ~100 KB (`proxy_handlers.go:203`); manifest must fit the latter |
| `MessagePart.files` is dead code | frontend survey: declared `types.ts:152`, never populated, dropped by `extractPromptText` |
| Frontend selectors are self-contained movable components | `ModelSelector.tsx`/`RoleSelector.tsx` render at `ChatPage.tsx:1053-1058` with no header-local state entanglement |

## Edge Cases

- Client disconnect mid-stream → context cancellation propagates → agentd aborts `.tmp` (I3)
- agentd briefly down (pod restarting, phase still Active) → 502, no panic (I5)
- Upload in flight while workspace suspends → clean failure, no partial (I9)
- Filename sanitizes to empty → 400, nothing written (U1.1.14)
- User text + manifest exceeds prompt cap → 413/400 at send (U1.3.11)
- Disk-pressure notice and manifest both active → order fixture-locked: notice first, manifest after (U1.4.6)
- `clientMessageID` retry → deterministic composed text, single manifest (U1.4.5)
- Forged manifest line inside user text → parser consumes only the trailing block (U1.3.10/U1.6.10)
- In-pod adversary pre-creates `.tmp` or symlink at a guessed upload path → `O_EXCL` + fresh-uuid retry (D14, U1.1.15/16, I13)
- Caller text already ends with a manifest block (copied/replayed/forged) → strip-then-append idempotency (D15, U1.3.14, U1.5.10)
- agentd crashes between rename and response → orphan file, client 5xx, retry = new uuid — accepted (D19)

## Non-Functional Requirements

- **Security:** no filenames/paths/bytes logged beyond necessity (filename is untrusted input); agentd enforces cap independently of API; auth on both hops.
- **Performance:** API memory bounded — 25 MiB streams forwarded chunked, never buffered (U1.2.3); no per-request allocations of the body.
- **Robustness:** atomic-or-absent uploads; cancellation end-to-end; `-race` clean under 32 parallel uploads (U1.1.12).
- **Compatibility:** manifest v1 additive-only; SDK regeneration required but non-breaking for existing callers.

## Observability

- Upload counters (accepted/rejected by reason: cap/phase/disk/auth) on the API Prometheus surface
- agentd: per-endpoint error counter for `/v1/files` (write failures, cap hits, scrub count on boot)
- No new dashboards required for v1; counters suffice for alert wiring later

## Stories

Execution status lives on GitHub (epic issue + US-68.x sub-issues). Story list with scope and acceptance criteria is maintained there; this section records the split for the as-built record:

- **US-68.1** — agentd file-ingest endpoint (`PUT /v1/files`)
- **US-68.2** — API upload route (streaming multipart + gates)
- **US-68.3** — Attachment manifest contract + send-path integration
- **US-68.4** — MCP upload tool + `session_message` files param
- **US-68.5** — Frontend composer drawer + upload UX + manifest strip
- **US-68.6** — Specs, SDKs, docs, and e2e completion

## Dependency Graph

```
US-68.1 ──► US-68.2 ──► US-68.4
   │           │
   │           └────────► US-68.5 (needs US-68.3 format locked)
   └──► US-68.3 ──► US-68.4, US-68.5
US-68.1..67.5 ──► US-68.6
```

US-68.1 and US-68.3 can start in parallel; US-68.2 needs 67.1; US-68.5 needs 67.3's format locked before the TS parser is written.

---

## As-built deviations

The shipped code matches D1–D19 except where noted here (both corrections
are already folded into the decision text above):

1. **agentd port (D1):** `PUT /v1/files` serves on the **user mux (:4097)**,
   symmetric with reload-secrets — not the admin port 4098 sketched in the
   architecture diagram. **Sidecar mode clean-fails (5xx):** the sidecar's
   `/workspace` mount is read-only (`controller/internal/workspace/agentd_sidecar.go`);
   uploads are supported in single-container mode. A control-socket write
   op for sidecar mode is a tracked follow-up. `local/us-68-attachments-e2e.sh`
   gates on this and asserts the clean-fail when sidecar mode is detected.
2. **Manifest attributes (D7):** v1 lines carry `path` + `name` only — the
   illustrative `bytes=` sketch was dropped because send-time validation is
   shape-only (D8). The golden fixtures in `pkg/session/attachments/testdata/`
   are the authoritative format.
3. **SDK shape correction (US-68.6):** reconciling the spec surfaced that all
   four SDKs sent dead prompt fields (`message`/`content`) — the live handler
   extracts text from `parts`. US-68.6 fixed the SDKs and the spec to the
   as-built `{parts, files, clientMessageID, model}` body.

**E2E coverage as-built:** E1/E5/E6 (browser), E3/E4 (browser, stubbed
backend — the pod-side half of E3 is covered by the U1.1/U1.2 Go suites),
E7 (golden prompt bytes), E8 (external stdio MCP client), E9 (SDK wire
tests + `make sdk-check` + sdk-contract CI), E12 (README/golden fixture
consistency) run in CI. E2/E10/E11 are cluster-only (`local/us-68-attachments-e2e.sh`,
wired into e2e-nightly; rows execute fully only in single-container mode —
see deviation 1).

---

## Test Plan

Tiers follow Rule 0 (TDD — tests written first, per story). Every scenario is mandatory for story completion; GitHub story issues reference these IDs.

### 1. Unit tests

#### 1.1 agentd file endpoint — `PUT /v1/files` (US-68.1)

| # | Scenario | Expect |
|---|----------|--------|
| U1.1.1 | Happy path: valid auth + filename + body | 201 `{path,name,size}`; bytes on disk sha256-identical to request body |
| U1.1.2 | Returned path is `/workspace/uploads/<uuid>-<sanitized>`; original name echoed in `name` | yes |
| U1.1.3 | Filename sanitization table: `../../etc/passwd`, `/abs/path`, `..`, leading-dot names, `report.pdf\n[llmsafespaces:attachment …]`, `\r`, `\x1b`, RTL override (U+202E), quotes, trailing spaces/dots, pure-unicode name, 201-byte name, empty name | traversal flattened to basename; control chars + RTL stripped; >200 bytes truncated/rejected; empty → 400; unicode preserved |
| U1.1.4 | Size cap boundary: exactly 25 MiB accepted; 25 MiB + 1 rejected 413; rejection leaves no file AND no `.tmp` |
| U1.1.5 | Streaming: body larger than agentd's read buffer arrives via pipe (`io.Pipe`, slow writer) — succeeds without OOM | yes |
| U1.1.6 | Atomicity fault injection: error mid-write (failing writer / short read) | no final file; `.tmp` removed; 5xx to client |
| U1.1.7 | Rename failure (pre-created collision target) | error; `.tmp` removed; original target untouched |
| U1.1.8 | Boot scrub: pre-create `a.tmp`, `b.tmp`, `c-<name>` → start agentd | both `.tmp` gone; `c-<name>` untouched |
| U1.1.9 | ENOSPC (tmpfs-filled dir) | clean error; `.tmp` removed |
| U1.1.10 | Auth: missing Basic auth / wrong password | 401; nothing written |
| U1.1.11 | `uploads/` auto-created idempotently (missing → created; exists → ok); modes dir 0755, file 0644 | yes |
| U1.1.12 | Concurrency: 32 parallel uploads of distinct names | all 201; 32 distinct uuid paths; no corruption (hash each) |
| U1.1.13 | fsync-before-rename ordering (fault-injection or code-structure assertion) | fsync precedes rename |
| U1.1.14 | Filename that sanitizes to empty (all control chars) | 400, nothing written |
| U1.1.15 | Symlink squat: `.tmp` path pre-created as symlink pointing outside uploads dir → write attempt | `O_EXCL` fails, retry with new uuid; symlink NOT followed; nothing written through it |
| U1.1.16 | `.tmp` squat (plain pre-created file at guessed path) | `EEXIST` → fresh uuid retry → success |
| U1.1.17 | Same filename uploaded twice sequentially | two distinct uuid paths; no overwrite |
| U1.1.18 | Slowloris: body writer stalls (e.g. 1 byte/10s) | write deadline aborts, `.tmp` removed, 5xx |
| U1.1.19 | Filename only whitespace after sanitize | 400, nothing written |
| U1.1.20 | Response leaks only final path — no `.tmp` path, no internal paths, in success or error bodies | verified |
| U1.1.21 | fsync failure injection (failing writer on Sync) | error; `.tmp` removed; no final file |

#### 1.2 API upload handler (US-68.2)

| # | Scenario | Expect |
|---|----------|--------|
| U1.2.1 | Happy path multipart, one file field | 201 `{path,name,size}` matching fake agentd's response |
| U1.2.2 | Forwards Basic auth + correct `PUT /v1/files` target (fake agentd asserts headers) | yes |
| U1.2.3 | Streaming: 25 MiB body forwarded without full buffering (instrumented reader receives chunks) | no pre-read |
| U1.2.4 | Cap enforcement API-side when `Content-Length` known: over-cap rejected locally (413) without dialing agentd | yes |
| U1.2.5 | Cap when chunked (unknown length): `LimitReader` cut at cap+1 → 413; agentd `.tmp` cleaned | yes |
| U1.2.6 | Client disconnect mid-stream → context cancelled → agentd body closed (fake agentd observes) | propagated |
| U1.2.7 | Phase gate table: all 9 CRD phases | only `Active` passes; others 409 with `phase` in error body |
| U1.2.8 | Disk gate: ratio ≥0.95 → 507 fail-closed; 0.949 passes; `totalBytes==0` → fail-open pass | yes |
| U1.2.9 | agentd error mapping: conn-refused → 502; agentd 413 → 413; agentd 500 → 502; timeout → 504 | mapped |
| U1.2.10 | Malformed multipart: no file part; wrong field name; two file parts | 400 (two-parts: reject, documented) |
| U1.2.11 | API-side filename sanitization (same hostile table as U1.1.3) applied before forwarding | sanitized |
| U1.2.12 | Method wrong (GET/PUT on route) | 404/405 per gin conventions |
| U1.2.13 | Empty filename in part (`filename=""`) | 400 |
| U1.2.14 | Content-Length spoof: claims < cap, streams > cap | `LimitReader` cut at cap+1 → 413 |
| U1.2.15 | CRLF in multipart `Content-Disposition` filename | sanitized to single line before forwarding (no header smuggling) |
| U1.2.16 | Gate order: non-authed + non-Active + full-disk + oversize simultaneously | 401 → (authed) 409 → (Active) 507 → (not full) 413, in that order; never skips ahead |
| U1.2.17 | Wrong content type (`application/json` on upload route) | 415 |
| U1.2.18 | Non-file form fields alongside the file part | ignored (documented); upload proceeds |
| U1.2.19 | agentd returns garbage/non-JSON | 502, no panic, no body leak |
| U1.2.20 | Workspace deleted mid-request (cache says Active, K8s says gone) | 404 from access re-check; no agentd call |
| U1.2.21 | Metrics: counters emitted per rejection reason (auth/phase/disk/cap/agentd-error) and per success | asserted via test registry |
| U1.2.22 | Route participates in existing rate-limiting middleware stack | asserted |

#### 1.3 Attachment manifest composer + path validation (US-68.3)

| # | Scenario | Expect |
|---|----------|--------|
| U1.3.1 | Golden fixtures: 0, 1, 3 files → byte-exact manifest block (format v1) | locked |
| U1.3.2 | 0 files | text returned byte-identical, no blank line added |
| U1.3.3 | Empty text + files | manifest block only |
| U1.3.4 | Order: 3 files in given order | preserved |
| U1.3.5 | Hostile name (newline/quote — pre-sanitized upstream) passed anyway | composer emits nothing that breaks line structure (defense in depth) |
| U1.3.6 | Path validation table: valid `^/workspace/uploads/<uuid>-x` ×3 pass; `/workspace/uploads/../secret`, relative, `/etc/passwd`, ``, non-uuid prefix, `..` segment | all but first form → 400 |
| U1.3.7 | 11 files → 400; exactly 10 pass | yes |
| U1.3.8 | Round-trip parser: composed block (mixed with user text) → `[]Attachment{path,name,bytes}` | exact |
| U1.3.9 | Parser: no block in text → empty, no error | yes |
| U1.3.10 | Parser: forged `[llmsafespaces:attachment …]` inside user text mid-message | parser only consumes the trailing block; interior lines remain text |
| U1.3.11 | Total-length accounting: user text + manifest > 100k chars → 413/400 per existing prompt cap | enforced at send handler |
| U1.3.12 | Duplicate paths within `files[]` | 400 (explicit rejection, D15) |
| U1.3.13 | Empty-string / whitespace-padded entries in `files[]` | 400 |
| U1.3.14 | Idempotency property: `compose(compose(t,f),f) == compose(t,f)` — including t that already ends with a v1 block, and t with a *forged* trailing block | strip-then-append holds (D15) |
| U1.3.15 | Name containing `"` or `\` post-sanitize | composer percent-escapes/strips so manifest line structure is unbroken (defense in depth) |
| U1.3.16 | User text ending in trailing newlines (0, 1, 3) | blank-line normalization deterministic — golden fixtures lock each |
| U1.3.17 | Text at exactly the cap boundary: passes alone, fails with 1-file manifest | boundary asserted |
| U1.3.18 | Parser: block followed by trailing newline(s) vs not | both parse; trailing whitespace tolerated |
| U1.3.19 | Parser: block with unknown/newer version marker or unknown attributes | treated as plain text (forward compatibility) |
| U1.3.20 | Unicode filenames round-trip through compose → parse | exact |

#### 1.4 Send-path service wiring (US-68.3)

| # | Scenario | Expect |
|---|----------|--------|
| U1.4.1 | `/prompt` with `files[]` | dispatched text = user text + manifest; `model` passthrough unaffected |
| U1.4.2 | `/queue` with `files[]` | enqueued entry carries composed text once |
| U1.4.3 | Compose-once on retry: outbox entry persisted after composition; simulated 503 retry re-dispatches stored text | manifest exactly once |
| U1.4.4 | V1 `/message` with `files` | explicit 400 (`files not supported on this route`) |
| U1.4.5 | `files[]` + `clientMessageID` idempotency retry | same composed text (deterministic) |
| U1.4.6 | `files[]` + disk-pressure notice both active | both appended; order: disk notice first, manifest after — fixture-locked |

#### 1.5 MCP tools (US-68.4)

| # | Scenario | Expect |
|---|----------|--------|
| U1.5.1 | `workspace_file_upload(workspace_id, filename, content_b64)` happy path | tool result `{path,name,size}`; upload service called |
| U1.5.2 | Invalid base64 | tool error result (not server panic) |
| U1.5.3 | Decoded > 5 MiB | tool error naming the cap + "use REST/SDK" |
| U1.5.4 | Missing `workspace_id` / `filename` | tool error |
| U1.5.5 | Hostile filename | sanitized identically to REST |
| U1.5.6 | `session_message` with `files[]` | same validation + composer as REST (shared code path) |
| U1.5.7 | `session_message` `files` + oversized text | existing `maxMessageSize` accounting includes manifest |
| U1.5.8 | Base64 with embedded newlines/whitespace (CLI-wrapped) | normalized, decodes, succeeds (D18) |
| U1.5.9 | Base64 empty string / content missing | tool error |
| U1.5.10 | `session_message` where caller's text already ends with a (possibly forged) v1 block | compose idempotency (D15/U1.3.14) applies on the MCP path too — single block dispatched |
| U1.5.11 | Upload to non-Active workspace via MCP tool | tool error naming the phase (not a raw HTTP error passthrough) |
| U1.5.12 | Concurrent MCP `workspace_file_upload` calls (4 parallel) | all succeed; distinct uuid paths |
| U1.5.13 | Huge non-base64 arg (e.g. 6 MB of `z`) | tool error before decode (cap checked on input length); no server OOM |

#### 1.6 Frontend unit/component — vitest (US-68.5)

| # | Scenario | Expect |
|---|----------|--------|
| U1.6.1 | Chevron toggles drawer; state persisted via user-preferences API; mobile viewport (375px) defaults collapsed | yes |
| U1.6.2 | Model + persona selectors render inside drawer; same API calls as header versions (reused components) | yes |
| U1.6.3 | "+" opens native picker; selected file → upload API called once; chip shows name + human size | yes |
| U1.6.4 | Upload failure → chip error state + toast; retry/remove offered | yes |
| U1.6.5 | Chip removed → excluded from `files[]`; send works with zero chips | yes |
| U1.6.6 | 11th file blocked with toast (client-side mirror of server cap) | yes |
| U1.6.7 | Send payload: `{parts:[{type:"text",…}], files:[…], …}` — text NOT mutated client-side | payload snapshot |
| U1.6.8 | Busy-queue path (session busy) carries `files[]` | yes |
| U1.6.9 | User bubble render: trailing manifest block → stripped; chips rendered via parser | yes |
| U1.6.10 | Interior forged line renders as plain text (not chip) | yes |
| U1.6.11 | Dead `MessagePart.files` removed; typecheck green; no usages remain | yes |
| U1.6.12 | Drawer open state does not leak across workspaces/sessions (scoped setting) | yes |
| U1.6.13 | Upload in flight while send clicked | send disabled until uploads settle (D17); uploading chips visually distinct |
| U1.6.14 | File-picker cancel (no selection) | no-op, no chip, no upload call |
| U1.6.15 | Multi-select: 5 files picked at once | 5 chips; client cap (10) enforced across the batch |
| U1.6.16 | Same file attached twice | two chips, two uploads (no client dedup — documented) |
| U1.6.17 | Session switch (same workspace) mid-pending-chips | chips persist (workspace-scoped); workspace switch clears pending chips |
| U1.6.18 | Upload error → retry chip → new upload (new uuid, no stale path reuse) | yes |
| U1.6.19 | History entry that is ONLY a manifest block (no prose) | renders chips, empty text; no crash, no empty bubble artifact |
| U1.6.20 | Chevron a11y: `aria-expanded`, keyboard operable; chips removable via keyboard | yes |
| U1.6.21 | Text field retains content while drawer toggles and chips change (no state clobber) | yes |

### 2. Integration tests

| # | Scenario | Layers | Expect |
|---|----------|--------|--------|
| I1 | Router: `POST /uploads` behind AuthMiddleware + WorkspaceAccessMiddleware — no JWT → 401; other user's workspace → 404 | router + fakes | enforced |
| I2 | Full vertical in-process: real upload handler → real agentd `PUT /v1/files` handler (httptest, temp dir as `/workspace`) → file on disk | handler + agentd | bytes identical; uuid path |
| I3 | Cancellation: client aborts mid-25MiB stream; real agentd handler under test | handler + agentd | upstream write aborted; no file; no `.tmp` residue |
| I4 | Concurrent: 8 parallel uploads, mixed sizes incl. boundary | handler + agentd | all succeed; distinct paths; hashes correct |
| I5 | agentd down (pod restarting): upload → 502 + phase still Active | handler + blackhole | 502, no panic |
| I6 | Upload → `/prompt` with returned path: fake opencode asserts exact dispatched text = fixture | handler chain | fixture match |
| I7 | Outbox/queue persistence: `files` on `/queue`; retry after simulated agent 503 | handler + miniredis | manifest once (ties to U1.4.3) |
| I8 | Disk-pressure integration: CRD status stubbed to 96% → upload 507; send path unaffected | services | gated |
| I9 | Phase transition race: upload in flight while workspace suspends (agentd closes) | handler | clean failure, no partial file |
| I10 | MCP server via mcp-go test client (stdio): `workspace_file_upload` → `session_message(files)` → fake agent asserts manifest | pkg/mcp + fakes | full path |
| I11 | Playwright/MSW component flow: attach → chip → send → payload shape; strip-on-render | frontend | yes |
| I12 | SSE/history round trip: fake agentd history contains manifest text; transform strips | frontend + fakes | yes |
| I13 | Symlink attack vertical: in-pod adversary pre-creates `.tmp` symlink → API upload → agentd | handler + agentd | `O_EXCL` path taken; nothing written outside uploads dir |
| I14 | Retry storm: same `clientMessageID` retried 5× against flaky agent (alternating 503/success) | handler chain | exactly one manifest; no duplicate files referenced |
| I15 | Gate-order integration: layered failure states on a single workspace | router + services | response reflects first-failing gate (D16) |

### 3. E2E tests (kind cluster via `local/`, Playwright via `frontend/tests/e2e/`, stub-agent harness in `tests/`)

| # | Scenario | Expect |
|---|----------|--------|
| E1 | Full happy path: browser attach `notes.txt` → send "read the attached file and quote its first line" → agent Tool part reads the file → answer quotes line; user bubble shows chip, no manifest text | green |
| E2 | Persistence: upload → suspend → resume → file present (terminal `cat` via existing WS path) | green |
| E3 | Oversize: 26 MiB file → friendly composer error; `ls /workspace/uploads` in pod shows nothing | green |
| E4 | Suspended workspace: upload attempt → 409 surfaced in UI with phase hint | green |
| E5 | Mobile viewport 375×812: drawer collapsed by default; chevron opens; model + persona selectable; chips do not overflow composer | green |
| E6 | Chip removed before send → prompt reaches pod without manifest (stub agent asserts) | green |
| E7 | Prompt-bytes golden: stub runtime agent echoes dispatched prompt; CI compares byte-exact against v1 fixture (locks D7 cross-release) | green |
| E8 | MCP external: stdio MCP client (CI harness) against deployed API — upload base64 + `session_message(files)` | green |
| E9 | SDK codegen: regenerated SDKs compile and expose `uploads` + `files` (sdks CI job) | green |
| E10 | Multi-tenant: two users, two workspaces, simultaneous uploads — no cross-workspace path leakage | green |
| E11 | Chaos: pod killed mid-upload (upload API call in flight) → client sees clean 5xx; pod restarts; retry succeeds; exactly one intact file on disk (no `.tmp`) | green |
| E12 | Docs-consistency: README-LLM manifest snippet byte-matches the golden fixture (CI grep against `testdata/`) — prevents doc/format drift | green |

### 4. Non-functional / regression (all stories)

- `go test -race -count=1` clean; full `make test && make lint && make build`
- No filename/path/size logged beyond needed; filenames treated as untrusted in logs
- API memory bounded during 25 MiB streams — enforced by review + U1.2.3
- repolint/gofmt/goimports/golangci-lint clean; worklog per session; Rule 11 adversarial review documented per story PR
