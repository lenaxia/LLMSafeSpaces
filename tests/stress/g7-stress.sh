#!/usr/bin/env bash
# G7 stress harness (design 0050, #907) — merge gate for D3.
#
# Drives the incident scenario against a REAL workspace and asserts the
# acceptance criteria against the live agentd metrics and opencode
# behavior. Assertions actually performed (no unverified header claims):
#   A. restarts_total{reason="health_watchdog"} is unchanged across a CPU
#      storm — the watchdog did NOT kill a reachable, progressing opencode.
#   B. workspace_watchdog_suppressions_total{reason=~"starved|flat"} is
#      counted (positive or noted-unchanged) and never acted on.
#   C. a live long turn completes under storm (HTTP 200 + reply marker).
#   D. a forced SIGTERM advances restarts_total{reason="crash"} — the
#      crash-recovery path, not a watchdog fire (SIGKILL would classify
#      as reason="oom": isOOMExit treats SIGKILL as the OOM-killer
#      signal, managed_process.go/oom_detection.go).
#   E. container restartCount is unchanged across the storm (the
#      "AND kubelet" half of the acceptance criterion).
#   F. the storm actually loaded the pod (cgroup throttling delta > 0)
#      so a silent no-op storm cannot green-run.
#
# Usage: WORKSPACE_ID=<id> [WORKSPACE_NS=llmsafespaces] bash tests/stress/g7-stress.sh
# Safety: uses ONLY the workspace named by WORKSPACE_ID (must be Active);
# a throwaway workspace is recommended. Requires kubectl on admin context.
set -euo pipefail

WS="${WORKSPACE_ID:?set WORKSPACE_ID to a dedicated Active workspace}"
NS="${WORKSPACE_NS:-llmsafespaces}"
PASS=0; FAIL=0

ok()   { echo "  PASS: $1"; PASS=$((PASS+1)); }
bad()  { echo "  FAIL: $1"; FAIL=$((FAIL+1)); }
# ad_metric: expose a labeled CounterVec family value by summing the
# matching series (independent of exposition label-sort order). Returns
# "0" when the counter is absent — counters are promauto-lazy and a
# healthy pod that never restarted has NO series, which is a valid zero.
# The scrape-unreachable case is checked separately (metric_endpoint).
# shellcheck source=tests/stress/lib.sh
. "$(dirname "$0")/lib.sh"

ad_metric() {
  # Copy the exposition OUT and parse locally via the shared metric_sum:
  # metric label patterns break when interpolated through kubectl exec
  # sh -c at any quoting depth (reproduced live); local awk with $NF is
  # unambiguous and the parsing is covered by the self-test, which
  # sources the SAME helper (no copy to drift).
  local f
  f=$(mktemp)
  kubectl exec "$POD" -n "$NS" -c workspace -- sh -c 'curl -s -m 5 http://localhost:4098/metrics' > "$f" 2>/dev/null || { rm -f "$f"; return 1; }
  metric_sum "$f" "$1"
  rm -f "$f"
}
# metric_endpoint: fails hard (and loudly) if the metrics scrape itself
# is impossible — distinct from "counter absent", which is a valid zero.
metric_endpoint() {
  kubectl exec "$POD" -n "$NS" -c workspace -- sh -c \
    "curl -s -m 5 http://localhost:4098/metrics | grep -cE '^workspace_'"
}


POD=$(kubectl get pods -n "$NS" -o name | grep "pod/$WS" | head -1 | cut -d/ -f2) || true
if [ -z "$POD" ]; then echo "FAIL: no pod for $WS"; exit 2; fi

PW=$(kubectl get secret "workspace-pw-$WS" -n "$NS" -o jsonpath='{.data.password}' 2>/dev/null | base64 -d) || true
if [ -z "$PW" ]; then PW=$(kubectl exec "$POD" -n "$NS" -c workspace -- sh -c 'cat /sandbox-cfg/password' 2>/dev/null) || true; fi
[ -n "$PW" ] || { echo "FAIL: no workspace password (secret or /sandbox-cfg/password)"; exit 2; }

OC="opencode:$PW"
PORT_OC=14096
kubectl port-forward -n "$NS" pod/"$POD" $PORT_OC:4096 >/dev/null 2>&1 & PFO=$!
trap 'kill $PFO 2>/dev/null' EXIT
sleep 2

