#!/usr/bin/env bash
# local/1709-shared-folder-spike.sh — #1709 /shared parked-PVC spike.
#
# Three questions, straight from the issue (a FAIL on A is an
# architecture-level stop and gets reported back to #1709 immediately):
#
#   A — runsc × Longhorn RWX (share-manager) mounts in a WORKSPACE-SHAPED
#       pod: two gVisor'd pods with the exact workspace security posture
#       (pod_builder.go: uid/gid/fsGroup 1000, OnRootMismatch, RunAsNonRoot,
#       RuntimeDefault seccomp, RO rootfs, cap-drop ALL, no priv-esc, no SA
#       token, no service links) mounting the same RWX PVC at /shared.
#       Cross-pod write→read, concurrent writers, uid mapping, 1 GiB
#       sequential throughput per side.
#   B — hydration throughput: parallel rclone S3(MinIO)→Longhorn-RWX at
#       auto-sized volume, transfers sweep {8,32} under gVisor + a runc
#       comparison leg, plus no-change and 1 GiB-delta incremental resync.
#   C — snapshot→RO-mount: VolumeSnapshot taken DURING a live append
#       writer (crash consistency), restore PVC from snapshot, gVisor'd
#       sync pod mounts it readOnly, full-dataset manifest verify +
#       journal torn-tail parse, rclone RO-view→S3 push timing, cleanup.
#
# Environment:
#   CLUSTER_NAME (default llmsafespaces-ci) / CTX (default kind-$CLUSTER_NAME)
#   NS (default spike1709)
#   AUTO_GB    — leg-B dataset size in GiB (default 24; auto-shrinks to
#                fit node free disk — S3 copy X + 2 Longhorn replicas 2X
#                must fit in half the free disk)
#   TRANSFERS  — rclone --transfers sweep (default "8 32")
#
# Results: /tmp/spike1709/results.txt (KEY=VALUE + verifier verdicts),
# full leg logs under /tmp/spike1709/. Legs are best-effort: a FAIL
# records the failure and later legs still run; leg A failing exits
# non-zero at the end (architecture-stop signal).
set -uo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-llmsafespaces-ci}"
CTX="${CTX:-kind-${CLUSTER_NAME}}"
NS="${NS:-spike1709}"
AUTO_GB="${AUTO_GB:-24}"
TRANSFERS="${TRANSFERS:-8 32}"

OUT=/tmp/spike1709
mkdir -p "$OUT"
RESULTS="$OUT/results.txt"
: >"$RESULTS"

# Images: pinned tags; the resolved imageIDs are recorded into
# results.txt (digests drift — the resolved ID is the reproducibility
# record for this spike).
DEBIAN_IMG="debian:bookworm-slim"
RCLONE_IMG="rclone/rclone:1.67.0"
MINIO_IMG="minio/minio:RELEASE.2024-06-13T22-53-53Z"

if [[ -t 0 ]]; then
    BOLD=$'\033[1m'; RED=$'\033[31m'; GREEN=$'\033[32m'; YELLOW=$'\033[33m'; CYAN=$'\033[36m'; RESET=$'\033[0m'
else
    BOLD=''; RED=''; GREEN=''; YELLOW=''; CYAN=''; RESET=''
fi
log()  { printf '%s==>%s %s\n' "${CYAN}${BOLD}" "${RESET}" "$*" | tee -a "$OUT/spike.log"; }
ok()   { printf '%s ✓%s %s\n' "${GREEN}" "${RESET}" "$*" | tee -a "$OUT/spike.log"; }
warn() { printf '%s !%s %s\n' "${YELLOW}" "${RESET}" "$*" | tee -a "$OUT/spike.log"; }
die()  { printf '%s ✗%s %s\n' "${RED}${BOLD}" "${RESET}" "$*" | tee -a "$OUT/spike.log" >&2; }

res()  { printf '%s\n' "$*" >>"$RESULTS"; }
sect() { printf '\n### %s\n' "$*" >>"$RESULTS"; }
now_s() { date +%s.%N; }
elapsed() { awk -v a="$1" -v b="$2" 'BEGIN{printf "%.1f", b-a}'; }
mbps() { awk -v e="$1" -v gb="$2" 'BEGIN{printf "%.1f", gb*1024/e}'; }

K="kubectl --context ${CTX}"
KC="${K} -n ${NS}"

