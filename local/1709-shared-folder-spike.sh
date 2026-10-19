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
#   B — hydration throughput: parallel rclone S3(SeaweedFS)→Longhorn-RWX at
#       auto-sized volume, transfers sweep {8,32} under gVisor + a runc
#       comparison leg, plus no-change and 1 GiB-delta incremental resync.
#   C — snapshot→RO-mount: VolumeSnapshot taken DURING a live append
#       writer (crash consistency), restore PVC from snapshot, gVisor'd
#       sync pod mounts it readOnly, full-dataset manifest verify +
#       journal gap/dupe/torn-tail parse (gaps/dupes FAIL the leg), rclone
#       RO-view→S3 push timing, cleanup.
#
# Environment:
#   CLUSTER_NAME (default llmsafespaces-ci) / CTX (default kind-$CLUSTER_NAME)
#   NS (default spike1709)
#   AUTO_GB    — leg-B dataset size in GiB (default 24; auto-shrinks to
#                fit node free disk — SeaweedFS data + seed staging + 2
#                Longhorn replicas = 4× the dataset within half of free)
#   TRANSFERS  — rclone --transfers sweep (default "8 32")
#
# Results: /tmp/spike1709/results.txt (KEY=VALUE + verifier verdicts),
# full leg logs under /tmp/spike1709/. Exit contract: non-zero iff ANY
# leg fails (0, A, B, or C) — a green workflow check means every leg
# produced evidence.
#
# Self-test: `selftest` exercises the journal parser on synthetic
# fixtures (clean / torn-tail / gap / dupe / empty) and the exit-contract
# helper — local/shared_folder_spike_script_test.go drives it; `render
# <dir>` emits every generated pod/job script for dash -n validation.
set -uo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-llmsafespaces-ci}"
CTX="${CTX:-kind-${CLUSTER_NAME}}"
NS="${NS:-spike1709}"
AUTO_GB="${AUTO_GB:-24}"
TRANSFERS="${TRANSFERS:-8 32}"

OUT=/tmp/spike1709
RESULTS="$OUT/results.txt"

# Images: digest-pinned (resolved digests recorded in results.txt are
# the reproducibility record; re-pin on tag refresh).
# FINDING (env note): docker.io/minio/minio is DELISTED from Docker Hub
# (hub API "object not found"; pull fails with "repository does not
# exist" — spike runs 38864577660/38865021006) even though prod runs an
# old cached tag. The spike's S3 tier therefore runs SeaweedFS 4.48 —
# the version the prod cluster pins and runs today, pullable, real S3
# implementation. Recorded in the design doc's evidence section.
DEBIAN_IMG="debian@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587"
RCLONE_IMG="rclone/rclone@sha256:ec0f519fefc0d157b95ef30e230c0e945715e0235e4a868432622527f1d578f9"
S3_IMG="chrislusf/seaweedfs@sha256:4e61d15fd35994cb1e43e1e553dff106794841fd9a99ade2fc8c8bfce4d7872d"

# ---------------------------------------------------------------------------
# Journal record parser (leg C's crash-consistency gate). Shared by the
# sync job and selftest. Reads a journal on stdin, prints
# "JOURNAL_RECS=<n> JOURNAL_GAPS=<g> JOURNAL_DUPES=<d>". Verdict is the
# caller's: gaps/dupes > 0 = corruption; a torn FINAL line is acceptable
# under crash consistency; zero parsed records = vacuous = FAIL.
# Records: `REC%08d|<base64 4KiB>` one per line; seq strictly +1.
journal_parse() {
    awk -F'|' '
        NF==2 && $1 ~ /^REC[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]$/ {
            s = substr($1,4)+0
            if (p != "") {
                if (s == p) dup++
                else if (s != p+1) gap++
            }
            p = s; n++
        }
        END { printf "JOURNAL_RECS=%d JOURNAL_GAPS=%d JOURNAL_DUPES=%d\n", n+0, gap+0, dup+0 }
    '
}

# journal_verdict <file>: prints the parse + verdict line; exit 0 =
# consistent (≥1 record, no gaps, no dupes — torn tail tolerated).
journal_verdict() { # <journal-file>
    local out recs gaps dupes
    out=$(journal_parse <"$1")
    echo "$out"
    recs=$(printf '%s\n' "$out" | sed -n 's/.*JOURNAL_RECS=\([0-9]*\).*/\1/p')
    gaps=$(printf '%s\n' "$out" | sed -n 's/.*JOURNAL_GAPS=\([0-9]*\).*/\1/p')
    dupes=$(printf '%s\n' "$out" | sed -n 's/.*JOURNAL_DUPES=\([0-9]*\).*/\1/p')
    if [ "${gaps:-1}" != "0" ] || [ "${dupes:-1}" != "0" ]; then
        echo "JOURNAL_VERDICT=FAIL (gaps=${gaps:-?} dupes=${dupes:-?})"
        return 1
    fi
    if [ "${recs:-0}" -lt 1 ]; then
        echo "JOURNAL_VERDICT=FAIL (no records parsed)"
        return 1
    fi
    echo "JOURNAL_VERDICT=PASS (torn tail tolerated if present)"
    return 0
}

# Exit-contract helper: green iff ALL FOUR leg statuses (0/A/B/C) PASS.
leg_exit() { [ "$1" = "PASS" ] && [ "$2" = "PASS" ] && [ "$3" = "PASS" ] && [ "$4" = "PASS" ]; }

