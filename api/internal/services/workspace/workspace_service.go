// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
	apiinterfaces "github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/settings"
	"github.com/lenaxia/llmsafespaces/pkg/types"

	"github.com/google/uuid"
)

func init() {
	opencode.Register()
}

// WorkspaceConfig is non-sensitive workspace metadata persisted for pod boot.
// Re-exported from pkg/types to avoid requiring callers to import both.
type WorkspaceConfig = types.WorkspaceConfig

// Service implements apiinterfaces.WorkspaceService.
type Service struct {
	logger             pkginterfaces.LoggerInterface
	k8sClient          pkginterfaces.KubernetesClient
	dbService          apiinterfaces.DatabaseService
	cacheService       apiinterfaces.CacheService
	metricsService     apiinterfaces.MetricsService
	sessionIndex       apiinterfaces.SessionIndexService
	credProvisioner    CredentialProvisioner
	secretProvisioner  SecretAutoProvisioner
	instanceSettings   *settings.InstanceService
	orgStore           OrgMembershipChecker
	policyChecker      PolicyChecker
	podIdentityTracker PodIdentityTracker
	secretPusher       SecretPusher
	config             *Config
}

type OrgMembershipChecker interface {
	IsOrgMember(ctx context.Context, orgID, userID string) (bool, error)
	IsOrgAdmin(ctx context.Context, orgID, userID string) (bool, error)
	// GetUserOrgID returns the user's single org ID (or "" if not in any org).
	// Used by CreateWorkspace for D4 auto-attribution.
	GetUserOrgID(ctx context.Context, userID string) (string, error)
}

// PolicyChecker reads the effective org policy for enforcement. The policy
// service implements this; nil means no policy enforcement (dev/test).
type PolicyChecker interface {
	GetEffectivePolicy(ctx context.Context, orgID string) (*types.OrgPolicyValues, error)
}

func (s *Service) SetOrgStore(store OrgMembershipChecker) {
	s.orgStore = store
}

// SetPolicyChecker installs the org policy checker for workspace quota enforcement.
func (s *Service) SetPolicyChecker(checker PolicyChecker) {
	s.policyChecker = checker
}

// CredentialProvisioner seeds workspace_credential_bindings from credential_auto_apply.
type CredentialProvisioner interface {
	SeedWorkspaceCredentials(ctx context.Context, workspaceID, userID string, orgID *string) error
}

// SecretAutoProvisioner seeds user_secret_bindings from user_secrets where
// global_default=true. Called on workspace creation as a best-effort operation.
type SecretAutoProvisioner interface {
	SeedGlobalDefaultSecrets(ctx context.Context, workspaceID, userID string) error
}

// PodIdentityTracker persists the (podName, podStartTime) tuple last
// observed by the API for a workspace and reports whether the current
// tuple represents a transition. Satisfied by *database.Service via the
// GetLastSeenPodIdentity / UpsertLastSeenPodIdentity /
// MarkPodIdentityTransition / ClearPendingRefreshAfterAutoPush methods.
// See worklog 0589 for the design.
//
// The interface is narrow (four methods) on purpose: the workspace
// service does not need — and should not depend on — the ~40 methods
// on the full DatabaseService interface. Narrow interfaces at consumers
// is the SOLID DIP pattern used across this codebase for other
// optional deps (e.g. CredentialStateWriter on SecretsHandler).
type PodIdentityTracker interface {
	// GetLastSeenPodIdentity returns the previously-observed (name, startTime)
	// tuple, or ("", zero-time, nil error) if no observation has been recorded
	// yet. Callers treat the zero tuple as "initial observation" and MUST NOT
	// trigger an auto-push — instead they persist the current tuple via
	// UpsertLastSeenPodIdentity so subsequent transitions can be detected.
	GetLastSeenPodIdentity(ctx context.Context, workspaceID string) (string, time.Time, error)
	// UpsertLastSeenPodIdentity persists the current identity WITHOUT touching
	// pending_refresh. Used for the initial-observation write.
	UpsertLastSeenPodIdentity(ctx context.Context, workspaceID, podName string, startTime time.Time) error
	// MarkPodIdentityTransition persists the NEW identity AND flips
	// pending_refresh=TRUE with last_credential_changed_at=NOW(). The atomic
	// batched write is important: pending_refresh must be TRUE before the
	// caller fires the fire-and-forget push goroutine so a concurrent
	// GetWorkspace list-read surfaces agentNeedsRefresh (the fallback banner UX).
	//
	// Returns the DB-clock timestamp written to last_credential_changed_at
	// so the caller can round-trip it back through ClearPendingRefreshAfterAutoPush
	// to correctly interact with MarkAgentReloaded's optimistic-concurrency
	// check (a bind arriving DURING the push window must keep pending_refresh=TRUE).
	MarkPodIdentityTransition(ctx context.Context, workspaceID, podName string, startTime time.Time) (time.Time, error)
	// ClearPendingRefreshAfterAutoPush flips pending_refresh=FALSE after a
	// successful auto-push, UNLESS a new credential was staged during the
	// push window (in which case the flag stays TRUE and the banner
	// correctly re-appears for the fresh change). Wraps MarkAgentReloaded
	// in a self-contained transaction so the fire-and-forget goroutine
	// doesn't need to manage a *sql.Tx. Returns the disposed-at timestamp
	// (unused by the workspace-service caller today, but kept in the
	// signature to match *database.Service's method exactly so the
	// concrete implementation satisfies the interface without an adapter).
	ClearPendingRefreshAfterAutoPush(ctx context.Context, workspaceID string, priorChangedAt time.Time) (time.Time, error)
}

// SecretPusher decrypts the user's bound secret snapshot with their DEK
// (extracted from ctx) and posts it to the running workspace pod's
// agentd. Satisfied by *agentpush.Service. The workspace service depends
// on this interface, not the concrete type, so tests can inject a fake.
type SecretPusher interface {
	Push(ctx context.Context, userID, workspaceID string) error
}

// SetPodIdentityTracker installs the pod-identity persistence layer. If
// nil, GetWorkspaceStatus skips the detection logic entirely (dev/test).
func (s *Service) SetPodIdentityTracker(t PodIdentityTracker) {
	s.podIdentityTracker = t
}

// SetSecretPusher installs the auto-push service. If nil, transitions
// are still recorded but no push fires (dev/test).
//
// Metric emission for auto-push outcomes is the pusher's responsibility
// (agentpush.WithMetricsHook), not the workspace service's — otherwise
// each successful push would increment the same counter twice, and the
// two increment sites could get out of sync as the pusher grows more
// outcomes over time (e.g. rate-limited, quota-exceeded).
func (s *Service) SetSecretPusher(p SecretPusher) {
	s.secretPusher = p
}

// SetCredentialProvisioner installs the credential auto-apply seeder.
func (s *Service) SetCredentialProvisioner(cp CredentialProvisioner) {
	s.credProvisioner = cp
}

// SetSecretAutoProvisioner installs the global-default secret seeder.
func (s *Service) SetSecretAutoProvisioner(sp SecretAutoProvisioner) {
	s.secretProvisioner = sp
}

func (s *Service) workspaceCRDClient() (pkginterfaces.WorkspaceInterface, error) {
	v1Client, err := s.k8sClient.LlmsafespacesV1()
	if err != nil {
		return nil, fmt.Errorf("initialize LLMSafespacesV1 client: %w", err)
	}
	return v1Client.Workspaces(s.config.Namespace), nil
}

// markDeleted soft-deletes a workspace metadata row in the background.
// It accepts a context for symmetry with the caller and to silence
// contextcheck, but deliberately does NOT propagate it: the caller is
// typically a request goroutine whose context gets canceled as soon
// as the HTTP response is flushed, which would race with the marker
// write. context.Background inside the goroutine is correct and
// intentional. The ctx parameter is currently unused; it exists so
// future tracing/observability can be plumbed without changing every
// call site.
func (s *Service) markDeleted(_ context.Context, workspaceID string) {
	db := s.dbService
	if db == nil {
		return
	}
	//nolint:gosec,contextcheck // G118: intentional fresh context for fire-and-forget cleanup; see godoc above
	go func() {
		defer func() { _ = recover() }()
		db.MarkWorkspaceDeleted(context.Background(), workspaceID)
	}()
}

// Config holds workspace service configuration.
type Config struct {
	Namespace    string
	OpencodePort int // Port for opencode on sandbox pods. Default: 4096.
}

var _ apiinterfaces.WorkspaceService = (*Service)(nil)

