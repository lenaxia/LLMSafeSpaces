// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package types

import (
	"fmt"
	"time"
)

// Workspace is the API transfer object for a workspace resource.
type Workspace struct {
	ID                      string            `json:"id"`
	Name                    string            `json:"name"`
	UserID                  string            `json:"userId"`
	Runtime                 string            `json:"runtime"`
	StorageSize             string            `json:"storageSize"`
	Phase                   string            `json:"phase"`
	PVCName                 string            `json:"pvcName,omitempty"`
	Labels                  map[string]string `json:"labels,omitempty"`
	DefaultModel            string            `json:"defaultModel,omitempty"`
	CreatedAt               time.Time         `json:"createdAt"`
	UpdatedAt               time.Time         `json:"updatedAt"`
	AgentNeedsRefresh       bool              `json:"agentNeedsRefresh"`
	CredentialsPendingSince *time.Time        `json:"credentialsPendingSince,omitempty"`
	// DevPreviewEnabled reflects spec.networkAccess.devPreview (Epic 66).
	DevPreviewEnabled bool `json:"devPreviewEnabled,omitempty"`
}

// CreateWorkspaceRequest is the request body for creating a workspace.
type CreateWorkspaceRequest struct {
	Name         string            `json:"name"`
	Runtime      string            `json:"runtime"`
	StorageSize  string            `json:"storageSize"`
	StorageClass string            `json:"storageClass,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	OrgID        *string           `json:"orgId,omitempty"`
	// ImageConfigHash references an image-factory config (design/0046).
	// When set, the workspace service resolves it to a Ready config's
	// built image ref and overrides Runtime with it. The config must be
	// Ready and owned by the user, their org, or be platform-scoped.
	ImageConfigHash string `json:"imageConfigHash,omitempty"`
}

// WorkspaceListResult bundles workspace list items with pagination.
type WorkspaceListResult struct {
	Items      []WorkspaceListItem `json:"items"`
	Pagination *PaginationMetadata `json:"pagination,omitempty"`
}

// WorkspaceListItem is a lightweight workspace representation for list responses.
type WorkspaceListItem struct {
	ID                      string     `json:"id"`
	Name                    string     `json:"name"`
	UserID                  string     `json:"userId"`
	Runtime                 string     `json:"runtime"`
	StorageSize             string     `json:"storageSize"`
	Phase                   string     `json:"phase,omitempty"`
	ImageTag                string     `json:"imageTag,omitempty"`
	AgentVersion            string     `json:"agentVersion,omitempty"`
	DefaultModel            string     `json:"defaultModel,omitempty"`
	MaxActiveSessions       int        `json:"maxActiveSessions,omitempty"`
	CreatedAt               time.Time  `json:"createdAt"`
	UpdatedAt               time.Time  `json:"updatedAt"`
	AgentNeedsRefresh       bool       `json:"agentNeedsRefresh"`
	CredentialsPendingSince *time.Time `json:"credentialsPendingSince,omitempty"`
	// OrgID is the owning org for org-scoped workspaces (Epic 11; nil for
	// personal workspaces). The frontend relies on this field to decide
	// whether to fetch and enforce the org's allow_user_prompt policy in
	// the Workspace Settings drawer's "Custom Instructions" Lock UI.
	// Mirrors WorkspaceMetadata.OrgID.
	OrgID *string `json:"orgId,omitempty"`
}

// WorkspaceStatusResult carries the status fields read from the Workspace CRD.
type WorkspaceStatusResult struct {
	Phase            string                     `json:"phase"`
	PVCName          string                     `json:"pvcName,omitempty"`
	ActiveSessions   int                        `json:"activeSessions"`
	LastActivityAt   *time.Time                 `json:"lastActivityAt,omitempty"`
	Message          string                     `json:"message,omitempty"`
	Conditions       []WorkspaceConditionResult `json:"conditions,omitempty"`
	CredentialState  CredentialStateResult      `json:"credentialState"`
	AgentHealth      AgentHealthResult          `json:"agentHealth"`
	Sessions         []SessionStatusItem        `json:"sessions,omitempty"`
	ImageTag         string                     `json:"imageTag,omitempty"`
	DiskUsedBytes    int64                      `json:"diskUsedBytes,omitempty"`
	DiskTotalBytes   int64                      `json:"diskTotalBytes,omitempty"`
	MemoryUsedBytes  int64                      `json:"memoryUsedBytes,omitempty"`
	MemoryTotalBytes int64                      `json:"memoryTotalBytes,omitempty"`
	ContextUsed      int64                      `json:"contextUsed"`
	ContextTotal     int64                      `json:"contextTotal"`
}

// WorkspaceMetadata is the database record for a workspace.
//
// Phase and pvc_state used to live here as a denormalised cache of the
// Workspace CRD's status fields. The cache was removed in migration 9
// because it was eventually-consistent at best (best-effort writes from
// `syncPhase`) and routinely diverged from the CRD shortly after creation,
// which caused the sidebar to render new workspaces with no phase. The CRD
// is now the only source of truth; phase is fetched directly from the
// kube-apiserver in `ListWorkspaces` and `enforceMaxActiveWorkspaces`.
type WorkspaceMetadata struct {
	ID           string    `json:"id" db:"id"`
	UserID       string    `json:"userId" db:"user_id"`
	Name         string    `json:"name" db:"name"`
	Runtime      string    `json:"runtime" db:"runtime"`
	StorageSize  string    `json:"storageSize" db:"storage_size"`
	ImageTag     string    `json:"imageTag" db:"image_tag"`
	AgentVersion string    `json:"agentVersion" db:"agent_version"`
	CreatedAt    time.Time `json:"createdAt" db:"created_at"`
	UpdatedAt    time.Time `json:"updatedAt" db:"updated_at"`
	// Model selection (migration 000013)
	DefaultModel string `json:"defaultModel,omitempty" db:"default_model"`
	// Epic 27a: agent credential state (LEFT JOIN workspace_agent_state)
	AgentNeedsRefresh       bool       `json:"agentNeedsRefresh" db:"agent_needs_refresh"`
	CredentialsPendingSince *time.Time `json:"credentialsPendingSince,omitempty" db:"credentials_pending_since"`
	// Epic 11: org attribution (nullable — personal workspaces have no org)
	OrgID *string `json:"orgId,omitempty" db:"org_id"`
}

// WorkspaceUpdates carries the fields that may be changed on a WorkspaceMetadata record.
type WorkspaceUpdates struct {
	Name         *string `json:"name,omitempty"`
	DefaultModel *string `json:"defaultModel,omitempty"`
}

// WorkspaceConfig is non-sensitive workspace metadata (default model)
// delivered to the pod via the bootstrap HTTP endpoint at boot.
type WorkspaceConfig struct {
	DefaultModel string `json:"defaultModel,omitempty"`
}

// WorkspaceNotFoundError is returned when a workspace cannot be found.
type WorkspaceNotFoundError struct {
	ID string
}

func (e *WorkspaceNotFoundError) Error() string {
	return fmt.Sprintf("workspace %s not found", e.ID)
}

// ActivateWorkspaceResponse is returned by POST /workspaces/:id/activate.
type ActivateWorkspaceResponse struct {
	Resumed   string `json:"resumed"`
	Suspended string `json:"suspended,omitempty"`
}

// RefreshWorkspaceResult is returned by POST /workspaces/:id/refresh-compute.
// It reports the restartGeneration that will trigger a pod rebuild, which
// re-resolves the runtime image to its latest version and applies the
// refreshed resource requests.
type RefreshWorkspaceResult struct {
	RestartGeneration int64 `json:"restartGeneration"`
}
