// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

type orgStore interface {
	CreateOrgWithAdmin(ctx context.Context, org *types.Organization, adminUserID string) (*types.Organization, error)
	GetOrg(ctx context.Context, orgID string) (*types.Organization, error)
	GetOrgBySlug(ctx context.Context, slug string) (*types.Organization, error)
	ListOrgsForUser(ctx context.Context, userID string) ([]*types.OrgResponse, error)
	UpdateOrg(ctx context.Context, orgID string, req types.UpdateOrgRequest) (*types.Organization, error)
	SoftDeleteOrg(ctx context.Context, orgID string) error
	IsOrgMember(ctx context.Context, orgID, userID string) (bool, error)
	IsOrgAdmin(ctx context.Context, orgID, userID string) (bool, error)
	GetOrgMember(ctx context.Context, orgID, userID string) (*types.OrgMember, error)
	ListOrgMembers(ctx context.Context, orgID string) ([]*types.OrgMember, error)
	AddOrgMember(ctx context.Context, orgID, userID string, role types.OrgRole) error
	RemoveOrgMember(ctx context.Context, orgID, userID string) error
	RemoveOrgAdminIfNotLast(ctx context.Context, orgID, targetUserID string) (bool, error)
	DemoteOrgAdminIfNotLast(ctx context.Context, orgID, targetUserID string) (bool, error)
	UpdateOrgMemberRole(ctx context.Context, orgID, userID string, role types.OrgRole) error
	ListOrgWorkspaces(ctx context.Context, orgID string, limit, offset int) ([]*types.WorkspaceMetadata, *types.PaginationMetadata, error)
	GetUserIDByEmail(ctx context.Context, email string) (string, error)
	GetUserOrgID(ctx context.Context, userID string) (string, error)
	UserExistsByID(ctx context.Context, userID string) (bool, error)
	GetStripeCustomerID(ctx context.Context, orgID string) (string, error)
	UpdateOrgStatus(ctx context.Context, orgID string, status *types.OrgStatus, subStatus *types.OrgSubscriptionStatus, planID *types.OrgPlan) error
	// MarkUserEmailVerified bypasses the email-verification token flow and
	// marks the user as email-verified. Used by the org-admin Verify action.
	MarkUserEmailVerified(ctx context.Context, userID string) error
	// LogOrgEvent emits an org-scoped audit entry (domain='org'). Used by the
	// Verify action to record the admin override.
	LogOrgEvent(ctx context.Context, orgID, actorID, action, targetID string, metadata map[string]any) error
}

// orgAuthService is the minimal auth interface used by OrgsHandler.
type orgAuthService interface {
	GetUserID(c *gin.Context) string
}

// orgsLogger is the optional logger surface used to record non-fatal audit
// emission failures. Matches the policyLogger / ssoLogger shapes used by the
// other org-scoped handlers. Nil ⇒ silent (test default).
type orgsLogger interface {
	Warn(msg string, args ...any)
}

// OrgsHandler handles org CRUD endpoints.
type OrgsHandler struct {
	orgStore   orgStore
	authSvc    orgAuthService
	billing    OrgBilling
	successURL string
	cancelURL  string
	portalURL  string
	logger     orgsLogger
}

// NewOrgsHandler creates a new OrgsHandler.
func NewOrgsHandler(
	store orgStore,
	authSvc orgAuthService,
) *OrgsHandler {
	return &OrgsHandler{
		orgStore: store,
		authSvc:  authSvc,
	}
}

// SetBilling wires the Stripe checkout/portal provider and redirect URLs.
// When not called (or passed a nil provider), org creation succeeds but produces
// no checkout URL — development mode without Stripe configured.
func (h *OrgsHandler) SetBilling(b OrgBilling, successURL, cancelURL, portalURL string) {
	h.billing = b
	h.successURL = successURL
	h.cancelURL = cancelURL
	h.portalURL = portalURL
}

// SetLogger wires an optional logger used to surface non-fatal audit emission
// failures (e.g. a VerifyMember action that succeeds but whose audit row could
// not be written). When not called, audit failures are silent.
func (h *OrgsHandler) SetLogger(l orgsLogger) {
	h.logger = l
}

// GetOrg exposes orgStore.GetOrg so middleware.FeatureGuard can read the org's
// plan without depending on the store directly. Satisfies orgPlanReader.
func (h *OrgsHandler) GetOrg(ctx context.Context, orgID string) (*types.Organization, error) {
	return h.orgStore.GetOrg(ctx, orgID)
}

