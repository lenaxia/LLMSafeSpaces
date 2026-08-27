"""Tests for AsyncLLMSafeSpaces — the async Python SDK client (US-14.4)."""

from __future__ import annotations

import httpx
import pytest
import respx

from llmsafespaces import AsyncLLMSafeSpaces
from llmsafespaces.errors import AuthError, NotFoundError, RateLimitError


BASE = "https://llmsafespaces.test"


@pytest.fixture
async def client():
    c = AsyncLLMSafeSpaces(BASE, api_key="lsp_test")
    yield c
    await c.close()


@respx.mock
@pytest.mark.asyncio
async def test_async_list_workspaces(client: AsyncLLMSafeSpaces):
    respx.get(f"{BASE}/api/v1/workspaces").mock(
        return_value=httpx.Response(200, json={
            "items": [{
                "id": "ws-1", "name": "x", "userId": "u1", "runtime": "python",
                "storageSize": "10Gi", "createdAt": "2026-01-01T00:00:00Z",
                "updatedAt": "2026-01-01T00:00:00Z", "phase": "Active",
            }],
            "pagination": {},
        })
    )
    result = await client.workspaces.list()
    assert len(result.items) == 1
    assert result.items[0].id == "ws-1"


@respx.mock
@pytest.mark.asyncio
async def test_async_get_workspace(client: AsyncLLMSafeSpaces):
    respx.get(f"{BASE}/api/v1/workspaces/ws-1").mock(
        return_value=httpx.Response(200, json={
            "id": "ws-1", "name": "x", "userId": "u1", "runtime": "python",
            "storageSize": "10Gi", "phase": "Active",
            "createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z",
        })
    )
    ws = await client.workspaces.get("ws-1")
    assert ws.id == "ws-1"


@respx.mock
@pytest.mark.asyncio
async def test_async_send_message_extracts_text(client: AsyncLLMSafeSpaces):
    respx.post(f"{BASE}/api/v1/workspaces/ws-1/sessions/ses-1/message").mock(
        return_value=httpx.Response(200, json={"id": "msg-1", "type": "assistant", "parts": [{"type": "text", "text": "hello"}]})
    )
    resp = await client.sessions.send_message("ws-1", "ses-1", "hi")
    assert resp["type"] == "assistant"
    assert resp["parts"][0]["text"] == "hello"


@respx.mock
@pytest.mark.asyncio
async def test_async_ensure_session(client: AsyncLLMSafeSpaces):
    respx.post(f"{BASE}/api/v1/workspaces/ws-1/sessions/new").mock(
        return_value=httpx.Response(200, json={
            "workspaceId": "ws-1", "workspacePhase": "Active",
            "sessionId": "ses-new", "resumed": False,
        })
    )
    r = await client.sessions.ensure("ws-1")
    assert r.sessionId == "ses-new"


@respx.mock
@pytest.mark.asyncio
async def test_async_not_found(client: AsyncLLMSafeSpaces):
    respx.get(f"{BASE}/api/v1/workspaces/missing").mock(return_value=httpx.Response(404, json={"error": "nope"}))
    with pytest.raises(NotFoundError):
        await client.workspaces.get("missing")


@respx.mock
@pytest.mark.asyncio
async def test_async_auth_error(client: AsyncLLMSafeSpaces):
    respx.get(f"{BASE}/api/v1/workspaces").mock(return_value=httpx.Response(403, json={"error": "forbidden"}))
    with pytest.raises(AuthError):
        await client.workspaces.list()


@respx.mock
@pytest.mark.asyncio
async def test_async_rate_limit(client: AsyncLLMSafeSpaces):
    respx.get(f"{BASE}/api/v1/workspaces").mock(return_value=httpx.Response(429, json={"error": "slow down"}))
    with pytest.raises(RateLimitError):
        await client.workspaces.list()


@respx.mock
@pytest.mark.asyncio
async def test_async_terminal_ticket(client: AsyncLLMSafeSpaces):
    respx.post(f"{BASE}/api/v1/workspaces/ws-1/terminal/ticket").mock(
        return_value=httpx.Response(200, json={"ticket": "abc123", "expiresAt": "2026-01-01T00:00:00Z"})
    )
    t = await client.terminal.get_ticket("ws-1")
    assert t.ticket == "abc123"


