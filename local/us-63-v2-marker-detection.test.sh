#!/usr/bin/env bash
# Regression test for the history_contains echo-detection logic in
# local/us-63-v2-behavior-e2e.sh.
#
# The original substring match was a false positive: the user's own queued
# message ("reply with exactly: <MARKER>") is stored in history, so a
# substring match always passed — before the assistant replied. The fix keys
# on a text part whose stripped content EQUALS the marker exactly.
#
# This test exercises the Python detection logic directly with fixture JSON.
set -euo pipefail

MARKER="ACK_TEST_123"

run_check() {
    local fixture="$1"
    echo "$fixture" | MARKER="$MARKER" python3 -c '
import json, os, sys
marker = os.environ["MARKER"]
try:
    d = json.load(sys.stdin)
except:
    print("NO"); sys.exit()
msgs = d if isinstance(d, list) else d.get("data", d.get("messages", []))
if not isinstance(msgs, list):
    print("NO"); sys.exit()
for m in msgs:
    parts = m.get("parts", []) if isinstance(m, dict) else []
    for p in parts:
        if isinstance(p, dict) and p.get("text", "").strip() == marker:
            print("YES"); sys.exit()
print("NO")
'
}

PASS=0; FAIL=0
check() {
    local name="$1" expected="$2" actual="$3"
    if [[ "$expected" == "$actual" ]]; then
        echo "  PASS: $name"; PASS=$((PASS + 1))
    else
        echo "  FAIL: $name (expected $expected, got $actual)"; FAIL=$((FAIL + 1))
    fi
}

echo "=== history_contains regression tests ==="

# Fixture 1: user prompt only — must NOT match (no assistant reply yet).
check "user-prompt-only → NO" "NO" "$(run_check '[{"role":"user","parts":[{"type":"text","text":"reply with exactly: '"$MARKER"'"}]}]')"

# Fixture 2: user + assistant echo — must match.
check "user+echo → YES" "YES" "$(run_check '[{"role":"user","parts":[{"type":"text","text":"reply with exactly: '"$MARKER"'"}]},{"role":"assistant","parts":[{"type":"text","text":"'"$MARKER"'"}]}]')"

# Fixture 3: echo only — must match.
check "echo-only → YES" "YES" "$(run_check '[{"role":"assistant","parts":[{"type":"text","text":"'"$MARKER"'"}]}]')"

# Fixture 4: no match — must NOT match.
check "no-match → NO" "NO" "$(run_check '[{"role":"assistant","parts":[{"type":"text","text":"unrelated"}]}]')"

# Fixture 5: marker embedded in longer text — must NOT match (exact equality).
check "embedded-not-exact → NO" "NO" "$(run_check '[{"role":"assistant","parts":[{"type":"text","text":"the answer is '"$MARKER"' plus more"}]}]')"

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[[ "$FAIL" -eq 0 ]] || exit 1
