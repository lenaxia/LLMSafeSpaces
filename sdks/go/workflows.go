// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package llmsafespaces

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// --- Epic 64: Workflow + Trigger services ---

// Workflow represents a workflow definition.
type Workflow struct {
	ID                string          `json:"id"`
	OwnerType         string          `json:"ownerType"`
	Name              string          `json:"name"`
	Slug              string          `json:"slug"`
	Description       string          `json:"description,omitempty"`
	SpecYAML          string          `json:"specYaml"`
	InputSchema       json.RawMessage `json:"inputSchema,omitempty"`
	TargetWorkspaceID string          `json:"targetWorkspaceId,omitempty"`
	Status            string          `json:"status"`
	CreatedAt         time.Time       `json:"createdAt"`
	UpdatedAt         time.Time       `json:"updatedAt"`
}

// CreateWorkflowReq is the body for creating a workflow.
type CreateWorkflowReq struct {
	Name     string `json:"name"`
	SpecYAML string `json:"specYaml"`
	Status   string `json:"status,omitempty"`
}

// UpdateWorkflowReq is the body for partially updating a workflow.
type UpdateWorkflowReq struct {
	Name     *string `json:"name,omitempty"`
	Status   *string `json:"status,omitempty"`
	SpecYAML *string `json:"specYaml,omitempty"`
}

// WorkflowRun represents a single execution of a workflow.
type WorkflowRun struct {
	ID         string          `json:"id"`
	WorkflowID string          `json:"workflowId"`
	Status     string          `json:"status"`
	ErrorCode  string          `json:"errorCode,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
	Output     json.RawMessage `json:"output,omitempty"`
	StartedAt  *time.Time      `json:"startedAt,omitempty"`
	FinishedAt *time.Time      `json:"finishedAt,omitempty"`
	CreatedAt  time.Time       `json:"createdAt"`
}

// Trigger represents a trigger definition.
type Trigger struct {
	ID                  string          `json:"id"`
	Name                string          `json:"name"`
	Enabled             bool            `json:"enabled"`
	SourceType          string          `json:"sourceType"`
	SourceConfig        json.RawMessage `json:"sourceConfig"`
	TargetType          string          `json:"targetType"`
	TargetConfig        json.RawMessage `json:"targetConfig"`
	ConsecutiveFailures int             `json:"consecutiveFailures"`
	AutoDisableAfter    int             `json:"autoDisableAfter"`
	LastFiredAt         *time.Time      `json:"lastFiredAt,omitempty"`
	NextFireAt          *time.Time      `json:"nextFireAt,omitempty"`
}

// CreateTriggerReq is the body for creating a trigger.
type CreateTriggerReq struct {
	Name         string          `json:"name"`
	SourceType   string          `json:"sourceType"`
	TargetType   string          `json:"targetType"`
	SourceConfig json.RawMessage `json:"sourceConfig"`
	TargetConfig json.RawMessage `json:"targetConfig"`
}

// UpdateTriggerReq is the body for partially updating a trigger.
type UpdateTriggerReq struct {
	Enabled          *bool `json:"enabled,omitempty"`
	AutoDisableAfter *int  `json:"autoDisableAfter,omitempty"`
}

// WorkflowsService manages workflow definitions.
type WorkflowsService struct{ c *Client }

// List returns all workflows owned by the authenticated user.
func (s *WorkflowsService) List(ctx context.Context) ([]Workflow, error) {
	var resp struct {
		Workflows []Workflow `json:"workflows"`
	}
	if err := s.c.do(ctx, http.MethodGet, "/me/workflows", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Workflows, nil
}

// Get returns a single workflow by ID.
func (s *WorkflowsService) Get(ctx context.Context, id string) (*Workflow, error) {
	var wf Workflow
	if err := s.c.do(ctx, http.MethodGet, fmt.Sprintf("/me/workflows/%s", id), nil, &wf); err != nil {
		return nil, err
	}
	return &wf, nil
}

// Create creates a new workflow.
func (s *WorkflowsService) Create(ctx context.Context, req CreateWorkflowReq) (*Workflow, error) {
	var wf Workflow
	if err := s.c.do(ctx, http.MethodPost, "/me/workflows", req, &wf); err != nil {
		return nil, err
	}
	return &wf, nil
}

// Update partially updates a workflow.
func (s *WorkflowsService) Update(ctx context.Context, id string, req UpdateWorkflowReq) (*Workflow, error) {
	var wf Workflow
	if err := s.c.do(ctx, http.MethodPut, fmt.Sprintf("/me/workflows/%s", id), req, &wf); err != nil {
		return nil, err
	}
	return &wf, nil
}

// Delete deletes a workflow.
func (s *WorkflowsService) Delete(ctx context.Context, id string) error {
	return s.c.do(ctx, http.MethodDelete, fmt.Sprintf("/me/workflows/%s", id), nil, nil)
}

// Run starts a manual workflow run with optional input JSON.
func (s *WorkflowsService) Run(ctx context.Context, id string, input json.RawMessage, workspaceID string) (*WorkflowRun, error) {
	body := map[string]any{"input": input, "workspaceId": workspaceID}
	var run WorkflowRun
	if err := s.c.do(ctx, http.MethodPost, fmt.Sprintf("/me/workflows/%s/runs", id), body, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// GetRun returns the status of a workflow run.
func (s *WorkflowsService) GetRun(ctx context.Context, runID string) (*WorkflowRun, error) {
	var run WorkflowRun
	if err := s.c.do(ctx, http.MethodGet, fmt.Sprintf("/me/runs/%s", runID), nil, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// CancelRun cancels a running workflow.
func (s *WorkflowsService) CancelRun(ctx context.Context, runID string) error {
	return s.c.do(ctx, http.MethodPost, fmt.Sprintf("/me/runs/%s/cancel", runID), nil, nil)
}

// TriggersService manages trigger definitions.
type TriggersService struct{ c *Client }

// List returns all triggers owned by the authenticated user.
func (s *TriggersService) List(ctx context.Context) ([]Trigger, error) {
	var resp struct {
		Triggers []Trigger `json:"triggers"`
	}
	if err := s.c.do(ctx, http.MethodGet, "/me/triggers", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Triggers, nil
}

// Create creates a new trigger.
func (s *TriggersService) Create(ctx context.Context, req CreateTriggerReq) (*Trigger, error) {
	var trig Trigger
	if err := s.c.do(ctx, http.MethodPost, "/me/triggers", req, &trig); err != nil {
		return nil, err
	}
	return &trig, nil
}

// Update partially updates a trigger.
func (s *TriggersService) Update(ctx context.Context, id string, req UpdateTriggerReq) (*Trigger, error) {
	var trig Trigger
	if err := s.c.do(ctx, http.MethodPut, fmt.Sprintf("/me/triggers/%s", id), req, &trig); err != nil {
		return nil, err
	}
	return &trig, nil
}

// Delete deletes a trigger.
func (s *TriggersService) Delete(ctx context.Context, id string) error {
	return s.c.do(ctx, http.MethodDelete, fmt.Sprintf("/me/triggers/%s", id), nil, nil)
}
