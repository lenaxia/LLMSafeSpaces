#!/usr/bin/env bash
# mcp-tools-liveprobe.sh — L3 leg of docs/testing/agentd-mcp-tools-test-plan.md.
#
# Encodes the empirically validated wire contract (opencode 1.18.15,
# live pod, 2026-09-13) as runnable probes against a workspace pod's
# own opencode (:4096). Creates a scratch session, exercises every
# surface the agentd MCP tools ride, prints PASS/FAIL per probe, and
# cleans up.
#
# Usage (inside a workspace pod):
#   scripts/mcp-tools-liveprobe.sh
#
# Exits 0 when all probes pass, 1 otherwise. Read-only except for the
# scratch session it creates and deletes.

set -u
PW=$(cat /sandbox-cfg/password 2>/dev/null) || { echo "FAIL: cannot read /sandbox-cfg/password"; exit 1; }
OC=http://127.0.0.1:4096
PASS=0; FAIL=0
note() { if [ "$1" = 0 ]; then echo "PASS: $2"; PASS=$((PASS+1)); else echo "FAIL: $2"; FAIL=$((FAIL+1)); fi }

S=$(curl -s -u "opencode:$PW" -X POST "$OC/session" -H 'Content-Type: application/json' -d '{"title":"liveprobe"}' | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])' 2>/dev/null)
[ -n "$S" ] && note 0 "session create (POST /session)" || note 1 "session create"

# rename: PATCH must actually change the title (POST was a silent no-op — the 2026-09-13 finding)
curl -s -o /dev/null -u "opencode:$PW" -X PATCH "$OC/session/$S" -H 'Content-Type: application/json' -d '{"title":"liveprobe-renamed"}'
T=$(curl -s -u "opencode:$PW" "$OC/session" | python3 -c "import json,sys; print([s.get('title') for s in json.load(sys.stdin) if s['id']=='$S'][0])" 2>/dev/null)
[ "$T" = "liveprobe-renamed" ] && note 0 "session rename takes effect (PATCH)" || note 1 "session rename takes effect (got '$T')"

# message list pagination header (only meaningful once ≥1 message
# exists — an empty history serves no cursor)
H=$(curl -s -D- -o /tmp/liveprobe.body -u "opencode:$PW" "$OC/session/$S/message?limit=2")
if grep -q '^\[$\|^\[\]' /tmp/liveprobe.body 2>/dev/null && [ "$(cat /tmp/liveprobe.body)" = "[]" ]; then
  note 0 "message pagination (empty history — no cursor expected)"
elif echo "$H" | grep -qi "x-next-cursor"; then
  note 0 "message pagination (X-Next-Cursor)"
else
  note 1 "message pagination"
fi
rm -f /tmp/liveprobe.body

# busy map shape
ST=$(curl -s -u "opencode:$PW" "$OC/session/status")
echo "$ST" | grep -q '{"type":' && note 0 "session statuses map" || note 1 "session statuses map"

# context endpoint serves JSON
CT=$(curl -s -u "opencode:$PW" "$OC/api/session/$S/context")
echo "$CT" | grep -q '"data"' && note 0 "context endpoint (V2)" || note 1 "context endpoint"

# catalog: limits + capabilities present
CA=$(curl -s -u "opencode:$PW" "$OC/config/providers")
echo "$CA" | grep -q 'limit' && echo "$CA" | grep -q 'image' && note 0 "model catalog (limits + capabilities)" || note 1 "model catalog"

