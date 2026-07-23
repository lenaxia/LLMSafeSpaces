"""LLMSafeSpaces Python SDK client."""

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
    EnsureSessionResponse,
    MessageResponse,
    ProviderCredential,
    SecretResponse,
    TerminalTicket,
    Workspace,
    WorkspaceListItem,
    WorkspaceListResult,
)


class LLMSafeSpaces:
    """Synchronous client for the LLMSafeSpaces API."""

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
        self._client = httpx.Client(timeout=timeout)

        self.workspaces = _WorkspacesAPI(self)
        self.sessions = _SessionsAPI(self)
        self.auth = _AuthAPI(self)
        self.account = _AccountAPI(self)
        self.secrets = _SecretsAPI(self)
        self.terminal = _TerminalAPI(self)
        self.user_settings = _UserSettingsAPI(self)
        self.provider_credentials = _ProviderCredentialsAPI(self)
        self.admin_provider_credentials = _AdminProviderCredentialsAPI(self)
        self.prompts = _PromptsAPI(self)
        self.agent_roles = _AgentRolesAPI(self)

    def close(self) -> None:
        self._client.close()

    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.close()

    def _request(
        self, method: str, path: str, *, json: Any = None, timeout: float | None = None
    ) -> Any:
        return self._request_with_retry(method, path, json=json, timeout=timeout, _retried_401=False)

    def _request_with_retry(
        self, method: str, path: str, *, json: Any = None, timeout: float | None = None, _retried_401: bool = False
    ) -> Any:
        url = f"{self._base_url}/api/v1{path}"
        headers = self._auth_headers()

        try:
            resp = self._client.request(
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
            return self._request_with_retry(method, path, json=json, timeout=timeout, _retried_401=True)

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

    def _auth_headers(self) -> dict[str, str]:
        if self._api_key:
            return {"Authorization": f"Bearer {self._api_key}"}
        if self._token:
            return {"Authorization": f"Bearer {self._token}"}
        if self._email and self._password:
            self._login()
            return {"Authorization": f"Bearer {self._token}"}
        return {}

    def _login(self) -> None:
        resp = self._client.post(
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


class _WorkspacesAPI:
    def __init__(self, client: LLMSafeSpaces):
        self._c = client

    def list(self, limit: int = 20, offset: int = 0) -> WorkspaceListResult:
        data = self._c._request("GET", f"/workspaces?limit={limit}&offset={offset}")
        items = [WorkspaceListItem(**i) for i in data.get("items", [])]
        return WorkspaceListResult(items=items, pagination=data.get("pagination"))

    def create(
        self, *, name: str = "", runtime: str = "", storage_size: str = ""
    ) -> Workspace:
        body = {"name": name, "runtime": runtime, "storageSize": storage_size}
        return Workspace(**self._c._request("POST", "/workspaces", json=body))

    def get(self, workspace_id: str) -> Workspace:
        return Workspace(**self._c._request("GET", f"/workspaces/{workspace_id}"))

    def rename(self, workspace_id: str, name: str) -> Workspace:
        self._c._request("PUT", f"/workspaces/{workspace_id}", json={"name": name})
        return self.get(workspace_id)

    def delete(self, workspace_id: str) -> None:
        self._c._request("DELETE", f"/workspaces/{workspace_id}")

    def suspend(self, workspace_id: str) -> None:
        self._c._request("POST", f"/workspaces/{workspace_id}/suspend")

    def activate(self, workspace_id: str) -> dict[str, str]:
        return self._c._request("POST", f"/workspaces/{workspace_id}/activate")

    def get_status(self, workspace_id: str) -> dict[str, Any]:
        return self._c._request("GET", f"/workspaces/{workspace_id}/status")

    def restart(self, workspace_id: str) -> None:
        self._c._request("POST", f"/workspaces/{workspace_id}/restart")

    def refresh_compute(self, workspace_id: str) -> dict[str, Any]:
        return self._c._request("POST", f"/workspaces/{workspace_id}/refresh-compute")

    def set_bindings(self, workspace_id: str, secret_ids: list[str]) -> None:
        self._c._request(
            "PUT",
            f"/workspaces/{workspace_id}/bindings",
            json={"secretIds": secret_ids},
        )

    def get_bindings(self, workspace_id: str) -> dict[str, Any]:
        return self._c._request("GET", f"/workspaces/{workspace_id}/bindings")

    def reload_secrets(self, workspace_id: str) -> dict[str, Any]:
        return self._c._request("POST", f"/workspaces/{workspace_id}/reload-secrets")

    def set_model(self, workspace_id: str, model: str) -> None:
        self._c._request(
            "PUT", f"/workspaces/{workspace_id}/model", json={"model": model}
        )

    def get_models(self, workspace_id: str) -> dict[str, Any]:
        return self._c._request("GET", f"/workspaces/{workspace_id}/models")

    def set_env(self, workspace_id: str, vars: dict[str, str]) -> None:
        self._c._request("PUT", f"/workspaces/{workspace_id}/env", json={"vars": vars})

    def get_env(self, workspace_id: str) -> dict[str, Any]:
        return self._c._request("GET", f"/workspaces/{workspace_id}/env")

    def delete_env(self, workspace_id: str, var_name: str) -> None:
        self._c._request("DELETE", f"/workspaces/{workspace_id}/env/{var_name}")


class _SessionsAPI:
    def __init__(self, client: LLMSafeSpaces):
        self._c = client

    def ensure(self, workspace_id: str) -> EnsureSessionResponse:
        return EnsureSessionResponse(
            **self._c._request("POST", f"/workspaces/{workspace_id}/sessions/new")
        )

    def list(self, workspace_id: str) -> list[dict[str, Any]]:
        return self._c._request("GET", f"/workspaces/{workspace_id}/sessions")

    def send_message(
        self, workspace_id: str, session_id: str, content: str
    ) -> MessageResponse:
        raw = self._c._request(
            "POST",
            f"/workspaces/{workspace_id}/sessions/{session_id}/message",
            json={"content": content, "parts": [{"type": "text", "text": content}]},
        )
        text = _extract_text(raw)
        return MessageResponse(raw=raw, content=text)

    def get_history(self, workspace_id: str, session_id: str) -> list[Any]:
        return self._c._request(
            "GET", f"/workspaces/{workspace_id}/sessions/{session_id}/message"
        )

    def abort(self, workspace_id: str, session_id: str) -> None:
        self._c._request(
            "POST", f"/workspaces/{workspace_id}/sessions/{session_id}/abort"
        )

    def rename(self, workspace_id: str, session_id: str, title: str) -> None:
        self._c._request(
            "PUT",
            f"/workspaces/{workspace_id}/sessions/{session_id}/title",
            json={"title": title},
        )

    def get(self, workspace_id: str, session_id: str) -> dict[str, Any]:
        return self._c._request(
            "GET", f"/workspaces/{workspace_id}/sessions/{session_id}"
        )

    def get_active(self, workspace_id: str) -> dict[str, Any]:
        return self._c._request("GET", f"/workspaces/{workspace_id}/sessions/active")

    def send_prompt_async(
        self, workspace_id: str, session_id: str, message: str
    ) -> None:
        self._c._request(
            "POST",
            f"/workspaces/{workspace_id}/sessions/{session_id}/prompt",
            json={"message": message},
        )

    def delete(self, workspace_id: str, session_id: str) -> None:
        self._c._request(
            "DELETE",
            f"/workspaces/{workspace_id}/sessions/{session_id}",
        )

    def enqueue(self, workspace_id: str, session_id: str, text: str) -> str:
        resp = self._c._request(
            "POST",
            f"/workspaces/{workspace_id}/sessions/{session_id}/queue",
            json={"text": text},
        )
        return resp["messageID"]

    def list_queue(self, workspace_id: str, session_id: str) -> list[dict[str, Any]]:
        resp = self._c._request(
            "GET",
            f"/workspaces/{workspace_id}/sessions/{session_id}/queue",
        )
        return resp.get("messages", [])

    def dismiss_queued(self, workspace_id: str, session_id: str, message_id: str) -> None:
        self._c._request(
            "DELETE",
            f"/workspaces/{workspace_id}/sessions/{session_id}/queue/{message_id}",
        )

    def mark_seen(self, workspace_id: str, session_id: str) -> None:
        self._c._request(
            "PUT",
            f"/workspaces/{workspace_id}/sessions/{session_id}/seen",
        )


class _AuthAPI:
    def __init__(self, client: LLMSafeSpaces):
        self._c = client

    def me(self) -> dict[str, Any]:
        return self._c._request("GET", "/auth/me")

    def list_api_keys(self) -> list[APIKey]:
        data = self._c._request("GET", "/auth/api-keys")
        return [APIKey(**k) for k in data]

    def create_api_key(self, name: str) -> APIKey:
        return APIKey(**self._c._request("POST", "/auth/api-keys", json={"name": name}))

    def delete_api_key(self, key_id: str) -> None:
        self._c._request("DELETE", f"/auth/api-keys/{key_id}")


class _AccountAPI:
    def __init__(self, client: LLMSafeSpaces):
        self._c = client

    def rotate_key(self, password: str) -> dict[str, Any]:
        return self._c._request("POST", "/account/rotate-key", json={"password": password})

    def change_password(self, old_password: str, new_password: str) -> None:
        self._c._request(
            "POST",
            "/account/change-password",
            json={"oldPassword": old_password, "newPassword": new_password},
        )

    def recover(self, user_id: str, recovery_key: str, new_password: str) -> dict[str, Any]:
        return self._c._request(
            "POST",
            "/account/recover",
            json={"userId": user_id, "recoveryKey": recovery_key, "newPassword": new_password},
        )


class _SecretsAPI:
    def __init__(self, client: LLMSafeSpaces):
        self._c = client

    def create(
        self, *, name: str, type: str, value: str, metadata: Any = None
    ) -> SecretResponse:
        body: dict[str, Any] = {"name": name, "type": type, "value": value}
        if metadata is not None:
            body["metadata"] = metadata
        return SecretResponse(**self._c._request("POST", "/secrets", json=body))

    def list(self) -> list[SecretResponse]:
        # API returns {"secrets": [...]} wrapper
        data = self._c._request("GET", "/secrets")
        if isinstance(data, dict):
            items = data.get("secrets", [])
        else:
            items = data
        return [SecretResponse(**s) for s in items]

    def get(self, secret_id: str) -> SecretResponse:
        return SecretResponse(**self._c._request("GET", f"/secrets/{secret_id}"))

    def update(self, secret_id: str, value: str) -> None:
        self._c._request("PUT", f"/secrets/{secret_id}", json={"value": value})

    def delete(self, secret_id: str) -> None:
        self._c._request("DELETE", f"/secrets/{secret_id}")

    def reveal(self, secret_id: str, password: str = "") -> str:
        data = self._c._request(
            "POST", f"/secrets/{secret_id}/reveal", json={"password": password}
        )
        return data["value"]

    def get_audit_log(self) -> list[dict]:
        data = self._c._request("GET", "/secrets/audit")
        if isinstance(data, dict):
            return data.get("entries", [])
        return data

    def get_bindings_for_secret(self, secret_id: str) -> list[str]:
        data = self._c._request("GET", f"/secrets/{secret_id}/bindings")
        if isinstance(data, dict):
            return data.get("workspaces", [])
        return data


class _TerminalAPI:
    def __init__(self, client: LLMSafeSpaces):
        self._c = client

    def get_ticket(self, workspace_id: str) -> TerminalTicket:
        return TerminalTicket(
            **self._c._request("POST", f"/workspaces/{workspace_id}/terminal/ticket")
        )


def _extract_text(raw: Any) -> str:
    """Extract text content from opencode response parts."""
    if not isinstance(raw, dict):
        return ""
    parts = raw.get("parts", [])
    if not isinstance(parts, list):
        return ""
    return "".join(
        p.get("text", "")
        for p in parts
        if isinstance(p, dict) and p.get("type") == "text"
    )


class _UserSettingsAPI:
    def __init__(self, client: LLMSafeSpaces):
        self._c = client

    def get(self) -> dict[str, Any]:
        return self._c._request("GET", "/users/me/settings")

    def get_schema(self) -> dict[str, Any]:
        return self._c._request("GET", "/users/me/settings/schema")

    def set(self, key: str, value: Any) -> dict[str, Any]:
        return self._c._request(
            "PUT", f"/users/me/settings/{key}", json={"value": value}
        )


class _ProviderCredentialsAPI:
    def __init__(self, client: LLMSafeSpaces):
        self._c = client

    def create(
        self,
        *,
        name: str,
        kind: str,
        slug: str,
        api_key: str,
        base_url: str = "",
    ) -> ProviderCredential:
        body: dict[str, Any] = {
            "name": name,
            "kind": kind,
            "slug": slug,
            "apiKey": api_key,
        }
        if base_url:
            body["baseURL"] = base_url
        return ProviderCredential(
            **self._c._request("POST", "/provider-credentials", json=body)
        )

    def list(self) -> list[ProviderCredential]:
        data = self._c._request("GET", "/provider-credentials")
        return [ProviderCredential(**c) for c in data]

    def get(self, cred_id: str) -> ProviderCredential:
        return ProviderCredential(
            **self._c._request("GET", f"/provider-credentials/{cred_id}")
        )

    def delete(self, cred_id: str) -> None:
        self._c._request("DELETE", f"/provider-credentials/{cred_id}")

    def probe_models(self, cred_id: str) -> dict[str, Any]:
        return self._c._request(
            "GET", f"/provider-credentials/{cred_id}/models"
        )

    def list_bindings(self, cred_id: str) -> list[str]:
        return self._c._request(
            "GET", f"/provider-credentials/{cred_id}/bindings"
        )

    def bind(self, cred_id: str, workspace_id: str) -> dict[str, Any]:
        return self._c._request(
            "POST", f"/provider-credentials/{cred_id}/bind/{workspace_id}"
        )

    def unbind(self, cred_id: str, workspace_id: str) -> None:
        self._c._request(
            "DELETE", f"/provider-credentials/{cred_id}/bind/{workspace_id}"
        )


class _AdminProviderCredentialsAPI:
    def __init__(self, client: LLMSafeSpaces):
        self._c = client

    def create(
        self,
        *,
        name: str,
        kind: str,
        slug: str,
        api_key: str,
        base_url: str = "",
    ) -> ProviderCredential:
        body: dict[str, Any] = {
            "name": name,
            "kind": kind,
            "slug": slug,
            "apiKey": api_key,
        }
        if base_url:
            body["baseURL"] = base_url
        return ProviderCredential(
            **self._c._request("POST", "/admin/provider-credentials", json=body)
        )

    def list(self) -> list[ProviderCredential]:
        data = self._c._request("GET", "/admin/provider-credentials")
        return [ProviderCredential(**c) for c in data]

    def get(self, cred_id: str) -> ProviderCredential:
        return ProviderCredential(
            **self._c._request("GET", f"/admin/provider-credentials/{cred_id}")
        )

    def update(self, cred_id: str, **kwargs: Any) -> ProviderCredential:
        return ProviderCredential(
            **self._c._request(
                "PUT", f"/admin/provider-credentials/{cred_id}", json=kwargs
            )
        )

    def delete(self, cred_id: str) -> None:
        self._c._request("DELETE", f"/admin/provider-credentials/{cred_id}")

    def probe_models(self, cred_id: str) -> dict[str, Any]:
        return self._c._request(
            "GET", f"/admin/provider-credentials/{cred_id}/models"
        )

    def create_auto_apply(
        self, cred_id: str, *, target_type: str, target_id: str = ""
    ) -> dict[str, Any]:
        body: dict[str, Any] = {"targetType": target_type}
        if target_id:
            body["targetId"] = target_id
        return self._c._request(
            "POST", f"/admin/provider-credentials/{cred_id}/auto-apply", json=body
        )

    def list_auto_apply(self, cred_id: str) -> list[dict[str, Any]]:
        return self._c._request(
            "GET", f"/admin/provider-credentials/{cred_id}/auto-apply"
        )

    def delete_auto_apply(
        self, cred_id: str, target_type: str, target_id: str
    ) -> None:
        self._c._request(
            "DELETE",
            f"/admin/provider-credentials/{cred_id}/auto-apply/{target_type}/{target_id}",
        )


class _PromptsAPI:
    def __init__(self, client: LLMSafeSpaces):
        self._c = client

    def get_platform(self) -> dict[str, Any]:
        return self._c._request("GET", "/admin/prompt")

    def set_platform(self, prompt: str) -> None:
        self._c._request("PUT", "/admin/prompt", json={"prompt": prompt})

    def get_org(self, org_id: str) -> dict[str, Any]:
        return self._c._request("GET", f"/orgs/{org_id}/prompt")

    def set_org(self, org_id: str, prompt: str | None = None, allow_user_prompt: bool | None = None) -> None:
        body: dict[str, Any] = {}
        if prompt is not None:
            body["prompt"] = prompt
        if allow_user_prompt is not None:
            body["allowUserPrompt"] = allow_user_prompt
        self._c._request("PUT", f"/orgs/{org_id}/prompt", json=body)

    def get_workspace(self, workspace_id: str) -> dict[str, Any]:
        return self._c._request("GET", f"/workspaces/{workspace_id}/prompt")

    def set_workspace(self, workspace_id: str, prompt: str) -> None:
        self._c._request("PUT", f"/workspaces/{workspace_id}/prompt", json={"prompt": prompt})


class _AgentRolesAPI:
    def __init__(self, client: LLMSafeSpaces):
        self._c = client

    # Platform roles
    def list_platform(self) -> list[dict[str, Any]]:
        data = self._c._request("GET", "/admin/agent-roles")
        return data if isinstance(data, list) else data.get("items", [])

    def create_platform(self, **kwargs: Any) -> dict[str, Any]:
        return self._c._request("POST", "/admin/agent-roles", json=kwargs)

    def get_platform(self, role_id: str) -> dict[str, Any]:
        return self._c._request("GET", f"/admin/agent-roles/{role_id}")

    def update_platform(self, role_id: str, **kwargs: Any) -> dict[str, Any]:
        return self._c._request("PUT", f"/admin/agent-roles/{role_id}", json=kwargs)

    def delete_platform(self, role_id: str) -> None:
        self._c._request("DELETE", f"/admin/agent-roles/{role_id}")

    # Org roles
    def list_org(self, org_id: str) -> list[dict[str, Any]]:
        data = self._c._request("GET", f"/orgs/{org_id}/agent-roles")
        return data if isinstance(data, list) else data.get("items", [])

    def create_org(self, org_id: str, **kwargs: Any) -> dict[str, Any]:
        return self._c._request("POST", f"/orgs/{org_id}/agent-roles", json=kwargs)

    def get_org(self, org_id: str, role_id: str) -> dict[str, Any]:
        return self._c._request("GET", f"/orgs/{org_id}/agent-roles/{role_id}")

    def update_org(self, org_id: str, role_id: str, **kwargs: Any) -> dict[str, Any]:
        return self._c._request("PUT", f"/orgs/{org_id}/agent-roles/{role_id}", json=kwargs)

    def delete_org(self, org_id: str, role_id: str) -> None:
        self._c._request("DELETE", f"/orgs/{org_id}/agent-roles/{role_id}")

    # Workspace role selection
    def get_workspace_role(self, workspace_id: str) -> dict[str, Any] | None:
        return self._c._request("GET", f"/workspaces/{workspace_id}/agent-role")

    def set_workspace_role(self, workspace_id: str, role_id: str) -> None:
        self._c._request("PUT", f"/workspaces/{workspace_id}/agent-role", json={"roleId": role_id})

    def get_effective_workspace_role(self, workspace_id: str) -> dict[str, Any]:
        return self._c._request("GET", f"/workspaces/{workspace_id}/effective-agent-role")