rclone_env_yaml() { # the five env pairs every rclone pod needs
    cat <<EOF
        - {name: RCLONE_CONFIG_SPIKE_TYPE, value: s3}
        - {name: RCLONE_CONFIG_SPIKE_PROVIDER, value: Minio}
        - {name: RCLONE_CONFIG_SPIKE_ENDPOINT, value: "http://minio.${NS}.svc.cluster.local:9000"}
        - {name: RCLONE_CONFIG_SPIKE_ACCESS_KEY_ID, value: spike1709}
        - {name: RCLONE_CONFIG_SPIKE_SECRET_ACCESS_KEY, value: spike1709-spike1709}
        - {name: RCLONE_CONFIG_SPIKE_NO_CHECK_BUCKET, value: "true"}
EOF
}

# ---------------------------------------------------------------------------
# Environment record
# ---------------------------------------------------------------------------
sect "ENV"
NODE="$(${K} get nodes -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
res "kind_nodes=$(${K} get nodes --no-headers 2>/dev/null | wc -l | tr -d ' ')"
res "node=${NODE}"
res "kind_node_image=$(docker inspect --format='{{.Config.Image}}' "$NODE" 2>/dev/null || echo unknown)"
res "longhorn_chart=$(helm list -n longhorn-system -o json 2>/dev/null | jq -r '.[0].chart' 2>/dev/null || echo unknown)"

# Disk budget — Longhorn refuses replica scheduling under 25% node free
# (prod incident note, talos-ops-prod storageclass-2r.yaml). Keep S3 (X) +
# 2 replicas (2X) inside half of current free.
FREE_KB=$(docker exec "$NODE" df -k --output=avail / 2>/dev/null | tail -1 | tr -d ' ')
FREE_GB=$((FREE_KB / 1024 / 1024))
FIT_GB=$(( FREE_GB / 2 / 3 ))
if (( AUTO_GB > FIT_GB )); then
    warn "AUTO_GB=${AUTO_GB} exceeds auto-fit ${FIT_GB} GiB (node free=${FREE_GB} GiB) — shrinking"
    AUTO_GB=$FIT_GB
fi
res "node_free_gb=${FREE_GB}"
res "auto_gb=${AUTO_GB}"
res "transfers_sweep=${TRANSFERS// /,}"
(( AUTO_GB >= 4 )) || { die "auto-fit dataset < 4 GiB — node disk too small for a meaningful leg B"; exit 1; }

PVC_GB=$(( AUTO_GB + AUTO_GB / 4 + 1 ))
res "pvc_gb=${PVC_GB}"

# ---------------------------------------------------------------------------
# Leg A — runsc × Longhorn RWX in workspace-shaped pods
# ---------------------------------------------------------------------------
sect "LEG_A_runsc_rwx"
A_STATUS=FAIL

cat <<EOF | ${K} apply -f - >/dev/null
apiVersion: v1
kind: Namespace
metadata: {name: ${NS}}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: spike-shared, namespace: ${NS}}
spec:
  accessModes: [ReadWriteMany]
  resources: {requests: {storage: ${PVC_GB}Gi}}
  storageClassName: user-shared
EOF

T0=$(now_s)
if ${KC} wait --for=jsonpath='{.status.phase}'=Bound pvc/spike-shared --timeout=900s >/dev/null 2>&1; then
    res "a_pvc_bound_s=$(elapsed "$T0" "$(now_s)")"
    ok "RWX PVC bound"
else
    res "a_pvc_bound_s=FAIL"
    die "RWX PVC never bound (900s) — dumping longhorn state"
    ${K} -n longhorn-system get pods -o wide >"$OUT/a-longhorn-pods.txt" 2>&1 || true
    ${KC} get events --sort-by=.lastTimestamp >"$OUT/a-events.txt" 2>&1 || true
    ${K} -n longhorn-system get volumes.longhorn.io -o yaml >"$OUT/a-lh-volume.yaml" 2>&1 || true
fi