// New creates a validated workspace service. config may be nil to use defaults.
func New(
	logger pkginterfaces.LoggerInterface,
	k8sClient pkginterfaces.KubernetesClient,
	dbService apiinterfaces.DatabaseService,
	cacheService apiinterfaces.CacheService,
	metricsService apiinterfaces.MetricsService,
	config *Config,
) (*Service, error) {
	if logger == nil {
		return nil, fmt.Errorf("logger cannot be nil")
	}
	if k8sClient == nil {
		return nil, fmt.Errorf("kubernetes client cannot be nil")
	}
	if dbService == nil {
		return nil, fmt.Errorf("database service cannot be nil")
	}
	if config == nil {
		config = &Config{Namespace: "default"}
	}
	return &Service{
		logger:         logger,
		k8sClient:      k8sClient,
		dbService:      dbService,
		cacheService:   cacheService,
		metricsService: metricsService,
		config:         config,
	}, nil
}

// SetSessionIndex injects the session index service. Optional — nil disables session tracking.
func (s *Service) SetSessionIndex(si apiinterfaces.SessionIndexService) {
	s.sessionIndex = si
}

func (s *Service) Start() error {
	s.logger.Info("Starting workspace service")
	return nil
}

func (s *Service) Stop() error {
	s.logger.Info("Stopping workspace service")
	return nil
}

// CreateWorkspace validates the request, creates a Workspace CRD, and persists
// metadata to the database. On database failure the CRD is deleted.
func (s *Service) CreateWorkspace(ctx context.Context, userID string, req types.CreateWorkspaceRequest) (*types.Workspace, error) {
	start := time.Now()
	defer func() {
		if s.metricsService != nil {
			s.metricsService.RecordRequest("CreateWorkspace", "", 0, time.Since(start), 0)
		}
	}()

	if req.Name == "" {
		return nil, apierrors.NewValidationError(
			"workspace name is required",
			map[string]interface{}{"field": "name"},
			fmt.Errorf("name is empty"),
		)
	}
	if req.StorageSize == "" && s.instanceSettings != nil {
		if size, err := s.instanceSettings.DefaultStorageSize(ctx); err == nil && size != "" {
			req.StorageSize = size
		}
	}
	if req.StorageSize == "" {
		return nil, apierrors.NewValidationError(
			"storage size is required",
			map[string]interface{}{"field": "storageSize"},
			fmt.Errorf("storageSize is empty"),
		)
	}

	// D4: workspace auto-attribution. When the user is in an org and did not
	// supply OrgID, auto-attribute the workspace to their org. Users cannot
	// create personal workspaces while part of an org. Non-org users get
	// personal workspaces (org_id stays nil).
	if (req.OrgID == nil || *req.OrgID == "") && s.orgStore != nil {
		userOrgID, err := s.orgStore.GetUserOrgID(ctx, userID)
		if err != nil {
			s.logger.Warn("Failed to look up user org for auto-attribution", "userID", userID, "error", err)
			// Non-fatal: proceed as a personal workspace.
		} else if userOrgID != "" {
			req.OrgID = &userOrgID
		}
	}

	if req.OrgID != nil && *req.OrgID != "" {
		if s.orgStore == nil {
			return nil, apierrors.NewValidationError(
				"org support not configured",
				map[string]interface{}{"field": "orgId"},
				fmt.Errorf("org store not available"),
			)
		}
		isMember, err := s.orgStore.IsOrgMember(ctx, *req.OrgID, userID)
		if err != nil {
			return nil, apierrors.NewInternalError("org_membership_check_failed", err)
		}
		if !isMember {
			return nil, apierrors.NewForbiddenError("user is not a member of the specified org", fmt.Errorf("user %s is not a member of org %s", userID, *req.OrgID))
		}

		// US-43.8: Enforce org workspace quotas.
		if s.policyChecker != nil {
			pol, err := s.policyChecker.GetEffectivePolicy(ctx, *req.OrgID)
			if err != nil {
				return nil, apierrors.NewInternalError("policy_check_failed", err)
			}
			if pol != nil {
				if maxTotal := pol.MaxWorkspaces(); maxTotal >= 0 {
					count, err := s.dbService.CountWorkspacesByUserAndOrg(ctx, userID, *req.OrgID)
					if err != nil {
						return nil, apierrors.NewInternalError("workspace_count_failed", err)
					}
					if count >= maxTotal {
						return nil, apierrors.NewValidationError(
							fmt.Sprintf("workspace quota exceeded: you have %d of %d allowed workspaces in this org", count, maxTotal),
							map[string]interface{}{"policy": "max_workspaces_per_member"},
							fmt.Errorf("quota exceeded: %d >= %d", count, maxTotal),
						)
					}
				}
				if maxActive := pol.MaxActive(); maxActive >= 0 {
					active, err := s.dbService.CountActiveWorkspacesByUserAndOrg(ctx, userID, *req.OrgID)
					if err != nil {
						return nil, apierrors.NewInternalError("active_workspace_count_failed", err)
					}
					if active >= maxActive {
						return nil, apierrors.NewValidationError(
							fmt.Sprintf("active workspace quota exceeded: you have %d of %d concurrent active workspaces", active, maxActive),
							map[string]interface{}{"policy": "max_active_workspaces_per_member"},
							fmt.Errorf("active quota exceeded: %d >= %d", active, maxActive),
						)
					}
				}
			}
		}
	}

	// Apply default runtime from settings if not specified
	if req.Runtime == "" && s.instanceSettings != nil {
		if img, err := s.instanceSettings.GetString(ctx, settings.KeyWorkspaceDefaultImage.Name()); err == nil && img != "" {
			req.Runtime = img
		}
	}

	workspaceID := uuid.New().String()

	crd := buildWorkspaceCRD(workspaceID, userID, req, s.config.Namespace)

	// Apply defaults from instance settings to the CRD spec
	s.applyWorkspaceDefaults(ctx, crd)

	s.logger.Info("Creating workspace in Kubernetes", "userID", userID, "name", req.Name)

	created, err := func() (*v1.Workspace, error) {
		wsClient, err := s.workspaceCRDClient()
		if err != nil {
			return nil, err
		}
		return wsClient.Create(ctx, crd)
	}()
	if err != nil {
		s.logger.Error("Failed to create workspace in Kubernetes", err, "userID", userID)
		return nil, apierrors.NewInternalError("workspace_creation_failed", err)
	}

	meta := &types.WorkspaceMetadata{
		ID:          created.Name,
		UserID:      userID,
		Name:        req.Name,
		Runtime:     req.Runtime,
		StorageSize: req.StorageSize,
		OrgID:       req.OrgID,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	if err := s.dbService.CreateWorkspace(ctx, meta); err != nil {
		s.logger.Error("Failed to store workspace metadata", err, "workspaceID", created.Name)
		if delErr := func() error {
			wsClient, wErr := s.workspaceCRDClient()
			if wErr != nil {
				return wErr
			}
			return wsClient.Delete(ctx, created.Name, metav1.DeleteOptions{})
		}(); delErr != nil {
			s.logger.Error("Failed to clean up workspace after metadata error", delErr, "workspaceID", created.Name)
		}
		return nil, apierrors.NewInternalError("metadata_creation_failed", err)
	}

	s.logger.Info("Workspace created", "workspaceID", created.Name, "userID", userID)

	// Auto-provision default credentials if enabled (Epic 30).
	// Seeding is best-effort: a failure does NOT roll back workspace creation,
	// but is logged at Error (not Warn) so it surfaces in alerting dashboards.
	if s.credProvisioner != nil {
		if err := s.credProvisioner.SeedWorkspaceCredentials(ctx, meta.ID, userID, meta.OrgID); err != nil {
			s.logger.Error("credential seeding failed for new workspace; it will have no LLM credentials",
				err, "workspaceID", meta.ID, "userID", userID)
		}
	}

	// Auto-bind global-default secrets if any exist for this user.
	// Best-effort: a failure does NOT roll back workspace creation.
	if s.secretProvisioner != nil {
		if err := s.secretProvisioner.SeedGlobalDefaultSecrets(ctx, meta.ID, userID); err != nil {
			s.logger.Error("global-default secret seeding failed for new workspace",
				err, "workspaceID", meta.ID, "userID", userID)
		}
	}

	// Epic 35: secrets are no longer pre-written to a K8s Secret — the init
	// container fetches them from the API via the bootstrap endpoint at pod
	// boot. No action needed here.

	ws := &types.Workspace{
		ID:          meta.ID,
		Name:        meta.Name,
		UserID:      meta.UserID,
		Runtime:     meta.Runtime,
		StorageSize: meta.StorageSize,
		Phase:       string(created.Status.Phase),
		CreatedAt:   meta.CreatedAt,
		UpdatedAt:   meta.UpdatedAt,
	}

	return ws, nil
}

// GetWorkspace retrieves a workspace by ID, verifying owner.
func (s *Service) GetWorkspace(ctx context.Context, userID, workspaceID string) (*types.Workspace, error) {
	start := time.Now()
	defer func() {
		if s.metricsService != nil {
			s.metricsService.RecordRequest("GetWorkspace", "", 0, time.Since(start), 0)
		}
	}()

	meta, err := s.dbService.GetWorkspace(ctx, workspaceID)
	if err != nil {
		s.logger.Error("Failed to retrieve workspace", err, "workspaceID", workspaceID)
		return nil, apierrors.NewInternalError("workspace_retrieval_failed", err)
	}
	if meta == nil {
		return nil, apierrors.NewNotFoundError("workspace", workspaceID, fmt.Errorf("workspace not found"))
	}
	if err := s.verifyOwner(ctx, userID, workspaceID); err != nil {
		return nil, err
	}

	crd, err := func() (*v1.Workspace, error) {
		wsClient, wErr := s.workspaceCRDClient()
		if wErr != nil {
			return nil, wErr
		}
		return wsClient.Get(ctx, workspaceID, metav1.GetOptions{})
	}()
	if err != nil {
		if k8serrors.IsNotFound(err) {
			s.markDeleted(ctx, workspaceID)
			crd = nil
		} else {
			s.logger.Warn("Failed to get workspace CRD status", "error", err, "workspaceID", workspaceID)
			crd = nil
		}
	}

	ws := &types.Workspace{
		ID:                      meta.ID,
		Name:                    meta.Name,
		UserID:                  meta.UserID,
		Runtime:                 meta.Runtime,
		StorageSize:             meta.StorageSize,
		CreatedAt:               meta.CreatedAt,
		UpdatedAt:               meta.UpdatedAt,
		DefaultModel:            meta.DefaultModel,
		AgentNeedsRefresh:       meta.AgentNeedsRefresh,
		CredentialsPendingSince: meta.CredentialsPendingSince,
	}
	if crd != nil {
		ws.Phase = string(crd.Status.Phase)
		ws.PVCName = crd.Status.PVCName
	}

	return ws, nil
}

// ListWorkspaces returns workspace metadata for a user with pagination.
func (s *Service) ListWorkspaces(ctx context.Context, userID string, opts types.ListOptions) (*types.WorkspaceListResult, error) {
	start := time.Now()
	defer func() {
		if s.metricsService != nil {
			s.metricsService.RecordRequest("ListWorkspaces", "", 0, time.Since(start), 0)
		}
	}()

	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}

	metas, pagination, err := s.dbService.ListWorkspaces(ctx, userID, limit, opts.Offset)
	if err != nil {
		s.logger.Error("Failed to list workspaces", err, "userID", userID)
		return nil, apierrors.NewInternalError("workspace_list_failed", err)
	}

	// Phase is owned by the Workspace CRD; the DB only stores immutable
	// metadata. We enrich the list with phase via a single label-scoped LIST.
	// On k8s error the items are returned with empty phase; the platform is
	// already unusable in that scenario (every other operation hits the
	// kube-apiserver too) so there's nothing meaningful to fall back to.

	items := make([]types.WorkspaceListItem, 0, len(metas))
	phases := s.fetchUserWorkspacePhases(ctx, userID)
	for _, m := range metas {
		items = append(items, types.WorkspaceListItem{
			ID:                      m.ID,
			Name:                    m.Name,
			UserID:                  m.UserID,
			Runtime:                 m.Runtime,
			StorageSize:             m.StorageSize,
			Phase:                   phases[m.ID],
			ImageTag:                m.ImageTag,
			AgentVersion:            m.AgentVersion,
			CreatedAt:               m.CreatedAt,
			UpdatedAt:               m.UpdatedAt,
			DefaultModel:            m.DefaultModel,
			AgentNeedsRefresh:       m.AgentNeedsRefresh,
			CredentialsPendingSince: m.CredentialsPendingSince,
			// Epic 11: org attribution. The frontend's Workspace Settings
			// drawer uses this to fetch the owning org's allow_user_prompt
			// policy and gate the Custom Instructions Lock UI.
			OrgID: m.OrgID,
		})
	}

	return &types.WorkspaceListResult{Items: items, Pagination: pagination}, nil
}

