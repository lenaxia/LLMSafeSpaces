import { api } from "./client";
import type { UpsertSSOConfigRequest } from "./sso";
import type { ProviderCredential } from "./providerCredentialTypes";
import type { WorkspaceListItem } from "./types";

export type OrgStatus = "pending_activation" | "active" | "suspended";
export type OrgPlan = "free" | "team" | "business" | "enterprise";
export type OrgSubscriptionStatus =
  | "inactive"
  | "active"
  | "trialing"
  | "past_due"
  | "canceled"
  | "unpaid";

export interface Organization {
  id: string;
  name: string;
  slug: string;
  createdBy: string;
  createdAt: string;
  updatedAt: string;
  status: OrgStatus;
  planId: OrgPlan;
  subscriptionStatus: OrgSubscriptionStatus;
}

export interface OrgResponse extends Organization {
  userRole?: "admin" | "member";
  memberCount: number;
}

export type CreateOrgResponse = OrgResponse;

export interface OrgMember {
  orgId: string;
  userId: string;
  username: string;
  email: string;
  role: "admin" | "member";
  /**
   * Mirrors users.email_verified from the backend. Surfaced so org admins can
   * see which members have not completed the email-verification flow and use
   * the "Verify" action to bypass it (see verifyMember).
   */
  emailVerified: boolean;
  createdAt: string;
}

export interface OrgCredential extends ProviderCredential {
  orgId: string;
  modelAllowlist: string[];
  modelContextLimits: Record<string, number>;
  modelOutputLimits: Record<string, number>;
  /** Present when the credential was created but org-workspace auto-bind failed. */
  bindWarning?: string;
}

export interface OrgInvitation {
  id: string;
  orgId: string;
  email: string;
  role: "admin" | "member";
  invitedBy: string;
  expiresAt: string;
  acceptedAt?: string;
  declinedAt?: string;
  bounceType?: string;
  bouncedAt?: string;
  createdAt: string;

  /**
   * True when a `users` row exists with this invitation's email
   * (case-folded match). Surfaced so the org admin UI can render the
   * per-row Verify button only when force-verify is actionable.
   * Optional for backward compat with cached responses from older
   * API versions.
   */
  inviteeUserExists?: boolean;

  /**
   * Mirrors `users.email_verified` for the row matched by
   * inviteeUserExists. Undefined when no users row exists. The org
   * admin UI hides the Verify button when this is true.
   */
  inviteeEmailVerified?: boolean;
}

export interface AuditEntry {
  id: number;
  actorId: string;
  domain: string;
  action: string;
  targetId?: string;
  orgId?: string;
  metadata?: Record<string, unknown>;
  createdAt: string;
}

export interface InvitationDetail {
  orgName: string;
  orgSlug: string;
  inviterName: string;
  role: "admin" | "member";
  expiresAt: string;
}

/**
 * The org's initial OIDC configuration — REQUIRED at creation (design 0065
 * §4, Option B): all orgs require OIDC, complete in the same request. Same
 * shape as UpsertSSOConfigRequest except clientSecret is mandatory (at
 * creation there is no stored secret for the "leave unchanged" path).
 */
export type InitialSSOConfig = UpsertSSOConfigRequest & { clientSecret: string };

export interface CreateOrgRequest {
  name: string;
  slug: string;
  /** Owner's email; resolved to a user ID server-side (design 0031 D1). */
  ownerEmail: string;
  planId?: OrgPlan;
  /** Required: the org is born with its IdP — no two-step bootstrap. */
  sso: InitialSSOConfig;
}

export interface CreateInvitationsRequest {
  emails: string[];
  role: "admin" | "member";
}

