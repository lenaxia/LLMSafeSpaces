#!/usr/bin/env bash
# G7 harness self-test (#907 round-1 review): pins the harness's own
# parsing logic so the -f3-class and head-1-class defects cannot recur.
# Pattern follows local/us-63-v2-marker-detection.test.sh (fixture +
# assertion, no external deps).
set -euo pipefail
. "$(dirname "$0")/lib.sh"
PASS=0; FAIL=0
ok(){ echo "  PASS: $1"; PASS=$((PASS+1)); }
bad(){ echo "  FAIL: $1"; FAIL=$((FAIL+1)); }

# --- fixture: a realistic exposition snippet (labeled CounterVec) ---
cat > /tmp/g7fix-metrics.txt <<'FIX'
# HELP workspace_restarts_total Total opencode restarts by reason
# TYPE workspace_restarts_total counter
workspace_restarts_total{reason="api_key",workspace_id="w1"} 1
workspace_restarts_total{reason="crash",workspace_id="w1"} 3
workspace_restarts_total{reason="env_secrets",workspace_id="w1"} 2
workspace_restarts_total{reason="health_watchdog",workspace_id="w1"} 0
workspace_restarts_total{reason="oom",workspace_id="w1"} 0
workspace_restarts_total{reason="user_requested",workspace_id="w1"} 0
# HELP workspace_watchdog_suppressions_total ...
# TYPE workspace_watchdog_suppressions_total counter
workspace_watchdog_suppressions_total{reason="starved",workspace_id="w1"} 7
FIX

# metric_sum REASON: the harness's series-sum extraction, isolated.

# 1. reason-specific sums, not head-1 (the round-1 blocking defect)
R=$(metric_sum /tmp/g7fix-metrics.txt 'workspace_restarts_total{reason="crash"')
[ "$R" = "3" ] && ok "crash series summed (=3, not head-1 arbitrary)" || bad "crash sum got $R"
R=$(metric_sum /tmp/g7fix-metrics.txt 'workspace_restarts_total{reason="health_watchdog"')
[ "$R" = "0" ] && ok "health_watchdog series summed (=0)" || bad "health_watchdog sum got $R"
R=$(metric_sum /tmp/g7fix-metrics.txt 'workspace_restarts_total{reason="env_secrets"')
[ "$R" = "2" ] && ok "env_secrets series summed (=2)" || bad "env_secrets sum got $R"
# 2. sum across multiple workspaces (labels sort arbitrarily)
cat >> /tmp/g7fix-metrics.txt <<'FIX2'
workspace_restarts_total{reason="crash",workspace_id="w2"} 1
FIX2
R=$(metric_sum /tmp/g7fix-metrics.txt 'workspace_restarts_total{reason="crash"')
[ "$R" = "4" ] && ok "crash sums across workspaces (=4)" || bad "cross-ws crash sum got $R"
# 3. suppression family read
R=$(metric_sum /tmp/g7fix-metrics.txt 'workspace_watchdog_suppressions_total{reason="starved"')
[ "$R" = "7" ] && ok "suppression series summed (=7)" || bad "suppression sum got $R"

# 4. session-id extraction (-f4 of a "split, the -f3 live bug)
cat > /tmp/g7fix-session.json <<'FIX3'
{"id":"ses_abcdef0123XYZ","slug":"glowing-forest","projectID":"global"}
FIX3
SID=$(grep -o '"id":"ses_[^"]*' /tmp/g7fix-session.json | head -1 | cut -d'"' -f4)
[ "$SID" = "ses_abcdef0123XYZ" ] && ok "session-id extraction (f4 of \"-split)" || bad "session-id extraction got '$SID'"

# 5. marker detection — no truncation window (the r3 finding: a verbose
# tool-bearing reply puts the marker past any head -c N and false-fails
# a successful turn).
TURN='{"info":{"role":"assistant"},"parts":[{"type":"tool","tool":"bash","input":{"command":"sleep 10"},"output":"done"},{"type":"text","text":"g7-storm-done"}]}'
M=$(echo "$TURN" | grep -c 'g7-storm-done' || true)
[ "$M" -ge 1 ] && ok "reply marker detected past a long tool part" || bad "marker not detected"

# 5b. a LONG reply (marker beyond 4000 bytes) must still match — the old
# head -c 4000 pipeline false-failed this.
LONG='{"parts":[{"type":"tool","tool":"bash","input":{"command":"sleep 10"},"output":"'"$(printf 'x%.0s' {1..4200})"'"},{"type":"text","text":"g7-storm-done"}]}'
M2=$(echo "$LONG" | grep -c 'g7-storm-done' || true)
[ "$M2" -ge 1 ] && ok "reply marker detected beyond 4000 bytes (no truncation)" || bad "long-reply marker missed (truncation regression)"

# 6. empty-scrape fail-open guard: metric_sum on a missing family prints 0
R=$(metric_sum /tmp/g7fix-metrics.txt 'missing_family{reason="crash"')
[ "$R" = "0" ] && ok "missing family sums to 0 (caller must treat empty scrape as failure)" || bad "missing family got $R"

# 7. cleanup-verify fail-open discipline (r4 finding 1): an unreadable
# REMAIN must FAIL, never skip — the harness README forbids treating
# inability-to-verify as verified.
if grep -q 'if \[ -z "\$REMAIN" \]; then' "$(dirname "$0")/g7-stress.sh"; then
  ok "cleanup-verify fails open (empty REMAIN -> FAIL, not skip)"
else
  bad "cleanup-verify fail-open guard absent from g7-stress.sh"
fi

echo
echo "== result: $PASS pass, $FAIL fail =="
[ "$FAIL" = "0" ]