// fetchUserWorkspacePhases returns id -> phase for the user's workspaces by
// listing CRDs filtered with the user-id label. Returns nil on k8s error so
// callers degrade gracefully (empty phase is propagated to the API response).
// A nil map is safe to read from in Go.
func (s *Service) fetchUserWorkspacePhases(ctx context.Context, userID string) map[string]string {
	if s.k8sClient == nil || userID == "" {
		return nil
	}
	list, err := func() (*v1.WorkspaceList, error) {
		wsClient, wErr := s.workspaceCRDClient()
		if wErr != nil {
			return nil, wErr
		}
		return wsClient.List(ctx, metav1.ListOptions{
			LabelSelector: "user-id=" + userID,
		})
	}()
	if err != nil {
		s.logger.Warn("Failed to list workspaces from CRDs for phase enrichment",
			"userID", userID, "error", err.Error())
		return nil
	}
	out := make(map[string]string, len(list.Items))
	for i := range list.Items {
		w := &list.Items[i]
		out[w.Name] = string(w.Status.Phase)
	}
	return out
}

// DeleteWorkspace marks a workspace as terminating and deletes the CRD.
func (s *Service) DeleteWorkspace(ctx context.Context, userID, workspaceID string) error {
	start := time.Now()
	defer func() {
		if s.metricsService != nil {
			s.metricsService.RecordRequest("DeleteWorkspace", "", 0, time.Since(start), 0)
		}
	}()

	if err := s.verifyOwner(ctx, userID, workspaceID); err != nil {
		return err
	}

	if err := func() error {
		wsClient, wErr := s.workspaceCRDClient()
		if wErr != nil {
			return wErr
		}
		return wsClient.Delete(ctx, workspaceID, metav1.DeleteOptions{})
	}(); err != nil && !k8serrors.IsNotFound(err) {
		s.logger.Error("Failed to delete workspace CRD", err, "workspaceID", workspaceID)
		return apierrors.NewInternalError("workspace_deletion_failed", err)
	}

	s.markDeleted(ctx, workspaceID)

	s.logger.Info("Workspace deleted", "workspaceID", workspaceID, "userID", userID)
	return nil
}

// SuspendWorkspace transitions a workspace to Suspending phase.
func (s *Service) SuspendWorkspace(ctx context.Context, userID, workspaceID string) error {
	start := time.Now()
	defer func() {
		if s.metricsService != nil {
			s.metricsService.RecordRequest("SuspendWorkspace", "", 0, time.Since(start), 0)
		}
	}()

	if err := s.verifyOwner(ctx, userID, workspaceID); err != nil {
		return err
	}

	crd, err := func() (*v1.Workspace, error) {
		wsClient, wErr := s.workspaceCRDClient()
		if wErr != nil {
			return nil, wErr
		}
		return wsClient.Get(ctx, workspaceID, metav1.GetOptions{})
	}()
	if err != nil {
		return apierrors.NewInternalError("workspace_get_failed", err)
	}

	if crd.Status.Phase == v1.WorkspacePhaseSuspended || crd.Status.Phase == v1.WorkspacePhaseSuspending {
		return nil
	}

	if crd.Status.Phase != v1.WorkspacePhaseActive {
		return apierrors.NewConflictError(
			"workspace",
			workspaceID,
			fmt.Errorf("cannot suspend workspace in phase %q", crd.Status.Phase),
		)
	}

	// US-23.3: write Spec.Suspend instead of Status.Phase. The controller
	// observes the spec change in handleActive and transitions Phase.
	// This makes the controller the sole writer of Status.Phase.
	// Pointer semantics: non-nil true is a distinct state from nil
	// (unspecified) so pre-migration workspaces are not auto-resumed.
	suspendTrue := true
	crd.Spec.Suspend = &suspendTrue
	if _, err := func() (*v1.Workspace, error) {
		wsClient, wErr := s.workspaceCRDClient()
		if wErr != nil {
			return nil, wErr
		}
		return wsClient.Update(ctx, crd)
	}(); err != nil {
		s.logger.Error("Failed to set Spec.Suspend=true", err, "workspaceID", workspaceID)
		return apierrors.NewInternalError("workspace_suspend_failed", err)
	}

	s.logger.Info("Workspace suspend initiated", "workspaceID", workspaceID, "userID", userID)
	return nil
}

