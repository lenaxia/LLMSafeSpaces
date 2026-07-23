"""Typed models for LLMSafeSpaces API."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class Workspace:
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
    maxActiveSessions: int | None = None


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
class MessageResponse:
    raw: Any
    content: str


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


@dataclass
class TerminalTicket:
    ticket: str
    expiresAt: str


# Regex pattern for valid secret names. Keep in sync with pkg/validation/name.go.
SECRET_NAME_PATTERN = r"^[a-z0-9._-]+$"


@dataclass
class SecretResponse:
    id: str
    name: str
    type: str
    createdAt: str
    updatedAt: str
    metadata: Any = None


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
class UserSettings:
    settings: dict[str, Any]
    schemaVersion: int