# ---------------------------------------------------------------------------
# selftest — journal-parser fixtures + exit-contract helper. Driven by
# local/shared_folder_spike_script_test.go; no cluster required.
# Assertions avoid `fn | grep -q` shapes: under `set -o pipefail` the
# verdict function's deliberate non-zero rc (corrupt journal) or a
# SIGPIPE from grep -q's early exit would poison the pipeline and fail
# correct cases — capture output, test rc and pattern separately.
selftest() {
    local fail=0 dir out rc
    dir=$(mktemp -d)
    printf 'REC00000000|%s\nREC00000001|%s\nREC00000002|%s\n' a b c >"$dir/clean.j"
    printf 'REC00000000|%s\nREC00000001|%s\nREC00000' a b >"$dir/torn.j"
    printf 'REC00000000|%s\nREC00000002|%s\n' a c >"$dir/gap.j"
    printf 'REC00000000|%s\nREC00000000|%s\n' a a >"$dir/dupe.j"
    : >"$dir/empty.j"

    out=$(journal_verdict "$dir/clean.j"); rc=$?
    { [ "$rc" = "0" ] && [[ $out == *"JOURNAL_VERDICT=PASS"* ]]; } || { echo "selftest: clean journal must PASS"; fail=1; }
    out=$(journal_verdict "$dir/torn.j"); rc=$?
    { [ "$rc" = "0" ] && [[ $out == *"JOURNAL_VERDICT=PASS"* ]]; } || { echo "selftest: torn tail must PASS (crash consistency)"; fail=1; }
    [[ $out == *"JOURNAL_RECS=2 "* ]] || { echo "selftest: torn tail counts only complete records"; fail=1; }
    out=$(journal_verdict "$dir/gap.j"); rc=$?
    { [ "$rc" != "0" ] && [[ $out == *"JOURNAL_VERDICT=FAIL"* ]]; } || { echo "selftest: gap journal must FAIL"; fail=1; }
    out=$(journal_verdict "$dir/dupe.j"); rc=$?
    { [ "$rc" != "0" ] && [[ $out == *"JOURNAL_VERDICT=FAIL"* ]]; } || { echo "selftest: dupe journal must FAIL"; fail=1; }
    out=$(journal_verdict "$dir/empty.j"); rc=$?
    { [ "$rc" != "0" ] && [[ $out == *"JOURNAL_VERDICT=FAIL"* ]]; } || { echo "selftest: empty journal must FAIL"; fail=1; }

    leg_exit PASS PASS PASS PASS || { echo "selftest: all-PASS must exit 0"; fail=1; }
    leg_exit PASS PASS FAIL PASS && { echo "selftest: leg-B FAIL must exit non-zero"; fail=1; }
    leg_exit PASS PASS PASS FAIL && { echo "selftest: leg-C FAIL must exit non-zero"; fail=1; }
    leg_exit PASS FAIL PASS PASS && { echo "selftest: leg-A FAIL must exit non-zero"; fail=1; }
    leg_exit FAIL PASS PASS PASS && { echo "selftest: leg-0 FAIL must exit non-zero"; fail=1; }

    rm -rf "$dir"
    if [ "$fail" = "0" ]; then echo "SELFTEST=PASS"; return 0; fi
    echo "SELFTEST=FAIL"; return 1
}

# ---------------------------------------------------------------------------
# render <dir> — emit every generated pod/job embedded script (the
# block-scalar bodies the heredocs produce at defaults) for external
# dash -n validation. No cluster required. Each file starts with the
# `# spike-script: <name>` anchor. Placeholders are substituted at the
# same values the live render would use.
render() { # dir
    local dir="$1"
    mkdir -p "$dir"
    local SEED_BIG=$(( AUTO_GB * 8 ))
    local SEED_SMALL=$(( AUTO_GB * 32 ))
    local LAST_T; LAST_T="$(echo $TRANSFERS | awk '{print $NF}')"
    local PVC_GB=$(( AUTO_GB + AUTO_GB / 4 + 1 ))
    local EXPECTED_BYTES=$(( AUTO_GB * 1024 * 1024 * 1024 ))
    local EXPECTED_90=$(( EXPECTED_BYTES * 9 / 10 ))

    { echo "# spike-script: seed"
      sed -e "s/@@SEED_SMALL@@/${SEED_SMALL}/g" -e "s/@@SEED_BIG@@/${SEED_BIG}/g" <<'REOF'
set -e
mkdir -p /data/big /data/small
i=0; while [ $i -lt @@SEED_SMALL@@ ]; do
  dd if=/dev/urandom of=/data/small/s-$i.bin bs=256k count=1 2>/dev/null
  i=$((i+1))
done
i=0; while [ $i -lt @@SEED_BIG@@ ]; do
  dd if=/dev/urandom of=/data/big/b-$i.bin bs=1M count=128 2>/dev/null
  i=$((i+1))
done
cd /data && find big small -type f | sort | xargs sha256sum > /data/manifest.txt
rclone copy /data/big spike:shared/big --transfers 16
rclone copy /data/small spike:shared/small --transfers 16
rclone copyto /data/manifest.txt spike:shared/manifest.txt
rclone size spike:shared || true
REOF
    } >"$dir/seed.sh"

    { echo "# spike-script: hydrate"
      sed -e "s/@@T@@/${LAST_T}/g" -e "s/@@E90@@/${EXPECTED_90}/g" -e "s/@@E@@/${EXPECTED_BYTES}/g" <<'REOF'
set -e
rclone sync spike:shared /shared --transfers @@T@@ --checkers @@T@@
sz=$(rclone size --json /shared | sed -n 's/.*"bytes":\([0-9]*\).*/\1/p')
[ -n "$sz" ] || { echo "HYD_SIZE=FAIL (no size)"; exit 1; }
if [ "$sz" -lt @@E90@@ ]; then echo "HYD_SIZE=FAIL ($sz bytes < 90% of @@E@@)"; exit 1; fi
echo "HYD_SIZE=OK bytes=$sz"
rclone size /shared || true
REOF
    } >"$dir/hydrate.sh"

    { echo "# spike-script: writer"; cat <<'REOF'
i=0
while [ $i -lt 100000 ]; do
  { printf 'REC%08d|' "$i"; dd if=/dev/urandom bs=4096 count=1 2>/dev/null | base64 -w0; printf '\n'; } >> /shared/journal.bin
  i=$((i+1))
done
REOF
    } >"$dir/writer.sh"

    { echo "# spike-script: sync"; cat <<'REOF'
set -e
echo "-- RO mount check"
if touch /shared/.rw-probe 2>/dev/null; then echo "RO_CHECK=FAIL (mount writable)"; exit 1; else echo "RO_CHECK=PASS"; fi
echo "-- manifest verify"
rclone cat spike:shared/manifest.txt > /tmp/manifest.txt
cd /shared && sha256sum -c /tmp/manifest.txt > /tmp/verify.out 2>&1 || { echo "MANIFEST=FAIL"; tail -20 /tmp/verify.out; exit 1; }
echo "MANIFEST=PASS files=$(grep -c ': OK' /tmp/verify.out || true)"
echo "-- journal parse (gaps/dupes FAIL; torn tail tolerated)"
awk -F'|' 'NF==2 && $1 ~ /^REC[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]$/ { s=substr($1,4)+0; if (p != "") { if (s == p) dup++; else if (s != p+1) gap++ } p=s; n++ } END { printf "JOURNAL_RECS=%d JOURNAL_GAPS=%d JOURNAL_DUPES=%d\n", n+0, gap+0, dup+0 }' /shared/journal.bin > /tmp/journal.out
cat /tmp/journal.out
JG=$(sed -n 's/.*JOURNAL_GAPS=\([0-9]*\).*/\1/p' /tmp/journal.out)
JD=$(sed -n 's/.*JOURNAL_DUPES=\([0-9]*\).*/\1/p' /tmp/journal.out)
JR=$(sed -n 's/.*JOURNAL_RECS=\([0-9]*\).*/\1/p' /tmp/journal.out)
[ "$JG" = "0" ] && [ "$JD" = "0" ] || { echo "JOURNAL_VERDICT=FAIL (gaps=$JG dupes=$JD)"; exit 1; }
[ "$JR" -ge 1 ] || { echo "JOURNAL_VERDICT=FAIL (no records)"; exit 1; }
echo "JOURNAL_VERDICT=PASS"
echo "-- rclone RO-view push"
rclone sync /shared spike:sync-copy --transfers 32
rclone size spike:sync-copy || true
echo "SYNC_POD=PASS"
REOF
    } >"$dir/sync.sh"

    echo "rendered: $(ls "$dir" | tr '\n' ' ')"
}

