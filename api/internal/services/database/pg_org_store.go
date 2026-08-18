// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/pgarray"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// PendingOrgCleanup describes a pending_activation org eligible for the cleanup
// cron. StripeCustomerID lets the cron verify checkout state with Stripe before
// deleting.
type PendingOrgCleanup struct {
	OrgID            string
	Slug             string
	CreatedAt        time.Time
	StripeCustomerID string
}

// OrgStore is the data-access interface for organizations and their memberships.
type OrgStore interface {
	CreateOrgWithAdmin(ctx context.Context, org *types.Organization, adminUserID string) (*types.Organization, error)
	GetOrg(ctx context.Context, orgID string) (*types.Organization, error)
	GetOrgBySlug(ctx context.Context, slug string) (*types.Organization, error)
	ListOrgsForUser(ctx context.Context, userID string) ([]*types.OrgResponse, error)
	UpdateOrg(ctx context.Context, orgID string, req types.UpdateOrgRequest) (*types.Organization, error)
	SoftDeleteOrg(ctx context.Context, orgID string) error

	AddOrgMember(ctx context.Context, orgID, userID string, role types.OrgRole) error
	GetOrgMember(ctx context.Context, orgID, userID string) (*types.OrgMember, error)
	ListOrgMembers(ctx context.Context, orgID string) ([]*types.OrgMember, error)
	// CountOrgAdmins returns the number of admin members in an active (non-
	// deleted) org. Used by the SSO login flow to avoid demoting the last admin
	// on an IdP-driven role change (org orphaning prevention, cf. D19).
	CountOrgAdmins(ctx context.Context, orgID string) (int, error)
	UpdateOrgMemberRole(ctx context.Context, orgID, userID string, role types.OrgRole) error
	RemoveOrgMember(ctx context.Context, orgID, userID string) error
	RemoveOrgAdminIfNotLast(ctx context.Context, orgID, targetUserID string) (bool, error)
	DemoteOrgAdminIfNotLast(ctx context.Context, orgID, targetUserID string) (bool, error)
	IsOrgMember(ctx context.Context, orgID, userID string) (bool, error)
	IsOrgAdmin(ctx context.Context, orgID, userID string) (bool, error)
	ListOrgWorkspaces(ctx context.Context, orgID string, limit, offset int) ([]*types.WorkspaceMetadata, *types.PaginationMetadata, error)
	// GetUserIDByEmail resolves an owner email to a user ID for admin-driven org
	// creation (design 0031 D1). Returns ("", nil) when no user matches. This is
	// a single targeted lookup, never a search/list endpoint, to prevent account
	// enumeration. Users are hard-deleted (no deleted_at column), so no soft-delete
	// filter is needed.
	GetUserIDByEmail(ctx context.Context, email string) (string, error)
	// GetUserEmail resolves a user ID to their email (inverse of GetUserIDByEmail).
	// Used by invitation acceptance to verify email binding.
	GetUserEmail(ctx context.Context, userID string) (string, error)
	// MarkUserEmailVerified sets users.email_verified=true for the given user,
	// bypassing the email-verification token flow. Used by the org-admin
	// "Verify" action when an admin has confirmed the member's identity
	// out-of-band. Idempotent.
	MarkUserEmailVerified(ctx context.Context, userID string) error
	// GetUserOrgID returns the user's single org ID (or "" if not in any org).
	// With single-org enforcement (D8), a user belongs to at most one org. Used
	// by invitation acceptance (S3 cross-org check) and workspace auto-attribution
	// (D4). Returns ("", nil) on no membership. S7 in 0034.
	GetUserOrgID(ctx context.Context, userID string) (string, error)

	// US-43.1: Stripe lifecycle. UpdateOrgStatus sets the operational status
	// (active/suspended) and/or subscription_status and/or plan_id. A nil/empty
	// argument leaves the column unchanged.
	UpdateOrgStatus(ctx context.Context, orgID string, status *types.OrgStatus, subStatus *types.OrgSubscriptionStatus, planID *types.OrgPlan) error
	// GetOrgIDByStripeCustomer resolves a Stripe customer ID to the owning org's
	// ID via billing_accounts. Returns ("", nil) when no row matches.
	GetOrgIDByStripeCustomer(ctx context.Context, stripeCustomerID string) (string, error)
	// GetStripeCustomerID resolves an org to its Stripe customer id via
	// billing_accounts. Returns ("", nil) when no billing account exists.
	GetStripeCustomerID(ctx context.Context, orgID string) (string, error)
	// --- US-43.2: Invitations ---
	CreateInvitation(ctx context.Context, inv *types.OrgInvitation) error
	ListPendingInvitations(ctx context.Context, orgID string) ([]*types.OrgInvitation, error)
	GetInvitationByTokenHash(ctx context.Context, tokenHash string) (*types.OrgInvitation, error)
	GetInvitationByID(ctx context.Context, invID string) (*types.OrgInvitation, error)
	// AcceptInvitation performs the accept flow atomically under FOR UPDATE:
	// locks the invitation row, re-checks it is still pending, inserts the
	// membership, and marks the invitation accepted. Returns
	// (membership, alreadyAccepted, error).
	AcceptInvitationTx(ctx context.Context, invID, userID string, role types.OrgRole) (*types.OrgMember, bool, error)
	DeclineInvitation(ctx context.Context, invID string) error
	DeleteInvitation(ctx context.Context, invID string) error
	CountInvitationsLastHour(ctx context.Context, orgID string) (int, error)

	// --- US-43.7: Policies ---
	GetOrgPolicies(ctx context.Context, orgID string) ([]*types.OrgPolicy, error)
	SetOrgPolicy(ctx context.Context, orgID string, key types.OrgPolicyKey, value json.RawMessage, updatedBy string) error
	DeleteOrgPolicy(ctx context.Context, orgID string, key types.OrgPolicyKey) error

	// --- US-43.13: Org-scoped audit log ---
	LogOrgEvent(ctx context.Context, orgID, actorID, action, targetID string, metadata map[string]any) error
	// LogAuditEvent is the general audit writer (US-43.19). domain must be one
	// of audit_log_domain_chk's allowed values; orgID is nil for platform-level
	// (non-org-scoped) events.
	LogAuditEvent(ctx context.Context, domain, actorID, action, targetID string, orgID *string, metadata map[string]any) error
	ListOrgAudit(ctx context.Context, orgID string, limit, offset int) ([]*types.AuditEntry, *types.PaginationMetadata, error)
	// US-43.20: cross-org audit. ListAllAudit returns audit_log rows across all
	// orgs, narrowed by the supplied filters (nil pointers ⇒ no filter). Limit
	// defaults to 100 and is clamped to [1, 500]; Offset defaults to 0.
	ListAllAudit(ctx context.Context, filters types.AuditFilters) ([]*types.AuditEntry, *types.PaginationMetadata, error)
	// US-43.19: last-admin deadlock prevention. Returns orgs where the given
	// user is the sole active admin — suspending them would orphan the org.
	OrgsWhereUserIsLastActiveAdmin(ctx context.Context, userID string) ([]types.LastAdminOrg, error)
	// US-43.18: platform-admin dashboard. ListAllOrgs returns every
	// non-deleted org with aggregated member + workspace counts, optionally
	// narrowed by status. statusFilter is applied only when non-nil/non-empty.
	// limit is clamped to [1, adminListMaxLimit]; offset defaults to 0.
	ListAllOrgs(ctx context.Context, limit, offset int, statusFilter *string) ([]types.OrgSummary, *types.PaginationMetadata, error)

	// --- US-43.10: OIDC SSO configuration ---
	// GetSSOConfig returns the org's SSO config or (nil, nil) when none exists.
	GetSSOConfig(ctx context.Context, orgID string) (*types.OrgSSOConfig, error)
	// UpsertSSOConfig inserts or replaces the org's SSO config. ClientSecret is
	// the already-encrypted blob (server KEK, D17-S4). VerifiedDomains is the
	// caller-computed subset of ClaimedDomains that remain verified after this
	// update (the service layer intersects existing verified with new claimed).
	// VerificationToken is generated on INSERT if empty; ON CONFLICT preserves
	// the existing token (rotation is via RotateVerificationToken).
	UpsertSSOConfig(ctx context.Context, config *types.OrgSSOConfig) error
	// DeleteSSOConfig removes the org's SSO config.
	DeleteSSOConfig(ctx context.Context, orgID string) error
	// FindSSOConfigByDomain resolves a claimed email domain (without leading
	// "@") to the owning org's SSO config. Returns (nil, nil) when no org has
	// claimed the domain.
	FindSSOConfigByDomain(ctx context.Context, domain string) (*types.OrgSSOConfig, error)
	// ListSSODomains returns every DNS-verified domain across all orgs, for
	// the login-page discovery endpoint. Unverified claimed domains are NOT
	// returned — they cannot auto-route until the org admin completes DNS
	// verification (D17 Q-S2).
	ListSSODomains(ctx context.Context) ([]types.SSODomain, error)
	// CountSSOConfigs returns the number of orgs with an SSO config. Used by
	// GET /auth/config to set the OIDCEnabled feature flag.
	CountSSOConfigs(ctx context.Context) (int, error)
	// SetDomainVerified atomically appends a domain to verified_domains. The
	// domain MUST already be in claimed_domains (enforced by the WHERE clause);
	// a domain not in claimed_domains is silently not added. Idempotent: adding
	// an already-verified domain is a no-op. Returns (true, nil) if the domain
	// was newly verified, (false, nil) if it was already verified or not claimed.
	SetDomainVerified(ctx context.Context, orgID, domain string) (bool, error)
	// RotateVerificationToken replaces the org's DNS verification token with a
	// fresh random value and returns it. Used both for initial token creation
	// (when verification_token is NULL) and for rotation. Old tokens stop
	// matching after rotation — admins must update their DNS TXT record.
	RotateVerificationToken(ctx context.Context, orgID string) (string, error)
}

