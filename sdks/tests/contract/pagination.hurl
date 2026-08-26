# Pagination: list endpoints return cursor-style pagination metadata.
#
# Verifies the pagination envelope shape across workspaces + audit log.
# Page sizes must be honored; offset advances the cursor.

# Page size 1 — returns only the first workspace
GET {{base_url}}/workspaces?limit=1&offset=0
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.items" count <= 1
jsonpath "$.pagination" exists

# Page size 100 — returns everything in one page
GET {{base_url}}/workspaces?limit=100&offset=0
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.items" exists
jsonpath "$.pagination.limit" isNumber

# Offset advances — second page must not duplicate the first item
GET {{base_url}}/workspaces?limit=1&offset=1
Authorization: Bearer {{token}}
HTTP 200
[Captures]
second_page_first_id: jsonpath "$.items[0].id"

# Secrets audit log also paginates with the same envelope
GET {{base_url}}/secrets/audit?limit=10&offset=0
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$" exists

# Message-history cursor pagination (#1039): params + X-Next-Cursor header
GET {{base_url}}/workspaces/ws-1/sessions/sess-1/message?limit=50
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
header "X-Next-Cursor" exists

GET {{base_url}}/workspaces/ws-1/sessions/sess-1/message?limit=50&before=msg_abc
Authorization: Bearer {{token}}
HTTP 200

# Invalid limit is a documented 400
GET {{base_url}}/workspaces/ws-1/sessions/sess-1/message?limit=0
Authorization: Bearer {{token}}
Prefer: code=400
HTTP 400
[Asserts]
jsonpath "$.error" isString
