#!/usr/bin/env bash
# test-entrypoint.sh — Tests entrypoint-common.sh sourcing behavior.
# Tests the critical fix: sourcing the script does NOT exit the parent shell.
set -euo pipefail

PASS=0
FAIL=0
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$SCRIPT_DIR/.."
ENTRYPOINT="$REPO_ROOT/runtimes/base/tools/entrypoints/entrypoint-common.sh"

pass() { PASS=$((PASS + 1)); echo "  ✓ $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  ✗ $1"; }

# ============================================================
# Test 1: 'return 0' in sourced script does not kill parent
# (This is the bug we fixed — exit 0 was killing the shell)
# ============================================================
test_source_does_not_exit_parent() {
    # Simulate: no secrets.json, no credentials — the early return path
    RESULT=$(bash -c '
        mkdir -p /tmp/test-ep-$$/sandbox-cfg
        # Patch the script to use our temp paths
        sed "s|/sandbox-cfg|/tmp/test-ep-'$$'/sandbox-cfg|g" "'"$ENTRYPOINT"'" > /tmp/test-ep-$$/entrypoint.sh
        chmod +x /tmp/test-ep-$$/entrypoint.sh
        export HOME=/tmp/test-ep-$$/home
        mkdir -p $HOME
        source /tmp/test-ep-$$/entrypoint.sh
        echo "SURVIVED"
        rm -rf /tmp/test-ep-$$
    ' 2>&1 || true)
    if echo "$RESULT" | grep -q "SURVIVED"; then
        pass "source without secrets.json: parent shell survives"
    else
        fail "source without secrets.json: parent shell died (output: $RESULT)"
    fi
}

# ============================================================
# Test 2: With credentials file, parent still survives
# ============================================================
test_source_with_credentials_survives() {
    RESULT=$(bash -c '
        TDIR=/tmp/test-ep2-$$
        mkdir -p $TDIR/sandbox-cfg $TDIR/home
        echo "{\"providers\":[]}" > $TDIR/sandbox-cfg/credentials
        sed "s|/sandbox-cfg|$TDIR/sandbox-cfg|g" "'"$ENTRYPOINT"'" > $TDIR/entrypoint.sh
        chmod +x $TDIR/entrypoint.sh
        export HOME=$TDIR/home
        source $TDIR/entrypoint.sh
        echo "SURVIVED"
        rm -rf $TDIR
    ' 2>&1 || true)
    if echo "$RESULT" | grep -q "SURVIVED"; then
        pass "source with credentials: parent shell survives"
    else
        fail "source with credentials: parent shell died"
    fi
    # Verify config was copied
    if [[ -f /tmp/agent-config.json ]]; then
        pass "source with credentials: agent-config.json created"
    else
        fail "source with credentials: agent-config.json missing"
    fi
}

# ============================================================
# Test 3: With secrets.json, all types materialized correctly
# ============================================================
test_full_secrets_materialization() {
    RESULT=$(bash -c '
        TDIR=/tmp/test-ep3-$$
        mkdir -p $TDIR/sandbox-cfg $TDIR/home
        export HOME=$TDIR/home
        cat > $TDIR/sandbox-cfg/secrets.json <<EOFJ
[
  {"type":"llm-provider","name":"main","metadata":{},"plaintext":"{\"model\":\"claude\"}"},
  {"type":"env-secret","name":"key1","metadata":{"var_name":"SECRET_KEY"},"plaintext":"val123"},
  {"type":"ssh-key","name":"deploy","metadata":{"key_type":"ed25519","host":"github.com"},"plaintext":"ssh-key-data"},
  {"type":"git-credential","name":"gh","metadata":{"host":"github.com","protocol":"https"},"plaintext":"ghp_abc"}
]
EOFJ
        sed -e "s|/sandbox-cfg|$TDIR/sandbox-cfg|g" \
            -e "s|ENV_FILE=.*|ENV_FILE=\"$TDIR/env\"|" \
            "'"$ENTRYPOINT"'" > $TDIR/entrypoint.sh
        chmod +x $TDIR/entrypoint.sh
        source $TDIR/entrypoint.sh
        echo "SURVIVED"
        # Check outputs
        grep -q "claude" /tmp/agent-config.json && echo "LLM_OK"
        grep -q "SECRET_KEY" $TDIR/env && echo "ENV_OK"
        [[ -f $HOME/.ssh/id_ed25519_deploy ]] && echo "SSH_OK"
        grep -q "ghp_abc" $HOME/.git-credentials && echo "GIT_OK"
        rm -rf $TDIR
    ' 2>&1 || true)

    echo "$RESULT" | grep -q "SURVIVED" && pass "full secrets: parent survives" || fail "full secrets: parent died"
    echo "$RESULT" | grep -q "LLM_OK" && pass "full secrets: llm-provider materialized" || fail "full secrets: llm-provider missing"
    echo "$RESULT" | grep -q "ENV_OK" && pass "full secrets: env-secret materialized" || fail "full secrets: env-secret missing"
    echo "$RESULT" | grep -q "SSH_OK" && pass "full secrets: ssh-key materialized" || fail "full secrets: ssh-key missing"
    echo "$RESULT" | grep -q "GIT_OK" && pass "full secrets: git-credential materialized" || fail "full secrets: git-credential missing"
}

# ============================================================
# Test 4: agentd reload-secrets endpoint
# ============================================================
test_agentd_reload() {
    # Build agentd
    (cd "$REPO_ROOT" && go build -o /tmp/test-agentd ./cmd/workspace-agentd/) 2>/dev/null || {
        fail "agentd reload: build failed"
        return
    }

    mkdir -p /tmp/test-agentd-cfg
    echo "pw" > /tmp/test-agentd-cfg/password

    # #887 D5.1: agentd needs an admin-mux bearer in env or file; env for
    # the test harness (the boot gate is D5.2's business — this script
    # only asserts the HTTP server comes up).
    export AGENTD_ADMIN_TOKEN="test-token"

    # Start agentd (reads password from /sandbox-cfg/password — symlink it)
    mkdir -p /sandbox-cfg 2>/dev/null || true
    ln -sf /tmp/test-agentd-cfg/password /tmp/sandbox-cfg-password 2>/dev/null || true

    # Can't write to /sandbox-cfg in this env, so just test the binary starts and serves HTTP
    /tmp/test-agentd &
    PID=$!
    sleep 0.3

    # Test healthz (will return 502 since no opencode, but proves server is up)
    CODE=$(curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:4097/v1/healthz 2>/dev/null || echo "000")
    kill $PID 2>/dev/null; wait $PID 2>/dev/null || true

    if [[ "$CODE" == "502" || "$CODE" == "200" ]]; then
        pass "agentd: HTTP server starts and responds ($CODE)"
    else
        fail "agentd: HTTP server not responding (code=$CODE)"
    fi

    rm -f /tmp/test-agentd
}

# ============================================================
echo "=== Entrypoint & Agentd E2E Tests ==="
test_source_does_not_exit_parent
test_source_with_credentials_survives
test_full_secrets_materialization
test_agentd_reload
echo ""
echo "Results: $PASS passed, $FAIL failed"
[[ $FAIL -eq 0 ]] || exit 1