// NeutralizeUserWorkspaces suspends every Active workspace owned by
// userID and best-effort deletes any legacy workspace-secrets-<id> K8s
// Secret for each. Post-Epic-35, workspaces no longer create this Secret
// (secretless injection), so this is an upgrade-path cleanup no-op for
// post-Epic-35 workspaces. For pre-Epic-35 workspaces with a leftover
// Secret, it ensures the plaintext is scrubbed.
//
// Suspend is best-effort: workspaces not in the Active phase (Creating,
// Resuming, already Suspended, ...) are skipped without aborting the
// loop, and individual failures are logged. The Secret scrub runs for
// every workspace regardless of phase and ignores NotFound.
// A nil k8sClient (dev/test) makes the scrub a no-op.
func (s *Service) NeutralizeUserWorkspaces(ctx context.Context, userID string) error {
	phases := s.fetchUserWorkspacePhases(ctx, userID)
	for wsID := range phases {
		if err := s.SuspendWorkspace(ctx, userID, wsID); err != nil {
			// A conflict means the workspace is not Active (Creating,
			// Resuming, Suspended, Terminating, ...). That is expected
			// during a bulk sweep and must not be noisy. Anything else
			// is logged so an operator sees real degradation.
			var apiErr *apierrors.APIError
			if !errors.As(err, &apiErr) || apiErr.Type != apierrors.ErrorTypeConflict {
				s.logger.Warn("neutralize: suspend workspace failed",
					"workspaceID", wsID, "error", err.Error())
			}
		}
		if err := s.deleteWorkspaceSecretsManifest(ctx, wsID); err != nil && !k8serrors.IsNotFound(err) {
			s.logger.Warn("neutralize: scrub secrets manifest failed",
				"workspaceID", wsID, "error", err.Error())
		}
	}
	return nil
}

// deleteWorkspaceSecretsManifest deletes a legacy workspace-secrets-<id>
// K8s Secret if one exists (upgrade-path cleanup; post-Epic-35
// workspaces never create it).
func (s *Service) deleteWorkspaceSecretsManifest(ctx context.Context, workspaceID string) error {
	if s.k8sClient == nil {
		return nil
	}
	secretName := fmt.Sprintf("workspace-secrets-%s", workspaceID)
	clientset := s.k8sClient.Clientset()
	return clientset.CoreV1().Secrets(s.config.Namespace).Delete(ctx, secretName, metav1.DeleteOptions{})
}

// RestartWorkspace bumps spec.restartGeneration so the controller's
// handleFailed (Epic 21 Change A) or handleActive recovery paths walk
// the workspace back through Pending and rebuild the pod from scratch.
//
// Use cases:
//   - Recover a Failed workspace (the original motivation; previously
//     required `kubectl patch --subresource=status`).
//   - Force-restart a stuck Active workspace whose agent is hung but
//     the controller hasn't yet exhausted its transient-failure budget.
//
// Restart is REJECTED for Terminating/Terminated phases — those are
// genuinely terminal and would race with finalizer cleanup. For all
// other phases the call is idempotent at the spec layer (each call
// bumps the field by 1; the controller responds to each bump exactly
// once via the strict-greater-than check on observedRestartGeneration).
//
// Epic 35: the pod's init container fetches credentials via the bootstrap
// endpoint at boot — no pre-writing of a K8s Secret is needed.
func (s *Service) RestartWorkspace(ctx context.Context, userID, workspaceID string) error {
	start := time.Now()
	defer func() {
		if s.metricsService != nil {
			s.metricsService.RecordRequest("RestartWorkspace", "", 0, time.Since(start), 0)
		}
	}()

	if err := s.verifyOwner(ctx, userID, workspaceID); err != nil {
		return err
	}

	crd, err := func() (*v1.Workspace, error) {
		wsClient, wErr := s.workspaceCRDClient()
		if wErr != nil {
			return nil, wErr
		}
		return wsClient.Get(ctx, workspaceID, metav1.GetOptions{})
	}()
	if err != nil {
		return apierrors.NewInternalError("workspace_get_failed", err)
	}

	if crd.Status.Phase == v1.WorkspacePhaseTerminating || crd.Status.Phase == v1.WorkspacePhaseTerminated {
		return apierrors.NewConflictError(
			"workspace",
			workspaceID,
			fmt.Errorf("cannot restart workspace in phase %q", crd.Status.Phase),
		)
	}

	crd.Spec.RestartGeneration++
	if _, err := func() (*v1.Workspace, error) {
		wsClient, wErr := s.workspaceCRDClient()
		if wErr != nil {
			return nil, wErr
		}
		return wsClient.Update(ctx, crd)
	}(); err != nil {
		s.logger.Error("Failed to bump RestartGeneration", err, "workspaceID", workspaceID)
		return apierrors.NewInternalError("workspace_restart_failed", err)
	}

	s.logger.Info("Workspace restart initiated",
		"workspaceID", workspaceID, "userID", userID,
		"restartGeneration", crd.Spec.RestartGeneration, "fromPhase", string(crd.Status.Phase))
	return nil
}

// RefreshWorkspaceCompute re-syncs a workspace CRD with the platform's current
// defaults, then bumps spec.restartGeneration so the controller rebuilds the
// pod. The rebuild re-resolves spec.runtime to the latest RuntimeEnvironment
// image (picking up new image versions) and applies the refreshed resource
// requests.
//
// Use cases:
//   - A new runtime image version is published; the user wants the workspace
//     to pick it up without a full recreate.
//   - The platform's default CPU/memory (workspace.defaultResources) increased
//     and the user wants their long-lived workspace to adopt the new values.
//
// Fields re-applied from instance settings (overwritten to current platform
// defaults when configured): Resources.CPU, Resources.Memory, SecurityLevel,
// Storage.StorageClassName, MaxActiveSessions. Fields the platform has no
// opinion on (empty/zero default) are left untouched, so refresh never
// clobbers a deliberate user setting with a schema default.
//
// Like RestartWorkspace, refresh is REJECTED for Terminating/Terminated phases
// (they race with finalizer cleanup) and idempotent at the spec layer.
//
// Suspended workspaces have no pod, and handleSuspended only observes
// spec.suspend (NOT restartGeneration) — so a generation bump alone would be a
// no-op. When the workspace is Suspended, refresh also requests a resume via
// ActivateWorkspace (which enforces the active-workspace cap and writes
// spec.suspend=false), so the controller builds a fresh pod carrying the
// refreshed spec. If the resume fails after the spec refresh has persisted,
// the error is returned but the config update remains in effect for the next
// manual activate.
func (s *Service) RefreshWorkspaceCompute(ctx context.Context, userID, workspaceID string) (*types.RefreshWorkspaceResult, error) {
	start := time.Now()
	defer func() {
		if s.metricsService != nil {
			s.metricsService.RecordRequest("RefreshWorkspaceCompute", "", 0, time.Since(start), 0)
		}
	}()

	if err := s.verifyOwner(ctx, userID, workspaceID); err != nil {
		return nil, err
	}

	crd, err := func() (*v1.Workspace, error) {
		wsClient, wErr := s.workspaceCRDClient()
		if wErr != nil {
			return nil, wErr
		}
		return wsClient.Get(ctx, workspaceID, metav1.GetOptions{})
	}()
	if err != nil {
		return nil, apierrors.NewInternalError("workspace_get_failed", err)
	}

	if crd.Status.Phase == v1.WorkspacePhaseTerminating || crd.Status.Phase == v1.WorkspacePhaseTerminated {
		return nil, apierrors.NewConflictError(
			"workspace",
			workspaceID,
			fmt.Errorf("cannot refresh compute for workspace in phase %q", crd.Status.Phase),
		)
	}

	wasSuspended := crd.Status.Phase == v1.WorkspacePhaseSuspended

	s.reapplyComputeDefaults(ctx, crd)
	crd.Spec.RestartGeneration++

	if _, err := func() (*v1.Workspace, error) {
		wsClient, wErr := s.workspaceCRDClient()
		if wErr != nil {
			return nil, wErr
		}
		return wsClient.Update(ctx, crd)
	}(); err != nil {
		s.logger.Error("Failed to refresh workspace compute", err, "workspaceID", workspaceID)
		return nil, apierrors.NewInternalError("workspace_refresh_failed", err)
	}

	// A suspended workspace has no pod; the restartGeneration bump is invisible
	// to handleSuspended (it watches only spec.suspend). Resume so the
	// controller builds a fresh pod from the refreshed spec. ActivateWorkspace
	// enforces the active-workspace cap exactly like a manual resume.
	if wasSuspended {
		if _, err := s.ActivateWorkspace(ctx, userID, workspaceID); err != nil {
			s.logger.Error("Refresh applied defaults but resume failed", err, "workspaceID", workspaceID)
			return nil, err
		}
	}

	s.logger.Info("Workspace compute refresh initiated",
		"workspaceID", workspaceID, "userID", userID,
		"restartGeneration", crd.Spec.RestartGeneration, "fromPhase", string(crd.Status.Phase),
		"resumed", wasSuspended)
	return &types.RefreshWorkspaceResult{RestartGeneration: crd.Spec.RestartGeneration}, nil
}