// PgOrgStore implements OrgStore using database/sql.
type PgOrgStore struct {
	db *sql.DB
}

// NewPgOrgStore creates a new PgOrgStore.
func NewPgOrgStore(db *sql.DB) *PgOrgStore {
	return &PgOrgStore{db: db}
}

func (s *PgOrgStore) CreateOrgWithAdmin(ctx context.Context, org *types.Organization, adminUserID string) (*types.Organization, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	err = tx.QueryRowContext(ctx,
		`INSERT INTO organizations (id, name, slug, created_by, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, NOW(), NOW())
		 RETURNING id, name, slug, created_by, created_at, updated_at, status, plan_id, subscription_status`,
		org.ID, org.Name, org.Slug, org.CreatedBy,
	).Scan(&org.ID, &org.Name, &org.Slug, &org.CreatedBy, &org.CreatedAt, &org.UpdatedAt,
		&org.Status, &org.PlanID, &org.SubscriptionStatus)
	if err != nil {
		return nil, fmt.Errorf("insert organization: %w", err)
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO org_memberships (org_id, user_id, role, created_at)
		 VALUES ($1, $2, 'admin', NOW())`,
		org.ID, adminUserID,
	)
	if err != nil {
		return nil, fmt.Errorf("insert org membership: %w", err)
	}

	// D4: migrate the owner's existing personal workspaces to the org (same as
	// AcceptInvitationTx). Keeps the two "join the org" paths consistent.
	if _, err := tx.ExecContext(ctx,
		`UPDATE workspaces SET org_id = $2, updated_at = NOW()
		 WHERE user_id = $1 AND org_id IS NULL AND deleted_at IS NULL`,
		adminUserID, org.ID,
	); err != nil {
		return nil, fmt.Errorf("migrate owner personal workspaces to org: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit transaction: %w", err)
	}

	return org, nil
}

func (s *PgOrgStore) GetOrg(ctx context.Context, orgID string) (*types.Organization, error) {
	var org types.Organization
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, slug, created_by, created_at, updated_at, status, plan_id, subscription_status
		 FROM organizations WHERE id = $1 AND deleted_at IS NULL`,
		orgID,
	).Scan(&org.ID, &org.Name, &org.Slug, &org.CreatedBy, &org.CreatedAt, &org.UpdatedAt,
		&org.Status, &org.PlanID, &org.SubscriptionStatus)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get organization: %w", err)
	}
	return &org, nil
}

func (s *PgOrgStore) GetOrgBySlug(ctx context.Context, slug string) (*types.Organization, error) {
	var org types.Organization
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, slug, created_by, created_at, updated_at, status, plan_id, subscription_status
		 FROM organizations WHERE LOWER(slug) = LOWER($1) AND deleted_at IS NULL`,
		slug,
	).Scan(&org.ID, &org.Name, &org.Slug, &org.CreatedBy, &org.CreatedAt, &org.UpdatedAt,
		&org.Status, &org.PlanID, &org.SubscriptionStatus)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get organization by slug: %w", err)
	}
	return &org, nil
}

func (s *PgOrgStore) ListOrgsForUser(ctx context.Context, userID string) ([]*types.OrgResponse, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT o.id, o.name, o.slug, o.created_by, o.created_at, o.updated_at,
		        o.status, o.plan_id, o.subscription_status,
		        m.role,
		        COUNT(m2.user_id) AS member_count
		 FROM organizations o
		 JOIN org_memberships m ON m.org_id = o.id AND m.user_id = $1
		 JOIN org_memberships m2 ON m2.org_id = o.id
		 WHERE o.deleted_at IS NULL
		 GROUP BY o.id, o.name, o.slug, o.created_by, o.created_at, o.updated_at,
		          o.status, o.plan_id, o.subscription_status, m.role
		 ORDER BY o.created_at DESC`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list orgs for user: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var orgs []*types.OrgResponse
	for rows.Next() {
		var r types.OrgResponse
		if err := rows.Scan(
			&r.ID, &r.Name, &r.Slug, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt,
			&r.Status, &r.PlanID, &r.SubscriptionStatus,
			&r.UserRole,
			&r.MemberCount,
		); err != nil {
			return nil, fmt.Errorf("scan org row: %w", err)
		}
		orgs = append(orgs, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate org rows: %w", err)
	}
	if orgs == nil {
		orgs = []*types.OrgResponse{}
	}
	return orgs, nil
}

func (s *PgOrgStore) UpdateOrg(ctx context.Context, orgID string, req types.UpdateOrgRequest) (*types.Organization, error) {
	var org types.Organization
	err := s.db.QueryRowContext(ctx,
		`UPDATE organizations
		 SET name      = CASE WHEN $2 != '' THEN $2 ELSE name END,
		     slug      = CASE WHEN $3 != '' THEN LOWER($3) ELSE slug END,
		     updated_at = NOW()
		 WHERE id = $1 AND deleted_at IS NULL
		 RETURNING id, name, slug, created_by, created_at, updated_at, status, plan_id, subscription_status`,
		orgID, req.Name, req.Slug,
	).Scan(&org.ID, &org.Name, &org.Slug, &org.CreatedBy, &org.CreatedAt, &org.UpdatedAt,
		&org.Status, &org.PlanID, &org.SubscriptionStatus)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("update organization: %w", err)
	}
	return &org, nil
}

func (s *PgOrgStore) SoftDeleteOrg(ctx context.Context, orgID string) error {
	// F6: workspaces keep their org_id after org deletion. The org is
	// soft-deleted (deleted_at set), so IsOrgMember returns false for all
	// members → workspaces become frozen (no access by anyone). This prevents
	// former members from walking away with org workspaces as personal ones.
	_, err := s.db.ExecContext(ctx,
		`UPDATE organizations SET deleted_at = NOW() WHERE id = $1`, orgID,
	)
	if err != nil {
		return fmt.Errorf("soft delete organization: %w", err)
	}
	return nil
}

