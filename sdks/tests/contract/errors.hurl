# Error responses: verify the documented error contracts against the mock.
#
# Prism always serves the happy-path response for a request that matches a
# path — the `Prefer: code=N` header selects a specific documented response.
# This validates that each error response EXISTS in the spec and matches the
# Error schema shape. (Live-API error behavior is covered by the canaries.)

# 401 - No auth header (Prism enforces security schemes)
GET {{base_url}}/auth/me
HTTP 401
[Asserts]
jsonpath "$.error" isString

# 404 - Workspace not found (documented response)
GET {{base_url}}/workspaces/nonexistent-id-12345
Authorization: Bearer {{token}}
Prefer: code=404
HTTP 404
[Asserts]
jsonpath "$.error" isString

# 401 - Login with wrong credentials (documented response)
POST {{base_url}}/auth/login
Content-Type: application/json
Prefer: code=401
{
  "email": "x@example.com",
  "password": "wrong"
}
HTTP 401
[Asserts]
jsonpath "$.error" isString

