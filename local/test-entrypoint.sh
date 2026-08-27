#!/usr/bin/env bash
# test-entrypoint.sh — Local shell-level tests for the boot credential path.
#
# Test 1 locks the entrypoint-common sourcing contract (worklog 0078):
# sourcing the script must NOT exit the parent shell. Tests 2–3 exercise
# the REAL `workspace-agentd materialize` subcommand (the entrypoint's
# boot step since Epic 17 G2) through the same LLMSAFESPACES_* env-var
# path overrides the Go suite uses, against a temp tree that mirrors the
# production volume layout (PVC home symlinks → tmpfs targets).
#
# The git-credential assertions are the shell variant of the #1087
# regression gate (Go twin: cmd/workspace-agentd/git_creds_boot_test.go):
# a bound git-credential secret must materialize on a cold boot such that
# $HOME/.git-credentials — the path git's credential store helper reads —
# resolves with the credential line in it.
set -euo pipefail

PASS=0
FAIL=0
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$SCRIPT_DIR/.."
ENTRYPOINT="$REPO_ROOT/runtimes/base/tools/entrypoints/entrypoint-common.sh"

pass() { PASS=$((PASS + 1)); echo "  ✓ $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  ✗ $1"; }

# go may only be reachable via mise (the documented dev setup).
if ! command -v go >/dev/null 2>&1 && command -v mise >/dev/null 2>&1; then
    eval "$(mise activate bash)"
fi

# Build the agentd binary once, always fresh (source may have changed
# since a previous run left a stale binary in /tmp).
build_agentd() {
    rm -rf /tmp/test-agentd-bin
    mkdir -p /tmp/test-agentd-bin
    (cd "$REPO_ROOT" && go build -o /tmp/test-agentd-bin/workspace-agentd ./cmd/workspace-agentd/)
}

