// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package types

import (
	"encoding/json"
	"regexp"
	"time"
)

// --- Epic 64: Triggers & Workflows (user|org scope) ---

// WorkflowStatus is the lifecycle state of a workflow definition (draft → active → archived).
const (
	WorkflowStatusDraft   = "draft"
	WorkflowStatusActive  = "active"
	WorkflowStatusArchive = "archived"
)

// Owner scopes (mirrors provider_credentials.owner_type — minus admin, deferred to v2 per D7).
const (
	WorkflowOwnerUser = "user"
	WorkflowOwnerOrg  = "org"
)

// TriggerSourceType enumerates trigger source types (design D5: 'manual' is NOT a source type;
// manual runs go through POST /workflows/:id/runs with trigger_id = null).
const (
	TriggerSourceCron    = "cron"
	TriggerSourceWebhook = "webhook"
)

// TriggerTargetType enumerates what a trigger fires.
const (
	TriggerTargetRunWorkflow = "run_workflow"
	TriggerTargetRunScript   = "run_script"
)

// NodeType enumerates the four v1 node types (design Node Type Specifications).
const (
	NodeTypeScript    = "script"
	NodeTypeAgent     = "agent"
	NodeTypeHTTP      = "http"
	NodeTypeCondition = "condition"
)

// RunStatus is the six-state workflow run state machine (no library, design D8).
const (
	RunStatusQueued    = "queued"
	RunStatusRunning   = "running"
	RunStatusSucceeded = "succeeded"
	RunStatusFailed    = "failed"
	RunStatusCanceled  = "canceled"
	RunStatusTimedOut  = "timed_out"
)

// IsTerminalRunStatus reports whether s is a terminal run state.
func IsTerminalRunStatus(s string) bool {
	switch s {
	case RunStatusSucceeded, RunStatusFailed, RunStatusCanceled, RunStatusTimedOut:
		return true
	}
	return false
}

// NodeRunStatus is the per-node state.
const (
	NodeRunStatusPending   = "pending"
	NodeRunStatusRunning   = "running"
	NodeRunStatusSucceeded = "succeeded"
	NodeRunStatusFailed    = "failed"
	NodeRunStatusSkipped   = "skipped"
)

// TriggerFireStatus records what happened when a trigger fired.
const (
	TriggerFireFired           = "fired"
	TriggerFireDelivered       = "delivered"
	TriggerFireFailed          = "failed"
	TriggerFireValidationError = "validation_error"
	TriggerFireRateLimited     = "rate_limited"
	TriggerFireSkipped         = "skipped"
	TriggerFireAutoDisabled    = "auto_disabled"
)

// WebhookIdempotencyMode controls how duplicate deliveries are detected.
const (
	WebhookIdempotencyHeader   = "header"
	WebhookIdempotencyHash     = "hash"
	WebhookIdempotencyDisabled = "disabled"
)

// RunErrorCode is the machine-readable failure categorization on workflow_runs
// (and workflow_node_runs.error_code). Bounded by the migration's CHECK constraint.
const (
	RunErrorCodeNodeFailed           = "node_failed"
	RunErrorCodeWorkspaceUnavailable = "workspace_unavailable"
	RunErrorCodeCanceled             = "canceled"
	RunErrorCodeTimedOut             = "timed_out"
	RunErrorCodeValidationError      = "validation_error"
	RunErrorCodeSchemaMismatch       = "schema_mismatch"
	RunErrorCodeOutputOversize       = "output_oversize"
	RunErrorCodeAgentNotFound        = "agent_not_found"
	RunErrorCodeSessionNotFound      = "session_not_found"
	RunErrorCodeSecretNotFound       = "secret_not_found"
	RunErrorCodeScriptFailed         = "script_failed"
	RunErrorCodeScriptOutputInvalid  = "script_output_invalid"
	RunErrorCodeAPIRestart           = "api_restart"
)

// workflowNameRe bounds workflow/trigger names — readable, no shell-special chars.
var workflowNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9 _-]{0,127}$`)

// workflowSlugRe bounds workflow slugs — URL-safe lowercase.
var workflowSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,127}$`)

// ValidWorkflowName reports whether name is an acceptable workflow/trigger name.
func ValidWorkflowName(name string) bool { return workflowNameRe.MatchString(name) }

