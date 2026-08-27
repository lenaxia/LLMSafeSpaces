/** Base error for all LLMSafeSpaces API errors. */
export class LLMSafeSpacesError extends Error {
  constructor(
    message: string,
    public readonly status: number,
    public readonly code?: string,
  ) {
    super(message);
    this.name = "LLMSafeSpacesError";
  }
}

export class AuthError extends LLMSafeSpacesError {
  constructor(message: string, status: number = 401) {
    super(message, status, "AUTH_ERROR");
    this.name = "AuthError";
  }
}

export class NotFoundError extends LLMSafeSpacesError {
  constructor(message: string) {
    super(message, 404, "NOT_FOUND");
    this.name = "NotFoundError";
  }
}

export class ConflictError extends LLMSafeSpacesError {
  /** Current workspace phase, when the 409 body carries one (upload phase gate, Epic 67 D5). */
  public phase?: string;

  constructor(message: string) {
    super(message, 409, "CONFLICT");
    this.name = "ConflictError";
  }
}

export class TimeoutError extends LLMSafeSpacesError {
  constructor(message: string = "Request timed out — the prompt may still be processing") {
    super(message, 0, "TIMEOUT");
    this.name = "TimeoutError";
  }
}

export class RateLimitError extends LLMSafeSpacesError {
  constructor(message: string = "Rate limit exceeded") {
    super(message, 429, "RATE_LIMIT");
    this.name = "RateLimitError";
  }
}

export class ServiceUnavailableError extends LLMSafeSpacesError {
  /**
   * The workspace exists but cannot service requests (503). The `reason`
   * field distinguishes the cause:
   * - "not_ready" — workspace is booting or resuming
   * - "agent_unreachable" — the agent process hung or crashed
   * - "agent_restarting" — the agent is being restarted by the health
   *   watchdog or a credential reload
   *
   * Retry after `retryAfter` seconds (defaults to 10).
   */
  public readonly reason?: string;
  public readonly retryAfter?: number;

  constructor(
    message: string = "Service temporarily unavailable",
    reason?: string,
    retryAfter?: number,
  ) {
    super(message, 503, "SERVICE_UNAVAILABLE");
    this.name = "ServiceUnavailableError";
    this.reason = reason;
    this.retryAfter = retryAfter;
  }
}
