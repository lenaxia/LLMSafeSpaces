# Passkeys (Epic 59): ceremony shapes + error responses (#1041)

# Register begin
POST {{base_url}}/auth/passkey/register/begin
Content-Type: application/json
{
  "email": "new@example.com"
}
HTTP 200
[Asserts]
jsonpath "$.options" exists
jsonpath "$.sessionToken" isString

# Register begin with invalid email (documented 400)
POST {{base_url}}/auth/passkey/register/begin
Content-Type: application/json
Prefer: code=400
{
  "email": "not-an-email"
}
HTTP 400
[Asserts]
jsonpath "$.error" isString

# Recover with an invalid code (documented 401)
POST {{base_url}}/auth/passkey/recover
Content-Type: application/json
Prefer: code=401
{
  "email": "x@example.com",
  "code": "deadbeef"
}
HTTP 401
[Asserts]
jsonpath "$.error" isString

# List account passkeys (auth required)
GET {{base_url}}/account/passkeys
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.passkeys" exists