// reapplyComputeDefaults overwrites the compute-oriented spec fields with the
// platform's current instance settings. Unlike applyWorkspaceDefaults (which
// only fills empty fields), this force-converges the workspace to the current
// platform defaults — the intent of "refresh compute".
//
// The settings service returns schema defaults for unconfigured keys (CPU
// "500m", memory "1Gi", securityLevel "standard", maxActiveSessions 5 — from
// schema.go), so refresh converges resources to the platform default even when
// an admin hasn't explicitly overridden them. StorageClass has an empty schema
// default, so it is only overwritten when explicitly configured — refresh
// never forces a storage class the platform has no opinion on.
func (s *Service) reapplyComputeDefaults(ctx context.Context, crd *v1.Workspace) {
	if s.instanceSettings == nil {
		return
	}

	cpu, cpuErr := s.instanceSettings.GetString(ctx, settings.KeyWorkspaceDefaultResourcesCPU.Name())
	mem, memErr := s.instanceSettings.GetString(ctx, settings.KeyWorkspaceDefaultResourcesMemory.Name())
	if cpuOk := cpuErr == nil && cpu != ""; cpuOk || (memErr == nil && mem != "") {
		if crd.Spec.Resources == nil {
			crd.Spec.Resources = &v1.ResourceRequirements{}
		}
		if cpuOk {
			crd.Spec.Resources.CPU = cpu
		}
		if memErr == nil && mem != "" {
			crd.Spec.Resources.Memory = mem
		}
	}

	if level, err := s.instanceSettings.GetString(ctx, settings.KeyWorkspaceDefaultSecurityLevel.Name()); err == nil && level != "" {
		crd.Spec.SecurityLevel = level
	}

	if sc, err := s.instanceSettings.DefaultStorageClass(ctx); err == nil && sc != "" {
		crd.Spec.Storage.StorageClassName = sc
	}

	if v, err := s.instanceSettings.GetInt(ctx, settings.KeyWorkspaceDefaultMaxActiveSessions.Name()); err == nil && v > 0 {
		crd.Spec.MaxActiveSessions = int32(v) //nolint:gosec // bounded by settings schema (1-100)
	}
}

// GetWorkspaceStatus returns infrastructure state from the Workspace CRD.
func (s *Service) GetWorkspaceStatus(ctx context.Context, userID, workspaceID string) (*types.WorkspaceStatusResult, error) {
	start := time.Now()
	defer func() {
		if s.metricsService != nil {
			s.metricsService.RecordRequest("GetWorkspaceStatus", "", 0, time.Since(start), 0)
		}
	}()

	if err := s.verifyOwner(ctx, userID, workspaceID); err != nil {
		return nil, err
	}

	crd, err := func() (*v1.Workspace, error) {
		wsClient, wErr := s.workspaceCRDClient()
		if wErr != nil {
			return nil, wErr
		}
		return wsClient.Get(ctx, workspaceID, metav1.GetOptions{})
	}()
	if err != nil {
		if k8serrors.IsNotFound(err) {
			s.markDeleted(ctx, workspaceID)
			return nil, apierrors.NewNotFoundError("workspace", workspaceID, err)
		}
		return nil, apierrors.NewInternalError("workspace_get_failed", err)
	}

	result := &types.WorkspaceStatusResult{
		Phase:          string(crd.Status.Phase),
		PVCName:        crd.Status.PVCName,
		ActiveSessions: int(crd.Status.ActiveSessions),
		Message:        crd.Status.Message,
		ImageTag:       crd.Status.ImageTag,
	}

	// Fallback: if controller hasn't set ImageTag yet (pre-upgrade pods), read from pod spec
	if result.ImageTag == "" && crd.Status.PodName != "" {
		ns := crd.Status.PodNamespace
		if ns == "" {
			ns = s.config.Namespace
		}
		if pod, podErr := s.k8sClient.Clientset().CoreV1().Pods(ns).Get(ctx, crd.Status.PodName, metav1.GetOptions{}); podErr == nil {
			if len(pod.Spec.Containers) > 0 {
				image := pod.Spec.Containers[0].Image
				if i := strings.LastIndex(image, ":"); i >= 0 {
					result.ImageTag = image[i+1:]
				} else {
					result.ImageTag = image
				}
			}
		}
	}
	// US-23.3: read LastActivityAt from the annotation (authoritative)
	// with fallback to the deprecated Status field.
	if lastActivity := v1.GetLastActivityAt(crd); lastActivity != nil {
		t := lastActivity.Time
		result.LastActivityAt = &t
	}
	for _, c := range crd.Status.Conditions {
		result.Conditions = append(result.Conditions, types.WorkspaceConditionResult{
			Type:    string(c.Type),
			Status:  c.Status,
			Reason:  c.Reason,
			Message: c.Message,
		})
	}

	result.CredentialState = credStateFromConditions(crd.Status.Conditions)
	result.AgentHealth = agentHealthFromConditions(crd.Status.Conditions, crd.Status.LastHealthCheckAt)

	for _, s := range crd.Status.Sessions {
		result.Sessions = append(result.Sessions, types.SessionStatusItem{
			ID: s.ID, Title: s.Title, Status: s.Status, ContextUsed: s.ContextUsed,
		})
	}
	result.DiskUsedBytes = crd.Status.DiskUsedBytes
	result.DiskTotalBytes = crd.Status.DiskTotalBytes
	result.MemoryUsedBytes = crd.Status.MemoryUsedBytes
	result.MemoryTotalBytes = crd.Status.MemoryTotalBytes
	result.ContextUsed = crd.Status.ContextUsed
	result.ContextTotal = crd.Status.ContextTotal

	// Persist version info to DB so it's available in workspace list without extra K8s calls
	if result.ImageTag != "" || result.AgentHealth.AgentVersion != "" {
		s.dbService.SyncWorkspaceVersionInfo(ctx, workspaceID, result.ImageTag, result.AgentHealth.AgentVersion)
	}

	// US-Epic 27b (worklog 0589): auto-push user-DEK secrets on pod
	// recreation. See maybeAutoPushOnPodTransition for the state machine
	// (unchanged / initial-observation / transition-fires-push).
	s.maybeAutoPushOnPodTransition(ctx, userID, workspaceID, crd)

	return result, nil
}

// maybeAutoPushOnPodTransition compares the CRD's current pod identity
// (name, startTime) with the last observation persisted for this
// workspace. On a genuine transition (both tuples non-zero and different)
// it records the new identity with pending_refresh=TRUE and fires a
// fire-and-forget goroutine that pushes user-DEK secrets to the newly-
// recreated pod's agentd. The user's DEK is extracted from ctx by the
// pusher (agentpush.AuthFromContext) — the router must attach it via
// agentpush.WithAuth before calling GetWorkspaceStatus.
//
// This is the load-bearing fix for the "eventually disappear" symptom:
// pod recreation wipes the /sandbox-runtime tmpfs and the boot-time
// materialize replays only phase-1 (server-KEK) content. Every user-
// owned secret needs a phase-2 push from an authenticated user request
// context; this hook wires the API's ubiquitous status-poll to that
// push. See worklog 0589 for design rationale, rejected alternatives,
// and adversarial checks.
func (s *Service) maybeAutoPushOnPodTransition(ctx context.Context, userID, workspaceID string, crd *v1.Workspace) {
	if s.podIdentityTracker == nil {
		return // wiring not configured (dev/test); nothing to do
	}
	// Only Active pods have a stable identity worth tracking. Non-Active
	// phases either have no PodName (Pending/Creating pre-write, Suspended)
	// or are mid-terminating; recording those overwrites the previous
	// identity with empty values and would suppress the next Active
	// transition. Skip cleanly.
	if crd.Status.Phase != v1.WorkspacePhaseActive {
		return
	}
	currentName := crd.Status.PodName
	if currentName == "" || crd.Status.StartTime == nil {
		return
	}
	currentStart := crd.Status.StartTime.Time

	priorName, priorStart, err := s.podIdentityTracker.GetLastSeenPodIdentity(ctx, workspaceID)
	if err != nil {
		s.logger.Warn("pod identity: read failed; skipping auto-push detection this poll",
			"workspaceID", workspaceID, "error", err.Error())
		return
	}

	sameName := priorName == currentName
	sameStart := priorStart.Equal(currentStart)
	if sameName && sameStart {
		return // steady state — this is the vast majority of polls
	}

	// Initial observation: no prior identity recorded. Persist the current
	// one so subsequent polls can detect a transition. Do NOT fire the push:
	// on deploy day this would stampede every active workspace.
	if priorName == "" && priorStart.IsZero() {
		if err := s.podIdentityTracker.UpsertLastSeenPodIdentity(ctx, workspaceID, currentName, currentStart); err != nil {
			s.logger.Warn("pod identity: upsert initial failed",
				"workspaceID", workspaceID, "error", err.Error())
		}
		return
	}

	// True transition: record with pending_refresh=TRUE, then fire the push.
	priorChangedAt, err := s.podIdentityTracker.MarkPodIdentityTransition(ctx, workspaceID, currentName, currentStart)
	if err != nil {
		s.logger.Warn("pod identity: mark transition failed; auto-push not fired",
			"workspaceID", workspaceID, "error", err.Error())
		return
	}

	if s.secretPusher == nil {
		// No pusher wired; the DB flag stays TRUE and AgentReloadBanner
		// serves as the manual fallback. This is fine — a warning in
		// wiring, not a data-plane bug.
		return
	}

	// Fire-and-forget. context.WithoutCancel detaches from the request
	// context so the push survives the caller's return (Gin cancels
	// ctx on response commit).
	go s.runAutoPush(context.WithoutCancel(ctx), userID, workspaceID, priorName, currentName, currentStart, priorChangedAt)
}

