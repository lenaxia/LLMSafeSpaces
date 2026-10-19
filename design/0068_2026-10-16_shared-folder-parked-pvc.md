# 0068 — /shared persistent user folder: parked-PVC architecture (RWX warm volume + S3 park)

**Status:** Proposed (2026-10-16; deferral ruled 2026-10-18) — every architecture decision below is owner-settled in #1709 and transcribed verbatim in §2; this document elaborates **HOW**, never whether. **Owner ruling (2026-10-18, on the #1709 spike-blocker escalation): the leg evidence is DEFERRED to the implementation lane's first real-cluster soak** — the spike run ledger (§3.1, `design/evidence/shared-folder-spike/README.md`, incl. its r24 re-adjudication row) is the recorded justification: the pool's genuine measured limitation is the Longhorn v1 engine `starting` hang under nested dind (replicas running, iscsid alive, engines never starting — runs 39079884685/39089893139/39093640739, and reflected in every post-quoting-fix run with green prereqs (39178767180, 39190530387, 39195400662, 39200512597, 39203311203, 39207836177, 39211105790): iscsid dies between prereqs and legs on this pool, and even the gate-revived run still showed engines hanging with the namespaced initiator failing — though the revived run's IPC verdict rode the pre-fix capture code with a 30-minute unmeasured window after it, so daemon re-death and the namespaced-path pathology remain formally unseparated pending a run that records env_iscsid_at_legs=alive with the adjudicable capture); the later "runner-identity/exec-drift" record was the kit's own quoting bug (an apostrophe in a payload comment truncated the single-quoted `bash -c` string; the post-truncation script ran on the CI runner, not the node) — withdrawn and fixed (heredoc-fed payload + parse pin), and leg A, though reached repeatedly, never produced a valid runsc×RWX verdict — its failures sat on the storage path (RWX volumes never attached; the workspace pods never started, so runsc itself was never exercised, while runsc provisioning was green throughout). No architecture verdict, positive or negative, is asserted. **Per the PENDING protocol: every `[R:...]` row stays PENDING until the implementation soak commits its `results.txt` into `design/evidence/shared-folder-spike/`; no PASS claim exists in this document.** Implementation stories follow owner approval; this PR is the design deliverable + spike kit + run ledger only.
**Date:** 2026-10-16
**Issues:** #1709 (charter — the settled architecture, rulings, rejected alternatives, spike list) · design 0051 V9 (the runsc mount-validation leg this spike descends from) · #1500 / design 0060 (stage-and-pull: the ownership-by-construction precedent the sync pod inherits) · #1056 / Epic 68 (attachment ingest: the atomic tmp+rename + boot-scrub hygiene pattern the park path reuses) · PR #1703 / design 0067 (boards §authz — the ownership-seam discipline §8 follows; NNN 0067 is claimed by that open PR, hence 0068 here).
**Spike artifacts:** `design/evidence/shared-folder-spike/` currently holds the run ledger (`README.md` — the 2026-10-18 ruling's recorded justification); `results.txt` (structured KEY=VALUE rows for legs 0/A/B/C, cited below as [R:KEY]), the leg logs, and the runner environment record LAND THERE FROM THE IMPLEMENTATION SOAK per the ruling. The spike scripts themselves are committed now: `local/1709-shared-folder-spike.sh` + `local/lib/longhorn.sh` (the workflow that runs them: `.github/workflows/shared-folder-spike.yml`).

---

## 1. The question and the answer in one page

**Question (#1709):** users carry 10–100 GB of personal tooling — scripts, rc files, datasets — between workspaces. Every workspace the user starts should have it at **/shared**, read-write, POSIX-native, with no platform opinion about the content. The owner settled the architecture over the alternatives (§9): **one RWX PVC per user while warm, S3 while parked** — and three spike questions gate the design: does gVisor mount Longhorn RWX in the real workspace posture, how fast can 100 GB hydrate from S3, and can a snapshot be mounted read-only in a sandboxed pod for the sync path.

**Answer:** the three verdict rows are `[R:a_status]` / `[R:b_status]` / `[R:c_status]` — PENDING by the 2026-10-18 ruling, filled by the implementation lane's first real-cluster soak (not asserted here): workspace-shaped gVisor pods mounting Longhorn RWX (share-manager NFS) with the full security posture and cross-pod uid semantics ([R:a_uid_mapping]); parallel rclone hydration at **[R:b_hydrate_gvisor_t32_mbs]** MB/s under gVisor vs **[R:b_hydrate_runc_t32_mbs]** MB/s under runc (the gofer tax is the delta); and the snapshot→restore→RO-mount cycle in **[R:c_restore_bound_s]+[R:c_sync_pod_s]**s with a full checksum pass. The architecture stands as settled if and only if those rows read PASS at the soak; a leg-A FAIL there is the charter's architecture-level stop (reported on #1709 immediately). This doc specifies the rest: the SharedVolume lifecycle state machine (controller-owned, the workspace phase machine's sibling), snapshot-sourced sync at a 15-minute cadence, the `user-shared` StorageClass posture, quota-as-PVC-size with org accounting, the S3 layout + retention lifecycle, the user-level ownership seam, and the rollout sequence.

The one design-forcing **environment finding** the spike surfaced along the way: **minio/minio is delisted from Docker Hub** (§3.5) — any platform component that must pull an S3-relevant image fresh (the sync pod is rclone, not MinIO, so the design is unaffected; the spike substituted the prod-pinned SeaweedFS) must not assume `docker.io/minio/*` is pullable.

---

## 2. Settled rulings (owner-approved in #1709 — transcribed verbatim)

> **Settled architecture — PARKED-PVC (owner-selected over live S3 mounts)**
> - **Warm**: one RWX PVC per user (Longhorn) mounted at /shared in every live workspace — plain PVC mount (the existing, gVisor-proven path), native-volume performance, full local POSIX.
> - **Parked**: S3 only (`users/{id}/shared/**`, MinIO-compatible).
> - **Lifecycle (controller-owned state machine)**: first workspace up → hydrate PVC from S3 (pre-warm starts at Pending phase; workspace Ready gates on hydration); ALL workspaces down/suspended for a TTL (default 24h) → final sync → PVC discarded.
> - **Sync**: Longhorn block-snapshot (crash-consistent, cheap COW) → **sandboxed sync pod** mounts the snapshot read-only (gVisor) → rclone to S3 → snapshot deleted. Cadence **15 min** = the primary durability knob (RPO ≤15 min on dual-failure/cluster-loss).
> - **Replication**: `user-shared` StorageClass, `numberOfReplicas: "2"` default (chart-overridable; 3 = documented durability-max override; cloud CSI = provider replication). S3 is the durability tier (survives cluster loss); hot replicas are the HA tier (zero-RPO single-node loss + automatic rebuild).

> **Rulings (owner, in conversation)**
> 1. TRUE mount required at 10–100 GB (sync-to-each-workspace withdrawn as primary).
> 2. **Unsandboxed mount producers are an attack vector — DISQUALIFIED**: no FUSE sidecars, no node-level userspace S3 clients, no runc mount producers anywhere in the data path. (This killed JuiceFS-as-mounted-FS and the S3-CSI path; the sandboxed batch sync pod is the only S3 client and it is gVisor'd.)
> 3. /shared = fixed path, RWX, content-agnostic.
> 4. Ownership is USER-level (consistent with the boards R7 pattern); access/ownership separated — future collaboration = additive seam.
> 5. Quota = PVC size (ENOSPC semantics, exact): platform default / per-user max / per-org aggregate (accounting check at create/resize).
> 6. Posture default: 2 hot replicas + 15-min snapshot cadence (owner-accepted); 3+hourly = per-deployment override.

Every normative section below conforms to these; nothing here re-litigates them.

---

## 3. The spike (gates the design — evidence)

### 3.1 Environment

Self-hosted `lenaxia-dind-runner` (the US-70 delivery pool's privileged docker-in-dind set — ops-prod #2385), kind v0.32.0 single-node cluster (kindest/node v1.35.5) with `serviceSubnet 10.217.0.0/16` and iptables kube-proxy (the dind-nesting guards from `local/kind-cluster.yaml`), runsc provisioned into the kind node via `local/lib/gvisor.sh` (gVisor latest release bundle, sha512-verified — the pool's own recipe), **Longhorn chart 1.12.1 — the exact version the production cluster runs** (talos-ops-prod `storage/longhorn`), snapshot-controller 5.3.0 (prod's pin), StorageClass `user-shared` with the prod `longhorn-2r` parameter set (§5), and a synthetic S3 tier in-cluster. **No cloud bucket was used** (per the charter's MinIO allowance): the S3 tier is **SeaweedFS 4.48 — prod's pinned S3-compatible object store** — because minio/minio turned out to be unpullable (§3.5). Every pod that MOUNTS THE LONGHORN VOLUME runs under the exact workspace posture of `controller/internal/workspace/pod_builder.go` (`runAsUser/runAsGroup/fsGroup 1000`, `FSGroupChangePolicy: OnRootMismatch`, `runAsNonRoot`, `seccompProfile: RuntimeDefault`, `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]`, `automountServiceAccountToken: false`, `enableServiceLinks: false`) — the spike proves the real shape, not a relaxed one. (The S3 tier and the S3-only seed jobs are spike-side infrastructure — no Longhorn mount, no runtime class — with service-account tokens disabled on every namespace object regardless.) Dataset: synthetic random data, two file classes (128 MiB bulk + 256 KiB small) + per-file sha256 manifest; size auto-fitted to the runner's free disk (SeaweedFS copy + seed staging + 2 Longhorn replicas = 4× the dataset must fit) — **[R:auto_gb]** GiB measured, per-GB throughput stated for extrapolation to the 100 GB budget (the runner's disk cannot hold 4× 100 GB; §3.3 gives the scaling evidence).

### 3.2 Leg A — runsc × Longhorn RWX in workspace-shaped pods: **[R:a_status]**

Two gVisor'd pods (`runtimeClassName: gvisor`, full posture above) mount ONE `ReadWriteMany` PVC (`user-shared` SC) at `/shared` [R:a_ws_a_imageid]/[R:a_ws_b_imageid] resolved imageIDs:

| Check | Result | Row |
|---|---|---|
| RWX PVC bound | **[R:a_pvc_bound_s]**s | [R:a_pvc_bound_s] |
| Both pods Ready on the volume | **[R:a_pods_ready]** | [R:a_pods_ready] |
| runsc active (`/proc/version` reports gVisor) | **[R:a_runsc_active]** | [R:a_runsc_active] |
| Cross-pod write→read, 64 MiB sha256 match | **[R:a_cross_pod_rw]** | [R:a_cross_pod_rw] |
| Concurrent writers (2×16×4 MiB), full cross-verify | **[R:a_concurrent_writers]** | [R:a_concurrent_writers] |
| uid mapping — file written by ws-a reads back `1000:1000` from both | **[R:a_uid_mapping]** | [R:a_stat_from_a_ws_a] / [R:a_stat_from_a_ws_b] |
| Sequential 1 GiB dd write / read through the gofer | **[R:a_dd_write_gvisor_mbs]** / **[R:a_dd_read_gvisor_mbs]** MB/s | [R:a_dd_write_gvisor_mbs] |

Verdict (evidence-gated, per this doc's Status protocol): if the `[R:a_*]` rows above read PASS, the **descendant of design 0051's V9 question** is answered the same way the V9 leg answered — the runsc mount path holds for the Longhorn share-manager (NFS-backed) export exactly as it held for the per-workspace RWO PVC, including fsGroup propagation (`1000:1000` from both pods — the `OnRootMismatch` policy avoids the recursive-chown cost the workspace PVC already documented). If any row reads FAIL, leg A is the charter's architecture stop.

### 3.3 Leg B — hydration throughput (parallel rclone S3→Longhorn RWX): **[R:b_status]**

Dataset **[R:b_dataset_gb]** GiB seeded to S3 ([R:b_seed_s]s), hydrated into the RWX volume by rclone 1.67.0 in the workspace posture, `--transfers` sweep, gVisor vs runc:

| Run | Wall | Throughput |
|---|---|---|
| gVisor, `--transfers=8` | **[R:b_hydrate_gvisor_t8_s]**s | **[R:b_hydrate_gvisor_t8_mbs]** MB/s |
| gVisor, `--transfers=32` | **[R:b_hydrate_gvisor_t32_s]**s | **[R:b_hydrate_gvisor_t32_mbs]** MB/s |
| runc, `--transfers=32` | **[R:b_hydrate_runc_t32_s]**s | **[R:b_hydrate_runc_t32_mbs]** MB/s |
| Resync, no change (metadata scan) | **[R:b_resync_nochange_s]**s | — |
| Resync, +1 GiB delta (8×128 MiB) | **[R:b_resync_1gib_delta_s]**s | — |

**Ready-gate budget at 100 GB:** linear extrapolation of the gVisor `--transfers=32` rate gives **~[R:100gb_extrapolated_minutes] minutes**; the single-node runner caveat applies (S3 tier, Longhorn replicas, and the hydrating pod share one physical disk — a real cluster separates these, so the measured number is a *floor*; the no-change resync [R:b_resync_nochange_s]s and delta resync [R:b_resync_1gib_delta_s]s rows bound the rehydrate-after-short-park path, which is the common case). Design consequence (§4.4): hydration runs during the workspace's **Pending** phase with pre-warm started at user-intent signal (workspace create), and the Ready gate shows hydration progress — a cold 100 GB first-start is a progress bar, not a stall.

### 3.4 Leg C — snapshot → RO mount in a gVisor'd sync pod: **[R:c_status]**

A live writer appends self-verifying records (`REC%08d|base64(4KiB random)` — one pipe, the parser's field separator) to `/shared/journal.bin` **while** the VolumeSnapshot is taken; the restore PVC is then mounted **readOnly** by a gVisor'd sync pod (full posture) which (a) proves the mount is read-only by attempting a write, (b) verifies every dataset file against the seed manifest, (c) parses the journal for gaps/dupes (crash consistency: torn tail acceptable, gaps not), and (d) rclone-syncs the RO view to S3:

| Step | Result | Row |
|---|---|---|
| Snapshot `readyToUse` during live writer | **[R:c_snapshot_ready_s]**s | [R:c_snapshot_ready_s] |
| Restore PVC Bound (CSI restore from snapshot) | **[R:c_restore_bound_s]**s | [R:c_restore_bound_s] |
| RO-mount enforced (`touch` → EROFS) | **[R:RO_CHECK]** | RO_CHECK |
| Manifest checksum pass | **[R:MANIFEST]** | MANIFEST |
| Journal parse — records/gaps/dupes | **[R:JOURNAL_RECS]** recs, gaps **[R:JOURNAL_GAPS]**, dupes **[R:JOURNAL_DUPES]**, torn tail tolerated | JOURNAL_* |
| rclone RO-view → S3 push (the 15-min-cadence operation at full volume) | **[R:c_sync_pod_s]**s total incl. verify | [R:c_sync_pod_s] |
| Snapshot chain after cleanup | **[R:c_lh_snapshots_after_cleanup]** remaining | [R:c_lh_snapshots_after_cleanup] |

Verdict (evidence-gated): if the `[R:c_*]` rows above read PASS, the sync path is as cheap as the settled architecture assumed — the snapshot itself COW-instant, the CSI restore the measurable cost, the whole cycle at the spike's volume inside a 15-minute cadence at single-digit-percent duty cycle (the numbers above ARE the claim; if any row reads FAIL or PENDING, no cadence claim is made).

### 3.5 Spike-disclosed environment finding — Docker Hub delisting of minio/minio

Two spike runs died at image pull with `pull access denied for minio/minio, repository does not exist`; the Hub API confirms the repository is delisted (`object not found` for `minio/minio` while `rclone/rclone`, `library/debian`, `chrislusf/seaweedfs` all resolve) — prod's running tag is a pre-delisting cache. **The design's data path never pulls a MinIO image** (the only S3 client is the rclone-based sync pod, §4.5), but operator docs must not recommend `docker.io/minio/*` for the parked tier either; §7 notes the supported S3 endpoints (any MinIO-*compatible* endpoint the operator already runs — including SeaweedFS, proven by this spike — plus cloud S3).

---

## 4. Architecture

### 4.1 The SharedVolume CRD — controller-owned sibling of the Workspace phase machine

The lifecycle is a **new CRD, `SharedVolume`**, reconciled by the workspace controller (same binary, new controller — it shares the direct reader, RBAC scope, and namespace posture the Workspace reconciler already runs under). One SharedVolume per **(user, namespace)** — ownership is user-level per ruling 4; the namespace-local instance is an implementation consequence of Kubernetes (PVCs are namespace-scoped; workspaces live in the release namespace by default, `helm/values.yaml:9-13`), and multi-namespace deployments simply get one warm PVC per namespace backed by the **same** S3 prefix — the durability tier is shared even when the warm instance is not.

```yaml
apiVersion: llmsafespaces.dev/v1
kind: SharedVolume
metadata:
  name: shared-<userID>            # deterministic; one per (user, namespace)
  namespace: <workspace namespace>
spec:
  userID: <uuid>                   # owner; NOT org-attributed (see §8)
  size: 100Gi                      # quota = PVC size (ruling 5)
  storageClassName: user-shared    # §5; default from chart value
  park:
    idleTTLSeconds: 86400          # §4.6; default 24h (open ruling OR-1)
    syncCadenceSeconds: 900        # §4.5; default 15m (ruling 6)
status:
  phase: ""                        # §4.3 state machine
  lastSyncAt: null                 # watermark for cadence + park finality
  lastHydratedAt: null
  observedGeneration: 0
```

Owner references: the SharedVolume is **not** owned by any Workspace (it outlives all of them); it is garbage-collected only through Park + user deletion (§4.6, §7). Labels mirror the workspace set (`llmsafespaces.dev/app`, `component=user-shared`, `user=<userID>`) so the existing namespace NetworkPolicy and admission posture apply unchanged.

### 4.2 Pod wiring — extend `pod_builder.go`, invent nothing

`buildPod` (`controller/internal/workspace/pod_builder.go:20`) gains one conditional volume+mount: when the owning user's SharedVolume is `Ready`, the pod mounts its PVC at `/shared` (no subPath — the volume is single-purpose, unlike the three-subPath workspace PVC at `pod_builder.go:209-221`). The PVC is built by the SharedVolume reconciler via the same `buildPVC` shape (`controller/internal/workspace/pvc.go:53-80`) with `ReadWriteMany` and the `user-shared` class. The mount is RW in every workspace of the user — ruling 3's fixed path; content stays untouched (no dotfile wiring; users copy to `$HOME` themselves, per the charter). When the SharedVolume is absent or parked, the mount is omitted and the workspace boots without `/shared` (the degrade path §4.7d).

### 4.3 Lifecycle state machine

```
                 create (first workspace of user)
                        │
                        ▼
                   ┌─────────┐   PVC Bound, hydrate job running
     ┌── requeue ──│Hydrating│──────────────────────────────┐
     │             └────┬────┘                              │
     │        S3 empty/  │ hydration complete                │ verify pass
     │        manifest   ▼                                   ▼
     │        OK    ┌────────┐  ALL workspaces down/suspended ≥ idleTTL
     └──────────────│ Ready  │──────────────────────────────┐
                    │ (warm) │                              │
                    └────┬───┘                              ▼
          cadence tick   │                        ┌────────────┐ final sync ok
              ┌──────────┘                        │  Parking   │─────────┐
              ▼                                   │ (final     │         │
      snapshot→RO→rclone (§4.5)                    │  sync)     │         ▼
              │                                   └─────┬──────┘   delete PVC +
              ▼                                          │ fail       snapshot,
        lastSyncAt=now, resync                           │ (retry)   keep S3
                                                                 ┌─────────┐
                                                                 │ Parked  │
                                                                 └────┬────┘
                                                     any workspace│create
                                                                  ▼
                                                              (Hydrating)
```

States are **SharedVolume.Status.Phase** values: `Hydrating`, `Ready`, `Parking`, `Parked` (+ `Failed` terminal-with-message, same shape as Workspace's `Failed` + `failureReason` at `workspace_types.go:241-249,493-503`). The controller is the sole writer of Status.Phase (the US-23.3 single-writer discipline the Workspace CRD already pins).

### 4.4 Hydration — pre-warm at Pending, Ready-gating, progress

- **Trigger:** a Workspace entering Pending whose user has no SharedVolume (or a Parked one) creates/reactivates it. Pre-warm begins at **workspace Pending**, before Creating — the pod is not built until the PVC exists and is Bound, so the mount is present from first boot.
- **Mechanics:** the SharedVolume reconciler runs a hydration Job (rclone sync S3→PVC, `--transfers 32` — the spike's best gVisor setting [R:b_hydrate_gvisor_t32_s]) in the workspace posture on the RWX volume. First-ever volumes hydrate an empty prefix — effectively instant (the no-change resync row, [R:b_resync_nochange_s]s, is the shape of that path).
- **Ready gate:** the workspace's Pending phase waits on SharedVolume `Ready` (timeout: `sharedFolder.readyGateTimeoutSeconds`, default 3600s — the 100 GB extrapolation §3.3 plus margin). On timeout the workspace proceeds **without** /shared (degrade, §4.7d) and the SharedVolume flips to Failed with reason — hydration must never brick workspace boot. Pending-timeout handling mirrors `pendingTimedOut` (`controller/internal/workspace/pvc.go:31-33`).
- **Concurrent starts** (two workspaces, same user, same instant): both watch the same SharedVolume name; CR create is idempotent (deterministic name + owner-reference check, the `isPVCStale` pattern `pvc.go:16-29`); the second reconciler pass sees Hydrating and gates — there is exactly one hydration job per SharedVolume generation. The race the issue names resolves by construction: hydration is keyed to the SharedVolume, never to either workspace.

### 4.5 Sync — snapshot → RO mount → rclone, every 15 minutes

The cadence loop is SharedVolume-reconciler-owned (`RequeueAfter: cadenceSeconds - (now - lastSyncAt)`, the same arithmetic as the suspended-TTL requeue at `phase_suspend.go:172-180`):

1. **Snapshot** the PVC (CSI `VolumeSnapshot`, class `user-shared-snap`) — COW, crash-consistent, [R:c_snapshot_ready_s]s.
2. **Sync Job**: gVisor'd pod (`runtimeClassName: gvisor`, full §3.1 posture — the ONLY S3 client in the platform, ruling 2), mounting a **restore PVC** (CSI restore from the snapshot, RWO — a single reader) **readOnly: true** at `/snapshot`. rclone syncs `/snapshot` → `s3://users/{id}/shared/` (`--transfers 32`, `--delete` — the S3 prefix converges to the snapshot's point-in-time truth).
3. **Cleanup**: restore PVC + VolumeSnapshot deleted; snapshot chain verified empty ([R:c_lh_snapshots_after_cleanup]).

rclone runs in the workspace posture with an env-only config (no config file, no SA token — credentials via the existing secret-delivery seam: a dedicated `sharedfolder-sync` credential minted by the controller into the job's env through the same Secret path agentd credentials ride today). Job success updates `lastSyncAt`; failure leaves the previous watermark and requeues with backoff — a missed cadence never deletes user data.

**Cadence = RPO knob** (ruling 6): 15 min default; chart value `sharedFolder.syncCadenceSeconds` (§11). The whole cycle measured at [R:c_sync_pod_s]s for [R:b_dataset_gb] GiB; if those rows read PASS, §3.3's extrapolation bounds the duty cycle at the default cadence (single-digit percent at 100 GB) and the sync pod stays a Job, not a resident workload — if they read PENDING or FAIL, no duty-cycle claim is made (§3.4's gate).

### 4.6 Park — TTL, final sync, discard

- **Idle detection:** the SharedVolume tracks the user's live workspace set (list Workspaces by owner label, same index the reconciler already maintains). `idleSince = max(suspendedAt/terminatedAt over all workspaces)`; when `now - idleSince ≥ idleTTLSeconds` (default 24h, chart `sharedFolder.parkIdleTTLSeconds` — open ruling OR-1) the state machine moves Ready→Parking.
- **Parking** = one final §4.5 sync (no cadence skip: the final sync uses the same snapshot discipline), then PVC deletion (`reclaimPolicy: Delete` on `user-shared`, §5) and phase Parked. The S3 prefix IS the user's data while parked.
- **Rehydrate** on the next workspace start (§4.4): the delta-resync rows ([R:b_resync_nochange_s]s unchanged, [R:b_resync_1gib_delta_s]s per GiB changed) bound the short-park rehydrate cost — parking for hours and coming back is minutes, not a cold 100 GB re-hydration, because rclone sync is content-addressed.

### 4.7 Edge cases (the issue's open list, resolved)

- **(a) Crash during park** (controller dies between final-sync-complete and PVC delete): Parking is re-derived from persisted state — `lastSyncAt` and phase survive in Status; a reconciler that wakes in Parking first **verifies the final sync's watermark** (snapshot content manifest matches S3 manifest — the sync job's own manifest-compare step, §3.4's MANIFEST pattern) before deleting the PVC; if the watermark is stale it re-runs the sync. The PVC is never deleted on an unverified park. Crash between PVC delete and Parked-flip: recompute (PVC absent + watermark fresh → Parked).
- **(b) Simultaneous-start race:** §4.4 — deterministic SharedVolume name; single hydration job; both workspaces gate on the same Ready.
- **(c) Suspend-state interplay:** suspending ALL workspaces does not park immediately — the TTL clock starts at the LAST workspace's suspend (idleSince = max, §4.6); a resume during Parking cancels the park if the PVC still exists (Ready flips back) or rehydrates if it does not (§4.4). The suspend→TTL→park requeue arithmetic is the `handleSuspended` TTL pattern (`phase_suspend.go:161-180`) applied at the SharedVolume layer.
- **(d) Degrade paths:** hydration timeout or sync-image unavailable → workspace boots WITHOUT /shared (§4.4); the failure is loud in SharedVolume status (and the workspace event stream) but never blocks boot. No-Longhorn-RWX deployments: chart `sharedFolder.enabled=false` + the fallback tier (per-workspace rclone sync, §9, retained for exactly this).

---

## 5. StorageClass — `user-shared`

Chart-managed (the chart already manages storage posture via `workspace.defaultStorageClass`, `helm/values.yaml:659-673`):

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: user-shared
provisioner: driver.longhorn.io
allowVolumeExpansion: true          # quota raise = PVC expand (§6)
reclaimPolicy: Delete               # park deletes the PVC; S3 retains the data
volumeBindingMode: Immediate
parameters:
  numberOfReplicas: "2"             # ruling 6 default; "3" = durability-max override
  staleReplicaTimeout: "30"
  fsType: "ext4"
  dataLocality: "best-effort"
  disableRevisionCounter: "true"
  dataEngine: "v1"
  unmapMarkSnapChainRemoved: "ignored"   # park-time TRIM keeps the snap chain short
```

Parameter set = the prod `longhorn-2r` class verbatim (talos-ops-prod `storage/longhorn/app/storageclass-2r.yaml`) — the platform deploys what the reference operator already runs. Chart overrides: `sharedFolder.storageClass.replicas` (2→3), `sharedFolder.storageClass.name` (point at a cloud CSI for provider-replicated deployments — then `numberOfReplicas` is ignored and provider replication applies, ruling 6's cloud-CSI clause). `mountOptions: [nosuid, nodev]` recommended in docs (the G23 gap note in `docs/operator/storage.md` applies here identically).

**Single-node operator note (spike-disclosed, runs 38866952477/38873829660):** Longhorn's default `replicaSoftAntiAffinity=false` hard-affines replicas of one volume to **distinct nodes** — on a single-node cluster a 2-replica /shared volume can never assemble: replica #2 stays unscheduled, the RWX engine stays degraded, the share-manager's ganesha crash-loops on its readiness probe (`/var/run/ganesha.pid` never appears), and the volume faults. Any single-node homelab running /shared must set `replica-soft-anti-affinity=true` (and understand the second replica then lives on the same disk — the durability tier is S3 in that posture, which is exactly the architecture's story). This goes in the operator doc as a required-setup row.

## 6. Quota — PVC size is the quota (ruling 5)

- **Platform default**: `sharedFolder.defaultSize` chart value → instance setting `sharedFolder.defaultSize` (Tier 2, admin-mutable — the `workspace.defaultStorageSize` pattern, `docs/operator/storage.md` "The storage-settings trace"). Default proposal: **50Gi** (open ruling OR-2).
- **Per-user max**: `sharedFolder.maxSizePerUserGi` (default 200Gi — covers the issue's 10–100 GB band with headroom). Enforced at SharedVolume create and at resize.
- **Per-org aggregate**: `sharedFolder.maxAggregatePerOrgGi` (default 0 = unlimited until the owner rules OR-3). The accounting check at create/resize sums the org members' SharedVolume sizes (`SUM(size) WHERE user IN orgMembers`) — a single read in the workspace DB, checked by the API before the CR is created (the webhook ceiling pattern `webhooks.maxWorkspaceStorageGi` guards magnitude; org aggregate guards fairness).
- **Semantics**: quota is EXACT — ENOSPC at the PVC boundary, no burst, no soft limits; the frontend shows usage/limit from the existing PVC-usage plumbing (quota display is /shared's only UI, per the charter). Resize up = PVC expansion (`allowVolumeExpansion: true`); resize down is delete-and-rehydrate (documented; data flows through the S3 park so nothing is lost, but the volume rehydrates at the smaller size with rclone erroring on the first over-quota file — docs call this out).
- **PVCs are never auto-resized** (the platform's existing no-resize posture, `storage.md` "PVCs are never resized" — the /shared exception is explicit user-initiated expansion only).

## 7. S3 layout, retention, versioning

```
s3://<bucket>/users/{userID}/shared/**            # the park/durability tier
s3://<bucket>/users/{userID}/shared.manifest      # per-sync manifest (the §4.7a verify key)
```

- **Bucket/endpoint**: chart `sharedFolder.s3.*` (endpoint, bucket, region, secretKeyRef — the operator's existing MinIO-compatible store or cloud S3; SeaweedFS proven by the spike, §3.5 — including the operator note that Seaweed IAM actions are plain names (`Admin`, `Read`, `Write`, `List`, `Tagging`), not scoped/regex forms: a scoped action set that matches nothing yields blanket 403s, the spike's run-38873829660 lesson). One bucket for the platform; per-user prefixes, never per-user buckets (prefix lifecycle is cheap; bucket lifecycle is operational debt).
- **Prefix is user-owned** (ruling 4): the IAM policy the operator provisions for the platform grants the sync identity `s3:list/s3:get/s3:put/s3:delete` on `users/${userID}/shared/*` ONLY — the sync job's credential is minted per-sync with the user's prefix bound in (the stage-and-pull ownership lesson: the credential can only ever touch the owner's prefix).
- **Retention/versioning**: `sharedFolder.s3.versioning` (default **off**; when the endpoint supports versioning, sync `--delete` becomes a versioned delete = 30-day recoverable via `sharedFolder.s3.noncurrentExpirationDays`, default 30). Rationale: the warm volume is the primary; S3 deletions mirror user intent (rclone `--delete`); versioning-on adds silent cost growth for data users deleted deliberately — off by default, on for compliance deployments. Parked-user deletion (account deletion) = prefix delete + versioned-objects purge lifecycle rule on the bucket (operator doc).
- **Never in S3**: secrets, dotfile wiring, anything the platform injects — content-agnostic by ruling 3; the prefix is bytes.

## 8. Ownership and access — the boards §authz seam, applied

Design 0067's authz discipline (PR #1703): ownership is a first-class field; access is a separate, additive seam; workers delegate as their user. /shared follows it exactly:

- **Owner**: the Workspace's `Spec.Owner.UserID` (`workspace_types.go:13-16`) — **user-level even for org workspaces** (deliberately NOT `tenantID()`/org-attributed, `constants.go:108-114`: org workspaces get org-attributed *tenant* labels, but the shared folder is the USER's — it follows them across personal and org workspaces, which is the feature: "scripts, rc files, datasets, tooling" are the person's).
- **Access (v1)**: the owning user, via their workspaces, RW. Nothing else. No sharing, no read-only grants, no org visibility.
- **Future collaboration = additive seam**: an `access` subresource (grant list: subject user/org + role reader/writer) added later changes ONLY the mount decision (whose workspaces get the mount) and the IAM policy width — never the ownership field. Recorded here so v1's shapes don't preclude it (the 0067 lesson: separate the fields now).
- **Data-flow note surfaced as open ruling OR-5**: an org workspace mounting the user's personal /shared is an egress channel by design (it is the user's own data). Orgs that must confine data flow need an org-policy switch (`org.policy.allowPersonalSharedFolder`, default on) — flagged for the owner, not decided here.

## 9. Rejected alternatives (transcribed verbatim from #1709 — keep the record)

> - **JuiceFS-NFS-over-S3**: stress findings — FUSE-free-server-mode-under-runsc risk, metadata engine becomes tier-0 platform state (loss = filesystem gone; hard-mount hangs platform-wide), per-user NFS-server instances required for lock correctness, four-layer latency stack (gofer→NFS→JuiceFS→Redis+S3).
> - **Cloud file CSI (EFS/Azure Files) direct**: cloud-only, no per-user quota on EFS, homelab gap.
> - **Per-workspace rclone sync**: retained ONLY as the fallback tier for no-Longhorn-RWX / tiny-quota deployments.

And the disqualifying ruling that killed the mounted-S3 family (ruling 2, §2): unsandboxed mount producers — FUSE sidecars, node-level userspace S3 clients, runc mount producers — are an attack vector; the sandboxed batch sync pod is the only S3 client.

## 10. Open rulings (owner menu — the design is complete under any answer)

| # | Question | Default proposed here | Notes |
|---|---|---|---|
| OR-1 | Park TTL default | 24h | issue's own suggestion; chart-overridable per deployment |
| OR-2 | Platform default quota | 50Gi | between the 10 GB casual and 100 GB power users; per-user max 200Gi |
| OR-3 | Org over-quota policy | reject create with clear error (org admin raises cap or user frees space) | vs. allow-but-park-oldest (complex, surprising) |
| OR-4 | S3 versioning default | off | §7 rationale; on for compliance deployments |
| OR-5 | Org policy: personal /shared inside org workspaces | allowed (default on) with per-org disable switch | §8 data-flow note |

## 11. Rollout sequencing

1. **CRD + controller** (`SharedVolume` CRD, shared-volume controller in the controller binary, chart values `sharedFolder.*`, StorageClass + VolumeSnapshotClass templates) — inert until a workspace references it (flag-gated like every delivery lane: `sharedFolder.enabled=false` default until the flip).
2. **Sync Job image** — rclone on the runtime base (already pullable; NO MinIO dependency anywhere, §3.5), built in the existing image lane (`image-build.yml` family) as `ghcr.io/lenaxia/llmsafespaces/synclet`.
3. **API surface** — quota settings + org accounting check at create/resize (`workspace_service.go` create path); `/shared` presence+quota in workspace responses.
4. **Frontend — minimal by charter**: quota used/limit display on the workspace view (the existing disk-usage banner pattern, #1686 lane); no file browser, no sharing UI.
5. **Operator docs** — `docs/operator/storage.md` gains the /shared section (layout diagram row, `user-shared` class, park/TTL/cadence knobs, S3 endpoint config, versioning guidance, the minio-delisting note); runbook gains park/crash-during-park recovery.
6. **Enablement — gated on the deferred evidence (2026-10-18 ruling)** — the FIRST real-cluster soak of the implementation lane is the spike's evidence leg: it runs the committed kit (`local/1709-shared-folder-spike.sh`, legs 0/A/B/C) against the deployment's actual storage, commits `results.txt` + leg logs into `design/evidence/shared-folder-spike/`, fills every `[R:...]` row in this document, and only then flips `sharedFolder.enabled=true` on the reference deployment. A leg-A FAIL at the soak is the charter's architecture stop. (`.github/workflows/shared-folder-spike.yml` becomes the scheduled regression shape once its file is on main.)

## 12. Verification trail (this branch @ the evidence commit; spike run ledger in `design/evidence/shared-folder-spike/README.md`)

- Workspace phase machine + sole-writer discipline: `pkg/apis/llmsafespaces/v1/workspace_types.go:237-249` (phases), `:493-503` (Phase + failureReason), US-23.3 annotation at `:229-235`.
- PVC build/shape: `controller/internal/workspace/pvc.go:53-80` (buildPVC, RWO/RWX by spec); staleness/owner-ref pattern `pvc.go:16-29`; `pendingTimedOut` `pvc.go:31-33`.
- Pod posture (the spike's template): `controller/internal/workspace/pod_builder.go:203-207` (container SC), `:209-221` (subPath mounts), `:469-473` (RuntimeClassName), `buildPodSecurityContext` `:632-677`; RuntimeClass resolution `:384-389`; `reconciler.go:71-76` (DefaultRuntimeClass wiring).
- TTL-then-act requeue pattern: `controller/internal/workspace/phase_suspend.go:161-180` (handleSuspended TTL → Terminating + RequeueAfter arithmetic).
- Storage settings trace + no-resize posture: `docs/operator/storage.md` (PVC-backed mounts, StorageClass selection, sizing, "PVCs are never resized", webhook ceiling `webhooks.maxWorkspaceStorageGi`); chart pin `helm/values.yaml:659-673` (defaultStorageClass), `:1280-1284` (maxWorkspaceStorageGi).
- Namespace model (per-(user,namespace) instance justification): `helm/values.yaml:9-13`, `:159-161`.
- Owner model + tenant attribution (the §8 contrast): `pkg/apis/llmsafespaces/v1/workspace_types.go:13-16`; `controller/internal/workspace/constants.go:108-114`.
- Prod Longhorn posture (StorageClass params, chart 1.12.1, snapshot-controller 5.3.0): talos-ops-prod `kubernetes/apps/storage/longhorn/app/{helm-release.yaml,storageclass-2r.yaml}`, `kubernetes/apps/storage/snapshot-controller/app/helm-release.yaml`.
- Boards authz seam: design 0067 (PR #1703) §2 "Authz (dual granularity)" + §4.7 — ownership user|org, access additive, workers delegate as their user.
- Stage-and-pull ownership + atomicity hygiene: #1500 / design 0060; #1056 / Epic 68 D3 (tmp+rename + boot scrub) — reused by §4.5 cleanup and §7 manifest.
- Spike kit: `local/1709-shared-folder-spike.sh` (legs), `local/lib/longhorn.sh` (Longhorn-in-kind), `local/lib/gvisor.sh` (pre-existing runsc provisioning, reused verbatim), `.github/workflows/shared-folder-spike.yml`; evidence in `design/evidence/shared-folder-spike/`.
