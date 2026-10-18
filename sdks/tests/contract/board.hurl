# Task Board (#1699, design 0067 §4.3a): user-level manager CRUD over
# the /me board routes. Fixture: one workspace (project target, R8),
# two tasks in one epic for the dependency arms. The pr:N anchor form
# (owner ruling OR4) is pinned on the create/update arms.

# Fixture: a workspace row for the project target (single workspace per
# project, ruling R8 — parallel work is sibling projects).
POST {{base_url}}/workspaces
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "name": "contract-test-board-ws",
  "runtime": "python:3.11",
  "storageSize": "1Gi"
}
HTTP 201
[Captures]
board_ws_id: jsonpath "$.id"

# Create project (name + target workspace required)
POST {{base_url}}/me/projects
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "name": "contract-test-board",
  "targetWorkspaceId": "{{board_ws_id}}",
  "tags": ["contract"]
}
HTTP 201
[Asserts]
jsonpath "$.id" isString
jsonpath "$.short_id" matches /^prj-[0-9a-z]{8}$/
jsonpath "$.user_id" isString
jsonpath "$.status" == "active"
jsonpath "$.worker_slots" == 2
[Captures]
project_id: jsonpath "$.id"
project_sid: jsonpath "$.short_id"

# Create project — validation: name required
POST {{base_url}}/me/projects
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "targetWorkspaceId": "{{board_ws_id}}"
}
HTTP 400
[Asserts]
jsonpath "$.error" isString

# Create project — authz: a foreign workspace target is rejected (R7/R8)
POST {{base_url}}/me/projects
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "name": "contract-test-board-x",
  "targetWorkspaceId": "00000000-0000-0000-0000-000000000000"
}
HTTP 404

# List projects — present
GET {{base_url}}/me/projects
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.projects" exists
jsonpath "$.projects[0].short_id" isString

# Get project by short id (case-insensitive resolution, design 0066)
GET {{base_url}}/me/projects/{{project_sid}}
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.id" == "{{project_id}}"

# Get project — did-you-mean 404 (near-miss short id carries suggestions)
GET {{base_url}}/me/projects/prj-zzzzzzzz
Authorization: Bearer {{token}}
HTTP 404
[Asserts]
jsonpath "$.error" isString

# Patch project (rename; tags replace — send the full set)
PATCH {{base_url}}/me/projects/{{project_sid}}
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "name": "contract-test-board-renamed",
  "tags": ["contract", "board"]
}
HTTP 200
[Asserts]
jsonpath "$.name" == "contract-test-board-renamed"
jsonpath "$.tags[1]" == "board"

# Archive → reversible storage; unarchive → active again
POST {{base_url}}/me/projects/{{project_sid}}/archive
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.status" == "archived"

POST {{base_url}}/me/projects/{{project_sid}}/unarchive
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.status" == "active"

# Create epic (default draft per ruling R2)
POST {{base_url}}/me/projects/{{project_sid}}/epics
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "title": "Epic one",
  "outcome": "the contract suite passes",
  "priority": "P1"
}
HTTP 201
[Asserts]
jsonpath "$.short_id" matches /^epc-[0-9a-z]{8}$/
jsonpath "$.status" == "draft"
jsonpath "$.priority" == "P1"
[Captures]
epic_id: jsonpath "$.id"
epic_sid: jsonpath "$.short_id"

# Get epic — derived progress renders zero-denominator as no-progress
GET {{base_url}}/me/epics/{{epic_sid}}
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.id" == "{{epic_id}}"
jsonpath "$.progress_derived" == false

# Patch epic (outcome is the human's steering wheel)
PATCH {{base_url}}/me/epics/{{epic_sid}}
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "outcome": "steered"
}
HTTP 200
[Asserts]
jsonpath "$.outcome" == "steered"

# Epic lifecycle: activate → pause
POST {{base_url}}/me/epics/{{epic_sid}}/activate
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.status" == "active"

POST {{base_url}}/me/epics/{{epic_sid}}/pause
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.status" == "paused"

# paused → pause again is not a lifecycle transition (guard 409)
POST {{base_url}}/me/epics/{{epic_sid}}/pause
Authorization: Bearer {{token}}
HTTP 409
[Asserts]
jsonpath "$.code" == "epic_state"

# Back to active for the task arms
POST {{base_url}}/me/epics/{{epic_sid}}/activate
Authorization: Bearer {{token}}
HTTP 200