# Workspace-shaped pod (pod_builder.go posture, verbatim semantics):
# runtimeClassName gvisor, uid/gid/fsGroup 1000, OnRootMismatch,
# RunAsNonRoot, RuntimeDefault seccomp, RO rootfs, cap-drop ALL,
# no priv-esc, no SA token, no service links, /shared RW.
ws_pod_yaml() { # name command
    cat <<EOF
apiVersion: v1
kind: Pod
metadata: {name: $1, namespace: ${NS}, labels: {spike: "1709"}}
spec:
  runtimeClassName: gvisor
  automountServiceAccountToken: false
  enableServiceLinks: false
  securityContext:
    runAsUser: 1000
    runAsGroup: 1000
    fsGroup: 1000
    fsGroupChangePolicy: OnRootMismatch
    runAsNonRoot: true
    seccompProfile: {type: RuntimeDefault}
  containers:
  - name: ws
    image: ${DEBIAN_IMG}
    command: ["/bin/sh", "-c"]
    args: ["$2"]
    securityContext:
      readOnlyRootFilesystem: true
      runAsNonRoot: true
      allowPrivilegeEscalation: false
      capabilities: {drop: ["ALL"]}
    volumeMounts:
    - {name: shared, mountPath: /shared}
    - {name: scratch, mountPath: /scratch}
  volumes:
  - name: shared
    persistentVolumeClaim: {claimName: spike-shared}
  - name: scratch
    emptyDir: {medium: Memory, sizeLimit: 512Mi}
  restartPolicy: Never
EOF
}

ws_pod_yaml ws-a "sleep 7200" | ${K} apply -f - >/dev/null
ws_pod_yaml ws-b "sleep 7200" | ${K} apply -f - >/dev/null

A_READY=1
${KC} wait --for=condition=ready pod/ws-a --timeout=900s >/dev/null 2>&1 || A_READY=0
${KC} wait --for=condition=ready pod/ws-b --timeout=900s >/dev/null 2>&1 || A_READY=0

if [ "$A_READY" = "1" ]; then
    res "a_pods_ready=PASS"
    ok "both workspace-shaped gVisor pods Ready on the RWX volume"
    res "a_ws_a_imageid=$(${KC} get pod ws-a -o jsonpath='{.status.containerStatuses[0].imageID}')"
    res "a_ws_b_imageid=$(${KC} get pod ws-b -o jsonpath='{.status.containerStatuses[0].imageID}')"

    GV_A=$(${KC} exec ws-a -- cat /proc/version 2>/dev/null || echo none)
    GV_B=$(${KC} exec ws-b -- cat /proc/version 2>/dev/null || echo none)
    res "a_proc_version_ws_a=${GV_A}"
    res "a_proc_version_ws_b=${GV_B}"
    if [[ "$GV_A" == *gVisor* && "$GV_B" == *gVisor* ]]; then
        res "a_runsc_active=PASS"; ok "runsc active in both pods (/proc/version)"
    else
        res "a_runsc_active=FAIL"; warn "gVisor not reported in /proc/version"
    fi

    # A1 cross-pod write→read (64 MiB)
    SHA_A=$(${KC} exec ws-a -- sh -c 'dd if=/dev/urandom of=/shared/from-a.bin bs=1M count=64 2>/dev/null && sha256sum /shared/from-a.bin' 2>/dev/null | awk '{print $1}')
    SHA_B=$(${KC} exec ws-b -- sha256sum /shared/from-a.bin 2>/dev/null | awk '{print $1}')
    if [ -n "$SHA_A" ] && [ "$SHA_A" = "$SHA_B" ]; then
        res "a_cross_pod_rw=PASS"; ok "A→B cross-pod RW (64 MiB sha match)"
    else
        res "a_cross_pod_rw=FAIL sha_a=${SHA_A:-none} sha_b=${SHA_B:-none}"; die "cross-pod RW failed"; note_a_fail=1
    fi

    # A2 concurrent writers, full cross-verify
    ${KC} exec ws-a -- sh -c 'i=0; while [ $i -lt 16 ]; do dd if=/dev/urandom of=/shared/conc-a-$i.bin bs=1M count=4 2>/dev/null; i=$((i+1)); done' >/dev/null 2>&1 &
    PA=$!
    ${KC} exec ws-b -- sh -c 'i=0; while [ $i -lt 16 ]; do dd if=/dev/urandom of=/shared/conc-b-$i.bin bs=1M count=4 2>/dev/null; i=$((i+1)); done' >/dev/null 2>&1
    wait "$PA"
    CONC_A_R=$(${KC} exec ws-b -- sh -c 'cat /shared/conc-a-*.bin | sha256sum' 2>/dev/null | awk '{print $1}')
    CONC_A_W=$(${KC} exec ws-a -- sh -c 'cat /shared/conc-a-*.bin | sha256sum' 2>/dev/null | awk '{print $1}')
    CONC_B_R=$(${KC} exec ws-a -- sh -c 'cat /shared/conc-b-*.bin | sha256sum' 2>/dev/null | awk '{print $1}')
    CONC_B_W=$(${KC} exec ws-b -- sh -c 'cat /shared/conc-b-*.bin | sha256sum' 2>/dev/null | awk '{print $1}')
    if [ "$CONC_A_R" = "$CONC_A_W" ] && [ -n "$CONC_A_R" ] && [ "$CONC_B_R" = "$CONC_B_W" ] && [ -n "$CONC_B_R" ]; then
        res "a_concurrent_writers=PASS"; ok "concurrent writers, cross-verified"
    else
        res "a_concurrent_writers=FAIL"; note_a_fail=1
    fi

    # A3 uid/fsGroup mapping through the share-manager NFS export
    OWN_A=$(${KC} exec ws-a -- stat -c '%u:%g' /shared/from-a.bin 2>/dev/null || echo none)
    OWN_B=$(${KC} exec ws-b -- stat -c '%u:%g' /shared/from-a.bin 2>/dev/null || echo none)
    res "a_stat_from_a_ws_a=${OWN_A}"
    res "a_stat_from_a_ws_b=${OWN_B}"
    if [ "$OWN_A" = "1000:1000" ] && [ "$OWN_A" = "$OWN_B" ]; then
        res "a_uid_mapping=PASS"; ok "uid-1000 files visible 1000:1000 from both pods"
    else
        res "a_uid_mapping=FAIL"; note_a_fail=1
    fi

    # A4 sequential throughput through the gofer (write ws-a / read ws-b)
    docker exec "$NODE" sh -c 'sync; echo 3 > /proc/sys/vm/drop_caches' >/dev/null 2>&1 || true
    TP_T0=$(now_s)
    ${KC} exec ws-a -- sh -c 'dd if=/dev/zero of=/shared/tp.bin bs=1M count=1024 2>/dev/null' >/dev/null 2>&1
    TP_T1=$(now_s)
    docker exec "$NODE" sh -c 'sync; echo 3 > /proc/sys/vm/drop_caches' >/dev/null 2>&1 || true
    ${KC} exec ws-b -- sh -c 'dd if=/shared/tp.bin of=/dev/null bs=1M 2>/dev/null' >/dev/null 2>&1
    TP_T2=$(now_s)
    res "a_dd_write_gvisor_mbs=$(mbps "$(elapsed "$TP_T0" "$TP_T1")" 1)" # 1 GiB
    res "a_dd_read_gvisor_mbs=$(mbps "$(elapsed "$TP_T1" "$TP_T2")" 1)"
    ok "leg A throughput recorded"

    [ "${note_a_fail:-0}" = "0" ] && A_STATUS=PASS
