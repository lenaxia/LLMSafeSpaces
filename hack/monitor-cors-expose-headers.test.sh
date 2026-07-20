#!/usr/bin/env bash
# Hermetic test runner for hack/monitor-cors-expose-headers.sh.
# Does NOT use bats — pure bash so no external test framework deps.
#
# Spins up a stub HTTP server, runs the monitor against it, asserts on
# exit code + output. Each test is a function; failures print FAIL and
# set the global exit code.
#
# Usage: ./hack/monitor-cors-expose-headers.test.sh
set -uo pipefail

MONITOR_SCRIPT="${MONITOR_SCRIPT:-$(dirname "$0")/monitor-cors-expose-headers.sh}"
PASS=0
FAIL=0
SERVER_PIDS=()

# Spawn a stub server returning a fixed Access-Control-Expose-Headers value.
# Args: <port> <expose_value> (empty string = omit header entirely)
spawn_stub() {
  local port="$1"
  local expose_value="$2"
  EXPOSE_HEADER_VALUE="${expose_value}" python3 -c "
import http.server, os
class H(http.server.BaseHTTPRequestHandler):
    def do_HEAD(self):
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        expose = os.environ.get('EXPOSE_HEADER_VALUE', '')
        if expose:
            self.send_header('Access-Control-Expose-Headers', expose)
        self.end_headers()
    def log_message(self, *args):
        pass
http.server.HTTPServer(('127.0.0.1', ${port}), H).serve_forever()
" >/dev/null 2>&1 &
  SERVER_PIDS+=($!)
  # Give the server a moment to bind.
  sleep 0.3
}

cleanup() {
  for pid in "${SERVER_PIDS[@]:-}"; do
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  done
}
trap cleanup EXIT

# Run the monitor and capture exit code + output. The `|| true` ensures
# the script doesn't terminate under `set -e` (which is off here, but
# the explicit capture is still clearer than relying on that).
run_monitor() {
  local url="$1"
  set +e
  OUTPUT=$(bash "${MONITOR_SCRIPT}" "${url}" 2>&1)
  EXIT_CODE=$?
  set -e
}

# Assert helpers.
assert_exit() {
  local expected="$1"
  local label="$2"
  if [ "${EXIT_CODE}" -eq "${expected}" ]; then
    echo "  OK   ${label} (exit=${EXIT_CODE})"
    PASS=$((PASS + 1))
  else
    echo "  FAIL ${label} — expected exit=${expected}, got exit=${EXIT_CODE}"
    echo "       output: ${OUTPUT}"
    FAIL=$((FAIL + 1))
  fi
}

assert_contains() {
  local needle="$1"
  local label="$2"
  if printf '%s' "${OUTPUT}" | grep -qF "${needle}"; then
    echo "  OK   ${label}"
    PASS=$((PASS + 1))
  else
    echo "  FAIL ${label} — output missing '${needle}'"
    echo "       output: ${OUTPUT}"
    FAIL=$((FAIL + 1))
  fi
}

# --- Tests ---

echo "Test 1: monitor passes when all expected headers are present"
spawn_stub 18080 "X-Request-Id, X-RateLimit-Limit, X-RateLimit-Remaining, X-RateLimit-Reset, X-Next-Cursor"
run_monitor "http://127.0.0.1:18080"
assert_exit 0 "exit code 0 on full header set"
assert_contains "OK: all expected headers present." "success message"

echo "Test 2: monitor fails when X-Next-Cursor is missing (the talos-ops-prod #2053 scenario)"
spawn_stub 18081 "X-Request-Id"
run_monitor "http://127.0.0.1:18081"
assert_exit 1 "exit code 1 on missing X-Next-Cursor"
assert_contains "FAIL X-Next-Cursor" "missing header reported"
assert_contains "FAIL: one or more expected headers are missing" "summary"

echo "Test 3: monitor fails when header is entirely absent"
spawn_stub 18082 ""
run_monitor "http://127.0.0.1:18082"
assert_exit 1 "exit code 1 on absent header"
assert_contains "FAIL: Access-Control-Expose-Headers header is absent" "absent reported"

echo "Test 4: monitor fails on unreachable host"
run_monitor "http://127.0.0.1:1"
assert_exit 2 "exit code 2 on unreachable host"
assert_contains "FAIL: could not reach" "unreachable reported"

echo "Test 5: monitor is case-insensitive on header names"
spawn_stub 18083 "x-request-id, X-RATELIMIT-LIMIT, x-ratelimit-remaining, X-RateLimit-Reset, x-next-cursor"
run_monitor "http://127.0.0.1:18083"
assert_exit 0 "exit code 0 on mixed-case headers"
assert_contains "OK: all expected headers present." "success on mixed-case"

echo ""
echo "=== summary ==="
echo "PASS: ${PASS}"
echo "FAIL: ${FAIL}"
[ "${FAIL}" -eq 0 ]
