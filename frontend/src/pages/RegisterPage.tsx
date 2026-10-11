import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useAuth } from "../providers/AuthProvider";
import { authApi } from "../api/auth";
import { sanitiseReturnTo } from "../lib/returnTo";
import { AuthCard } from "../components/auth/AuthCard";
import { RegisterForm } from "../components/auth/RegisterForm";
import { PasskeyRegisterForm } from "../components/auth/PasskeyRegisterForm";
import { RecoveryCodesDisplay } from "../components/auth/RecoveryCodesDisplay";
import { Button } from "../components/ui/Button";

type Mode = "passkey" | "password" | "recovery-codes" | "verify-email";

export function RegisterPage() {
  const { register, loginWithToken } = useAuth();
  const [returnTo, setReturnTo] = useState("");
  const [passkeyEnabled, setPasskeyEnabled] = useState(false);
  const [mode, setMode] = useState<Mode>("password");
  const [recoveryCodes, setRecoveryCodes] = useState<string[]>([]);
  // #1651: set when registration returns emailVerified=false — the instance
  // wires an email verifier, so the user MUST verify before their next
  // login or they are locked out (login is 403-gated until verified).
  const [registeredEmail, setRegisteredEmail] = useState("");

  useEffect(() => {
    const raw = new URLSearchParams(window.location.search).get("return_to");
    if (raw) setReturnTo(sanitiseReturnTo(raw));
    authApi.getConfig().then((c) => {
      setPasskeyEnabled(c.passkeyEnabled ?? false);
      if (c.passkeyDefaultSignup && c.passkeyEnabled) {
        setMode("passkey");
      }
    }).catch(() => {});
  }, []);

  const redirectAfterAuth = () => {
    if (returnTo) {
      window.location.href = returnTo;
    }
  };

  // Passkey registration success → show recovery codes → login → redirect
  const handlePasskeySuccess = async (_token: string, codes: string[]) => {
    // The passkey handler sets the lsp_session cookie (HttpOnly + Secure).
    // Recovery codes are displayed BEFORE loginWithToken — if me() fails
    // (transient network error), the user still sees their one-time codes.
    setRecoveryCodes(codes);
    setMode("recovery-codes");
  };

  const handleRecoveryCodesContinue = async () => {
    // Best-effort: populate the user state. The cookie is already set by
    // the server; loginWithToken just calls /auth/me.
    try {
      await loginWithToken("");
    } catch {
      // Non-fatal — cookie is set, session will work on reload.
    }
    redirectAfterAuth();
  };

  // #1651: password registration needs the RESPONSE (not just success) to
  // branch on user.emailVerified. With a verifier wired (email_verified=false
  // at signup) show verification guidance instead of redirecting: the next
  // login is 403-gated until the emailed link is clicked. AuthProvider
  // deliberately leaves user null in that branch — GuestOnly would otherwise
  // bounce the user off /register before they read the guidance. For the
  // auto-verified branch the provider has already set the user, so the
  // GuestOnly bounce to /chat (or the returnTo hard-nav) completes the flow.
  const handlePasswordRegister = async (
    username: string,
    email: string,
    password: string,
    turnstileToken?: string,
  ) => {
    const res = await register(username, email, password, turnstileToken);
    if (res.user?.emailVerified === false) {
      setRegisteredEmail(email);
      setMode("verify-email");
      return;
    }
    redirectAfterAuth();
  };

  if (mode === "recovery-codes" && recoveryCodes.length > 0) {
    return (
      <AuthCard
        title="Recovery codes"
        description="Save these before continuing"
      >
        <RecoveryCodesDisplay
          codes={recoveryCodes}
          onContinue={handleRecoveryCodesContinue}
        />
      </AuthCard>
    );
  }

  // #1651: post-signup verification guidance. The register session is valid
  // (only the NEXT login is gated), so offer both next steps: verify, or
  // continue into the app and verify later before the session expires.
  if (mode === "verify-email") {
    const verifyHref = `/verify-email?email=${encodeURIComponent(registeredEmail)}${
      returnTo ? `&return_to=${encodeURIComponent(returnTo)}` : ""
    }`;
    return (
      <AuthCard
        title="Check your email"
        description="One more step to finish signing up"
      >
        <div className="flex flex-col gap-4">
          <p className="text-sm text-muted-foreground">
            We sent a verification link to{" "}
            <span className="font-medium">{registeredEmail}</span>. Click the
            link to verify your email address, then sign in — you'll need to
            verify before your next sign-in.
          </p>
          <Link
            to={verifyHref}
            className="block w-full text-center text-primary underline-offset-4 hover:underline"
          >
            Verify or resend the verification email
          </Link>
          <div className="border-t border-border pt-4">
            <Button
              variant="outline"
              className="w-full"
              onClick={() => {
                window.location.href = returnTo || "/chat";
              }}
            >
              Continue without verifying
            </Button>
            <p className="mt-2 text-center text-xs text-muted-foreground">
              The verification link expires in 24 hours.
            </p>
          </div>
        </div>
      </AuthCard>
    );
  }

  return (
    <AuthCard
      title="Create account"
      description="Get started with Safe Space"
      footer={
        <Link to={returnTo ? `/login?return_to=${encodeURIComponent(returnTo)}` : "/login"} className="text-primary underline-offset-4 hover:underline">
          Already have an account? Sign in
        </Link>
      }
    >
      {mode === "passkey" ? (
        <div className="flex flex-col gap-4">
          <PasskeyRegisterForm onSuccess={async (_token, codes) => {
            await handlePasskeySuccess(_token, codes);
          }} />
          <button
            type="button"
            onClick={() => setMode("password")}
            className="text-sm text-muted-foreground underline-offset-4 hover:underline"
          >
            Use password instead
          </button>
        </div>
      ) : (
        <div className="flex flex-col gap-4">
          <RegisterForm onSubmit={handlePasswordRegister} />
          {passkeyEnabled && (
            <div className="border-t border-border pt-4">
              <p className="mb-2 text-center text-xs text-muted-foreground">or</p>
              <Button
                variant="outline"
                className="w-full"
                onClick={() => setMode("passkey")}
              >
                Sign up with passkey
              </Button>
            </div>
          )}
        </div>
      )}
    </AuthCard>
  );
}