# Create task A — OR4 pin: the pr:N anchor is an explicit tag
# (pattern ^pr:[1-9][0-9]*$; no free-text guessing, no URL scraping)
POST {{base_url}}/me/epics/{{epic_sid}}/tasks
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "title": "Task A",
  "description": "markdown charter",
  "priority": "P0",
  "tags": ["pr:1234"],
  "acceptanceCriteria": [
    {"text": "tests green", "met": true},
    {"text": "docs updated", "met": false}
  ]
}
HTTP 201
[Asserts]
jsonpath "$.short_id" matches /^tsk-[0-9a-z]{8}$/
jsonpath "$.stage" == "backlog"
jsonpath "$.priority" == "P0"
jsonpath "$.tags[0]" matches /^pr:[1-9][0-9]*$/
jsonpath "$.acceptance_criteria[0].met" == true
[Captures]
task_a_sid: jsonpath "$.short_id"

# Create task B
POST {{base_url}}/me/epics/{{epic_sid}}/tasks
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "title": "Task B"
}
HTTP 201
[Captures]
task_b_sid: jsonpath "$.short_id"

# Task B depends on A — structural edge (the only blocking mechanism, R3/R4)
POST {{base_url}}/me/tasks/{{task_b_sid}}/dependencies
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "dependsOn": "{{task_a_sid}}"
}
HTTP 200
[Asserts]
jsonpath "$.task.is_blocked" == true
jsonpath "$.task.blocked_by[0]" == "{{task_a_sid}}"

# Dependency write — self edge is the 409 class (R4a)
POST {{base_url}}/me/tasks/{{task_b_sid}}/dependencies
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "dependsOn": "{{task_b_sid}}"
}
HTTP 409
[Asserts]
jsonpath "$.code" == "self"

# Dependency target near-miss: the 404 carries project-scoped
# did-you-mean suggestions (design 0066 shape, R4a)
POST {{base_url}}/me/tasks/{{task_b_sid}}/dependencies
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "dependsOn": "{{task_b_sid}}x"
}
HTTP 404
[Asserts]
jsonpath "$.suggestions" exists

# Cycle rejection: A → B would close the loop (409 + did-you-mean, R4a)
POST {{base_url}}/me/tasks/{{task_a_sid}}/dependencies
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "dependsOn": "{{task_b_sid}}"
}
HTTP 409
[Asserts]
jsonpath "$.code" == "cycle"

# Remove the edge — B unblocks in the same read
DELETE {{base_url}}/me/tasks/{{task_b_sid}}/dependencies/{{task_a_sid}}
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.task.is_blocked" == false

# Get task by short id
GET {{base_url}}/me/tasks/{{task_b_sid}}
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.title" == "Task B"

# List project tasks (+ stage filter, enumerated set)
GET {{base_url}}/me/projects/{{project_sid}}/tasks?stage=backlog
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.tasks" count == 2

GET {{base_url}}/me/projects/{{project_sid}}/tasks?stage=wip
Authorization: Bearer {{token}}
HTTP 400

# Patch task — criteria check-off rides the PATCH (Done preparation);
# priority changes are ordinary writes
PATCH {{base_url}}/me/tasks/{{task_a_sid}}
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "setCriteriaMet": 1
}
HTTP 200
[Asserts]
jsonpath "$.acceptance_criteria[1].met" == true

# move_stage from backlog: the stage machine owns the refusal — only the
# enumerated transitions exist (backlog leaves by claim; the human rows
# enter the machine at pending_input exits)
POST {{base_url}}/me/tasks/{{task_a_sid}}/move-stage
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "toStage": "in_progress"
}
HTTP 409
[Asserts]
jsonpath "$.code" == "stage_transition"

POST {{base_url}}/me/tasks/{{task_a_sid}}/move-stage
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "toStage": "done"
}
HTTP 409
[Asserts]
jsonpath "$.code" == "stage_transition"

# Cancel task B from backlog — terminal write, renders the cancelled row
POST {{base_url}}/me/tasks/{{task_b_sid}}/cancel
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "reason": "contract cleanup"
}
HTTP 200
[Asserts]
jsonpath "$.stage" == "cancelled"

# Cancelled is immutable-terminal, and cancel is idempotent on it
POST {{base_url}}/me/tasks/{{task_b_sid}}/cancel
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "reason": "again"
}
HTTP 200
[Asserts]
jsonpath "$.stage" == "cancelled"

# Cancel epic: cascades to tasks, epic itself lands paused (R2's set has
# no cancelled epic value)
POST {{base_url}}/me/epics/{{epic_sid}}/cancel
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "reason": "contract cleanup"
}
HTTP 200
[Asserts]
jsonpath "$.status" == "paused"

# Authz arm: unauthenticated reads are 401
GET {{base_url}}/me/projects
HTTP 401
