# Worklog: #1709 — /shared parked-PVC spike (kit + run ledger) and design doc 0068

**Date:** 2026-10-18
**Session:** w34 design lane (charter: design doc for #1709 + the three spike deliverables that gate it).
**Status:** In progress — spike kit hardened through 24 completed runs; design doc drafted with evidence-gated claims; green-run evidence pending (the open blocker is the pool's exec-identity contract, resolved this round by reverting to bare `docker exec` + a measured-identity gate — it sits BEFORE gVisor, so leg A has no verdict yet).

---

## Objective

Produce design doc 0068 for #1709 (the owner-settled parked-PVC architecture: RWX warm volume per user + S3 park), gated on the issue's three spike questions: (a) runsc × Longhorn RWX in a workspace-shaped pod, (b) 100 GB hydration throughput via parallel rclone, (c) snapshot→RO-mount in a gVisor'd pod. Design + spike only — no production implementation; scripts under `local/`, evidence under `design/evidence/shared-folder-spike/`, workflow on the `lenaxia-dind-runner` pool (kind + runsc + Longhorn 1.12.1 + snapshot-controller 5.3.0 — prod's pins; SeaweedFS 4.48 as the synthetic S3 tier).

## Work Completed

- Spike kit: `local/1709-shared-folder-spike.sh` (legs 0/A/B/C + selftest + render modes), `local/lib/longhorn.sh` (Longhorn-in-kind provisioning), `.github/workflows/shared-folder-spike.yml` (PR-triggered dispatch-equivalent, paths-filtered, concurrency-deduped).
- Design doc 0068 drafted: parked-PVC lifecycle state machine (Hydrating/Ready/Parking/Parked + the issue's four edge cases), pre-warm/Ready-gating, snapshot→RO→rclone sync at 15-min cadence, `user-shared` StorageClass (prod `longhorn-2r` parameter set), quota-as-PVC-size (platform/per-user/org-aggregate), S3 layout + retention/versioning, user-level ownership seam (boards 0067 §authz), rejected alternatives verbatim, OR-1..OR-5 open-rulings menu, rollout sequencing. All result claims are `[R:...]` rows explicitly gated on committed evidence.
- Review remediation across rounds 1–8 (C1–C4, N1, N-A/N-B/N-C, R1–R6): heredoc parse-fatal class dead (`\$i` single-escape + dash -n gate), journal writer whole-record appends, 4-way leg exit contract, share-manager wait after consumers, runc RuntimeClass + preflight, fail-fast job waits, real chart key, node image pre-warm via ctr, posture-claim scoping, digest pins, 4× disk budget.
- Run ledger driven to 23 ledgered entries (24 completed runs); every failure root-caused or captured for diagnosis (see Blockers).

## Key Decisions

- **SeaweedFS 4.48 as the spike's S3 tier** (not MinIO): minio/minio is delisted from Docker Hub — fresh pulls fail everywhere; the prod-pinned SeaweedFS is pullable and is a real S3 implementation. Design consequence recorded: no platform component may assume docker.io/minio/*.
- **Seaweed IAM actions are plain names** (`Admin`, `Read`, …); scoped/regex forms match nothing → blanket 403s.
- **Longhorn-on-single-node needs `replica-soft-anti-affinity=true`** (chart default hard-affines a volume's replicas to distinct nodes — unsatisfiable on one node; RWX volume faults with a ganesha probe crash-loop). Now in the operator-doc section of 0068.
- **kind load + digest refs is a trap** (multi-arch manifest import dies on missing platform blobs — the pool's own 2026-09-07 class); node pre-warm uses `ctr -n k8s.io images pull --platform linux/amd64` with docker.io-qualified refs.
- **Leg 0 (RWO/RWX sanity under runc) gates the run**: a storage-path failure in the environment is NOT a runsc×RWX verdict — legs A–C skip with recorded rows and the run exits non-zero.

## Blockers

- **RESOLVED THIS ROUND (r18's analysis of runs 39101239324-39149620957):** the four consecutive prereq EACCES failures were SELF-INFLICTED — the round-15 `-u 0` pin never delivers root on this pool (its execs land as `uid=1001(runner)`, the runner-image identity — kindest/node has no `runner` user — i.e. not inside the node at all), while the bare `docker exec` gvisor.sh has used EVERY run delivers root-in-node reliably. Reverted to bare exec with a measured-identity gate (`docker exec <node> id -u` probed and gated before any provisioning). Whether Longhorn v1 engines additionally hit the nsenter→iscsiadm dind-nesting limit remains UNANSWERED — the earlier engine-`starting` hangs were observed with iscsid provably alive, but under prereqs that never completed cleanly; the first gated clean run will answer it. The wall still sits BEFORE gVisor — leg A has no verdict. Owner decision still requested on #1709: (a) non-nested runner lane, (b) real-cluster spike, or (c) evidence-deferred merge — but (a) may now be unnecessary if the bare-exec revert lets the pool answer.

## Tests Run

- `bash local/1709-shared-folder-spike.sh selftest` — journal-parser fixtures (clean/torn-tail/gap/dupe/empty) + 4-way exit-contract helper. PASS locally.
- `bash local/1709-shared-folder-spike.sh render <dir>` + `dash -n` on every emitted body. PASS locally (non-vacuous: anchor + loop-construct + live-script drift pins in the Go test).
- `go test ./local/ -run 'TestSharedFolderSpike|TestLonghornLib'` — 12 companion pins (bash -n ×2, rendered dash -n + content + drift, `\$\$` dead signature, selftest, exit contract, fail-sentinel normalization, leg-0 shape incl. namespace ordering + scoped deletes, writer redirect, RuntimeClass preflight, hydration size gate, longhorn lib keys/params, workflow shape incl. ctr ref qualification). PASS.
- Spike runs: 24 on the pool (ledger in `design/evidence/shared-folder-spike/README.md`); zero green leg executions yet — the open blocker above.

## Files Modified

- `local/1709-shared-folder-spike.sh` (new), `local/lib/longhorn.sh` (new), `local/shared_folder_spike_script_test.go` (new), `.github/workflows/shared-folder-spike.yml` (new), `design/0068_2026-10-16_shared-folder-parked-pvc.md` (new), `design/evidence/shared-folder-spike/README.md` (new), this worklog.

## Next Steps

1. Land one CLEAN gated run (bare exec + identity gate): legs 0/A answer whether the pool can host Longhorn v1 at all; if engines still hang with iscsid provably alive, the nsenter→iscsiadm dind-nesting limit is confirmed and the owner decision (a)/(b)/(c) on #1709 becomes hard.
2. On a green run: commit `results.txt` + leg logs; fill every `[R:...]`; flip PR out of draft; iterate review to APPROVED; never self-merge.
3. Report the leg-A verdict (either way) on #1709 the moment it exists.