// Create handles POST /api/v1/orgs.
//
// Per design 0031 D1, org creation is platform-admin only. Non-admin callers
// receive 403. A platform admin supplies the intended owner's email; the
// backend resolves it to a user ID (single lookup, 404 when no such user) and
// creates the org already active with the requested plan (default enterprise),
// adding the resolved user as the org's first admin member. The caller admin is
// recorded as CreatedBy; the owner is the email-resolved user. No Stripe
// Checkout session is created at org-creation time — the self-service flow is
// deferred to the future billing-portal epic.
func (h *OrgsHandler) Create(c *gin.Context) {
	callerID := h.authSvc.GetUserID(c)
	if callerID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	if !isPlatformAdmin(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "only platform admins can create organizations"})
		return
	}

	var req types.CreateOrgRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, bindingErrorResponse(err, &req))
		return
	}
	req.Slug = strings.ToLower(req.Slug)
	ownerEmail := strings.ToLower(strings.TrimSpace(req.OwnerEmail))

	ctx := c.Request.Context()

	ownerID, err := h.orgStore.GetUserIDByEmail(ctx, ownerEmail)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve owner"})
		return
	}
	if ownerID == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "owner not found"})
		return
	}

	// Single-org enforcement (D8): if the resolved owner is already in another
	// org, CreateOrgWithAdmin's membership insert would hit the unique index on
	// org_memberships(user_id) and bubble up as a 23505 that isDuplicateErr
	// would misclassify as "slug already in use". Pre-check for a clear 409.
	existingOrgID, err := h.orgStore.GetUserOrgID(ctx, ownerID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check owner org membership"})
		return
	}
	if existingOrgID != "" {
		c.JSON(http.StatusConflict, gin.H{"error": "owner is already a member of another organization"})
		return
	}

	existing, err := h.orgStore.GetOrgBySlug(ctx, req.Slug)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check slug uniqueness"})
		return
	}
	if existing != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "slug already in use"})
		return
	}

	newOrg := &types.Organization{
		ID:        uuid.New().String(),
		Name:      req.Name,
		Slug:      req.Slug,
		CreatedBy: callerID,
	}

	created, err := h.orgStore.CreateOrgWithAdmin(ctx, newOrg, ownerID)
	if err != nil {
		if errors.Is(ClassifyPostgresError(err), ErrDuplicateCredential) {
			c.JSON(http.StatusConflict, gin.H{"error": "slug already in use"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create organization"})
		return
	}

	plan := req.PlanID
	if plan == "" {
		plan = types.PlanEnterprise
	}
	active := types.OrgStatusActive
	sub := types.SubscriptionActive
	if err := h.orgStore.UpdateOrgStatus(ctx, created.ID, &active, &sub, &plan); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to activate organization"})
		return
	}
	created.Status = active
	created.PlanID = plan
	created.SubscriptionStatus = sub

	// UserRole reflects the calling user's membership context (types.go OrgResponse).
	// The caller is the creator (CreatedBy). When the admin creates the org for a
	// different owner, the caller is not a member — so the role is empty unless
	// the admin is also the resolved owner.
	userRole := types.OrgRole("")
	if ownerID == callerID {
		userRole = types.OrgRoleAdmin
	}

	c.JSON(http.StatusCreated, types.CreateOrgResponse{
		OrgResponse: types.OrgResponse{
			Organization: *created,
			UserRole:     userRole,
			MemberCount:  1,
		},
	})
}

// List handles GET /api/v1/orgs.
func (h *OrgsHandler) List(c *gin.Context) {
	userID := h.authSvc.GetUserID(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	orgs, err := h.orgStore.ListOrgsForUser(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list organizations"})
		return
	}

	c.JSON(http.StatusOK, orgs)
}

// Get handles GET /api/v1/orgs/:id.
func (h *OrgsHandler) Get(c *gin.Context) {
	orgID := c.Param("id")
	userID := h.authSvc.GetUserID(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	ctx := c.Request.Context()

	org, err := h.orgStore.GetOrg(ctx, orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get organization"})
		return
	}
	if org == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "organization not found"})
		return
	}

	member, err := h.orgStore.GetOrgMember(ctx, orgID, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get membership"})
		return
	}
	if member == nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "not a member of this organization"})
		return
	}

	orgs, err := h.orgStore.ListOrgsForUser(ctx, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get org details"})
		return
	}

	for _, o := range orgs {
		if o.ID == orgID {
			c.JSON(http.StatusOK, o)
			return
		}
	}

	c.JSON(http.StatusOK, types.OrgResponse{
		Organization: *org,
		UserRole:     member.Role,
		MemberCount:  0,
	})
}

