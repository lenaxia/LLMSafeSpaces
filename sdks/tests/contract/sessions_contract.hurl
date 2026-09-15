# Session surface contract rows (#1304 / 4b-c2): the typed Session on
# getSession, the accepted-prompt receipt body, and the bodyless 204s on
# abort/delete — pinned to what the live handlers emit
# (api/internal/handlers/proxy_handlers.go). Prism serves spec examples;
# value-level behavior is canary territory.

# Create a workspace for this test run
POST {{base_url}}/workspaces
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "name": "contract-session-ws",
  "runtime": "python:3.11",
  "storageSize": "5Gi"
}
HTTP 201
[Captures]
workspace_id: jsonpath "$.id"

# Ensure a session
POST {{base_url}}/workspaces/{{workspace_id}}/sessions/new
Authorization: Bearer {{token}}
HTTP 200
[Captures]
session_id: jsonpath "$.sessionId"

# Get a session — the contract Session (pkg/session), not a raw agent object
GET {{base_url}}/workspaces/{{workspace_id}}/sessions/{{session_id}}
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.id" exists
jsonpath "$.workspaceId" exists
jsonpath "$.status" exists

# Send a message — the parts body is what the server extracts text from
POST {{base_url}}/workspaces/{{workspace_id}}/sessions/{{session_id}}/message
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "content": "hello",
  "parts": [{"type": "text", "text": "hello"}]
}
HTTP 200
[Asserts]
jsonpath "$.id" exists
jsonpath "$.type" exists

# Async prompt — 202 carries the accepted receipt body (delivery outbox)
POST {{base_url}}/workspaces/{{workspace_id}}/sessions/{{session_id}}/prompt
Authorization: Bearer {{token}}
Content-Type: application/json
Prefer: code=202
{
  "parts": [{"type": "text", "text": "hello"}]
}
HTTP 202
[Asserts]
jsonpath "$.messageID" isString
jsonpath "$.status" isString

# Async prompt retried clientMessageID — 200 duplicate receipt (idempotent accept)
POST {{base_url}}/workspaces/{{workspace_id}}/sessions/{{session_id}}/prompt
Authorization: Bearer {{token}}
Content-Type: application/json
Prefer: code=200
{
  "parts": [{"type": "text", "text": "hello"}],
  "clientMessageID": "cmid-contract-1"
}
HTTP 200
[Asserts]
jsonpath "$.messageID" isString
jsonpath "$.status" isString

# Abort the current turn — bodyless 204
POST {{base_url}}/workspaces/{{workspace_id}}/sessions/{{session_id}}/abort
Authorization: Bearer {{token}}
HTTP 204

# Delete the session — bodyless 204
DELETE {{base_url}}/workspaces/{{workspace_id}}/sessions/{{session_id}}
Authorization: Bearer {{token}}
HTTP 204

# Cleanup the workspace
DELETE {{base_url}}/workspaces/{{workspace_id}}
Authorization: Bearer {{token}}
HTTP 204
