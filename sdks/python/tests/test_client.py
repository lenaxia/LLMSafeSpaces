"""Unit tests for LLMSafeSpaces Python SDK."""

import pytest
import httpx
import respx

from llmsafespaces import (
    LLMSafeSpaces,
    NotFoundError,
    AuthError,
    ConflictError,
    LLMSafeSpacesError,
    TimeoutError,
    ServiceUnavailableError,
    Message,
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
def test_service_unavailable_error():
    respx.get(f"{BASE}/workspaces?limit=20&offset=0").respond(
        status_code=503,
        json={
            "error": "workspace connection failed",
            "message": "The agent is not responding.",
            "reason": "agent_unreachable",
            "retryAfter": 10,
        },
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    with pytest.raises(ServiceUnavailableError) as exc_info:
        client.workspaces.list()
    assert exc_info.value.reason == "agent_unreachable"
    assert exc_info.value.retry_after == 10
    assert "not responding" in exc_info.value.args[0]


@respx.mock
def test_send_message_returns_contract_message():
    # Contract-shaped response (pkg/session Message via the adapter seam).
    contract_resp = {
        "id": "msg-1",
        "type": "assistant",
        "parts": [
            {"type": "text", "text": "Hello "},
            {"type": "text", "text": "world!"},
            {"type": "tool", "tool": {"name": "read_file", "state": {"status": "completed"}}},
        ],
    }
    respx.post(f"{BASE}/workspaces/ws-1/sessions/sess-1/message").respond(json=contract_resp)
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    result = client.sessions.send_message("ws-1", "sess-1", "hi")
    assert isinstance(result, dict)
    assert result["type"] == "assistant"
    assert len(result["parts"]) == 3
    text = "".join(p.get("text", "") for p in result["parts"] if p.get("type") == "text")
    assert text == "Hello world!"


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
def test_session_enqueue_empty_text_400():
    respx.post(f"{BASE}/workspaces/ws-1/sessions/sess-1/queue").respond(
        status_code=400, json={"error": "text must not be empty"}
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    with pytest.raises(LLMSafeSpacesError):
        client.sessions.enqueue("ws-1", "sess-1", "")


@respx.mock
def test_session_delete_not_found():
    respx.delete(f"{BASE}/workspaces/ws-1/sessions/nonexistent").respond(
        status_code=404, json={"error": "session not found"}
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    with pytest.raises(NotFoundError):
        client.sessions.delete("ws-1", "nonexistent")


@respx.mock
def test_user_settings_set_not_found():
    respx.put(f"{BASE}/users/me/settings/nonexistent_key").respond(
        status_code=404, json={"error": "setting not found"}
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    with pytest.raises(NotFoundError):
        client.user_settings.set("nonexistent_key", "value")


@respx.mock
def test_admin_provider_credentials_get_not_found():
    respx.get(f"{BASE}/admin/provider-credentials/nonexistent").respond(
        status_code=404, json={"error": "credential not found"}
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    with pytest.raises(NotFoundError):
        client.admin_provider_credentials.get("nonexistent")


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


# ─── Epic 64: Workflows + Triggers ───────────────────────────────────────────

@respx.mock
def test_list_workflows():
    respx.get(f"{BASE}/me/workflows").respond(
        json={"workflows": [{"id": "wf-1", "name": "test", "status": "active"}]}
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    result = client.workflows.list()
    assert len(result) == 1
    assert result[0]["id"] == "wf-1"


@respx.mock
def test_create_workflow():
    respx.post(f"{BASE}/me/workflows").respond(
        status_code=201,
        json={"id": "wf-new", "name": "test-wf", "status": "draft"},
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    wf = client.workflows.create(name="test-wf", spec_yaml="{}", status="draft")
    assert wf["id"] == "wf-new"


@respx.mock
def test_run_workflow():
    respx.post(f"{BASE}/me/workflows/wf-1/runs").respond(
        status_code=202,
        json={"id": "run-1", "status": "queued"},
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    run = client.workflows.run("wf-1")
    assert run["id"] == "run-1"
    assert run["status"] == "queued"


@respx.mock
def test_cancel_run():
    respx.post(f"{BASE}/me/runs/run-1/cancel").respond(status_code=200)
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    client.workflows.cancel_run("run-1")  # no exception = pass


@respx.mock
def test_list_triggers():
    respx.get(f"{BASE}/me/triggers").respond(
        json={"triggers": [{"id": "trig-1", "name": "cron", "enabled": True}]}
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    result = client.triggers.list()
    assert len(result) == 1
    assert result[0]["id"] == "trig-1"


@respx.mock
def test_delete_trigger():
    respx.delete(f"{BASE}/me/triggers/trig-1").respond(status_code=200)
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    client.triggers.delete("trig-1")  # no exception = pass


# ── Workspace model round-trip (PR #870 / issue #867) ─────────────────────
# The server emits fields the dataclasses historically lacked; strict
# **kwargs expansion raised TypeError instead of degrading. These tests pin
# the FULL server payload shapes (pkg/types/workspace.go Workspace DTO and
# WorkspaceListItem) so the next added field fails here first.


@respx.mock
def test_workspace_get_full_payload_round_trip():
    respx.get(f"{BASE}/workspaces/ws-1").respond(
        json={
            "id": "ws-1",
            "name": "test",
            "userId": "u1",
            "runtime": "python:3.11",
            "storageSize": "10Gi",
            "phase": "Active",
            "pvcName": "pvc-1",
            "labels": {"env": "ci"},
            "defaultModel": "gpt-test",
            "createdAt": "2026-01-01T00:00:00Z",
            "updatedAt": "2026-01-01T00:00:00Z",
            "agentNeedsRefresh": True,
            "credentialsPendingSince": "2026-01-02T00:00:00Z",
            "devPreviewEnabled": True,
        }
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    ws = client.workspaces.get("ws-1")
    assert ws.agentNeedsRefresh is True
    assert ws.devPreviewEnabled is True
    assert ws.defaultModel == "gpt-test"
    assert ws.credentialsPendingSince == "2026-01-02T00:00:00Z"


@respx.mock
def test_workspace_list_full_payload_round_trip():
    respx.get(f"{BASE}/workspaces?limit=20&offset=0").respond(
        json={
            "items": [
                {
                    "id": "ws-1",
                    "name": "test",
                    "userId": "u1",
                    "runtime": "python:3.11",
                    "storageSize": "10Gi",
                    "phase": "Active",
                    "imageTag": "sha256:abc",
                    "agentVersion": "1.18.10",
                    "defaultModel": "gpt-test",
                    "maxActiveSessions": 3,
                    "createdAt": "2026-01-01T00:00:00Z",
                    "updatedAt": "2026-01-01T00:00:00Z",
                    "agentNeedsRefresh": False,
                    "credentialsPendingSince": None,
                    "orgId": "org-9",
                }
            ],
            "pagination": None,
        }
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    result = client.workspaces.list()
    item = result.items[0]
    assert item.agentNeedsRefresh is False
    assert item.imageTag == "sha256:abc"
    assert item.agentVersion == "1.18.10"
    assert item.orgId == "org-9"
    assert item.maxActiveSessions == 3


def test_workspace_models_reject_unknown_fields():
    """Strict parsing IS the drift detector (PR #870 design decision).

    The dataclasses deliberately expand **kwargs with no catch-all field:
    an unknown server field must raise TypeError so drift fails loudly in
    the SDK suite (and canaries) instead of silently degrading — the
    #867 failure mode. If this test ever fails because the server added a
    field, add the field to the dataclass AND the OpenAPI schema.
    """
    from llmsafespaces.types import Workspace, WorkspaceListItem

    with pytest.raises(TypeError):
        WorkspaceListItem(
            id="ws-1", name="x", userId="u1", runtime="python",
            storageSize="10Gi", createdAt="t", updatedAt="t",
            someFutureServerField=True,
        )
    with pytest.raises(TypeError):
        Workspace(
            id="ws-1", name="x", userId="u1", runtime="python",
            storageSize="10Gi", phase="Active", createdAt="t", updatedAt="t",
            anotherFutureField=1,
        )


@respx.mock
def test_secret_response_full_payload_round_trip():
    """SecretResponse must accept the full server payload (pkg/secrets
    SecretResponse: id, name, type, metadata, globalDefault, createdAt,
    updatedAt) — globalDefault is always present (no omitempty)."""
    respx.post(f"{BASE}/secrets").respond(
        status_code=201,
        json={
            "id": "sec-1", "name": "canary", "type": "env-secret",
            "metadata": {"var_name": "CANARY_VAR"},
            "globalDefault": False,
            "createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z",
        },
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    s = client.secrets.create(name="canary", type="env-secret", value="v", metadata={"var_name": "CANARY_VAR"})
    assert s.globalDefault is False
    assert s.metadata == {"var_name": "CANARY_VAR"}


@respx.mock
def test_get_history_page_params_and_cursor():
    route = respx.get(f"{BASE}/workspaces/ws-1/sessions/sess-1/message").respond(
        json=[{"id": "m1", "type": "user", "text": "hi"}],
        headers={"X-Next-Cursor": "msg_42"},
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    page = client.sessions.get_history_page("ws-1", "sess-1", limit=50, before="msg_99")
    assert route.called
    assert "limit=50" in str(route.calls.last.request.url)
    assert "before=msg_99" in str(route.calls.last.request.url)
    assert page["nextCursor"] == "msg_42"
    assert page["messages"][0]["id"] == "m1"


@respx.mock
def test_get_history_page_defaults_empty_cursor():
    route = respx.get(f"{BASE}/workspaces/ws-1/sessions/sess-1/message").respond(json=[])
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    page = client.sessions.get_history_page("ws-1", "sess-1")
    assert "?" not in str(route.calls.last.request.url)
    assert page["nextCursor"] == ""
    assert page["messages"] == []


@respx.mock
def test_mcp_servers_user_scope_crud():
    create_route = respx.post(f"{BASE}/me/mcp-servers").respond(
        status_code=201,
        json={"id": "srv-1", "name": "n", "transport": "http", "hasSecret": True, "enabled": True},
    )
    list_route = respx.get(f"{BASE}/me/mcp-servers").respond(
        json={"servers": [{"id": "srv-1", "name": "n", "transport": "stdio", "hasSecret": False, "enabled": True}]}
    )
    update_route = respx.put(f"{BASE}/me/mcp-servers/srv-1").respond(
        json={"id": "srv-1", "name": "renamed", "transport": "http", "hasSecret": False, "enabled": True}
    )
    delete_route = respx.delete(f"{BASE}/me/mcp-servers/srv-1").respond(status_code=200, json={"deleted": True})

    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    srv = client.mcp_servers.create({"name": "n", "transport": "http", "url": "https://m.example"})
    assert srv["id"] == "srv-1"
    assert create_route.called

    listed = client.mcp_servers.list()
    assert listed[0]["transport"] == "stdio"

    updated = client.mcp_servers.update("srv-1", {"name": "renamed"})
    assert updated["name"] == "renamed"
    import json as _json
    assert _json.loads(update_route.calls.last.request.content) == {"name": "renamed"}

    client.mcp_servers.delete("srv-1")
    assert delete_route.called


@respx.mock
def test_mcp_servers_bind_and_auto_apply():
    bind_route = respx.post(f"{BASE}/me/mcp-servers/srv-1/bindings").respond(status_code=200, json={"bound": True})
    aa_route = respx.post(f"{BASE}/me/mcp-servers/srv-1/auto-apply").respond(
        status_code=201, json={"created": True}
    )
    list_aa_route = respx.get(f"{BASE}/me/mcp-servers/srv-1/auto-apply").respond(
        json={"rules": [{"targetType": "all"}]}
    )

    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    client.mcp_servers.bind("srv-1", "ws-1")
    import json as _json
    assert _json.loads(bind_route.calls.last.request.content) == {"workspaceId": "ws-1"}

    client.mcp_servers.create_auto_apply("srv-1", "user", "u-1")
    assert _json.loads(aa_route.calls.last.request.content) == {"targetType": "user", "targetId": "u-1"}

    rules = client.mcp_servers.list_auto_apply("srv-1")
    assert rules == [{"targetType": "all"}]
    assert list_aa_route.called


@respx.mock
def test_mcp_servers_admin_and_org_scopes():
    admin_route = respx.get(f"{BASE}/admin/mcp-servers").respond(json={"servers": []})
    org_route = respx.get(f"{BASE}/orgs/org-9/mcp-servers").respond(json={"servers": []})
    del_aa_route = respx.delete(f"{BASE}/admin/mcp-servers/srv-1/auto-apply/user").respond(status_code=204)

    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    assert client.admin_mcp_servers.list() == []
    assert client.org_mcp_servers.list("org-9") == []
    client.admin_mcp_servers.delete_auto_apply("srv-1", "user")
    assert admin_route.called and org_route.called and del_aa_route.called


@respx.mock
def test_mcp_servers_unhappy_paths():
    respx.get(f"{BASE}/me/mcp-servers/missing").respond(
        status_code=404, json={"error": "mcp server not found"}
    )
    respx.post(f"{BASE}/me/mcp-servers").respond(
        status_code=409, json={"error": "name already in use"}
    )
    respx.get(f"{BASE}/me/mcp-servers").respond(json={"servers": []})
    respx.get(f"{BASE}/me/mcp-servers/srv-1/auto-apply").respond(json={"rules": []})

    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    with pytest.raises(NotFoundError):
        client.mcp_servers.get("missing")
    with pytest.raises(ConflictError):
        client.mcp_servers.create({"name": "dup", "transport": "http"})
    assert client.mcp_servers.list() == []
    assert client.mcp_servers.list_auto_apply("srv-1") == []


@respx.mock
def test_mcp_servers_admin_delete_auto_apply_variants():
    bare = respx.delete(f"{BASE}/admin/mcp-servers/srv-1/auto-apply/all").respond(status_code=204)
    scoped = respx.delete(f"{BASE}/admin/mcp-servers/srv-1/auto-apply/user/u-1").respond(status_code=204)

    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    client.admin_mcp_servers.delete_auto_apply("srv-1", "all")
    client.admin_mcp_servers.delete_auto_apply("srv-1", "user", "u-1")
    assert bare.called and scoped.called


# ── Epic 67: workspace upload + files-on-send (wire-level) ──────────────────

UPLOAD_PATH = "/workspace/uploads/11111111-2222-3333-4444-555555555555-notes.txt"
UPLOAD_RESP = {"path": UPLOAD_PATH, "name": "notes.txt", "size": 5}


@respx.mock
def test_upload_file():
    route = respx.post(f"{BASE}/workspaces/ws-1/uploads").respond(status_code=201, json=UPLOAD_RESP)
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    up = client.workspaces.upload_file("ws-1", "notes.txt", b"hello")
    assert up.path == UPLOAD_PATH
    assert up.name == "notes.txt"
    assert up.size == 5
    request = route.calls[0].request
    assert request.headers["content-type"].startswith("multipart/form-data")
    assert b'name="file"; filename="notes.txt"' in request.content
    assert b"hello" in request.content


@respx.mock
def test_upload_file_phase_surfaces_on_409():
    respx.post(f"{BASE}/workspaces/ws-1/uploads").respond(
        status_code=409, json={"error": "workspace not active", "phase": "Suspended"}
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    with pytest.raises(ConflictError) as exc_info:
        client.workspaces.upload_file("ws-1", "f.txt", b"x")
    assert exc_info.value.phase == "Suspended"


@respx.mock
def test_send_prompt_async_files():
    route = respx.post(f"{BASE}/workspaces/ws-1/sessions/ses_1/prompt").respond(status_code=202)
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    client.sessions.send_prompt_async("ws-1", "ses_1", "review please", files=[UPLOAD_PATH])
    import json as _json

    body = _json.loads(route.calls[0].request.content)
    assert body == {
        "parts": [{"type": "text", "text": "review please"}],
        "files": [UPLOAD_PATH],
    }


@respx.mock
def test_send_prompt_async_parts_shape_no_dead_fields():
    route = respx.post(f"{BASE}/workspaces/ws-1/sessions/ses_1/prompt").respond(status_code=202)
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    client.sessions.send_prompt_async("ws-1", "ses_1", "hi")
    import json as _json

    body = _json.loads(route.calls[0].request.content)
    assert body == {"parts": [{"type": "text", "text": "hi"}]}
    assert "message" not in body
    assert "content" not in body


@respx.mock
def test_enqueue_files():
    route = respx.post(f"{BASE}/workspaces/ws-1/sessions/ses_1/queue").respond(
        status_code=202, json={"messageID": "qm-1"}
    )
    client = LLMSafeSpaces("http://localhost:8080", api_key="lsp_test")
    mid = client.sessions.enqueue("ws-1", "ses_1", "later", files=[UPLOAD_PATH])
    assert mid == "qm-1"
    import json as _json

    body = _json.loads(route.calls[0].request.content)
    assert body == {"text": "later", "files": [UPLOAD_PATH]}
