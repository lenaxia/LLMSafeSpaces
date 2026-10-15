// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package types

import "time"

// OrgRole represents a user's role within an organization.
type OrgRole string

const (
	OrgRoleAdmin  OrgRole = "admin"
	OrgRoleMember OrgRole = "member"
)

// OrgStatus is the operational status of an organization. It gates access:
// only non-suspended orgs are usable via OrgMemberGuard/OrgAdminGuard. Both
// 'active' and 'pending_activation' allow access (the creator needs to reach
// the portal and Stripe checkout while pending); 'suspended' is fully locked.
type OrgStatus string

const (
	OrgStatusPendingActivation OrgStatus = "pending_activation"
	OrgStatusActive            OrgStatus = "active"
	OrgStatusSuspended         OrgStatus = "suspended"
)

// OrgPlan is the product plan identifier stored in organizations.plan_id and
// used locally for feature gating. The plan is set at org creation
// (enterprise for platform-admin orgs; the selected checkout plan on
// checkout.session.completed for self-service orgs). Per-event plan syncing
// from Stripe is planned for US-43.15.
type OrgPlan string

const (
	PlanFree       OrgPlan = "free"
	PlanTeam       OrgPlan = "team"
	PlanBusiness   OrgPlan = "business"
	PlanEnterprise OrgPlan = "enterprise"
)

// OrgSubscriptionStatus tracks the Stripe subscription lifecycle separately
// from OrgStatus. An org can be status='active' (members retain access) while
// subscription_status='past_due' (in the 7-day Smart Retries grace window).
type OrgSubscriptionStatus string

const (
	SubscriptionInactive OrgSubscriptionStatus = "inactive"
	SubscriptionActive   OrgSubscriptionStatus = "active"
	SubscriptionTrialing OrgSubscriptionStatus = "trialing"
	SubscriptionPastDue  OrgSubscriptionStatus = "past_due"
	SubscriptionCanceled OrgSubscriptionStatus = "canceled"
	SubscriptionUnpaid   OrgSubscriptionStatus = "unpaid"
)

// Organization is the API DTO for an organization.
type Organization struct {
	ID                 string                `json:"id"`
	Name               string                `json:"name"`
	Slug               string                `json:"slug"`
	CreatedBy          string                `json:"createdBy"`
	CreatedAt          time.Time             `json:"createdAt"`
	UpdatedAt          time.Time             `json:"updatedAt"`
	Status             OrgStatus             `json:"status"`
	PlanID             OrgPlan               `json:"planId"`
	SubscriptionStatus OrgSubscriptionStatus `json:"subscriptionStatus"`
}

// OrgMember is the API DTO for an organization membership.
//
// EmailVerified mirrors users.email_verified for the member's user account. It
// is exposed so org admins can see which members have not yet completed the
// email-verification flow, and offers a "Verify" action to bypass it
// (POST /orgs/:id/members/:userID/verify). Verification state lives on the
// user row (not the membership) — there is exactly one user account per
// member, and a single user belongs to at most one org under single-org
// enforcement (D8).
type OrgMember struct {
	OrgID         string    `json:"orgId"`
	UserID        string    `json:"userId"`
	Username      string    `json:"username"`
	Email         string    `json:"email"`
	Role          OrgRole   `json:"role"`
	EmailVerified bool      `json:"emailVerified"`
	CreatedAt     time.Time `json:"createdAt"`
}

// CreateOrgRequest is the request body for creating an organization. The slug is
// lowercased by the service before insert and uniqueness check.
//
// Per design 0031 D1, org creation is platform-admin only. The admin supplies
// the intended owner's email; the backend resolves it to a user ID. This is a
// single lookup, not a search/list endpoint (account-enumeration prevention).
//
// Slug format: lowercase letters, digits, and single hyphens between segments
// (e.g. "my-org", "team-1"). The `slug` validator is registered in
// pkg/types/validators.go. Hyphens are required because the frontend's
// slugify() produces them from multi-word names; rejecting hyphens would
// produce an unreachable 400 for any user-friendly name.
//
// SSO carries the org's INITIAL OIDC configuration and is required at
// creation per design 0065 §4 (Option B, issue #1679): all orgs require
// OIDC, and the configuration must arrive complete in the same request —
// no two-step bootstrap, no waiting state. The handler (not binding tags)
// validates the block so nested failures surface with `sso.<field>` detail
// keys; in particular ClientSecret is REQUIRED here even though the shared
// UpsertSSOConfigRequest treats it as optional (at creation there is no
// stored secret for the "leave unchanged" path to fall back on).
type CreateOrgRequest struct {
	Name       string                  `json:"name"       binding:"required,min=2,max=100"`
	Slug       string                  `json:"slug"       binding:"required,min=2,max=50,slug"`
	OwnerEmail string                  `json:"ownerEmail" binding:"required,email"`
	PlanID     OrgPlan                 `json:"planId"     binding:"omitempty"`
	SSO        *UpsertSSOConfigRequest `json:"sso"       binding:"omitempty"`
}

// UpdateOrgRequest is the request body for updating an organization.
type UpdateOrgRequest struct {
	Name string `json:"name" binding:"omitempty,min=2,max=100"`
	Slug string `json:"slug" binding:"omitempty,min=2,max=50,slug"`
}

// CreateOrgResponse is returned by POST /api/v1/orgs. Org creation is
// platform-admin only (design 0031 D1).
type CreateOrgResponse struct {
	OrgResponse
}

// OrgResponse extends Organization with the calling user's membership context.
// UserRole is omitempty so that an empty string (caller is not a member — e.g.
// platform admin creating an org for someone else) is omitted from JSON rather
// than appearing as `"userRole": ""`.
type OrgResponse struct {
	Organization
	UserRole    OrgRole `json:"userRole,omitempty"`
	MemberCount int     `json:"memberCount"`
}