// Update handles PUT /api/v1/orgs/:id.
func (h *OrgsHandler) Update(c *gin.Context) {
	orgID := c.Param("id")

	var req types.UpdateOrgRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, bindingErrorResponse(err, &req))
		return
	}
	// Lowercase the slug before any uniqueness check or persistence — mirrors
	// Create's behavior so the stored case is consistent across both paths.
	// The DB enforces case-insensitive uniqueness via
	// idx_orgs_slug_lower_active (migration 000030); this keeps the stored
	// value canonical too, which matters for SSO URL routing
	// (/auth/sso/:orgSlug/*) and display consistency.
	req.Slug = strings.ToLower(req.Slug)

	ctx := c.Request.Context()

	if req.Slug != "" {
		existing, err := h.orgStore.GetOrgBySlug(ctx, req.Slug)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check slug uniqueness"})
			return
		}
		if existing != nil && existing.ID != orgID {
			c.JSON(http.StatusConflict, gin.H{"error": "slug already in use"})
			return
		}
	}

	updated, err := h.orgStore.UpdateOrg(ctx, orgID, req)
	if err != nil {
		if errors.Is(ClassifyPostgresError(err), ErrDuplicateCredential) {
			c.JSON(http.StatusConflict, gin.H{"error": "slug already in use"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update organization"})
		return
	}
	if updated == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "organization not found"})
		return
	}

	userID := h.authSvc.GetUserID(c)
	orgs, err := h.orgStore.ListOrgsForUser(ctx, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get org details"})
		return
	}
	for _, o := range orgs {
		if o.ID == orgID {
			c.JSON(http.StatusOK, o)
			return
		}
	}

	c.JSON(http.StatusOK, types.OrgResponse{Organization: *updated})
}

// Delete handles DELETE /api/v1/orgs/:id.
func (h *OrgsHandler) Delete(c *gin.Context) {
	orgID := c.Param("id")
	ctx := c.Request.Context()

	// S12: with always-org-attributed workspaces (D4), every active org has
	// workspaces, so the old OrgHasActiveWorkspaces guard made deletion
	// impossible. Workspaces now become frozen (org_id retained, IsOrgMember
	// returns false for deleted orgs) instead of blocking deletion.
	if err := h.orgStore.SoftDeleteOrg(ctx, orgID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete organization"})
		return
	}

	c.Status(http.StatusNoContent)
}

// ListWorkspaces handles GET /api/v1/orgs/:id/workspaces.
func (h *OrgsHandler) ListWorkspaces(c *gin.Context) {
	orgID := c.Param("id")

	limit := 20
	offset := 0
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
			if limit > 100 {
				limit = 100
			}
		}
	}
	if v := c.Query("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	workspaces, pagination, err := h.orgStore.ListOrgWorkspaces(c.Request.Context(), orgID, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list org workspaces"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"items":      workspaces,
		"pagination": pagination,
	})
}

// ListMembers handles GET /api/v1/orgs/:id/members.
func (h *OrgsHandler) ListMembers(c *gin.Context) {
	orgID := c.Param("id")
	members, err := h.orgStore.ListOrgMembers(c.Request.Context(), orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list members"})
		return
	}
	if members == nil {
		members = []*types.OrgMember{}
	}
	c.JSON(http.StatusOK, members)
}

// AddMember handles POST /api/v1/orgs/:id/members.
func (h *OrgsHandler) AddMember(c *gin.Context) {
	orgID := c.Param("id")
	ctx := c.Request.Context()

	var req types.AddOrgMemberRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, bindingErrorResponse(err, &req))
		return
	}

	if req.Role != types.OrgRoleAdmin && req.Role != types.OrgRoleMember {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role must be 'admin' or 'member'"})
		return
	}

	// The parent-id audit (instance 8): a ghost userId previously fell
	// through both pre-checks (GetOrgMember -> (nil,nil), GetUserOrgID ->
	// ("",nil) on ErrNoRows) and hit the FK as an opaque 500.
	exists, err := h.orgStore.UserExistsByID(ctx, req.UserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check user"})
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	existing, err := h.orgStore.GetOrgMember(ctx, orgID, req.UserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check membership"})
		return
	}
	if existing != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "user is already a member"})
		return
	}

	// Single-org enforcement (D8): adding a user already in another org would
	// hit the unique index on org_memberships(user_id) as a raw constraint error.
	// Pre-check to return a clear 409.
	currentOrgID, err := h.orgStore.GetUserOrgID(ctx, req.UserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check existing org membership"})
		return
	}
	if currentOrgID != "" {
		c.JSON(http.StatusConflict, gin.H{"error": "user is already a member of another organization"})
		return
	}

	if err := h.orgStore.AddOrgMember(ctx, orgID, req.UserID, req.Role); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to add member"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{})
}