@respx.mock
@pytest.mark.asyncio
async def test_async_refresh_compute_202_body(client: AsyncLLMSafeSpaces):
    """202 Accepted MAY carry a body (RFC 7231 §6.3.3); the response must be
    parsed, not discarded like an empty 204."""
    respx.post(f"{BASE}/api/v1/workspaces/ws-1/refresh-compute").mock(
        return_value=httpx.Response(202, json={"restartGeneration": 9})
    )
    result = await client.workspaces.refresh_compute("ws-1")
    assert result == {"restartGeneration": 9}


@respx.mock
@pytest.mark.asyncio
async def test_async_suspend_empty_body_returns_none(client: AsyncLLMSafeSpaces):
    """Guards the shared _request empty-body path: a 204 with no body must
    return None rather than attempting to decode an empty body."""
    respx.post(f"{BASE}/api/v1/workspaces/ws-1/suspend").mock(
        return_value=httpx.Response(204)
    )
    assert await client.workspaces.suspend("ws-1") is None


@respx.mock
@pytest.mark.asyncio
async def test_async_context_manager():
    respx.get(f"{BASE}/api/v1/workspaces").mock(
        return_value=httpx.Response(200, json={"items": [], "pagination": {}})
    )
    async with AsyncLLMSafeSpaces(BASE, api_key="lsp_x") as c:
        result = await c.workspaces.list()
        assert result.items == []


@respx.mock
@pytest.mark.asyncio
async def test_async_login_with_credentials():
    route = respx.post(f"{BASE}/api/v1/auth/login").mock(
        return_value=httpx.Response(200, json={"token": "jwt"})
    )
    respx.get(f"{BASE}/api/v1/workspaces").mock(
        return_value=httpx.Response(200, json={"items": [], "pagination": {}})
    )
    async with AsyncLLMSafeSpaces(BASE, email="u@x.com", password="pw") as c:
        await c.workspaces.list()
    assert route.called


@respx.mock
@pytest.mark.asyncio
async def test_async_401_relogin_after_token_expiry():
    login = respx.post(f"{BASE}/api/v1/auth/login").mock(
        return_value=httpx.Response(200, json={"token": "jwt2"})
    )
    respx.get(f"{BASE}/api/v1/workspaces").mock(
        side_effect=[
            httpx.Response(401, json={"error": "expired"}),
            httpx.Response(200, json={"items": [], "pagination": {}}),
        ]
    )
    async with AsyncLLMSafeSpaces(BASE, email="u@x.com", password="pw") as c:
        await c.workspaces.list()  # first call: 401 → clear token → re-login → retry → 200
    assert login.call_count == 2


@respx.mock
@pytest.mark.asyncio
async def test_async_concurrent_requests_run_in_parallel(client: AsyncLLMSafeSpaces):
    import asyncio
    respx.get(f"{BASE}/api/v1/workspaces").mock(
        return_value=httpx.Response(200, json={"items": [], "pagination": {}})
    )
    await asyncio.gather(*(client.workspaces.list() for _ in range(10)))


@respx.mock
@pytest.mark.asyncio
async def test_async_401_persistent_after_relogin_does_not_loop():
    # Pathological server: accepts login (200 + token) but rejects every
    # subsequent request with 401. Without the single-retry guard this would
    # recurse until Python's stack limit. With the guard, the second 401
    # surfaces as AuthError after exactly one re-login attempt.
    login = respx.post(f"{BASE}/api/v1/auth/login").mock(
        return_value=httpx.Response(200, json={"token": "jwt"})
    )
    respx.get(f"{BASE}/api/v1/workspaces").mock(
        return_value=httpx.Response(401, json={"error": "still expired"})
    )
    async with AsyncLLMSafeSpaces(BASE, email="u@x.com", password="pw") as c:
        with pytest.raises(AuthError):
            await c.workspaces.list()
    # Exactly 2 login calls: initial + one retry. NOT unbounded.
    assert login.call_count == 2


# --- US-62.3: Async parity tests ---


@respx.mock
async def test_async_session_delete():
    respx.delete(f"{BASE}/api/v1/workspaces/ws-1/sessions/sess-1").respond(
        status_code=200
    )
    async with AsyncLLMSafeSpaces(BASE, api_key="lsp_test") as c:
        await c.sessions.delete("ws-1", "sess-1")


