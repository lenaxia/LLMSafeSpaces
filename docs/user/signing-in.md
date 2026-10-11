# Signing In — passwords, passkeys, and single sign-on

How to reach your account. If you use a **password or passkey**, sign in at your
instance's main page. If your organisation uses **single sign-on (SSO)**, this
page explains how to get to the right sign-in door.

!!! note "Instance-dependent"
    SSO availability, registration, passkeys, and password reset all depend on
    how your instance is configured. This page describes the common shapes.

## Personal accounts (password / passkey)

At the main sign-in page, enter your email and password, or use a passkey if
your instance has them enabled. Forgot your password or need to verify your
email? See [Account Recovery](account-recovery.md).

## Organisation single sign-on (SSO)

If your organisation (or the team that invited you) uses its own identity
provider — Google Workspace, Okta, Azure AD, or any OIDC provider — you sign in
through **your organisation's sign-in link**, not by creating a password.

### Which door do I use?

| Your situation | What to do |
|---|---|
| You were given a link like `https://acme.app.example.com` | That is your organisation's sign-in page. Open it and click **Sign in with {your organisation}**. |
| You were given a link like `https://app.example.com/sso/acme` | Same thing — it jumps straight into your organisation's sign-in. |
| You already have an account and type your email at the main page | If your email's domain belongs to a **verified** organisation domain, a "Sign in with {organisation}" button appears; or use *Continue with email* and you'll be routed to your organisation's sign-in. |
| You were invited by email and you use a password/passkey | The invitation link is the whole journey: open it and accept using the account whose email matches the invitation. |
| You were invited by email and you're SSO-only (no password) | Use your organisation's sign-in link instead of the invitation link — see below. |

### What the "Sign in with {organisation}" button does

The button starts your organisation's single sign-on: your browser is
redirected to your organisation's identity provider, you sign in there with
whatever account that provider knows for you, and you come back signed in to
the platform. On first sign-in your account and organisation membership are
created automatically (if your organisation allows it). No platform password
is involved — your credentials stay with your organisation's provider.

On your organisation's sign-in page there is exactly **one** organisation
button — the one for the organisation whose page you're on. The password and
passkey forms below it remain available for personal accounts.

### Where you land after signing in

After single sign-in completes you land in the main app (the chat workspace
list) at your instance's main address — not on your organisation's subdomain.
That's normal: your organisation membership travels with your account, and
your organisation's workspaces are right there. If your instance is set up
with organisation subdomains, your session is *intended* to work across the
main site and those subdomains.

### "We couldn't find an account for that email"

If you typed your email at the main page and got this message, the platform
genuinely doesn't know which organisation you mean — and for privacy it will
not guess or list them. Ask your administrator, or the person who invited you,
for **your organisation's sign-in link**. That link is the way in.

## Troubleshooting

- **"Your account is suspended"** — contact your administrator.
- **"This organisation does not have single sign-on configured"** — the link
  points at an organisation without SSO; check you have the right link.
- **"Single sign-on session expired or was invalid"** — start again from your
  organisation's sign-in link; the sign-in window is short (10 minutes).
- **Sign-in works but you land without your organisation's workspaces** —
  contact your administrator; membership is managed on the organisation side.
