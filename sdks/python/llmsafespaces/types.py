"""Typed models for LLMSafeSpaces API."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, TypedDict


@dataclass
class Workspace:
    # Mirrors the API transfer object (pkg/types/workspace.go Workspace):
    # id, name, userId, runtime, storageSize, phase, pvcName, labels,
    # defaultModel, createdAt, updatedAt, agentNeedsRefresh,
    # credentialsPendingSince, devPreviewEnabled. NOT WorkspaceMetadata —
    # the DB record also carries imageTag/agentVersion/orgId, which the
    # DTO does not emit.
    id: str
    name: str
    userId: str
    runtime: str
    storageSize: str
    phase: str
    createdAt: str
    updatedAt: str
    pvcName: str | None = None
    labels: dict[str, str] | None = None
    defaultModel: str | None = None
    agentNeedsRefresh: bool = False
    credentialsPendingSince: str | None = None
    devPreviewEnabled: bool = False


@dataclass
class WorkspaceListItem:
    id: str
    name: str
    userId: str
    runtime: str
    storageSize: str
    createdAt: str
    updatedAt: str
    phase: str | None = None
    imageTag: str | None = None
    agentVersion: str | None = None
    defaultModel: str | None = None
    maxActiveSessions: int | None = None
    agentNeedsRefresh: bool = False
    credentialsPendingSince: str | None = None
    orgId: str | None = None


@dataclass
class WorkspaceListResult:
    items: list[WorkspaceListItem] = field(default_factory=list)
    pagination: dict[str, Any] | None = None


@dataclass
class EnsureSessionResponse:
    workspaceId: str
    workspacePhase: str
    sessionId: str
    resumed: bool


@dataclass
class ToolState(TypedDict, total=False):
    status: str
    error: str
    startedAt: str
    completedAt: str


class ToolPart(TypedDict, total=False):
    """Every tool call is a ToolPart discriminated by name."""

    callId: str
    name: str
    input: Any
    output: Any
    state: ToolState


class FileDiff(TypedDict, total=False):
    """Unified-diff payload of a file-change part (patch text authoritative)."""

    path: str
    oldPath: str
    status: str
    patch: str
    additions: int
    deletions: int


class InputOption(TypedDict, total=False):
    label: str
    description: str


class ToolRef(TypedDict, total=False):
    """Identifies the tool call that triggered an InputRequest."""

    messageId: str
    callId: str


class InputRequest(TypedDict, total=False):
    """The unified pending-input shape ("the agent needs a human",
    design 0049 §4.5)."""

    id: str
    sessionId: str
    rootSessionId: str
    kind: str
    question: str
    header: str
    options: list[InputOption]
    multiple: bool
    custom: bool
    permission: str
    patterns: list[str]
    always: list[str]
    metadata: dict[str, Any]
    tool: ToolRef


class Part(TypedDict, total=False):
    """One renderable part of a message — the closed 5-type union."""

    type: str
    id: str
    text: str
    reasoning: str
    tool: ToolPart
    fileChange: FileDiff
    custom: dict[str, Any]


class ModelRef(TypedDict, total=False):
    id: str
    provider: str


class Cost(TypedDict, total=False):
    """Display-only token/cost data (never billing)."""

    inputTokens: int
    outputTokens: int
    reasoningTokens: int
    cacheReadTokens: int
    cacheWriteTokens: int
    totalTokens: int
    costUsd: float


class Message(TypedDict, total=False):
    """One entry in a session transcript (pkg/session contract).

    Flat discriminated struct: type selects which fields are meaningful.
    """

    id: str
    sessionId: str
    type: str
    createdAt: str
    parts: list[Part]
    model: ModelRef
    cost: Cost
    text: str
    command: str
    exitCode: int
    fromAgent: str
    toAgent: str
    error: dict[str, Any]


class HistoryPage(TypedDict):
    """One page of session history plus the cursor for the next (older) page.

    nextCursor is "" when the beginning of the session was reached
    (no X-Next-Cursor response header).
    """

    messages: list[Message]
    nextCursor: str


@dataclass
class AuthResponse:
    token: str
    user: dict[str, Any]


@dataclass
class APIKey:
    id: str
    name: str
    prefix: str
    active: bool
    createdAt: str
    key: str | None = None
    expiresAt: str | None = None
    decryptAccess: bool = False
    dekSynced: bool = False


@dataclass
class TerminalTicket:
    ticket: str
    expiresAt: str


# Regex pattern for valid secret names. Keep in sync with pkg/validation/name.go.
SECRET_NAME_PATTERN = r"^[a-z0-9._-]+$"


@dataclass
class SecretResponse:
    # Mirrors pkg/secrets/types.go SecretResponse.
    id: str
    name: str
    type: str
    createdAt: str
    updatedAt: str
    metadata: Any = None
    globalDefault: bool = False


@dataclass
class ProviderCredential:
    id: str
    name: str
    kind: str
    slug: str
    createdAt: str
    updatedAt: str
    baseURL: str | None = None
    modelAllowlist: list[str] | None = None
    modelContextLimits: dict[str, int] | None = None
    modelOutputLimits: dict[str, int] | None = None


@dataclass
class UpdateProviderCredentialRequest:
    name: str | None = None
    apiKey: str | None = None
    baseURL: str | None = None
    modelAllowlist: list[str] | None = None
    modelContextLimits: dict[str, int] | None = None
    modelOutputLimits: dict[str, int] | None = None


@dataclass
class CreateAgentRoleRequest:
    name: str
    slug: str
    description: str = ""
    extends: str | None = None
    isDefault: bool = False
    config: dict[str, Any] | None = None


@dataclass
class UpdateAgentRoleRequest:
    name: str | None = None
    slug: str | None = None
    description: str | None = None
    extends: str | None = None
    isDefault: bool | None = None
    config: dict[str, Any] | None = None


class McpServer(TypedDict, total=False):
    """External MCP server registration (Epic 53). Secrets (env/headers)
    are write-only — responses carry hasSecret only."""

    id: str
    name: str
    transport: str
    url: str
    command: str
    args: list[str]
    timeoutMs: int
    hasSecret: bool
    enabled: bool
    createdAt: str
    updatedAt: str


class CreateMcpServerRequest(TypedDict, total=False):
    name: str
    transport: str
    url: str
    command: str
    args: list[str]
    timeoutMs: int
    enabled: bool
    env: dict[str, str]
    headers: dict[str, str]
    autoApply: dict[str, str]


class UpdateMcpServerRequest(TypedDict, total=False):
    name: str
    url: str
    command: str
    args: list[str]
    timeoutMs: int
    enabled: bool
    env: dict[str, str]
    headers: dict[str, str]


class McpAutoApplyRule(TypedDict, total=False):
    targetType: str
    targetId: str
