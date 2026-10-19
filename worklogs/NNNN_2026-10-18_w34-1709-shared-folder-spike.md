# Worklog: #1709 — /shared parked-PVC spike (kit + run ledger) and design doc 0068

**Date:** 2026-10-18
**Session:** w34 design lane (charter: design doc for #1709 + the three spike deliverables that gate it).
**Status:** Complete per the 2026-10-18 owner ruling (#1709): leg evidence DEFERRED to the implementation lane's first real-cluster soak — the run ledger (see the evidence README; row set is the record, not a count) is the recorded justification; the design doc's PENDING protocol carries the deferral; the PR is driven to APPROVED on that basis.

---

## Objective

Produce design doc 0068 for #1709 (the owner-settled parked-PVC architecture: RWX warm volume per user + S3 park), gated on the issue's three spike questions: (a) runsc × Longhorn RWX in a workspace-shaped pod, (b) 100 GB hydration throughput via parallel rclone, (c) snapshot→RO-mount in a gVisor'd pod. Design + spike only — no production implementation; scripts under `local/`, evidence under `design/evidence/shared-folder-spike/`, workflow on the `lenaxia-dind-runner` pool (kind + runsc + Longhorn 1.12.1 + snapshot-controller 5.3.0 — prod's pins; SeaweedFS 4.48 as the synthetic S3 tier).

## Work Completed

- Spike kit: `local/1709-shared-folder-spike.sh` (legs 0/A/B/C + selftest + render modes), `local/lib/longhorn.sh` (Longhorn-in-kind provisioning), `.github/workflows/shared-folder-spike.yml` (PR-triggered dispatch-equivalent, paths-filtered, concurrency-deduped).
- Design doc 0068 drafted: parked-PVC lifecycle state machine (Hydrating/Ready/Parking/Parked + the issue's four edge cases), pre-warm/Ready-gating, snapshot→RO→rclone sync at 15-min cadence, `user-shared` StorageClass (prod `longhorn-2r` parameter set), quota-as-PVC-size (platform/per-user/org-aggregate), S3 layout + retention/versioning, user-level ownership seam (boards 0067 §authz), rejected alternatives verbatim, OR-1..OR-5 open-rulings menu, rollout sequencing. All result claims are `[R:...]` rows explicitly gated on committed evidence.
- Review remediation across rounds 1–8 (C1–C4, N1, N-A/N-B/N-C, R1–R6): heredoc parse-fatal class dead (`\$i` single-escape + dash -n gate), journal writer whole-record appends, 4-way leg exit contract, share-manager wait after consumers, runc RuntimeClass + preflight, fail-fast job waits, real chart key, node image pre-warm via ctr, posture-claim scoping, digest pins, 4× disk budget.
- Run ledger maintained row-per-run through the lane; every failure root-caused or captured for diagnosis (see Blockers; earlier drafts of this entry carried three inaccurate rows and two run omissions — reconciled in the r21 pass against the GitHub API and run logs).

## Key Decisions

- **SeaweedFS 4.48 as the spike's S3 tier** (not MinIO): minio/minio is delisted from Docker Hub — fresh pulls fail everywhere; the prod-pinned SeaweedFS is pullable and is a real S3 implementation. Design consequence recorded: no platform component may assume docker.io/minio/*.
- **Seaweed IAM actions are plain names** (`Admin`, `Read`, …); scoped/regex forms match nothing → blanket 403s.
- **Longhorn-on-single-node needs `replica-soft-anti-affinity=true`** (chart default hard-affines a volume's replicas to distinct nodes — unsatisfiable on one node; RWX volume faults with a ganesha probe crash-loop). Now in the operator-doc section of 0068.
- **kind load + digest refs is a trap** (multi-arch manifest import dies on missing platform blobs — the pool's own 2026-09-07 class); node pre-warm uses `ctr -n k8s.io images pull --platform linux/amd64` with docker.io-qualified refs.
- **Leg 0 (RWO/RWX sanity under runc) gates the run**: a storage-path failure in the environment is NOT a runsc×RWX verdict — legs A–C skip with recorded rows and the run exits non-zero.

## Blockers

- **CLOSED BY OWNER RULING (2026-10-18): option (c) — evidence deferred to the implementation lane's first real-cluster soak.** Final mechanism record (five stories were falsified in sequence; this is the reconciled one): the pool's GENUINE limitation is the **Longhorn v1 engine `starting` hang** — replicas running, iscsid provably alive, engines never starting (runs 39079884685/39089893139/39093640739, and reproduced after the quoting fix in run 39178767180 (prereqs green at t=0, though iscsid measured dead at leg time — daemon-death and the nesting limit not separable in that run; the kit now revives and gates iscsid at leg time)), implicating the nsenter→iscsiadm initiator path under nested dind. The entire later "runner-identity / exec-drift / mid-exec mutation" record was **the kit's own quoting bug**: an apostrophe in a payload comment (`iscsid's`) closed the single-quoted `bash -c` string mid-script; everything after it executed on the CI RUNNER (uid 1001, zero caps) rather than the node, and `bash -n` was structurally blind (quotes balance file-wide). Withdrawn; fixed by heredoc-fed `bash -s` delivery + a standalone-parse pin. A green pool run is now plausible but unproven; the ruling's deferral stands unless the owner revisits. **Leg A was reached and failed on the storage path** (RWX never attached; pods never started; runsc never exercised) — no valid runsc×RWX verdict exists. `[R:...]` rows stay PENDING; the soak commits results and fills them; a leg-A FAIL there is the charter's architecture stop.

## Tests Run

- `bash local/1709-shared-folder-spike.sh selftest` — journal-parser fixtures (clean/torn-tail/gap/dupe/empty) + 4-way exit-contract helper. PASS locally.
- `bash local/1709-shared-folder-spike.sh render <dir>` + `dash -n` on every emitted body. PASS locally (non-vacuous: anchor + loop-construct + live-script drift pins in the Go test).
- `go test ./local/ -run 'TestSharedFolderSpike|TestLonghornLib|TestGvisorLib'` — 17 companion pins (incl. standalone payload-parse for longhorn.sh and gvisor.sh, and the iscsid-at-leg-time gate) (bash -n ×2, rendered dash -n + content + drift, `\$\$` dead signature, selftest, exit contract, fail-sentinel normalization, leg-0 shape incl. namespace ordering + scoped deletes, writer redirect, RuntimeClass preflight, hydration size gate, longhorn lib keys/params, workflow shape incl. ctr ref qualification). PASS.
- Spike runs: ledgered in `design/evidence/shared-folder-spike/README.md` row-per-run (remediation pushes structurally fire runs; rows are appended as they complete); zero green leg executions — the blocker above.

## Files Modified

- `local/1709-shared-folder-spike.sh` (new), `local/lib/longhorn.sh` (new), `local/shared_folder_spike_script_test.go` (new), `.github/workflows/shared-folder-spike.yml` (new), `design/0068_2026-10-16_shared-folder-parked-pvc.md` (new), `design/evidence/shared-folder-spike/README.md` (new), this worklog.

## Next Steps

1. Drive PR #1711 to APPROVED on the deferral basis (ruling cited in the doc Status); relay any reviewer objection to the deferral shape to the orchestrator — the ruling is citable.
2. Hand off to the implementation lane: first real-cluster soak runs the committed kit, commits results.txt + leg logs into `design/evidence/shared-folder-spike/`, fills every `[R:...]` in 0068, then flips `sharedFolder.enabled=true`.
3. Report the leg-A verdict (either way) on #1709 the moment the soak produces it.
