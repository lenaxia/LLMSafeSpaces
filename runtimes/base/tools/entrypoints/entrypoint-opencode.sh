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

# Enable opencode's experimental event system so the /event SSE stream
# carries the full lifecycle (step events, session.status). Relocated
# here from the controller's pod builder (#942): an opencode env-var
# name is runtime knowledge, not platform knowledge. Proven load-bearing
# by live cluster experiment (worklog 0263).
export OPENCODE_EXPERIMENTAL_EVENT_SYSTEM=true

if [[ -f /sandbox-cfg/password ]]; then
    export OPENCODE_SERVER_PASSWORD="$(cat /sandbox-cfg/password)"
fi

# Design 0051 US-2: in sidecar mode this container's PID 1 is the
# same-uid supervisor (spawn/reap/signal/status only); agentd's policy
# half runs as the native sidecar container talking to it over
# 127.0.0.1:4099 (Appendix A). Plain [ = ] test — POSIX, no bashisms
# (the dash lesson, #933 round 2; this script is bash but stays
# portable by policy).
if [ "${AGENTD_SIDECAR_MODE:-}" = "1" ]; then
    exec "${AGENTD_BIN}" supervise-opencode
fi

# agentd is PID 1 (supervisor). It manages opencode as a child process.
# AGENTD_BIN is exported by entrypoint-common.sh: the sha256-verified
# overlay binary (#863) or the baked-in one (legacy mode).
exec "${AGENTD_BIN}" --supervise
