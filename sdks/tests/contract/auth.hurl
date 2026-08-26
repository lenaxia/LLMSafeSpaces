# Auth: Register + Login + API Key CRUD
# Run against: http://localhost:8080 (or mock server)

# Register
POST {{base_url}}/auth/register
Content-Type: application/json
{
  "username": "testuser",
  "email": "test@example.com",
  "password": "securepass123"
}
HTTP 201
[Asserts]
jsonpath "$.token" isString
jsonpath "$.user.id" isString
jsonpath "$.user.email" isString

# Login
POST {{base_url}}/auth/login
Content-Type: application/json
{
  "email": "test@example.com",
  "password": "securepass123"
}
HTTP 200
[Asserts]
jsonpath "$.token" isString
[Captures]
token: jsonpath "$.token"

# Get current user
GET {{base_url}}/auth/me
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.id" isString
jsonpath "$.username" isString

# Create API key
POST {{base_url}}/auth/api-keys
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "name": "test-key"
}
HTTP 201
[Asserts]
jsonpath "$.id" isString
jsonpath "$.key" isString
jsonpath "$.name" isString
[Captures]
api_key_id: jsonpath "$.id"

# List API keys
GET {{base_url}}/auth/api-keys
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$" count >= 1

# Delete API key
DELETE {{base_url}}/auth/api-keys/{{api_key_id}}
Authorization: Bearer {{token}}
HTTP 204