// RemoveMember handles DELETE /api/v1/orgs/:id/members/:userID.
func (h *OrgsHandler) RemoveMember(c *gin.Context) {
	orgID := c.Param("id")
	targetUserID := c.Param("userID")
	callerUserID := h.authSvc.GetUserID(c)
	ctx := c.Request.Context()

	if targetUserID == callerUserID {
		c.JSON(http.StatusConflict, gin.H{"error": "org admins cannot remove themselves; transfer admin role first"})
		return
	}

	targetMember, err := h.orgStore.GetOrgMember(ctx, orgID, targetUserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check membership"})
		return
	}
	if targetMember == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "member not found"})
		return
	}

	if targetMember.Role == types.OrgRoleAdmin {
		removed, err := h.orgStore.RemoveOrgAdminIfNotLast(ctx, orgID, targetUserID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to remove admin"})
			return
		}
		if !removed {
			c.JSON(http.StatusConflict, gin.H{"error": "cannot remove the last org admin"})
			return
		}
	} else {
		if err := h.orgStore.RemoveOrgMember(ctx, orgID, targetUserID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to remove member"})
			return
		}
	}

	c.Status(http.StatusNoContent)
}

// ChangeMemberRole handles PUT /api/v1/orgs/:id/members/:userID.
func (h *OrgsHandler) ChangeMemberRole(c *gin.Context) {
	orgID := c.Param("id")
	targetUserID := c.Param("userID")
	callerUserID := h.authSvc.GetUserID(c)
	ctx := c.Request.Context()

	var req types.ChangeOrgMemberRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, bindingErrorResponse(err, &req))
		return
	}

	if req.Role != types.OrgRoleAdmin && req.Role != types.OrgRoleMember {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role must be 'admin' or 'member'"})
		return
	}

	target, err := h.orgStore.GetOrgMember(ctx, orgID, targetUserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check membership"})
		return
	}
	if target == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "member not found"})
		return
	}

	if target.Role == req.Role {
		c.JSON(http.StatusConflict, gin.H{"error": "member already has this role"})
		return
	}

	if req.Role == types.OrgRoleAdmin {
		if err := h.orgStore.UpdateOrgMemberRole(ctx, orgID, targetUserID, types.OrgRoleAdmin); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to promote member"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "Member role updated"})
		return
	}

	if targetUserID == callerUserID {
		c.JSON(http.StatusConflict, gin.H{"error": "org admins cannot demote themselves"})
		return
	}

	demoted, err := h.orgStore.DemoteOrgAdminIfNotLast(ctx, orgID, targetUserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to demote admin"})
		return
	}
	if !demoted {
		c.JSON(http.StatusConflict, gin.H{"error": "cannot demote the last org admin"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Member role updated"})
}

// VerifyMember handles POST /api/v1/orgs/:id/members/:userID/verify.
//
// Org-admin only (OrgAdminGuard middleware). Marks the member's user account
// as email_verified=true, bypassing the email-verification token flow. Use
// cases: the admin has confirmed the member's identity out-of-band (e.g. in
// person, via a trusted channel), or the member cannot receive the
// verification email and the admin chooses to override.
//
// Idempotent: verifying an already-verified member returns 200. The action
// is recorded in the org audit log (domain='org', action='member.verify')
// with the actor and target IDs so the override is traceable.
func (h *OrgsHandler) VerifyMember(c *gin.Context) {
	orgID := c.Param("id")
	targetUserID := c.Param("userID")
	callerUserID := h.authSvc.GetUserID(c)
	ctx := c.Request.Context()

	target, err := h.orgStore.GetOrgMember(ctx, orgID, targetUserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check membership"})
		return
	}
	if target == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "member not found"})
		return
	}

	if err := h.orgStore.MarkUserEmailVerified(ctx, targetUserID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify member"})
		return
	}

	if err := h.orgStore.LogOrgEvent(ctx, orgID, callerUserID, "member.verify", targetUserID, map[string]any{
		"email": target.Email,
	}); err != nil && h.logger != nil {
		// Non-fatal: the verification succeeded; only the audit trail is
		// missing. Surface it so operators can investigate, but do not undo
		// the verification — the admin's intent was recorded on the user row.
		h.logger.Warn("audit log emission failed", "action", "member.verify", "orgID", orgID, "targetUserID", targetUserID, "error", err.Error())
	}

	c.JSON(http.StatusOK, gin.H{"message": "Member verified"})
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// IsOrgMember satisfies the middleware.orgMemberChecker interface by delegating to orgStore.
func (h *OrgsHandler) IsOrgMember(ctx context.Context, orgID, userID string) (bool, error) {
	return h.orgStore.IsOrgMember(ctx, orgID, userID)
}

// IsOrgAdmin satisfies the middleware.orgMemberChecker interface by delegating to orgStore.
func (h *OrgsHandler) IsOrgAdmin(ctx context.Context, orgID, userID string) (bool, error) {
	return h.orgStore.IsOrgAdmin(ctx, orgID, userID)
}
