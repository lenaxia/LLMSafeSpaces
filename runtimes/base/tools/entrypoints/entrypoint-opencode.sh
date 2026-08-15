#!/usr/bin/env bash
set -euo pipefail
source /usr/local/bin/entrypoint-common.sh

eval "$(mise activate bash)"

# US-35.7: secrets-env is now on tmpfs (/sandbox-runtime), not /tmp.
# agentd sources these and forwards as env vars to the opencode child.
if [[ -f /sandbox-runtime/secrets-env ]]; then
    source /sandbox-runtime/secrets-env
fi

export OPENCODE_CONFIG=/sandbox-runtime/agent-config.json
export XDG_DATA_HOME=/workspace/.local

if [[ -f /sandbox-cfg/password ]]; then
    export OPENCODE_SERVER_PASSWORD="$(cat /sandbox-cfg/password)"
fi

# agentd is PID 1 (supervisor). It manages opencode as a child process.
# AGENTD_BIN is exported by entrypoint-common.sh: the sha256-verified
# overlay binary (#863) or the baked-in one (legacy mode).
exec "${AGENTD_BIN}" --supervise
