"""Unit tests for LLMSafeSpaces Python SDK."""

import pytest
import httpx
import respx

from llmsafespaces import (
    LLMSafeSpaces,
    NotFoundError,
    AuthError,
    ConflictError,
    TimeoutError,
    MessageResponse,
    ProviderCredential,
)


BASE = "http://localhost:8080/api/v1"


@respx.mock
def test_list_workspaces():
    respx.get(f"{BASE}/workspaces?limit=20&offset=0").respond(
        json={"items": [{"id": "ws-1", "name": "test", "userId": "u1", "runtime": "python", "storageSize": "10Gi", "createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z"}], "pagination": None}
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    result = client.workspaces.list()
    assert len(result.items) == 1
    assert result.items[0].id == "ws-1"


@respx.mock
def test_create_workspace():
    respx.post(f"{BASE}/workspaces").respond(
        status_code=201,
        json={"id": "ws-new", "name": "my-ws", "userId": "u1", "runtime": "python:3.11", "storageSize": "10Gi", "phase": "Pending", "createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z"},
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    ws = client.workspaces.create(name="my-ws", runtime="python:3.11", storage_size="10Gi")
    assert ws.id == "ws-new"


@respx.mock
def test_not_found():
    respx.get(f"{BASE}/workspaces/nonexistent").respond(status_code=404, json={"error": "workspace not found"})
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    with pytest.raises(NotFoundError):
        client.workspaces.get("nonexistent")


@respx.mock
def test_auth_error():
    respx.get(f"{BASE}/auth/me").respond(status_code=401, json={"error": "authentication required"})
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_bad")
    with pytest.raises(AuthError):
        client.auth.me()


@respx.mock
def test_send_message_extracts_content():
    opencode_resp = {
        "id": "msg-1",
        "role": "assistant",
        "parts": [
            {"type": "text", "text": "Hello "},
            {"type": "text", "text": "world!"},
            {"type": "tool-invocation", "toolName": "read_file"},
        ],
    }
    respx.post(f"{BASE}/workspaces/ws-1/sessions/sess-1/message").respond(json=opencode_resp)
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    result = client.sessions.send_message("ws-1", "sess-1", "hi")
    assert isinstance(result, MessageResponse)
    assert result.content == "Hello world!"
    assert result.raw == opencode_resp


@respx.mock
def test_ensure_session():
    respx.post(f"{BASE}/workspaces/ws-1/sessions/new").respond(
        json={"workspaceId": "ws-1", "workspacePhase": "Active", "sessionId": "sess-1", "resumed": False}
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    result = client.sessions.ensure("ws-1")
    assert result.sessionId == "sess-1"


@respx.mock
def test_terminal_ticket():
    respx.post(f"{BASE}/workspaces/ws-1/terminal/ticket").respond(
        json={"ticket": "tkt_abc123", "expiresAt": "2026-05-29T18:00:00Z"}
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    ticket = client.terminal.get_ticket("ws-1")
    assert ticket.ticket == "tkt_abc123"


@respx.mock
def test_api_key_header():
    route = respx.get(f"{BASE}/auth/me").respond(json={"id": "u1", "username": "test"})
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_mykey")
    client.auth.me()
    assert route.calls[0].request.headers["authorization"] == "Bearer lsp_mykey"


@respx.mock
def test_auto_login_with_credentials():
    respx.post(f"{BASE}/auth/login").respond(json={"token": "jwt-abc", "user": {"id": "u1"}})
    respx.get(f"{BASE}/auth/me").respond(json={"id": "u1", "username": "test"})
    client = LLMSafeSpaces("http://localhost:8080", email="test@example.com", password="pass123")
    result = client.auth.me()
    assert result["id"] == "u1"


@respx.mock
def test_suspend_workspace():
    respx.post(f"{BASE}/workspaces/ws-1/suspend").respond(status_code=202)
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    # Should not raise
    client.workspaces.suspend("ws-1")


@respx.mock
def test_refresh_compute_202_body():
    """202 Accepted MAY carry a body (RFC 7231 §6.3.3); the response must be
    parsed, not discarded like an empty 204."""
    respx.post(f"{BASE}/workspaces/ws-1/refresh-compute").respond(
        status_code=202, json={"restartGeneration": 7}
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    result = client.workspaces.refresh_compute("ws-1")
    assert result == {"restartGeneration": 7}


@respx.mock
def test_refresh_compute_api_error():
    respx.post(f"{BASE}/workspaces/ws-1/refresh-compute").respond(status_code=409)
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    with pytest.raises(Exception):
        client.workspaces.refresh_compute("ws-1")


@respx.mock
def test_suspend_204_empty_body_returns_none():
    """Guards the shared _request empty-body path: a 204 (or 202) with no body
    must return None rather than attempting to decode an empty body."""
    respx.post(f"{BASE}/workspaces/ws-1/suspend").respond(status_code=204)
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    assert client.workspaces.suspend("ws-1") is None


# --- US-62.2: New service tests ---


def test_version_is_importable():
    import llmsafespaces

    assert llmsafespaces.__version__


@respx.mock
def test_session_delete():
    respx.delete(f"{BASE}/workspaces/ws-1/sessions/sess-1").respond(status_code=200)
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    client.sessions.delete("ws-1", "sess-1")


@respx.mock
def test_session_enqueue():
    respx.post(f"{BASE}/workspaces/ws-1/sessions/sess-1/queue").respond(
        status_code=202, json={"messageID": "qmsg-1"}
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    msg_id = client.sessions.enqueue("ws-1", "sess-1", "hello")
    assert msg_id == "qmsg-1"


@respx.mock
def test_session_list_queue():
    respx.get(f"{BASE}/workspaces/ws-1/sessions/sess-1/queue").respond(
        json={"messages": [{"id": "qmsg-1", "text": "hi", "session_id": "sess-1",
             "workspace_id": "ws-1", "enqueued_at": "2026-07-22T00:00:00Z",
             "retry_count": 0}]}
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    msgs = client.sessions.list_queue("ws-1", "sess-1")
    assert len(msgs) == 1
    assert msgs[0]["id"] == "qmsg-1"


@respx.mock
def test_session_dismiss_queued():
    respx.delete(f"{BASE}/workspaces/ws-1/sessions/sess-1/queue/qmsg-1").respond(
        status_code=204
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    client.sessions.dismiss_queued("ws-1", "sess-1", "qmsg-1")


@respx.mock
def test_session_mark_seen():
    respx.put(f"{BASE}/workspaces/ws-1/sessions/sess-1/seen").respond(
        status_code=204
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    client.sessions.mark_seen("ws-1", "sess-1")


@respx.mock
def test_user_settings_get():
    respx.get(f"{BASE}/users/me/settings").respond(
        json={"settings": {"theme": "dark"}, "schemaVersion": 1}
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    result = client.user_settings.get()
    assert result["settings"]["theme"] == "dark"


@respx.mock
def test_user_settings_get_schema():
    respx.get(f"{BASE}/users/me/settings/schema").respond(
        json={"schemaVersion": "1", "settings": [{"key": "theme"}]}
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    result = client.user_settings.get_schema()
    assert result["schemaVersion"] == "1"


@respx.mock
def test_user_settings_set():
    respx.put(f"{BASE}/users/me/settings/theme").respond(
        json={"key": "theme", "value": "dark"}
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    result = client.user_settings.set("theme", "dark")
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
def test_provider_credentials_create():
    respx.post(f"{BASE}/provider-credentials").respond(
        status_code=201, json=_cred_json()
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    result = client.provider_credentials.create(
        name="my-key", kind="openai", slug="my-key", api_key="sk-..."
    )
    assert isinstance(result, ProviderCredential)
    assert result.id == "cred-1"


@respx.mock
def test_provider_credentials_create_207_partial_success():
    respx.post(f"{BASE}/provider-credentials").respond(
        status_code=207,
        json={"credential": _cred_json(), "bindWarning": "failed to auto-bind"},
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    result = client.provider_credentials.create(
        name="my-key", kind="openai", slug="my-key", api_key="sk-..."
    )
    assert isinstance(result, ProviderCredential)
    assert result.id == "cred-1"


@respx.mock
def test_provider_credentials_create_conflict():
    respx.post(f"{BASE}/provider-credentials").respond(
        status_code=409, json={"error": "slug already exists"}
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    with pytest.raises(ConflictError):
        client.provider_credentials.create(
            name="dup", kind="openai", slug="dup", api_key="sk-..."
        )


@respx.mock
def test_provider_credentials_get_not_found():
    respx.get(f"{BASE}/provider-credentials/nonexistent").respond(
        status_code=404, json={"error": "credential not found"}
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    with pytest.raises(NotFoundError):
        client.provider_credentials.get("nonexistent")


@respx.mock
def test_provider_credentials_list():
    respx.get(f"{BASE}/provider-credentials").respond(
        json=[_cred_json("c1"), _cred_json("c2")]
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    result = client.provider_credentials.list()
    assert len(result) == 2
    assert result[0].id == "c1"


@respx.mock
def test_provider_credentials_get():
    respx.get(f"{BASE}/provider-credentials/cred-1").respond(json=_cred_json())
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    result = client.provider_credentials.get("cred-1")
    assert result.slug == "my-key"


@respx.mock
def test_provider_credentials_delete():
    respx.delete(f"{BASE}/provider-credentials/cred-1").respond(status_code=204)
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    client.provider_credentials.delete("cred-1")


@respx.mock
def test_provider_credentials_probe_models():
    respx.get(f"{BASE}/provider-credentials/cred-1/models").respond(
        json={"models": [{"id": "gpt-4", "name": "GPT-4"}]}
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    result = client.provider_credentials.probe_models("cred-1")
    assert "models" in result


@respx.mock
def test_provider_credentials_list_bindings():
    respx.get(f"{BASE}/provider-credentials/cred-1/bindings").respond(
        json={"workspaceIds": ["ws-1", "ws-2"], "bindings": []}
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    result = client.provider_credentials.list_bindings("cred-1")
    assert result == ["ws-1", "ws-2"]


@respx.mock
def test_provider_credentials_bind():
    respx.post(f"{BASE}/provider-credentials/cred-1/bind/ws-1").respond(
        status_code=200, json={"ok": True}
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    client.provider_credentials.bind("cred-1", "ws-1")


@respx.mock
def test_provider_credentials_unbind():
    respx.delete(f"{BASE}/provider-credentials/cred-1/bind/ws-1").respond(
        status_code=204
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    client.provider_credentials.unbind("cred-1", "ws-1")


@respx.mock
def test_admin_provider_credentials_list():
    respx.get(f"{BASE}/admin/provider-credentials").respond(
        json=[_cred_json()]
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    result = client.admin_provider_credentials.list()
    assert len(result) == 1


@respx.mock
def test_admin_provider_credentials_create():
    respx.post(f"{BASE}/admin/provider-credentials").respond(
        status_code=201, json=_cred_json()
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    result = client.admin_provider_credentials.create(
        name="admin-key", kind="anthropic", slug="admin-key", api_key="sk-..."
    )
    assert result.id == "cred-1"


@respx.mock
def test_admin_provider_credentials_get():
    respx.get(f"{BASE}/admin/provider-credentials/cred-1").respond(
        json=_cred_json()
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    result = client.admin_provider_credentials.get("cred-1")
    assert result.id == "cred-1"


@respx.mock
def test_admin_provider_credentials_update():
    respx.put(f"{BASE}/admin/provider-credentials/cred-1").respond(
        json=_cred_json()
    )
    from llmsafespaces import UpdateProviderCredentialRequest
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    result = client.admin_provider_credentials.update(
        "cred-1", UpdateProviderCredentialRequest(name="renamed")
    )
    assert result.id == "cred-1"


@respx.mock
def test_admin_provider_credentials_delete():
    respx.delete(f"{BASE}/admin/provider-credentials/cred-1").respond(
        status_code=204
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    client.admin_provider_credentials.delete("cred-1")


@respx.mock
def test_admin_provider_credentials_probe_models():
    respx.get(f"{BASE}/admin/provider-credentials/cred-1/models").respond(
        json={"models": [{"id": "claude-3"}]}
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    result = client.admin_provider_credentials.probe_models("cred-1")
    assert "models" in result


@respx.mock
def test_admin_provider_credentials_auto_apply_create():
    respx.post(f"{BASE}/admin/provider-credentials/cred-1/auto-apply").respond(
        status_code=201, json={"id": "aa-1"}
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    client.admin_provider_credentials.create_auto_apply(
        "cred-1", target_type="all"
    )


@respx.mock
def test_admin_provider_credentials_auto_apply_list():
    respx.get(f"{BASE}/admin/provider-credentials/cred-1/auto-apply").respond(
        json=[{"credentialId": "cred-1", "targetType": "all", "withinPriority": 0}]
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    result = client.admin_provider_credentials.list_auto_apply("cred-1")
    assert len(result) == 1


@respx.mock
def test_admin_provider_credentials_auto_apply_delete():
    respx.delete(
        f"{BASE}/admin/provider-credentials/cred-1/auto-apply/user/u1"
    ).respond(status_code=204)
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    client.admin_provider_credentials.delete_auto_apply(
        "cred-1", "user", "u1"
    )
