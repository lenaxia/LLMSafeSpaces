# Session queue: enqueue, list, dismiss
#
# Self-contained: creates its own workspace at the top.

# Create a workspace for this test run
POST {{base_url}}/api/v1/workspaces
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "name": "contract-queue-ws",
  "runtime": "python:3.11",
  "storageSize": "5Gi"
}
HTTP 201
[Captures]
workspace_id: jsonpath "$.id"

# Ensure a session
POST {{base_url}}/api/v1/workspaces/{{workspace_id}}/sessions/new
Authorization: Bearer {{token}}
HTTP 200
[Captures]
session_id: jsonpath "$.sessionId"

# Enqueue a message
POST {{base_url}}/api/v1/workspaces/{{workspace_id}}/sessions/{{session_id}}/queue
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "text": "queued message 1"
}
HTTP 202
[Asserts]
jsonpath "$.messageID" isString
[Captures]
message_id: jsonpath "$.messageID"

# List queued messages — should contain the one we just enqueued
GET {{base_url}}/api/v1/workspaces/{{workspace_id}}/sessions/{{session_id}}/queue
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.messages" isCollection
jsonpath "$.messages[0].id" == "{{message_id}}"

# Dismiss the queued message
DELETE {{base_url}}/api/v1/workspaces/{{workspace_id}}/sessions/{{session_id}}/queue/{{message_id}}
Authorization: Bearer {{token}}
HTTP 204

# List again — should be empty (or not contain the dismissed message)
GET {{base_url}}/api/v1/workspaces/{{workspace_id}}/sessions/{{session_id}}/queue
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.messages[0]" notExists

# Error: enqueue with empty text
POST {{base_url}}/api/v1/workspaces/{{workspace_id}}/sessions/{{session_id}}/queue
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "text": ""
}
HTTP 400
