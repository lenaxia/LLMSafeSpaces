# Task Board (#1699, design 0067 §4.3a): user-level manager CRUD over
# the /me board routes. Fixture: one workspace (project target, R8),
# two tasks in one epic for the dependency arms. The pr:N anchor form
# (owner ruling OR4) is pinned on the create arm.
#
# Dual-mode discipline (same as errors.hurl): this suite runs against
# BOTH the live API (make contract-test) and the Prism validation mock
# (make contract-test-mock). Prism serves schema-derived happy bodies
# and honors `Prefer: code=N` to select a documented error response —
# so asserts are structural (isString/pattern/enum-first) on happy arms
# and carry Prefer on error arms; the live-value pins (exact renames,
# blocked-by edges, guard code values) live in the canaries and the
# handler unit tests, which see real engine rows.

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
jsonpath "$.worker_slots" isInteger
[Captures]
project_id: jsonpath "$.id"
project_sid: jsonpath "$.short_id"

# Create project — validation: name required (real 400 in both modes)
POST {{base_url}}/me/projects
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "targetWorkspaceId": "{{board_ws_id}}"
}
HTTP 400
[Asserts]
jsonpath "$.error" isString

# Create project — authz: a nonexistent workspace target is the 404
# class under R7/R8
POST {{base_url}}/me/projects
Authorization: Bearer {{token}}
Content-Type: application/json
Prefer: code=404
{
  "name": "contract-test-board-x",
  "targetWorkspaceId": "00000000-0000-0000-0000-000000000000"
}
HTTP 404
[Asserts]
jsonpath "$.error" isString

# List projects — present
GET {{base_url}}/me/projects
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.projects" exists
jsonpath "$.projects[0].short_id" isString

# Get project by short id (case-insensitive resolution, design 0066;
# the uppercase form is pinned by the canaries — hurl cannot case-fold)
GET {{base_url}}/me/projects/{{project_sid}}
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.short_id" matches /^prj-[0-9a-z]{8}$/

# Get project — did-you-mean 404 (documented response)
GET {{base_url}}/me/projects/prj-zzzzzzzz
Authorization: Bearer {{token}}
Prefer: code=404
HTTP 404
[Asserts]
jsonpath "$.error" isString

# Patch project (rename cascades display inheritance server-side)
PATCH {{base_url}}/me/projects/{{project_sid}}
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "name": "contract-test-board-renamed",
  "tags": ["contract", "board"]
}
HTTP 200
[Asserts]
jsonpath "$.short_id" matches /^prj-[0-9a-z]{8}$/
jsonpath "$.status" isString

# Archive → reversible storage; unarchive → active again
POST {{base_url}}/me/projects/{{project_sid}}/archive
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.status" isString

POST {{base_url}}/me/projects/{{project_sid}}/unarchive
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.status" isString

# Create epic (default draft per ruling R2)
POST {{base_url}}/me/projects/{{project_sid}}/epics
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "title": "Epic one",
  "outcome": "the contract suite passes"
}
HTTP 201
[Asserts]
jsonpath "$.short_id" matches /^epc-[0-9a-z]{8}$/
jsonpath "$.status" == "draft"
[Captures]
epic_id: jsonpath "$.id"
epic_sid: jsonpath "$.short_id"

# Get epic — derived progress renders zero-denominator as no-progress
GET {{base_url}}/me/epics/{{epic_sid}}
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.short_id" matches /^epc-[0-9a-z]{8}$/
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
jsonpath "$.outcome" isString

# Epic lifecycle: activate → pause (r5: the activate 200 is the
# {"epic", "initial_ready_set"} wrapper — handler shape, spec'd)
POST {{base_url}}/me/epics/{{epic_sid}}/activate
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.epic.status" isString
jsonpath "$.initial_ready_set" isCollection

POST {{base_url}}/me/epics/{{epic_sid}}/pause
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.status" isString

# paused → pause again is not a lifecycle transition (guard 409)
POST {{base_url}}/me/epics/{{epic_sid}}/pause
Authorization: Bearer {{token}}
Prefer: code=409
HTTP 409
[Asserts]
jsonpath "$.code" isString

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
jsonpath "$.tags[0]" matches /^pr:[1-9][0-9]*$/
jsonpath "$.acceptance_criteria[0].met" isBoolean
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
jsonpath "$.task.short_id" matches /^tsk-[0-9a-z]{8}$/
jsonpath "$.task.is_blocked" isBoolean

# Dependency write — self edge is the 409 class (R4a)
POST {{base_url}}/me/tasks/{{task_b_sid}}/dependencies
Authorization: Bearer {{token}}
Content-Type: application/json
Prefer: code=409
{
  "dependsOn": "{{task_b_sid}}"
}
HTTP 409
[Asserts]
jsonpath "$.code" isString

# Dependency target near-miss: the 404 carries project-scoped
# did-you-mean suggestions (design 0066 shape, R4a)
POST {{base_url}}/me/tasks/{{task_b_sid}}/dependencies
Authorization: Bearer {{token}}
Content-Type: application/json
Prefer: code=404
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
Prefer: code=409
{
  "dependsOn": "{{task_b_sid}}"
}
HTTP 409
[Asserts]
jsonpath "$.code" isString

