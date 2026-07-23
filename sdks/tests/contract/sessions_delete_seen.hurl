# Session delete and mark-seen
#
# Self-contained: creates its own workspace at the top.

# Create a workspace for this test run
POST {{base_url}}/api/v1/workspaces
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "name": "contract-delete-seen-ws",
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

# Mark the session as seen
PUT {{base_url}}/api/v1/workspaces/{{workspace_id}}/sessions/{{session_id}}/seen
Authorization: Bearer {{token}}
HTTP 204

# Delete the session
DELETE {{base_url}}/api/v1/workspaces/{{workspace_id}}/sessions/{{session_id}}
Authorization: Bearer {{token}}
HTTP 200
