#!/usr/bin/env bash
# opencode-binary-contract.sh — bump-gate behavioral contract for the opencode
# runtime binary. Run this against EVERY candidate opencode before bumping
# runtimes/base/Dockerfile. It encodes the semantics learned on 2026-08-28
# (#1119) that the unit goldens cannot: execution behavior, not just shapes.
#
# Usage: OPENCODE_BIN=/path/to/opencode ./local/opencode-binary-contract.sh
#
# NOTE: validate polarity against PRISTINE upstream release tarballs. A
# locally installed "opencode" may be a fork build whose behavior is newer
# than its version constant (observed 2026-08-29: a host binary reporting
# 1.18.10 that already had the 1.18.15 Model.Ref shape).
# Requires: the candidate binary, curl, python3, jq is NOT required.
#
# Checks (fail = do not bump):
#   B1  V1 message route exists and executes (control)
#   B2  V2 prompt route exists (POST 400-on-invalid, not SPA/404)
#   B3  Model.Ref probe: {id,providerID} accepted, {modelID,...} rejected —
#       the exact drift that silently broke per-prompt overrides on 1.18.15
#   B4  Idle admission: admitted → prompted fires even when the turn dies at
#       model-resolve (defect-class) — the outbox completion seam depends on
#       prompted being observable for dying promotions
#   B5  Durable event log: per-session seq strictly monotonic, admitted and
#       prompted events carry messageID/assistantMessageID
set -euo pipefail

OPENCODE_BIN="${OPENCODE_BIN:-opencode}"
PORT="${PORT:-40960}"
BASE="http://127.0.0.1:${PORT}"
PW="contract-probe-password"
WORK="$(mktemp -d)"
FAIL=0

log() { printf '[contract] %s\n' "$*"; }
die() { printf '[contract] FAIL: %s\n' "$*" >&2; FAIL=1; }
ok()  { printf '[contract] ok: %s\n' "$*"; }

cleanup() { [[ -n "${SRV_PID:-}" ]] && kill "${SRV_PID}" 2>/dev/null || true; rm -rf "${WORK}" 2>/dev/null || true; }
trap cleanup EXIT

# ---- start candidate ----
# Isolate ALL candidate state (auth, db, logs) under the temp dir so the
# gate never touches a developer's or CI host's real opencode data.
export HOME="${WORK}/home"
export XDG_DATA_HOME="${HOME}/.local/share"
mkdir -p "${XDG_DATA_HOME}"
cd "${WORK}"
OPENCODE_SERVER_PASSWORD="${PW}" "${OPENCODE_BIN}" serve --hostname 127.0.0.1 --port "${PORT}" >"${WORK}/serve.log" 2>&1 </dev/null &
SRV_PID=$!
for i in $(seq 1 40); do
    curl -sf -m 2 -u "opencode:${PW}" "${BASE}/session" >/dev/null 2>&1 && break
    sleep 0.5
done
curl -sf -m 2 -u "opencode:${PW}" "${BASE}/session" >/dev/null 2>&1 || { die "server did not start"; tail -5 "${WORK}/serve.log" >&2 || true; exit 1; }
ok "candidate server up (pid ${SRV_PID})"

auth=(-u "opencode:${PW}" -H 'content-type: application/json')

# ---- B2/B3: route + Model.Ref shape via validation-error probes ----
P_CODE=$(curl -s -o "${WORK}/p.json" -w '%{http_code}' "${auth[@]}" -X POST -d '{}' "${BASE}/api/session/00000000000000000000000000/prompt" || true)
if [[ "${P_CODE}" == "400" ]] && grep -q InvalidRequestError "${WORK}/p.json"; then
    ok "B2 V2 prompt route present"
else
    die "B2 V2 prompt route missing (code ${P_CODE}: $(head -c 120 "${WORK}/p.json"))"
fi

# ---- session with a deliberately unresolvable model (defect-class trigger) ----
SID=$(curl -s "${auth[@]}" -X POST -d '{"directory":"'"${WORK}"'"}' "${BASE}/session" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')

M_PROBE_SID="${SID}"
M_CODE=$(curl -s -o "${WORK}/m.json" -w '%{http_code}' "${auth[@]}" -X POST -d '{"model":{}}' "${BASE}/api/session/${M_PROBE_SID}/model" || true)
if [[ "${M_CODE}" == "400" ]] && grep -q '\[\\"id\\"\]' "${WORK}/m.json"; then
    ok "B3 Model.Ref requires {id, providerID} (1.18.15+ shape)"
elif [[ "${M_CODE}" == "400" ]] && grep -q 'modelID' "${WORK}/m.json"; then
    die "B3 Model.Ref wants legacy {modelID} shape — adapter capability probe handles it, but the pinned floor expects the id shape; update goldens + floor review"
else
    die "B3 model probe indeterminate (code ${M_CODE}: $(head -c 120 "${WORK}/m.json"))"
fi

curl -s -o /dev/null "${auth[@]}" -X POST -d '{"model":{"id":"contract-bogus","providerID":"nonexistent"}}' "${BASE}/api/session/${SID}/model"

# ---- B1 control: V1 message route executes (and fail-fasts on the bogus model) ----
V1_CODE=$(curl -s -m 30 -o "${WORK}/v1.json" -w '%{http_code}' "${auth[@]}" -X POST \
    -d '{"parts":[{"type":"text","text":"v1 control"}]}' "${BASE}/session/${SID}/message" || true)
case "${V1_CODE}" in
    2*) ok "B1 V1 message executed";;
    5*) ok "B1 V1 message fail-fast 5xx (defect surfaced synchronously — the V1 contract)";;
    *)  die "B1 V1 message returned ${V1_CODE}: $(head -c 120 "${WORK}/v1.json")";;
esac

# ---- B4: idle V2 admission with defect-class death — prompted must fire ----
curl -s -o "${WORK}/adm.json" "${auth[@]}" -X POST \
    -d '{"prompt":{"text":"contract idle admit"},"delivery":"queue"}' "${BASE}/api/session/${SID}/prompt"
sleep 5
HIST=$(curl -s "${auth[@]}" "${BASE}/api/session/${SID}/history")
PROMPTED=$(printf '%s' "${HIST}" | python3 -c '
import json,sys
d=json.load(sys.stdin)
n=0
for e in d.get("data",[]):
    if e.get("type")=="session.next.prompted": n+=1
print(n)')
[[ "${PROMPTED}" -ge 1 ]] && ok "B4 prompted observable for idle admission (n=${PROMPTED})" \
    || die "B4 no prompted event — the outbox completion seam (#1119 fix) cannot observe this binary"

# ---- B5: durable event envelope integrity ----
printf '%s' "${HIST}" | python3 -c '
import json,sys
d=json.load(sys.stdin)
seqs=[]; bad=0
for e in d.get("data",[]):
    dur=e.get("durable") or {}
    s=dur.get("seq")
    if s is None: bad+=1; continue
    seqs.append(s)
    if e.get("type")=="session.next.prompt.admitted" and not (e.get("data",{}).get("messageID")):
        bad+=1
if bad or (seqs != sorted(seqs)) or len(set(seqs))!=len(seqs):
    sys.exit(1)
' && ok "B5 durable envelope: monotonic seq, admitted carries messageID" \
  || die "B5 durable event envelope degraded (seq gaps/dupes or missing messageID)"

if [[ "${FAIL}" -ne 0 ]]; then
    log "RESULT: FAIL — do NOT bump; see failures above"
    exit 1
fi
log "RESULT: PASS — behavioral contract holds for this binary"