@respx.mock
async def test_async_session_enqueue():
    respx.post(f"{BASE}/api/v1/workspaces/ws-1/sessions/sess-1/queue").respond(
        status_code=202, json={"messageID": "qmsg-1"}
    )
    async with AsyncLLMSafeSpaces(BASE, api_key="lsp_test") as c:
        msg_id = await c.sessions.enqueue("ws-1", "sess-1", "hello")
    assert msg_id == "qmsg-1"


@respx.mock
async def test_async_session_list_queue():
    respx.get(f"{BASE}/api/v1/workspaces/ws-1/sessions/sess-1/queue").respond(
        json={"messages": [{"id": "qmsg-1", "text": "hi", "session_id": "sess-1",
             "workspace_id": "ws-1", "enqueued_at": "2026-07-22T00:00:00Z",
             "retry_count": 0}]}
    )
    async with AsyncLLMSafeSpaces(BASE, api_key="lsp_test") as c:
        msgs = await c.sessions.list_queue("ws-1", "sess-1")
    assert len(msgs) == 1


@respx.mock
async def test_async_session_dismiss_queued():
    respx.delete(f"{BASE}/api/v1/workspaces/ws-1/sessions/sess-1/queue/qmsg-1").respond(
        status_code=204
    )
    async with AsyncLLMSafeSpaces(BASE, api_key="lsp_test") as c:
        await c.sessions.dismiss_queued("ws-1", "sess-1", "qmsg-1")


@respx.mock
async def test_async_session_mark_seen():
    respx.put(f"{BASE}/api/v1/workspaces/ws-1/sessions/sess-1/seen").respond(
        status_code=204
    )
    async with AsyncLLMSafeSpaces(BASE, api_key="lsp_test") as c:
        await c.sessions.mark_seen("ws-1", "sess-1")


@respx.mock
async def test_async_user_settings_get():
    respx.get(f"{BASE}/api/v1/users/me/settings").respond(
        json={"settings": {"theme": "dark"}, "schemaVersion": 1}
    )
    async with AsyncLLMSafeSpaces(BASE, api_key="lsp_test") as c:
        result = await c.user_settings.get()
    assert result["settings"]["theme"] == "dark"


@respx.mock
async def test_async_user_settings_set():
    respx.put(f"{BASE}/api/v1/users/me/settings/theme").respond(
        json={"key": "theme", "value": "dark"}
    )
    async with AsyncLLMSafeSpaces(BASE, api_key="lsp_test") as c:
        result = await c.user_settings.set("theme", "dark")
    assert result["key"] == "theme"


def _cred_json(cred_id: str = "cred-1") -> dict:
    return {
        "id": cred_id,
        "name": "my-key",
        "kind": "openai",
        "slug": "my-key",
        "baseURL": "https://api.openai.com/v1",
        "modelAllowlist": [],
        "modelContextLimits": {},
        "modelOutputLimits": {},
        "createdAt": "2026-07-22T00:00:00Z",
        "updatedAt": "2026-07-22T00:00:00Z",
    }


@respx.mock
async def test_async_provider_credentials_create():
    respx.post(f"{BASE}/api/v1/provider-credentials").respond(
        status_code=201, json=_cred_json()
    )
    async with AsyncLLMSafeSpaces(BASE, api_key="lsp_test") as c:
        result = await c.provider_credentials.create(
            name="my-key", kind="openai", slug="my-key", api_key="sk-..."
        )
    assert result.id == "cred-1"


@respx.mock
async def test_async_provider_credentials_list():
    respx.get(f"{BASE}/api/v1/provider-credentials").respond(
        json=[_cred_json("c1"), _cred_json("c2")]
    )
    async with AsyncLLMSafeSpaces(BASE, api_key="lsp_test") as c:
        result = await c.provider_credentials.list()
    assert len(result) == 2


@respx.mock
async def test_async_provider_credentials_delete():
    respx.delete(f"{BASE}/api/v1/provider-credentials/cred-1").respond(
        status_code=204
    )
    async with AsyncLLMSafeSpaces(BASE, api_key="lsp_test") as c:
        await c.provider_credentials.delete("cred-1")