// AddOrgMember inserts an org_memberships row AND migrates the new
// member's personal (NULL-org_id, non-deleted) workspaces to the org —
// mirroring CreateOrgWithAdmin (M1/D4) and AcceptInvitationTx (D4).
// Both operations run in a single transaction so the membership and
// the workspace migration commit atomically.
//
// Callers (audited 2026-06-25):
//   - admin "Add member" UI → POST /orgs/:id/members → orgs.go handler
//   - SSO JIT provisioning during first login (sso.go)
//
// Pre-fix: this function only INSERTed the membership row. The new
// member's existing workspaces stayed with org_id IS NULL forever,
// silently skipping org-credential auto-binding via
// BindCredentialToAllOrgWorkspaces (which filters w.org_id = $orgID).
// That produced the same orphan state migration 000044 was created
// to backfill. See the worklog `add-org-member-migrates-workspaces`
// for the discovery trail.
func (s *PgOrgStore) AddOrgMember(ctx context.Context, orgID, userID string, role types.OrgRole) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO org_memberships (org_id, user_id, role, created_at)
		 VALUES ($1, $2, $3, NOW())`,
		orgID, userID, role,
	); err != nil {
		return fmt.Errorf("add org member: %w", err)
	}

	// M2: migrate the new member's personal workspaces to the org.
	// Identical UPDATE to the D4 block in CreateOrgWithAdmin and
	// AcceptInvitationTx so the three join paths are behaviorally
	// identical from the workspace's point of view. Search the file
	// for "UPDATE workspaces SET org_id" to find the other two —
	// line numbers drift; structural search is reliable.
	if _, err := tx.ExecContext(ctx,
		`UPDATE workspaces SET org_id = $2, updated_at = NOW()
		 WHERE user_id = $1 AND org_id IS NULL AND deleted_at IS NULL`,
		userID, orgID,
	); err != nil {
		return fmt.Errorf("migrate new member's personal workspaces to org: %w", err)
	}

	committed = true
	return tx.Commit()
}

func (s *PgOrgStore) GetOrgMember(ctx context.Context, orgID, userID string) (*types.OrgMember, error) {
	var m types.OrgMember
	err := s.db.QueryRowContext(ctx,
		`SELECT m.org_id, m.user_id, u.username, u.email, m.role, u.email_verified, m.created_at
		 FROM org_memberships m
		 JOIN users u ON u.id = m.user_id
		 WHERE m.org_id = $1 AND m.user_id = $2`,
		orgID, userID,
	).Scan(&m.OrgID, &m.UserID, &m.Username, &m.Email, &m.Role, &m.EmailVerified, &m.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get org member: %w", err)
	}
	return &m, nil
}

func (s *PgOrgStore) ListOrgMembers(ctx context.Context, orgID string) ([]*types.OrgMember, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.org_id, m.user_id, u.username, u.email, m.role, u.email_verified, m.created_at
		 FROM org_memberships m
		 JOIN users u ON u.id = m.user_id
		 WHERE m.org_id = $1
		 ORDER BY m.created_at ASC`,
		orgID,
	)
	if err != nil {
		return nil, fmt.Errorf("list org members: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var members []*types.OrgMember
	for rows.Next() {
		var m types.OrgMember
		if err := rows.Scan(&m.OrgID, &m.UserID, &m.Username, &m.Email, &m.Role, &m.EmailVerified, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan org member: %w", err)
		}
		members = append(members, &m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate org member rows: %w", err)
	}
	if members == nil {
		members = []*types.OrgMember{}
	}
	return members, nil
}

func (s *PgOrgStore) UpdateOrgMemberRole(ctx context.Context, orgID, userID string, role types.OrgRole) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE org_memberships SET role = $3 WHERE org_id = $1 AND user_id = $2`,
		orgID, userID, role,
	)
	if err != nil {
		return fmt.Errorf("update org member role: %w", err)
	}
	return nil
}

func (s *PgOrgStore) CountOrgAdmins(ctx context.Context, orgID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM org_memberships m
		 JOIN organizations o ON o.id = m.org_id
		 WHERE m.org_id = $1 AND m.role = 'admin' AND o.deleted_at IS NULL`,
		orgID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count org admins: %w", err)
	}
	return count, nil
}

func (s *PgOrgStore) RemoveOrgMember(ctx context.Context, orgID, userID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM org_memberships WHERE org_id = $1 AND user_id = $2`,
		orgID, userID,
	); err != nil {
		return fmt.Errorf("delete org membership: %w", err)
	}

	committed = true
	return tx.Commit()
}

func (s *PgOrgStore) RemoveOrgAdminIfNotLast(ctx context.Context, orgID, targetUserID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	adminRows, err := tx.QueryContext(ctx,
		`SELECT user_id FROM org_memberships WHERE org_id = $1 AND role = 'admin' FOR UPDATE`,
		orgID,
	)
	if err != nil {
		return false, fmt.Errorf("lock admin rows: %w", err)
	}
	adminCount := 0
	for adminRows.Next() {
		adminCount++
	}
	closeErr := adminRows.Close() //nolint:sqlclosecheck // must close before next tx query; defer would close too late
	if err := adminRows.Err(); err != nil {
		return false, fmt.Errorf("iterate admin rows: %w", err)
	}
	if closeErr != nil {
		return false, fmt.Errorf("close admin rows: %w", closeErr)
	}

	var targetRole string
	err = tx.QueryRowContext(ctx,
		`SELECT role FROM org_memberships WHERE org_id = $1 AND user_id = $2`,
		orgID, targetUserID,
	).Scan(&targetRole)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get target member role: %w", err)
	}

	if targetRole == "admin" && adminCount <= 1 {
		return false, nil
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM org_memberships WHERE org_id = $1 AND user_id = $2`,
		orgID, targetUserID,
	); err != nil {
		return false, fmt.Errorf("delete org membership: %w", err)
	}

	committed = true
	return true, tx.Commit()
}

func (s *PgOrgStore) DemoteOrgAdminIfNotLast(ctx context.Context, orgID, targetUserID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	adminRows, err := tx.QueryContext(ctx,
		`SELECT user_id FROM org_memberships WHERE org_id = $1 AND role = 'admin' FOR UPDATE`,
		orgID,
	)
	if err != nil {
		return false, fmt.Errorf("lock admin rows: %w", err)
	}
	adminCount := 0
	for adminRows.Next() {
		adminCount++
	}
	closeErr := adminRows.Close() //nolint:sqlclosecheck // must close before next tx query; defer would close too late
	if err := adminRows.Err(); err != nil {
		return false, fmt.Errorf("iterate admin rows: %w", err)
	}
	if closeErr != nil {
		return false, fmt.Errorf("close admin rows: %w", closeErr)
	}

	if adminCount <= 1 {
		return false, nil
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE org_memberships SET role = 'member' WHERE org_id = $1 AND user_id = $2`,
		orgID, targetUserID,
	); err != nil {
		return false, fmt.Errorf("demote org admin: %w", err)
	}

	committed = true
	return true, tx.Commit()
}

// queryer is the subset of *sql.DB / *sql.Tx needed to run the last-admin
// lookup. Sharing the query between the standalone read and the transactional
// suspend lets us guarantee atomicity without duplicating the SQL.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
}

// lastActiveAdminOrgsQuery is the SQL that finds every org where userID is an
// admin AND no OTHER active admin exists. It is shared by the standalone
// OrgsWhereUserIsLastActiveAdmin read and the transactional
// SuspendUserGuardedByLastAdmin write (F7) so the two cannot drift.
//
// "Active admin" means role='admin' with users.status='active'. The legacy
// pending_key_wrap filter from D19 is intentionally NOT used: that column was
// dropped in migration 000035, and the authoritative gate is now users.status.
// Soft-deleted orgs are excluded (their memberships are irrelevant).
const lastActiveAdminOrgsQuery = `SELECT m.org_id, o.name
		 FROM org_memberships m
		 JOIN organizations o ON o.id = m.org_id
		 WHERE m.user_id = $1 AND m.role = 'admin' AND o.deleted_at IS NULL
		   AND NOT EXISTS (
		     SELECT 1 FROM org_memberships m2
		     JOIN users u ON u.id = m2.user_id
		     WHERE m2.org_id = m.org_id AND m2.role = 'admin'
		       AND m2.user_id <> m.user_id AND u.status = 'active'
		   )`

func scanLastActiveAdminOrgs(ctx context.Context, q queryer, userID string) ([]types.LastAdminOrg, error) {
	rows, err := q.QueryContext(ctx, lastActiveAdminOrgsQuery, userID)
	if err != nil {
		return nil, fmt.Errorf("find last-active-admin orgs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var orgs []types.LastAdminOrg
	for rows.Next() {
		var lo types.LastAdminOrg
		if err := rows.Scan(&lo.OrgID, &lo.OrgName); err != nil {
			return nil, fmt.Errorf("scan last-active-admin org: %w", err)
		}
		orgs = append(orgs, lo)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate last-active-admin orgs: %w", err)
	}
	if orgs == nil {
		orgs = []types.LastAdminOrg{}
	}
	return orgs, nil
}

// OrgsWhereUserIsLastActiveAdmin returns every organization where the given
// user is an admin AND no OTHER active admin exists. Suspending such a user
// (D19) would orphan the org — no remaining admin could manage it (promote
// members, change policies, manage billing). The user-suspend path refuses
// with 409 when this returns a non-empty slice (unless force=true).
func (s *PgOrgStore) OrgsWhereUserIsLastActiveAdmin(ctx context.Context, userID string) ([]types.LastAdminOrg, error) {
	return scanLastActiveAdminOrgs(ctx, s.db, userID)
}

// SuspendUserGuardedByLastAdmin atomically refuses to suspend the user when
// they are the sole active admin of any org (unless force), and otherwise sets
// the user's status to suspended. The SELECT … FOR UPDATE on the admin
// membership rows of every org the user administers plus the UPDATE on users
// run in a single transaction, closing the TOCTOU window of the prior
// read-then-write sequence (F7, US-43.19): two concurrent admin suspensions or
// a suspend racing a demote can no longer both pass the last-admin check and
// leave the org adminless. `active` is mirrored to `false` so the legacy column
// cannot drift from `status` (F6).
//
// Returns a non-nil *LastAdminOrg when the suspend was refused (last admin); the
// caller surfaces this as 409. Returns (nil, nil) on a successful suspension.
func (s *PgOrgStore) SuspendUserGuardedByLastAdmin(ctx context.Context, userID string, force bool) (*types.LastAdminOrg, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if !force {
		// Lock ALL admin rows of every org the user administers so a concurrent
		// suspend/demote of another admin in any of those orgs serializes behind
		// this transaction and re-evaluates the last-admin check against the
		// post-commit state. This is the same locking discipline as
		// RemoveOrgAdminIfNotLast/DemoteOrgAdminIfNotLast, generalized to the
		// multi-org case.
		lockRows, err := tx.QueryContext(ctx,
			`SELECT m.org_id FROM org_memberships m
			 WHERE m.role = 'admin'
			   AND m.org_id IN (SELECT org_id FROM org_memberships WHERE user_id = $1 AND role = 'admin')
			 FOR UPDATE`,
			userID,
		)
		if err != nil {
			return nil, fmt.Errorf("lock admin rows: %w", err)
		}
		// Drain + check Err() to surface driver-level errors from the lock
		// query (matches the rows.Err() discipline in RemoveOrgAdminIfNotLast /
		// DemoteOrgAdminIfNotLast). The rows themselves are not read — the lock
		// is the FOR UPDATE side effect — but Err() catches execution failures.
		for lockRows.Next() {
		}
		if err := lockRows.Err(); err != nil {
			_ = lockRows.Close() //nolint:sqlclosecheck // best-effort cleanup before early return on iteration error
			return nil, fmt.Errorf("iterate admin lock rows: %w", err)
		}
		if err := lockRows.Close(); err != nil { //nolint:sqlclosecheck // must close before next tx query; defer would close too late
			return nil, fmt.Errorf("close admin lock rows: %w", err)
		}

		conflicts, err := scanLastActiveAdminOrgs(ctx, tx, userID)
		if err != nil {
			return nil, err
		}
		if len(conflicts) > 0 {
			// Refuse — defer rolls the tx back; no state changes.
			return &conflicts[0], nil
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET status = 'suspended', active = false, updated_at = NOW() WHERE id = $1`,
		userID,
	); err != nil {
		return nil, fmt.Errorf("suspend user: %w", err)
	}

	committed = true
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit suspend user: %w", err)
	}
	return nil, nil
}