// runAutoPush executes one Push attempt with structured logging and
// (on success) clears pending_refresh via ClearPendingRefreshAfterAutoPush.
// The priorChangedAt timestamp captured at transition time is fed back
// so the DB's optimistic-concurrency check correctly leaves the flag
// TRUE if a NEW credential was staged during the push window.
func (s *Service) runAutoPush(ctx context.Context, userID, workspaceID, priorPodName, newPodName string, newPodStart time.Time, priorChangedAt time.Time) {
	start := time.Now()
	err := s.secretPusher.Push(ctx, userID, workspaceID)
	elapsed := time.Since(start)
	if err != nil {
		s.logger.Warn("auto-push after pod recreation: failed",
			"workspaceID", workspaceID,
			"oldPodName", priorPodName,
			"newPodName", newPodName,
			"newPodStartTime", newPodStart.Format(time.RFC3339),
			"elapsedMs", elapsed.Milliseconds(),
			"error", err.Error())
		return
	}

	// Clear pending_refresh so the AgentReloadBanner disappears from
	// the frontend within one poll cycle. MarkAgentReloaded (wrapped by
	// ClearPendingRefreshAfterAutoPush) preserves the flag if a NEW
	// credential arrived during the push window (`currentChangedAt >
	// priorChangedAt`), so the banner correctly re-appears for the
	// fresh change.
	if _, err := s.podIdentityTracker.ClearPendingRefreshAfterAutoPush(ctx, workspaceID, priorChangedAt); err != nil {
		// The push already delivered the secrets — the pod has what it
		// needs. Failing to clear the flag is a UX regression (banner
		// stays visible until the next transition or manual reload) but
		// not a data-plane failure. Log and move on.
		s.logger.Warn("auto-push after pod recreation: clear pending_refresh failed",
			"workspaceID", workspaceID,
			"oldPodName", priorPodName,
			"newPodName", newPodName,
			"error", err.Error())
	}

	s.logger.Info("auto-push after pod recreation: success",
		"workspaceID", workspaceID,
		"oldPodName", priorPodName,
		"newPodName", newPodName,
		"newPodStartTime", newPodStart.Format(time.RFC3339),
		"elapsedMs", elapsed.Milliseconds())
}

// ResolveWorkspace fetches workspace metadata by ID. It is the pure-fetch half
// of verifyOwner and the entry point for WorkspaceAccessMiddleware. Returns a
// NotFound APIError when the workspace does not exist (or the id is empty,
// mirroring GetWorkspace's empty-input contract) and an Internal APIError when
// the underlying lookup fails.
func (s *Service) ResolveWorkspace(ctx context.Context, workspaceID string) (*types.WorkspaceMetadata, error) {
	meta, err := s.dbService.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, apierrors.NewInternalError("workspace_retrieval_failed", err)
	}
	if meta == nil {
		return nil, apierrors.NewNotFoundError("workspace", workspaceID, fmt.Errorf("workspace not found"))
	}
	return meta, nil
}

// CheckOwnership is the pure-authorisation half of verifyOwner, operating on a
// previously-resolved *types.WorkspaceMetadata so callers can avoid a second
// DB hit. Behavior is identical to the post-fetch portion of verifyOwner:
//
//   - nil meta → fail-closed Forbidden (defense-in-depth; ResolveWorkspace
//     already returns NotFound for missing rows).
//   - creator match → for org-attributed workspaces the creator must still be
//     a CURRENT org member (D5 offboarding); personal workspaces pass.
//   - non-creator on an org workspace → allowed only as org admin (D6).
//   - anything else → Forbidden.
//
// Org-store failures propagate as wrapped errors (NOT *APIError) so callers
// can distinguish infrastructure failure from authorisation denial — this
// preserves the exact return-shape contract verifyOwner had before the split.
func (s *Service) CheckOwnership(ctx context.Context, userID string, meta *types.WorkspaceMetadata) error {
	if meta == nil {
		return apierrors.NewForbiddenError("workspace access denied", fmt.Errorf("nil metadata"))
	}
	if meta.UserID == userID {
		if meta.OrgID != nil && *meta.OrgID != "" && s.orgStore != nil {
			isMember, err := s.orgStore.IsOrgMember(ctx, *meta.OrgID, userID)
			if err != nil {
				return fmt.Errorf("check org membership: %w", err)
			}
			if !isMember {
				return apierrors.NewForbiddenError(
					"workspace access denied",
					fmt.Errorf("user %s is no longer a member of org %s", userID, *meta.OrgID),
				)
			}
		}
		return nil
	}
	if meta.OrgID != nil && *meta.OrgID != "" && s.orgStore != nil {
		isAdmin, err := s.orgStore.IsOrgAdmin(ctx, *meta.OrgID, userID)
		if err != nil {
			return fmt.Errorf("check org admin: %w", err)
		}
		if isAdmin {
			return nil
		}
	}
	return apierrors.NewForbiddenError(
		"workspace access denied",
		fmt.Errorf("user %s does not have access to workspace %s", userID, meta.ID),
	)
}

// verifyOwner returns a forbidden or not-found error if the user does not own
// the workspace. Returns nil when the user is the owner.
//
// As of design 0041 Story 2 this is a thin wrapper over ResolveWorkspace +
// CheckOwnership that trusts WorkspaceAccessMiddleware's prior decision when
// the middleware has already validated ownership for THIS workspace in the
// current request context. The short-circuit is what makes the middleware the
// single ownership gate without forcing the 11 service-layer callers to drop
// their defense-in-depth check: on the HTTP path the check becomes free, on
// background-job paths (no middleware) it still runs the full Resolve +
// Check (design 0041 edge case 5). The meta.ID == workspaceID guard is
// defensive — without it a caller could forge ownership of any workspace by
// stuffing an unrelated meta into context.
//
// Per Epic 43 decision D6, access to an org workspace requires either being the
// creator (meta.UserID) or an org admin (IsOrgAdmin). Plain org members can only
// access workspaces they themselves created — they can no longer reach other
// members' org workspaces. This makes verifyOwner equivalent to the former
// verifyOrgAdmin; that method has been consolidated into this one.
func (s *Service) verifyOwner(ctx context.Context, userID, workspaceID string) error {
	if meta, ok := types.WorkspaceMetaFromCtx(ctx); ok && meta.ID == workspaceID {
		return nil
	}
	resolved, err := s.ResolveWorkspace(ctx, workspaceID)
	if err != nil {
		return err
	}
	return s.CheckOwnership(ctx, userID, resolved)
}

// buildWorkspaceCRD constructs a v1.Workspace CRD from an API request.
func buildWorkspaceCRD(workspaceID, userID string, req types.CreateWorkspaceRequest, namespace string) *v1.Workspace {
	owner := v1.WorkspaceOwner{UserID: userID}
	if req.OrgID != nil {
		owner.OrgID = *req.OrgID
	}

	// Epic 51 S51.3: tenant label for quota enforcement + audit attribution.
	// tenantID = orgID if set (Design 0031 D4), else userID.
	tenantIDVal := userID
	if owner.OrgID != "" {
		tenantIDVal = owner.OrgID
	}

	// Merge user-supplied labels first into an empty map, then set system
	// labels — system labels must always win so users cannot spoof identity
	// attributes (tenant, user-id, app). This prevents cross-tenant
	// information disclosure and quota bypass via label spoofing.
	labels := make(map[string]string)
	for k, v := range req.Labels {
		labels[k] = v
	}
	labels["app"] = "llmsafespaces"
	labels["user-id"] = userID
	labels["llmsafespaces.dev/tenant"] = tenantIDVal

	spec := v1.WorkspaceSpec{
		Owner: owner,
		Storage: v1.WorkspaceStorageConfig{
			Size:             req.StorageSize,
			StorageClassName: req.StorageClass,
		},
		Runtime: req.Runtime,
	}

	return &v1.Workspace{
		TypeMeta: metav1.TypeMeta{APIVersion: "llmsafespaces.dev/v1", Kind: "Workspace"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      workspaceID,
			Namespace: namespace,
			Labels:    labels,
			Annotations: map[string]string{
				"llmsafespaces.dev/created-by": userID,
				"llmsafespaces.dev/name":       req.Name,
				// AnnotationRequestedAt records the exact moment the API
				// received the create request. The controller reads this to
				// anchor WorkspaceCreateDurationSeconds from the user's
				// perspective rather than the controller's first reconcile.
				v1.AnnotationRequestedAt: time.Now().UTC().Format(time.RFC3339Nano),
			},
		},
		Spec: spec,
	}
}