@respx.mock
async def test_async_admin_provider_credentials_list():
    respx.get(f"{BASE}/api/v1/admin/provider-credentials").respond(
        json=[_cred_json()]
    )
    async with AsyncLLMSafeSpaces(BASE, api_key="lsp_test") as c:
        result = await c.admin_provider_credentials.list()
    assert len(result) == 1


@respx.mock
async def test_async_admin_provider_credentials_update():
    respx.put(f"{BASE}/api/v1/admin/provider-credentials/cred-1").respond(
        json=_cred_json()
    )
    from llmsafespaces import UpdateProviderCredentialRequest
    async with AsyncLLMSafeSpaces(BASE, api_key="lsp_test") as c:
        result = await c.admin_provider_credentials.update(
            "cred-1", UpdateProviderCredentialRequest(name="renamed")
        )
    assert result.id == "cred-1"


# ── Workspace model round-trips, async twins (PR #870) ────────────────────
# async_client.py duplicates the parse paths (Workspace(**...),
# WorkspaceListItem(**...)); these pin the full server payload against the
# async client exactly as the sync twins do in test_client.py.


@respx.mock
@pytest.mark.asyncio
async def test_async_workspace_get_full_payload_round_trip(client: AsyncLLMSafeSpaces):
    respx.get(f"{BASE}/api/v1/workspaces/ws-1").mock(
        return_value=httpx.Response(200, json={
            "id": "ws-1", "name": "test", "userId": "u1",
            "runtime": "python:3.11", "storageSize": "10Gi", "phase": "Active",
            "pvcName": "pvc-1", "labels": {"env": "ci"}, "defaultModel": "gpt-test",
            "createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z",
            "agentNeedsRefresh": True,
            "credentialsPendingSince": "2026-01-02T00:00:00Z",
            "devPreviewEnabled": True,
        })
    )
    ws = await client.workspaces.get("ws-1")
    assert ws.agentNeedsRefresh is True
    assert ws.devPreviewEnabled is True
    assert ws.defaultModel == "gpt-test"


@respx.mock
@pytest.mark.asyncio
async def test_async_workspace_list_full_payload_round_trip(client: AsyncLLMSafeSpaces):
    respx.get(f"{BASE}/api/v1/workspaces").mock(
        return_value=httpx.Response(200, json={
            "items": [{
                "id": "ws-1", "name": "test", "userId": "u1",
                "runtime": "python:3.11", "storageSize": "10Gi", "phase": "Active",
                "imageTag": "sha256:abc", "agentVersion": "1.18.10",
                "defaultModel": "gpt-test", "maxActiveSessions": 3,
                "createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z",
                "agentNeedsRefresh": False, "credentialsPendingSince": None,
                "orgId": "org-9",
            }],
            "pagination": None,
        })
    )
    result = await client.workspaces.list()
    item = result.items[0]
    assert item.imageTag == "sha256:abc"
    assert item.agentVersion == "1.18.10"
    assert item.agentNeedsRefresh is False
    assert item.orgId == "org-9"


@respx.mock
@pytest.mark.asyncio
async def test_async_secret_response_full_payload_round_trip(client: AsyncLLMSafeSpaces):
    """Async twin of the SecretResponse round-trip — pins the duplicated
    async parse path against the full server payload (globalDefault has
    no omitempty and is always present)."""
    respx.post(f"{BASE}/api/v1/secrets").mock(
        return_value=httpx.Response(201, json={
            "id": "sec-1", "name": "canary", "type": "env-secret",
            "metadata": {"var_name": "CANARY_VAR"},
            "globalDefault": False,
            "createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z",
        })
    )
    s = await client.secrets.create(name="canary", type="env-secret", value="v", metadata={"var_name": "CANARY_VAR"})
    assert s.globalDefault is False
    assert s.metadata == {"var_name": "CANARY_VAR"}


@respx.mock
@pytest.mark.asyncio
async def test_get_history_page_async():
    route = respx.get(f"{BASE}/api/v1/workspaces/ws-1/sessions/sess-1/message").respond(
        json=[{"id": "m1", "type": "user", "text": "hi"}],
        headers={"X-Next-Cursor": "msg_7"},
    )
    async with AsyncLLMSafeSpaces(BASE, api_key="lsp_test") as client:
        page = await client.sessions.get_history_page("ws-1", "sess-1", limit=10)
    assert route.called
    assert page["nextCursor"] == "msg_7"
    assert page["messages"][0]["id"] == "m1"


