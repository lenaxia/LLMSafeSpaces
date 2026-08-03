"""Async LLMSafeSpaces Python SDK client (US-14.4).

Mirrors the synchronous LLMSafeSpaces client using httpx.AsyncClient so
async-native agent frameworks (FastAPI, LangChain async, asyncio pipelines)
can call the API without blocking the event loop.
"""

from __future__ import annotations

from typing import Any

import httpx

from .errors import (
    AuthError,
    ConflictError,
    LLMSafeSpacesError,
    NotFoundError,
    RateLimitError,
    TimeoutError,
)
from .types import (
    APIKey,
    AuthResponse,
    CreateAgentRoleRequest,
    EnsureSessionResponse,
    MessageResponse,
    ProviderCredential,
    SecretResponse,
    TerminalTicket,
    UpdateAgentRoleRequest,
    UpdateProviderCredentialRequest,
    Workspace,
    WorkspaceListItem,
    WorkspaceListResult,
)


class AsyncLLMSafeSpaces:
    """Asynchronous client for the LLMSafeSpaces API."""

    def __init__(
        self,
        base_url: str,
        *,
        api_key: str | None = None,
        email: str | None = None,
        password: str | None = None,
        timeout: float = 120.0,
    ):
        self._base_url = base_url.rstrip("/")
        self._api_key = api_key
        self._email = email
        self._password = password
        self._timeout = timeout
        self._token: str | None = None
        self._client = httpx.AsyncClient(timeout=timeout)

        self.workspaces = _AsyncWorkspacesAPI(self)
        self.sessions = _AsyncSessionsAPI(self)
        self.auth = _AsyncAuthAPI(self)
        self.account = _AsyncAccountAPI(self)
        self.secrets = _AsyncSecretsAPI(self)
        self.terminal = _AsyncTerminalAPI(self)
        self.user_settings = _AsyncUserSettingsAPI(self)
        self.provider_credentials = _AsyncProviderCredentialsAPI(self)
        self.admin_provider_credentials = _AsyncAdminProviderCredentialsAPI(self)
        self.usage = _AsyncUsageAPI(self)
        self.input_requests = _AsyncInputRequestsAPI(self)
        self.probe = _AsyncProbeAPI(self)
        self.prompts = _AsyncPromptsAPI(self)
        self.agent_roles = _AsyncAgentRolesAPI(self)

    async def close(self) -> None:
        await self._client.aclose()

    async def __aenter__(self):
        return self

    async def __aexit__(self, *_):
        await self.close()

    async def _request(
        self, method: str, path: str, *, json: Any = None, timeout: float | None = None
    ) -> Any:
        return await self._request_with_retry(method, path, json=json, timeout=timeout, _retried_401=False)

    async def _request_with_retry(
        self, method: str, path: str, *, json: Any = None, timeout: float | None = None, _retried_401: bool = False
    ) -> Any:
        url = f"{self._base_url}/api/v1{path}"
        headers = await self._auth_headers()

        try:
            resp = await self._client.request(
                method,
                url,
                headers=headers,
                json=json,
                timeout=timeout or self._timeout,
            )
        except httpx.TimeoutException as e:
            raise TimeoutError(str(e)) from e

        if resp.status_code == 401 and self._email and self._token and not _retried_401:
            self._token = None
            return await self._request_with_retry(method, path, json=json, timeout=timeout, _retried_401=True)

        if resp.status_code >= 400:
            self._raise_for_status(resp)

        # 204 No Content has no body by definition. 202 Accepted MAY carry a
        # payload describing the accepted operation (RFC 7231 §6.3.3), so
        # parse the body and return None only when it is actually empty
        # (preserving the void contract for Suspend/Restart).
        if resp.status_code == 204:
            return None
        if resp.content:
            return resp.json()
        return None

    async def _auth_headers(self) -> dict[str, str]:
        if self._api_key:
            return {"Authorization": f"Bearer {self._api_key}"}
        if self._token:
            return {"Authorization": f"Bearer {self._token}"}
        if self._email and self._password:
            await self._login()
            return {"Authorization": f"Bearer {self._token}"}
        return {}

    async def _login(self) -> None:
        resp = await self._client.post(
            f"{self._base_url}/api/v1/auth/login",
            json={"email": self._email, "password": self._password},
            timeout=10.0,
        )
        if resp.status_code != 200:
            raise AuthError("Login failed", resp.status_code)
        self._token = resp.json()["token"]

    @staticmethod
    def _raise_for_status(resp: httpx.Response) -> None:
        msg = "Unknown error"
        try:
            msg = resp.json().get("error", msg)
        except Exception:
            pass
        match resp.status_code:
            case 401 | 403:
                raise AuthError(msg, resp.status_code)
            case 404:
                raise NotFoundError(msg)
            case 409:
                raise ConflictError(msg)
            case 429:
                raise RateLimitError(msg)
            case _:
                raise LLMSafeSpacesError(msg, resp.status_code)