func (s *PgOrgStore) IsOrgMember(ctx context.Context, orgID, userID string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(
		   SELECT 1 FROM org_memberships m
		   JOIN organizations o ON o.id = m.org_id
		   WHERE m.org_id = $1 AND m.user_id = $2 AND o.deleted_at IS NULL
		     AND o.status != 'suspended'
		 )`,
		orgID, userID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check org membership: %w", err)
	}
	return exists, nil
}

func (s *PgOrgStore) IsOrgAdmin(ctx context.Context, orgID, userID string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(
		   SELECT 1 FROM org_memberships m
		   JOIN organizations o ON o.id = m.org_id
		   WHERE m.org_id = $1 AND m.user_id = $2
		     AND m.role = 'admin'
		     AND o.deleted_at IS NULL
		     AND o.status != 'suspended'
		 )`,
		orgID, userID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check org admin: %w", err)
	}
	return exists, nil
}

func (s *PgOrgStore) ListOrgWorkspaces(ctx context.Context, orgID string, limit, offset int) ([]*types.WorkspaceMetadata, *types.PaginationMetadata, error) {
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workspaces WHERE org_id = $1 AND deleted_at IS NULL`,
		orgID,
	).Scan(&total); err != nil {
		return nil, nil, fmt.Errorf("count org workspaces: %w", err)
	}

	pagination := &types.PaginationMetadata{
		Total:  total,
		Start:  offset,
		End:    offset + limit,
		Limit:  limit,
		Offset: offset,
	}
	if pagination.End > total {
		pagination.End = total
	}

	if total == 0 {
		return []*types.WorkspaceMetadata{}, pagination, nil
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT w.id, w.user_id, w.name, w.runtime, w.storage_size,
		        COALESCE(w.image_tag, '') AS image_tag,
		        COALESCE(w.agent_version, '') AS agent_version,
		        w.created_at, w.updated_at,
		        COALESCE(w.default_model, '') AS default_model,
		        COALESCE(s.pending_refresh, FALSE) AS agent_needs_refresh,
		        s.last_credential_changed_at AS credentials_pending_since,
		        w.org_id
		 FROM workspaces w
		 LEFT JOIN workspace_agent_state s ON s.workspace_id = w.id
		 WHERE w.org_id = $1 AND w.deleted_at IS NULL
		 ORDER BY w.created_at DESC
		 LIMIT $2 OFFSET $3`,
		orgID, limit, offset,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("list org workspaces: %w", err)
	}
	defer func() { _ = rows.Close() }()

	workspaces := make([]*types.WorkspaceMetadata, 0)
	for rows.Next() {
		var ws types.WorkspaceMetadata
		if err := rows.Scan(
			&ws.ID, &ws.UserID, &ws.Name, &ws.Runtime,
			&ws.StorageSize, &ws.ImageTag, &ws.AgentVersion,
			&ws.CreatedAt, &ws.UpdatedAt, &ws.DefaultModel,
			&ws.AgentNeedsRefresh, &ws.CredentialsPendingSince,
			&ws.OrgID,
		); err != nil {
			return nil, nil, fmt.Errorf("scan org workspace row: %w", err)
		}
		workspaces = append(workspaces, &ws)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate org workspace rows: %w", err)
	}

	return workspaces, pagination, nil
}

func (s *PgOrgStore) GetUserIDByEmail(ctx context.Context, email string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM users WHERE email = $1`,
		email,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get user id by email: %w", err)
	}
	return id, nil
}

func (s *PgOrgStore) GetUserEmail(ctx context.Context, userID string) (string, error) {
	var email string
	err := s.db.QueryRowContext(ctx,
		`SELECT email FROM users WHERE id = $1`,
		userID,
	).Scan(&email)
	if err != nil {
		return "", fmt.Errorf("get user email: %w", err)
	}
	return email, nil
}

// MarkUserEmailVerified sets users.email_verified=true for the given user,
// bypassing the email-verification token flow. Used by the org-admin "Verify"
// action (POST /orgs/:id/members/:userID/verify) when an admin has confirmed
// the member's identity out-of-band. The membership is verified by the caller
// (OrgAdminGuard + GetOrgMember) before this is invoked, so a bare userID is
// safe here. Idempotent: re-verifying an already-verified user is a no-op.
func (s *PgOrgStore) MarkUserEmailVerified(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET email_verified = TRUE, updated_at = NOW() WHERE id = $1`,
		userID,
	)
	if err != nil {
		return fmt.Errorf("mark user email verified: %w", err)
	}
	return nil
}

func (s *PgOrgStore) GetUserOrgID(ctx context.Context, userID string) (string, error) {
	var orgID string
	err := s.db.QueryRowContext(ctx,
		`SELECT org_id FROM org_memberships WHERE user_id = $1`,
		userID,
	).Scan(&orgID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get user org id: %w", err)
	}
	return orgID, nil
}

// GetUserPlan reads users.plan_id for an individual user. Used by the
// MCP-servers user-scope handler to resolve the MaxPersonalMcpServers
// quota (Epic 53 D12). Returns PlanFree on any error (fail-safe: a
// solo user whose plan can't be read gets the free-tier quota, not
// unlimited).
func (s *PgOrgStore) GetUserPlan(ctx context.Context, userID string) (string, error) {
	var plan string
	err := s.db.QueryRowContext(ctx,
		`SELECT plan_id FROM users WHERE id = $1`,
		userID,
	).Scan(&plan)
	if err == sql.ErrNoRows {
		return "free", nil
	}
	if err != nil {
		return "free", fmt.Errorf("get user plan: %w", err)
	}
	return plan, nil
}

func (s *PgOrgStore) UpdateOrgStatus(ctx context.Context, orgID string, status *types.OrgStatus, subStatus *types.OrgSubscriptionStatus, planID *types.OrgPlan) error {
	if status == nil && subStatus == nil && planID == nil {
		return nil
	}

	setParts := []string{"updated_at = NOW()"}
	args := []interface{}{orgID}
	argIdx := 2
	if status != nil {
		setParts = append(setParts, fmt.Sprintf("status = $%d", argIdx))
		args = append(args, string(*status))
		argIdx++
	}
	if subStatus != nil {
		setParts = append(setParts, fmt.Sprintf("subscription_status = $%d", argIdx))
		args = append(args, string(*subStatus))
		argIdx++
	}
	if planID != nil {
		setParts = append(setParts, fmt.Sprintf("plan_id = $%d", argIdx))
		args = append(args, string(*planID))
	}

	query := fmt.Sprintf( //nolint:gosec // G201: $N placeholder indexes only, no string interpolation of user input //nolint:gosec // setParts contains only literal column assignments, no user input
		`UPDATE organizations SET %s WHERE id = $1 AND deleted_at IS NULL`,
		strings.Join(setParts, ", "),
	)
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("update org status: %w", err)
	}
	return nil
}

