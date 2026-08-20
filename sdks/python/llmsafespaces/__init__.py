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
    ServiceUnavailableError,
    TimeoutError,
)
from .types import (
    APIKey,
    AuthResponse,
    CreateAgentRoleRequest,
    EnsureSessionResponse,
    Message,
    Part,
    ToolPart,
    FileDiff,
    InputRequest,
    InputOption,
    ToolRef,
    ToolState,
    ModelRef,
    Cost,
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
    "ServiceUnavailableError",
    "Workspace",
    "WorkspaceListItem",
    "WorkspaceListResult",
    "EnsureSessionResponse",
    "Message",
    "Part",
    "ToolPart",
    "FileDiff",
    "ToolState",
    "ModelRef",
    "Cost",
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