case "${1:-}" in
    selftest) selftest; exit $? ;;
    render) render "${2:?render needs a dir}"; exit 0 ;;
esac

mkdir -p "$OUT"
: >"$RESULTS"

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

# wait_job <name> <timeout-s>: fail FAST on a terminal-Failed job
# (kubectl's condition=complete wait polls the full timeout otherwise —
# review R1). Polls status.conditions every 10s.
wait_job() { # name timeout
    local name="$1" tmo="$2" deadline=$(( $(date +%s) + tmo )) cond
    while [ "$(date +%s)" -lt "$deadline" ]; do
        cond=$(${KC} get job "$name" -o jsonpath='{range .status.conditions[*]}{.type}={.status} {end}' 2>/dev/null || true)
        case "$cond" in
            *"Complete=True"*) return 0 ;;
            *"Failed=True"*)   echo "wait_job: job $name FAILED" >&2; return 1 ;;
        esac
        sleep 10
    done
    echo "wait_job: job $name timed out after ${tmo}s (conditions: ${cond:-none})" >&2
    return 1
}

# Full longhorn-system state dump — the run-38866952477 lesson: the
# volume sat at "Waiting for volume share to be available" for 28
# minutes with zero diagnostics captured.
lh_dump_state() {
    ${K} -n longhorn-system get pods -o wide >"$OUT/lh-pods.txt" 2>&1 || true
    ${K} -n longhorn-system get nodes.longhorn.io -o yaml >"$OUT/lh-nodes.yaml" 2>&1 || true
    ${K} -n longhorn-system get volumes.longhorn.io -o yaml >"$OUT/lh-volumes.yaml" 2>&1 || true
    ${K} -n longhorn-system get replicas.longhorn.io -o wide >"$OUT/lh-replicas.txt" 2>&1 || true
    ${K} -n longhorn-system logs daemonset/longhorn-manager --tail=100 >"$OUT/lh-manager.log" 2>&1 || true
    ${K} -n longhorn-system logs -l longhorn.io/component=instance-manager --tail=250 >"$OUT/lh-instance-manager.log" 2>&1 || true
    ${K} -n longhorn-system logs -l longhorn.io/component=share-manager --tail=80 >"$OUT/lh-share.log" 2>&1 || true
    ${K} -n longhorn-system describe pod -l longhorn.io/component=share-manager >"$OUT/lh-share-describe.txt" 2>&1 || true
    ${K} -n longhorn-system get events --sort-by=.lastTimestamp >"$OUT/lh-events.txt" 2>&1 || true
}

# capture_share_manager <prefix>: poll up to 6 min for a non-Terminating
# share-manager pod (prefer it over a Terminating corpse from a prior
# attach cycle); capture live + previous logs and the pod object. Sets
# SM_CAPTURE=captured|never-found.
capture_share_manager() { # prefix
    local prefix="$1" i name phase
    SM_NAME=""
    for i in $(seq 1 36); do
        while IFS=$'\t' read -r name phase; do
            if [ "$phase" != "Terminating" ]; then SM_NAME="$name"; break; fi
            [ -z "$SM_NAME" ] && SM_NAME="$name"   # fall back to a corpse
        done < <(${K} -n longhorn-system get pods --no-headers 2>/dev/null | awk '$1 ~ /share-manager/ {print $1"\t"$3}' | head -3)
        # NOTE: a terminating pod's .status.phase stays "Running" — the
        # Terminating signal lives in kubectl's STATUS column, which the
        # awk latch above already filters on. No phase re-check here.
        [ -n "$SM_NAME" ] && break
        sleep 10
    done
    if [ -n "${SM_NAME:-}" ]; then
        ${K} -n longhorn-system logs "${SM_NAME}" --tail=80 >"$OUT/${prefix}-share-manager-live.log" 2>&1 || true
        ${K} -n longhorn-system logs "${SM_NAME}" --previous --tail=80 >"$OUT/${prefix}-share-manager-prev.log" 2>&1 || true
        ${K} -n longhorn-system get pod "${SM_NAME}" -o yaml >"$OUT/${prefix}-share-manager-pod.yaml" 2>&1 || true
        SM_CAPTURE="captured(${SM_NAME})"
    else
        SM_CAPTURE="never-found"
    fi
}

