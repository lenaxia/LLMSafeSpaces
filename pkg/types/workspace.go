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
	// Harness is the bound coding-agent harness (owner ruling
	// 2026-11-16: bind-once, immutable forever — workspaces do NOT
	// switch harnesses). nil = UNBOUND: created without a harness
	// choice; no pod runs until the bind (first session's picker
	// submit, or PUT /workspaces/{id}/harness). "opencode"/"pi" =
	// bound for the workspace's lifetime.
	Harness *string `json:"harness,omitempty"`
	// HarnessOptions is the instance's enabled harness set (the
	// picker's offer list; opencode always present). Read-only
	// context for clients — binding consumes it, but a bound value
	// is unaffected by later enablement changes (immutability
	// protects it).
	HarnessOptions []string `json:"harnessOptions,omitempty"`
	// PhraseID is the #1757 workspace phrase (2 words + 1 digit,
	// fleet-unique) — THE address for display; the UUID stays the
	// internal key. omitempty: absent only pre-backfill.
	PhraseID string `json:"phraseId,omitempty"`
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
	// Harness optionally binds the coding-agent harness at create
	// (owner ruling 2026-11-16: bind-once): "opencode" or "pi", must
	// be in the instance's enabled set (harnessOptions on reads).
	// DEFAULT CHANGE (2026-11-16): omitted no longer implicitly means
	// "opencode" — the workspace is created UNBOUND (no pod until the
	// harness is chosen at first session or via
	// PUT /workspaces/{id}/harness). Pre-existing workspaces were
	// backfilled to explicit opencode by the platform, so existing
	// callers see no change on old workspaces; new omitted-harness
	// creates must complete the bind before first use.
	Harness string `json:"harness,omitempty"`
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
	// PhraseID is the #1757 workspace phrase (2 words + 1 digit,
	// fleet-unique) — THE address for the display surfaces (dashboard,
	// pickers, board refs — completion tracked in #1792); the UUID
	// above stays the internal key. omitempty: absent only pre-backfill.
	// Mirrors WorkspaceMetadata.PhraseID.
	PhraseID string `json:"phraseId,omitempty"`
	// Harness is the bound coding-agent harness (owner ruling
	// 2026-11-16: bind-once, immutable forever). nil = unbound (no
	// pod until the first-session picker submit or an explicit bind);
	// "opencode"/"pi" = bound for life.
	Harness *string `json:"harness,omitempty"`
	// HarnessOptions is the instance's enabled harness set (the
	// picker's offer list; opencode always present — the union
	// invariant). Mirrors Workspace.HarnessOptions / GetWorkspace.
	// MUST be populated by ListWorkspaces: the sidebar evaluates the
	// list it already holds when the user clicks "new session" — a
	// starved item degrades the frontend to the opencode-only fallback
	// and the first-session picker can never render (the 2026-11-18
	// prod incident; same defect class as OrgID #477 and PhraseID
	// #1757 r2).
	HarnessOptions []string `json:"harnessOptions,omitempty"`
	// BindBlocked (#2092): true while this unbound workspace's binds
	// are FAILING to persist — the HarnessBindingPending condition
	// carries reason BindNotPersisted because the cluster's Workspace
	// CRD predates spec.harness (every bind write is pruned). The
	// picker/sidebar renders the degraded hint ("blocked on cluster
	// CRD — operator action required") instead of the unbounded
	// Creating state. omitempty: only ever true; clears on the
	// successful bind.
	BindBlocked bool `json:"bindBlocked,omitempty"`
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
	// AgentdEffectiveDigest / AgentdPendingDigest (#1746): the agentd
	// overlay digest the current pod runs vs. the desired digest not yet
	// applied. Pending set + effective different = "agentd X pending —
	// applies on next refresh". Empty when converged or in legacy
	// baked-in delivery.
	AgentdEffectiveDigest string `json:"agentdEffectiveDigest,omitempty"`
	AgentdPendingDigest   string `json:"agentdPendingDigest,omitempty"`
	// AgentdVersion / AgentdCommit / AgentdBuildTime (RC-A, 2026-10-30
	// incident): the RUNNING agentd's self-reported build identity,
	// scraped from the pod's /v1/healthz by the controller's liveness
	// poll — the same fields the incident needed a manual
	// `curl localhost:4098/v1/healthz` to see. Empty while the pod is
	// unreachable. The AgentdVersionStale condition (in Conditions)
	// flags a version older than the fleet release.
	AgentdVersion   string `json:"agentdVersion,omitempty"`
	AgentdCommit    string `json:"agentdCommit,omitempty"`
	AgentdBuildTime string `json:"agentdBuildTime,omitempty"`
	// Supervisor (#1760): the supervisor/watchdog agent-restart slice —
	// lifetime restart count, the last restart's attribution
	// (reason/verdict/lastError/at), and the suppression count. Nil when
	// the workspace has no restart evidence (never restarted, or a
	// pre-#1760 runtime).
	Supervisor *SupervisorStatusResult `json:"supervisor,omitempty"`
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
	// PhraseID is the #1757 workspace phrase (2 words + 1 digit,
	// fleet-unique) — THE address for display and cross-workspace
	// referencing; the UUID above stays the internal key. omitempty:
	// absent only in the pre-backfill window (converges on the next
	// authorized read).
	PhraseID string `json:"phraseId,omitempty" db:"phrase_id"`
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

// BindHarnessRequest is the body for PUT /workspaces/:id/harness — the
// explicit bind of an UNBOUND workspace's harness (owner ruling
// 2026-11-16: bind-once). On an already-bound workspace the same value
// is an idempotent no-op; any other value is a 409 harness_immutable
// (there is no switch or recreate path at any tier).
type BindHarnessRequest struct {
	// Harness to bind: "opencode" or "pi", must be in the instance's
	// enabled set.
	Harness string `json:"harness"`
}

// BindHarnessResult is the response for PUT /workspaces/:id/harness.
type BindHarnessResult struct {
	// Harness is the workspace's harness after the call (bound value,
	// or the pre-existing value on the idempotent path).
	Harness string `json:"harness"`
	// AlreadyBound is true when the workspace was already bound to
	// this value and nothing changed (the idempotent 200 path).
	AlreadyBound bool `json:"alreadyBound,omitempty"`
	// HarnessOptions echoes the instance's enabled set (picker context).
	HarnessOptions []string `json:"harnessOptions,omitempty"`
}