# file part: schema acceptance + forwarding, using the pod's REAL
# default model (an invalid model ref 400s at validation — it would
# conflate schema acceptance with model rejection). 200 = the part
# parsed AND the turn completed.
DEFMODEL=$(curl -s -u "opencode:$PW" "$OC/config" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("model",""))' 2>/dev/null)
PROV="${DEFMODEL%%/*}"; MID="${DEFMODEL#*/}"
# A VALID 1x1 PNG is load-bearing: the attachment processor decodes
# the image and 400s on garbage bytes (conflating schema acceptance
# with decode failure). Session-default model: the attachment layer
# itself reports the model's vision capability in-band.
PNG="iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
R=$(curl -s -o /tmp/liveprobe.err -w "%{http_code}" -u "opencode:$PW" -X POST "$OC/session/$S/message" \
  -H 'Content-Type: application/json' \
  -d "{\"parts\":[{\"type\":\"text\",\"text\":\"If you received an image, reply IMAGE; otherwise reply NO-IMAGE. One word.\"},{\"type\":\"file\",\"mime\":\"image/png\",\"filename\":\"p.png\",\"url\":\"data:image/png;base64,$PNG\"}]}" \
  --max-time 120)
B=$(cat /tmp/liveprobe.err 2>/dev/null)
REPLY=$(echo "$B" | python3 -c 'import json,sys
try:
  d=json.load(sys.stdin)
  print(next((p["text"][:80] for p in d.get("parts",[]) if p.get("type")=="text"), ""))
except Exception: print("")' 2>/dev/null)
rm -f /tmp/liveprobe.err
if [ "$R" = 200 ]; then
  note 0 "file part acceptance (200; model said: ${REPLY:-<no text>})"
else
  note 1 "file part acceptance (status $R: $(echo "$B" | head -c 120))"
fi

# busy-block: second message to a busy session must NOT return quickly.
# Skipped by default (needs a long-running turn; set LIVEPROBE_BUSY=1).
if [ "${LIVEPROBE_BUSY:-0}" = 1 ]; then
  curl -s -o /dev/null -u "opencode:$PW" -X POST "$OC/session/$S/message" -H 'Content-Type: application/json' \
    -d '{"parts":[{"type":"text","text":"Write a 900-word essay about lighthouses."}]}' --max-time 300 &
  BPID=$!
  sleep 2
  CODE=$(curl -s -o /dev/null -w "%{http_code}" -u "opencode:$PW" -X POST "$OC/session/$S/message" -H 'Content-Type: application/json' -d '{"parts":[{"type":"text","text":"x"}]}' --max-time 8)
  kill $BPID 2>/dev/null
  curl -s -o /dev/null -u "opencode:$PW" -X POST "$OC/session/$S/abort" -H 'Content-Type: application/json' -d '{}'
  [ "$CODE" = 000 ] && note 0 "busy session blocks incoming messages (HTTP $CODE)" || note 1 "busy-block (got $CODE)"
fi

# cleanup
curl -s -o /dev/null -u "opencode:$PW" -X DELETE "$OC/session/$S"

echo "---"
echo "liveprobe: $PASS pass, $FAIL fail"
[ "$FAIL" = 0 ]

# --- send_message + abort_session probes (PR #1382) ---
S2=$(curl -s -u "opencode:$PW" -X POST "$OC/session" -H 'Content-Type: application/json' -d '{"title":"liveprobe-msg"}' | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])' 2>/dev/null)
[ -n "$S2" ] && note 0 "send_message target session created" || note 1 "send_message target create"
if [ -n "$S2" ]; then
  ST=$(curl -s -u "opencode:$PW" "$OC/session/status" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('$S2',{}).get('type','idle'))" 2>/dev/null)
  # abort on an idle session = server-side no-op 2xx (the seam's consolidated Client.Abort)
  AC=$(curl -s -o /dev/null -w "%{http_code}" -u "opencode:$PW" -X POST "$OC/session/$S2/abort" -H 'Content-Type: application/json' -d '{}' --max-time 10)
  [ "$AC" = 200 ] || [ "$AC" = 204 ] && note 0 "abort_session idle no-op (HTTP $AC)" || note 1 "abort_session idle no-op (HTTP $AC)"
  curl -s -o /dev/null -u "opencode:$PW" -X DELETE "$OC/session/$S2"
fi