rclone_env_yaml() { # the env pairs every rclone pod needs
    cat <<EOF
        - {name: RCLONE_CONFIG_SPIKE_TYPE, value: s3}
        - {name: RCLONE_CONFIG_SPIKE_PROVIDER, value: Other}
        - {name: RCLONE_CONFIG_SPIKE_ENDPOINT, value: "http://s3srv.${NS}.svc.cluster.local:8333"}
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
# iscsid liveness AT RUN TIME (not just provision time) — run 39097290981
# had it alive at t=0 and engines failing "can not connect to iSCSI
# daemon" 30 minutes later; the window must be visible in results.
# iscsid liveness, tri-stated by the probe's OWN exit code (N-R20-6a
# residual): pgrep rc 0 = alive, rc 1 = measured dead, anything else or
# an exec failure = EXEC_FAILED — never "dead" for an unmeasured state.
if docker exec "$NODE" pgrep -x iscsid >/tmp/iscsi-probe.$$ 2>/dev/null; then
    res "env_iscsid_alive=$(head -1 /tmp/iscsi-probe.$$ 2>/dev/null || echo unknown-pid)"
elif [ "$?" = "1" ] && docker exec "$NODE" true >/dev/null 2>&1; then
    res "env_iscsid_alive=dead"
else
    res "env_iscsid_alive=EXEC_FAILED"
fi
rm -f /tmp/iscsi-probe.$$
# Node-exec sanity row: the identity the spike's OWN execs see, beside
# whatever the prereqs payload measured (the historical identity-drift
# record was the kit's quoting bug, since fixed — this row now simply
# verifies delivery).
res "env_exec_identity_spike_probe=$(docker exec "$NODE" id -u 2>/dev/null || echo exec-failed)@$(docker exec "$NODE" hostname 2>/dev/null || echo unknown)"

# iscsid must be alive AT LEG TIME (r26: run 39178767180 measured
# env_iscsid_alive=dead — the daemon died between green prereqs and the
# legs; the engine failures then cannot be cleanly attributed to the
# nesting limit while their initiator's daemon is down). Attempt one
# revival, re-measure, and gate: legs do not run against a dead daemon.
if ! docker exec "$NODE" pgrep -x iscsid >/dev/null 2>&1; then
    warn "iscsid dead at leg time — attempting revival"
    docker exec "$NODE" sh -c 'systemctl start iscsid 2>/dev/null || iscsid >/dev/null 2>&1 || true' >/dev/null 2>&1 || true
    sleep 3
    if docker exec "$NODE" pgrep -x iscsid >/dev/null 2>&1; then
        res "env_iscsid_revived=yes"
        ok "iscsid revived at leg time"
    else
        res "env_iscsid_revived=no"
        res "s0_status=FAIL"
        res "s0_iscsid_at_legs=dead-revival-failed"
        res "leg_a=SKIPPED_ISCSID_DEAD"
        res "leg_b=SKIPPED_ISCSID_DEAD"
        res "leg_c=SKIPPED_ISCSID_DEAD"
        res "finished_at=$(date -u +%FT%TZ)"
        log "spike aborted — iscsid died between prereqs and legs and could not be revived"
        exit 1
    fi
fi

# RuntimeClass preflight — every runtimeClassName referenced below must
# be registered BEFORE any pod is created (review N-A: a missing runc
# class kills the B2 comparison at admission, silently to the legs).
for RC in gvisor runc; do
    if ! ${K} get runtimeclass "$RC" >/dev/null 2>&1; then
        res "env_runtimeclass_${RC}=MISSING"
        die "RuntimeClass ${RC} not registered — apply it before running the spike"
        exit 1
    fi
    res "env_runtimeclass_${RC}=OK"
done

# Disk budget — Longhorn refuses replica scheduling under 25% node free
# (prod incident note, talos-ops-prod storageclass-2r.yaml). FOUR copies
# of the dataset touch node disk: SeaweedFS data + seed staging scratch +
# 2 Longhorn replicas (review N-C). Keep all four inside half of free.
FREE_KB=$(docker exec "$NODE" df -k --output=avail / 2>/dev/null | tail -1 | tr -d ' ')
case "$FREE_KB" in
    ''|*[!0-9]*)
        die "node free-disk probe failed (exec returned '${FREE_KB:-empty}') — not a disk-size verdict"
        res "node_free_gb=EXEC_FAILED"
        exit 1
        ;;
esac
FREE_GB=$((FREE_KB / 1024 / 1024))
FIT_GB=$(( FREE_GB / 2 / 4 ))
if (( AUTO_GB > FIT_GB )); then
    warn "AUTO_GB=${AUTO_GB} exceeds auto-fit ${FIT_GB} GiB (node free=${FREE_GB} GiB) — shrinking"
    AUTO_GB=$FIT_GB
fi
res "node_free_gb=${FREE_GB}"
res "auto_gb=${AUTO_GB}"
res "transfers_sweep=${TRANSFERS// /,}"
res "expected_bytes=$(( AUTO_GB * 1024 * 1024 * 1024 ))"
(( AUTO_GB >= 4 )) || { die "auto-fit dataset < 4 GiB — node disk too small for a meaningful leg B"; exit 1; }

PVC_GB=$(( AUTO_GB + AUTO_GB / 4 + 1 ))
res "pvc_gb=${PVC_GB}"
EXPECTED_BYTES=$(( AUTO_GB * 1024 * 1024 * 1024 ))
EXPECTED_BYTES_90=$(( EXPECTED_BYTES * 9 / 10 ))

# ---------------------------------------------------------------------------
# Leg 0 — storage sanity WITHOUT gVisor: separates environment from
# architecture. If Longhorn's base path (RWO+runc) or the RWX share path
# (RWX+runc) is broken on this runner, leg A's FAIL is a runner-class
# finding, NOT a runsc×RWX verdict (runs 39079884685 et al: share-manager
# crash-loops its ganesha readiness probe before the engine ever starts;
# the manager meanwhile logs "No share-manager pod yet" against a pod the
# events show existing). Leg 0 captures the share-manager container logs
# BY NAME (label selectors missed the pod in prior dumps).
# ---------------------------------------------------------------------------
sect "LEG_0_storage_sanity"
S0_STATUS=FAIL