# ============================================================
# Test 1: 'return 0' in sourced entrypoint does not kill parent
# (This is the bug we fixed — exit 0 was killing the shell)
# ============================================================
test_source_does_not_exit_parent() {
    # Simulate: no /sandbox-cfg, no credentials — the early return path
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
# Test 2: materialize subcommand — full secret batch on a cold boot,
# including the git-credential → $HOME/.git-credentials round trip
# (#1087). Drives the real binary with env-var path overrides against
# a PVC/tmpfs-shaped temp tree.
# ============================================================
test_full_secrets_materialization() {
    if ! build_agentd 2>/dev/null; then
        fail "full secrets: build failed"
        return
    fi
    AGENTD_BIN=/tmp/test-agentd-bin/workspace-agentd

    RESULT=$(bash -c '
        set -e
        TDIR=$(mktemp -d /tmp/test-ep3-XXXXXX)
        AGENTD_BIN="'"$AGENTD_BIN"'"
        PVC=$TDIR/pvc
        RT=$TDIR/sandbox-runtime
        CFG=$TDIR/sandbox-cfg
        mkdir -p $PVC/home $RT/rt/ssh $RT/rt/secrets $CFG
        # init-fs-style symlink farm: PVC home paths → tmpfs targets
        ln -s $RT/rt/git-credentials $PVC/home/.git-credentials
        ln -s $RT/rt/ssh $PVC/home/.ssh
        cat > $CFG/secrets.json <<EOFJ
[
  {"type":"llm-provider","name":"main","metadata":{},"plaintext":"{\"kind\":\"anthropic\",\"slug\":\"anthropic\",\"apiKey\":\"sk-ant-test\"}"},
  {"type":"env-secret","name":"key1","metadata":{"var_name":"SECRET_KEY"},"plaintext":"val123"},
  {"type":"ssh-key","name":"deploy","metadata":{"key_type":"ed25519","host":"github.com"},"plaintext":"ssh-key-data"},
  {"type":"git-credential","name":"gh","metadata":{"host":"github.com","protocol":"https"},"plaintext":"ghp_abc"}
]
EOFJ
        # HOME is exported for the WHOLE block so the post-boot checks
        # below resolve through the same $HOME the subcommand saw.
        export HOME=$PVC/home
        export LLMSAFESPACES_SECRETS_BASE_DIR=$RT/rt/secrets \
               LLMSAFESPACES_SSH_DIR=$RT/rt/ssh \
               LLMSAFESPACES_AGENT_CONFIG_PATH=$RT/agent-config.json \
               LLMSAFESPACES_SECRETS_ENV_PATH=$RT/secrets-env \
               LLMSAFESPACES_GIT_CREDS_PATH=$RT/rt/git-credentials \
               LLMSAFESPACES_RELOAD_CACHE_PATH=$RT/reload-cache.json
        # Hermetic: never inherit a live pod relay URL into the test.
        unset INFERENCE_RELAY_BASEURL
        "$AGENTD_BIN" materialize --from $CFG/secrets.json
        # Check outputs — through the $HOME symlinks, exactly as tools see them
        grep -q "anthropic" $RT/agent-config.json && echo "LLM_OK"
        grep -q "SECRET_KEY" $RT/secrets-env && echo "ENV_OK"
        [[ -f $HOME/.ssh/id_ed25519_deploy ]] && echo "SSH_OK"
        grep -q "https://oauth2:ghp_abc@github.com" $HOME/.git-credentials && echo "GIT_OK"
        rm -rf $TDIR
    ' 2>&1 || true)

    echo "$RESULT" | grep -q "materialize: " && pass "full secrets: subcommand ran" || fail "full secrets: subcommand did not run (output: $RESULT)"
    echo "$RESULT" | grep -q "LLM_OK" && pass "full secrets: llm-provider materialized" || fail "full secrets: llm-provider missing"
    echo "$RESULT" | grep -q "ENV_OK" && pass "full secrets: env-secret materialized" || fail "full secrets: env-secret missing"
    echo "$RESULT" | grep -q "SSH_OK" && pass "full secrets: ssh-key materialized" || fail "full secrets: ssh-key missing"
    echo "$RESULT" | grep -q "GIT_OK" && pass "full secrets: git-credential resolves via \$HOME/.git-credentials (#1087)" || fail "full secrets: git-credential missing"
}

# ============================================================
# Test 3: agentd HTTP server comes up and serves /v1/healthz
# ============================================================
test_agentd_http() {
    if [[ ! -x /tmp/test-agentd-bin/workspace-agentd ]]; then
        if ! build_agentd 2>/dev/null; then
            fail "agentd http: build failed"
            return
        fi
    fi

    # #887 D5.1: agentd needs an admin-mux bearer in env or file; env for
    # the test harness (the boot gate is D5.2's business — this script
    # only asserts the HTTP server comes up). Bare invocation (no
    # --supervise) is the server-only mode; the admin-token assignment
    # nil-guards it (see main.go — this test is its regression gate).
    export AGENTD_ADMIN_TOKEN="test-token"
    unset INFERENCE_RELAY_BASEURL

    /tmp/test-agentd-bin/workspace-agentd &
    PID=$!
    sleep 0.3

    # Test healthz on the ADMIN port (US-22.8: healthz/readyz/statusz
    # live on 4098; 4097 is the user mux). 502 is expected when no
    # opencode is supervised — it proves the server is up and routed.
    CODE=$(curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:4098/v1/healthz 2>/dev/null || echo "000")
    kill $PID 2>/dev/null || true
    wait $PID 2>/dev/null || true

    if [[ "$CODE" == "502" || "$CODE" == "200" ]]; then
        pass "agentd: HTTP server starts and responds ($CODE)"
    else
        fail "agentd: HTTP server not responding (code=$CODE)"
    fi
}

# ============================================================
echo "=== Entrypoint & Agentd E2E Tests ==="
test_source_does_not_exit_parent
test_full_secrets_materialization
test_agentd_http
echo ""
echo "Results: $PASS passed, $FAIL failed"
[[ $FAIL -eq 0 ]] || exit 1
