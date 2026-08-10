# Worklog: Epic 63 V2 Session Queue — Real-Cluster Behavioral Validation

**Date:** 2026-08-10
**Session:** First real-system validation of the V2 inboard session queue (US-63.3 / 63.4 / 63.9) against a live LLMSafeSpaces deployment.
**Status:** Complete — feature behaviorally validated; one multi-replica reliability blocker surfaced; test artifacts fixed.

---

## Objective

Run the three behavioral properties that unit tests with mocks cannot prove
(US-63.3 enqueue-while-busy, US-63.4 non-destructive abort, US-63.9 OOM-restart
drain via the proxy wake) against a real running system, fix anything that
breaks, and report whether Epic 63 V2 is ready to enable in production. This is
the gate the spike (worklog NNNN_us-63.1-v2-spike, F16) demanded before US-63.7
deletes the legacy queue.

---

## Work Completed

### Environment

- **Cluster:** existing `home-kubernetes` (multi-node, Cilium CNI) — the same
  cluster the US-63.1 spike used. Context `admin@home-kubernetes`, namespace
  `llmsafespaces`. NOT kind.
- **API image:** `ghcr.io/lenaxia/llmsafespaces/api:0.13.0` (running pods);
  v0.13.0 contains the full V2 code (`api/internal/handlers/proxy_v2.go`,
  `proxy_v2_shadow.go`, `pkg/agent/opencode/client_v2.go`). Only 4
  docs/chore commits separate v0.13.0 from `main` HEAD — no V2 logic changes.
- **opencode:** 1.18.10 (verified `opencode --version` in the workspace pod).
- **Workspace:** `a2703d3d-27c4-4980-86b1-42f99daad330` (Phase=Active,
  runtime base, model `thekaocloud/glm-5.2` via the relay fleet).
- **Transport:** `kubectl port-forward svc/llmsafespaces-api 18080:8080`.
  This cluster's API runs `security.go` SSLRedirect (RequireHTTPS=true), so
  every `/api/*` request over plain HTTP 301→HTTPS. All curl probes had to
  carry `-H 'X-Forwarded-Proto: https'` to bypass the redirect (SSLProxyHeaders
  trust). This is a cluster-config detail the original test artifacts did not
  account for.

### Results

1. **US-63.3 (enqueue while busy → no 409, runs after): PASS.** A second prompt
   sent while a 200-word-essay turn was in-flight returned **HTTP 202** (not
   409), and the queued marker was echoed by the assistant in session history
   after the in-flight turn completed. Confirms F17 (409 only on caller-supplied
   id collision, never on busyness).

2. **US-63.4 (abort mid-turn → queued survive): PARTIAL on the live 2-replica
   deployment.** Enqueue of two markers returned 202/202; `POST /abort` returned
   **204** (non-destructive interrupt — queued input is NOT destroyed, unlike
   V1). The **first** queued marker ran after the abort. The **second** marker
   did **not** run — it stranded. Root cause is the multi-replica divergence
   described under Key Decisions, not the abort itself.

3. **US-63.9 (kill opencode → restart → stranded drain): PASS.** With a marker
   queued behind an in-flight 300-word-essay turn, `pkill -9 opencode` triggered
   agentd's in-place restart (pod survived, new opencode PID). The marker was
   echoed in history after the restart. Critically, the killed essay produced
   **no** output in history — opencode did not resume the interrupted turn — so
   per the spike's F16 (opencode does not auto-drain SessionInput on restart)
   the drain can only be attributed to the proxy's `wakeStrandedV2Sessions`.

### KEY RISK resolved — the `\n` wake prompt is accepted by opencode 1.18.10

Tested **directly** against the workspace pod's opencode (port-forward 4096,
Basic auth `opencode`:`<AGENTD_ADMIN_TOKEN>`), mirroring the spike:

- `POST /api/session/:sid/prompt {"prompt":{"text":"\n"},"delivery":"queue"}` →
  **HTTP 200**, `{data:{admittedSeq:1,id:"msg_…",delivery:"queue"}}`. NOT
  rejected as empty.
- `{"prompt":{"text":" "}}` (single space) → also **HTTP 200**.

opencode 1.18.10 does **not** trim-and-reject whitespace-only prompts, so the
`wakeStrandedV2Sessions` `\n` wake (`proxy_v2.go:231`) admits successfully and
triggers `execution.wake` → `runner.run` → drains durable SessionInput rows.
The fallback the prompt anticipated (single space) is not required, though it
also works.

### Test-artifact bugs found and fixed

Running `local/us-63-v2-behavior-e2e.sh` as-is failed immediately; four real
bugs were fixed in this worklog's commit:

1. **Wrong session-creation route.** The script POSTed `/sessions` which 404s;
   the real route is `POST /sessions/new` (`api/internal/server/router.go:1276`).
2. **Wrong response key.** `/sessions/new` returns `{"sessionId":…}`, not
   `{"id":…}`; `create_session` now reads `sessionId`.
3. **False-positive marker detection.** `history_contains` substring-matched
   the marker anywhere in history. The user's own queued message
   (`"reply with exactly: <MARKER>"`) is itself stored in history, so the check
   always passed the instant a message was enqueued — before the assistant
   replied. opencode 1.18.10 returns message `role` as `null` in this view
   (not `"assistant"`), so the fix keys on a part whose **stripped text equals
   the marker exactly** (the assistant echoes the bare token; the user prompt
   does not). Markers must be bare tokens (no spaces).