# Remove the edge — renders the re-derived task (unblocked on the live API)
DELETE {{base_url}}/me/tasks/{{task_b_sid}}/dependencies/{{task_a_sid}}
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.task.is_blocked" isBoolean

# Get task by short id
GET {{base_url}}/me/tasks/{{task_b_sid}}
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.short_id" matches /^tsk-[0-9a-z]{8}$/
jsonpath "$.stage" isString

# List project tasks (+ stage filter): the enumerated set is accepted
GET {{base_url}}/me/projects/{{project_sid}}/tasks?stage=backlog
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.tasks" exists

# Unknown stage filter is a real 400 in both modes (schema enum)
GET {{base_url}}/me/projects/{{project_sid}}/tasks?stage=wip
Authorization: Bearer {{token}}
HTTP 400

# Patch task — criteria check-off rides the PATCH (Done preparation)
PATCH {{base_url}}/me/tasks/{{task_a_sid}}
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "setCriteriaMet": 1
}
HTTP 200
[Asserts]
jsonpath "$.acceptance_criteria[1].met" isBoolean

# move_stage from backlog: the stage machine owns the refusal — only the
# enumerated transitions exist (backlog leaves by claim)
POST {{base_url}}/me/tasks/{{task_a_sid}}/move-stage
Authorization: Bearer {{token}}
Content-Type: application/json
Prefer: code=409
{
  "toStage": "in_progress"
}
HTTP 409
[Asserts]
jsonpath "$.code" isString

POST {{base_url}}/me/tasks/{{task_a_sid}}/move-stage
Authorization: Bearer {{token}}
Content-Type: application/json
Prefer: code=409
{
  "toStage": "done"
}
HTTP 409

# Delete task B (#1747: cancel is eliminated — delete replaces it; the
# result lists orphaned dependents. The gone-row-replay 404 is an
# engine-state behavior pinned by the kill-tests — this Prism-mocked
# contract tier is stateless (every matched route 200s), so only the
# contract shape is asserted here.)
DELETE {{base_url}}/me/tasks/{{task_b_sid}}
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.item_kind" isString

# Delete epic: cascades to its tasks (#1747; the audit keeps every
# snapshot)
DELETE {{base_url}}/me/epics/{{epic_sid}}
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.item_kind" isString

# Authz arm: unauthenticated reads are 401 (Prism enforces security
# schemes; the live API rejects the missing bearer)
GET {{base_url}}/me/projects
HTTP 401

# --- #1699 seam closings: epic listing, board search, board health,
# lifecycle replay keys -------------------------------------------------

# Epic listing: derived progress rides every row (ruling R2 — never
# stored; the zero-denominator shape renders no-progress)
GET {{base_url}}/me/projects/{{project_sid}}/epics
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.epics[0].short_id" matches /^epc-[0-9a-z]{8}$/
jsonpath "$.epics[0].progress_total" isInteger
jsonpath "$.epics[0].progress_derived" isBoolean

# Epic listing on a foreign/missing project: plain 404, never disclosed
GET {{base_url}}/me/projects/prj-zzzzzzzz/epics
Authorization: Bearer {{token}}
Prefer: code=404
HTTP 404

# Board search: q required; structural happy shape
GET {{base_url}}/me/board/search?q=contract&tag=contract
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.results[0].short_id" isString

# Search by exact short id (case-insensitive resolution shape)
GET {{base_url}}/me/board/search?q={{task_a_sid}}
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.results[0].short_id" isString

# Missing q: adapter-owned 400
GET {{base_url}}/me/board/search
Authorization: Bearer {{token}}
HTTP 400

# Unknown stage filter: adapter-owned 400 (enumerated set)
GET {{base_url}}/me/board/search?q=x&stage=wip
Authorization: Bearer {{token}}
HTTP 400

# Board health: the §4.8 snapshot shape, zeroed arrays on a quiet board
GET {{base_url}}/me/board/health
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.project_slots[0].worker_slots" isInteger
jsonpath "$.project_slots[0].in_use" isInteger

# Lifecycle replay key: the optional requestId body threads (a replay
# renders 200-with-original — the duplicate arm pins it end-to-end)
POST {{base_url}}/me/epics/{{epic_sid}}/activate
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "requestId": "11111111-2222-3333-4444-555555555555"
}
HTTP 200
[Asserts]
jsonpath "$.status" isString

# The replay: SAME requestId — 200-with-original, never an error
POST {{base_url}}/me/epics/{{epic_sid}}/activate
Authorization: Bearer {{token}}
Content-Type: application/json
{
  "requestId": "11111111-2222-3333-4444-555555555555"
}
HTTP 200
[Asserts]
jsonpath "$.status" isString

# DELETE replay key rides the query parameter
DELETE {{base_url}}/me/tasks/{{task_b_sid}}/dependencies/{{task_a_sid}}?requestId=11111111-2222-3333-4444-555555555566
Authorization: Bearer {{token}}
HTTP 200
[Asserts]
jsonpath "$.task.short_id" isString