// ValidWorkflowSlug reports whether slug is an acceptable workflow slug.
func ValidWorkflowSlug(slug string) bool { return workflowSlugRe.MatchString(slug) }

// ValidWorkflowStatus reports whether s is a valid workflow definition status.
func ValidWorkflowStatus(s string) bool {
	switch s {
	case WorkflowStatusDraft, WorkflowStatusActive, WorkflowStatusArchive:
		return true
	}
	return false
}

// ValidWorkflowOwnerType reports whether t is a supported owner_type (v1: user|org).
func ValidWorkflowOwnerType(t string) bool {
	switch t {
	case WorkflowOwnerUser, WorkflowOwnerOrg:
		return true
	}
	return false
}

// ValidTriggerSourceType reports whether t is a supported trigger source type.
func ValidTriggerSourceType(t string) bool {
	switch t {
	case TriggerSourceCron, TriggerSourceWebhook:
		return true
	}
	return false
}

// ValidTriggerTargetType reports whether t is a supported trigger target type.
func ValidTriggerTargetType(t string) bool {
	switch t {
	case TriggerTargetRunWorkflow, TriggerTargetRunScript:
		return true
	}
	return false
}

// ValidNodeType reports whether t is a supported node type (v1: 4 types).
func ValidNodeType(t string) bool {
	switch t {
	case NodeTypeScript, NodeTypeAgent, NodeTypeHTTP, NodeTypeCondition:
		return true
	}
	return false
}

// ValidRunStatus reports whether s is a valid workflow run status.
func ValidRunStatus(s string) bool {
	switch s {
	case RunStatusQueued, RunStatusRunning, RunStatusSucceeded, RunStatusFailed, RunStatusCanceled, RunStatusTimedOut:
		return true
	}
	return false
}

// ValidNodeRunStatus reports whether s is a valid per-node run status.
func ValidNodeRunStatus(s string) bool {
	switch s {
	case NodeRunStatusPending, NodeRunStatusRunning, NodeRunStatusSucceeded, NodeRunStatusFailed, NodeRunStatusSkipped:
		return true
	}
	return false
}

// ValidWebhookIdempotencyMode reports whether m is a supported idempotency mode.
func ValidWebhookIdempotencyMode(m string) bool {
	switch m {
	case WebhookIdempotencyHeader, WebhookIdempotencyHash, WebhookIdempotencyDisabled:
		return true
	}
	return false
}

// ValidRunErrorCode reports whether c is a member of the bounded error_code set
// (enforced by the workflow_runs.error_code CHECK constraint in migration 000016).
func ValidRunErrorCode(c string) bool {
	switch c {
	case RunErrorCodeNodeFailed, RunErrorCodeWorkspaceUnavailable, RunErrorCodeCanceled,
		RunErrorCodeTimedOut, RunErrorCodeValidationError, RunErrorCodeSchemaMismatch,
		RunErrorCodeOutputOversize, RunErrorCodeAgentNotFound, RunErrorCodeSessionNotFound,
		RunErrorCodeSecretNotFound, RunErrorCodeScriptFailed, RunErrorCodeScriptOutputInvalid,
		RunErrorCodeAPIRestart:
		return true
	}
	return false
}

// --- Transfer objects (API request/response shapes) -------------------------

// WorkflowResponse is the API response shape for a workflow definition.
// spec_yaml is the author's input; spec_json is the parsed DAG (denormalized for execution).
// target_workspace_id is nullable (null = caller picks at run time).
type WorkflowResponse struct {
	ID                string          `json:"id"`
	OwnerType         string          `json:"ownerType"`
	OwnerID           string          `json:"ownerId,omitempty"`
	Name              string          `json:"name"`
	Slug              string          `json:"slug"`
	Description       string          `json:"description,omitempty"`
	SpecYAML          string          `json:"specYaml"`
	InputSchema       json.RawMessage `json:"inputSchema,omitempty"`
	TargetWorkspaceID string          `json:"targetWorkspaceId,omitempty"`
	Status            string          `json:"status"`
	Defaults          json.RawMessage `json:"defaults,omitempty"`
	CreatedAt         time.Time       `json:"createdAt"`
	UpdatedAt         time.Time       `json:"updatedAt"`
}

