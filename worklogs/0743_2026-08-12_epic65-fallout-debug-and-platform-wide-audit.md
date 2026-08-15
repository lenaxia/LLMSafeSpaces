# Worklog: Epic 65 fallout debug + platform-wide security/billing/frontend audit

**Date:** 2026-08-12
**Issues filed:** #743–#789 (29 issues), 5 comments on #739/#740
**Status:** Investigation complete; 29 issues filed across backend, frontend, security, billing, lifecycle, and infrastructure layers.

---

## Summary

Started as a debug session for "response interrupted / connection timeout" on workspace `1044f4f2` (opencode 1.18.10). Expanded into a comprehensive platform-wide audit spanning:

- Epic 65 wire-shape drift (12 issues, #743–#756)
- Security (5 issues, #757, #762, #763, #764, #765)
- Billing accuracy (5 issues, #758, #759, #766, #767, #768)
- Workspace lifecycle (4 issues, #760, #761, #770, #772)
- Frontend UX (7 issues, #775–#781)
- Frontend/backend contract conformance (8 issues, #782–#789)
- Infrastructure incident (Longhorn stuck volume — operator intervention, no issue)
- SSE event taxonomy diagnostic (comment + gist fixture, no separate issue)

All 29 issues have been filed with GitHub labels (`bug`, `perf`, where available). The `security` label does not exist in the repo — security findings were filed with `bug` only.

---

## Part 1: Initial debug — workspace `1044f4f2` response-interrupted symptom

### Symptom

User reported "response interrupted / connection timeout" on workspace `1044f4f2` (opencode 1.18.10, session `ses_015c5a29effeVY00SdXAQIuRIL`, 65 messages, "E65 Decouple from Opencode").

### Diagnostic path

**1. Pod health confirmed healthy:**
- `/global/health` returned `{"healthy":true,"version":"1.18.10"}`
- opencode RSS 500MB, 37 fds
- One restart 97m prior (memory pressure cycle, self-recovered)

**2. API logs captured the smoking gun:**
```
18:34:52  GET .../sessions/ses_015c5a29...  → 502 {"error":"failed to get session"}  (30ms)
18:34:55  GET .../sessions/ses_015c5a29...  → 502 {"error":"failed to get session"}  (26ms)
```
Two fast 502s (30ms, 26ms) on `GET /sessions/:id`. Meanwhile the `POST /prompt` for the same session streamed for 4m5s before completing 200.

**3. Root cause traced to `proxy_handlers.go:766-768`:**
```go
s, err := h.adapter.GetSession(c.Request.Context(), "", wid, sid)
if err != nil {
    c.JSON(http.StatusBadGateway, gin.H{"error": "failed to get session"})
```
The 502 originated from the adapter path with NO error logging (finding N). The frontend polls `GET /sessions/:id` during active prompt streaming. When it gets the 502, the UI surfaces it as "response interrupted / connection timeout."

**4. Why the adapter path fails (the Summary type mismatch — finding O):**

`ocSession.Summary` is declared as `string` (`translate.go:526`) but opencode 1.18.10 returns `"summary":{"additions":0,"deletions":0,"files":0}` — an **object**. `ParseSessionWire` fails with:
```
json: cannot unmarshal object into Go struct field ocSession.summary of type string
```
Verified with a standalone Go test. The failure is **deterministic** — every 1.18.10 session with git-diff summary data hits it. See issue #743 for the broader wire-shape drift cluster.

### Lessons for debugging workflow

- **Always verify before stating** (Rule 11). I caught myself asserting "the adapter silently truncates" (#751) when in fact the adapter errors loudly with logging. The actual silent path was the parse failure → swallowed error in GetSession (finding N).
- **Capture live wire shapes.** The definitive event-taxonomy capture (gist fixture) reframed #739 entirely. Without it, the other agent would have continued chasing a renamed event string that doesn't exist.
- **Mark assumptions explicitly.** When I told the user "be clear about assumptions," it caught an unfounded inference about the `files:59001` summary → history size (the actual reason was tool outputs, not git-summary).

---

## Part 2: Epic 65 wire-shape drift audit (issues #743–#756)

### Confirmed drift surfaces

| Endpoint / surface | Drifted field | Effect | Issue |
|---|---|---|---|
| `GET /session/:id` (ParseSessionWire) | `Summary string` vs object | Deterministic 502 on every 1.18.10 session | #743 (Finding O) |
| `ocModelRef.Provider` | `json:"provider"` vs `providerID` | Provider silently empty (4 sites) | #743 (F-1) |
| `ocSession.Agent` | field missing | `AgentID` always empty | #743 (F-2) |
| `ocSession.Status` | value type, no omitempty | Latent 502 if 1.18.x sends bare string | #743 (F-3) |
| Reasoning parts (`translate.go:420`) | `p.Reasoning` vs `p.Text` | Chain-of-thought silently empty | #750 |
| SSE event pipeline | `session.next.step.ended` doesn't exist on 1.18.10 | `persistContextFromEvent` never called → context_used NULL | #739 |
| SSE tracker `handleSessionUpdated` | `Cost float64`, `ProviderID` shape | Billing silent-zero on drift | #751 |
| `agentd` parsers | Independent, not synced with platform adapter | Same drift class in a second component | #747 |
| MCP tools (`session_list`, `session_read`) | Return raw opencode bytes | #730/#743 fixes don't apply to MCP callers | #749 |

### SSE event taxonomy — definitive capture

Captured 60s of live SSE events from workspace `1044f4f2` (opencode 1.18.10) during a full prompt→response→idle cycle. This is the missing diagnostic that reframed #739.

**Key findings:**

1. **`session.next.step.ended` does NOT exist on 1.18.10.** Zero occurrences in 60s capture during a complete LLM turn.

2. **Token data lives in `step-finish` PARTS within `message.part.updated` events** — not in a standalone event:
   ```json
   {"type":"message.part.updated","properties":{
     "sessionID":"ses_015c5a29...",
     "part":{"type":"step-finish","reason":"stop","tokens":{
       "total":737221,"input":66,"output":3,"reasoning":0,
       "cache":{"write":0,"read":737152}
     },"cost":0}
   }}
   ```
   The platform already receives these events but nobody looks inside the `part` object for `type === "step-finish"`.

3. **Cumulative tokens are on `session.updated` events** — for context tracking, the cumulative value is more appropriate than the per-step delta.

4. **New event types not handled by the platform:** `session.idle`, `session.diff`, `server.connected`, `server.heartbeat`, `plugin.added` (45 in capture!), `catalog.updated`, `reference.updated`, `integration.updated`. All silently ignored (no metric, no log).

5. **Event type namespacing changed from dots to hyphens** in 1.18.10 — `step-start`/`step-finish` are PART types, not event types. Event types are still dot-separated (`session.updated`, `message.part.updated`).

**Golden fixture:** Captured as a gist for the other agent to use in parser tests:
- `sse_step_lifecycle_1_18_10.jsonl` — full prompt→response→idle cycle

### Notable findings filed separately

**SEV1 #755 — messages disappear on send:**
`SendPromptAsync` always routes through the V2 endpoint (`POST /api/session/:sid/prompt` with `delivery:"queue"`). opencode 1.18.10 admits the prompt but the queue is never drained (the V2 bridge depends on SSE events that drifted). Direct test:
- V2 endpoint: HTTP 200, returns messageID, but message NEVER appears in history.
- V1 endpoint (`POST /session/:sid/message`): HTTP 200, full response in 8s, message persists.

Fix: gate `SendPromptAsync` adapter path behind a V2-available check; fall back to V1 endpoint.

**#746 — adapter 10s hard timeout:**
`newTunedHTTPClient` sets `http.Client.Timeout: 10 * time.Second`. This covers the entire exchange including body read. `adapter.Send` calls `POST /session/:id/message` (synchronous, blocks on LLM completion). Every turn >10s will fail. Latent today (V2 disabled) but activates the moment V2 is enabled.

**#745 — filediff dead wiring:**
The `filediff.Producer` is never constructed in production. `Capabilities()` falsely advertises `CapDiff`. The `git init` in pod_builder is dead infrastructure (cost without benefit). When eventually wired, security gaps exist (no `GIT_CONFIG_NOSYSTEM`, no `core.hooksPath=/dev/null`, no fsmonitor hardening).

### Longhorn infrastructure incident

During the session, workspace `1044f4f2` got stuck in `Creating` after a manual refresh-compute. Root cause: Longhorn volume stuck in `attaching` state after worker-01 went down.

**Why it couldn't self-heal:** Longhorn's volume controller had stale in-memory state. The volume was in an impossible `state=creating` for a 41h-old volume with `robustness=unknown`. The controller's reconciliation loop kept hitting `robustness=unknown` guards that blocked engine start, replica creation, and attachment.

**Fix applied:**
1. Deleted the dead replica on worker-01 (unreachable node)
2. Restarted the Longhorn manager on worker-00 → cleared stale controller state
3. Fresh manager correctly identified the worker-04 replica as healthy, set `robustness=degraded`, started the engine, created a replacement replica on worker-02
4. CSI successfully attached (`SuccessfulAttachVolume` event)
5. Pod progressed through init (132,899 files fsGroup permission change → Active)

**Operational gap filed as #760:** `FailureClassInfrastructure` has `SafeModeAfter=0` meaning Infrastructure failures NEVER escalate to SafeMode. The workspace retries forever (MaxAttempts=0) with 2-minute backoff, never alerting the operator. The Longhorn case would have looped invisibly without manual intervention.

---

## Part 3: Security audit (issues #757, #762, #763, #764, #765)

### CRITICAL: gin TrustedProxies never applied (#757)

The gin engine is created with `gin.New()` (`router.go:328`) and `SetTrustedProxies()` is **never called**. The code at `security.go:190-197` acknowledges this:
```go
// Set trusted proxies - this needs to be done at the engine level, not here
// We'll log a warning instead of trying to set it in middleware
```
But it only LOGS the config — never applies it. Gin's default trusts `0.0.0.0/0` → every `c.ClientIP()` accepts spoofed `X-Forwarded-For`.

**Affected controls:**
- Rate limiting (`middleware/rate_limit.go:103`) — bypassable with header rotation
- Account lockout (`auth.go:1334`) — `lockoutKey(email, clientIP)` includes spoofed IP
- Webhook IP allowlist (`webhook_receiver.go:125`)
- API key CIDR enforcement (`auth.go:851`)

**Fix:** `router.SetTrustedProxies(cfg.TrustedProxies)` after `gin.New()`.

### Lockout TOCTOU race (#765)

`recordFailedAttempt` uses GET→SET instead of atomic INCR. Concurrent brute-force attempts can both read the same count and both write count+1. Combined with #757, brute-force protection is nearly ineffective.

### Recovery-code timing leak (#763)

`ConsumeRecoveryCode` returns instantly for unknown users (no bcrypt burn), unlike `Login` which correctly burns a dummy hash. Leaks user existence and remaining code count.

### Other security findings

- **crypto/rand fallback** (no issue number, noted in audit): `GenerateRandomString` falls back to `time.Now().UnixNano()` (predictable) if `crypto/rand.Read` fails. Should panic instead.
- **SameSite missing** (#764): Session cookie has no `SameSite` attribute. SSO cookie correctly sets `Lax`.
- **agentd workflow/node/execute no auth** (#762): Unauthenticated RCE + SSRF from inside workspace pods.

### Verified SECURE

- JWT `alg=none` rejection, iss/aud enforcement
- Bcrypt cost 12, dummy-hash timing equalization on login
- Recovery codes bcrypt-hashed at rest
- Password change revokes all sessions
- WorkspaceAccessMiddleware verifies ownership (creator + org admin)
- API keys are SHA256-hashed + AES-encrypted at rest
- LLM API keys never touch workspace pods (relay injects `key: "public"`)
- Image pulling webhook-gated with registry allowlist
- Network isolation (default-deny ingress, egress allowlist, DNS-rebinding protection)

---

## Part 4: Billing accuracy audit (issues #758, #759, #766, #767, #768)

### Double-billing after API restart (#759) — HIGH

`sessionTokenSeen`/`sessionCostSeen` are in-process memory only. On API pod restart, they reset. opencode's `session.updated` reports **cumulative** tokens. After restart, the full cumulative input is re-billed.

**Scenario:** Session starts → event bills input=100k → API restart → next event re-bills input=100k. User billed 200k for 100k.

### Stripe export advances cursor on failure (#758) — HIGH

`ExportUsage` advances `billing_export_cursor.last_exported_id` to `maxID` regardless of Stripe API success. The code comment promises a "future reconciliation job" — none exists. Any Stripe hiccup = permanent under-billing for affected users.

### No per-token quota, fail-open, concurrent race (#768)

Three compounding issues:
- No per-token quota (only request-count — a 1M-token request counts as 1)
- Fail-open on DB error (`"Quota check failed, allowing request"`)
- Read-then-write race (no atomic reservation, concurrent requests both pass)

### Other billing findings

- **Metering buffer drops** (#766): 4096-slot channel with non-blocking send. Buffer-full silently drops events with only a warn log.
- **No refund path** (#767): Schema CHECK forbids `quantity < 0`. Users charged for failed turns.

---

## Part 5: Workspace lifecycle audit (issues #760, #761, #770, #772)

### Controller kills in-flight turns (#761) — HIGH

All controller-initiated pod deletions (suspend, restart-gen bump, arch drift, health restart, idle auto-suspend, terminate) call `deletePodByName` with no in-flight turn check. `terminationGracePeriodSeconds: 5` ensures any active LLM turn is killed mid-stream.

The request buffer (30s) only protects in-place opencode restarts, NOT controller-initiated pod deletions (`proxy.go:376-378` explicitly excludes these).

agentd's restart path IS session-aware (`makeSessionAwareRestartDecision`) but this doesn't extend to controller-initiated deletions.

### Infrastructure recovery never escalates (#760) — HIGH

`FailureClassInfrastructure: {SafeModeAfter: 0}` → Infrastructure failures NEVER enter SafeMode. The Longhorn case would have looped forever at 2-minute intervals without manual intervention.

### Stuck finalizer (#772) — HIGH

If PVC `Delete` returns any error other than NotFound, the workspace finalizer is never removed → stuck in Terminating forever. Fix: make PVC delete best-effort (rely on owner-reference GC, which is already set).

### Auto-evict uses wrong timestamp (#770) — MEDIUM

`enforceMaxActiveWorkspaces` orders candidates by DB `UpdatedAt`, not `LastActivityAt`. An actively-used workspace with no recent spec changes gets evicted over a stale one.

---

## Part 6: Frontend audit

### Frontend UX bugs (issues #775–#781)

| Issue | Sev | Finding |
|---|---|---|
| #775 | HIGH | `window.confirm` fail-open — destructive actions bypass confirmation when blocked |
| #776 | HIGH | No stuck-creation timeout/cancel — "Creating" spinner runs forever |
| #777 | MED-HIGH | Streamed content wiped on timeout — partial LLM response vanishes |
| #778 | MED-HIGH | No virtual scrolling — long conversations degrade performance |
| #779 | MEDIUM | Session switch lands at wrong scroll position |
| #780 | HIGH | Deleted/404 workspace → infinite loading spinner |
| #781 | MEDIUM | Session rename failures silently swallowed |

### Frontend/backend contract conformance (issues #782–#789)

| Issue | Sev | Finding |
|---|---|---|
| #782 | HIGH | 202-empty-body breaks JSON parser — suspend/restart/delete silently fail |
| #783 | HIGH | 5 backend message types silently dropped (shell, agent-switch, model-switch, compaction, system) |
| #784 | HIGH | No password-reset or email-verification UI |
| #785 | HIGH | `rotateKey`/`changePassword` call non-existent endpoints |
| #786 | MEDIUM | `session.status` "aborted"/"deleted" events silently dropped |
| #787 | MEDIUM | Workspace list never paginated — >20 workspaces silently capped |
| #788 | MEDIUM | RoleConfig.tools/MCP stripped on edit — data loss |
| #789 | MEDIUM | SendMessageRequest.model silently dropped — per-message model selection non-functional |

### Frontend areas verified CLEAN

- XSS hardening: `rehype-sanitize` on all markdown, safe link rewriting
- SSE robustness: read-timeout, exponential backoff with jitter, AbortController swap
- Deep linking: session ID in URL, bookmarks work
- Theme persistence: localStorage + API sync
- Mobile/responsive: swipeable sidebar, touch targets
- Accessibility: `aria-live` on messages, skip-link
- Code splitting: admin portal lazy-loaded, shiki in web worker

---

## Part 7: Database audit

### SQL injection — CLEAN

All 24 `fmt.Sprintf` calls in SQL construct only `$%d` placeholder indices — never interpolate user input. Developers added `//nolint:gosec` comments documenting this. No injection found.

### Connection handling — CLEAN

All `QueryContext` calls have matching `defer rows.Close()`. All 16 transactions use the `committed := false; defer func() { if !committed { tx.Rollback() } }()` idiom correctly.

### Missing index (#773)

`usage_events` query filters on `event_time` but the index is on `period`. User-facing usage endpoint may be slow. Fix: `CREATE INDEX idx_usage_owner_event_time ON usage_events(owner_id, owner_type, event_time)`.

### Body-size DoS (#771)

~30 handlers use `c.ShouldBindJSON` with no `http.MaxBytesReader` cap. Mitigated by rate limiting + reverse proxy, but no defense-in-depth.

---

## Part 8: Encryption/DEK audit — CLEAN

- AES-256-GCM throughout
- 12-byte random nonce per call
- HKDF-SHA256 for purpose-separated key derivation
- Argon2id for sealed-key KDF
- Fail-closed defaults (missing KEK refuses boot)
- Multiple root-key providers (static, sealed, AWS KMS, GCP KMS, composite)
- Key rotation with multi-version transition window
- DEK cached in Redis with optional AES wrapping

GCP KMS provider has CRC32C integrity checks; AWS KMS provider lacks them (HARDENING).

---

## Complete issue inventory

### Epic 65 drift (12 issues)

| Issue | Title |
|---|---|
| #743 | opencode 1.18.10 wire-shape drift — providerID, Agent, Status |
| #744 | V2 session-queue bridge — spurious wake, in-memory leak |
| #745 | filediff.Producer never wired — dead git init, false CapDiff |
| #746 | Adapter HTTP client 10s hard timeout |
| #747 | agentd maintains independent wire parsers — drift |
| #749 | MCP server tools — no body cap, no auth, bypasses middleware |
| #750 | Reasoning (chain-of-thought) silently dropped |
| #751 | SSE tracker — billing silent-zero, memory leak, reconnect race |
| #752 | Frontend broken on 1.18.10 — reasoning invisible, model badge missing |
| #753 | Secrets rotation can restart mid-turn |
| #754 | session_index channel drops events silently |
| #755 | SEV1: messages disappear — V2 queue never drained |
| #756 | GetHistory fails on large sessions — 16MB cap truncates 94.5MB |

### Non-Epic-65 (17 issues)

| Issue | Title |
|---|---|
| #757 | CRITICAL: gin TrustedProxies never applied |
| #758 | Stripe export advances cursor on failure |
| #759 | Double-billing input tokens after API restart |
| #760 | Infrastructure recovery never escalates to SafeMode |
| #761 | Controller kills in-flight LLM turns |
| #762 | agentd workflow/node/execute — unauthenticated RCE |
| #763 | Recovery-code timing leak |
| #764 | Session cookie missing SameSite |
| #765 | Lockout counter TOCTOU race |
| #766 | Metering Record() silently drops events |
| #767 | No billing refund/reversal path |
| #768 | No per-token quota, fail-open, concurrent race |
| #769 | Relay-router no per-user rate limiting |
| #770 | Eviction uses DB UpdatedAt not real activity |
| #771 | ~30 handlers no body-size limit |
| #772 | PVC delete error wedges workspace in Terminating |
| #773 | usage_events index/query mismatch |

### Frontend (15 issues)

| Issue | Title |
|---|---|
| #775 | window.confirm fail-open on destructive actions |
| #776 | No stuck-creation timeout/cancel |
| #777 | Streamed content wiped on timeout |
| #778 | No virtual scrolling |
| #779 | Session switch lands at wrong scroll position |
| #780 | Deleted/404 workspace → infinite spinner |
| #781 | Session rename failures silently swallowed |
| #782 | 202-empty-body breaks JSON parser |
| #783 | 5 backend message types silently dropped |
| #784 | No password-reset or email-verification UI |
| #785 | rotateKey/changePassword call non-existent endpoints |
| #786 | session.status aborted/deleted silently dropped |
| #787 | Workspace list never paginated |
| #788 | RoleConfig.tools/MCP stripped on edit |
| #789 | SendMessageRequest.model silently dropped |

---

## Cross-cutting themes

1. **Wire-shape drift is systemic.** Five independent parser surfaces (platform translate.go, proxy_events.go, dialect.go, tracker.go, agentd) all independently match opencode-specific strings. None are version-aware. None share fixtures. When opencode changes, each breaks independently and silently.

2. **Silent data loss is the dominant failure mode.** Reasoning content, billing, provider attribution, file diffs, context usage, session status, permission metadata, 5 message types, model selection — all silently dropped on various paths.

3. **Zero observability everywhere.** No counters for unrecognized SSE events, no metrics for parse failures, no warnings on drift. Every drift discovery requires a user report.

4. **Frontend type safety is aspirational, not enforced.** 17 type mismatches found between frontend TS interfaces and backend Go structs. Contract tests cover only 8 of ~40 major types.

5. **The highest-impact finds were NOT in the original issues.** The SEV1 messages-disappearing bug (#755), the SSE event taxonomy discovery (reframed #739), the TrustedProxies bypass (#757), the double-billing (#759), and the 5 dropped message types (#783) were all outside the scope of the original #739/#740 issues.

6. **The codebase is mature in specific areas.** SQL injection discipline is exemplary. Transaction handling is consistent. Encryption is fail-closed with proper key hierarchy. XSS hardening is solid. The bugs cluster in HTTP-layer hardening, wire-shape compatibility, and lifecycle edge cases.

---

## Diagnostic artifacts produced

1. **SSE event taxonomy fixture** — [gist](https://paste.thekao.dev): `sse_step_lifecycle_1_18_10.jsonl` — golden testdata from live opencode 1.18.10 capture showing the full prompt→response→idle cycle. Proves `session.next.step.ended` doesn't exist and token data lives in `step-finish` parts.

2. **Live wire-shape captures:**
   - `GET /session/:id` response (94.5MB history session) — proves the Summary type mismatch
   - `GET /session` list response — proves summary drift
   - SSE event type distribution from 60s capture

3. **Go test verifying the Summary type mismatch:**
   ```go
   body := []byte(`{"id":"ses_test","summary":{"additions":0,"deletions":0,"files":0}}`)
   var s ocSession
   err := json.Unmarshal(body, &s)
   // err = json: cannot unmarshal object into Go struct field ocSession.summary of type string
   ```

---

## Recommendations for the team

### Immediate (1-line fixes)

1. **#757** — `router.SetTrustedProxies(cfg.TrustedProxies)` after `gin.New()`
2. **#782** — `if (res.status === 204 || res.status === 202) return undefined as T;`
3. **#764** — `c.SetSameSite(http.SameSiteLaxMode)` before `SetCookie`
4. **#765** — Replace GET→SET with Redis `INCR`
5. **#763** — Add dummy bcrypt burn on user-not-found path
6. **#786** — Add "aborted"/"deleted" branches to session.status handler
7. **#787** — Add `?limit=100` to workspace list call
8. **#785** — Remove dead `rotateKey`/`changePassword` methods

### Short-term (epic-level coordination)

1. **Wire-shape drift epic** — centralize all opencode wire parsing into one version-aware module with shared fixtures. Eliminate the 5 independent parser surfaces.
2. **Frontend contract test expansion** — extend `contract.test.ts` from 8 to ~40 types. Add a generator that emits fixtures from Go structs.
3. **Billing accuracy epic** — persist dedup state to Redis (#759), implement Stripe reconciliation (#758), add per-token quota (#768), add refund path (#767).
4. **Lifecycle drain epic** — thread active-session state into controller pod-deletion paths (#761), add SafeModeAfter for Infrastructure (#760), fix stuck finalizer (#772).

### Operational

1. **Add Prometheus alerts** for: unrecognized SSE events, parse failures, billing channel drops, stuck-in-Creating workspaces.
2. **Document the opencode 1.18.10 event taxonomy** as a contract reference (the gist fixture is the source of truth).
3. **Add a Longhorn volume-stuck detection runbook** — restart the owning manager to clear stale controller state.

---

## Files touched

None. This was a pure investigation session — no code changes were made. All findings are documented in GitHub issues #743–#789 and the SSE event taxonomy gist.

The fixes belong to the agents working #739, #740, and the new issues filed. This worklog exists to provide them with the diagnostic context they need.
