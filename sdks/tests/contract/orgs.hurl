# Org-core CRUD (#1088): happy paths + documented error contracts

# List (bare array)
GET {{base_url}}/orgs
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$" exists

# Get (member scope) — documented 403 for non-members
GET {{base_url}}/orgs/org-1
Authorization: Bearer {{token}}
Prefer: code=403
HTTP 403
[Asserts]
jsonpath "$.error" isString

# Create — validation-failure contract (documented 400)
POST {{base_url}}/orgs
Authorization: Bearer {{token}}
Content-Type: application/json
Prefer: code=400
{
  "name": "x",
  "slug": "bad__slug!!",
  "ownerEmail": "not-an-email"
}
HTTP 400
[Asserts]
jsonpath "$.error" isString

# Update — slug-conflict contract (documented 409)
PUT {{base_url}}/orgs/org-1
Authorization: Bearer {{token}}
Content-Type: application/json
Prefer: code=409
{
  "slug": "taken-slug"
}
HTTP 409
[Asserts]
jsonpath "$.error" isString

# Delete — 204 contract via Prefer is not status-selectable on this mock;
# the router-level presence is covered by TestOpenAPIRouterContract.

# Members list (bare array)
GET {{base_url}}/orgs/org-1/members
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$" exists

# Workspaces list (items + pagination wrapper)
GET {{base_url}}/orgs/org-1/workspaces?limit=20&offset=0
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.items" exists
jsonpath "$.pagination" exists

# Billing checkout — not-configured contract (documented 503)
POST {{base_url}}/orgs/org-1/billing/checkout
Authorization: Bearer {{token}}
Content-Type: application/json
Prefer: code=503
{
  "planId": "team"
}
HTTP 503
[Asserts]
jsonpath "$.error" isString