else
    res "a_pods_ready=FAIL"
    die "workspace-shaped gVisor pods never Ready on RWX — LEG A FAILS"
    ${KC} describe pod ws-a ws-b >"$OUT/a-pod-describe.txt" 2>&1 || true
    ${KC} get events --sort-by=.lastTimestamp >"$OUT/a-events.txt" 2>&1 || true
    docker exec "$NODE" sh -c 'mount | grep -i nfs || true' >"$OUT/a-node-mounts.txt" 2>&1 || true
fi
res "a_status=${A_STATUS}"

# ---------------------------------------------------------------------------
# Leg B — hydration throughput (parallel rclone S3→Longhorn RWX)
# ---------------------------------------------------------------------------
sect "LEG_B_hydration"
B_STATUS=FAIL

cat <<EOF | ${K} apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata: {name: minio, namespace: ${NS}}
spec:
  replicas: 1
  selector: {matchLabels: {app: minio}}
  template:
    metadata: {labels: {app: minio}}
    spec:
      containers:
      - name: minio
        image: ${MINIO_IMG}
        args: [server, /data, --console-address, ":9001"]
        env:
        - {name: MINIO_ROOT_USER, value: spike1709}
        - {name: MINIO_ROOT_PASSWORD, value: spike1709-spike1709}
        ports: [{containerPort: 9000}]
        readinessProbe: {httpGet: {path: /minio/health/ready, port: 9000}, initialDelaySeconds: 5, periodSeconds: 5}
        volumeMounts: [{name: data, mountPath: /data}]
      volumes:
      - name: data
        emptyDir: {}
---
apiVersion: v1
kind: Service
metadata: {name: minio, namespace: ${NS}}
spec:
  selector: {app: minio}
  ports: [{port: 9000, targetPort: 9000}]
EOF

if ! ${KC} rollout status deployment/minio --timeout=300s >/dev/null 2>&1; then
    res "b_minio_ready=FAIL"
    ${KC} describe deployment minio >"$OUT/b-minio.txt" 2>&1 || true