# The namespace must exist BEFORE leg 0 — it was previously created in
# leg A's apply, so leg 0's probes died NotFound (run 39083785719's
# s0-rwo-describe.txt: namespaces "spike1709" not found).
if ! ${K} create namespace "${NS}" --dry-run=client -o yaml | ${K} apply -f - >/dev/null; then
    die "namespace pre-create failed (dead context?) — aborting before any waits"
    exit 1
fi

cat <<EOF | ${K} apply -f - >/dev/null
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: rwo-test, namespace: ${NS}}
spec:
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 2Gi}}
  storageClassName: user-shared
---
apiVersion: v1
kind: Pod
metadata: {name: rwo-probe, namespace: ${NS}, labels: {spike: "1709"}}
spec:
  automountServiceAccountToken: false
  enableServiceLinks: false
  containers:
  - name: ws
    image: ${DEBIAN_IMG}
    command: ["/bin/sh", "-c"]
    args: ["sleep 1800"]
    volumeMounts: [{name: v, mountPath: /data}]
  volumes:
  - name: v
    persistentVolumeClaim: {claimName: rwo-test}
  restartPolicy: Never
EOF
if ${KC} wait --for=condition=ready pod/rwo-probe --timeout=600s >/dev/null 2>&1 \
   && ${KC} exec rwo-probe -- sh -c 'dd if=/dev/urandom of=/data/x bs=1M count=8 2>/dev/null && sha256sum /data/x' >"$OUT/s0-rwo.txt" 2>&1; then
    res "s0_rwo_runc=PASS"
    ok "leg0: Longhorn RWO + runc write/read works"
else
    res "s0_rwo_runc=FAIL"
    ${KC} describe pod rwo-probe >"$OUT/s0-rwo-describe.txt" 2>&1 || true
    lh_dump_state
fi

cat <<EOF | ${K} apply -f - >/dev/null
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: rwx-test, namespace: ${NS}}
spec:
  accessModes: [ReadWriteMany]
  resources: {requests: {storage: 2Gi}}
  storageClassName: user-shared
---
apiVersion: v1
kind: Pod
metadata: {name: rwx-probe, namespace: ${NS}, labels: {spike: "1709"}}
spec:
  automountServiceAccountToken: false
  enableServiceLinks: false
  containers:
  - name: ws
    image: ${DEBIAN_IMG}
    command: ["/bin/sh", "-c"]
    args: ["sleep 1800"]
    volumeMounts: [{name: v, mountPath: /data}]
  volumes:
  - name: v
    persistentVolumeClaim: {claimName: rwx-test}
  restartPolicy: Never
EOF
RWX0_T0=$(now_s)
if ${KC} wait --for=condition=ready pod/rwx-probe --timeout=1200s >/dev/null 2>&1 \
   && ${KC} exec rwx-probe -- sh -c 'dd if=/dev/urandom of=/data/x bs=1M count=8 2>/dev/null && sha256sum /data/x' >"$OUT/s0-rwx.txt" 2>&1; then
    res "s0_rwx_runc=PASS"
    res "s0_rwx_attach_s=$(elapsed "$RWX0_T0" "$(now_s)")"
    ok "leg0: Longhorn RWX share + runc works ($(elapsed "$RWX0_T0" "$(now_s)")s to ready)"
else
    res "s0_rwx_runc=FAIL"
    # decisive dumps: the share-manager pod lives only ~40s of every ~2m
    # backoff cycle (probe kill → recreate), so poll for up to 6 minutes
    # for a NON-TERMINATING pod and grab BOTH the live and --previous
    # container logs (every prior dump landed in a gap or latched a
    # Terminating corpse).
    capture_share_manager s0
    res "s0_share_manager_capture=${SM_CAPTURE:-never-found}"
    ${K} -n longhorn-system get engines.longhorn.io -o wide >"$OUT/s0-engines.txt" 2>&1 || true
    ${K} -n longhorn-system get replicas.longhorn.io -o wide >"$OUT/s0-replicas.txt" 2>&1 || true
    lh_dump_state
fi

${KC} delete pod rwo-probe rwx-probe --ignore-not-found >/dev/null 2>&1 || true
${KC} delete pvc rwo-test rwx-test --ignore-not-found >/dev/null 2>&1 || true
if ! sed -n '/### LEG_0/,/### LEG_A/p' "$RESULTS" | grep -q "=FAIL"; then S0_STATUS=PASS; fi
res "s0_status=${S0_STATUS}"

# ---------------------------------------------------------------------------
# Leg A — runsc × Longhorn RWX in workspace-shaped pods
# ---------------------------------------------------------------------------
sect "LEG_A_runsc_rwx"
A_STATUS=FAIL
if [ "$S0_STATUS" != "PASS" ]; then
    # Leg-0 failure gates the run (N-R8-3): the storage path is broken in
    # this environment — legs A-C could only re-demonstrate it, so they
    # are recorded SKIPPED and the run exits non-zero (the ~35 min of
    # leg waits are not paid).
    res "a_status=SKIPPED_LEG0_FAILED"
    res "b_status=SKIPPED_LEG0_FAILED"
    res "c_status=SKIPPED_LEG0_FAILED"
    res "leg_a=SKIPPED_LEG0_FAILED"
    res "leg_b=SKIPPED_LEG0_FAILED"
    res "leg_c=SKIPPED_LEG0_FAILED"
    res "finished_at=$(date -u +%FT%TZ)"
    log "spike complete — S0=${S0_STATUS} (legs A/B/C skipped: storage path broken)"
    exit 1
fi

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

# Apply the consumer pods FIRST (review N1: Longhorn creates a
# share-manager only when the RWX volume is first ATTACHED — waiting for
# it before any consumer exists is a no-op by construction). The pods
# schedule and stall on FailedAttachVolume while the share-manager
# boots; then poll-then-wait for the share pod.
ws_pod_yaml ws-a "sleep 7200" | ${K} apply -f - >/dev/null
ws_pod_yaml ws-b "sleep 7200" | ${K} apply -f - >/dev/null

SM_COUNT=0
for i in $(seq 1 30); do
    SM_COUNT=$(${K} -n longhorn-system get pods -l longhorn.io/component=share-manager -o name 2>/dev/null | wc -l | tr -d ' ')
    [ "$SM_COUNT" -ge 1 ] && break
    sleep 10
