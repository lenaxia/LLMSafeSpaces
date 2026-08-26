# Workspaces: CRUD + lifecycle operations

# Create workspace
POST {{base_url}}/workspaces
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "name": "contract-test-ws",
  "runtime": "python:3.11",
  "storageSize": "5Gi"
}
HTTP 201
[Asserts]
jsonpath "$.id" isString
jsonpath "$.name" isString
jsonpath "$.runtime" isString
[Captures]
workspace_id: jsonpath "$.id"

# List workspaces
GET {{base_url}}/workspaces?limit=10&offset=0
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.items" count >= 1

# Get workspace
GET {{base_url}}/workspaces/{{workspace_id}}
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.id" == "{{workspace_id}}"
jsonpath "$.name" isString

# Rename workspace
PUT {{base_url}}/workspaces/{{workspace_id}}
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "name": "renamed-ws"
}
HTTP 204

# Get workspace status
GET {{base_url}}/workspaces/{{workspace_id}}/status
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.phase" isString

# Suspend then re-activate workspace
POST {{base_url}}/workspaces/{{workspace_id}}/suspend
Authorization: Bearer {{token}}
HTTP 202

POST {{base_url}}/workspaces/{{workspace_id}}/activate
Authorization: Bearer {{token}}
HTTP 200

# Delete workspace
DELETE {{base_url}}/workspaces/{{workspace_id}}
Authorization: Bearer {{token}}
HTTP 204
