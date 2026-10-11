# Account Recovery — Email Verification & Password Reset

How to verify your email address and recover access to your account if you
forget your password. Both flows work from the web UI — no administrator
involved.

!!! note "Instance-dependent"
    Email delivery must be configured on your instance (most production
    deployments use SES). On instances without an email provider — local
    development, air-gapped installs — accounts are verified automatically at
    signup and this page doesn't apply.

## Verifying your email address

When your instance sends email, every new password-based account starts
**unverified**: after registering you'll see a *Check your email* screen, and
**sign-in is blocked until you verify** (the login page will tell you exactly
that, with a link to resend the email).

1. Open the message titled **"Verify your LLMSafeSpaces email"** and click
   *Click here to verify*. The link points at the platform's
   `/verify-email` page and carries a single-use token.
2. The page verifies automatically and confirms **Email verified**.
3. Click **Continue to sign in** and log in normally.

Details worth knowing:

- Verification links are **single-use** and expire after **24 hours**.
- Re-opening a used link says so and points you to sign-in — your email is
  already verified.
- If your link expired, the page offers to **send a new one**. For privacy,
  the confirmation never reveals whether an email belongs to an account.
- You can also trigger a resend from the sign-in page: try signing in with
  the correct password while unverified and follow the
  *Resend verification email* link.

## Resetting a forgotten password

1. On the sign-in page, click **Forgot your password?**
2. Enter your email and click **Send reset link**. For privacy, the
   confirmation is the same whether or not the address has an account (and
   email is only sent to **verified** addresses).
3. Open **"Reset your LLMSafeSpaces password"** and click
   *Click here to set a new password*. The link points at the platform's
   `/reset-password` page and carries a single-use token.
4. Choose a new password (minimum 8 characters, entered twice) and click
   **Reset password**.
5. Click **Sign in with your new password** — the sign-in page confirms your
   password was updated.

Details worth knowing:

- Reset links are **single-use** and expire after **15 minutes**. If yours
  expired or was already used, the page offers to request a new one.
- **Resetting signs you out everywhere** — all active sessions are revoked.
- **Saved secrets are deleted.** Provider credentials and user secrets are
  encrypted with a key derived from your account; a reset provisions fresh
  keys, so anything saved under *Settings → Secrets / Provider keys* must be
  re-entered after the reset.
- You'll receive a **"Your password was changed"** email after a successful
  reset. If you didn't request it, reset your password again immediately and
  contact your administrator.