done
if [ "${SM_COUNT:-0}" -ge 1 ] && ${K} -n longhorn-system wait --for=condition=ready pod -l longhorn.io/component=share-manager --timeout=900s >/dev/null 2>&1; then
    res "a_share_manager=PASS"
    ok "share-manager Ready (attach-triggered)"
else
    res "a_share_manager=FAIL"
    warn "share-manager never appeared/Ready — dumping longhorn-system"
    lh_dump_state
fi

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
        res "a_cross_pod_rw=FAIL"; die "cross-pod RW failed"
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
        res "a_concurrent_writers=FAIL"
    fi

    # A3 uid/fsGroup mapping through the share-manager NFS export
    OWN_A=$(${KC} exec ws-a -- stat -c '%u:%g' /shared/from-a.bin 2>/dev/null || echo none)
    OWN_B=$(${KC} exec ws-b -- stat -c '%u:%g' /shared/from-a.bin 2>/dev/null || echo none)
    res "a_stat_from_a_ws_a=${OWN_A}"
    res "a_stat_from_a_ws_b=${OWN_B}"
    if [ "$OWN_A" = "1000:1000" ] && [ "$OWN_A" = "$OWN_B" ]; then
        res "a_uid_mapping=PASS"; ok "uid-1000 files visible 1000:1000 from both pods"
    else
        res "a_uid_mapping=FAIL"
    fi

    # A4 sequential throughput through the gofer (write ws-a / read ws-b)
    CACHE_SUFFIX=""
    if docker exec "$NODE" sh -c 'sync; echo 3 > /proc/sys/vm/drop_caches' >/dev/null 2>&1; then
        res "a_cache_drop=ok"
    else
        res "a_cache_drop=drop_failed"
        CACHE_SUFFIX="_hotcache"
        warn "cache drop exec failed — throughput rows carry the _hotcache suffix"
    fi
    TP_T0=$(now_s)
    ${KC} exec ws-a -- sh -c 'dd if=/dev/zero of=/shared/tp.bin bs=1M count=1024 2>/dev/null' >/dev/null 2>&1
    TP_T1=$(now_s)
    docker exec "$NODE" sh -c 'sync; echo 3 > /proc/sys/vm/drop_caches' >/dev/null 2>&1 || res "a_cache_drop_mid=drop_failed"
    ${KC} exec ws-b -- sh -c 'dd if=/shared/tp.bin of=/dev/null bs=1M 2>/dev/null' >/dev/null 2>&1
    TP_T2=$(now_s)
    res "a_dd_write_gvisor_mbs${CACHE_SUFFIX}=$(mbps "$(elapsed "$TP_T0" "$TP_T1")" 1)" # 1 GiB
    res "a_dd_read_gvisor_mbs${CACHE_SUFFIX}=$(mbps "$(elapsed "$TP_T1" "$TP_T2")" 1)"
    ok "leg A throughput recorded"

    # A_STATUS aggregates the leg's sub-verdicts
    if sed -n '/### LEG_A/,/### LEG_B/p' "$RESULTS" | grep -q "=FAIL"; then A_STATUS=FAIL; else A_STATUS=PASS; fi
else
    res "a_pods_ready=FAIL"
    die "workspace-shaped gVisor pods never Ready on RWX — LEG A FAILS"
    ${KC} describe pod ws-a ws-b >"$OUT/a-pod-describe.txt" 2>&1 || true
    ${KC} get events --sort-by=.lastTimestamp >"$OUT/a-events.txt" 2>&1 || true
    docker exec "$NODE" sh -c 'mount | grep -i nfs || true' >"$OUT/a-node-mounts.txt" 2>&1 || true
    capture_share_manager a
    res "a_share_manager_capture=${SM_CAPTURE:-never-found}"
    lh_dump_state
fi
res "a_status=${A_STATUS}"

# ---------------------------------------------------------------------------
# Leg B — hydration throughput (parallel rclone S3→Longhorn RWX)
# ---------------------------------------------------------------------------
sect "LEG_B_hydration"
B_STATUS=FAIL

# S3 tier: SeaweedFS all-in-one (master+volume+s3, one process), data on
# emptyDir (NOT Longhorn — keeps the measurement off the volume under
# test). IAM identity grants the spike key full S3 actions (PLAIN action
# names — "Admin:.*" regex forms match nothing and every call 403s; run
# 38873829660).
cat <<EOF | ${K} apply -f - >/dev/null
apiVersion: v1
kind: ConfigMap
metadata: {name: seaweed-s3-iam, namespace: ${NS}}
data:
  s3.json: |
    {"identities":[{"name":"spike","credentials":[{"accessKey":"spike1709","secretKey":"spike1709-spike1709"}],"actions":["Admin","Read","Write","List","Tagging"]}]}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: s3srv, namespace: ${NS}}
spec:
  replicas: 1
  selector: {matchLabels: {app: s3srv}}
  template:
    metadata: {labels: {app: s3srv}}
    spec:
      automountServiceAccountToken: false
      enableServiceLinks: false
      containers:
      - name: weed
        image: ${S3_IMG}
        args: ["server", "-dir=/data", "-master.volumeSizeLimitMB=8192", "-s3", "-s3.port=8333", "-s3.config=/etc/seaweed/s3.json"]
        ports: [{containerPort: 8333}]
        readinessProbe: {tcpSocket: {port: 8333}, initialDelaySeconds: 5, periodSeconds: 5}
        volumeMounts:
        - {name: data, mountPath: /data}
        - {name: iam, mountPath: /etc/seaweed, readOnly: true}
      volumes:
      - name: data
        emptyDir: {}
      - name: iam
        configMap: {name: seaweed-s3-iam}
---
apiVersion: v1
kind: Service
metadata: {name: s3srv, namespace: ${NS}}
spec:
  selector: {app: s3srv}
  ports: [{port: 8333, targetPort: 8333}]
EOF

if ! ${KC} rollout status deployment/s3srv --timeout=300s >/dev/null 2>&1; then
    res "b_s3srv_ready=FAIL"
    ${KC} describe deployment s3srv >"$OUT/b-s3srv.txt" 2>&1 || true
    ${KC} logs deployment/s3srv --tail=50 >"$OUT/b-s3srv.log" 2>&1 || true