// applyWorkspaceDefaults reads instance settings and applies defaults to the
// CRD spec for fields not already set by the request. Gracefully degrades if
// settings are unavailable.
func (s *Service) applyWorkspaceDefaults(ctx context.Context, crd *v1.Workspace) {
	if s.instanceSettings == nil {
		return
	}

	// Security level
	if crd.Spec.SecurityLevel == "" {
		if level, err := s.instanceSettings.GetString(ctx, settings.KeyWorkspaceDefaultSecurityLevel.Name()); err == nil && level != "" {
			crd.Spec.SecurityLevel = level
		}
	}

	// Storage class
	if crd.Spec.Storage.StorageClassName == "" {
		if sc, err := s.instanceSettings.DefaultStorageClass(ctx); err == nil && sc != "" {
			crd.Spec.Storage.StorageClassName = sc
		}
	}

	// Resources
	if crd.Spec.Resources == nil {
		cpu, _ := s.instanceSettings.GetString(ctx, settings.KeyWorkspaceDefaultResourcesCPU.Name())
		mem, _ := s.instanceSettings.GetString(ctx, settings.KeyWorkspaceDefaultResourcesMemory.Name())
		if cpu != "" || mem != "" {
			crd.Spec.Resources = &v1.ResourceRequirements{
				CPU: cpu, Memory: mem,
			}
		}
	}

	// Auto-suspend (only if not already set by request/CRD)
	if crd.Spec.AutoSuspend == nil {
		autoSuspendEnabled := true
		idleTimeout := int64(86400)
		if v, err := s.instanceSettings.GetBool(ctx, settings.KeyWorkspaceAutoSuspendEnabled.Name()); err == nil {
			autoSuspendEnabled = v
		}
		if v, err := s.instanceSettings.GetInt(ctx, settings.KeyWorkspaceAutoSuspendIdleTimeout.Name()); err == nil && v > 0 {
			idleTimeout = int64(v) * 60
		}
		crd.Spec.AutoSuspend = &v1.WorkspaceAutoSuspend{
			Enabled:            autoSuspendEnabled,
			IdleTimeoutSeconds: idleTimeout,
		}
	}

	// TTL after suspended
	if days, err := s.instanceSettings.GetInt(ctx, settings.KeyWorkspaceTTLDaysAfterSuspended.Name()); err == nil && days > 0 {
		crd.Spec.TTLSecondsAfterSuspended = int64(days) * 86400
	}

	// Network access
	if crd.Spec.NetworkAccess == nil {
		ingress, _ := s.instanceSettings.GetBool(ctx, settings.KeyWorkspaceDefaultNetworkIngress.Name())
		domains, _ := s.instanceSettings.GetStrings(ctx, settings.KeyWorkspaceDefaultNetworkEgress.Name())
		if ingress || len(domains) > 0 {
			egress := make([]v1.WorkspaceEgressRule, len(domains))
			for i, d := range domains {
				egress[i] = v1.WorkspaceEgressRule{Domain: d}
			}
			crd.Spec.NetworkAccess = &v1.WorkspaceNetworkAccess{
				Ingress: ingress, Egress: egress,
			}
		}
	}

	// Max active sessions — enforced by the proxy on every request.
	// Without this, the proxy falls back to a hardcoded constant of 5
	// regardless of what the admin configured. (Epic 13 US-13.3)
	if crd.Spec.MaxActiveSessions == 0 {
		if v, err := s.instanceSettings.GetInt(ctx, settings.KeyWorkspaceDefaultMaxActiveSessions.Name()); err == nil && v > 0 {
			crd.Spec.MaxActiveSessions = int32(v) //nolint:gosec // v is bounded by settings schema (1-100); overflow impossible
		}
	}
}

// --- Frontend methods (Phase A) ---

// EnsureSession guarantees the workspace has a Running sandbox and creates a
// new session on it. If the workspace is suspended it resumes it; if no sandbox
// exists it creates one. Blocks until the sandbox reaches Running, then creates
// the session via opencode's POST /session endpoint.
func (s *Service) EnsureSession(ctx context.Context, userID, workspaceID string) (*types.EnsureSessionResponse, error) {
	if err := s.verifyOwner(ctx, userID, workspaceID); err != nil {
		return nil, err
	}

	crd, err := func() (*v1.Workspace, error) {
		wsClient, wErr := s.workspaceCRDClient()
		if wErr != nil {
			return nil, wErr
		}
		return wsClient.Get(ctx, workspaceID, metav1.GetOptions{})
	}()
	if err != nil {
		return nil, apierrors.NewInternalError("workspace_get_failed", err)
	}

	resumed := false
	switch crd.Status.Phase {
	case v1.WorkspacePhaseSuspended:
		// ActivateWorkspace injects credentials then transitions to Resuming.
		if _, err := s.ActivateWorkspace(ctx, userID, workspaceID); err != nil {
			return nil, err
		}
		resumed = true
	case v1.WorkspacePhaseTerminating, v1.WorkspacePhaseTerminated, v1.WorkspacePhaseFailed:
		return nil, apierrors.NewValidationError(
			"workspace is not usable",
			map[string]interface{}{"phase": string(crd.Status.Phase)},
			fmt.Errorf("workspace %s is in %s phase", workspaceID, crd.Status.Phase),
		)
	case v1.WorkspacePhasePending, v1.WorkspacePhaseCreating, v1.WorkspacePhaseActive, v1.WorkspacePhaseResuming:
		// Will wait for Active below.
	}

	// Wait for workspace to reach Active with PodIP.
	podIP, err := s.waitForWorkspaceActive(ctx, workspaceID)
	if err != nil {
		return nil, err
	}

	// Create session directly on workspace pod.
	sessionID, err := s.createSessionOnWorkspace(ctx, workspaceID, podIP)
	if err != nil {
		return nil, err
	}

	return &types.EnsureSessionResponse{
		WorkspaceID:    workspaceID,
		WorkspacePhase: "Active",
		SessionID:      sessionID,
		Resumed:        resumed,
	}, nil
}

// waitForWorkspaceActive polls the workspace CRD until it reaches Active with
// a PodIP, or the context is canceled. Returns the pod IP.
func (s *Service) waitForWorkspaceActive(ctx context.Context, workspaceID string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		crd, err := func() (*v1.Workspace, error) {
			wsClient, wErr := s.workspaceCRDClient()
			if wErr != nil {
				return nil, wErr
			}
			return wsClient.Get(ctx, workspaceID, metav1.GetOptions{})
		}()
		if err != nil {
			return "", apierrors.NewInternalError("workspace_get_failed", err)
		}
		if crd.Status.Phase == v1.WorkspacePhaseActive && crd.Status.PodIP != "" {
			return crd.Status.PodIP, nil
		}
		if crd.Status.Phase == v1.WorkspacePhaseFailed || crd.Status.Phase == v1.WorkspacePhaseTerminated {
			return "", apierrors.NewInternalError("workspace_failed", fmt.Errorf("workspace %s entered %s phase", workspaceID, crd.Status.Phase))
		}
		select {
		case <-ctx.Done():
			return "", apierrors.NewInternalError("workspace_timeout", fmt.Errorf("timed out waiting for workspace %s to reach Active", workspaceID))
		case <-ticker.C:
		}
	}
}

// createSessionOnWorkspace calls opencode's POST /session on the workspace pod.
func (s *Service) createSessionOnWorkspace(ctx context.Context, workspaceID, podIP string) (string, error) {
	secretName := fmt.Sprintf("workspace-pw-%s", workspaceID)
	secret, err := s.k8sClient.Clientset().CoreV1().Secrets(s.config.Namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return "", apierrors.NewInternalError("workspace_password_failed", err)
	}
	password := string(secret.Data["password"])

	port := s.config.OpencodePort
	if port == 0 {
		port = 4096
	}
	url := fmt.Sprintf("http://%s:%d/session", podIP, port)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", apierrors.NewInternalError("session_request_failed", err)
	}
	req.SetBasicAuth("opencode", password)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", apierrors.NewInternalError("session_create_failed", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", apierrors.NewInternalError("session_create_failed", fmt.Errorf("opencode returned %d", resp.StatusCode))
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", apierrors.NewInternalError("session_decode_failed", err)
	}
	return result.ID, nil
}