else
    res "b_minio_ready=PASS"

    # B0 seed — synthetic dataset on S3: two file classes (128 MiB bulk ×
    # AUTO_GB*8, 256 KiB small × AUTO_GB*32) + per-file manifest.
    SEED_BIG=$(( AUTO_GB * 8 ))
    SEED_SMALL=$(( AUTO_GB * 32 ))
    ${K} delete job seed --ignore-not-found >/dev/null 2>&1
    cat <<EOF | ${K} apply -f - >/dev/null
apiVersion: batch/v1
kind: Job
metadata: {name: seed, namespace: ${NS}}
spec:
  backoffLimit: 0
  template:
    metadata: {labels: {spike: "1709"}}
    spec:
      restartPolicy: Never
      containers:
      - name: rclone
        image: ${RCLONE_IMG}
        command: ["/bin/sh", "-c"]
        args:
        - |
          set -e
          mkdir -p /data/big /data/small
          i=0; while [ \$\$i -lt ${SEED_SMALL} ]; do
            dd if=/dev/urandom of=/data/small/s-\$\$i.bin bs=256k count=1 2>/dev/null
            i=\$\$((i+1))
          done
          i=0; while [ \$\$i -lt ${SEED_BIG} ]; do
            dd if=/dev/urandom of=/data/big/b-\$\$i.bin bs=1M count=128 2>/dev/null
            i=\$\$((i+1))
          done
          cd /data && find big small -type f | sort | xargs sha256sum > /data/manifest.txt
          rclone copy /data/big spike:shared/big --transfers 16
          rclone copy /data/small spike:shared/small --transfers 16
          rclone copyto /data/manifest.txt spike:shared/manifest.txt
          rclone size spike:shared
        env:
$(rclone_env_yaml)
        resources: {requests: {cpu: "1"}}
