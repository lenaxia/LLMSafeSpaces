"""EA3 contract test: POST /sessions/{id}/message is synchronous and returns JSON.

Validates the foundational assumption underlying every SDK's send_message:
the call blocks until the assistant turn completes and returns a single JSON
object, not an SSE stream.

Run: API_URL=http://localhost:18080 API_KEY=lsp_... python tests/test_ea3_message_contract.py
"""

import sys
import os
import time

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import httpx

from llmsafespaces import LLMSafeSpaces

API_URL = os.environ.get("API_URL", "http://localhost:18080")
API_KEY = os.environ.get("API_KEY", "lsp_upgradetest1234567890abcdef")

client = LLMSafeSpaces(API_URL, api_key=API_KEY, timeout=120.0)

passed = 0
failed = 0
errors = []


def ok(cond, label):
    global passed, failed
    if cond:
        print(f"  ✓ {label}")
        passed += 1
    else:
        print(f"  ✗ {label}")
        failed += 1
        errors.append(label)


def wait_healthy(ws_id, max_wait=150):
    start = time.time()
    while time.time() - start < max_wait:
        s = client.workspaces.get_status(ws_id)
        ah = s.get("agentHealth", {})
        if ah.get("status") == "Healthy":
            return "Healthy"
        if s.get("phase") == "Failed":
            return "Failed"
        time.sleep(5)
    return (
        client.workspaces.get_status(ws_id)
        .get("agentHealth", {})
        .get("status", "timeout")
    )


print("=== EA3 Contract Test: /message blocking + JSON shape ===\n")

ws = client.workspaces.create(
    name="ea3-contract-test", runtime="base", storage_size="1Gi"
)
ws_id = ws.id
print(f"  Created workspace: {ws_id}")

try:
    print("  Waiting for agent healthy...")
    health = wait_healthy(ws_id)
    if health != "Healthy":
        print(f"  ✗ Agent did not become healthy (got: {health}), aborting")
        sys.exit(1)

    session = client.sessions.ensure(ws_id)
    print(f"  Session: {session.sessionId}")

    # ── Part 1: SDK-level — content must be non-empty (proves blocking) ──
    print("\n─── Part 1: SDK send_message returns non-empty content ───")
    result = client.sessions.send_message(
        ws_id, session.sessionId, "Reply with exactly: EA3-OK"
    )
    ok(
        isinstance(result.content, str) and len(result.content) > 0,
        "content is non-empty (proves the call blocked for the full response)",
    )
    ok(
        isinstance(result.raw, dict),
        "raw is a dict (JSON object, not a stream fragment)",
    )
    if isinstance(result.raw, dict):
        ok(
            "parts" in result.raw,
            "raw has 'parts' array (opencode message shape)",
        )

    # ── Part 2: Raw HTTP — Content-Type must NOT be text/event-stream ──
    print("\n─── Part 2: Raw HTTP Content-Type is not SSE ───")
    resp = httpx.post(
        f"{API_URL}/api/v1/workspaces/{ws_id}/sessions/{session.sessionId}/message",
        headers={"Authorization": f"Bearer {API_KEY}"},
        json={
            "content": "Reply with exactly: EA3-OK-RAW",
            "parts": [{"type": "text", "text": "Reply with exactly: EA3-OK-RAW"}],
        },
        timeout=120,
    )

    ok(resp.status_code == 200, f"status is 200 (got {resp.status_code})")

    content_type = resp.headers.get("content-type", "")
    if content_type.startswith("text/event-stream"):
        print(
            "  ✗ EA3 INVALIDATED: POST /message returns SSE, not JSON. "
            "All SDK send_message implementations must be redesigned from "
            "resp.json()/JSON.parse to SSE-stream-then-accumulate. "
            f"Content-Type: {content_type}"
        )
        failed += 1
        errors.append("EA3 invalidated: /message returns SSE")
    else:
        ok(
            True,
            f"Content-Type is not text/event-stream (got: {content_type})",
        )

    try:
        body = resp.json()
        ok(isinstance(body, dict), "raw body parses as JSON object")
        ok("parts" in body, "JSON body has 'parts' array")
    except Exception as e:
        ok(False, f"raw body parses as JSON (error: {e})")

finally:
    print("\n─── Cleanup ───")
    client.workspaces.delete(ws_id)
    ok(True, f"workspace {ws_id} deleted")

print(f"\n═══ EA3 Contract: {passed} passed, {failed} failed ═══")
if errors:
    print(f"Failures:\n  " + "\n  ".join(errors))
sys.exit(1 if failed > 0 else 0)