export const orgsApi = {
  list: () => api.get<OrgResponse[]>("/orgs"),
  create: (req: CreateOrgRequest) =>
    api.post<CreateOrgResponse>("/orgs", req),
  get: (id: string) => api.get<OrgResponse>(`/orgs/${id}`),
  update: (id: string, req: { name?: string; slug?: string }) =>
    api.put<Organization>(`/orgs/${id}`, req),
  delete: (id: string) => api.delete<void>(`/orgs/${id}`),

  listMembers: (id: string) => api.get<OrgMember[]>(`/orgs/${id}/members`),
  addMember: (id: string, req: { userId: string; role: "admin" | "member" }) =>
    api.post<OrgMember>(`/orgs/${id}/members`, req),
  removeMember: (id: string, userId: string) =>
    api.delete<void>(`/orgs/${id}/members/${userId}`),
  changeMemberRole: (id: string, userId: string, role: "admin" | "member") =>
    api.put<{ message: string }>(`/orgs/${id}/members/${userId}`, { role }),
  /**
   * Force-verify a member's email, bypassing the email-validation flow.
   * Org-admin only. Idempotent. Used by the "Verify" button in the org admin
   * members table when an admin has confirmed the member's identity
   * out-of-band.
   */
  verifyMember: (id: string, userId: string) =>
    api.post<{ message: string }>(`/orgs/${id}/members/${userId}/verify`),

  listInvitations: (id: string) =>
    api.get<OrgInvitation[]>(`/orgs/${id}/invitations`),
  createInvitations: (id: string, req: CreateInvitationsRequest) =>
    api.post<OrgInvitation[]>(`/orgs/${id}/invitations`, req),
  revokeInvitation: (id: string, invId: string) =>
    api.delete<void>(`/orgs/${id}/invitations/${invId}`),
  resendInvitation: (id: string, invId: string) =>
    api.post<OrgInvitation>(`/orgs/${id}/invitations/${invId}/resend`),
  /**
   * Force-verify the user account associated with a pending invitation.
   * Org-admin only. Idempotent. Used when the invitee already has an
   * existing platform account but never completed email verification —
   * the admin override sets users.email_verified=true so the invitee
   * can log in. The invitation itself stays pending; the user must
   * still click the invitation link to accept and join the org.
   *
   * On 422 with body `{"error":"no_account_for_email"}`, the invitee
   * has no users row yet and must sign up before this action can be
   * used. The frontend renders a clear message in that case.
   */
  verifyInvitee: (id: string, invId: string) =>
    api.post<{ message: string }>(
      `/orgs/${id}/invitations/${invId}/verify-user`,
    ),

  getInvitationByToken: (token: string) =>
    api.get<InvitationDetail>(`/invitations/${token}`),
  acceptInvitation: (token: string) =>
    api.post<{ membership: OrgMember }>(`/invitations/${token}/accept`),
  declineInvitation: (token: string) =>
    api.post<{ status: string }>(`/invitations/${token}/decline`),

  listCredentials: (id: string) =>
    api.get<OrgCredential[]>(`/orgs/${id}/credentials`),
  createCredential: (
    id: string,
    req: {
      name: string;
      kind: string;
      slug: string;
      apiKey: string;
      baseURL?: string;
      modelAllowlist?: string[];
      modelContextLimits?: Record<string, number>;
      modelOutputLimits?: Record<string, number>;
    },
  ) => api.post<OrgCredential>(`/orgs/${id}/credentials`, req),
  updateCredential: (
    id: string,
    credId: string,
    req: {
      name?: string;
      kind?: string;
      slug?: string;
      apiKey?: string;
      baseURL?: string;
      modelAllowlist?: string[];
      modelContextLimits?: Record<string, number>;
      modelOutputLimits?: Record<string, number>;
    },
  ) => api.put<OrgCredential>(`/orgs/${id}/credentials/${credId}`, req),
  deleteCredential: (id: string, credId: string) =>
    api.delete<void>(`/orgs/${id}/credentials/${credId}`),
  probeCredentialModels: (id: string, credId: string) =>
    api.get<{
      models: { id: string; contextLimit: number; outputLimit: number }[];
      baseURL?: string;
      warning?: string;
    }>(`/orgs/${id}/credentials/${credId}/models`),

  listWorkspaces: (id: string) =>
    api.get<{ items: WorkspaceListItem[] }>(`/orgs/${id}/workspaces`),

  checkout: (id: string, planId: string) =>
    api.post<{ url: string }>(`/orgs/${id}/billing/checkout`, { planId }),
  portal: (id: string) =>
    api.post<{ url: string }>(`/orgs/${id}/billing/portal`),
  listAudit: (id: string) =>
    api.get<{ items: AuditEntry[] }>(`/orgs/${id}/audit`),
};

export interface AuditListFilters {
  orgId?: string;
  actorId?: string;
  domain?: string;
  limit?: number;
  offset?: number;
}

export interface AuditListResponse {
  items: AuditEntry[];
  pagination?: {
    total: number;
    start: number;
    end: number;
    limit: number;
    offset: number;
  };
}

export const adminAuditApi = {
  list: (filters: AuditListFilters = {}) => {
    const params = new URLSearchParams();
    if (filters.orgId) params.set("org_id", filters.orgId);
    if (filters.actorId) params.set("actor_id", filters.actorId);
    if (filters.domain) params.set("domain", filters.domain);
    if (filters.limit != null) params.set("limit", String(filters.limit));
    if (filters.offset != null) params.set("offset", String(filters.offset));
    const query = params.toString();
    return api.get<AuditListResponse>(
      `/admin/audit${query ? `?${query}` : ""}`,
    );
  },
};

// --- US-43.18: Platform admin dashboard ---

export type UserStatus = "active" | "suspended";

export interface OrgSummary extends Organization {
  memberCount: number;
  workspaceCount: number;
}

export interface UserListEntry {
  id: string;
  email: string;
  role: string;
  status: UserStatus;
  createdAt: string;
  orgCount: number;
  orgId?: string;
  orgName?: string;
}

export interface AdminListFilters {
  limit?: number;
  offset?: number;
  status?: OrgStatus | UserStatus;
}

export interface AdminListResponse<T> {
  items: T[];
  pagination?: {
    total: number;
    start: number;
    end: number;
    limit: number;
    offset: number;
  };
}

function adminListQuery(filters: AdminListFilters): string {
  const params = new URLSearchParams();
  if (filters.limit != null) params.set("limit", String(filters.limit));
  if (filters.offset != null) params.set("offset", String(filters.offset));
  if (filters.status) params.set("status", filters.status);
  const query = params.toString();
  return query ? `?${query}` : "";
}

export const adminPlatformApi = {
  listOrgs: (filters: AdminListFilters = {}) =>
    api.get<AdminListResponse<OrgSummary>>(
      `/admin/orgs${adminListQuery(filters)}`,
    ),
  listUsers: (filters: AdminListFilters = {}) =>
    api.get<AdminListResponse<UserListEntry>>(
      `/admin/users${adminListQuery(filters)}`,
    ),
  suspendOrg: (orgId: string) =>
    api.post<{ status: string }>(`/admin/orgs/${orgId}/suspend`),
  unsuspendOrg: (orgId: string) =>
    api.post<{ status: string }>(`/admin/orgs/${orgId}/unsuspend`),
  suspendUser: (userId: string, force = false) =>
    api.post<{ status: string }>(
      `/admin/users/${userId}/suspend${force ? "?force=true" : ""}`,
    ),
  unsuspendUser: (userId: string) =>
    api.post<{ status: string }>(`/admin/users/${userId}/unsuspend`),
};