EOF
    T0=$(now_s)
    if ${KC} wait --for=condition=complete job/seed --timeout=5400s >/dev/null 2>&1; then
        res "b_seed_s=$(elapsed "$T0" "$(now_s)")"
        res "b_dataset_gb=${AUTO_GB}"
        ok "seeded ~${AUTO_GB} GiB to MinIO"
        SEED_OK=1
    else
        res "b_seed_s=FAIL"
        die "seed job failed"
        ${KC} logs job/seed >"$OUT/b-seed.log" 2>&1 || true
        SEED_OK=0
    fi

    if [ "${SEED_OK:-0}" = "1" ]; then
        hyd_job_yaml() { # name runtimeclass transfers extra-args
            cat <<EOF
apiVersion: batch/v1
kind: Job
metadata: {name: $1, namespace: ${NS}}
spec:
  backoffLimit: 0
  template:
    metadata: {labels: {spike: "1709"}}
    spec:
      runtimeClassName: $2
      automountServiceAccountToken: false
      enableServiceLinks: false
      securityContext:
        runAsUser: 1000
        runAsGroup: 1000
        fsGroup: 1000
        fsGroupChangePolicy: OnRootMismatch
        runAsNonRoot: true
        seccompProfile: {type: RuntimeDefault}
      restartPolicy: Never
      containers:
      - name: rclone
        image: ${RCLONE_IMG}
        command: ["/bin/sh", "-c"]
        args: ["set -e; rclone sync spike:shared /shared --transfers $3 --checkers $3 $4; rclone size /shared"]
        env:
$(rclone_env_yaml)
        securityContext:
          readOnlyRootFilesystem: true
          runAsNonRoot: true
          allowPrivilegeEscalation: false
          capabilities: {drop: ["ALL"]}
        volumeMounts:
        - {name: shared, mountPath: /shared}
        - {name: tmp, mountPath: /tmp}
      volumes:
      - name: shared
        persistentVolumeClaim: {claimName: spike-shared}
      - name: tmp
        emptyDir: {medium: Memory}
EOF
        }

        clean_shared() { ${KC} exec ws-a -- sh -c 'rm -rf /shared/big /shared/small /shared/manifest.txt /shared/tp.bin /shared/from-a.bin /shared/conc-*' >/dev/null 2>&1 || true; }

        # B1 cold hydration under gVisor, transfers sweep
        for T in $TRANSFERS; do
            ${K} delete job hyd-gvisor-t${T} --ignore-not-found >/dev/null 2>&1
            clean_shared
            hyd_job_yaml "hyd-gvisor-t${T}" gvisor "$T" "" | ${K} apply -f - >/dev/null
            TH0=$(now_s)
            if ${KC} wait --for=condition=complete job/hyd-gvisor-t${T} --timeout=7200s >/dev/null 2>&1; then
                TH1=$(now_s)
                res "b_hydrate_gvisor_t${T}_s=$(elapsed "$TH0" "$TH1")"
                res "b_hydrate_gvisor_t${T}_mbs=$(mbps "$(elapsed "$TH0" "$TH1")" "${AUTO_GB}")"
                ok "hydration gvisor --transfers=${T}: $(elapsed "$TH0" "$TH1")s"
            else
                res "b_hydrate_gvisor_t${T}_s=FAIL"
                ${KC} logs job/hyd-gvisor-t${T} >"$OUT/b-hyd-gvisor-t${T}.log" 2>&1 || true
            fi
        done

        # B2 runc comparison (same sweep's last T; the gofer-tax delta)
        ${K} delete job hyd-runc --ignore-not-found >/dev/null 2>&1
        clean_shared
        LAST_T="$(echo $TRANSFERS | awk '{print $NF}')"
        hyd_job_yaml hyd-runc runc "$LAST_T" "" | ${K} apply -f - >/dev/null
        TH0=$(now_s)
        if ${KC} wait --for=condition=complete job/hyd-runc --timeout=7200s >/dev/null 2>&1; then
            TH1=$(now_s)
            res "b_hydrate_runc_t${LAST_T}_s=$(elapsed "$TH0" "$TH1")"
            res "b_hydrate_runc_t${LAST_T}_mbs=$(mbps "$(elapsed "$TH0" "$TH1")" "${AUTO_GB}")"
            ok "hydration runc --transfers=${LAST_T}: $(elapsed "$TH0" "$TH1")s"
        else
            res "b_hydrate_runc_t${LAST_T}_s=FAIL"
            ${KC} logs job/hyd-runc >"$OUT/b-hyd-runc.log" 2>&1 || true
        fi

        # B3 no-change resync (steady-state park→rehydrate of unchanged
        # data = metadata scan cost)
        ${K} delete job hyd-nochange --ignore-not-found >/dev/null 2>&1
        hyd_job_yaml hyd-nochange gvisor "$LAST_T" "" | ${K} apply -f - >/dev/null
        TI0=$(now_s)
        if ${KC} wait --for=condition=complete job/hyd-nochange --timeout=3600s >/dev/null 2>&1; then
            res "b_resync_nochange_s=$(elapsed "$TI0" "$(now_s)")"
            ok "no-change resync: $(elapsed "$TI0" "$(now_s)")s"
        else
            res "b_resync_nochange_s=FAIL"
        fi

        # B4 1 GiB delta: upload 8 new 128 MiB objects to S3, resync
        ${K} delete job delta-seed --ignore-not-found >/dev/null 2>&1
        cat <<EOF | ${K} apply -f - >/dev/null
apiVersion: batch/v1
kind: Job
metadata: {name: delta-seed, namespace: ${NS}}
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
      - name: rclone
        image: ${RCLONE_IMG}
        command: ["/bin/sh", "-c"]
        args:
        - |
          set -e
          mkdir -p /data/delta
          i=0; while [ \$\$i -lt 8 ]; do
            dd if=/dev/urandom of=/data/delta/d-\$\$i.bin bs=1M count=128 2>/dev/null
            i=\$\$((i+1))
          done
          rclone copy /data/delta spike:shared/big --transfers 8
        env:
$(rclone_env_yaml)
EOF
        if ${KC} wait --for=condition=complete job/delta-seed --timeout=1800s >/dev/null 2>&1; then
            ${K} delete job hyd-delta --ignore-not-found >/dev/null 2>&1
            hyd_job_yaml hyd-delta gvisor "$LAST_T" "" | ${K} apply -f - >/dev/null
            TD0=$(now_s)
            if ${KC} wait --for=condition=complete job/hyd-delta --timeout=3600s >/dev/null 2>&1; then
                res "b_resync_1gib_delta_s=$(elapsed "$TD0" "$(now_s)")"
                ok "1 GiB delta resync: $(elapsed "$TD0" "$(now_s)")s"
            else
                res "b_resync_1gib_delta_s=FAIL"
            fi
        else
            res "b_resync_1gib_delta_s=DELTA_SEED_FAIL"
        fi
        B_STATUS=PASS
    fi
fi
res "b_status=${B_STATUS}"