else
    res "b_s3srv_ready=PASS"
    res "b_s3_impl=seaweedfs-4.48"

    # B0 seed — synthetic dataset on S3: two file classes (128 MiB bulk ×
    # AUTO_GB*8, 256 KiB small × AUTO_GB*32) + per-file manifest. The
    # final rclone size is REPORT-ONLY (|| true — review N-C: a reporting
    # failure must not fail a successful seed).
    SEED_BIG=$(( AUTO_GB * 8 ))
    SEED_SMALL=$(( AUTO_GB * 32 ))
    ${KC} delete job seed --ignore-not-found >/dev/null 2>&1
    cat <<EOF | ${K} apply -f - >/dev/null
apiVersion: batch/v1
kind: Job
metadata: {name: seed, namespace: ${NS}}
spec:
  backoffLimit: 0
  template:
    metadata: {labels: {spike: "1709"}}
    spec:
      automountServiceAccountToken: false
      enableServiceLinks: false
      restartPolicy: Never
      containers:
      - name: rclone
        image: ${RCLONE_IMG}
        command: ["/bin/sh", "-c"]
        args:
        - |
          set -e
          mkdir -p /data/big /data/small
          i=0; while [ \$i -lt ${SEED_SMALL} ]; do
            dd if=/dev/urandom of=/data/small/s-\$i.bin bs=256k count=1 2>/dev/null
            i=\$((i+1))
          done
          i=0; while [ \$i -lt ${SEED_BIG} ]; do
            dd if=/dev/urandom of=/data/big/b-\$i.bin bs=1M count=128 2>/dev/null
            i=\$((i+1))
          done
          cd /data && find big small -type f | sort | xargs sha256sum > /data/manifest.txt
          rclone copy /data/big spike:shared/big --transfers 16
          rclone copy /data/small spike:shared/small --transfers 16
          rclone copyto /data/manifest.txt spike:shared/manifest.txt
          rclone size spike:shared || true
        env:
$(rclone_env_yaml)
        resources: {requests: {cpu: "1"}}
        volumeMounts:
        - {name: scratch, mountPath: /data}
      volumes:
      - name: scratch
        emptyDir: {}