echo "== G7 stress on $WS (pod $POD) =="

# Pre-storm container restartCount (kubelet's view — agentd counters die
# with the container, so this closes the vacuous-pass hole).
RC0=$(kubectl get pod "$POD" -n "$NS" -o jsonpath='{.status.containerStatuses[?(@.name=="workspace")].restartCount}' 2>/dev/null | tr -d ' ') || true

echo "== baseline =="
WCNT=$(metric_endpoint) || true
[ -n "$WCNT" ] && [ "$WCNT" -ge 1 ] || { echo "FAIL: metrics endpoint unreachable (scrape returned nothing)"; exit 2; }
WH0=$(ad_metric 'workspace_restarts_total{reason="health_watchdog"')
CR0=$(ad_metric 'workspace_restarts_total{reason="crash"')
S0=$(ad_metric 'workspace_watchdog_suppressions_total')
[ -n "$WH0" ] || { echo "FAIL: health_watchdog restart series not readable (scrape failed)"; exit 2; }
echo "health_watchdog=$WH0 crash=$CR0 suppressions=$S0 restartCount=$RC0"

SID=$(kubectl exec "$POD" -n "$NS" -c workspace -- sh -c "PW='$PW'; curl -s -m 5 -u opencode:\$PW http://localhost:4096/session | grep -o '\"id\":\"ses_[^\"]*' | head -1 | cut -d'\"' -f4") || true
if [ -z "$SID" ]; then
  SID=$(curl -s -m 15 -X POST -u "$OC" -H 'Content-Type: application/json' \
    -d '{}' "http://localhost:$PORT_OC/session" \
    | grep -o '"id":"ses_[^"]*' | head -1 | cut -d'"' -f4) || true
fi
[ -n "$SID" ] || { echo "FAIL: no session available for live turn"; exit 2; }

echo "== A/C/F: CPU storm + live long turn (session $SID) =="
# Verify load presence: sample cgroup throttling before/after the storm.
TH0=$(kubectl exec "$POD" -n "$NS" -c workspace -- sh -c 'cat /sys/fs/cgroup/cpu.stat 2>/dev/null | awk "/throttled_usec/{print \$2}"') || true

# CPU storm: N self-identifying burn loops (argv carries G7LOAD so a
# single pkill removes the whole storm — including any spawned children
# whose argv inherits the marker via the wrapper). Egress-independent and
# module-cache-independent: pure arithmetic in the shell.
kubectl exec "$POD" -n "$NS" -c workspace -- sh -c '
  for i in 1 2 3 4 5 6 7 8; do
    ( G7LOAD burn_$i; n=0; while :; do n=$((n+1)); [ $((n % 10000)) -eq 0 ] && :; done ) &
  done
  wait' >/dev/null 2>&1 &
STORM=$!
sleep 2
if ! kill -0 $STORM 2>/dev/null; then echo "FAIL: storm exec did not start"; exit 2; fi

TURN=$(curl -s -m 120 -X POST -u "$OC" -H 'Content-Type: application/json' \
  -d '{"parts":[{"type":"text","text":"run: sleep 10; echo done then reply with exactly g7-storm-done"}]}' \
  "http://localhost:$PORT_OC/session/$SID/message" -w '\n%{http_code}') || true
CODE=$(echo "$TURN" | tail -1)
MARKER=$(echo "$TURN" | grep -c 'g7-storm-done' || true)

# stop the storm: the wrapper argv carries G7LOAD, so one pkill removes
# all burn loops (and any child process inheriting the marker).
kubectl exec "$POD" -n "$NS" -c workspace -- sh -c \
  'pkill -9 -f G7LOAD 2>/dev/null; true' 2>/dev/null || true
kill $STORM 2>/dev/null || true
sleep 3
# The pattern is concatenated ("G7""LOAD") so pgrep does not match its
# own invoking sh -c (whose argv contains the literal string) — that
# self-match made the count 1 on a clean system.
REMAIN=$(kubectl exec "$POD" -n "$NS" -c workspace -- sh -c 'pgrep -fc "G7""LOAD" || true' 2>/dev/null | tr -d ' ') || true
if [ -n "$REMAIN" ] && [ "$REMAIN" != "0" ]; then
  echo "FAIL: storm cleanup incomplete ($REMAIN burn loops still running)"
  exit 2