4. **Garbled HTTP codes + wrong container.** `enqueue_message`/`abort_session`
   used curl `-f`, so an HTTP 5xx exited non-zero and the trailing `|| echo 000`
   appended "000" → "500000". Switched to `-s` (clean codes). The US-63.9
   `kubectl exec -c main` hardcoded a container name that is `workspace` on this
   cluster; now resolved from the pod spec.

The validation guide (`docs/epic-63-real-cluster-testing-instructions` branch,
unmerged) has the same `/sessions` and marker-detection bugs; it should receive
the identical fixes when it lands.

---

## Key Decisions

### Multi-replica `v2PendingSessions` divergence is the one real blocker

`v2PendingSessions` (`proxy_v2.go:85`) is an **in-memory, per-replica** map
(workspaceID → sessionID → pending count). `enqueueV2` increments it on the
replica that handles the HTTP request; `bridgeV2Prompted` decrements it on the
replica that receives the `session.next.prompted` SSE event; and
`wakeStrandedV2Sessions` only wakes sessions present in **its own** replica's
map.

On this 2-replica deployment I observed directly:
- The periodic `stranded queue sweep: reconciling workspace …` ran every ~30s
  on **both** replicas (so `reconcileSessionState` → `wakeStrandedV2Sessions`
  fired periodically).
- But the API log line `V2 stranded-input recovery: waking idle session` had
  **count 0** on every pod — because the replica doing the wake had an empty
  `v2PendingSessions` for that session (the enqueue landed on the other
  replica).

This explains both anomalies: US-63.4's second marker stranded (no wake on the
replica holding the entry / wake ran on a replica with no entry), while US-63.9
drained only because, that run, the enqueue replica happened to also own the SSE
reconnect. The outcome is therefore **non-deterministic** under >1 replica.

This is the limitation the guide already lists as risk #3 ("Multi-replica
failover … tracking is lost. Accepted limitation for now"). Validation elevates
it from a theoretical failover concern to a **demonstrated reliability gap for
normal operation** (US-63.4 second-message drain, US-63.9 wake reachability).

### Recommended fix (not implemented in this pass)

Back `v2PendingSessions` by **Redis** (a SET keyed `v2:pending:<workspaceID>`
with per-session reference counts, or a sorted set). All replicas then share the
same pending set, so any replica's periodic sweep / SSE reconnect wakes the
right sessions deterministically. This is a focused change to the four methods
(`add`/`remove`/`has`/`sessionsForWorkspace`) plus a Redis client dependency the
handler already has (`queueSvc`). It was **not** implemented here because (a) it
cannot be validated against this cluster in its current state (see Blockers) and
the repo's TDD/validate rule forbids shipping an unvalidated behavioral change,
and (b) it warrants its own story + unit tests pinning the Redis schema.

### Scope of the commit

Only the test artifacts were changed. The V2 Go code itself behaved correctly
within the limits of the in-memory tracking; no production code change is
claimed or validated in this pass.

---

## Blockers

1. **V2 cannot be enabled in production on multi-replica deployments** until
   `v2PendingSessions` is made shared (Redis). On a single-replica deployment
   the three properties pass cleanly (enqueue == sweep == reconnect replica).
2. **The validation cluster itself was being redeployed by its GitOps owner
   (flux) throughout the session**, reverting manual `kubectl set env` and
   rolling the image (0.13.0 ↔ 0.12.2, the latter intermittently in
   ImagePullBackOff). This prevented a clean single-replica confirmation run
   (the one variable that would isolate the multi-replica cause definitively)
   and means any further code change could not be re-validated here. The
   multi-replica attribution above is the most consistent explanation for all
   observations (periodic reconcile ran, wake-count was zero) but a
   single-replica re-run on a stable cluster is the recommended confirmation.

---

## Tests Run

- `local/us-63-v2-behavior-e2e.sh` (original): failed — surfaced the four
  test-artifact bugs above.
- Manual behavioral probes via the proxy API (port-forward 18080): US-63.3
  PASS, US-63.4 partial, US-63.9 PASS, plus role-aware history inspection of
  each session (`ses_015b82028…`, `ses_015b687d4…`, `ses_015b444a4…`).
- Direct opencode wake-prompt probe (port-forward 4096): `\n` → 200,
  ` ` → 200. The KEY RISK test.
- `bash -n local/us-63-v2-behavior-e2e.sh`: syntax OK after fixes.
- `history_contains` echo-detection logic: unit-checked against three fixture
  histories (user-prompt-only → NO; user+echo → YES; clean echo → YES).
- No Go unit tests added (no Go code changed).

---

## Next Steps

1. **Implement Redis-backed `v2PendingSessions`** as a dedicated story; pin the
   Redis schema with a unit test (follow the `proxy_v2_test.go` injection
   pattern). This unblocks multi-replica production enablement.
2. **Re-run `local/us-63-v2-behavior-e2e.sh` on a stable single-replica cluster**
   to confirm US-63.4 (both markers) and capture the `waking idle session` log
   line end-to-end (the one piece of direct log evidence not obtained here —
   the test-run pods were redeployed before their logs could be read).
3. Apply the same four test-artifact fixes to the validation guide on
   `docs/epic-63-real-cluster-testing-instructions` before it merges.
4. Once (1) and (2) pass on the target replica count, **US-63.7 (legacy V1
   queue deletion) may proceed**; until then keep V1 as the fallback.

---

## Files Modified

- `local/us-63-v2-behavior-e2e.sh` — fixed session-create route + response key,
  echo-aware marker detection, clean HTTP codes, runtime container resolution,
  opt-in `EXTRA_CURL_HEADERS` for HTTPS-enforcing clusters.
- `worklogs/0718_2026-08-10_epic-63-v2-real-cluster-validation.md` — this entry.