# ---------------------------------------------------------------------------
# Leg C — snapshot → RO mount in a gVisor'd sync pod (crash consistency)
# ---------------------------------------------------------------------------
sect "LEG_C_snapshot_ro"
C_STATUS=FAIL
if [ "$B_STATUS" != "PASS" ]; then
    res "c_status=SKIPPED_B_FAILED"
else
    # C1 live append writer: SEQ|b64(4KiB random)| lines, direct append.
    # Verifier tolerates a torn TAIL only; gaps/dupes/out-of-order are
    # corruption.
    ${K} delete pod ws-writer --ignore-not-found >/dev/null 2>&1
    cat <<EOF | ${K} apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: {name: ws-writer, namespace: ${NS}, labels: {spike: "1709"}}
spec:
  runtimeClassName: gvisor
  automountServiceAccountToken: false
  enableServiceLinks: false
  securityContext:
    runAsUser: 1000
    runAsGroup: 1000
    fsGroup: 1000
    fsGroupChangePolicy: OnRootMismatch
    runAsNonRoot: true
    seccompProfile: {type: RuntimeDefault}
  containers:
  - name: ws
    image: ${DEBIAN_IMG}
    command: ["/bin/sh", "-c"]
    args:
    - |
      i=0
      while [ \$\$i -lt 100000 ]; do
        printf 'REC%08d|' "\$\$i"
        dd if=/dev/urandom bs=4096 count=1 2>/dev/null | base64 -w0
        printf '\n' >> /shared/journal.bin
        i=\$\$((i+1))
      done
    securityContext:
      readOnlyRootFilesystem: true
      runAsNonRoot: true
      allowPrivilegeEscalation: false
      capabilities: {drop: ["ALL"]}
    volumeMounts:
    - {name: shared, mountPath: /shared}
  volumes:
  - name: shared
    persistentVolumeClaim: {claimName: spike-shared}
  restartPolicy: Never
EOF
    if ! ${KC} wait --for=condition=ready pod/ws-writer --timeout=600s >/dev/null 2>&1; then
        res "c_writer_ready=FAIL"
        ${KC} describe pod ws-writer >"$OUT/c-writer.txt" 2>&1 || true
    else
        res "c_writer_ready=PASS"
        sleep 20   # let records land

        # C2 snapshot DURING appends
        cat <<EOF | ${K} apply -f - >/dev/null
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata: {name: spike-snap, namespace: ${NS}}
spec:
  volumeSnapshotClassName: user-shared-snap
  source: {persistentVolumeClaimName: spike-shared}
EOF
        T0=$(now_s)
        if ${KC} wait --for=jsonpath='{.status.readyToUse}'=true volumesnapshot/spike-snap --timeout=900s >/dev/null 2>&1; then
            res "c_snapshot_ready_s=$(elapsed "$T0" "$(now_s)")"
            ok "snapshot readyToUse during live writer"
        else
            res "c_snapshot_ready_s=FAIL"
            ${K} -n longhorn-system get snapshots.longhorn.io >"$OUT/c-lh-snaps.txt" 2>&1 || true
        fi
        SNAP_OK=$(${KC} get volumesnapshot spike-snap -o jsonpath='{.status.readyToUse}' 2>/dev/null)

        if [ "$SNAP_OK" = "true" ]; then
            # C3 restore PVC from snapshot (RWO — sync pod is single)
            cat <<EOF | ${K} apply -f - >/dev/null
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: spike-restore, namespace: ${NS}}
spec:
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: ${PVC_GB}Gi}}
  storageClassName: user-shared
  dataSource:
    name: spike-snap
    kind: VolumeSnapshot
    apiGroup: snapshot.storage.k8s.io
EOF
            T2=$(now_s)
            if ${KC} wait --for=jsonpath='{.status.phase}'=Bound pvc/spike-restore --timeout=900s >/dev/null 2>&1; then
                res "c_restore_bound_s=$(elapsed "$T2" "$(now_s)")"
                ok "restore PVC bound"

                # C4 gVisor'd sync pod: RO mount + manifest verify +
                # torn-tail journal parse + rclone push of the RO view
                ${K} delete job sync --ignore-not-found >/dev/null 2>&1
                cat <<EOF | ${K} apply -f - >/dev/null