class _AsyncWorkspacesAPI:
    def __init__(self, client: AsyncLLMSafeSpaces):
        self._c = client

    async def list(self, limit: int = 20, offset: int = 0) -> WorkspaceListResult:
        data = await self._c._request("GET", f"/workspaces?limit={limit}&offset={offset}")
        items = [WorkspaceListItem(**i) for i in data.get("items", [])]
        return WorkspaceListResult(items=items, pagination=data.get("pagination"))

    async def create(
        self, *, name: str = "", runtime: str = "", storage_size: str = ""
    ) -> Workspace:
        body = {"name": name, "runtime": runtime, "storageSize": storage_size}
        return Workspace(**await self._c._request("POST", "/workspaces", json=body))

    async def get(self, workspace_id: str) -> Workspace:
        return Workspace(**await self._c._request("GET", f"/workspaces/{workspace_id}"))

    async def rename(self, workspace_id: str, name: str) -> Workspace:
        await self._c._request("PUT", f"/workspaces/{workspace_id}", json={"name": name})
        return await self.get(workspace_id)

    async def delete(self, workspace_id: str) -> None:
        await self._c._request("DELETE", f"/workspaces/{workspace_id}")

    async def suspend(self, workspace_id: str) -> None:
        await self._c._request("POST", f"/workspaces/{workspace_id}/suspend")

    async def activate(self, workspace_id: str) -> dict[str, str]:
        return await self._c._request("POST", f"/workspaces/{workspace_id}/activate")

    async def get_status(self, workspace_id: str) -> dict[str, Any]:
        return await self._c._request("GET", f"/workspaces/{workspace_id}/status")

    async def restart(self, workspace_id: str) -> None:
        await self._c._request("POST", f"/workspaces/{workspace_id}/restart")

    async def refresh_compute(self, workspace_id: str) -> dict[str, Any]:
        return await self._c._request("POST", f"/workspaces/{workspace_id}/refresh-compute")

    async def set_bindings(self, workspace_id: str, secret_ids: list[str]) -> None:
        await self._c._request(
            "PUT",
            f"/workspaces/{workspace_id}/bindings",
            json={"secretIds": secret_ids},
        )

    async def get_bindings(self, workspace_id: str) -> dict[str, Any]:
        return await self._c._request("GET", f"/workspaces/{workspace_id}/bindings")

    async def reload_secrets(self, workspace_id: str) -> dict[str, Any]:
        return await self._c._request("POST", f"/workspaces/{workspace_id}/reload-secrets")

    async def set_model(self, workspace_id: str, model: str) -> None:
        await self._c._request(
            "PUT", f"/workspaces/{workspace_id}/model", json={"model": model}
        )

    async def get_models(self, workspace_id: str) -> dict[str, Any]:
        return await self._c._request("GET", f"/workspaces/{workspace_id}/models")

    async def set_env(self, workspace_id: str, vars: dict[str, str]) -> None:
        await self._c._request("PUT", f"/workspaces/{workspace_id}/env", json={"vars": vars})

    async def get_env(self, workspace_id: str) -> dict[str, Any]:
        return await self._c._request("GET", f"/workspaces/{workspace_id}/env")

    async def delete_env(self, workspace_id: str, var_name: str) -> None:
        await self._c._request("DELETE", f"/workspaces/{workspace_id}/env/{var_name}")


