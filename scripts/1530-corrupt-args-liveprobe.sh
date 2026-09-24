#!/usr/bin/env python3
# 1530-corrupt-args-liveprobe.sh (python rewrite) — issue #1530, live leg.
#
# Drives agentd's MCP endpoint (:4097) with raw JSON-RPC tools/call
# bodies reproducing the EXACT corrupt send_message argument shapes
# from instances 1-13: session_id values containing an ESCAPED JSON
# tail fragment (,\"lsp_injected_session\":\"<id>\"}) — valid JSON on
# the wire (the escapes are inside the string value), which is the
# model-emission signature. Raw HTTP has no plugin, so origin rides
# the #1469 hybrid's declared fallback (from_session_id).
#
# All targets are bogus ids (ses_PROBE_*): resolution fails, nothing
# delivers — safe to fire repeatedly.
#
# Usage: scripts/1530-corrupt-args-liveprobe.sh
# Exit 0 = characterization complete; exit 1 = unexpected behavior.

import json
import sys
import urllib.request
import base64

PW = open("/sandbox-cfg/password").read().strip()
AG = "http://127.0.0.1:4097/v1/mcp"
DECLARED = "ses_PROBE_DECLARED_ORIGIN"

def call(arguments):
    body = json.dumps({
        "jsonrpc": "2.0", "id": 1, "method": "tools/call",
        "params": {"name": "send_message", "arguments": arguments},
    }).encode()
    req = urllib.request.Request(AG, data=body, method="POST")
    req.add_header("Content-Type", "application/json")
    req.add_header("Authorization", "Basic " + base64.b64encode(f"opencode:{PW}".encode()).decode())
    try:
        with urllib.request.urlopen(req, timeout=15) as r:
            env = json.loads(r.read())
    except urllib.error.HTTPError as e:
        return f"HTTP-{e.code}: {e.read()[:120]!r}"
    if "error" in env:
        return "RPC-ERROR: " + env["error"].get("message", "")[:160]
    return env["result"]["content"][0]["text"][:240]

passed = failed = 0
def note(ok, label):
    global passed, failed
    print(("PASS: " if ok else "FAIL: ") + label)
    passed, failed = passed + ok, failed + (not ok)

base = {"message": "probe-1530 (no action needed)", "from_session_id": DECLARED}

# v1 — the exact instance shape: escaped tail fragment inside the value,
# VALID JSON (what the model actually emitted in the 13 misfires)
v1 = call({**base, "session_id": 'ses_PROBE_TARGET","lsp_injected_session":"ses_PROBE_ORIGIN"}'})
print(f"v1 exact-instance (escaped fragment, valid JSON): {v1}")
note("failed to resolve session" in v1 or "not found" in v1, "v1 hard-fails cleanly (message lost)")

# v2 — fragment without trailing brace
v2 = call({**base, "session_id": 'ses_PROBE_TARGET","lsp_injected_session":"ses_PROBE_ORIGIN"'})
print(f"v2 no-brace variant: {v2}")
note("failed to resolve session" in v2 or "not found" in v2, "v2 hard-fails cleanly")

# v3 — control: clean bogus id, same expected failure class
v3 = call({**base, "session_id": "ses_PROBE_CLEAN_BOGUS"})
print(f"v3 clean bogus (control): {v3}")
note("failed to resolve session" in v3 or "not found" in v3, "v3 control hard-fails cleanly")

# v4 — model-supplied lsp_injected_session WITHOUT injection (hybrid:
# declared origin present, stale injected key should not confuse)
v4 = call({**base, "session_id": "ses_PROBE_CLEAN_BOGUS",
           "lsp_injected_session": "ses_PROBE_STALE_INJECTED"})
print(f"v4 declared origin + stale injected key: {v4}")
note("failed to resolve session" in v4 or "not found" in v4, "v4 fails on target, origin resolved via declared")

print("---")
print(f"liveprobe-1530: {passed} pass, {failed} fail "
      "(characterization: emission-side fragments currently HARD-FAIL — "
      "message lost, intent mechanically recoverable from the leading id)")
sys.exit(0 if failed == 0 else 1)
