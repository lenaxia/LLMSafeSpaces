# shared-folder-spike — #1709 evidence

Spike for the parked-PVC architecture's three gating questions (issue #1709).
Design doc: [`../../0068_2026-10-16_shared-folder-parked-pvc.md`](../0068_2026-10-16_shared-folder-parked-pvc.md).

## What ran

- **Runner:** self-hosted `lenaxia-dind-runner` (the US-70 delivery pool's privileged docker-in-dind set), GitHub run **38866952477** (branch `design/1709-shared-folder`).
- **Cluster:** kind v0.32.0, kindest/node v1.35.5, single node, `local/kind-cluster.yaml` (dind-nesting guards: `serviceSubnet 10.217.0.0/16`, iptables kube-proxy).
- **gVisor:** runsc + containerd-shim-runsc-v1 from the latest gVisor release bundle, sha512-verified, via `local/lib/gvisor.sh` (the US-70 pool recipe); RuntimeClass `gvisor`.
- **Longhorn:** chart **1.12.1** — the production cluster's version; node prereqs `open-iscsi` + `nfs-common` via `local/lib/longhorn.sh`; StorageClass `user-shared` (prod `longhorn-2r` parameter set, 2 replicas); snapshot-controller 5.3.0 (piraeus chart — prod's pin); VolumeSnapshotClass `user-shared-snap`.
- **S3 tier:** SeaweedFS `chrislusf/seaweedfs:4.48` in-cluster (prod's pin). **minio/minio is delisted from Docker Hub** — see the design doc §3.5; runs 38864577660/38865021006 record the delisting evidence.
- **Client:** rclone 1.67.0 (`rclone/rclone`), env-var S3 config, provider `Other`.
- **Pod posture:** every data-path pod is the exact workspace shape from `controller/internal/workspace/pod_builder.go` — `runtimeClassName: gvisor`, uid/gid/fsGroup 1000, `FSGroupChangePolicy: OnRootMismatch`, `runAsNonRoot`, `seccompProfile: RuntimeDefault`, `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]`, no SA token, no service links.
- **Dataset:** synthetic `/dev/urandom`, two classes (128 MiB bulk × N, 256 KiB small × 4N) + per-file sha256 manifest; auto-sized to node free disk (S3 copy + 2 Longhorn replicas within 50% of free).

## Files

- `results.txt` — structured KEY=VALUE rows for all three legs (the design doc cites these as `[R:KEY]`).
- `run-env.txt` — runner environment record (CPU/mem/disk/docker).
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
