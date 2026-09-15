"""Shared wire fixtures for the session-contract rows (#1304 / 4b-c2)."""

SESSION_ROW = {
    "id": "ses_7f9d",
    "workspaceId": "ws-1",
    "parentId": "ses_root",
    "title": "Refactor the adapter",
    "agentId": "plan",
    "model": {"id": "claude-sonnet-4.5", "provider": "anthropic"},
    "status": "busy",
    "cost": {"inputTokens": 120, "outputTokens": 80, "totalTokens": 200},
    "contextUsage": {"used": 45000, "window": 200000},
    "time": {"startedAt": "2026-09-15T10:00:00Z", "completedAt": None},
    "summary": "Wire the seam",
    "archived": False,
}

# 202 body of POST .../prompt (the delivery outbox accepted entry).
PROMPT_QUEUED_BODY = {
    "messageID": "msg_9",
    "clientMessageID": "cmid-1",
    "status": "queued",
}

# 200 body of a retried clientMessageID (idempotent accept returns the
# ORIGINAL accepted entry).
PROMPT_DUPLICATE_BODY = {
    "messageID": "msg_orig",
    "clientMessageID": "cmid-1",
    "status": "duplicate",
}
