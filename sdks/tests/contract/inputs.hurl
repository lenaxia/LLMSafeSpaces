# Input requests (questions/permissions): verify the #1302 contract
# surface against the mock — typed InputRequest lists, the reply
# vocabulary, the late-answer 202 body, the inbox dismiss exit, and the
# documented 409/503 rows. Prism serves spec examples; value-level
# behavior is canary territory.

# Create a workspace for this test run
POST {{base_url}}/workspaces
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "name": "contract-inputs-ws",
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

# List pending questions — the contract InputRequest array (kind=question)
GET {{base_url}}/workspaces/{{workspace_id}}/question
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$" isCollection
jsonpath "$[0].id" exists
jsonpath "$[0].kind" exists
jsonpath "$[0].sessionId" exists

# List pending permissions — the contract InputRequest array (kind=permission)
GET {{base_url}}/workspaces/{{workspace_id}}/permission
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$" isCollection
jsonpath "$[0].id" exists
jsonpath "$[0].kind" exists

# Reply to a question — live answer (200, no body)
POST {{base_url}}/workspaces/{{workspace_id}}/question/que_contract123/reply
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "answers": [["Go"]]
}
HTTP 200

# Reply to a question — late answer through the delivery outbox (#1313)
POST {{base_url}}/workspaces/{{workspace_id}}/question/que_contract123/reply
Authorization: Bearer {{token}}
Content-Type: application/json
Prefer: code=202
{
  "answers": [["Go"]]
}
HTTP 202
[Asserts]
jsonpath "$.status" isString
jsonpath "$.clientMessageID" isString
jsonpath "$.messageID" isString

# Reply to a question — the ask's record was dismissed (409 row)
POST {{base_url}}/workspaces/{{workspace_id}}/question/que_contract123/reply
Authorization: Bearer {{token}}
Content-Type: application/json
Prefer: code=409
{
  "answers": [["Go"]]
}
HTTP 409

# Reject (dismiss) a question
POST {{base_url}}/workspaces/{{workspace_id}}/question/que_contract123/reject
Authorization: Bearer {{token}}
HTTP 200

# Reply to a permission — the contract vocabulary
POST {{base_url}}/workspaces/{{workspace_id}}/permission/per_contract123/reply
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "reply": "once"
}
HTTP 200

# Reply to a permission — late decision (202, InboxLateAnswerAccepted)
POST {{base_url}}/workspaces/{{workspace_id}}/permission/per_contract123/reply
Authorization: Bearer {{token}}
Content-Type: application/json
Prefer: code=202
{
  "reply": "always"
}
HTTP 202
[Asserts]
jsonpath "$.status" isString

# Reply to a permission — late-answer delivery unavailable (503 row)
POST {{base_url}}/workspaces/{{workspace_id}}/permission/per_contract123/reply
Authorization: Bearer {{token}}
Content-Type: application/json
Prefer: code=503
{
  "reply": "reject"
}
HTTP 503

# Dismiss an unanswered-question inbox record (#1313 dismiss exit)
DELETE {{base_url}}/workspaces/{{workspace_id}}/sessions/{{session_id}}/inbox/que_contract123
Authorization: Bearer {{token}}
HTTP 204

# Request an input-snapshot flight (202)
POST {{base_url}}/workspaces/{{workspace_id}}/input-snapshot
Authorization: Bearer {{token}}
HTTP 202