func (s *PgOrgStore) GetOrgIDByStripeCustomer(ctx context.Context, stripeCustomerID string) (string, error) {
	var orgID string
	err := s.db.QueryRowContext(ctx,
		`SELECT owner_id FROM billing_accounts
		 WHERE external_customer_id = $1 AND provider = 'stripe' AND owner_type = 'org'`,
		stripeCustomerID,
	).Scan(&orgID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("lookup org by stripe customer: %w", err)
	}
	return orgID, nil
}

func (s *PgOrgStore) GetStripeCustomerID(ctx context.Context, orgID string) (string, error) {
	var customerID string
	err := s.db.QueryRowContext(ctx,
		`SELECT external_customer_id FROM billing_accounts
		 WHERE owner_id = $1 AND owner_type = 'org' AND provider = 'stripe'`,
		orgID,
	).Scan(&customerID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("lookup stripe customer for org: %w", err)
	}
	return customerID, nil
}

func (s *PgOrgStore) RecordStripeEvent(ctx context.Context, eventID, eventType string) (bool, error) {
	var inserted bool
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO stripe_events (event_id, event_type) VALUES ($1, $2)
		 ON CONFLICT (event_id) DO NOTHING
		 RETURNING TRUE`,
		eventID, eventType,
	).Scan(&inserted)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("record stripe event: %w", err)
	}
	return inserted, nil
}

func (s *PgOrgStore) DeleteStripeEvent(ctx context.Context, eventID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM stripe_events WHERE event_id = $1`,
		eventID,
	)
	if err != nil {
		return fmt.Errorf("delete stripe event: %w", err)
	}
	return nil
}

func (s *PgOrgStore) ListPendingOrgsOlderThan(ctx context.Context, maxAge time.Duration) ([]PendingOrgCleanup, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT o.id, o.slug, o.created_at,
		        COALESCE(b.external_customer_id, '') AS customer_id
		 FROM organizations o
		 LEFT JOIN billing_accounts b
		   ON b.owner_id = o.id AND b.owner_type = 'org' AND b.provider = 'stripe'
		 WHERE o.status = 'pending_activation'
		   AND o.deleted_at IS NULL
		   AND o.created_at < NOW() - make_interval(secs => $1)`,
		int(maxAge.Seconds()),
	)
	if err != nil {
		return nil, fmt.Errorf("list pending orgs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []PendingOrgCleanup
	for rows.Next() {
		var p PendingOrgCleanup
		if err := rows.Scan(&p.OrgID, &p.Slug, &p.CreatedAt, &p.StripeCustomerID); err != nil {
			return nil, fmt.Errorf("scan pending org: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending orgs: %w", err)
	}
	return out, nil
}

func (s *PgOrgStore) HardDeleteOrg(ctx context.Context, orgID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.ExecContext(ctx,
		`UPDATE workspaces SET org_id = NULL WHERE org_id = $1`, orgID,
	); err != nil {
		return fmt.Errorf("null workspace org_id: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM provider_credentials WHERE owner_type = 'org' AND owner_id = $1`, orgID,
	); err != nil {
		return fmt.Errorf("delete org provider credentials: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM billing_accounts WHERE owner_id = $1 AND owner_type = 'org'`, orgID,
	); err != nil {
		return fmt.Errorf("delete org billing accounts: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM organizations WHERE id = $1`, orgID,
	); err != nil {
		return fmt.Errorf("delete organization: %w", err)
	}

	committed = true
	return tx.Commit()
}

func (s *PgOrgStore) SetBillingAccountSubscription(ctx context.Context, ownerID, ownerType, provider, subscriptionID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE billing_accounts
		 SET external_subscription_id = $4, status = 'active', updated_at = NOW()
		 WHERE owner_id = $1 AND owner_type = $2 AND provider = $3`,
		ownerID, ownerType, provider, subscriptionID,
	)
	if err != nil {
		return fmt.Errorf("set billing account subscription: %w", err)
	}
	return nil
}

// --- US-43.2: Invitation implementations ---

func (s *PgOrgStore) CreateInvitation(ctx context.Context, inv *types.OrgInvitation) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO org_invitations (id, org_id, email, role, invited_by, token_hash, expires_at, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW())`,
		inv.ID, inv.OrgID, inv.Email, inv.Role, inv.InvitedBy, inv.TokenHash, inv.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("create invitation: %w", err)
	}
	return nil
}

