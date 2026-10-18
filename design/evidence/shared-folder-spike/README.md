# shared-folder-spike — #1709 evidence

Spike for the parked-PVC architecture's three gating questions (issue #1709).
Design doc: [`../../0068_2026-10-16_shared-folder-parked-pvc.md`](../0068_2026-10-16_shared-folder-parked-pvc.md).

## What ran

- **Runner:** self-hosted `lenaxia-dind-runner` (the US-70 delivery pool's privileged docker-in-dind set), GitHub run per the ledger below (branch `design/1709-shared-folder`).

## Run ledger

| Run | Outcome | Root cause / lesson (committed where it matters) |
|---|---|---|
| 38864577660 | fail @ image pull | minio/minio delisted from Docker Hub — design §3.5 |
| 38865021006 | fail @ image pull | same (tag was not the issue; the repo is gone) |
| 38865495450 | fail @ Longhorn wait | instance-manager async creation — poll-then-wait (`local/lib/longhorn.sh`) |
| 38865864638 | fail @ VSC apply | VolumeSnapshotClass needs snapshot-controller CRDs first — workflow step order |
| 38866952477 | fail @ leg A | createDefaultDiskLabeled created no disks — "Waiting for volume share" class |
| 38873829660 | fail @ legs A+B | replicaSoftAntiAffinity default hard-affine (single node) + Seaweed IAM plain action names |
| 38889584710 | fail @ chart fetch | transient GitHub 500 on the release-asset tgz — 3-attempt retry |
| 38892896347 | cancelled | first run to clear all provisioning; runner reclaimed mid-legs |
| 39075635564 | fail @ pre-warm | kind load + digest refs = multi-arch import trap — ctr pull on node |
| 39077806519 | fail @ pre-warm | ctr needs docker.io-qualified refs (bare repo@digest → dummy://) |
| 39079884685 | fail @ leg A | share-manager ganesha probe loop persists; full longhorn dumps captured |
| 39083785719 | fail @ legs 0+A | leg-0 probes hit a namespace that didn't exist yet (harness bug, fixed); RWX share still not converging |
| 39087698763 | cancelled | superseded by the r3-remediation push (concurrency cancel) |
| 39089893139 | fail @ leg 0 | capture landed: replicas RUNNING, engines hang in `starting`; share-manager healthy, waiting for attach |
| 39093640739 | fail @ leg 0 | instance-manager logs name it: engines fail `nsenter ... iscsiadm` — iscsiadm cannot reach iscsid |
| 39097290981 | fail @ leg 0 | iscsid alive at t=0, engines still fail — /run/lock/iscsi missing (deb tmpfiles never ran in the live node) |
| 39101239324 | fail @ prereqs | iscsid would not start at all (direct-launch stderr swallowed) |
| 39103144189 | fail @ prereqs | `mkdir /etc/iscsi: Permission denied` as root inside the kind node |
| 39104752680 | fail @ prereqs | same EACCES with `docker exec -u 0` pinned — host-level constraint |
| 39106627792 | fail @ prereqs | same EACCES (third consecutive) |
| 39108718318 | fail @ kind create | `etcdserver: request timed out` — failure now above every line the branch touches |
| 39149620957 | fail @ prereqs | **TERMINAL DIAGNOSIS**: `docker exec -u 0` lands as `uid=1001(runner)` with `CapEff: 0000000000000000` on an SELinux (`seclabel`) overlay — the runner's docker wrapper strips the uid and drops all capabilities; root-level node provisioning is impossible on this runner class |
| (green run) | — | results.txt + leg logs land here and fill every [R:...] in design 0068 — requires the owner's environment decision (#1709: non-nested runner / real cluster / evidence-deferred) |
- **Cluster:** kind v0.32.0, kindest/node v1.35.5, single node, `local/kind-cluster.yaml` (dind-nesting guards: `serviceSubnet 10.217.0.0/16`, iptables kube-proxy).
- **gVisor:** runsc + containerd-shim-runsc-v1 from the latest gVisor release bundle, sha512-verified, via `local/lib/gvisor.sh` (the US-70 pool recipe); RuntimeClass `gvisor`.
- **Longhorn:** chart **1.12.1** — the production cluster's version; node prereqs `open-iscsi` + `nfs-common` via `local/lib/longhorn.sh`; StorageClass `user-shared` (prod `longhorn-2r` parameter set, 2 replicas); snapshot-controller 5.3.0 (piraeus chart — prod's pin); VolumeSnapshotClass `user-shared-snap`.
- **S3 tier:** SeaweedFS `chrislusf/seaweedfs:4.48` in-cluster (prod's pin). **minio/minio is delisted from Docker Hub** — see the design doc §3.5; runs 38864577660/38865021006 record the delisting evidence.
- **Client:** rclone 1.67.0 (`rclone/rclone`), env-var S3 config, provider `Other`.
- **Pod posture:** every pod that MOUNTS THE LONGHORN VOLUME (ws-a/ws-b, ws-writer, hydration jobs, sync job) runs the exact workspace shape from `controller/internal/workspace/pod_builder.go` — `runtimeClassName: gvisor`, uid/gid/fsGroup 1000, `FSGroupChangePolicy: OnRootMismatch`, `runAsNonRoot`, `seccompProfile: RuntimeDefault`, `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]`, no SA token, no service links. The S3 tier and S3-only seed jobs are spike-side infrastructure (no Longhorn mount); SA tokens are disabled on every namespace object regardless.
- **Dataset:** synthetic `/dev/urandom`, two classes (128 MiB bulk × N, 256 KiB small × 4N) + per-file sha256 manifest; auto-sized to node free disk (S3 copy + 2 Longhorn replicas within 50% of free).

## Files

- `results.txt` — structured KEY=VALUE rows for legs 0/A/B/C (the design doc cites these as `[R:KEY]`). 0 files exist until the green run lands — every row is PENDING until then.
- `run-env.txt` — runner environment record (CPU/mem/disk/docker), written by the workflow's env step.
- `c-sync.log` — leg C sync pod: RO-mount check, manifest verify output, journal torn-tail parse, rclone push.
- `spike.log` — leg progress log (the script's own tee).
- Failed-run artifacts kept for the record: `run38865021006/` (minio delisting), `run38865864638/` (VSC-before-CRD ordering), `run38866952477/` (this run).

## Reproducing

```bash
gh workflow run shared-folder-spike.yml --ref <branch-with-this-file> \
  -f auto_gb=24 -f transfers="8 32"
# or: open/refresh a PR touching local/1709-shared-folder-spike.sh (the
# workflow fires from its PR; dispatch needs the file on main)
```