// AddOrgMemberRequest is the request body for adding an org member.
type AddOrgMemberRequest struct {
	UserID string  `json:"userId" binding:"required"`
	Role   OrgRole `json:"role"   binding:"required"`
}

// ChangeOrgMemberRoleRequest is the request body for changing a member's role.
type ChangeOrgMemberRoleRequest struct {
	Role OrgRole `json:"role" binding:"required"`
}

// --- US-43.2: Org invitations ---

// OrgInvitation is the API DTO for an org invitation row.
type OrgInvitation struct {
	ID         string     `json:"id"`
	OrgID      string     `json:"orgId"`
	Email      string     `json:"email"`
	Role       OrgRole    `json:"role"`
	InvitedBy  string     `json:"invitedBy"`
	TokenHash  string     `json:"-"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	AcceptedAt *time.Time `json:"acceptedAt,omitempty"`
	DeclinedAt *time.Time `json:"declinedAt,omitempty"`
	BounceType string     `json:"bounceType,omitempty"`
	BouncedAt  *time.Time `json:"bouncedAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`

	// InviteeUserExists is true when a `users` row exists with this
	// invitation's email (case-folded match). Surfaced so the org admin
	// UI can render the per-row Verify button only when force-verify is
	// actionable. Pointer so a missing-from-payload value is
	// distinguishable from a definite false on older API responses.
	InviteeUserExists *bool `json:"inviteeUserExists,omitempty"`

	// InviteeEmailVerified mirrors users.email_verified for the row
	// matched by InviteeUserExists. Nil when no users row exists. The
	// org admin UI hides the Verify button when this is true (the
	// override has already been applied or the user verified through
	// the normal flow).
	InviteeEmailVerified *bool `json:"inviteeEmailVerified,omitempty"`
}

// CreateInvitationsRequest is the body for POST /orgs/:id/invitations.
type CreateInvitationsRequest struct {
	Emails []string `json:"emails" binding:"required,min=1,max=100,dive,email"`
	Role   OrgRole  `json:"role"   binding:"required"`
}

// InvitationDetail is the public response for GET /invitations/:token. It does
// not expose the token hash or internal IDs beyond what the recipient needs to
// decide whether to accept.
type InvitationDetail struct {
	OrgName     string    `json:"orgName"`
	OrgSlug     string    `json:"orgSlug"`
	InviterName string    `json:"inviterName"`
	Role        OrgRole   `json:"role"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// LastAdminOrg identifies an org where a user is the sole active admin.
type LastAdminOrg struct {
	OrgID   string `json:"orgId"`
	OrgName string `json:"orgName"`
}

// OrgSummary extends Organization with aggregate counts for the platform
// admin dashboard. The counts are populated by a single SQL query (no N+1).
type OrgSummary struct {
	Organization
	MemberCount    int `json:"memberCount"`
	WorkspaceCount int `json:"workspaceCount"`
}

// OrgSSOConfig is the per-org OIDC SSO configuration. ClientSecret is the
// encrypted blob stored in the DB; it is never serialized to JSON.
// VerifiedDomains is a subset of ClaimedDomains that have passed DNS
// verification (D17 Q-S2); only verified domains auto-route on the login
// page. VerificationToken is the per-org random token the org admin places
// as a TXT record at _llmsafespaces-verify.<domain> to prove ownership.
type OrgSSOConfig struct {
	OrgID             string             `json:"-"`
	DiscoveryURL      string             `json:"discoveryUrl"`
	ClientID          string             `json:"clientId"`
	ClientSecret      []byte             `json:"-"`
	ClaimedDomains    []string           `json:"claimedDomains"`
	VerifiedDomains   []string           `json:"verifiedDomains"`
	VerificationToken string             `json:"verificationToken"`
	AutoProvision     bool               `json:"autoProvision"`
	GroupRoleMapping  map[string]OrgRole `json:"groupRoleMapping"`
	CreatedAt         time.Time          `json:"createdAt"`
	UpdatedAt         time.Time          `json:"updatedAt"`
}

// OrgSSOConfigResponse is the API response shape — omits the encrypted secret.
type OrgSSOConfigResponse struct {
	OrgID             string             `json:"orgId"`
	DiscoveryURL      string             `json:"discoveryUrl"`
	ClientID          string             `json:"clientId"`
	HasSecret         bool               `json:"hasSecret"`
	ClaimedDomains    []string           `json:"claimedDomains"`
	VerifiedDomains   []string           `json:"verifiedDomains"`
	VerificationToken string             `json:"verificationToken"`
	AutoProvision     bool               `json:"autoProvision"`
	GroupRoleMapping  map[string]OrgRole `json:"groupRoleMapping"`
	UpdatedAt         time.Time          `json:"updatedAt"`
}

// UpsertSSOConfigRequest is the API request body for creating/updating SSO config.
type UpsertSSOConfigRequest struct {
	DiscoveryURL     string             `json:"discoveryUrl"     binding:"required,url"`
	ClientID         string             `json:"clientId"         binding:"required,min=1"`
	ClientSecret     string             `json:"clientSecret"     binding:"omitempty"`
	ClaimedDomains   []string           `json:"claimedDomains"`
	AutoProvision    *bool              `json:"autoProvision"`
	GroupRoleMapping map[string]OrgRole `json:"groupRoleMapping"`
}

// SSODomain is a single entry in the domain discovery response.
type SSODomain struct {
	Domain  string `json:"domain"`
	OrgSlug string `json:"orgSlug"`
	OrgName string `json:"orgName"`
}