// CreateWorkflowRequest is the body for POST .../workflows.
// The server computes slug from name if not provided; spec_yaml is validated + parsed
// into spec_json by the DAG validator (US-64.4).
type CreateWorkflowRequest struct {
	Name              string          `json:"name" binding:"required"`
	Slug              string          `json:"slug,omitempty"`
	Description       string          `json:"description,omitempty"`
	SpecYAML          string          `json:"specYaml" binding:"required"`
	InputSchema       json.RawMessage `json:"inputSchema,omitempty"`
	TargetWorkspaceID string          `json:"targetWorkspaceId,omitempty"`
	Status            string          `json:"status,omitempty"`
	Defaults          json.RawMessage `json:"defaults,omitempty"`
}

// UpdateWorkflowRequest supports partial update. Pointer fields: nil = "keep existing".
// A non-nil value replaces it. status transitions (draft→active→archived) are validated
// in the service layer. Updating spec_yaml creates a new spec_snapshot baseline; in-flight
// runs are pinned to the snapshot at their start (D6).
type UpdateWorkflowRequest struct {
	Name              *string         `json:"name,omitempty"`
	Slug              *string         `json:"slug,omitempty"`
	Description       *string         `json:"description,omitempty"`
	SpecYAML          *string         `json:"specYaml,omitempty"`
	InputSchema       json.RawMessage `json:"inputSchema,omitempty"`
	TargetWorkspaceID *string         `json:"targetWorkspaceId,omitempty"`
	Status            *string         `json:"status,omitempty"`
	Defaults          json.RawMessage `json:"defaults,omitempty"`
}

// TriggerResponse is the API response shape for a trigger.
// source_config and target_config are typed JSON blobs (validated by the handler).
// next_fire_at is computed by the scheduler for cron triggers.
type TriggerResponse struct {
	ID                  string          `json:"id"`
	OwnerType           string          `json:"ownerType"`
	OwnerID             string          `json:"ownerId,omitempty"`
	Name                string          `json:"name"`
	Description         string          `json:"description,omitempty"`
	Enabled             bool            `json:"enabled"`
	SourceType          string          `json:"sourceType"`
	SourceConfig        json.RawMessage `json:"sourceConfig"`
	TargetType          string          `json:"targetType"`
	TargetConfig        json.RawMessage `json:"targetConfig"`
	ConsecutiveFailures int             `json:"consecutiveFailures"`
	AutoDisableAfter    int             `json:"autoDisableAfter"`
	LastFiredAt         *time.Time      `json:"lastFiredAt,omitempty"`
	NextFireAt          *time.Time      `json:"nextFireAt,omitempty"`
	CreatedAt           time.Time       `json:"createdAt"`
	UpdatedAt           time.Time       `json:"updatedAt"`
}

// CreateTriggerRequest is the body for POST .../triggers.
// For webhook sources, an accompanying webhooks row (with secret_cipher) is created
// in the same transaction. auto_disable_after defaults to 10 if unset.
type CreateTriggerRequest struct {
	Name             string          `json:"name" binding:"required"`
	Description      string          `json:"description,omitempty"`
	Enabled          *bool           `json:"enabled,omitempty"`
	SourceType       string          `json:"sourceType" binding:"required"`
	SourceConfig     json.RawMessage `json:"sourceConfig" binding:"required"`
	TargetType       string          `json:"targetType" binding:"required"`
	TargetConfig     json.RawMessage `json:"targetConfig" binding:"required"`
	AutoDisableAfter *int            `json:"autoDisableAfter,omitempty"`
	// Webhook-specific fields (required when sourceType == 'webhook'):
	WebhookAllowedIPs        []string `json:"webhookAllowedIps,omitempty"`
	WebhookIdempotencyMode   string   `json:"webhookIdempotencyMode,omitempty"`
	WebhookIdempotencyHeader string   `json:"webhookIdempotencyHeader,omitempty"`
}

// UpdateTriggerRequest supports partial update. Pointer fields: nil = "keep existing".
// source_type is NOT mutable after create (the source defines the trigger's identity).
// auto_disable_after must be >= 1 (validated at handler).
type UpdateTriggerRequest struct {
	Name             *string         `json:"name,omitempty"`
	Description      *string         `json:"description,omitempty"`
	Enabled          *bool           `json:"enabled,omitempty"`
	SourceConfig     json.RawMessage `json:"sourceConfig,omitempty"`
	TargetType       *string         `json:"targetType,omitempty"`
	TargetConfig     json.RawMessage `json:"targetConfig,omitempty"`
	AutoDisableAfter *int            `json:"autoDisableAfter,omitempty"`
}