@respx.mock
@pytest.mark.asyncio
async def test_mcp_servers_async_crud():
    create_route = respx.post(f"{BASE}/api/v1/me/mcp-servers").respond(
        status_code=201,
        json={"id": "srv-1", "name": "n", "transport": "http", "hasSecret": True, "enabled": True},
    )
    list_route = respx.get(f"{BASE}/api/v1/me/mcp-servers").respond(
        json={"servers": [{"id": "srv-1", "name": "n", "transport": "http", "hasSecret": False, "enabled": True}]}
    )
    bind_route = respx.post(f"{BASE}/api/v1/me/mcp-servers/srv-1/bindings").respond(status_code=200, json={"bound": True})

    async with AsyncLLMSafeSpaces(BASE, api_key="lsp_test") as client:
        srv = await client.mcp_servers.create({"name": "n", "transport": "http"})
        assert srv["id"] == "srv-1"
        listed = await client.mcp_servers.list()
        assert listed[0]["id"] == "srv-1"
        await client.mcp_servers.bind("srv-1", "ws-1")
    assert create_route.called and list_route.called and bind_route.called


@respx.mock
@pytest.mark.asyncio
async def test_mcp_servers_async_unhappy_path():
    respx.get(f"{BASE}/api/v1/me/mcp-servers/missing").respond(
        status_code=404, json={"error": "mcp server not found"}
    )
    async with AsyncLLMSafeSpaces(BASE, api_key="lsp_test") as client:
        with pytest.raises(NotFoundError):
            await client.mcp_servers.get("missing")


# ── Epic 67: workspace upload + files-on-send (wire-level, async) ────────────

UPLOAD_PATH = "/workspace/uploads/11111111-2222-3333-4444-555555555555-notes.txt"
UPLOAD_RESP = {"path": UPLOAD_PATH, "name": "notes.txt", "size": 5}


@respx.mock
@pytest.mark.asyncio
async def test_async_upload_file(client: AsyncLLMSafeSpaces):
    route = respx.post(f"{BASE}/api/v1/workspaces/ws-1/uploads").mock(
        return_value=httpx.Response(201, json=UPLOAD_RESP)
    )
    up = await client.workspaces.upload_file("ws-1", "notes.txt", b"hello")
    assert up.path == UPLOAD_PATH
    assert up.name == "notes.txt"
    request = route.calls[0].request
    assert request.headers["content-type"].startswith("multipart/form-data")
    assert b'name="file"; filename="notes.txt"' in request.content


@respx.mock
@pytest.mark.asyncio
async def test_async_upload_file_phase_on_409(client: AsyncLLMSafeSpaces):
    from llmsafespaces.errors import ConflictError

    respx.post(f"{BASE}/api/v1/workspaces/ws-1/uploads").mock(
        return_value=httpx.Response(409, json={"error": "workspace not active", "phase": "Suspended"})
    )
    with pytest.raises(ConflictError) as exc_info:
        await client.workspaces.upload_file("ws-1", "f.txt", b"x")
    assert exc_info.value.phase == "Suspended"


@respx.mock
@pytest.mark.asyncio
async def test_async_send_prompt_async_files(client: AsyncLLMSafeSpaces):
    import json as _json

    route = respx.post(f"{BASE}/api/v1/workspaces/ws-1/sessions/ses_1/prompt").mock(
        return_value=httpx.Response(202)
    )
    await client.sessions.send_prompt_async("ws-1", "ses_1", "review please", files=[UPLOAD_PATH])
    body = _json.loads(route.calls[0].request.content)
    assert body == {
        "parts": [{"type": "text", "text": "review please"}],
        "files": [UPLOAD_PATH],
    }


@respx.mock
@pytest.mark.asyncio
async def test_async_enqueue_files(client: AsyncLLMSafeSpaces):
    import json as _json

    route = respx.post(f"{BASE}/api/v1/workspaces/ws-1/sessions/ses_1/queue").mock(
        return_value=httpx.Response(202, json={"messageID": "qm-1"})
    )
    mid = await client.sessions.enqueue("ws-1", "ses_1", "later", files=[UPLOAD_PATH])
    assert mid == "qm-1"
    body = _json.loads(route.calls[0].request.content)
    assert body == {"text": "later", "files": [UPLOAD_PATH]}
