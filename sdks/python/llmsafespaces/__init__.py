"""LLMSafeSpaces Python SDK."""

from importlib.metadata import PackageNotFoundError, version as _version

from .client import LLMSafeSpaces
from .async_client import AsyncLLMSafeSpaces
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
    UpdateProviderCredentialRequest,
    UpdateAgentRoleRequest,
    Workspace,
    WorkspaceListItem,
    WorkspaceListResult,
)

try:
    __version__ = _version("llmsafespaces")
except PackageNotFoundError:
    __version__ = "dev"

__all__ = [
    "LLMSafeSpaces",
    "AsyncLLMSafeSpaces",
    "LLMSafeSpacesError",
    "AuthError",
    "NotFoundError",
    "ConflictError",
    "TimeoutError",
    "RateLimitError",
    "Workspace",
    "WorkspaceListItem",
    "WorkspaceListResult",
    "EnsureSessionResponse",
    "MessageResponse",
    "AuthResponse",
    "APIKey",
    "TerminalTicket",
    "SecretResponse",
    "ProviderCredential",
    "UpdateProviderCredentialRequest",
    "CreateAgentRoleRequest",
    "UpdateAgentRoleRequest",
    "__version__",
]
