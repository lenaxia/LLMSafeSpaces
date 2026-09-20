# 0060 — Upload delivery in sidecar mode: stage-and-signal over the control socket

**Status:** Proposed (2026-09-20) — design stage, holds for review; implementation lands in follow-up PRs (this document closes nothing)
**Date:** 2026-09-20
**Issues:** #1500 (uploads clean-fail on sidecar pods — the delivery leg) · #1497 (the investigation that established the disposition and the evidence base)
**Depends on:** design 0051 Appendix A (control socket v1 — the signaling surface), US-4b/US-70.1 (the shared `/sandbox-runtime` tmpfs + the uid-ownership-by-construction principle), #1165 R2b (the staged-files precedent: canonical bytes + ledger, applied by the consumer), Epic 68 (uploads D1–D19; D2/D3 persistence contract, D16 gate order)
**Composes with:** `refresh_files` (control protocol v1's existing staged-files apply method — the on-demand mid-lifecycle precedent this design generalizes), `scrubUploadTmpFiles`/`scrubUploadsAtBoot` (the orphan-hygiene class this design extends)
**Supersedes:** nothing merged. The "until a control-socket write op exists — see worklog" caveat at `cmd/workspace-agentd/uploads.go:25-27` becomes obsolete and is removed by the implementation PR.

---

## 1. Problem

### 1.1 Code truth (base @ `01481ffd`)

Uploads must land on the workspace PVC: `UploadsPath = "/workspace/uploads"` (`pkg/agentd/types.go:66`, Epic 68 D2/D3 — uploads survive suspend/resume). In single-container mode agentd writes them directly (temp+rename, `cmd/workspace-agentd/uploads.go`), which is why uploads work there. In sidecar mode — what prod runs (verified on a live prod pod: `/agentd-config` RO tmpfs, the US-4b layout) — the `/v1/files` mux is served by the **sidecar** (uid 2000), whose `/workspace` mount is **read-only by design** (US-4b; `controller/internal/workspace/agentd_sidecar.go`). Every upload therefore clean-fails: staging write → `ENOSPC`/`EROFS`-class error → agentd 500 `storage unavailable` → API 502 `workspace agent upload failed` (#1497 chain). The nightly pins this exact behavior as as-built (`e2e-nightly.yml:357-361`).

### 1.2 What already exists (the fix is a generalization, not an invention)

| Precedent | Where | What it proves |
|---|---|---|
| Stage-and-pull for files | #1165 R2b: the sidecar's materializer stages file-class secrets (canonical bytes + `spawn-files-ledger.json` manifest) and the **supervisor applies them** — spawn-time via `preSpawn`, mid-lifecycle via the control-socket `refresh_files` method (`control_socket.go:277-287`, found by #1244) | uid-1000 writes by construction; mid-lifecycle on-demand apply already has a protocol shape |
| Shared staging surface | `/sandbox-runtime` — one 96 MiB memory-medium emptyDir, **mounted RW in both containers** (sidecar: `agentd_sidecar.go:197`; workspace container: verified live) — already carries `staged-secret-files/`, `spawn-files-ledger.json`, `secrets-env`, `rt/*` | the budgeted staging surface the issue's directive names; cross-uid readability is established practice (0640/gid-1000, `LLMSAFESPACES_CROSS_UID_FILES`) |
| Control socket v1 | design 0051 Appendix A: one JSON request/response per TCP connection on `127.0.0.1:4099` (pod-shared network namespace — the sidecar reaches the workspace container's PID 1); closed enums; additive methods (`refresh_files` was added under v1) | the synchronous ack channel; the capability-equivalence rule (A.4) that governs any new method |
| Orphan hygiene | `scrubUploadTmpFiles` + `scrubUploadsAtBoot` (`uploads.go:276-303`) | the TTL/boot-scrub class to extend |
| End-to-end streaming | API: `io.Pipe` + `cap+1` LimitReader, 25 MiB cap (`api/internal/handlers/uploads.go`) | the streaming contract the new leg must preserve |

### 1.3 The owner's binding directive

**Disk/memory safety are first-class: the design must be incapable of causing disk-full conditions.** The six concrete requirements (staging admission by reserved bytes; byte-weighted semaphore; streaming; crash/orphan hygiene with TTL+boot-scrub; D16 write-time re-check + post-write verification; full observability) are treated as normative — §4 addresses each by number.

---

## 2. Decision

**Stage-and-signal** (not pull): the sidecar stages upload bytes on the shared tmpfs and signals the supervisor over the control socket; the supervisor streams them onto the PVC.

The alternative considered — an HTTP pull leg (sidecar serves staged bytes, supervisor GETs them, the `spawn-files` transport generalized) — moves the same bytes over a second authenticated surface and keeps them in sidecar-private memory when a shared, budgeted, already-established staging surface exists. The control socket stays what it is (small JSON signals — "a file is ready, apply it"), `refresh_files`'s semantic is preserved verbatim for credentials, and the only new socket surface is one bounded, closed-param method (§3.2). Staging on the shared tmpfs is exactly what the directive budgets for (requirement 1 names it), and cross-uid file semantics there are established practice.

What this is **not**: the sidecar never writes the PVC (US-4b preserved — the workspace container performs every durable write); the workspace container never gains a path to sidecar-private state (`/agentd-secrets` topology untouched); single-container behavior is byte-identical (the direct-write path is unchanged; §5.1).

---

## 3. Architecture

### 3.1 The upload sequence (sidecar mode, synchronous end-to-end)

```
client ── multipart (streamed) ──▶ API POST /workspaces/:id/uploads
                                    │ D16 gates (phase→disk-ratio→cap)   [unchanged]
                                    ▼
                     agentd (sidecar) :4097 /v1/files    [streamed, as today]
                       1 admission: reserve bytes (§4.1–4.2) ── reject: 507/429 (§4.6)
                       2 stream body → /sandbox-runtime/staged-upload-files/<id>.part
                         (chunked copy, sha256 on the fly, 0640/gid-1000)
                       3 same-fs rename <id>.part → <id>   [staged-complete atomicity]
                       4 control socket: upload_apply {id, size, sha256, target} (§3.2)
                                    │ (connection held; bounded wait, §3.3)
                                    ▼
                     supervisor (workspace container, uid 1000, PID 1)
                       5 write-time gates (§4.4): statfs(PVC) avail > size+margin
                       6 stream <id> → /workspace/uploads/<uuid>-<sanitized>.part
                         (bounded window, 0644 per U1.1.11) → fsync
                       7 verify size + sha256 (integrity across the boundary)
                       8 same-fs rename → <uuid>-<sanitized>              [visible-atomicity]
                       9 post-write statfs verification (§4.4)
                       ▸ ack {applied, path, size}
                                    │
                     agentd: delete staged <id>, release reservation (defer)
                       ▸ 201 {path, name, size} ──▶ API 201 ──▶ client
```

Steps 1–9 are one synchronous client request. Every write is streamed with a bounded window (≤256 KiB chunks): at no point does any process hold more than the window in memory, on either side, at any file size — requirement 3's end-to-end streaming is preserved (the API→agentd leg already streams; agentd→tmpfs and tmpfs→PVC are bounded-window copies).

### 3.2 Control protocol: the `upload_apply` method (additive under v1)

One new method, closed params, closed error enum — Appendix-A discipline:

```jsonc
// request
{ "v": 1, "id": 42, "method": "upload_apply",
  "params": { "upload_id": "<uuid>",            // also the staged basename
              "staged_name": "<uuid>",           // constrained: basename within
                                                  // /sandbox-runtime/staged-upload-files
              "size": 12345, "sha256": "<hex>",
              "target_name": "<sanitized-filename>" } }
// response (success)
{ "v": 1, "id": 42, "result": { "applied": true, "path": "/workspace/uploads/<uuid>-<name>", "size": 12345 } }
// response (failure) — closed enum
{ "v": 1, "id": 42, "error": { "code": "staged_missing|checksum_mismatch|size_mismatch|dest_disk_full|dest_write_failed|target_rejected|busy", "message": "..." } }
```

**A.4 capability-equivalence:** every capability `upload_apply` grants is strictly weaker than what the uid-1000 supervisor already holds — it instructs the supervisor to write a file *into its own RW `/workspace`* under a uuid-prefixed, sanitized name, from bytes already readable to it on the shared tmpfs. The params are a closed set: no arbitrary source paths (staged basename is validated `^[0-9a-f-]{36}$` under the fixed staging dir), no arbitrary destinations (target passes the same `sanitizeUploadFilename` as today, uuid-prefixed into the fixed uploads dir), no overwrite (uuid names make collisions meaningless). The socket's two standing invariants are untouched (no env values returned; no argv-shaped capability) — this method carries file metadata, not execution surface.

**Why not extend `refresh_files`:** that method's contract is "apply the credential manifest" (idempotent batch-apply against a ledger). Uploads are per-object, best-effort-once, ack-in-line. Different lifecycle, different idempotency — one method each, both narrow.

### 3.3 Synchronous ack, timeouts, and retry stance

The control connection is held through the supervisor's copy (milliseconds–seconds for ≤25 MiB; tmpfs→PVC). agentd bounds the wait (`UPLOAD_APPLY_TIMEOUT`, default 60 s — generous for a ≤25 MiB bounded-window copy; it is independent of the API's 5-min body-stream timeout because the body is fully staged *before* the signal, so the two windows never compose). On timeout, agentd responds to the API with 504 (`upstream_apply_timeout`) and **leaves the staged object in place**: the supervisor may still complete it (the copy is idempotent per `upload_id` — if the target exists with matching size+sha256, it re-acks `applied` without rewriting). Orphaned staged objects that never complete are reclaimed by hygiene (§4.3). The client's retry creates a new `upload_id`; no dedupe is required (Epic 68 D17 semantics unchanged).

### 3.4 Atomicity across the boundary (the "temp+rename equivalence")

Two same-filesystem renames bracket a cross-filesystem copy — that is the atomicity contract:

1. **Staged side:** `.part` → `<id>` on the tmpfs *before* the signal (a signal can only ever name a complete staged object; a crash mid-stage leaves a `.part` that hygiene reclaims and no signal ever references).
2. **Destination side:** `.part` → final on the PVC (the existing single-container contract, unchanged — readers never see partial files; Epic 68 D3).
3. **Integrity:** sha256 computed while staging, verified while writing (step 7) — corruption on either surface is a hard failure with a distinct error code, never a silent bad file.

---

## 4. Disk and memory safety (the binding directive, requirement by requirement)

### 4.1 R1 — Staging admission by reserved bytes (never evict credentials)

The staging dir is `/sandbox-runtime/staged-upload-files/` on the shared ~96 MiB tmpfs. **Admission is reservation-before-acceptance**, using the size known up front (Content-Length, already enforced ≤ the 25 MiB upload cap before the body is read):

```
admit(newBytes) ⟺  credentialUsage()                    // walk: staged-secret-files/,
                    + UPLOAD_STAGING_CREDENTIAL_FLOOR    // spawn-files-ledger, secrets-env,
                    + reservedUploads() + newBytes        // rt/*, admin-prompt, live .parts
                    ≤ tmpfsCapacity()                     // defaults: floor 16 MiB (cfg)
                                                       // capacity from statfs(tmpfs)
```

- `reservedUploads()` is the sum of *admitted-but-not-yet-released* reservations (released on ack, on error, on scrub — `defer`-shaped bookkeeping in-agentd, one mutex).
- `credentialUsage()` is recomputed at each admission (a stat walk of the credential surfaces — they are few and small); **credentials are never evicted and never blocked by uploads** — the floor plus the recomputed live usage guarantee headroom for credential staging at all times.
- `tmpfsCapacity()` comes from `statfs`, not a hardcoded 96 MiB — the design must not break if the volume is resized.
- Staged uploads themselves are written 0640/gid-1000 and counted the moment they are admitted; a rejected upload is rejected **before its first byte is staged** (507, §4.6) — an upload can never half-fill the tmpfs it was refused by.

Memory-medium note: tmpfs pages count against pod memory; the reservation scheme bounds the worst case at `UPLOAD_STAGING_MAX_FRACTION` (default 50%) of capacity — the pod's own memory limit is therefore protected from upload-driven pressure by the same budget that protects credentials.

### 4.2 R2 — Byte-weighted concurrency semaphore

Admission (4.1) is itself the byte-weighted semaphore: concurrent uploads' reservations can never exceed the fraction. A separate count cap (`UPLOAD_STAGING_MAX_CONCURRENT`, default 4) bounds simultaneous copies (fd/window pressure), rejecting with 429 (`staging_busy`) — clean, retryable, and distinct from budget exhaustion.

### 4.3 R4 — Crash/partial hygiene

Two reclaim paths, both extending the established `*.part`/boot-scrub class:

- **Boot scrub:** `scrubUploadsAtBoot` gains the staging dir (sidecar boot: any `staged-upload-files/*` is by-definition orphaned — the in-flight upload died with the process).
- **TTL sweeper:** a bounded ticker (default 10 min) removes staged objects older than `UPLOAD_STAGING_TTL` (default 15 min) — covers supervisor-crash, socket-timeout tail, and agentd-crash-then-rebooted windows. The sweeper also finalizes reservation bookkeeping for what it removes (gauge truth, §4.6).
- `.part` files are never signaled, never acked, and reclaimed by both paths on sight.

### 4.4 R5 — Destination gates: write-time re-check (TOCTOU) + post-write verification

The API's D16 disk-ratio gate (CRD-status-based) stays as the fast pre-filter. The **authoritative** gate is at the supervisor, at write time, against the real filesystem:

- **Pre-write:** `statfs(/workspace)` → reject `dest_disk_full` unless `avail > size + margin` (margin default 64 MiB — absorbs concurrent writers the stat cannot see).
- **Post-write:** `fsync` success **and** `statfs` re-check (avail ≥ 0 sanity vs pre-write) before the rename. A write that completed but filled the disk within the margin still renames (it succeeded) but is observable (`dest_margin_consumed` counter) — the disk is never *silently* full.

The supervisor cannot read the CRD (and must not — it would need API credentials); `statfs` is the stronger signal anyway (ground truth vs cached ratio).

### 4.5 R3 — Streaming (restated)

No component buffers an upload object in memory: API pipes (unchanged), agentd stages with a bounded window, the supervisor copies with a bounded window (≤256 KiB). sha256 is computed incrementally on both sides. There is no size at which memory behavior changes.

### 4.6 R6 — Observability (pressure visible before it breaks)

agentd metrics (the existing `uploads_total{reason}` gains classes; new gauges):

| Metric | Type | Meaning |
|---|---|---|
| `llmsafespaces_agentd_upload_staging_bytes` | gauge | staged-upload bytes on disk (walked truth, reconciled with reservations) |
| `llmsafespaces_agentd_upload_staging_reserved_bytes` | gauge | admitted-not-released reservations |
| `llmsafespaces_agentd_upload_staging_files` | gauge | staged object count |
| `llmsafespaces_agentd_upload_staging_credential_bytes` | gauge | credential-surface usage (the input to admission — makes the floor policy auditable) |
| `llmsafespaces_agentd_uploads_total{reason}` | counter | existing counter; new reasons: `staging_full`, `staging_busy`, `apply_timeout`, `apply_rejected`, `checksum_mismatch`, `staging_scrubbed` (alongside `success`) |
| `llmsafespaces_agentd_upload_dest_rejections_total{code}` | counter | supervisor-side gate outcomes (`dest_disk_full`, margin events) |

Rejection semantics (client-actionable, per the directive): staging budget → **507** `staging budget exhausted — retry after in-flight uploads settle or free tmpfs`; concurrency → **429** `staging busy`; destination disk (write-time) → **507** `workspace disk is full (write-time check)`; apply timeout → **504**; all others → the existing 502 class with the specific reason in the body.

---

## 5. Modes, rollout, and edge behavior

### 5.1 Single-container mode: unchanged

The supervisor-mode agentd writes `/workspace/uploads` directly today, with the same temp+rename and cap discipline. The staging leg activates **only** when the upload handler runs in sidecar context (the same mode detection the muxes already use). No wire change, no behavior change, no new config required.

### 5.2 Suspension/resume mid-upload

An in-flight upload dies with the pod (tmpfs wiped — the staging surface is per-pod by design). The client sees a transport error and retries against the resumed pod. No PVC `.part` survives: the supervisor's rename happened or it didn't; a crashed `.part` on `/workspace/uploads` is reclaimed by the existing boot scrub (`scrubUploadTmpFiles` — unchanged, it already covers the destination dir).

### 5.3 Concurrency within one pod

Multiple uploads: each has its own `upload_id`, staged object, and reservation; the supervisor applies them serially or concurrently (its choice — the copies are independent); `upload_apply` is safe to call concurrently (distinct targets by construction).

### 5.4 Rollout

Pure agentd + supervisor change (both delivered in the one digest-pinned agentd artifact) — no API wire change beyond new 507/429/504 reason strings it already passes through, no CRD/Helm schema change (new env knobs are additive with defaults). The nightly's sidecar upload rows flip from assert-clean-fail to assert-delivery when this lands (Epic 68 E2/E10/E11 un-skip).

---

## 6. Test plan (implementation lanes inherit this)

| Class | Pin |
|---|---|
| Staging admission | budget math unit table (floor/usage/reserve boundaries); reject-before-first-byte ordering; 507 shapes |
| Semaphore | concurrent admission never exceeds the fraction (race test); 429 class |
| Streaming | large-object memory flatness (allocation ceiling assertion on a ≥cap object); window bounded |
| Atomicity | crash-injected matrix: kill between every pair of steps 1–9 → no partial visible on either surface; idempotent re-apply on retry-after-timeout |
| Hygiene | boot scrub clears staging; TTL sweeper reclaims + reconciles gauges; `.part` never signaled |
| Destination gates | statfs pre-write rejection; post-write verification; margin-consumed counter |
| Protocol | `upload_apply` param validation (basename regex, sanitized target, closed error enum); unknown-param rejection per A.4 |
| E2E (nightly, sidecar) | upload → file present, owned uid 1000, survives suspend/resume; concurrent uploads; disk-full simulation → 507 write-time; multi-tenant isolation rows (E2/E10/E11 un-skip) |
| Regression | single-container path byte-identical (existing uploads tests untouched and green) |

---

## 7. Open questions (implementation-lane inputs, not blockers)

1. **Defaults:** credential floor 16 MiB, staging fraction 50%, TTL 15 min, apply timeout 60 s, concurrency 4, chunk window 256 KiB — all env-tunable; the implementation PR pins them with a table in the values file and these defaults.
2. **Supervisor apply concurrency:** serial queue (simplest, bounded) vs parallel — implementation detail; the design is indifferent (targets are independent).
3. **`statfs` margin accounting on shared PVCs:** Longhorn sizing semantics may make `avail` conservative; the margin default absorbs this, the counter makes deviations visible.

---

## 8. Implementation sequencing (post-approval, multi-PR)

1. **agentd staging leg** — staging dir, admission/semaphore, `.part`+rename, sha256, hygiene (boot+TTL), metrics, the 507/429 arms (sidecar context only).
2. **supervisor `upload_apply`** — the socket method, destination gates, bounded-window copy+verify+rename, ack + closed errors.
3. **Wiring + observability polish** — agentd→socket call with bounded wait, reason-string pass-through, gauge/consumer surfaces, values-file knobs.
4. **E2E un-skip** — nightly rows flip; Epic 68 docs updated (the as-built caveat at `uploads.go:25-27` and the README §File Attachments sidecar note both retire).
