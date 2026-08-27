# Epic 64 workflows + triggers: user-scope CRUD against the spec (#1040)

# Create workflow
POST {{base_url}}/me/workflows
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "name": "contract-test-wf",
  "specYaml": "{\"nodes\":[]}"
}
HTTP 201
[Asserts]
jsonpath "$.id" isString
jsonpath "$.status" isString
jsonpath "$.ownerType" == "user"
[Captures]
workflow_id: jsonpath "$.id"

# List workflows
GET {{base_url}}/me/workflows
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.workflows" exists

# Partial update (rename only)
PUT {{base_url}}/me/workflows/{{workflow_id}}
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "name": "renamed-wf"
}
HTTP 200
[Asserts]
jsonpath "$.id" == "{{workflow_id}}"
jsonpath "$.name" isString

# Start a run
POST {{base_url}}/me/workflows/{{workflow_id}}/runs
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "workspaceId": "ws-1"
}
HTTP 202
[Asserts]
jsonpath "$.id" isString
jsonpath "$.status" isString
[Captures]
run_id: jsonpath "$.id"

# Run detail + cancel
GET {{base_url}}/me/runs/{{run_id}}
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.id" == "{{run_id}}"

POST {{base_url}}/me/runs/{{run_id}}/cancel
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.canceled" isBoolean

# Trigger CRUD: create webhook trigger (wrapped create response)
POST {{base_url}}/me/triggers
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "name": "contract-test-trigger",
  "sourceType": "webhook",
  "sourceConfig": {},
  "workflowId": "{{workflow_id}}"
}
HTTP 201

# List triggers, capture one
GET {{base_url}}/me/triggers
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.triggers" exists
[Captures]
trigger_id: jsonpath "$.triggers[0].id"

# Partial trigger update — enabled is a BOOLEAN (#1035 regression pin)
PUT {{base_url}}/me/triggers/{{trigger_id}}
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "enabled": true
}
HTTP 200
[Asserts]
jsonpath "$.enabled" isBoolean

# Delete workflow
DELETE {{base_url}}/me/workflows/{{workflow_id}}
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.deleted" isBoolean