func (s *PgOrgStore) ListPendingInvitations(ctx context.Context, orgID string) ([]*types.OrgInvitation, error) {
	// LEFT JOIN with users so the response carries the invitee's
	// email-verification state. The org admin UI uses this to render the
	// per-row Verify button only when force-verify is actionable
	// (existing user account whose email is unverified). Without this,
	// the UI shows a Verify button even after the override has been
	// applied because the invitation row stays pending until the user
	// clicks the invitation link.
	//
	// users.email is stored pre-lowercased and trimmed (auth.go's
	// strings.ToLower + strings.TrimSpace on signup); org_invitations.email
	// is stored as-supplied. The JOIN must apply the SAME case-folding
	// AND trimming to the invitations side as the handler does (see
	// invitations.go's VerifyUserForInvitation:
	// strings.ToLower(strings.TrimSpace(inv.Email))). Otherwise an
	// invitation stored with leading/trailing whitespace would JOIN-miss
	// even though the handler would resolve the user — resulting in the
	// UI hiding the Verify button on a row where the action would
	// actually succeed.
	rows, err := s.db.QueryContext(ctx,
		`SELECT i.id, i.org_id, i.email, i.role, i.invited_by, i.expires_at,
		        i.bounce_type, i.bounced_at, i.created_at,
		        u.id IS NOT NULL AS invitee_user_exists,
		        u.email_verified
		   FROM org_invitations i
		   LEFT JOIN users u ON u.email = LOWER(BTRIM(i.email))
		  WHERE i.org_id = $1
		    AND i.accepted_at IS NULL
		    AND i.declined_at IS NULL
		  ORDER BY i.created_at DESC`,
		orgID,
	)
	if err != nil {
		return nil, fmt.Errorf("list pending invitations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*types.OrgInvitation
	for rows.Next() {
		var inv types.OrgInvitation
		var bounceType sql.NullString
		var bouncedAt sql.NullTime
		var inviteeUserExists sql.NullBool
		var inviteeEmailVerified sql.NullBool
		if err := rows.Scan(&inv.ID, &inv.OrgID, &inv.Email, &inv.Role, &inv.InvitedBy,
			&inv.ExpiresAt, &bounceType, &bouncedAt, &inv.CreatedAt,
			&inviteeUserExists, &inviteeEmailVerified); err != nil {
			return nil, fmt.Errorf("scan invitation: %w", err)
		}
		inv.BounceType = bounceType.String
		if bouncedAt.Valid {
			inv.BouncedAt = &bouncedAt.Time
		}
		// inviteeUserExists comes back from `IS NOT NULL` so it's never
		// SQL NULL; the NullBool wrapper just guards against a hypothetical
		// driver edge case. Materialize it as a pointer so the JSON
		// surface stays consistent.
		exists := inviteeUserExists.Valid && inviteeUserExists.Bool
		inv.InviteeUserExists = &exists
		if exists && inviteeEmailVerified.Valid {
			verified := inviteeEmailVerified.Bool
			inv.InviteeEmailVerified = &verified
		}
		out = append(out, &inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate invitations: %w", err)
	}
	if out == nil {
		out = []*types.OrgInvitation{}
	}
	return out, nil
}

func (s *PgOrgStore) GetInvitationByTokenHash(ctx context.Context, tokenHash string) (*types.OrgInvitation, error) {
	return s.scanInvitation(ctx,
		`SELECT id, org_id, email, role, invited_by, expires_at, accepted_at, accepted_by,
		        declined_at, bounce_type, bounced_at, created_at
		 FROM org_invitations WHERE token_hash = $1`, tokenHash)
}

func (s *PgOrgStore) GetInvitationByID(ctx context.Context, invID string) (*types.OrgInvitation, error) {
	return s.scanInvitation(ctx,
		`SELECT id, org_id, email, role, invited_by, expires_at, accepted_at, accepted_by,
		        declined_at, bounce_type, bounced_at, created_at
		 FROM org_invitations WHERE id = $1`, invID)
}

func (s *PgOrgStore) scanInvitation(ctx context.Context, query string, args ...interface{}) (*types.OrgInvitation, error) {
	var inv types.OrgInvitation
	var acceptedAt, declinedAt, bouncedAt sql.NullTime
	var acceptedBy sql.NullString
	var bounceType sql.NullString
	err := s.db.QueryRowContext(ctx, query, args...).Scan(
		&inv.ID, &inv.OrgID, &inv.Email, &inv.Role, &inv.InvitedBy, &inv.ExpiresAt,
		&acceptedAt, &acceptedBy, &declinedAt, &bounceType, &bouncedAt, &inv.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan invitation row: %w", err)
	}
	if acceptedAt.Valid {
		inv.AcceptedAt = &acceptedAt.Time
	}
	if declinedAt.Valid {
		inv.DeclinedAt = &declinedAt.Time
	}
	if bouncedAt.Valid {
		inv.BouncedAt = &bouncedAt.Time
	}
	inv.BounceType = bounceType.String
	return &inv, nil
}

func (s *PgOrgStore) AcceptInvitationTx(ctx context.Context, invID, userID string, role types.OrgRole) (*types.OrgMember, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var orgID string
	var acceptedAt, declinedAt sql.NullTime
	err = tx.QueryRowContext(ctx,
		`SELECT org_id, accepted_at, declined_at
		 FROM org_invitations
		 WHERE id = $1 AND expires_at > NOW()
		 FOR UPDATE`,
		invID,
	).Scan(&orgID, &acceptedAt, &declinedAt)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("lock invitation: %w", err)
	}
	if acceptedAt.Valid || declinedAt.Valid {
		return nil, true, nil
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO org_memberships (org_id, user_id, role, created_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (org_id, user_id) DO NOTHING`,
		orgID, userID, role,
	); err != nil {
		return nil, false, fmt.Errorf("insert membership: %w", err)
	}

	// D4: migrate the user's personal workspaces to the org. Single atomic
	// UPDATE inside the accept transaction — if anything else fails, the
	// migration rolls back too. Only non-deleted workspaces are migrated.
	if _, err := tx.ExecContext(ctx,
		`UPDATE workspaces SET org_id = $2, updated_at = NOW()
		 WHERE user_id = $1 AND org_id IS NULL AND deleted_at IS NULL`,
		userID, orgID,
	); err != nil {
		return nil, false, fmt.Errorf("migrate personal workspaces to org: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE org_invitations SET accepted_at = NOW(), accepted_by = $2 WHERE id = $1`,
		invID, userID,
	); err != nil {
		return nil, false, fmt.Errorf("mark invitation accepted: %w", err)
	}

	committed = true
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit: %w", err)
	}

	return &types.OrgMember{
		OrgID:  orgID,
		UserID: userID,
		Role:   role,
	}, false, nil
}

func (s *PgOrgStore) DeclineInvitation(ctx context.Context, invID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE org_invitations SET declined_at = NOW() WHERE id = $1
		 AND accepted_at IS NULL AND declined_at IS NULL`,
		invID,
	)
	if err != nil {
		return fmt.Errorf("decline invitation: %w", err)
	}
	return nil
}

func (s *PgOrgStore) DeleteInvitation(ctx context.Context, invID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM org_invitations WHERE id = $1 AND accepted_at IS NULL AND declined_at IS NULL`,
		invID,
	)
	if err != nil {
		return fmt.Errorf("delete invitation: %w", err)
	}
	return nil
}

func (s *PgOrgStore) CountInvitationsLastHour(ctx context.Context, orgID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM org_invitations
		 WHERE org_id = $1 AND created_at > NOW() - interval '1 hour'`,
		orgID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count invitations last hour: %w", err)
	}
	return count, nil
}

// --- US-43.7: Policy implementations ---

func (s *PgOrgStore) GetOrgPolicies(ctx context.Context, orgID string) ([]*types.OrgPolicy, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, value, updated_by, updated_at
		 FROM org_policies WHERE org_id = $1`,
		orgID,
	)
	if err != nil {
		return nil, fmt.Errorf("get org policies: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*types.OrgPolicy
	for rows.Next() {
		var p types.OrgPolicy
		var updatedBy sql.NullString
		if err := rows.Scan(&p.Key, &p.Value, &updatedBy, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan policy: %w", err)
		}
		p.OrgID = orgID
		p.UpdatedBy = updatedBy.String
		out = append(out, &p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate policies: %w", err)
	}
	if out == nil {
		out = []*types.OrgPolicy{}
	}
	return out, nil
}

func (s *PgOrgStore) SetOrgPolicy(ctx context.Context, orgID string, key types.OrgPolicyKey, value json.RawMessage, updatedBy string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO org_policies (org_id, key, value, updated_by, updated_at)
		 VALUES ($1, $2, $3, $4, NOW())
		 ON CONFLICT (org_id, key) DO UPDATE
		   SET value = EXCLUDED.value, updated_by = EXCLUDED.updated_by, updated_at = NOW()`,
		orgID, string(key), []byte(value), updatedBy,
	)
	if err != nil {
		return fmt.Errorf("set org policy: %w", err)
	}
	return nil
}

func (s *PgOrgStore) DeleteOrgPolicy(ctx context.Context, orgID string, key types.OrgPolicyKey) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM org_policies WHERE org_id = $1 AND key = $2`,
		orgID, string(key),
	)
	if err != nil {
		return fmt.Errorf("delete org policy: %w", err)
	}
	return nil
}

// --- US-43.13: Audit log implementations ---

// LogAuditEvent inserts a row into audit_log with an explicit domain and an
// optional org scope. It is the general audit writer used by both org-scoped
// events (domain='org', orgID non-nil) and platform-admin events
// (domain='admin', orgID nil). The domain must be one of the values allowed by
// the audit_log_domain_chk CHECK constraint (billing/secrets/admin/org).
func (s *PgOrgStore) LogAuditEvent(ctx context.Context, domain, actorID, action, targetID string, orgID *string, metadata map[string]any) error {
	var metaBytes []byte
	if metadata != nil {
		var err error
		metaBytes, err = json.Marshal(metadata)
		if err != nil {
			return fmt.Errorf("marshal audit metadata: %w", err)
		}
	} else {
		metaBytes = []byte(`{}`)
	}
	var oid interface{}
	if orgID != nil && *orgID != "" {
		oid = *orgID
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (actor_id, domain, action, target_id, org_id, metadata, created_at)
		 VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, NOW())`,
		actorID, domain, action, targetID, oid, metaBytes,
	)
	if err != nil {
		return fmt.Errorf("insert audit log: %w", err)
	}
	return nil
}

func (s *PgOrgStore) LogOrgEvent(ctx context.Context, orgID, actorID, action, targetID string, metadata map[string]any) error {
	return s.LogAuditEvent(ctx, "org", actorID, action, targetID, &orgID, metadata)
}

func (s *PgOrgStore) ListOrgAudit(ctx context.Context, orgID string, limit, offset int) ([]*types.AuditEntry, *types.PaginationMetadata, error) {
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE org_id = $1`,
		orgID,
	).Scan(&total); err != nil {
		return nil, nil, fmt.Errorf("count org audit entries: %w", err)
	}

	pagination := &types.PaginationMetadata{
		Total: total, Start: offset, End: offset + limit, Limit: limit, Offset: offset,
	}
	if pagination.End > total {
		pagination.End = total
	}
	if total == 0 {
		return []*types.AuditEntry{}, pagination, nil
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, actor_id, domain, action, COALESCE(target_id, ''), org_id::text, metadata, created_at
		 FROM audit_log WHERE org_id = $1
		 ORDER BY created_at DESC
		 LIMIT $2 OFFSET $3`,
		orgID, limit, offset,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("list org audit: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []*types.AuditEntry
	for rows.Next() {
		var e types.AuditEntry
		var metaBytes []byte
		if err := rows.Scan(&e.ID, &e.ActorID, &e.Domain, &e.Action, &e.TargetID, &e.OrgID, &metaBytes, &e.CreatedAt); err != nil {
			return nil, nil, fmt.Errorf("scan audit entry: %w", err)
		}
		if len(metaBytes) > 0 && string(metaBytes) != "{}" {
			if err := json.Unmarshal(metaBytes, &e.Metadata); err != nil {
				return nil, nil, fmt.Errorf("unmarshal audit metadata: %w", err)
			}
		}
		entries = append(entries, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate audit entries: %w", err)
	}
	if entries == nil {
		entries = []*types.AuditEntry{}
	}
	return entries, pagination, nil
}

// --- US-43.20: cross-org audit ---

const (
	auditDefaultLimit = 100
	auditMaxLimit     = 500
)

// ListAllAudit returns audit_log rows across every org, narrowed by filters.
// The WHERE clause is built from conditional ANDs over a parameterised args
// slice — no user input is ever interpolated into the SQL text.
func (s *PgOrgStore) ListAllAudit(ctx context.Context, filters types.AuditFilters) ([]*types.AuditEntry, *types.PaginationMetadata, error) {
	limit := filters.Limit
	if limit <= 0 {
		limit = auditDefaultLimit
	}
	if limit > auditMaxLimit {
		limit = auditMaxLimit
	}
	offset := filters.Offset
	if offset < 0 {
		offset = 0
	}

	var (
		conditions []string
		args       []interface{}
	)
	if filters.OrgID != nil && *filters.OrgID != "" {
		conditions = append(conditions, fmt.Sprintf("org_id = $%d", len(args)+1))
		args = append(args, *filters.OrgID)
	}
	if filters.ActorID != nil && *filters.ActorID != "" {
		conditions = append(conditions, fmt.Sprintf("actor_id = $%d", len(args)+1))
		args = append(args, *filters.ActorID)
	}
	if filters.Domain != nil && *filters.Domain != "" {
		conditions = append(conditions, fmt.Sprintf("domain = $%d", len(args)+1))
		args = append(args, *filters.Domain)
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = " WHERE " + strings.Join(conditions, " AND ")
	}

	var total int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM audit_log"+whereClause,
		args...,
	).Scan(&total); err != nil {
		return nil, nil, fmt.Errorf("count cross-org audit entries: %w", err)
	}

	pagination := &types.PaginationMetadata{
		Total: total, Start: offset, End: offset + limit, Limit: limit, Offset: offset,
	}
	if pagination.End > total {
		pagination.End = total
	}
	if total == 0 {
		return []*types.AuditEntry{}, pagination, nil
	}

	listArgs := append(args, limit, offset)
	limitIdx := len(args) + 1
	offsetIdx := len(args) + 2
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(
			"SELECT id, actor_id, domain, action, COALESCE(target_id, ''), org_id::text, metadata, created_at FROM audit_log%s ORDER BY created_at DESC LIMIT $%d OFFSET $%d",
			whereClause, limitIdx, offsetIdx,
		),
		listArgs...,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("list cross-org audit: %w", err)
	}
	defer func() { _ = rows.Close() }()

	entries := []*types.AuditEntry{}
	for rows.Next() {
		var e types.AuditEntry
		var metaBytes []byte
		if err := rows.Scan(&e.ID, &e.ActorID, &e.Domain, &e.Action, &e.TargetID, &e.OrgID, &metaBytes, &e.CreatedAt); err != nil {
			return nil, nil, fmt.Errorf("scan audit entry: %w", err)
		}
		if len(metaBytes) > 0 && string(metaBytes) != "{}" {
			if err := json.Unmarshal(metaBytes, &e.Metadata); err != nil {
				return nil, nil, fmt.Errorf("unmarshal audit metadata: %w", err)
			}
		}
		entries = append(entries, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate audit entries: %w", err)
	}
	return entries, pagination, nil
}

// --- US-43.18: platform-admin dashboard list ---

const (
	adminListDefaultLimit = 50
	adminListMaxLimit     = 200
)

func clampAdminLimit(limit int) int {
	if limit <= 0 {
		return adminListDefaultLimit
	}
	if limit > adminListMaxLimit {
		return adminListMaxLimit
	}
	return limit
}

// ListAllOrgs returns every non-deleted organization with aggregated member and
// workspace counts for the platform-admin dashboard. The optional statusFilter
// narrows the result to a single OrgStatus (e.g. "suspended"); an empty/nil
// filter returns all statuses. Results are ordered by created_at DESC.
//
// The two counts are correlated subqueries on the same row, so a single round
// trip returns the full summary without an N+1 fan-out. The COUNT(*) total is
// fetched first so an empty page short-circuits the SELECT.
func (s *PgOrgStore) ListAllOrgs(ctx context.Context, limit, offset int, statusFilter *string) ([]types.OrgSummary, *types.PaginationMetadata, error) {
	limit = clampAdminLimit(limit)
	if offset < 0 {
		offset = 0
	}

	var (
		countArgs    []interface{}
		countWhere   string
		listWhere    string
		listArgs     []interface{}
		statusArgIdx int
	)
	if statusFilter != nil && *statusFilter != "" {
		countArgs = append(countArgs, *statusFilter)
		countWhere = " WHERE deleted_at IS NULL AND status = $1"
		listArgs = append(listArgs, *statusFilter)
		statusArgIdx = 1
		listWhere = " WHERE deleted_at IS NULL AND status = $1"
	} else {
		countWhere = " WHERE deleted_at IS NULL"
		listWhere = " WHERE deleted_at IS NULL"
	}

	var total int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM organizations"+countWhere,
		countArgs...,
	).Scan(&total); err != nil {
		return nil, nil, fmt.Errorf("count all orgs: %w", err)
	}

	pagination := &types.PaginationMetadata{
		Total: total, Start: offset, End: offset + limit, Limit: limit, Offset: offset,
	}
	if pagination.End > total {
		pagination.End = total
	}
	if total == 0 {
		return []types.OrgSummary{}, pagination, nil
	}

	// LIMIT/OFFSET bind after the optional status parameter; their placeholder
	// indexes depend on whether the status filter is present.
	limitIdx := statusArgIdx + 1
	offsetIdx := statusArgIdx + 2
	listArgs = append(listArgs, limit, offset)
	query := fmt.Sprintf( //nolint:gosec // G201: $N placeholder indexes only, no string interpolation of user input
		`SELECT o.id, o.name, o.slug, o.created_by, o.created_at, o.updated_at,
		        o.status, o.plan_id, o.subscription_status,
		        (SELECT COUNT(*) FROM org_memberships m WHERE m.org_id = o.id) AS member_count,
		        (SELECT COUNT(*) FROM workspaces w WHERE w.org_id = o.id AND w.deleted_at IS NULL) AS workspace_count
		 FROM organizations o%s
		 ORDER BY o.created_at DESC
		 LIMIT $%d OFFSET $%d`,
		listWhere, limitIdx, offsetIdx,
	)

	rows, err := s.db.QueryContext(ctx, query, listArgs...)
	if err != nil {
		return nil, nil, fmt.Errorf("list all orgs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]types.OrgSummary, 0)
	for rows.Next() {
		var o types.OrgSummary
		if err := rows.Scan(
			&o.ID, &o.Name, &o.Slug, &o.CreatedBy, &o.CreatedAt, &o.UpdatedAt,
			&o.Status, &o.PlanID, &o.SubscriptionStatus,
			&o.MemberCount, &o.WorkspaceCount,
		); err != nil {
			return nil, nil, fmt.Errorf("scan org summary: %w", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate org summaries: %w", err)
	}
	return out, pagination, nil
}

// --- US-43.10: OIDC SSO configuration implementations ---

// decodeGroupRoleMapping parses a JSONB group_role_mapping blob into a typed
// map. Invalid values (non-admin/member roles) are dropped silently rather than
// failing the whole read — a corrupt mapping should not lock users out of SSO.
func decodeGroupRoleMapping(b []byte) map[string]types.OrgRole {
	if len(b) == 0 || string(b) == "null" {
		return map[string]types.OrgRole{}
	}
	var raw map[string]string
	if err := json.Unmarshal(b, &raw); err != nil {
		return map[string]types.OrgRole{}
	}
	out := make(map[string]types.OrgRole, len(raw))
	for group, role := range raw {
		switch types.OrgRole(role) {
		case types.OrgRoleAdmin, types.OrgRoleMember:
			out[group] = types.OrgRole(role)
		}
	}
	return out
}

// scanSSOConfig scans one org_sso_configs row into a typed config. group_role_mapping
// (JSONB) is scanned as raw bytes then decoded to map[string]OrgRole.
// verification_token is a nullable TEXT column scanned into a *string then
// dereferenced; empty/NULL becomes "".
func scanSSOConfig(row *sql.Row, cfg *types.OrgSSOConfig) error {
	var groupMappingBytes []byte
	var verifiedDomains []string
	var verificationToken sql.NullString
	if err := row.Scan(
		&cfg.OrgID,
		&cfg.DiscoveryURL,
		&cfg.ClientID,
		&cfg.ClientSecret,
		pgarray.Array(&cfg.ClaimedDomains),
		pgarray.Array(&verifiedDomains),
		&verificationToken,
		&cfg.AutoProvision,
		&groupMappingBytes,
		&cfg.CreatedAt,
		&cfg.UpdatedAt,
	); err != nil {
		return err
	}
	cfg.GroupRoleMapping = decodeGroupRoleMapping(groupMappingBytes)
	if cfg.ClaimedDomains == nil {
		cfg.ClaimedDomains = []string{}
	}
	cfg.VerifiedDomains = verifiedDomains
	if cfg.VerifiedDomains == nil {
		cfg.VerifiedDomains = []string{}
	}
	cfg.VerificationToken = verificationToken.String
	return nil
}

func (s *PgOrgStore) GetSSOConfig(ctx context.Context, orgID string) (*types.OrgSSOConfig, error) {
	var cfg types.OrgSSOConfig
	if err := scanSSOConfig(s.db.QueryRowContext(ctx,
		`SELECT org_id, oidc_discovery_url, oidc_client_id, oidc_client_secret,
		        claimed_domains, verified_domains, verification_token,
		        auto_provision, group_role_mapping, created_at, updated_at
		 FROM org_sso_configs WHERE org_id = $1`,
		orgID,
	), &cfg); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get sso config: %w", err)
	}
	return &cfg, nil
}

func (s *PgOrgStore) UpsertSSOConfig(ctx context.Context, config *types.OrgSSOConfig) error {
	groupMapping := encodeGroupRoleMapping(config.GroupRoleMapping)
	verified := ssoDomainsParam(config.VerifiedDomains)
	// On INSERT, generate a verification token if the caller didn't supply one
	// so the org admin can immediately set up DNS verification. On CONFLICT,
	// preserve the existing token (rotation is via RotateVerificationToken).
	token := config.VerificationToken
	if token == "" {
		token = randomVerificationToken()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO org_sso_configs
		   (org_id, oidc_discovery_url, oidc_client_id, oidc_client_secret,
		    claimed_domains, verified_domains, verification_token,
		    auto_provision, group_role_mapping, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW(), NOW())
		 ON CONFLICT (org_id) DO UPDATE SET
		   oidc_discovery_url = EXCLUDED.oidc_discovery_url,
		   oidc_client_id     = EXCLUDED.oidc_client_id,
		   oidc_client_secret = EXCLUDED.oidc_client_secret,
		   claimed_domains    = EXCLUDED.claimed_domains,
		   verified_domains   = EXCLUDED.verified_domains,
		   auto_provision     = EXCLUDED.auto_provision,
		   group_role_mapping = EXCLUDED.group_role_mapping,
		   updated_at         = NOW()`,
		config.OrgID, config.DiscoveryURL, config.ClientID, config.ClientSecret,
		pgarray.Array(ssoDomainsParam(config.ClaimedDomains)), pgarray.Array(verified), token,
		config.AutoProvision, groupMapping,
	)
	if err != nil {
		return fmt.Errorf("upsert sso config: %w", err)
	}
	return nil
}

func (s *PgOrgStore) DeleteSSOConfig(ctx context.Context, orgID string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM org_sso_configs WHERE org_id = $1`, orgID,
	); err != nil {
		return fmt.Errorf("delete sso config: %w", err)
	}
	return nil
}

// FindSSOConfigByDomain resolves a claimed email domain (without leading
// "@") to the owning org's SSO config. Returns (nil, nil) when no org has
// claimed the domain.
//
// NOTE: this matches on claimed_domains (NOT verified_domains). It is intended
// for internal lookups where the org has been identified by other means and
// its full config is needed regardless of verification status. The login-page
// auto-routing path uses ListSSODomains (which filters on verified_domains).
// If you wire this into login routing, you MUST add a verified_domains check
// or you bypass the DNS verification gate this migration introduces.
func (s *PgOrgStore) FindSSOConfigByDomain(ctx context.Context, domain string) (*types.OrgSSOConfig, error) {
	var cfg types.OrgSSOConfig
	if err := scanSSOConfig(s.db.QueryRowContext(ctx,
		`SELECT c.org_id, c.oidc_discovery_url, c.oidc_client_id, c.oidc_client_secret,
		        c.claimed_domains, c.verified_domains, c.verification_token,
		        c.auto_provision, c.group_role_mapping, c.created_at, c.updated_at
		 FROM org_sso_configs c
		 JOIN organizations o ON o.id = c.org_id AND o.deleted_at IS NULL
		 WHERE $1 = ANY (c.claimed_domains)`,
		domain,
	), &cfg); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("find sso config by domain: %w", err)
	}
	return &cfg, nil
}

func (s *PgOrgStore) ListSSODomains(ctx context.Context) ([]types.SSODomain, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT o.slug, o.name, c.verified_domains
		 FROM org_sso_configs c
		 JOIN organizations o ON o.id = c.org_id AND o.deleted_at IS NULL
		 WHERE array_length(c.verified_domains, 1) IS NOT NULL
		 ORDER BY o.name`)
	if err != nil {
		return nil, fmt.Errorf("list sso domains: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []types.SSODomain
	for rows.Next() {
		var slug, name string
		var domains []string
		if err := rows.Scan(&slug, &name, pgarray.Array(&domains)); err != nil {
			return nil, fmt.Errorf("scan sso domain row: %w", err)
		}
		for _, d := range domains {
			out = append(out, types.SSODomain{
				Domain:  normalizeDomain(d),
				OrgSlug: slug,
				OrgName: name,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sso domain rows: %w", err)
	}
	if out == nil {
		out = []types.SSODomain{}
	}
	return out, nil
}

func (s *PgOrgStore) CountSSOConfigs(ctx context.Context) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM org_sso_configs`,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("count sso configs: %w", err)
	}
	return count, nil
}

// SetDomainVerified atomically appends a domain to verified_domains. The
// domain must already be in claimed_domains (the WHERE clause enforces this);
// a domain not claimed is a no-op. Idempotent: re-verifying an already-
// verified domain returns (false, nil) without error. Returns (true, nil)
// only when the domain was newly promoted.
func (s *PgOrgStore) SetDomainVerified(ctx context.Context, orgID, domain string) (bool, error) {
	tag, err := s.db.ExecContext(ctx,
		`UPDATE org_sso_configs
		    SET verified_domains = array_append(verified_domains, $2),
		        updated_at = NOW()
		  WHERE org_id = $1
		    AND $2 = ANY (claimed_domains)
		    AND $2 <> ALL (COALESCE(verified_domains, ARRAY[]::text[]))`,
		orgID, domain,
	)
	if err != nil {
		return false, fmt.Errorf("set domain verified: %w", err)
	}
	n, err := tag.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("set domain verified: rows affected: %w", err)
	}
	return n > 0, nil
}

// RotateVerificationToken replaces the org's verification token with a fresh
// random 32-hex value and returns it. Used for both initial creation (when
// verification_token is NULL) and rotation. Returns the new token.
func (s *PgOrgStore) RotateVerificationToken(ctx context.Context, orgID string) (string, error) {
	token := randomVerificationToken()
	tag, err := s.db.ExecContext(ctx,
		`UPDATE org_sso_configs
		    SET verification_token = $2,
		        updated_at = NOW()
		  WHERE org_id = $1`,
		orgID, token,
	)
	if err != nil {
		return "", fmt.Errorf("rotate verification token: %w", err)
	}
	n, err := tag.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("rotate verification token: rows affected: %w", err)
	}
	if n == 0 {
		return "", fmt.Errorf("rotate verification token: org %s has no sso config", orgID)
	}
	return token, nil
}

// encodeGroupRoleMapping serializes the typed mapping to JSONB-ready bytes.
func encodeGroupRoleMapping(m map[string]types.OrgRole) []byte {
	if len(m) == 0 {
		return []byte(`{}`)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

// normalizeDomain returns the domain with a leading "@" and lowercased, the
// form exposed by the discovery endpoint and matched against email suffixes.
func normalizeDomain(d string) string {
	d = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(d), "@")))
	if d == "" {
		return ""
	}
	return "@" + d
}

// ssoDomainsParam normalizes the claimed-domains slice for binding: nil becomes
// an empty slice to honor the NOT NULL DEFAULT '{}' constraint.
func ssoDomainsParam(domains []string) []string {
	if domains == nil {
		return []string{}
	}
	return domains
}

// randomVerificationToken generates a fresh 32-hex-char DNS verification token
// for the _llmsafespaces-verify TXT record. Uses crypto/rand so tokens are
// unpredictable (an attacker who can guess a token can forge verification).
func randomVerificationToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// rand.Read only errors on catastrophic system failure; panicking is
		// the only safe response since the security of DNS verification
		// depends on token unpredictability.
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}
