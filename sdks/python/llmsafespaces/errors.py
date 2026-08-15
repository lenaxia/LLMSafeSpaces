"""Typed error hierarchy for LLMSafeSpaces API errors."""


class LLMSafeSpacesError(Exception):
    """Base error for all API errors."""

    def __init__(self, message: str, status: int = 0, code: str | None = None):
        super().__init__(message)
        self.status = status
        self.code = code


class AuthError(LLMSafeSpacesError):
    """Authentication or authorization failure (401/403)."""

    def __init__(self, message: str = "Authentication required", status: int = 401):
        super().__init__(message, status, "AUTH_ERROR")


class NotFoundError(LLMSafeSpacesError):
    """Resource not found (404)."""

    def __init__(self, message: str = "Resource not found"):
        super().__init__(message, 404, "NOT_FOUND")


class ConflictError(LLMSafeSpacesError):
    """Conflict state (409)."""

    def __init__(self, message: str = "Conflict"):
        super().__init__(message, 409, "CONFLICT")


class TimeoutError(LLMSafeSpacesError):
    """Request timed out — prompt may still be processing."""

    def __init__(self, message: str = "Request timed out"):
        super().__init__(message, 0, "TIMEOUT")


class RateLimitError(LLMSafeSpacesError):
    """Rate limit exceeded (429)."""

    def __init__(self, message: str = "Rate limit exceeded"):
        super().__init__(message, 429, "RATE_LIMIT")


class ServiceUnavailableError(LLMSafeSpacesError):
    """The workspace exists but cannot service requests (503).

    The ``reason`` attribute distinguishes the cause:
    - ``"not_ready"`` — workspace is booting or resuming
    - ``"agent_unreachable"`` — the agent process hung or crashed
    - ``"agent_restarting"`` — the agent is being restarted by the health
      watchdog or a credential reload

    Retry after ``retry_after`` seconds (defaults to 10).
    """

    def __init__(
        self,
        message: str = "Service temporarily unavailable",
        reason: str | None = None,
        retry_after: int = 10,
    ):
        super().__init__(message, 503, "SERVICE_UNAVAILABLE")
        self.reason = reason
        self.retry_after = retry_after
