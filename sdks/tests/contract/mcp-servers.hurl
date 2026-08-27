# MCP servers (Epic 53): user-scope CRUD + auto-apply (#1046)

# Create
POST {{base_url}}/me/mcp-servers
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "name": "contract-test-mcp",
  "transport": "http",
  "url": "https://mcp.example.com/mcp"
}
HTTP 201
[Asserts]
jsonpath "$.id" isString
jsonpath "$.transport" == "http"
[Captures]
mcp_id: jsonpath "$.id"

# List
GET {{base_url}}/me/mcp-servers
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.servers" exists

# Get
GET {{base_url}}/me/mcp-servers/{{mcp_id}}
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.id" == "{{mcp_id}}"
jsonpath "$.hasSecret" isBoolean

# Partial update (rename)
PUT {{base_url}}/me/mcp-servers/{{mcp_id}}
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "name": "renamed-mcp"
}
HTTP 200
[Asserts]
jsonpath "$.id" == "{{mcp_id}}"

# Bind (workspaceId body)
POST {{base_url}}/me/mcp-servers/{{mcp_id}}/bindings
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "workspaceId": "ws-1"
}
HTTP 200

# Auto-apply create + list
POST {{base_url}}/me/mcp-servers/{{mcp_id}}/auto-apply
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "targetType": "user"
}
HTTP 201

GET {{base_url}}/me/mcp-servers/{{mcp_id}}/auto-apply
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.rules" exists

# Delete
DELETE {{base_url}}/me/mcp-servers/{{mcp_id}}
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.deleted" isBoolean

# Error contract: documented 409 (quota exceeded) response shape
POST {{base_url}}/me/mcp-servers
Authorization: Bearer {{token}}
Content-Type: application/json
Prefer: code=409
{
  "name": "x",
  "transport": "http"
}
HTTP 409
[Asserts]
jsonpath "$.error" isString
