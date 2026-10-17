# Worklog: #1709 — /shared parked-PVC spike (kit + runs) and design doc 0068

**Date:** 2026-10-16 (multi-session lane; this entry covers the whole w34 charter to date)
**Session:** w34 design lane, charter: design doc for #1709 (/shared persistent user folder, parked-PVC architecture) + the three spike deliverables that gate it.
**Status:** In progress — spike kit hardened through 8 runs; design doc drafted; green-run evidence + [R:...] fill pending.

---

## Objective

Produce design doc 0068 for #1709 (the owner-settled parked-PVC architecture: RWX warm volume per user + S3 park), gated on the issue's three spike questions: (a) runsc × Longhorn RWX in a workspace-shaped pod, (b) 100 GB hydration throughput via parallel rclone, (c) snapshot→RO-mount in a gVisor'd pod. Design + spike only — no production implementation; scripts under `local/`, evidence under `design/evidence/shared-folder-spike/`.

## What ran (spike environment)

`lenaxia-dind-runner` (the US-70 pool's privileged dind set), kind v0.32.0 (kindest/node v1.35.5, the repo's `local/kind-cluster.yaml` dind guards), runsc via the pool's `local/lib/gvisor.sh`, Longhorn chart 1.12.1 (prod's version), snapshot-controller 5.3.0 (prod's pin), S3 tier in-cluster. Workflow `.github/workflows/shared-folder-spike.yml` — dispatch needs the file on main, so the spike fires from its own PR (paths-filtered, concurrency-deduped; same-repo write-access author — inside the pool's trust domain).

## Runs and what each taught (8 runs, all evidence committed or in run logs)

| Run | Outcome | Root cause / fix |
|---|---|---|
| 38864577660 | fail @ image pull | **minio/minio is DELISTED from Docker Hub** (hub API object-not-found) — no fresh pull works anywhere, including the platform's own future sync pods. Design consequence recorded in doc §3.5 + §7. |
| 38865021006 | fail @ image pull | same (guessed tag wasn't the issue — the repo is gone) |
| 38865495450 | fail @ Longhorn wait | Longhorn creates instance-managers asynchronously; bare `kubectl wait` races creation ("no matching resources") → poll-then-wait |
| 38865864638 | fail @ VolumeSnapshotClass apply | ordering: VSC needs snapshot-controller CRDs first → reorder workflow steps |
| 38866952477 | fail @ leg A (28m attach stall) | `createDefaultDiskLabeled=true` created NO disks (needs labeled nodes) — replicas never scheduled; captured the "Waiting for volume share" failure class with zero diagnostics → added `lh_dump_state` |
| 38873829660 | fail @ leg A + B | chart default `replicaSoftAntiAffinity=false` hard-affines a volume's replicas to distinct nodes — impossible single-node (ganesha crash-loops, volume faults) → set soft anti-affinity true. Seaweed IAM actions are PLAIN names (`Admin`), not regex forms (`Admin:.*` matches nothing → blanket 403) |
| 38889584710 | fail @ chart fetch | transient GitHub 500 on the release-asset tgz → 3-attempt retry |
| 38892896347 | cancelled mid-legs | first run to clear all provisioning; cancelled (runner reclaimed); the review-4 verdict confirmed provisioning now works |

## Adversarial review findings (4 rounds, all fixed this pass)

Critical/major: **C1** heredoc `$$i` parse-fatal (renders PID-escape; legs B/C could never produce results) → single `\$` everywhere + `render` mode + dash -n test; **C2** doc asserted PASS/evidence that didn't exist → doc claims now gated on committed evidence; **C3** journal writer leaked records to stdout (blank-line journal) → grouped redirect; **C4** leg B/C FAIL exited 0 → `leg_exit` A/B/C contract; **N1** share-manager wait ran before any consumer (no-op by construction — Longhorn starts share pods on attach) → consumers first, poll-then-wait; **N-A** no `runc` RuntimeClass registered → workflow applies it + script preflight; **N-B** journal gate couldn't fail on corruption → parser verdicts FAIL on gaps/dupes/empty (selftest fixtures). Robustness: **R1** complete-waits hang on Failed jobs → `wait_job` polls conditions fail-fast; **R2** dead helm key → `preUpgradeChecker.jobEnabled`; **R3** runner pulls didn't warm the node → `kind load`; **R4** posture claim over-scoped → scoped to mount-bearing pods + SA-token off everywhere; **R5** SC param mismatch → `unmapMarkSnapChainRemoved` added; **R6** mutable tags → digest pins; **N-C** seed reporting/report fragility → `|| true` + 4× disk budget. Companion tests: `local/shared_folder_spike_script_test.go` (bash -n, dash -n on rendered bodies, `\$\$` signature dead, selftest, exit contract, preflight markers, workflow shape).

## Design doc 0068 (drafted, evidence-pending)

Full elaboration of the settled architecture: SharedVolume CRD + lifecycle state machine (Hydrating/Ready/Parking/Parked + crash-during-park, simultaneous-start race, suspend interplay), pre-warm at Pending + Ready-gating, snapshot→RO→rclone sync at 15m cadence, `user-shared` StorageClass (prod longhorn-2r params), quota-as-PVC-size (platform default / per-user max / per-org aggregate), S3 layout + retention/versioning + the minio-delisting operator note, user-level ownership seam per boards 0067 §authz (PR #1703), rejected alternatives transcribed verbatim, open-rulings menu (OR-1..OR-5), rollout sequencing. `[R:...]` rows fill from the committed green run before the doc's claims turn PASS.

## Next

1. Push the remediation set; let the PR-triggered run complete legs A/B/C.
2. Commit `results.txt` + leg logs under `design/evidence/shared-folder-spike/`; fill every `[R:...]`; finalize doc claims; worklog closes.
3. Iterate review to APPROVED; never self-merge.
