# Per-org OIDC SSO

This page covers LLMSafeSpaces' per-organization OIDC single sign-on: the model (one identity provider per org, not instance-wide), the Authorization Code + PKCE login flow, the org-admin configuration API, auto-provisioning and role mapping, DNS domain verification, and the `redirect-base-url` requirement. Per-org SSO shipped in Epic 43 (US-43.10, decision D17).

## On this page

- [The model](#the-model)
- [Prerequisites and configuration](#prerequisites-and-configuration)
- [The redirect-base-url requirement](#the-redirect-base-url-requirement)
- [The PKCE login flow](#the-pkce-login-flow)
- [Org admin configuration](#org-admin-configuration)
- [Auto-provisioning and role mapping](#auto-provisioning-and-role-mapping)
- [DNS domain verification](#dns-domain-verification)
- [Security controls](#security-controls)
- [IdP-specific setup notes](#idp-specific-setup-notes)
- [Troubleshooting SSO](#troubleshooting-sso)
- [Known gaps and non-goals](#known-gaps-and-non-goals)

---

## The model

SSO in LLMSafeSpaces is **per-org, not instance-wide**. There is no global identity provider. Each organization configures its own OIDC IdP via the API; the platform owns the client secret at rest. This means:

- Org A can use Google Workspace; org B can use Okta; org C can use Azure AD — all on the same LLMSafeSpaces instance.
- `/auth/config` advertises `oidcEnabled = (CountSSOConfigs > 0)` so the frontend hides SSO UI when no org has configured it.
- Every SSO login is org-scoped: `/auth/sso/:orgSlug/...`.

### Data model

One row per org in `org_sso_configs`, keyed by `org_id`:

| Column | Type | Notes |
|---|---|---|
| `org_id` | `UUID` PK | FK → `organizations(id)` `ON DELETE CASCADE` |
| `oidc_discovery_url` | `TEXT` | IdP `.well-known/openid-configuration` URL |
| `oidc_client_id` | `TEXT` | Public client identifier |
| `oidc_client_secret` | `BYTEA` | **Encrypted at rest with the server KEK** (D17-S4) — derived from the master KEK, always decryptable, no org-DEK dependency |
| `claimed_domains` | `TEXT[]` | Email domains that route to this org on the login page; GIN-indexed for domain→org lookup |
| `auto_provision` | `BOOLEAN` | Create a new user on first SSO login if none exists for the email |
| `group_role_mapping` | `JSONB` | `{groupId: "admin"|"member"}`; applied on every login |

---

## Prerequisites and configuration

Per-org SSO requires **instance-level plumbing** (where the IdP redirects back to) configured via Helm or env vars, plus **per-org IdP config** entered by each org admin.

### Instance plumbing (Helm)

```yaml
oidc:
  redirectBaseUrl: "https://app.example.com"   # REQUIRED for SSO
  frontendRedirectUrl: "https://app.example.com" # where the browser lands after SSO
  stateCookieName: ""                            # defaults to "lsp_sso_state"
```

| Helm key | Env var | Default | Purpose |
|---|---|---|---|
| `oidc.redirectBaseUrl` | `LLMSAFESPACES_OIDC_REDIRECTBASEURL` | `""` | Absolute base for SSO callback URLs. **Required for SSO.** |
| `oidc.frontendRedirectUrl` | `LLMSAFESPACES_OIDC_FRONTENDREDIRECTURL` | `""` | Browser landing URL after SSO callback. Empty → `/`. |
| `oidc.stateCookieName` | `LLMSAFESPACES_OIDC_STATECOOKIENAME` | `""` (→ `lsp_sso_state`) | PKCE/state cookie name. Override only on collision. |

The state-cookie signing key is `deriveServerKey("oidc-state-cookie")`, derived from the same master secret as the KEK.

### Per-org IdP config

Entered by the org admin through the API (or the frontend's Org Admin → SSO tab). **Not** in `config.yaml`, `values.yaml`, or the settings system. See [Org admin configuration](#org-admin-configuration).

---

## The redirect-base-url requirement

`oidc.redirectBaseUrl` is **required** for SSO. The Start and Callback endpoints return a config error if it is unset, rather than trusting `X-Forwarded-*` headers.

**Why:** Previously, an empty `redirectBaseUrl` caused the start endpoint to derive the callback URL from `X-Forwarded-Proto` + `Host` headers. This is a header-trust gap (F11): a spoofed `X-Forwarded-Host` could redirect the IdP callback to an attacker-controlled host. The IdP's registered-redirect-URI check was the only mitigation. The platform now fails loud instead.

The full callback URL is:

```
{redirectBaseUrl}/api/v1/auth/sso/:orgSlug/callback
```

Register this exact URL in your IdP's allowed redirect URIs.

---

## The PKCE login flow

SSO uses Authorization Code flow with PKCE (S256), implemented via `coreos/go-oidc/v3`. The API is stateless — there is no server-side PKCE session store. The PKCE verifier and state are carried in a signed cookie.

```mermaid
sequenceDiagram
    participant B as Browser
    participant A as API (stateless)
    participant I as IdP

    B->>A: 1. GET /auth/sso/<slug>/start
    A->>A: load org_sso_configs[org]
    A->>A: oidc.NewProvider(discoveryURL)
    A->>A: generate verifier + state
    A->>A: sign cookie {state, verifier, orgID, exp} HMAC-SHA256
    A-->>B: 2. Set-Cookie: lsp_sso_state=<signed>; SameSite=Lax
    A-->>B: 3. 302 Location: <IdP authorize>?code_challenge=S256&state=...
    B->>I: 4. User authenticates
    I-->>B: 5. 302 /auth/sso/<slug>/callback?code=...&state=...
    B->>A: 6. GET /auth/sso/<slug>/callback
    A->>A: verify state cookie (HMAC, exp, orgID bound to slug)
    A->>I: 7. token exchange (code + verifier)
    I-->>A: id_token
    A->>A: verify id_token (provider.Verifier, clientID aud)
    A->>A: enforce email_verified == true (F8)
    A->>A: resolveUser (lookup or auto-provision)
    A->>A: resolveRole (highest-priv match in group_role_mapping)
    A->>A: ensureMembership (create/update role)
    A->>A: GenerateToken(userID) → JWT
    A-->>B: 8. Set-Cookie: lsp_session=<jwt>; HttpOnly; Secure
    A-->>B: 9. 302 <frontend>/?sso=success
```

### Why the state cookie carries the verifier

The API is stateless — there is no server-side PKCE session store. The cookie carries `{state, verifier, orgID, exp}`:

- **HMAC-SHA256 signed** — tampering is detected.
- **10-minute TTL** (`DefaultStateTTL`).
- **Callback bound to start org** — `org.ID == payload.OrgID` check. An attacker cannot start SSO for org A and replay the callback against org B.
- **SameSite=Lax** — survives the top-level IdP→callback redirect, blocked on cross-site POST.

---

## Org admin configuration

Org admins configure SSO via the API. The routes sit behind `OrgAdminGuard`.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/v1/orgs/:id/sso` | Org admin | Read this org's SSO config (secret omitted) |
| `PUT` | `/api/v1/orgs/:id/sso` | Org admin | Upsert SSO config; encrypts client secret, audits `sso.update` |
| `DELETE` | `/api/v1/orgs/:id/sso` | Org admin | Remove SSO config; audits `sso.delete` |
| `POST` | `/api/v1/orgs/:id/sso/domains/:domain/verify` | Org admin | On-demand DNS verification of a claimed domain |
| `POST` | `/api/v1/orgs/:id/sso/verification-token/rotate` | Org admin | Generate or rotate the DNS verification token |
| `GET` | `/api/v1/auth/sso/domains` | Public | List all orgs' **verified** domains (for login-page routing) |
| `GET` | `/api/v1/auth/sso/:orgSlug/start` | Public | Begin PKCE flow |
| `GET` | `/api/v1/auth/sso/:orgSlug/callback` | Public | Complete flow |

### Upsert example

```bash
curl -X PUT "$API/api/v1/orgs/$ORG_ID/sso" \
  -H "Authorization: Bearer $ORG_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "oidcDiscoveryUrl": "https://accounts.google.com",
    "oidcClientId": "your-client-id.apps.googleusercontent.com",
    "oidcClientSecret": "your-client-secret",
    "claimedDomains": ["example.com", "@corp.example.com"],
    "autoProvision": true,
    "groupRoleMapping": {
      "admins@example.com": "admin",
      "developers@example.com": "member"
    }
  }'
```

### Partial updates

The `PUT` body supports partial updates:

- **`ClientSecret` present** → `EncryptClientSecret` with the server KEK.
- **`ClientSecret` empty** → reuse the existing encrypted blob (leave unchanged).
- **`ClientSecret` empty and no existing** → `400 client secret is required`.

Domains are normalized (lowercased, leading `@` stripped, deduplicated).

### Frontend

The frontend ships an org-admin SSO config form at `OrgAdminLayout.tsx` → `sso` tab (`OrgSSOTab.tsx`). The login page (`LoginPage.tsx`) fetches `/auth/sso/domains` and matches the typed email domain against claimed domains, surfacing a "Sign in with {orgName}" button.

---

## Auto-provisioning and role mapping

### Auto-provisioning

When `auto_provision=true` and no user exists for the SSO-authenticated email:

- A user is created with a **random unusable bcrypt hash** (`$2a$12$<random>`) so password login is permanently blocked.
- The user has no password to derive a DEK from. **Personal credential operations stay unavailable** until they set a password; org workspaces still work via server-side injection.

When `auto_provision=false` and no user exists → `403 provisioning_disabled`.

### Role mapping

`group_role_mapping` maps IdP groups to org roles (`admin` | `member`). On every login:

1. Walk IdP groups (OIDC `groups` ∪ Azure AD `memberOf`).
2. The **highest-privilege match wins**; `admin` outranks `member`.
3. Unmapped/empty → `member` (safe default).
4. `ensureMembership` creates or updates the membership row so IdP-driven role changes propagate on every re-login.
5. A demotion `admin→member` is **skipped when the user is the sole admin** (last-admin protection; logged at WARN).

---

## DNS domain verification

Claimed domains must be verified before they appear in the login-page discovery endpoint (`ListSSODomains` filters on `verified_domains`). This prevents org A from claiming `example.com` and intercepting logins meant for org B.

### Verification flow

1. Org admin generates a verification token:

   ```bash
   curl -X POST "$API/api/v1/orgs/$ORG_ID/sso/verification-token/rotate" \
     -H "Authorization: Bearer $ORG_ADMIN_TOKEN"
   # → { "token": "abc123..." }
   ```

2. Org admin adds a DNS TXT record:

   ```
   _llmsafespaces-verify.<domain>.  TXT  "<token>"
   ```

3. Org admin triggers verification:

   ```bash
   curl -X POST "$API/api/v1/orgs/$ORG_ID/sso/domains/example.com/verify" \
     -H "Authorization: Bearer $ORG_ADMIN_TOKEN"
   ```

4. The platform queries DNS for the TXT record. On match, the domain is marked verified.

### Grandfathering

Existing claimed domains at migration time were grandfathered as verified (operator decision). New domains added after the migration must go through verification.

---

## Security controls

| Control | Implementation |
|---|---|
| Client secret encryption at rest | Server KEK (`RootKeyProvider.Encrypt`), `BYTEA` column (D17-S4) |
| PKCE S256 | `code_challenge` derived from random verifier; verifier in signed cookie |
| State cookie integrity | HMAC-SHA256 over `{state, verifier, orgID, exp}`; constant-time compare |
| State cookie expiry | 10-minute TTL |
| Callback bound to start org | `org.ID == payload.OrgID` check |
| `email_verified` enforcement (F8) | Absent/false → `ErrEmailUnverified` (403) |
| Email-claim trust | `email` only used for account binding when IdP-verified |
| Suspended-user block | `user.Status == suspended` → `ErrUserSuspended` |
| Last-admin protection | IdP demotion refused if user is sole org admin |
| Secret never in responses | `OrgSSOConfigResponse.HasSecret` replaces the blob |
| SameSite=Lax state cookie | Survives top-level redirect, blocked on cross-site POST |
| IdP-registered redirect URI | Defense-in-depth; IdP only redirects to registered URIs |
| Auto-provision off → 403 | `ErrAutoProvisionOff` mapped to `provisioning_disabled` |

---

## IdP-specific setup notes

### Google Workspace

- **Discovery URL:** `https://accounts.google.com`
- **Client ID:** `<project>.apps.googleusercontent.com`
- **Redirect URI:** `https://app.example.com/api/v1/auth/sso/<orgSlug>/callback`
- **Groups:** Google does not emit OIDC `groups` by default; use a Groups claim mapping in the Google admin console, or omit `groupRoleMapping` (all SSO users become `member`).

### Azure AD (Microsoft Entra ID)

- **Discovery URL:** `https://login.microsoftonline.com/<tenant-id>/v2.0`
- **Groups:** Azure AD exposes groups via `memberOf`. The role resolver walks `memberOf` ∪ OIDC `groups`. Map Azure AD group object IDs to roles:

  ```json
  {
    "group_role_mapping": {
      "<admin-group-object-id>": "admin",
      "<member-group-object-id>": "member"
    }
  }
  ```

- **Email claim:** Ensure the `email` claim is emitted (app registration → Token configuration → add `email` optional claim).

### Okta

- **Discovery URL:** `https://<your-org>.okta.com`
- **Groups:** Okta emits `groups` by default if the Groups scope is requested. Map Okta group names to roles.

### Generic OIDC (Keycloak, Auth0, etc.)

- **Discovery URL:** the IdP's `.well-known/openid-configuration` endpoint.
- Ensure the IdP emits `email`, `email_verified`, and `groups` (or equivalent) claims.
- The platform enforces `email_verified == true` (F8) — if your IdP doesn't set it, logins fail with `email_unverified`.

---

## Troubleshooting SSO

| Symptom | Likely cause | Fix |
|---|---|---|
| `state_invalid` on callback | State cookie expired (>10 min) or HMAC mismatch | Re-start the flow; check clock skew between browser and API. |
| `email_unverified` (403) | IdP did not set `email_verified: true` | Configure the IdP to emit the claim; or ensure the user's email is verified in the IdP. |
| `provisioning_disabled` (403) | `auto_provision=false` and user doesn't exist | Set `auto_provision=true`, or create the user first. |
| `suspended` | User's platform account is suspended | Unsuspend the user via the admin API. |
| Redirect loop | `redirectBaseUrl` misconfigured or IdP redirect URI not registered | Set `oidc.redirectBaseUrl` to the exact public origin; register the callback URL in the IdP. |
| "Sign in with {org}" button missing | Domain not verified | Complete [DNS domain verification](#dns-domain-verification). |
| Login works but role is wrong | `group_role_mapping` doesn't match IdP group IDs | Verify the group IDs/names the IdP emits; update the mapping. |

### Checking what the IdP emits

To debug claims, temporarily point a test org at a debugging IdP (e.g. `https://oidcdebugger.com`) or inspect the id_token in the API logs at debug level. The platform does not log the id_token by default (it contains PII).

---

## Known gaps and non-goals

### Gaps

- **`redirectBaseUrl` header-trust gap (F11)** — fixed. The platform now requires `redirectBaseUrl` and fails loud. Header derivation is removed.
- **State cookie replay within TTL** — the 10-minute window is the design trade-off for statelessness. Mitigated by orgID binding.

### Non-goals

- **No instance-level / platform-global OIDC** — every SSO login is org-scoped. A single-IdP-for-the-whole-deployment mode does not exist; `cfg.OIDC` carries only plumbing.
- **No SAML or SCIM** — explicitly deferred per Epic 43 decision D3.
- **No generic org-level settings tier** — org config lives in dedicated normalized tables, not a key-value `org_settings` table.

---

## Related

- [Security Hardening](security.md) — the master KEK that encrypts org SSO client secrets.
- [Configuration](configuration.md) — the `oidc.*` Helm values and env vars.
- [Multi-tenant Isolation](multi-tenant.md) — org-scoped access and tenant identity.