EOF
    T0=$(now_s)
    if wait_job seed 5400; then
        res "b_seed_s=$(elapsed "$T0" "$(now_s)")"
        res "b_dataset_gb=${AUTO_GB}"
        ok "seeded ~${AUTO_GB} GiB to SeaweedFS"
        SEED_OK=1
    else
        res "b_seed_s=FAIL"
        die "seed job failed"
        ${KC} logs job/seed >"$OUT/b-seed.log" 2>&1 || true
        SEED_OK=0
    fi

    if [ "${SEED_OK:-0}" = "1" ]; then
        hyd_job_yaml() { # name runtimeclass transfers
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
        args:
        - |
          set -e
          rclone sync spike:shared /shared --transfers $3 --checkers $3
          sz=\$(rclone size --json /shared | sed -n 's/.*"bytes":\([0-9]*\).*/\1/p')
          [ -n "\$sz" ] || { echo "HYD_SIZE=FAIL (no size)"; exit 1; }
          if [ "\$sz" -lt ${EXPECTED_BYTES_90} ]; then echo "HYD_SIZE=FAIL (\$sz bytes < 90% of ${EXPECTED_BYTES})"; exit 1; fi
          echo "HYD_SIZE=OK bytes=\$sz"
          rclone size /shared || true
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
            ${KC} delete job hyd-gvisor-t${T} --ignore-not-found >/dev/null 2>&1
            clean_shared
            hyd_job_yaml "hyd-gvisor-t${T}" gvisor "$T" | ${K} apply -f - >/dev/null
            TH0=$(now_s)
            if wait_job "hyd-gvisor-t${T}" 7200; then
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
        ${KC} delete job hyd-runc --ignore-not-found >/dev/null 2>&1
        clean_shared
        LAST_T="$(echo $TRANSFERS | awk '{print $NF}')"
        hyd_job_yaml hyd-runc runc "$LAST_T" | ${K} apply -f - >/dev/null
        TH0=$(now_s)
        if wait_job hyd-runc 7200; then
            TH1=$(now_s)
            res "b_hydrate_runc_t${LAST_T}_s=$(elapsed "$TH0" "$TH1")"
            res "b_hydrate_runc_t${LAST_T}_mbs=$(mbps "$(elapsed "$TH0" "$TH1")" "${AUTO_GB}")"
            ok "hydration runc --transfers=${LAST_T}: $(elapsed "$TH0" "$TH1")s"
        else
            res "b_hydrate_runc_t${LAST_T}_s=FAIL"
            ${KC} logs job/hyd-runc >"$OUT/b-hyd-runc.log" 2>&1 || true
        fi

        # B3 no-change resync (steady-state park→rehydrate of unchanged
        # data = metadata scan cost). Same dataset stays in place.
        ${KC} delete job hyd-nochange --ignore-not-found >/dev/null 2>&1
        hyd_job_yaml hyd-nochange gvisor "$LAST_T" | ${K} apply -f - >/dev/null
        TI0=$(now_s)
        if wait_job hyd-nochange 3600; then
            res "b_resync_nochange_s=$(elapsed "$TI0" "$(now_s)")"
            ok "no-change resync: $(elapsed "$TI0" "$(now_s)")s"
        else
            res "b_resync_nochange_s=FAIL"
        fi

        # B4 1 GiB delta: upload 8 new 128 MiB objects to S3, resync
        ${KC} delete job delta-seed --ignore-not-found >/dev/null 2>&1
        cat <<EOF | ${K} apply -f - >/dev/null
apiVersion: batch/v1
kind: Job
metadata: {name: delta-seed, namespace: ${NS}}
spec:
  backoffLimit: 0
  template:
    spec:
      automountServiceAccountToken: false
      enableServiceLinks: false
      restartPolicy: Never
      containers:
      - name: rclone
        image: ${RCLONE_IMG}
        command: ["/bin/sh", "-c"]
        args:
        - |
          set -e
          mkdir -p /data/delta
          i=0; while [ \$i -lt 8 ]; do
            dd if=/dev/urandom of=/data/delta/d-\$i.bin bs=1M count=128 2>/dev/null
            i=\$((i+1))
          done
          rclone copy /data/delta spike:shared/big --transfers 8
        env:
$(rclone_env_yaml)
        volumeMounts:
        - {name: scratch, mountPath: /data}
      volumes:
      - name: scratch
        emptyDir: {}
EOF
        if wait_job delta-seed 1800; then
            ${KC} delete job hyd-delta --ignore-not-found >/dev/null 2>&1
            hyd_job_yaml hyd-delta gvisor "$LAST_T" | ${K} apply -f - >/dev/null
            TD0=$(now_s)
            if wait_job hyd-delta 3600; then
                res "b_resync_1gib_delta_s=$(elapsed "$TD0" "$(now_s)")"
                ok "1 GiB delta resync: $(elapsed "$TD0" "$(now_s)")s"
            else
                res "b_resync_1gib_delta_s=FAIL"
            fi
        else
            res "b_resync_1gib_delta_s=FAIL (delta-seed job failed)"
        fi
        # B_STATUS aggregates the leg's sub-verdicts
        if ! sed -n '/### LEG_B/,/### LEG_C/p' "$RESULTS" | grep -q "=FAIL"; then B_STATUS=PASS; fi
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
    C_STATUS=SKIPPED_B_FAILED
else
    # C1 live append writer: `REC%08d|<b64>` lines appended WHOLE to the
    # journal (grouped redirect — the run-1 writer leaked everything but
    # the newline to pod stdout and the journal recorded blank lines,
    # making the crash-consistency gate vacuous).
    ${KC} delete pod ws-writer --ignore-not-found >/dev/null 2>&1
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
      while [ \$i -lt 100000 ]; do
        { printf 'REC%08d|' "\$i"; dd if=/dev/urandom bs=4096 count=1 2>/dev/null | base64 -w0; printf '\n'; } >> /shared/journal.bin
        i=\$((i+1))
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
                # journal parse (gaps/dupes FAIL) + rclone push of the
                # RO view. Every check exits non-zero on failure — a
                # green Job means every gate passed (review N-B/C3).
                ${KC} delete job sync --ignore-not-found >/dev/null 2>&1
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
          echo "-- journal parse (gaps/dupes FAIL; torn tail tolerated)"
          awk -F'|' 'NF==2 && \$1 ~ /^REC[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]\$/ { s=substr(\$1,4)+0; if (p != "") { if (s == p) dup++; else if (s != p+1) gap++ } p=s; n++ } END { printf "JOURNAL_RECS=%d JOURNAL_GAPS=%d JOURNAL_DUPES=%d\n", n+0, gap+0, dup+0 }' /shared/journal.bin > /tmp/journal.out
          cat /tmp/journal.out
          JG=\$(sed -n 's/.*JOURNAL_GAPS=\([0-9]*\).*/\1/p' /tmp/journal.out)
          JD=\$(sed -n 's/.*JOURNAL_DUPES=\([0-9]*\).*/\1/p' /tmp/journal.out)
          JR=\$(sed -n 's/.*JOURNAL_RECS=\([0-9]*\).*/\1/p' /tmp/journal.out)
          [ "\$JG" = "0" ] && [ "\$JD" = "0" ] || { echo "JOURNAL_VERDICT=FAIL (gaps=\$JG dupes=\$JD)"; exit 1; }
          [ "\$JR" -ge 1 ] || { echo "JOURNAL_VERDICT=FAIL (no records)"; exit 1; }
          echo "JOURNAL_VERDICT=PASS"
          echo "-- rclone RO-view push"
          rclone sync /shared spike:sync-copy --transfers 32
          rclone size spike:sync-copy || true
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
                if wait_job sync 7200; then
                    res "c_sync_pod_s=$(elapsed "$T4" "$(now_s)")"
                    ok "gVisor'd sync pod completed ($(elapsed "$T4" "$(now_s)")s)"
                    ${KC} logs job/sync >"$OUT/c-sync.log" 2>&1
                    grep -E "RO_CHECK|MANIFEST=|JOURNAL_RECS|JOURNAL_VERDICT|SYNC_POD|HYD_SIZE" "$OUT/c-sync.log" >>"$RESULTS" || true
                    grep -q "SYNC_POD=PASS" "$OUT/c-sync.log" 2>/dev/null && C_STATUS=PASS
                else
                    res "c_sync_pod_s=FAIL"
                    ${KC} logs job/sync >"$OUT/c-sync.log" 2>&1 || true
                fi

                # C5 cleanup + snapshot-chain verification
                ${KC} delete pvc spike-restore --ignore-not-found >/dev/null 2>&1
                ${KC} delete volumesnapshot spike-snap --ignore-not-found >/dev/null 2>&1
                sleep 15
                res "c_lh_snapshots_after_cleanup=$(${K} -n longhorn-system get snapshots.longhorn.io -o json 2>/dev/null | jq -r '.items | length' 2>/dev/null || echo unknown)"
            else
                res "c_restore_bound_s=FAIL"
                ${KC} describe pvc spike-restore >"$OUT/c-restore.txt" 2>&1 || true
            fi
        fi
        ${KC} delete pod ws-writer --ignore-not-found >/dev/null 2>&1 || true
    fi
fi
if [ "${C_STATUS}" != "SKIPPED_B_FAILED" ]; then
    res "c_status=${C_STATUS}"
fi

# ---------------------------------------------------------------------------
sect "SUMMARY"
res "leg_a=${A_STATUS}"
res "leg_b=${B_STATUS}"
res "leg_c=${C_STATUS}"
res "finished_at=$(date -u +%FT%TZ)"
log "spike complete — A=${A_STATUS} B=${B_STATUS} C=${C_STATUS} (results: ${RESULTS})"

# Exit contract: ANY failed leg ⇒ non-zero (a green workflow check means
# legs 0/A/B/C ALL produced evidence — reviews C4 + N-R8-3).
leg_exit "$S0_STATUS" "$A_STATUS" "$B_STATUS" "$C_STATUS" || exit 1
exit 0
