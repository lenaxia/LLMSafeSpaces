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
        return_value=httpx.Response(200, json={"parts": [{"type": "text", "text": "hello"}]})
    )
    resp = await client.sessions.send_message("ws-1", "ses-1", "hi")
    assert resp.content == "hello"


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
    async with AsyncLLMSafeSpaces(BASE, api_key="lsp_test") as c:
        result = await c.admin_provider_credentials.update("cred-1", name="renamed")
    assert result.id == "cred-1"