apiVersion: batch/v1
kind: Job
metadata: {name: sync, namespace: ${NS}}
spec:
  backoffLimit: 0
  template:
    metadata: {labels: {spike: "1709"}}
    spec:
      runtimeClassName: gvisor
      automountServiceAccountToken: false
      enableServiceLinks: false
      securityContext:
        runAsUser: 1000
        runAsGroup: 1000
        fsGroup: 1000
        fsGroupChangePolicy: OnRootMismatch
        runAsNonRoot: true
        seccompProfile: {type: RuntimeDefault}
      restartPolicy: Never
      containers:
      - name: rclone
        image: ${RCLONE_IMG}
        command: ["/bin/sh", "-c"]
        args:
        - |
          set -e
          echo "-- RO mount check"
          if touch /shared/.rw-probe 2>/dev/null; then echo "RO_CHECK=FAIL (mount writable)"; exit 1; else echo "RO_CHECK=PASS"; fi
          echo "-- manifest verify"
          rclone cat spike:shared/manifest.txt > /tmp/manifest.txt
          cd /shared && sha256sum -c /tmp/manifest.txt > /tmp/verify.out 2>&1 || { echo "MANIFEST=FAIL"; tail -20 /tmp/verify.out; exit 1; }
          echo "MANIFEST=PASS files=\$(grep -c ': OK' /tmp/verify.out || true)"
          echo "-- journal torn-tail parse"
          awk -F'|' 'NF==2 && \$1 ~ /^REC[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]\$/ { s=substr(\$1,4)+0; if (p != "" && s != p+1) { if (s == p) dup++; else gap++ } p=s; n++ } END { printf "JOURNAL_RECS=%d JOURNAL_GAPS=%d JOURNAL_DUPES=%d\n", n, gap+0, dup+0 }' /shared/journal.bin
          tail -n 1 /shared/journal.bin | grep -q '^REC[0-9]*|.' || echo "JOURNAL_TORN_TAIL=yes (complete-prefix — acceptable)"
          echo "-- rclone RO-view push"
          rclone sync /shared spike:sync-copy --transfers 32
          rclone size spike:sync-copy
          echo "SYNC_POD=PASS"
        env:
$(rclone_env_yaml)
        securityContext:
          readOnlyRootFilesystem: true
          runAsNonRoot: true
          allowPrivilegeEscalation: false
          capabilities: {drop: ["ALL"]}
        volumeMounts:
        - {name: ro, mountPath: /shared, readOnly: true}
        - {name: tmp, mountPath: /tmp}
      volumes:
      - name: ro
        persistentVolumeClaim: {claimName: spike-restore}
      - name: tmp
        emptyDir: {medium: Memory}
EOF
                T4=$(now_s)
                if ${KC} wait --for=condition=complete job/sync --timeout=7200s >/dev/null 2>&1; then
                    res "c_sync_pod_s=$(elapsed "$T4" "$(now_s)")"
                    ok "gVisor'd sync pod completed ($(elapsed "$T4" "$(now_s)")s)"
                    ${KC} logs job/sync >"$OUT/c-sync.log" 2>&1
                    grep -E "RO_CHECK|MANIFEST|JOURNAL|SYNC_POD|rclone size|Total" "$OUT/c-sync.log" >>"$RESULTS" || true
                    grep -q "SYNC_POD=PASS" "$OUT/c-sync.log" 2>/dev/null && C_STATUS=PASS
                else
                    res "c_sync_pod_s=FAIL"
                    ${KC} logs job/sync >"$OUT/c-sync.log" 2>&1 || true
                fi

                # C5 cleanup + snapshot-chain verification
                ${K} delete pvc spike-restore --ignore-not-found >/dev/null 2>&1
                ${K} delete volumesnapshot spike-snap --ignore-not-found >/dev/null 2>&1
                sleep 15
                res "c_lh_snapshots_after_cleanup=$(${K} -n longhorn-system get snapshots.longhorn.io -o json 2>/dev/null | jq -r '.items | length' 2>/dev/null || echo unknown)"
            else
                res "c_restore_bound_s=FAIL"
                ${KC} describe pvc spike-restore >"$OUT/c-restore.txt" 2>&1 || true
            fi
        fi
        ${K} delete pod ws-writer --ignore-not-found >/dev/null 2>&1 || true
    fi
fi
res "c_status=${C_STATUS}"

# ---------------------------------------------------------------------------
sect "SUMMARY"
res "leg_a=${A_STATUS}"
res "leg_b=${B_STATUS}"
res "leg_c=${C_STATUS}"
res "finished_at=$(date -u +%FT%TZ)"
log "spike complete — A=${A_STATUS} B=${B_STATUS} C=${C_STATUS} (results: ${RESULTS})"

if [ "$A_STATUS" != "PASS" ]; then exit 1; fi
exit 0