// WorkflowRunResponse is the API response shape for a workflow run.
// spec_snapshot is the immutable DAG pinned at run start. error_code is null on success.
type WorkflowRunResponse struct {
	ID            string          `json:"id"`
	WorkflowID    string          `json:"workflowId"`
	SpecSnapshot  json.RawMessage `json:"specSnapshot"`
	Input         json.RawMessage `json:"input,omitempty"`
	Output        json.RawMessage `json:"output,omitempty"`
	Status        string          `json:"status"`
	ErrorCode     string          `json:"errorCode,omitempty"`
	Error         json.RawMessage `json:"error,omitempty"`
	TriggerID     string          `json:"triggerId,omitempty"`
	TriggerFireID string          `json:"triggerFireId,omitempty"`
	WorkspaceID   string          `json:"workspaceId"`
	StartedAt     *time.Time      `json:"startedAt,omitempty"`
	FinishedAt    *time.Time      `json:"finishedAt,omitempty"`
	CreatedAt     time.Time       `json:"createdAt"`
	UpdatedAt     time.Time       `json:"updatedAt"`
}

// CreateWorkflowRunRequest is the body for POST .../workflows/:id/runs (manual run).
// workspace_id overrides the workflow's target_workspace_id if set. input must
// satisfy the workflow's inputSchema (strict validation for manual runs per D12).
type CreateWorkflowRunRequest struct {
	Input       json.RawMessage `json:"input,omitempty"`
	WorkspaceID string          `json:"workspaceId,omitempty"`
}

// WorkflowNodeRunResponse is the per-node state within a run.
// node_id matches spec_snapshot.nodes[].id (NOT the current spec — pinned).
type WorkflowNodeRunResponse struct {
	ID            string          `json:"id"`
	WorkflowRunID string          `json:"workflowRunId"`
	NodeID        string          `json:"nodeId"`
	NodeType      string          `json:"nodeType"`
	Status        string          `json:"status"`
	Attempt       int             `json:"attempt"`
	Input         json.RawMessage `json:"input,omitempty"`
	Output        json.RawMessage `json:"output,omitempty"`
	Branch        string          `json:"branch,omitempty"`
	ErrorCode     string          `json:"errorCode,omitempty"`
	Error         json.RawMessage `json:"error,omitempty"`
	StartedAt     time.Time       `json:"startedAt"`
	FinishedAt    *time.Time      `json:"finishedAt,omitempty"`
}

// TriggerFireResponse is the API response shape for a trigger fire audit row.
// input_envelope is the raw source payload (webhook body + headers; cron template render).
type TriggerFireResponse struct {
	ID            string          `json:"id"`
	TriggerID     string          `json:"triggerId"`
	SourceType    string          `json:"sourceType"`
	InputEnvelope json.RawMessage `json:"inputEnvelope,omitempty"`
	ActionType    string          `json:"actionType"`
	ActionResult  json.RawMessage `json:"actionResult,omitempty"`
	Status        string          `json:"status"`
	FiredAt       time.Time       `json:"firedAt"`
	CompletedAt   *time.Time      `json:"completedAt,omitempty"`
}

// CronSourceConfig is the typed shape of triggers.source_config for cron sources.
// expr is a cron expression (validated by the handler); tz is an IANA timezone name.
type CronSourceConfig struct {
	Expr string `json:"expr"`
	TZ   string `json:"tz,omitempty"`
}

// WebhookSourceConfig is the typed shape of triggers.source_config for webhook sources.
// WebhookID references the webhooks row carrying the HMAC secret + IP allowlist.
type WebhookSourceConfig struct {
	WebhookID string `json:"webhookId"`
}

// RunWorkflowTargetConfig is the typed shape of triggers.target_config for run_workflow.
// input_template is a text/template map rendered against the trigger envelope at fire time.
type RunWorkflowTargetConfig struct {
	WorkflowID    string            `json:"workflowId"`
	InputTemplate map[string]string `json:"inputTemplate,omitempty"`
}

// RunScriptTargetConfig is the typed shape of triggers.target_config for run_script.
type RunScriptTargetConfig struct {
	WorkspaceID string            `json:"workspaceId"`
	Path        string            `json:"path"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
}