// ActivateWorkspace resumes a workspace, suspending the stalest active one if at cap.
func (s *Service) ActivateWorkspace(ctx context.Context, userID, workspaceID string) (*types.ActivateWorkspaceResponse, error) {
	// Verify ownership
	if err := s.verifyOwner(ctx, userID, workspaceID); err != nil {
		return nil, err
	}

	// Inject credentials into ephemeral K8s Secret before transitioning to
	// Epic 35: the pod's init container fetches credentials via the bootstrap
	// endpoint at boot — no pre-writing of a K8s Secret is needed.

	// Enforce max active workspaces — may suspend the stalest workspace
	suspended, err := s.enforceMaxActiveWorkspaces(ctx, userID, workspaceID)
	if err != nil {
		return nil, err
	}

	// Fetch current CRD state and transition to Resuming.
	crd, err := func() (*v1.Workspace, error) {
		wsClient, wErr := s.workspaceCRDClient()
		if wErr != nil {
			return nil, wErr
		}
		return wsClient.Get(ctx, workspaceID, metav1.GetOptions{})
	}()
	if err != nil {
		return nil, apierrors.NewInternalError("workspace_get_failed", err)
	}

	if isActivePhase(crd.Status.Phase) {
		// Already active — nothing to do (idempotent).
		return &types.ActivateWorkspaceResponse{
			Resumed:   workspaceID,
			Suspended: suspended,
		}, nil
	}

	if crd.Status.Phase != v1.WorkspacePhaseSuspended {
		return nil, apierrors.NewConflictError(
			"workspace",
			workspaceID,
			fmt.Errorf("cannot activate workspace in phase %q (must be Suspended or Active)", crd.Status.Phase),
		)
	}

	// US-23.3: write Spec.Suspend=false (pointer non-nil, value false)
	// instead of Status.Phase. The controller observes the spec change
	// in handleSuspended and transitions to Resuming → Creating → Active.
	// This makes the controller the sole writer of Status.Phase.
	//
	// LastActivityAt is written to the metadata annotation (not Status)
	// so it uses a separate optimistic-concurrency lane from
	// Status().Update, eliminating the cross-writer race.
	//
	// M13-a: wrapped in RetryOnConflict to handle concurrent spec/annotation
	// writes. The re-get inside the closure re-applies both Spec.Suspend=false
	// and the annotation so the retry doesn't clobber a concurrent spec change.
	wsClient, wErr := s.workspaceCRDClient()
	if wErr != nil {
		return nil, apierrors.NewInternalError("workspace_resume_failed", wErr)
	}
	var persisted *v1.Workspace
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := wsClient.Get(ctx, crd.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		suspendFalse := false
		current.Spec.Suspend = &suspendFalse
		if current.Annotations == nil {
			current.Annotations = make(map[string]string)
		}
		v1.SetLastActivityAtAnnotation(current.Annotations, metav1.Now())
		persisted, err = wsClient.Update(ctx, current)
		return err
	}); err != nil {
		s.logger.Error("Failed to set Spec.Suspend=false", err, "workspaceID", workspaceID)
		return nil, apierrors.NewInternalError("workspace_resume_failed", err)
	}

	// Post-write read-back assertion. The K8s apiserver silently prunes
	// fields not declared in the CRD's OpenAPI schema (default behavior
	// when x-kubernetes-preserve-unknown-fields is unset), so a successful
	// Update can still leave Spec.Suspend=nil if the deployed CRD is older
	// than the binary. This produced the worklog 0465 incident: every
	// resume request returned 200 OK with {"resumed":...} but the controller
	// never observed a transition because the field was dropped before
	// persistence, leaving the workspace stuck Suspended.
	//
	// The check covers two distinct CRD-misconfiguration shapes:
	//
	//   1. Field absent from the schema → apiserver prunes → Spec.Suspend=nil.
	//   2. Field present but with a wrong default (e.g. `default: true` on
	//      a clone of the CRD) → apiserver applies the default after our
	//      &false write → Spec.Suspend=&true.
	//
	// Both are operator-fixable by re-applying charts/llmsafespaces/crds/
	// workspace.yaml. We surface a concrete operator-actionable error
	// instead of a phantom success and name the field so the operator can
	// correlate directly to CRD schema drift.
	if persisted == nil || persisted.Spec.Suspend == nil || *persisted.Spec.Suspend {
		err := fmt.Errorf(
			"apiserver did not persist spec.suspend=false (got %s); "+
				"deployed CRD likely lacks the suspend field or has a wrong default — "+
				"re-apply charts/llmsafespaces/crds/workspace.yaml",
			specSuspendValue(persisted),
		)
		s.logger.Error("spec.suspend not persisted after Update", err,
			"workspaceID", workspaceID)
		return nil, apierrors.NewInternalError("workspace_resume_failed", err)
	}

	s.logger.Info("Workspace activated", "workspaceID", workspaceID, "userID", userID)
	return &types.ActivateWorkspaceResponse{
		Resumed:   workspaceID,
		Suspended: suspended,
	}, nil
}

// specSuspendValue renders Spec.Suspend for log/error output. The pointer-to-bool
// shape needs three states: nil (pruned/unset), &false, &true.
func specSuspendValue(ws *v1.Workspace) string {
	if ws == nil {
		return "<no object returned>"
	}
	if ws.Spec.Suspend == nil {
		return "<nil — field was pruned or never written>"
	}
	if *ws.Spec.Suspend {
		return "true"
	}
	return "false"
}

// ListWorkspaceSessions returns session index entries for a workspace.
func (s *Service) ListWorkspaceSessions(ctx context.Context, userID, workspaceID string) ([]types.SessionListItem, error) {
	if err := s.verifyOwner(ctx, userID, workspaceID); err != nil {
		return nil, err
	}
	if s.sessionIndex == nil {
		return []types.SessionListItem{}, nil
	}
	return s.sessionIndex.ListByWorkspace(ctx, workspaceID)
}

func (s *Service) MarkSessionSeen(ctx context.Context, userID, workspaceID, sessionID string) error {
	if err := s.verifyOwner(ctx, userID, workspaceID); err != nil {
		return err
	}
	if s.sessionIndex == nil {
		return nil
	}
	return s.sessionIndex.UpdateLastSeen(ctx, workspaceID, sessionID)
}

// RenameWorkspace updates the name of a workspace.
func (s *Service) RenameWorkspace(ctx context.Context, userID, workspaceID, name string) error {
	if err := s.verifyOwner(ctx, userID, workspaceID); err != nil {
		return err
	}
	return s.dbService.UpdateWorkspace(ctx, workspaceID, types.WorkspaceUpdates{Name: &name})
}

// RenameSession updates the title of a session in the session index.
func (s *Service) RenameSession(ctx context.Context, userID, workspaceID, sessionID, title string) error {
	if err := s.verifyOwner(ctx, userID, workspaceID); err != nil {
		return err
	}
	if s.sessionIndex == nil {
		return nil
	}
	return s.sessionIndex.UpsertTitle(ctx, workspaceID, sessionID, title)
}

func credStateFromConditions(conditions []v1.WorkspaceCondition) types.CredentialStateResult {
	for _, c := range conditions {
		if c.Type == v1.WorkspaceConditionCredentialsAvailable {
			return types.CredentialStateResult{
				Available: c.Status == "True",
				Reason:    c.Reason,
				Message:   c.Message,
			}
		}
	}
	return types.CredentialStateResult{Available: false, Reason: "NotChecked"}
}

var connectedRe = regexp.MustCompile(`connected=\[([^\]]*)\]`)
var versionRe = regexp.MustCompile(`version=(\S+)`)
var configuredRe = regexp.MustCompile(`configured=(\d+)`)

func agentHealthFromConditions(conditions []v1.WorkspaceCondition, lastCheckAt *metav1.Time) types.AgentHealthResult {
	for _, c := range conditions {
		if c.Type == v1.WorkspaceConditionAgentHealthy {
			status := "Unknown"
			switch c.Status {
			case "True":
				status = "Healthy"
			case "False":
				switch c.Reason {
				case v1.ReasonAgentUnhealthy, v1.ReasonHealthCheckFailed:
					status = "Unhealthy"
				default:
					status = "Degraded"
				}
			}
			result := types.AgentHealthResult{
				Status:  status,
				Message: c.Message,
			}
			if m := connectedRe.FindStringSubmatch(c.Message); len(m) > 1 && m[1] != "" {
				parts := strings.Split(m[1], " ")
				result.Connected = make([]string, 0, len(parts))
				for _, p := range parts {
					if p != "" {
						result.Connected = append(result.Connected, p)
					}
				}
			}
			if m := versionRe.FindStringSubmatch(c.Message); len(m) > 1 {
				result.AgentVersion = m[1]
			}
			if m := configuredRe.FindStringSubmatch(c.Message); len(m) > 1 {
				_, _ = fmt.Sscanf(m[1], "%d", &result.ProvidersConfigured)
			}
			if lastCheckAt != nil {
				result.LastCheckedAt = lastCheckAt.Format(time.RFC3339)
			}
			return result
		}
	}
	return types.AgentHealthResult{Status: "Unknown"}
}

// --- Epic 10: Secret injection helpers ---

type sessionIDCtxKey struct{}

var sessionIDContextKey = sessionIDCtxKey{}

// ContextWithSessionID adds the session ID to context for secret injection during activation.
func ContextWithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionIDContextKey, sessionID)
}
