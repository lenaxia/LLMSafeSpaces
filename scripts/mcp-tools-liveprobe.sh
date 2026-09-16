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
# Exits 0 when all probes pass, 1 otherwise. NOT read-only: besides the
# scratch sessions it creates and deletes, the user-timezone leg
# PERSISTENTLY overwrites the pod's last-known browser zone (no reset
# endpoint exists). Run it on a pod whose user is not mid-session, or
# accept that the pod reports the probe's zone until the next browser
# (re)connect re-pushes the real one.

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


# --- send_message + abort_session probes (PR #1382) ---
S2=$(curl -s -u "opencode:$PW" -X POST "$OC/session" -H 'Content-Type: application/json' -d '{"title":"liveprobe-msg"}' | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])' 2>/dev/null)
[ -n "$S2" ] && note 0 "send_message target session created" || note 1 "send_message target create"
if [ -n "$S2" ]; then
  # send_message's wire path: POST /session/{id}/message on an IDLE target
  # completes synchronously (200 = accepted + turn ran; the exact reply
  # text is model-dependent and not asserted).
  DM=$(curl -s -u "opencode:$PW" "$OC/config" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("model",""))' 2>/dev/null)
  MC=$(curl -s -o /dev/null -w "%{http_code}" -u "opencode:$PW" -X POST "$OC/session/$S2/message" -H 'Content-Type: application/json' \
    -d "{\"parts\":[{\"type\":\"text\",\"text\":\"Reply with exactly: PROBE-OK\"}],\"model\":{\"modelID\":\"${DM#*/}\",\"providerID\":\"${DM%%/*}\"}}" --max-time 120)
  [ "$MC" = 200 ] && note 0 "send_message idle delivery (POST /message 200)" || note 1 "send_message idle delivery (HTTP $MC)"
  # abort on an idle session = server-side no-op 2xx (the consolidated Client.Abort)
  AC=$(curl -s -o /dev/null -w "%{http_code}" -u "opencode:$PW" -X POST "$OC/session/$S2/abort" -H 'Content-Type: application/json' -d '{}' --max-time 10)
  { [ "$AC" = 200 ] || [ "$AC" = 204 ]; } && note 0 "abort_session idle no-op (HTTP $AC)" || note 1 "abort_session idle no-op (HTTP $AC)"
  curl -s -o /dev/null -u "opencode:$PW" -X DELETE "$OC/session/$S2"
fi

echo "---"

# --- user-timezone probes (PR #1389) ---
# The live browser-zone channel end to end on a real pod: push a zone to
# this agentd, then get_datetime must report source:browser with the
# zone and a matching offset. Restores nothing — the zone persists
# until the next browser (re)connect re-pushes the real one; an
# already-connected session keeps reporting the probe's zone (see the
# header's NOT-read-only note).
# SKIPPED on agentd builds without the endpoint (pre-#1389 deployments):
# a 404 on the feature-detect probe skips the leg rather than failing.
TZR=$(curl -s -o /dev/null -w "%{http_code}" -u "opencode:$PW2" -X POST "http://127.0.0.1:4097/v1/user-timezone" -H 'Content-Type: application/json' -d '{"timezone":"Asia/Tokyo"}' --max-time 5)
if [ "$TZR" = 404 ]; then
  echo "SKIP: user-timezone probes (agentd predates PR #1389)"
else
[ "$TZR" = 200 ] && note 0 "user-timezone push accepted (200)" || note 1 "user-timezone push (HTTP $TZR)"
TZD=$(curl -s -u "opencode:$PW2" -X POST "http://127.0.0.1:4097/v1/mcp" -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{"name":"get_datetime","arguments":{}}}')
echo "$TZD" | grep -q '"source":"browser"' && note 0 "get_datetime source=browser after push" || note 1 "get_datetime source=browser"
echo "$TZD" | grep -q '"timezone":"Asia/Tokyo"' && note 0 "get_datetime reports pushed IANA zone" || note 1 "get_datetime IANA zone"
echo "$TZD" | grep -q '"utc_offset":"+09:00"' && note 0 "get_datetime Tokyo offset (+09:00)" || note 1 "Tokyo offset"
# Invalid zone rejected end to end.
TZB=$(curl -s -o /dev/null -w "%{http_code}" -u "opencode:$PW2" -X POST "http://127.0.0.1:4097/v1/user-timezone" -H 'Content-Type: application/json' -d '{"timezone":"Mars/Olympus_Mons"}' --max-time 5)
[ "$TZB" = 400 ] && note 0 "invalid zone rejected (400)" || note 1 "invalid zone rejection (HTTP $TZB)"
# Unauthenticated push rejected.
TZU=$(curl -s -o /dev/null -w "%{http_code}" -X POST "http://127.0.0.1:4097/v1/user-timezone" -H 'Content-Type: application/json' -d '{"timezone":"Asia/Tokyo"}' --max-time 5)
[ "$TZU" = 401 ] && note 0 "unauthenticated push rejected (401)" || note 1 "unauth rejection (HTTP $TZU)"
fi

echo "---"
echo "liveprobe: $PASS pass, $FAIL fail"
[ "$FAIL" = 0 ] && exit 0
exit 1