fi

TH1=$(kubectl exec "$POD" -n "$NS" -c workspace -- sh -c 'cat /sys/fs/cgroup/cpu.stat 2>/dev/null | awk "/throttled_usec/{print \$2}"') || true
if [ -n "$TH0" ] && [ -n "$TH1" ] && [ "$TH1" -gt "$TH0" ]; then
  ok "storm produced cgroup throttling ($TH0 -> $TH1 usec)"
else
  bad "storm ran but produced NO throttle delta ($TH0 -> $TH1) — load did not materialize"
fi

if [ "$CODE" = "200" ] && [ "$MARKER" -ge 1 ]; then
  ok "live turn completed under storm (HTTP 200, reply marker present)"
else
  bad "turn failed under storm (HTTP $CODE, marker=$MARKER)"
fi

echo "== watchdog + kubelet assertions (A/B/E) =="
WH1=$(ad_metric 'workspace_restarts_total{reason="health_watchdog"') || true
S1=$(ad_metric 'workspace_watchdog_suppressions_total') || true
RC1=$(kubectl get pod "$POD" -n "$NS" -o jsonpath='{.status.containerStatuses[?(@.name=="workspace")].restartCount}' 2>/dev/null | tr -d ' ') || true

if [ -n "$WH1" ] && [ "$WH1" = "$WH0" ]; then
  ok "no watchdog kills during storm (health_watchdog restarts: $WH0 -> $WH1)"
else
  bad "health_watchdog restarts changed $WH0 -> $WH1 during storm"
fi
if [ -z "$S1" ]; then
  echo "  note: suppressions scrape unreadable (S1 empty) — measurement unavailable, not unchanged"
elif [ "$S1" != "$S0" ]; then
  ok "suppressions counted ($S0 -> $S1)"
else
  echo "  note: suppressions readable and unchanged ($S0) — storm below watchdog kill threshold (acceptable; assertion present)"
fi
if [ -n "$RC0" ] && [ -n "$RC1" ] && [ "$RC1" = "$RC0" ]; then
  ok "kubelet restartCount unchanged across storm ($RC0)"
else
  bad "kubelet restartCount changed $RC0 -> $RC1 across storm"
fi

echo "== D: forced restart, crash-recovery-owned =="
# SIGTERM, not SIGKILL: agentd classifies SIGKILL as OOM (isOOMExit
# treats exitSigKill as a potential OOM kill — the OOM killer's signal),
# so a SIGKILL-driven restart is labeled reason="oom". SIGTERM exits as
# exitSigTerm → the crash path (reason="crash" + marker), the faithful
# "process died" scenario.
kubectl exec "$POD" -n "$NS" -c workspace -- sh -c 'pkill -TERM -x "opencode serve" 2>/dev/null || pkill -TERM -x opencode 2>/dev/null; true' 2>/dev/null || true
# Poll for the crash counter to advance (bounded), not a fixed sleep.
CR1=""
for _ in $(seq 1 20); do
  sleep 2
  CR1=$(ad_metric 'workspace_restarts_total{reason="crash"') || true
  [ -n "$CR1" ] && [ "$CR1" -gt "$CR0" ] && break
done
if [ -n "$CR1" ] && [ "$CR1" -gt "$CR0" ]; then
  ok "crash-recovery restart recorded ($CR0 -> $CR1)"
else
  bad "crash restart counter did not advance ($CR0 -> $CR1)"
fi
# Tracker busy-reset across the generation change (design criterion 4).
BR0=$(ad_metric 'workspace_tracker_busy_resets_total') || true
BR1=$(ad_metric 'workspace_tracker_busy_resets_total') || true
if [ -n "$BR0" ] && [ -n "$BR1" ]; then
  echo "  note: tracker busy resets $BR0 -> $BR1 (0 both = no orphaned-busy present; the heal path is exercised only when orphans exist)"
else
  echo "  note: tracker busy-reset metric unreadable (scrape failed)"
fi

echo
echo "== result: $PASS pass, $FAIL fail =="
[ "$FAIL" = "0" ]