class _AsyncSessionsAPI:
    def __init__(self, client: AsyncLLMSafeSpaces):
        self._c = client

    async def ensure(self, workspace_id: str) -> EnsureSessionResponse:
        return EnsureSessionResponse(
            **await self._c._request("POST", f"/workspaces/{workspace_id}/sessions/new")
        )

    async def list(self, workspace_id: str) -> list[dict[str, Any]]:
        return await self._c._request("GET", f"/workspaces/{workspace_id}/sessions")

    async def send_message(
        self, workspace_id: str, session_id: str, content: str
    ) -> MessageResponse:
        raw = await self._c._request(
            "POST",
            f"/workspaces/{workspace_id}/sessions/{session_id}/message",
            json={"content": content, "parts": [{"type": "text", "text": content}]},
        )
        text = _extract_text(raw)
        return MessageResponse(raw=raw, content=text)

    async def get_history(self, workspace_id: str, session_id: str) -> list[Any]:
        return await self._c._request(
            "GET", f"/workspaces/{workspace_id}/sessions/{session_id}/message"
        )

    async def abort(self, workspace_id: str, session_id: str) -> None:
        await self._c._request(
            "POST", f"/workspaces/{workspace_id}/sessions/{session_id}/abort"
        )

    async def rename(self, workspace_id: str, session_id: str, title: str) -> None:
        await self._c._request(
            "PUT",
            f"/workspaces/{workspace_id}/sessions/{session_id}/title",
            json={"title": title},
        )

    async def get(self, workspace_id: str, session_id: str) -> dict[str, Any]:
        return await self._c._request(
            "GET", f"/workspaces/{workspace_id}/sessions/{session_id}"
        )

    async def get_active(self, workspace_id: str) -> dict[str, Any]:
        return await self._c._request("GET", f"/workspaces/{workspace_id}/sessions/active")

    async def send_prompt_async(
        self, workspace_id: str, session_id: str, message: str
    ) -> None:
        await self._c._request(
            "POST",
            f"/workspaces/{workspace_id}/sessions/{session_id}/prompt",
            json={"message": message},
        )

    async def delete(self, workspace_id: str, session_id: str) -> None:
        await self._c._request(
            "DELETE",
            f"/workspaces/{workspace_id}/sessions/{session_id}",
        )

    async def enqueue(self, workspace_id: str, session_id: str, text: str) -> str:
        resp = await self._c._request(
            "POST",
            f"/workspaces/{workspace_id}/sessions/{session_id}/queue",
            json={"text": text},
        )
        return resp["messageID"]

    async def list_queue(self, workspace_id: str, session_id: str) -> list[dict[str, Any]]:
        resp = await self._c._request(
            "GET",
            f"/workspaces/{workspace_id}/sessions/{session_id}/queue",
        )
        return resp.get("messages", [])

    async def dismiss_queued(self, workspace_id: str, session_id: str, message_id: str) -> None:
        await self._c._request(
            "DELETE",
            f"/workspaces/{workspace_id}/sessions/{session_id}/queue/{message_id}",
        )

    async def mark_seen(self, workspace_id: str, session_id: str) -> None:
        await self._c._request(
            "PUT",
            f"/workspaces/{workspace_id}/sessions/{session_id}/seen",
        )


class _AsyncAuthAPI:
    def __init__(self, client: AsyncLLMSafeSpaces):
        self._c = client

    async def me(self) -> dict[str, Any]:
        return await self._c._request("GET", "/auth/me")

    async def list_api_keys(self) -> list[APIKey]:
        data = await self._c._request("GET", "/auth/api-keys")
        return [APIKey(**k) for k in data]

    async def create_api_key(self, name: str) -> APIKey:
        return APIKey(**await self._c._request("POST", "/auth/api-keys", json={"name": name}))

    async def delete_api_key(self, key_id: str) -> None:
        await self._c._request("DELETE", f"/auth/api-keys/{key_id}")

    async def register(self, username: str, email: str, password: str) -> dict[str, Any]:
        return await self._c._request(
            "POST", "/auth/register",
            json={"username": username, "email": email, "password": password},
        )

    async def logout(self) -> None:
        await self._c._request("POST", "/auth/logout")

    async def request_password_reset(self, email: str) -> None:
        await self._c._request("POST", "/auth/password-reset/request", json={"email": email})

    async def confirm_password_reset(self, token: str, new_password: str) -> None:
        await self._c._request("POST", "/auth/password-reset/confirm",
                               json={"token": token, "newPassword": new_password})

    async def verify_email(self, token: str) -> None:
        await self._c._request("POST", "/auth/verify-email", json={"token": token})

    async def resend_verification(self, email: str) -> None:
        await self._c._request("POST", "/auth/verify-email/resend", json={"email": email})

    async def lookup(self, email: str) -> str:
        resp = await self._c._request("POST", "/auth/lookup", json={"email": email})
        return resp.get("redirectUrl", "")

    async def unlock_dek(self, password: str) -> None:
        await self._c._request("POST", "/auth/unlock-dek", json={"password": password})


class _AsyncAccountAPI:
    def __init__(self, client: AsyncLLMSafeSpaces):
        self._c = client

