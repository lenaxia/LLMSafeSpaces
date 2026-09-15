"""Shared wire fixtures for the input-request contract rows (#1302 / 4b)."""

QUESTION_ROW = {
    "id": "que_1",
    "sessionId": "ses_1",
    "rootSessionId": "ses_root",
    "kind": "question",
    "question": "What language?",
    "header": "Choose language",
    "options": [{"label": "Go", "description": "Fast"}, {"label": "Python"}],
    "multiple": False,
    "custom": True,
    "tool": {"messageId": "msg_abc", "callId": "call_xyz"},
}

PERMISSION_ROW = {
    "id": "per_1",
    "kind": "permission",
    "permission": "bash",
    "patterns": ["/workspace/src/main.go"],
    "always": ["/workspace/*"],
    "metadata": {"command": "go build"},
}

LATE_ANSWER_BODY = {
    "status": "queued",
    "clientMessageID": "inbox-que_1-answer",
    "messageID": "msg_out_1",
    "duplicate": True,
}
